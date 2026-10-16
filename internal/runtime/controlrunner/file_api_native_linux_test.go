//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/api/etcdgateway"
	"github.com/goairix/sandbox/internal/provisioning"
	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestFileAPINative(t *testing.T) {
	if os.Getenv("SANDBOX_FILE_API_NATIVE") != "1" {
		t.Skip("requires exact owned file API Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{fileAPI: true})
}

// Observe the original service without replacing its execution, credentials,
// target, renewal, wait receipt, stdout writer or IO join.
type fileNativeService struct {
	*httpNativeService
	t         *testing.T
	fileCalls int
}

func (s *fileNativeService) Exec(ctx context.Context, sandboxID, requestID string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
	file := len(request.Argv) == 2 && request.Argv[1] == "file-operation"
	if file {
		s.fileCalls++
		require.Equal(s.t, []string{"/usr/local/bin/sandbox-launcher", "file-operation"}, request.Argv)
		require.Equal(s.t, uint32(1000), request.UID)
		require.Equal(s.t, uint32(1000), request.GID)
		require.Equal(s.t, "/workspace", request.WorkDir)
		require.Equal(s.t, map[string]string{"PATH": "/usr/bin:/bin"}, request.Env)
		require.False(s.t, request.RequiresNetwork)
		require.False(s.t, request.TTY)
	}
	result, err := s.httpNativeService.Exec(ctx, sandboxID, requestID, request, stdout, stderr)
	if file {
		descriptor, descriptorErr := p.NewExecutionDescriptor(request)
		require.NoError(s.t, descriptorErr)
		require.Equal(s.t, descriptor.Digest(), result.Receipt.DescriptorDigest(), "actual signed receipt binds the exact helper UID/GID/argv/stdin")
		require.Equal(s.t, sandboxID, result.Receipt.Context().SandboxID)
	}
	return result, err
}

func fileNativeRequest(t *testing.T, client *http.Client, origin, method, path string, body any) *http.Response {
	t.Helper()
	var input io.Reader
	if body != nil {
		wire, err := json.Marshal(body)
		require.NoError(t, err)
		input = bytes.NewReader(wire)
	}
	request, err := http.NewRequest(method, origin+path, input)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer native-http-key")
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	require.NoError(t, err)
	return response
}

func exerciseFileAPINative(t *testing.T, ctx context.Context, realService *provisioning.PlainColdService, sandboxID string) {
	t.Helper()
	service := &fileNativeService{httpNativeService: &httpNativeService{PlainColdService: realService, results: make(chan execServiceNativeAnswer, 2)}, t: t}
	settings := &deployment.HTTPSettings{APIKey: []byte("native-http-key"), Env: map[string]string{"PATH": "/usr/bin:/bin"}, ExecTimeoutSeconds: 5, MaxExecTimeoutSeconds: 15, MaxActive: 4, RequestsPerSecond: 100, UID: 1000, GID: 1000}
	gateway, err := etcdgateway.New(service, settings)
	require.NoError(t, err)
	settings.Zero()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	serverCtx, cancelServer := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	go func() { serverDone <- etcdgateway.Serve(serverCtx, listener, gateway) }()
	serverJoined := false
	t.Cleanup(func() {
		cancelServer()
		if !serverJoined {
			require.NoError(t, <-serverDone)
		}
	})
	client := &http.Client{Timeout: 15 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	origin := "http://" + listener.Addr().String()
	base := "/api/v1/sandboxes/" + sandboxID + "/files/"
	const dir = "/workspace/file-api"
	terminal := func() {
		t.Helper()
		result := requireHTTPNativeExec(t, ctx, service.httpNativeService, true)
		require.Zero(t, result.Receipt.RootWaitStatus())
	}
	runOriginal := func(command string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		result, err := service.Exec(ctx, sandboxID, uuid.NewString(), nativeExecRequest(command, 5), &stdout, &stderr)
		require.NoError(t, err)
		require.Equal(t, provisioning.PlainExecLocalTerminal, result.State)
		require.Empty(t, stderr.String())
		terminal()
		return stdout.String()
	}
	// The same actual UID/GID used by the helper can create real files but
	// cannot read formal launcher's protected control material.
	seed := `set -eu
test "$(id -u)" = 1000
test "$(id -g)" = 1000
test ! -r /run/sandbox-control/config.json
mkdir -p /workspace/file-api/sub
printf 'alpha\n\nomega' > /workspace/file-api/text.txt
printf '\000\377\200x' > /workspace/file-api/binary.bin
printf inner > /workspace/file-api/sub/inner.txt
special=$(printf 'line\n\t.txt')
printf special > "/workspace/file-api/$special"
bad=$(printf '\377')
printf invalid > "/workspace/file-api/$bad"
printf 'uid1000 gid1000 control-unreadable files-created\n'
`
	require.Equal(t, "uid1000 gid1000 control-unreadable files-created\n", runOriginal(seed))
	// APFS rejects this filename before helper execution. Here Linux really
	// creates it and the real helper returns a signed terminal application422.
	wire := httpNativeRead(t, fileNativeRequest(t, client, origin, "GET", base+"list?path="+url.QueryEscape(dir), nil), 422, nil)
	require.Contains(t, string(wire), `"code":"unsupported_type"`)
	terminal()
	runOriginal(`set -eu; bad=$(printf '\377'); rm -- "/workspace/file-api/$bad"`)

	var list types.FileListResponse
	httpNativeRead(t, fileNativeRequest(t, client, origin, "GET", base+"list?path="+url.QueryEscape(dir), nil), 200, &list)
	terminal()
	require.Equal(t, dir, list.Path)
	require.Len(t, list.Files, 4)
	require.Equal(t, "binary.bin", list.Files[0].Name)
	require.Equal(t, "line\n\t.txt", list.Files[1].Name)
	require.True(t, list.Files[2].IsDir)

	var recursive types.ListFilesRecursiveResponse
	httpNativeRead(t, fileNativeRequest(t, client, origin, "POST", base+"list-recursive", types.ListFilesRecursiveRequest{Path: dir, Page: 2, PageSize: 2}), 200, &recursive)
	terminal()
	require.Equal(t, 5, recursive.TotalCount)
	require.Equal(t, 2, recursive.Page)
	require.Equal(t, 2, recursive.PageSize)
	require.Len(t, recursive.Files, 2)
	require.Equal(t, dir+"/sub/inner.txt", recursive.Files[1].Path)

	var glob types.GlobFilesResponse
	httpNativeRead(t, fileNativeRequest(t, client, origin, "POST", base+"glob", types.GlobFilesRequest{Path: dir, Pattern: "sub/*.txt"}), 200, &glob)
	terminal()
	require.Equal(t, 1, glob.TotalCount)
	require.Equal(t, "sub/*.txt", glob.Pattern)
	require.Len(t, glob.Files, 1)
	require.Equal(t, "inner.txt", glob.Files[0].Name)

	response := fileNativeRequest(t, client, origin, "POST", base+"read", types.ReadFileRequest{Path: dir + "/binary.bin"})
	require.Equal(t, dir+"/binary.bin", response.Header.Get("X-File-Path"))
	require.Equal(t, "text/plain; charset=utf-8", response.Header.Get("Content-Type"))
	require.Equal(t, []byte{0, 255, 128, 'x'}, httpNativeRead(t, response, 200, nil))
	terminal()

	var lines types.ReadFileLinesResponse
	httpNativeRead(t, fileNativeRequest(t, client, origin, "POST", base+"read-lines", types.ReadFileLinesRequest{Path: dir + "/text.txt"}), 200, &lines)
	terminal()
	require.Equal(t, []string{"alpha", "", "omega"}, lines.Lines)
	require.Equal(t, 3, lines.TotalLines)
	require.Equal(t, 1, lines.StartLine)
	require.Equal(t, 3, lines.EndLine)

	var edit types.EditFileResponse
	httpNativeRead(t, fileNativeRequest(t, client, origin, "POST", base+"edit", types.EditFileRequest{Path: dir + "/text.txt", OldStr: "alpha", NewStr: "beta"}), 200, &edit)
	terminal()
	require.Equal(t, "ok", edit.Message)
	httpNativeRead(t, fileNativeRequest(t, client, origin, "POST", base+"edit-lines", types.EditFileLinesRequest{Path: dir + "/text.txt", StartLine: 2, EndLine: 2, NewContent: "middle\n"}), 200, &edit)
	terminal()
	require.Equal(t, "ok", edit.Message)

	response = fileNativeRequest(t, client, origin, "GET", base+"download?path="+url.QueryEscape(dir+"/text.txt"), nil)
	require.True(t, strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment;"))
	require.Equal(t, "application/octet-stream", response.Header.Get("Content-Type"))
	require.Equal(t, "beta\nmiddle\nomega", string(httpNativeRead(t, response, 200, nil)))
	terminal()
	require.Equal(t, 9, service.fileCalls, "eight successful file routes plus actual invalid-name rejection")
	require.Empty(t, service.results)
	cancelServer()
	select {
	case err := <-serverDone:
		serverJoined = true
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("file API HTTP handlers did not physically join")
	}
	t.Log("file API actual TLS-etcd/formal-launcher UID/GID1000 protected-control denial, Linux invalid-name422, eight routes, binary/line/edit bytes, signed terminal/drain and original Wait/IO joined")
}
