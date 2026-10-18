package etcdgateway

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/sandboxfiles"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
)

const gatewayFilesPath = "/api/v1/sandboxes/" + gatewaySandboxID + "/files/"

type gatewayUploadPart struct {
	field, filename string
	data            []byte
}

func gatewayUploadMultipart(t *testing.T, parts ...gatewayUploadPart) (string, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for _, part := range parts {
		var output io.Writer
		var err error
		if part.filename != "" {
			output, err = w.CreateFormFile(part.field, part.filename)
		} else {
			output, err = w.CreateFormField(part.field)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := output.Write(part.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return body.String(), w.FormDataContentType()
}

// These socket tests establish admission and descriptor binding only. The
// service deliberately returns Unknown, never a fabricated terminal success;
// the original Linux native fixture verifies actual upload and signed drain.
func TestGatewayUploadAdmitsOneBoundedDescriptor(t *testing.T) {
	binary := []byte{0, 255, 128, 'x'}
	file := gatewayUploadPart{field: "file", filename: "payload.bin", data: binary}
	path := gatewayUploadPart{field: "path", data: []byte("/workspace/nested/payload.bin")}
	for _, test := range []struct {
		name, query, declared, target string
		parts                         []gatewayUploadPart
		data                          []byte
	}{
		{"path-before", "", "", "/workspace/nested/payload.bin", []gatewayUploadPart{path, file}, binary},
		{"path-after", "?path=/tmp/overridden.bin", "", "/workspace/nested/payload.bin", []gatewayUploadPart{file, path}, binary},
		{"declared-query", "?path=/tmp/", "4", "/tmp/payload.bin", []gatewayUploadPart{file}, binary},
		{"default-directory", "", "", "/workspace/payload.bin", []gatewayUploadPart{file}, binary},
		{"exact-limit", "?path=/workspace/limit.bin", "524288", "/workspace/limit.bin", []gatewayUploadPart{{field: "file", filename: "limit.bin", data: bytes.Repeat([]byte{255}, 512<<10)}}, bytes.Repeat([]byte{255}, 512<<10)},
		{"empty", "?path=/workspace/empty", "0", "/workspace/empty", []gatewayUploadPart{{field: "file", filename: "empty"}}, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan p.ExecutionRequest, 1)
			service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
			service.execCall = func(_ context.Context, _, _ string, request p.ExecutionRequest, stdout, _ io.Writer) (provisioning.PlainExecResult, error) {
				requests <- request
				_, _ = stdout.Write([]byte(`{}`))
				return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown, Operation: state.OperationReference{OperationID: "upload-original-operation"}, Effect: state.ExecEffectReference{CommandID: "upload-original-command"}}, state.ErrDeliveryUnknown
			}
			_, client, origin := fileGatewaySocket(t, service, 4)
			body, contentType := gatewayUploadMultipart(t, test.parts...)
			if test.name == "declared-query" {
				body += "bounded legal MIME epilogue"
			}
			headers := http.Header{"Content-Type": {contentType}, "X-Request-Id": {"upload-attribution"}}
			if test.declared != "" {
				headers.Set("X-Sandbox-File-Size", test.declared)
			}
			status, _, wire := fileHTTPRequest(t, client, origin, "POST", gatewayFilesPath+"upload"+test.query, body, headers)
			if status != 502 || !strings.Contains(string(wire), `"code":"exec_unknown"`) || service.execs.Load() != 1 {
				t.Fatalf("bounded upload did not reach exactly one original Exec: status=%d calls=%d body=%s", status, service.execs.Load(), wire)
			}
			request := <-requests
			if len(request.Argv) != 2 || request.Argv[0] != "/usr/local/bin/sandbox-launcher" || request.Argv[1] != "file-operation" || request.WorkDir != "/workspace" || request.RequiresNetwork || request.TTY || len(request.Stdin) > 1<<20 {
				t.Fatal("upload bypassed the existing bounded helper descriptor")
			}
			var input struct {
				Operation string `json:"operation"`
				Path      string `json:"path"`
				Content   []byte `json:"content"`
			}
			if err := json.Unmarshal(request.Stdin, &input); err != nil || input.Operation != "upload" || input.Path != test.target || !bytes.Equal(input.Content, test.data) {
				t.Fatalf("descriptor lost exact binary payload/path: operation=%s path=%s bytes=%d err=%v", input.Operation, input.Path, len(input.Content), err)
			}
			if !strings.Contains(string(wire), "upload-original-operation") || !strings.Contains(string(wire), "upload-original-command") || !strings.Contains(string(wire), "do not automatically retry") {
				t.Fatal("unknown upload lost original attribution/no-retry semantics", string(wire))
			}
		})
	}
}

func TestGatewayUploadRejectsMultipartBeforeExec(t *testing.T) {
	file := gatewayUploadPart{field: "file", filename: "a.bin", data: []byte("data")}
	path := gatewayUploadPart{field: "path", data: []byte("/workspace/a.bin")}
	for _, test := range []struct {
		name, query, declared string
		parts                 []gatewayUploadPart
		truncate              bool
		status                int
	}{
		{"over-limit", "", "", []gatewayUploadPart{{field: "file", filename: "large", data: bytes.Repeat([]byte{'x'}, (512<<10)+1)}}, false, 413},
		{"declared-over-limit", "?path=/workspace/a", "524289", []gatewayUploadPart{file}, false, 413},
		{"shorter", "?path=/workspace/a", "5", []gatewayUploadPart{file}, false, 400},
		{"longer", "?path=/workspace/a", "3", []gatewayUploadPart{file}, false, 400},
		{"declared-no-query", "", "4", []gatewayUploadPart{file}, false, 400},
		{"declared-path-before", "?path=/workspace/a", "4", []gatewayUploadPart{path, file}, false, 400},
		{"declared-path-after", "?path=/workspace/a", "4", []gatewayUploadPart{file, path}, false, 400},
		{"two-files", "", "", []gatewayUploadPart{file, file}, false, 400},
		{"declared-two-files", "?path=/workspace/a", "4", []gatewayUploadPart{file, file}, false, 400},
		{"duplicate-path", "", "", []gatewayUploadPart{path, file, path}, false, 400},
		{"truncated", "", "", []gatewayUploadPart{file}, true, 400},
		{"no-file", "", "", []gatewayUploadPart{path}, false, 400},
		{"escaped-path", "", "", []gatewayUploadPart{file, {field: "path", data: []byte("/workspace/../outside")}}, false, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
			_, client, origin := fileGatewaySocket(t, service, 4)
			body, contentType := gatewayUploadMultipart(t, test.parts...)
			if test.truncate {
				body = body[:len(body)-8]
			}
			headers := http.Header{"Content-Type": {contentType}}
			if test.declared != "" {
				headers.Set("X-Sandbox-File-Size", test.declared)
			}
			status, _, wire := fileHTTPRequest(t, client, origin, "POST", gatewayFilesPath+"upload"+test.query, body, headers)
			if status != test.status || service.execs.Load() != 0 {
				t.Fatalf("invalid multipart reached execution: status=%d calls=%d body=%s", status, service.execs.Load(), wire)
			}
		})
	}
}

// A terminal enum and helper-looking stdout are deliberately not signed
// evidence. Only the existing verified receipt/drain path can acknowledge an
// upload, even when no transport error accompanied this refused model result.
func TestGatewayUploadRejectsUnverifiedTerminal(t *testing.T) {
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
	service.execCall = func(_ context.Context, _, _ string, _ p.ExecutionRequest, stdout, _ io.Writer) (provisioning.PlainExecResult, error) {
		_, _ = stdout.Write([]byte(`{}`))
		return provisioning.PlainExecResult{State: provisioning.PlainExecLocalTerminal}, nil
	}
	_, client, origin := fileGatewaySocket(t, service, 4)
	body, contentType := gatewayUploadMultipart(t, gatewayUploadPart{field: "file", filename: "a", data: []byte("data")})
	status, _, wire := fileHTTPRequest(t, client, origin, "POST", gatewayFilesPath+"upload", body, http.Header{"Content-Type": {contentType}})
	if status != 502 || !strings.Contains(string(wire), `"code":"exec_unknown"`) || service.execs.Load() != 1 {
		t.Fatalf("unverified terminal bypassed original receipt admission: status=%d calls=%d body=%s", status, service.execs.Load(), wire)
	}
}

func TestGatewayUploadRejectsAmbiguousSizeHeaders(t *testing.T) {
	for name, values := range map[string][]string{
		"duplicate":  {"4", "4"},
		"empty":      {""},
		"negative":   {"-1"},
		"false-zero": {"0"},
	} {
		t.Run(name, func(t *testing.T) {
			service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
			_, client, origin := fileGatewaySocket(t, service, 4)
			body, contentType := gatewayUploadMultipart(t, gatewayUploadPart{field: "file", filename: "a", data: []byte("data")})
			status, _, wire := fileHTTPRequest(t, client, origin, "POST", gatewayFilesPath+"upload?path=/workspace/a", body, http.Header{"Content-Type": {contentType}, "X-Sandbox-File-Size": values})
			if status != 400 || service.execs.Load() != 0 {
				t.Fatalf("invalid size header reached execution: status=%d calls=%d body=%s", status, service.execs.Load(), wire)
			}
		})
	}
}

// MIME completion is not HTTP-body completion. The original socket request
// must also reach a bounded clean body EOF before an operation can be created.
func TestGatewayUploadRejectsOversizeEpilogueBeforeExec(t *testing.T) {
	t.Run("oversize-epilogue", func(t *testing.T) {
		service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
		service.execCall = func(context.Context, string, string, p.ExecutionRequest, io.Writer, io.Writer) (provisioning.PlainExecResult, error) {
			return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, state.ErrDeliveryUnknown
		}
		_, client, origin := fileGatewaySocket(t, service, 4)
		body, contentType := gatewayUploadMultipart(t, gatewayUploadPart{field: "file", filename: "a", data: []byte("data")})
		body += strings.Repeat("x", (512<<10)+(64<<10))
		status, _, wire := fileHTTPRequest(t, client, origin, "POST", gatewayFilesPath+"upload", body, http.Header{"Content-Type": {contentType}})
		if status != 413 || service.execs.Load() != 0 {
			t.Fatalf("oversized trailing body reached execution: status=%d calls=%d body=%s", status, service.execs.Load(), wire)
		}
	})
	t.Run("truncated-content-length", func(t *testing.T) {
		service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
		service.execCall = func(context.Context, string, string, p.ExecutionRequest, io.Writer, io.Writer) (provisioning.PlainExecResult, error) {
			return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, state.ErrDeliveryUnknown
		}
		_, _, origin := fileGatewaySocket(t, service, 4)
		body, contentType := gatewayUploadMultipart(t, gatewayUploadPart{field: "file", filename: "a", data: []byte("data")})
		address := strings.TrimPrefix(origin, "http://")
		connection, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if err := connection.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		_, err = fmt.Fprintf(connection, "POST %supload HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer mounted-secret\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", gatewayFilesPath, address, contentType, len(body)+1, body)
		if err != nil {
			t.Fatal(err)
		}
		// Half-close only input: the server must observe unexpected body EOF
		// after a valid MIME closing boundary, while its response stays readable.
		if err := connection.(*net.TCPConn).CloseWrite(); err != nil {
			t.Fatal(err)
		}
		response, err := http.ReadResponse(bufio.NewReader(connection), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != 400 || service.execs.Load() != 0 {
			t.Fatalf("truncated body reached execution: status=%d calls=%d", response.StatusCode, service.execs.Load())
		}
	})
}

type fileServiceFake struct {
	*gatewayServiceFake
	ready    func(context.Context) error
	execCall func(context.Context, string, string, p.ExecutionRequest, io.Writer, io.Writer) (provisioning.PlainExecResult, error)
}

func (s *fileServiceFake) Ready(ctx context.Context) error {
	if s.ready != nil {
		return s.ready(ctx)
	}
	return nil
}

func (s *fileServiceFake) Exec(ctx context.Context, id, requestID string, req p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
	if s.execCall != nil {
		s.execs.Add(1)
		return s.execCall(ctx, id, requestID, req, stdout, stderr)
	}
	return s.gatewayServiceFake.Exec(ctx, id, requestID, req, stdout, stderr)
}

func fileGatewaySocket(t *testing.T, service Service, maxActive int) (*Gateway, *http.Client, string) {
	t.Helper()
	settings := gatewaySettings()
	settings.MaxActive = maxActive
	gateway, err := New(service, settings)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(gateway)
	client := &http.Client{Timeout: 4 * time.Second}
	t.Cleanup(func() { gateway.Close(); server.Close(); client.CloseIdleConnections() })
	return gateway, client, server.URL
}

func fileHTTPRequest(t *testing.T, client *http.Client, origin, method, route, body string, headers http.Header) (int, http.Header, []byte) {
	t.Helper()
	r, err := http.NewRequest(method, origin+route, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer mounted-secret")
	for key, values := range headers {
		r.Header[key] = values
	}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	wire, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, response.Header, wire
}

// The fake service uses genuinely signed and verified protocol evidence. This
// verifies HTTP consumption; the native test supplies physical runtime proof.
func fileTerminal(t *testing.T, request p.ExecutionRequest, exit int) provisioning.PlainExecResult {
	t.Helper()
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	delegate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	now := time.Now().UTC().Truncate(time.Second)
	binding := p.TrustBinding{Namespace: "/sandbox/file-tests/v1/", AuthorityID: "launcher", Target: "target", RestoreEpoch: "restore"}
	verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}
	runtime := p.RuntimeReference{ID: "runtime", UID: "runtime-uid", BootID: "runtime-boot"}
	certificate, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: delegate.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute), SandboxID: gatewaySandboxID, WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: runtime})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := p.NewExecutionDescriptor(request)
	if err != nil {
		t.Fatal(err)
	}
	c := p.ExecStartContext{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IssuerCertificateID: uuid.NewString(), IssuerCertificateDigest: strings.Repeat("b", 64), CommandID: uuid.NewString(), OperationID: uuid.NewString(), RequestID: "file-request", OperationDigest: strings.Repeat("c", 64), SandboxID: gatewaySandboxID, WorkspaceHash: strings.Repeat("a", 64), Generation: 1, DataGateEpoch: 1, ControlRevision: 1, AdmissionRevision: 1, LeaseID: 1, Runtime: runtime, ExpiresAt: now.Add(time.Minute)}
	claims := p.LocalExecReceiptClaims{Version: 1, State: "local_terminal", Context: c, DescriptorDigest: descriptor.Digest(), TicketDigest: strings.Repeat("d", 64), NotBefore: now.Add(-time.Second), NotAfter: now.Add(20 * time.Second), AuthorityDeadline: now.Add(20 * time.Second), RootPID: 42, RootWaitStatus: uint32(exit << 8), DrainConfirmed: true, Reason: "exited"}
	wire, err := p.SignLocalExecReceipt(delegate, claims)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := verifier.VerifyLocalExecReceipt(wire, certificate, c, descriptor.Digest(), claims.TicketDigest, claims.NotBefore, claims.NotAfter, claims.AuthorityDeadline, now)
	if err != nil {
		t.Fatal(err)
	}
	return provisioning.PlainExecResult{State: provisioning.PlainExecLocalTerminal, Receipt: receipt, Operation: state.OperationReference{OperationID: c.OperationID}, Effect: state.ExecEffectReference{CommandID: c.CommandID}}
}

func TestGatewayFileVerifiedReceiptFixture(t *testing.T) {
	request := p.ExecutionRequest{Argv: []string{"/usr/local/bin/sandbox-launcher", "file-operation"}, Env: map[string]string{"PATH": "/usr/bin:/bin"}, UID: 1000, GID: 1001, WorkDir: "/workspace", TimeoutSeconds: 30, Stdin: []byte(`{"operation":"read","path":"/workspace/a"}`)}
	for _, exit := range []int{0, 7} {
		result := fileTerminal(t, request, exit)
		if actual, ok := terminalExit(result, nil); !ok || actual != exit {
			t.Fatal("signed verified terminal fixture rejected", actual, ok)
		}
	}
	if _, ok := terminalExit(provisioning.PlainExecResult{State: provisioning.PlainExecLocalTerminal}, nil); ok {
		t.Fatal("unverified zero evidence accepted")
	}
}

func TestGatewayFileRoutesSingleOriginalDescriptor(t *testing.T) {
	file := types.FileInfo{Name: "a\n\t.txt", Path: "/workspace/a\n\t.txt", Size: 3, ModTime: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)}
	for _, tc := range []struct {
		name, method, query, body string
		request                   sandboxfiles.Request
		result                    sandboxfiles.Result
	}{
		{"list", "GET", "", "", sandboxfiles.Request{Operation: "list", Path: "/workspace"}, sandboxfiles.Result{Files: []types.FileInfo{file}, TotalCount: 1}},
		{"list-recursive", "POST", "", `{"path":"/workspace","max_depth":3,"page":2,"page_size":1}`, sandboxfiles.Request{Operation: "list-recursive", Path: "/workspace", MaxDepth: 3, Page: 2, PageSize: 1}, sandboxfiles.Result{Files: []types.FileInfo{file}, TotalCount: 3, Page: 2, PageSize: 1}},
		{"glob", "POST", "", `{"path":"/workspace","pattern":"*.txt","page":1,"page_size":2}`, sandboxfiles.Request{Operation: "glob", Path: "/workspace", Pattern: "*.txt", Page: 1, PageSize: 2}, sandboxfiles.Result{Files: []types.FileInfo{file}, TotalCount: 1, Page: 1, PageSize: 2}},
		{"read", "POST", "", `{"path":"/workspace/a"}`, sandboxfiles.Request{Operation: "read", Path: "/workspace/a"}, sandboxfiles.Result{Content: []byte{0, 255, 128, 'x'}}},
		{"download", "GET", "?path=%2Fworkspace%2Fa%22.txt", "", sandboxfiles.Request{Operation: "download", Path: `/workspace/a".txt`}, sandboxfiles.Result{Content: []byte{0, 255, 128, 'x'}}},
		{"read-lines", "POST", "", `{"path":"/workspace/a","start_line":2,"end_line":3}`, sandboxfiles.Request{Operation: "read-lines", Path: "/workspace/a", StartLine: 2, EndLine: 3}, sandboxfiles.Result{Lines: []string{"", "last"}, StartLine: 2, EndLine: 3, TotalLines: 3}},
		{"edit", "POST", "", `{"path":"/workspace/a","old_str":"old","new_str":"new","replace_all":true}`, sandboxfiles.Request{Operation: "edit", Path: "/workspace/a", OldStr: "old", NewStr: "new", ReplaceAll: true}, sandboxfiles.Result{}},
		{"edit-lines", "POST", "", `{"path":"/workspace/a","start_line":1,"end_line":2,"new_content":"new\n"}`, sandboxfiles.Request{Operation: "edit-lines", Path: "/workspace/a", StartLine: 1, EndLine: 2, NewContent: "new\n"}, sandboxfiles.Result{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
			service.execCall = func(_ context.Context, id, requestID string, req p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
				expected, err := json.Marshal(tc.request)
				if err != nil {
					t.Fatal(err)
				}
				if id != gatewaySandboxID || requestID != "file-attribution" || !bytes.Equal(req.Stdin, expected) || len(req.Argv) != 2 || req.Argv[0] != "/usr/local/bin/sandbox-launcher" || req.Argv[1] != "file-operation" || req.UID != 1000 || req.GID != 1001 || req.WorkDir != "/workspace" || req.TimeoutSeconds != 30 || req.RequiresNetwork || req.TTY || len(req.Env) != 2 || req.Env["BASE"] != "operator" || req.Env["PATH"] != "/usr/bin:/bin" {
					t.Errorf("file descriptor lost fixed profile or canonical exact stdin: %+v expected %s", req, expected)
				}
				if err := json.NewEncoder(stdout).Encode(tc.result); err != nil {
					t.Error(err)
				}
				return fileTerminal(t, req, 0), nil
			}
			_, client, origin := fileGatewaySocket(t, service, 32)
			status, header, wire := fileHTTPRequest(t, client, origin, tc.method, gatewayFilesPath+tc.name+tc.query, tc.body, http.Header{"X-Request-Id": {"file-attribution"}})
			if status != 200 || header.Get("X-Request-ID") != "file-attribution" || service.execs.Load() != 1 || service.lookups.Load() != 0 || service.creates.Load() != 0 {
				t.Fatalf("route did not consume one original Exec: %d %s", status, wire)
			}
			switch tc.name {
			case "read", "download":
				if !bytes.Equal(wire, tc.result.Content) {
					t.Fatalf("binary response changed: %v", wire)
				}
				if tc.name == "read" && (header.Get("X-File-Path") != tc.request.Path || header.Get("Content-Type") != "text/plain; charset=utf-8") {
					t.Fatal("raw read headers lost", header)
				}
				if tc.name == "download" && (!strings.HasPrefix(header.Get("Content-Disposition"), "attachment;") || strings.ContainsAny(header.Get("Content-Disposition"), "\r\n") || header.Get("Content-Type") != "application/octet-stream") {
					t.Fatal("download disposition unsafe", header)
				}
			case "read-lines":
				var response types.ReadFileLinesResponse
				if err := json.Unmarshal(wire, &response); err != nil || response.TotalLines != 3 || response.StartLine != 2 || response.EndLine != 3 || strings.Join(response.Lines, "|") != "|last" {
					t.Fatal("original line DTO lost", string(wire), err)
				}
			case "edit", "edit-lines":
				if string(bytes.TrimSpace(wire)) != `{"message":"ok"}` {
					t.Fatal("original edit DTO lost", string(wire))
				}
			default:
				var response struct {
					Files      []types.FileInfo `json:"files"`
					Path       string           `json:"path"`
					Pattern    string           `json:"pattern"`
					TotalCount int              `json:"total_count"`
					Page       int              `json:"page"`
					PageSize   int              `json:"page_size"`
				}
				if err := json.Unmarshal(wire, &response); err != nil || len(response.Files) != 1 || response.Files[0].Name != file.Name || response.Path != tc.request.Path {
					t.Fatal("original file DTO lost", string(wire), err)
				}
				if tc.name != "list" && (response.TotalCount != tc.result.TotalCount || response.Page != tc.result.Page || response.PageSize != tc.result.PageSize || response.Pattern != tc.request.Pattern) {
					t.Fatal("pagination/glob DTO lost", string(wire))
				}
			}
		})
	}
}

func TestGatewayFileStrictNoEffectValidation(t *testing.T) {
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
	gateway, client, origin := fileGatewaySocket(t, service, 32)
	for _, tc := range []struct {
		method, route, body string
		header              http.Header
		status              int
	}{
		{"POST", "read", `{"path":"/workspace/a","path":"/workspace/b"}`, nil, 400},
		{"POST", "read", `{"Path":"/workspace/a"}`, nil, 400},
		{"POST", "read", `{"path":null}`, nil, 400},
		{"POST", "read", `{"path":"/workspace/a","root":"/"}`, nil, 400},
		{"POST", "read", `{"path":"/workspace/a"} {}`, nil, 400},
		{"POST", "read", `{"path":"/workspace/a"}`, http.Header{"Idempotency-Key": {""}}, 400},
		{"POST", "read", `{"path":"/workspace/a"}`, http.Header{"X-Request-Id": {strings.Repeat("x", 129)}}, 400},
		{"POST", "read", `{"path":"/workspace/../tmp/a"}`, nil, 400},
		{"POST", "read", `{"path":"/etc/passwd"}`, nil, 400},
		{"POST", "edit", `{"path":"/workspace/a","old_str":""}`, nil, 400},
		{"POST", "edit", `{"path":"/workspace/a","old_str":"` + strings.Repeat("x", sandboxfiles.MaxContentBytes) + `","new_str":"` + strings.Repeat("y", sandboxfiles.MaxContentBytes) + `"}`, nil, 413},
		{"POST", "edit-lines", `{"path":"/workspace/a","start_line":0}`, nil, 400},
		{"POST", "list-recursive", `{"path":"/workspace","page":-1}`, nil, 400},
		{"POST", "glob", `{"path":"/workspace","pattern":"["}`, nil, 400},
		{"GET", "download", "", nil, 400},
		{"GET", "list?path=/workspace&path=/tmp", "", nil, 400},
		{"GET", "list?authority=root", "", nil, 400},
		{"GET", "list", `{}`, nil, 400},
		{"POST", "read", strings.Repeat(" ", (2<<20)+1), nil, 413},
		{"POST", "read", `{"path":"/workspace/a"}`, http.Header{"Authorization": {"Bearer invalid"}}, 401},
	} {
		status, _, body := fileHTTPRequest(t, client, origin, tc.method, gatewayFilesPath+tc.route, tc.body, tc.header)
		if status != tc.status || service.execs.Load() != 0 || service.lookups.Load() != 0 {
			t.Fatalf("invalid input caused effects/status mismatch: %s %d %s", tc.route, status, body)
		}
	}
	status, _, body := fileHTTPRequest(t, client, origin, "GET", "/api/v1/sandboxes/not-a-uuid/files/list", "", nil)
	if status != 400 || service.execs.Load() != 0 {
		t.Fatal("file route bypassed original UUID validation", status, string(body))
	}
	request := gatewayRequest("POST", gatewayFilesPath+"read", `{"path":"/workspace/a"}`)
	request.Header["Idempotency-Key"] = nil
	response := httptest.NewRecorder()
	gateway.ServeHTTP(response, request)
	if response.Code != 400 || service.execs.Load() != 0 {
		t.Fatal("empty header presence reached Exec", response.Code)
	}
}

func TestGatewayFileTerminalErrorsAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name, output, state, code string
		exit, status              int
		err                       error
	}{
		{"missing", `{"error":{"code":"not_found"}}`, "terminal", "not_found", 0, 404, nil},
		{"conflict", `{"error":{"code":"conflict"}}`, "terminal", "conflict", 0, 409, nil},
		{"limit", `{"error":{"code":"limit_exceeded"}}`, "terminal", "limit_exceeded", 0, 413, nil},
		{"type", `{"error":{"code":"unsupported_type"}}`, "terminal", "unsupported_type", 0, 422, nil},
		{"permission", `{"error":{"code":"permission_denied"}}`, "terminal", "permission_denied", 0, 422, nil},
		{"invalid", `{"error":{"code":"invalid_request"}}`, "terminal", "invalid_request", 0, 400, nil},
		{"io", `{"error":{"code":"io_error"}}`, "terminal", "io_error", 0, 500, nil},
		{"published", `{"error":{"code":"publish_uncertain"}}`, "terminal", "publish_uncertain", 0, 500, nil},
		{"malformed", `{"content":"YQ==","content":"Yg=="}`, "terminal", "file_result_invalid", 0, 502, nil},
		{"mixed", `{"error":{"code":"not_found"},"content":"YQ=="}`, "terminal", "file_result_invalid", 0, 502, nil},
		{"nonzero", `{"content":"YQ=="}`, "terminal", "file_helper_failed", 7, 502, nil},
		{"unknown", `{"content":"YQ=="}`, "unknown", "exec_unknown", 0, 502, state.ErrGuardExpired},
		{"unverified", `{"content":"YQ=="}`, "unverified", "exec_unknown", 0, 502, nil},
		{"rejected", "", "rejected", "not_found", 0, 404, provisioning.ErrPlainColdNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
			service.execCall = func(_ context.Context, _, _ string, req p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
				_, _ = io.WriteString(stdout, tc.output)
				_, _ = io.WriteString(stderr, "private-key-sentinel")
				if tc.state == "terminal" {
					return fileTerminal(t, req, tc.exit), tc.err
				}
				result := provisioning.PlainExecResult{State: provisioning.PlainExecState(tc.state), Operation: state.OperationReference{OperationID: "original-file-operation"}, Effect: state.ExecEffectReference{CommandID: "original-file-command"}}
				if tc.state == "unverified" {
					result.State = provisioning.PlainExecLocalTerminal
				}
				return result, tc.err
			}
			_, client, origin := fileGatewaySocket(t, service, 32)
			status, headers, wire := fileHTTPRequest(t, client, origin, "POST", gatewayFilesPath+"read", `{"path":"/workspace/a"}`, nil)
			if status != tc.status || !strings.Contains(string(wire), `"code":"`+tc.code+`"`) || strings.Contains(string(wire), "private-key-sentinel") || service.execs.Load() != 1 || headers.Get("Retry-After") != "" {
				t.Fatalf("terminal/error classification lost: %d %s", status, wire)
			}
			if tc.code == "exec_unknown" && (!strings.Contains(string(wire), "original-file-operation") || !strings.Contains(string(wire), "original-file-command") || !strings.Contains(string(wire), "do not automatically retry")) {
				t.Fatal("unknown lost attribution/no retry", string(wire))
			}
			if tc.name == "unknown" && !strings.Contains(string(wire), `"reason":"guard_expired"`) {
				t.Fatal("unknown lost typed cause", string(wire))
			}
		})
	}
}

func TestGatewayFileOutputOverflowJoinsOriginalExec(t *testing.T) {
	for _, stderrOverflow := range []bool{false, true} {
		t.Run(map[bool]string{false: "stdout", true: "stderr"}[stderrOverflow], func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
			service.execCall = func(ctx context.Context, _, _ string, _ p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
				writer, size := stdout, sandboxfiles.MaxOutputBytes+1
				if stderrOverflow {
					writer, size = stderr, sandboxfiles.MaxStderrBytes+1
				}
				if _, err := writer.Write(bytes.Repeat([]byte("x"), size)); err == nil {
					t.Error("output cap failed")
				}
				select {
				case <-ctx.Done():
				default:
					t.Error("overflow failed to cancel original Exec")
				}
				close(started)
				<-release
				return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, errors.Join(ctx.Err(), errors.New("private-key-sentinel"))
			}
			_, client, origin := fileGatewaySocket(t, service, 32)
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			done := make(chan int, 1)
			go func() {
				status, _, _ := fileHTTPRequest(t, client, origin, "POST", gatewayFilesPath+"read", `{"path":"/workspace/a"}`, nil)
				done <- status
			}()
			select {
			case <-started:
			case status := <-done:
				t.Fatalf("route bypassed original Exec: %d", status)
			case <-time.After(time.Second):
				t.Fatal("overflow did not reach service")
			}
			select {
			case <-done:
				t.Fatal("HTTP returned before original Exec joined")
			case <-time.After(30 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case status := <-done:
				if status != 502 {
					t.Fatal("overflow manufactured success", status)
				}
			case <-time.After(time.Second):
				t.Fatal("original Exec did not join")
			}
		})
	}
}

func TestGatewayFileReadSpecialPathHeaders(t *testing.T) {
	name := "/workspace/a\n\t.txt"
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
	service.execCall = func(_ context.Context, _, _ string, request p.ExecutionRequest, stdout, _ io.Writer) (provisioning.PlainExecResult, error) {
		var req sandboxfiles.Request
		if err := json.Unmarshal(request.Stdin, &req); err != nil || req.Path != name {
			t.Error("special path changed before execution", req.Path, err)
		}
		if err := json.NewEncoder(stdout).Encode(sandboxfiles.Result{Content: []byte("special bytes")}); err != nil {
			t.Error(err)
		}
		return fileTerminal(t, request, 0), nil
	}
	_, client, origin := fileGatewaySocket(t, service, 1)
	body, err := json.Marshal(types.ReadFileRequest{Path: name})
	if err != nil {
		t.Fatal(err)
	}
	status, headers, wire := fileHTTPRequest(t, client, origin, "POST", gatewayFilesPath+"read", string(body), nil)
	if status != 200 || string(wire) != "special bytes" || headers.Get("X-File-Path") != url.PathEscape(name) || headers.Get("X-File-Path-Encoding") != "percent-encoded" {
		t.Fatal("special path response failed HTTP-safe roundtrip", status, headers, string(wire))
	}
}
