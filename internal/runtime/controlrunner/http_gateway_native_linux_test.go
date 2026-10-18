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
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/api/etcdgateway"
	"github.com/goairix/sandbox/internal/provisioning"
	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
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

func TestHTTPGatewayTTLNative(t *testing.T) {
	if os.Getenv("SANDBOX_HTTP_TTL_NATIVE") != "1" {
		t.Skip("requires exact owned HTTP TTL renewal Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{ttlGateway: true, detached: true})
}

func TestHTTPGatewayTTLDestroyNative(t *testing.T) {
	if os.Getenv("SANDBOX_HTTP_TTL_DESTROY_NATIVE") != "1" {
		t.Skip("requires exact owned HTTP TTL renewal and detached destroy Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{ttlGateway: true, detached: true, destroyDetached: true, destroyTerminal: true})
}

func TestHTTPGatewayTTLRecoveryNative(t *testing.T) {
	if os.Getenv("SANDBOX_HTTP_TTL_RECOVERY_NATIVE") != "1" {
		t.Skip("requires exact owned HTTP TTL completion-loss and cold-recovery Linux fixture")
	}
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{ttlGateway: true, ttlRecovery: true, detached: true})
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
	unsupported := httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPut, path, "", nil, true), 501, nil)
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

// A metadata-only expiry update cannot satisfy this test: the real launcher,
// mTLS credentials, activation and signed Exec path must survive the old TTL.
// Waiting uses the actual clock shared with the signed clock server; the test
// never injects a replacement activation or advances a fixture-only clock.
type httpNativeTTLRecovery struct {
	arm            func()
	restart        func() (*provisioning.PlainColdService, *state.Backend)
	checkInstalled func(*state.SandboxTTLPending)
}

func exerciseHTTPGatewayTTLNative(t *testing.T, ctx context.Context, realService *provisioning.PlainColdService, backend *state.Backend, onePod func(), destroy *httpDestroyNativeContext, recovery *httpNativeTTLRecovery) {
	t.Helper()
	service := &httpNativeService{PlainColdService: realService, results: make(chan execServiceNativeAnswer, 1)}
	startServer := func() (string, func()) {
		// Serve permanently closes its Gateway on return. A cold restart owns
		// a new Gateway and fresh copied settings, never a reopened old gate.
		settings := &deployment.HTTPSettings{APIKey: []byte("native-http-key"), Env: map[string]string{"PATH": "/usr/bin:/bin"}, ExecTimeoutSeconds: 5, MaxExecTimeoutSeconds: 15, MaxActive: 4, RequestsPerSecond: 100, UID: 1000, GID: 1000}
		gateway, err := etcdgateway.New(service, settings)
		require.NoError(t, err)
		settings.Zero()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		serverCtx, cancelServer := context.WithCancel(ctx)
		serverDone := make(chan error, 1)
		go func() { serverDone <- etcdgateway.Serve(serverCtx, listener, gateway) }()
		var once sync.Once
		return "http://" + listener.Addr().String(), func() {
			once.Do(func() {
				cancelServer()
				require.NoError(t, <-serverDone, "original HTTP handler owners joined")
			})
		}
	}
	origin, stopServer := startServer()
	t.Cleanup(func() { stopServer() })
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{MaxIdleConns: 2, MaxIdleConnsPerHost: 2}}
	t.Cleanup(client.CloseIdleConnections)

	// Every variant crosses the original ten-second expiry. Cold recovery and
	// destroy use ninety seconds for the new TTL; their fixture contexts remain
	// sixty and 120 seconds respectively. Ordinary renewal keeps thirty seconds.
	renewalSeconds := 30
	if destroy != nil || recovery != nil {
		renewalSeconds = 90
	}
	create := types.CreateSandboxRequest{Mode: "persistent", Timeout: 10}
	key := uuid.NewString()
	var first types.CreateSandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", key, create, true), http.StatusCreated, &first)
	require.Equal(t, 10, first.Timeout)
	require.NotNil(t, first.ExpiresAt)
	originalExpiry := *first.ExpiresAt
	path := "/api/v1/sandboxes/" + first.ID
	before, err := backend.ReadSandboxView(ctx, first.ID)
	require.NoError(t, err)
	require.NotNil(t, before)
	original := before.Records().Control
	require.NotNil(t, original.Runtime)
	onePod()

	started := time.Now().UTC()
	var renewed types.UpdateTTLResponse
	var candidate *state.SandboxTTLPending
	if recovery != nil {
		recovery.arm()
		var unknown map[string]any
		httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPut, path+"/ttl", "", types.UpdateTTLRequest{Timeout: renewalSeconds}, true), http.StatusServiceUnavailable, &unknown)
		require.Equal(t, "outcome_unknown", unknown["code"])
		pendingView, err := backend.ReadSandboxView(ctx, first.ID)
		require.NoError(t, err)
		require.True(t, pendingView.Records().Control.ExpiresAt.Equal(originalExpiry), "missing completion must not silently commit old metadata")
		candidate, err = backend.ReadSandboxTTLPending(ctx, pendingView)
		require.NoError(t, err)
		require.NotNil(t, candidate)
		require.Equal(t, renewalSeconds, candidate.Timeout)
		recovery.checkInstalled(candidate)

		// Both service and Backend objects are replaced. Let actual old business
		// authority expire before retry: only the already-installed candidate may
		// authorize narrow metadata convergence; the old capability stays dead.
		// Stop and join every old HTTP handler before replacing the service
		// pointer. Closing only the business service would not synchronize the
		// Gateway's promoted methods with this cold restart.
		stopServer()
		service.PlainColdService, backend = recovery.restart()
		origin, stopServer = startServer()
		wait := time.Until(originalExpiry.Add(2 * time.Second))
		require.Positive(t, wait)
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			t.Fatal("original TTL did not elapse before cold recovery")
		}
		timer.Stop()
		onePod()
	}
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPut, path+"/ttl", "", types.UpdateTTLRequest{Timeout: renewalSeconds}, true), http.StatusOK, &renewed)
	if candidate != nil {
		require.True(t, renewed.ExpiresAt.Equal(candidate.ExpiresAt))
		renewedView, err := backend.ReadSandboxView(ctx, first.ID)
		require.NoError(t, err)
		pending, err := backend.ReadSandboxTTLPending(ctx, renewedView)
		require.NoError(t, err)
		require.Nil(t, pending)
		current, err := backend.ReadSandboxTTLCurrent(ctx, renewedView)
		require.NoError(t, err)
		require.NotNil(t, current)
		require.Equal(t, candidate.Activation.Digest(), current.Activation.Digest(), "cold recovery cannot sign or select a replacement candidate")
		require.Equal(t, candidate.Baseline, current.Baseline)
		recovery.checkInstalled(candidate)
	}
	finished := time.Now().UTC()
	require.Equal(t, renewalSeconds, renewed.Timeout)
	require.True(t, renewed.ExpiresAt.After(originalExpiry))
	var current types.SandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), http.StatusOK, &current)
	require.Equal(t, first.ID, current.ID)
	require.Equal(t, renewalSeconds, current.Timeout)
	require.NotNil(t, current.ExpiresAt)
	require.True(t, current.ExpiresAt.Equal(renewed.ExpiresAt))
	require.True(t, current.CreatedAt.Add(time.Duration(renewalSeconds)*time.Second).Equal(renewed.ExpiresAt), "old API resets its TTL baseline")
	require.False(t, current.CreatedAt.Before(started))
	require.False(t, current.CreatedAt.After(finished))
	var replay types.CreateSandboxResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, "/api/v1/sandboxes", key, create, true), http.StatusCreated, &replay)
	require.True(t, replay.Reused)
	require.Equal(t, current, replay.SandboxResponse, "Create replay displays renewed TTL without changing original request identity")

	// Two seconds exceed the protocol's one-second authority margin. Every
	// subsequent health check, handshake and command runs after the old bound.
	wait := time.Until(originalExpiry.Add(2 * time.Second))
	if recovery == nil {
		require.Positive(t, wait, "the successful renewal must precede the old expiry")
	}
	if wait < 0 {
		wait = 0
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal("original TTL did not elapse within the fixture lifetime")
	}
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), http.StatusOK, &current)
	require.Equal(t, renewalSeconds, current.Timeout)
	command := types.ExecRequest{Language: "bash", Code: "printf 'ttl-renewed-stdout\\n'; printf 'ttl-renewed-stderr\\n' >&2; exit 7"}
	var executed types.ExecResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodPost, path+"/exec", "", command, true), http.StatusOK, &executed)
	require.Equal(t, 7, executed.ExitCode)
	require.Equal(t, "ttl-renewed-stdout\n", executed.Stdout)
	require.Equal(t, "ttl-renewed-stderr\n", executed.Stderr)
	terminal := requireHTTPNativeExec(t, ctx, service, true)
	require.Equal(t, uint32(7<<8), terminal.Receipt.RootWaitStatus())
	require.Equal(t, p.RuntimeReference(*original.Runtime), terminal.Receipt.Context().Runtime)
	after, err := backend.ReadSandboxView(ctx, first.ID)
	require.NoError(t, err)
	require.NotNil(t, after)
	updated := after.Records().Control
	require.Equal(t, *original.Runtime, *updated.Runtime)
	require.Equal(t, original.Generation, updated.Generation)
	require.Equal(t, original.DataGateEpoch, updated.DataGateEpoch)
	require.True(t, updated.ExpiresAt.Equal(renewed.ExpiresAt))
	onePod()
	require.Empty(t, service.results)
	t.Log("HTTP actual PUT/GET/Create replay crossed original TTL; same runtime signed Exec exited7 with original Wait/IO drain")
	if destroy != nil {
		httpGatewayTTLDestroyNative(t, ctx, *destroy, client, origin, key, first, updated)
		onePod()
	}
}

// Continue only the sandbox created, renewed and executed above. Ordinary
// fixture inputs locate its actual source; the existing production worker must
// authenticate everything itself. Its original quota/P watch verifies refund
// before retirement, without a test-only Settle call or replacement sandbox.
func httpGatewayTTLDestroyNative(t *testing.T, ctx context.Context, f httpDestroyNativeContext, client *http.Client, origin, idempotency string, created types.CreateSandboxResponse, current *state.SandboxControlRecord) {
	t.Helper()
	var err error
	f.workspace, err = f.backend.DetachedIdentity(f.options.Business.Principal, idempotency)
	require.NoError(t, err)
	require.Equal(t, f.workspace.Hash(), current.WorkspaceHash)
	f.request.IntentID, f.request.SandboxID = current.IntentID, created.ID
	path := "/api/v1/sandboxes/" + created.ID
	var accepted httpDestroyNativeResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodDelete, path, "", nil, true), http.StatusAccepted, &accepted)
	require.Equal(t, "destroy_pending", accepted.Code)
	require.Equal(t, provisioning.PlainColdDestroyPending, accepted.State)
	require.Equal(t, created.ID, accepted.SandboxID)
	require.Equal(t, current.Generation, accepted.Generation)
	require.Equal(t, p.RuntimeReference(*current.Runtime), accepted.Runtime)
	require.NoError(t, accepted.Task.Validate())
	httpDestroyNativeBirth(t, ctx, f, accepted)
	httpDestroyNativeAutomatic(t, ctx, f, accepted, nil)

	owner, err := f.backend.LoadOwner(ctx, f.workspace)
	require.NoError(t, err)
	require.Nil(t, owner)
	var absent types.ErrorResponse
	httpNativeRead(t, httpNativeRequest(t, client, origin, http.MethodGet, path, "", nil, true), http.StatusNotFound, &absent)
	require.Equal(t, "not_found", absent.Code)
	proof, err := f.backend.ReadSandboxReleased(ctx, created.ID)
	require.NoError(t, err)
	require.NotNil(t, proof, "the original release reader must authenticate cleanup of the renewed runtime")
	require.Positive(t, f.provider.patches.Load())
	require.EqualValues(t, 1, f.provider.deletes.Load())
	t.Logf("HTTP same sandbox=%s old TTL crossed→signed Exec→DELETE202→original managed cleanup/refund/Pgone→GET404; runtime UID=%s BootID=%s", created.ID, current.Runtime.UID, current.Runtime.BootID)
}
