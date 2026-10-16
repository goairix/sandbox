package etcdgateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning"
	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

func downloadSettings(t *testing.T) *deployment.HTTPSettings {
	t.Helper()
	settings := gatewaySettings()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	settings.FileTransfer = &deployment.FileTransferSettings{Directory: dir, MaxFileBytes: 2 << 20, MaxTotalBytes: 4 << 20, MaxActive: 1, TimeoutSeconds: 3}
	return settings
}

func downloadSocket(t *testing.T, service Service, settings *deployment.HTTPSettings) (*Gateway, *http.Client, string) {
	t.Helper()
	gateway, err := New(service, settings)
	require.NoError(t, err)
	server := httptest.NewServer(gateway)
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(func() { client.CloseIdleConnections(); server.Close(); _ = gateway.Close() })
	return gateway, client, server.URL
}

// Detects early HTTP200, more than one Exec, a changed fixed descriptor, and
// retaining caller-owned transfer settings. Receipt creation is genuinely signed.
func TestGatewayDownloadSingleExecBeforeSuccessAndOwnedProfile(t *testing.T) {
	settings := downloadSettings(t)
	directory := settings.FileTransfer.Directory
	data := bytes.Repeat([]byte{0, 255, 128, '\n'}, (1<<18)+9)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
	service.execCall = func(ctx context.Context, id, requestID string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
		require.Equal(t, gatewaySandboxID, id)
		require.Equal(t, "download-request", requestID)
		require.Equal(t, []string{"/usr/local/bin/sandbox-launcher", "file-download"}, request.Argv)
		require.Equal(t, []byte(`{"path":"/tmp/data.bin","max_bytes":2097152}`), request.Stdin)
		require.Equal(t, uint32(1000), request.UID)
		require.Equal(t, uint32(1001), request.GID)
		require.Equal(t, map[string]string{"PATH": "/usr/bin:/bin", "BASE": "operator"}, request.Env)
		require.Equal(t, "/workspace", request.WorkDir)
		require.Equal(t, uint32(3), request.TimeoutSeconds)
		require.False(t, request.RequiresNetwork)
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.InDelta(t, 3.0, time.Until(deadline).Seconds(), 0.5)
		_, err := stdout.Write(data)
		require.NoError(t, err)
		close(started)
		<-release
		return fileTerminal(t, request, 0), nil
	}
	_, client, origin := downloadSocket(t, service, settings)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	settings.FileTransfer.Directory = "/changed"
	settings.FileTransfer.MaxFileBytes = 1
	settings.FileTransfer.TimeoutSeconds = 1
	settings.Env["BASE"] = "changed"
	response := make(chan *http.Response, 1)
	go func() {
		r, _ := http.NewRequest("GET", origin+gatewayFilesPath+"download?path=/tmp/data.bin", nil)
		r.Header.Set("Authorization", "Bearer mounted-secret")
		r.Header.Set("X-Request-ID", "download-request")
		answer, err := client.Do(r)
		if err != nil {
			t.Error(err)
		}
		response <- answer
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("original Exec did not start")
	}
	select {
	case <-response:
		t.Fatal("HTTP response preceded original signed terminal/Wait return")
	case <-time.After(30 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	answer := <-response
	require.NotNil(t, answer)
	defer answer.Body.Close()
	require.Equal(t, 200, answer.StatusCode)
	require.Equal(t, int64(len(data)), answer.ContentLength)
	require.Equal(t, "application/octet-stream", answer.Header.Get("Content-Type"))
	require.Contains(t, answer.Header.Get("Content-Disposition"), "attachment;")
	got, err := io.ReadAll(answer.Body)
	require.NoError(t, err)
	require.Equal(t, data, got)
	require.Equal(t, int32(1), service.execs.Load())
	entries, err := os.ReadDir(directory)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestGatewayDownloadTypedFailureAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		exit    int
		stderr  string
		unknown bool
		want    int
		code    string
	}{
		{"invalid", 1, `{"code":"invalid_request"}`, false, 400, "invalid_request"},
		{"not-found", 1, `{"code":"not_found"}`, false, 404, "not_found"},
		{"limit", 1, `{"code":"limit_exceeded"}`, false, 413, "limit_exceeded"},
		{"type", 1, `{"code":"unsupported_type"}`, false, 422, "unsupported_type"},
		{"permission", 1, `{"code":"permission_denied"}`, false, 422, "permission_denied"},
		{"io", 1, `{"code":"io_error"}`, false, 500, "io_error"},
		{"malformed", 1, `private-secret`, false, 502, "file_helper_failed"},
		{"duplicate", 1, `{"code":"not_found","code":"io_error"}`, false, 502, "file_helper_failed"},
		{"other-exit", 2, `{"code":"not_found"}`, false, 502, "file_helper_failed"},
		{"unknown", 0, `{"code":"not_found"}`, true, 502, "exec_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
			service.execCall = func(_ context.Context, _, _ string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
				_, _ = stdout.Write([]byte("partial must be discarded"))
				_, _ = stderr.Write([]byte(tc.stderr))
				result := fileTerminal(t, request, tc.exit)
				if tc.unknown {
					result.State = provisioning.PlainExecUnknown
					return result, context.Canceled
				}
				return result, nil
			}
			_, client, origin := downloadSocket(t, service, downloadSettings(t))
			status, _, wire := fileHTTPRequest(t, client, origin, "GET", gatewayFilesPath+"download?path=/tmp/data", "", nil)
			require.Equal(t, tc.want, status)
			require.Contains(t, string(wire), `"code":"`+tc.code+`"`)
			require.NotContains(t, string(wire), "partial must")
			require.NotContains(t, string(wire), "private-secret")
			if tc.unknown {
				require.Contains(t, string(wire), `"operation"`)
				require.Contains(t, string(wire), `"effect"`)
				require.Contains(t, string(wire), `"reason":"canceled"`)
			}
			require.Equal(t, int32(1), service.execs.Load())
		})
	}
}

func TestGatewayDownloadAdmissionAndDeadlineBeforeEffects(t *testing.T) {
	settings := downloadSettings(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
	service.execCall = func(ctx context.Context, _, _ string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
		close(started)
		<-release
		return fileTerminal(t, request, 0), nil
	}
	gateway, client, origin := downloadSocket(t, service, settings)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	recorder := httptest.NewRecorder()
	gateway.ServeHTTP(recorder, gatewayRequest("GET", gatewayFilesPath+"download?path=/tmp/data", ""))
	require.Equal(t, 503, recorder.Code)
	require.Zero(t, service.execs.Load(), "real socket deadline capability is required before reserve/Exec")
	done := make(chan int, 1)
	go func() {
		status, _, _ := fileHTTPRequest(t, client, origin, "GET", gatewayFilesPath+"download?path=/tmp/data", "", nil)
		done <- status
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("download did not start")
	}
	status, _, _ := fileHTTPRequest(t, client, origin, "GET", gatewayFilesPath+"download?path=/tmp/another", "", nil)
	require.Equal(t, 429, status)
	require.Equal(t, int32(1), service.execs.Load())
	once.Do(func() { close(release) })
	require.Equal(t, 200, <-done)
}

// A failed disk identity and over-budget stdout must poison readiness, preserve
// health, discard bytes, and expose cached cleanup error to lifecycle callers.
func TestGatewayDownloadSpoolFailuresLatchReadyAndCloseError(t *testing.T) {
	for _, scenario := range []string{"identity", "stdout-limit", "stderr-limit"} {
		t.Run(scenario, func(t *testing.T) {
			settings := downloadSettings(t)
			directory := settings.FileTransfer.Directory
			service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
			service.execCall = func(ctx context.Context, _, _ string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
				if scenario == "identity" {
					_, err := stdout.Write([]byte("data"))
					require.NoError(t, err)
					entries, err := os.ReadDir(directory)
					require.NoError(t, err)
					require.Len(t, entries, 1)
					name := filepath.Join(directory, entries[0].Name())
					require.NoError(t, os.Rename(name, name+"-original"))
					require.NoError(t, os.WriteFile(name, []byte("replacement"), 0600))
				} else {
					writer, size := stdout, (2<<20)+1
					if scenario == "stderr-limit" {
						writer, size = stderr, (64<<10)+1
					}
					_, err := writer.Write(bytes.Repeat([]byte("x"), size))
					require.Error(t, err)
					require.ErrorIs(t, ctx.Err(), context.Canceled)
				}
				return fileTerminal(t, request, 0), nil
			}
			gateway, client, origin := downloadSocket(t, service, settings)
			status, _, _ := fileHTTPRequest(t, client, origin, "GET", gatewayFilesPath+"download?path=/tmp/data", "", nil)
			require.Equal(t, 502, status)
			if scenario != "stderr-limit" {
				status, _, _ = fileHTTPRequest(t, client, origin, "GET", "/ready", "", nil)
				require.Equal(t, 503, status)
				status, _, _ = fileHTTPRequest(t, client, origin, "GET", "/health", "", nil)
				require.Equal(t, 200, status)
				closeErr := gateway.Close()
				require.Error(t, closeErr)
				require.Equal(t, closeErr, gateway.Close())
			}
		})
	}
}

func TestGatewayDownloadCloseAndCancellationPhysicallyJoin(t *testing.T) {
	settings := downloadSettings(t)
	started, release, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
	service.execCall = func(ctx context.Context, _, _ string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
		_, _ = stdout.Write([]byte("staged"))
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, ctx.Err()
	}
	gateway, client, origin := downloadSocket(t, service, settings)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	done := make(chan error, 1)
	go func() {
		r, _ := http.NewRequest("GET", origin+gatewayFilesPath+"download?path=/tmp/data", nil)
		r.Header.Set("Authorization", "Bearer mounted-secret")
		response, err := client.Do(r)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("original Exec did not start")
	}
	closed := make(chan error, 1)
	go func() { closed <- gateway.Close() }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel original Exec")
	}
	select {
	case <-closed:
		t.Fatal("Close skipped original Wait/IO join")
	default:
	}
	entries, err := os.ReadDir(settings.FileTransfer.Directory)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	once.Do(func() { close(release) })
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Close did not join released owner")
	}
	<-done
	entries, err = os.ReadDir(settings.FileTransfer.Directory)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestGatewayDownloadClientCancellationWaitsForOriginalOwner(t *testing.T) {
	settings := downloadSettings(t)
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
	service.execCall = func(ctx context.Context, _, _ string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
		_, _ = stdout.Write([]byte("staged"))
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, ctx.Err()
	}
	gateway, client, origin := downloadSocket(t, service, settings)
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		request, _ := http.NewRequestWithContext(ctx, "GET", origin+gatewayFilesPath+"download?path=/tmp/data", nil)
		request.Header.Set("Authorization", "Bearer mounted-secret")
		response, err := client.Do(request)
		if response != nil {
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("original Exec did not start")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("disconnect did not cancel original Exec")
	}
	entries, err := os.ReadDir(settings.FileTransfer.Directory)
	require.NoError(t, err)
	require.Len(t, entries, 1, "disconnect cannot release the unjoined owner's spool")
	closed := make(chan error, 1)
	go func() { closed <- gateway.Close() }()
	select {
	case <-closed:
		t.Fatal("client cancellation was mistaken for physical owner join")
	default:
	}
	once.Do(func() { close(release) })
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("released owner did not join")
	}
	require.Error(t, <-done)
}

func TestGatewayDownloadServeReturnsCleanupError(t *testing.T) {
	settings := downloadSettings(t)
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
	service.execCall = func(ctx context.Context, _, _ string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
		_, err := stdout.Write(bytes.Repeat([]byte("x"), (2<<20)+1))
		require.Error(t, err)
		return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, ctx.Err()
	}
	gateway, err := New(service, settings)
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, gateway) }()
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	status, _, _ := fileHTTPRequest(t, client, "http://"+listener.Addr().String(), "GET", gatewayFilesPath+"download?path=/tmp/data", "", nil)
	require.Equal(t, 502, status)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err, "production Serve must expose transfer cleanup failure")
		require.Error(t, gateway.Close())
	case <-time.After(2 * time.Second):
		t.Fatal("Serve failed to physically join original handler and store")
	}
}

// Real socket backpressure must produce a bounded truncated body, release the
// owner and make its transfer slot available again without a background copy.
func TestGatewayDownloadSlowSocketDeadline(t *testing.T) {
	settings := downloadSettings(t)
	settings.FileTransfer.MaxFileBytes, settings.FileTransfer.MaxTotalBytes = 16<<20, 16<<20
	settings.FileTransfer.TimeoutSeconds = 1
	service := &fileServiceFake{gatewayServiceFake: &gatewayServiceFake{}}
	service.execCall = func(_ context.Context, _, _ string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
		block := bytes.Repeat([]byte("x"), 32768)
		for i := 0; i < 512; i++ {
			if _, err := stdout.Write(block); err != nil {
				return provisioning.PlainExecResult{}, err
			}
		}
		return fileTerminal(t, request, 0), nil
	}
	gateway, _, origin := downloadSocket(t, service, settings)
	connection, err := net.Dial("tcp", strings.TrimPrefix(origin, "http://"))
	require.NoError(t, err)
	defer connection.Close()
	_, err = io.WriteString(connection, "GET "+gatewayFilesPath+"download?path=/tmp/data HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer mounted-secret\r\n\r\n")
	require.NoError(t, err)
	require.NoError(t, connection.SetReadDeadline(time.Now().Add(4*time.Second)))
	// Do not consume body until the total delivery deadline has passed.
	timer := time.NewTimer(1500 * time.Millisecond)
	<-timer.C
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	require.NoError(t, err)
	require.Equal(t, 200, response.StatusCode)
	got, err := io.Copy(io.Discard, response.Body)
	require.Error(t, err, "Content-Length must expose a deadline-truncated body")
	require.Less(t, got, int64(16<<20))
	require.NoError(t, response.Body.Close())
	require.NoError(t, gateway.Close())
	entries, err := os.ReadDir(settings.FileTransfer.Directory)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestGatewayDownloadNewRejectsInvalidDirectSettings(t *testing.T) {
	settings := downloadSettings(t)
	settings.FileTransfer.TimeoutSeconds = 601
	_, err := New(&gatewayServiceFake{}, settings)
	require.Error(t, err)
	settings.FileTransfer.TimeoutSeconds = 3
	settings.FileTransfer.MaxFileBytes = 0
	_, err = New(&gatewayServiceFake{}, settings)
	require.Error(t, err)
}

type downloadDeadlineFailureWriter struct{ *httptest.ResponseRecorder }

func (*downloadDeadlineFailureWriter) SetWriteDeadline(time.Time) error {
	return errors.New("private socket failure")
}

// An untouched writer defaults to HTTP200 when the handler returns, even if no
// file bytes were delivered. Preheader failures must explicitly commit an error.
func TestGatewayDownloadPreheaderFailureHasExplicitStatus(t *testing.T) {
	for _, scenario := range []string{"canceled", "deadline-capability"} {
		t.Run(scenario, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			var writer http.ResponseWriter = recorder
			ctx := context.Background()
			if scenario == "canceled" {
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
			} else {
				writer = &downloadDeadlineFailureWriter{ResponseRecorder: recorder}
			}
			deliverDownload(ctx, writer, http.NewResponseController(writer), nil, "binary.bin", 7)
			require.Equal(t, 503, recorder.Result().StatusCode)
			require.Contains(t, recorder.Body.String(), `"code":"download_unavailable"`)
			require.NotContains(t, recorder.Body.String(), "private socket failure")
			require.Empty(t, recorder.Header().Get("Content-Length"))
		})
	}
}
