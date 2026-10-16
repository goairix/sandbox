//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
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

func TestHTTPGatewayNative(t *testing.T) {
	if os.Getenv("SANDBOX_HTTP_GATEWAY_NATIVE") != "1" {
		t.Skip("requires exact owned HTTP gateway Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{httpGateway: true, detached: true})
}

// Observe the real service return without replacing any runtime, bridge, Wait,
// credential, admission or writer operation. Each bounded scenario consumes it.
type httpNativeService struct {
	*provisioning.PlainColdService
	results chan execServiceNativeAnswer
}

func (s *httpNativeService) Exec(ctx context.Context, sandboxID, requestID string, request p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
	result, err := s.PlainColdService.Exec(ctx, sandboxID, requestID, request, stdout, stderr)
	s.results <- execServiceNativeAnswer{result: result, err: err}
	return result, err
}

func requireHTTPNativeExec(t *testing.T, ctx context.Context, service *httpNativeService, terminal bool) provisioning.PlainExecResult {
	t.Helper()
	select {
	case answer := <-service.results:
		if terminal {
			require.NoError(t, answer.err)
			require.Equal(t, provisioning.PlainExecLocalTerminal, answer.result.State)
			require.Equal(t, "local_terminal", answer.result.Receipt.State())
			require.True(t, answer.result.Receipt.DrainConfirmed())
		} else {
			require.Error(t, answer.err)
			require.Equal(t, provisioning.PlainExecUnknown, answer.result.State)
			require.NotEqual(t, "local_terminal", answer.result.Receipt.State())
		}
		require.NoError(t, answer.result.Operation.Validate())
		require.NoError(t, answer.result.Effect.Validate())
		return answer.result
	case <-ctx.Done():
		t.Fatal("original HTTP Exec/renewal/Wait/IO owner did not join")
		return provisioning.PlainExecResult{}
	}
}

func httpNativeRequest(t *testing.T, client *http.Client, origin, method, path, key string, body any, authenticated bool) *http.Response {
	t.Helper()
	wire, err := json.Marshal(body)
	require.NoError(t, err)
	r, err := http.NewRequest(method, origin+path, bytes.NewReader(wire))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	if authenticated {
		r.Header.Set("Authorization", "Bearer native-http-key")
	}
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	response, err := client.Do(r)
	require.NoError(t, err)
	return response
}

func httpNativeRead(t *testing.T, response *http.Response, status int, output any) []byte {
	t.Helper()
	defer response.Body.Close()
	wire, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 2<<20)
	require.Equal(t, status, response.StatusCode, "%s", wire)
	if output != nil {
		require.NoError(t, json.Unmarshal(wire, output))
	}
	return wire
}

type httpNativeEvent struct {
	event string
	data  json.RawMessage
}

func httpNativeNextEvent(t *testing.T, reader *bufio.Reader) httpNativeEvent {
	t.Helper()
	var event httpNativeEvent
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err, "original HTTP stream ended before an expected event")
		require.LessOrEqual(t, len(line), 128<<10)
		line = strings.TrimSuffix(line, "\n")
		if line == "" {
			if event.event != "" {
				return event
			}
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			event.event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			event.data = json.RawMessage(strings.TrimPrefix(line, "data: "))
		}
	}
}

func exerciseHTTPGatewayNative(t *testing.T, ctx context.Context, realService *provisioning.PlainColdService, hooks *execServiceNativeHooks, zeroPod func(), onePod func()) {
	t.Helper()
	service := &httpNativeService{PlainColdService: realService, results: make(chan execServiceNativeAnswer, 8)}
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
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2}}
	t.Cleanup(client.CloseIdleConnections)
	origin := "http://" + listener.Addr().String()
	create := types.CreateSandboxRequest{Mode: "persistent"}
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", "", create, false), 401, nil)
	zeroPod()
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, "/ready", "", nil, false), 200, nil)
	key := uuid.NewString()
	response := httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", key, create, true)
	require.Equal(t, key, response.Header.Get("Idempotency-Key"))
	var first types.CreateSandboxResponse
	httpNativeRead(t, response, 201, &first)
	require.False(t, first.Reused)
	require.Equal(t, "running", first.State)
	require.Equal(t, "persistent", first.Mode)
	require.Equal(t, 60, first.Timeout)
	require.NotZero(t, first.CreatedAt)
	require.NotNil(t, first.ExpiresAt)
	onePod()
	var replay types.CreateSandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", key, create, true), 201, &replay)
	require.Equal(t, first.ID, replay.ID)
	require.True(t, replay.Reused)
	require.Equal(t, first.CreatedAt, replay.CreatedAt)
	conflict := create
	conflict.Timeout = 61
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", key, conflict, true), 409, nil)
	var current types.SandboxResponse
	path := "/api/v1/sandboxes/" + first.ID
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), 200, &current)
	require.Equal(t, first.SandboxResponse, current)
	unsupported := httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodDelete, path, "", nil, true), 501, nil)
	require.Contains(t, string(unsupported), `"code":"unsupported"`)
	execPath := path + "/exec"
	command := types.ExecRequest{Language: "bash", Code: "printf 'http-stdout\\n'; printf 'http-stderr\\n' >&2; exit 7"}
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, execPath, "not-exec-dedup", command, true), 400, nil)
	var syncResult types.ExecResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, execPath, "", command, true), 200, &syncResult)
	require.Equal(t, 7, syncResult.ExitCode)
	require.Equal(t, "http-stdout\n", syncResult.Stdout)
	require.Equal(t, "http-stderr\n", syncResult.Stderr)
	terminal := requireHTTPNativeExec(t, ctx, service, true)
	require.Equal(t, uint32(7<<8), terminal.Receipt.RootWaitStatus())
	response = httpNativeRequest(t, client, origin, http.MethodPost, execPath+"/stream", "", command, true)
	require.Equal(t, 200, response.StatusCode)
	require.Contains(t, response.Header.Get("Content-Type"), "text/event-stream")
	reader := bufio.NewReader(response.Body)
	var stdout, stderr strings.Builder
	for {
		event := httpNativeNextEvent(t, reader)
		if event.event == "done" {
			var done types.SSEDoneData
			require.NoError(t, json.Unmarshal(event.data, &done))
			require.Equal(t, 7, done.ExitCode)
			break
		}
		require.NotEqual(t, "error", event.event, "%s", event.data)
		var output types.SSEStdoutData
		require.NoError(t, json.Unmarshal(event.data, &output))
		if event.event == "stdout" {
			stdout.WriteString(output.Content)
		} else if event.event == "stderr" {
			stderr.WriteString(output.Content)
		}
	}
	response.Body.Close()
	require.Equal(t, "http-stdout\n", stdout.String())
	require.Equal(t, "http-stderr\n", stderr.String())
	requireHTTPNativeExec(t, ctx, service, true)

	short := types.ExecRequest{Language: "bash", Code: "printf 'http-disconnect-started\\n'; /bin/sleep 2", Timeout: 5}
	hooks.failNext.Store(true)
	response = httpNativeRequest(t, client, origin, http.MethodPost, execPath+"/stream", "", short, true)
	reader = bufio.NewReader(response.Body)
	var disconnectOutput strings.Builder
	for i := 0; i < 16 && !strings.Contains(disconnectOutput.String(), "http-disconnect-started"); i++ {
		event := httpNativeNextEvent(t, reader)
		require.NotEqual(t, "error", event.event, "%s", event.data)
		require.NotEqual(t, "done", event.event, "command finished before the actual disconnect proof")
		if event.event == "stdout" {
			var output types.SSEStdoutData
			require.NoError(t, json.Unmarshal(event.data, &output))
			require.LessOrEqual(t, disconnectOutput.Len()+len(output.Content), 64<<10)
			disconnectOutput.WriteString(output.Content)
		}
	}
	require.Contains(t, disconnectOutput.String(), "http-disconnect-started")
	response.Body.Close()
	requireHTTPNativeExec(t, ctx, service, false)

	// Physically lose one marked original bridge after actual command output.
	// The authenticated HTTP error must retain that same operation/effect.
	hooks.mu.Lock()
	previous := make(map[*startupBridgeFixture]bool, len(hooks.failed))
	for bridge := range hooks.failed {
		previous[bridge] = true
	}
	hooks.mu.Unlock()
	hooks.failNext.Store(true)
	response = httpNativeRequest(t, client, origin, http.MethodPost, execPath+"/stream", "", short, true)
	reader = bufio.NewReader(response.Body)
	require.Equal(t, "stdout", httpNativeNextEvent(t, reader).event)
	hooks.mu.Lock()
	var lost *startupBridgeFixture
	for bridge := range hooks.failed {
		if !previous[bridge] {
			lost = bridge
		}
	}
	hooks.mu.Unlock()
	require.NotNil(t, lost)
	lost.Close()
	event := httpNativeNextEvent(t, reader)
	require.Equal(t, "error", event.event, "bridge loss must never produce done")
	require.Contains(t, string(event.data), "exec_unknown")
	unknown := requireHTTPNativeExec(t, ctx, service, false)
	require.Contains(t, string(event.data), unknown.Operation.OperationID)
	require.Contains(t, string(event.data), unknown.Effect.CommandID)
	response.Body.Close()

	hooks.failNext.Store(true)
	response = httpNativeRequest(t, client, origin, http.MethodPost, execPath+"/stream", "", short, true)
	require.Equal(t, "stdout", httpNativeNextEvent(t, bufio.NewReader(response.Body)).event)
	cancelServer()
	select {
	case err := <-serverDone:
		serverJoined = true
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("HTTP server shutdown did not physically join handlers")
	}
	response.Body.Close()
	requireHTTPNativeExec(t, ctx, service, false)
	require.NoError(t, realService.Close())
	onePod()
	require.Empty(t, service.results)
	t.Log("HTTP actual TLS-etcd/formal-launcher create/replay/conflict/current health/sync exit7/SSE terminal/disconnect/bridge unknown/shutdown joined")
}
