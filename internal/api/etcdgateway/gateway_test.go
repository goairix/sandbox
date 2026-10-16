package etcdgateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning"
	"github.com/goairix/sandbox/internal/provisioning/deployment"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/sandboxsnapshot"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
)

const gatewaySandboxID = "00000000-0000-4000-8000-000000000001"
const gatewayExecPath = "/api/v1/sandboxes/" + gatewaySandboxID + "/exec"

type gatewayServiceFake struct {
	creates atomic.Int32
	lookups atomic.Int32
	execs   atomic.Int32
	create  func(provisioning.PlainColdBusinessInput) (provisioning.PlainColdBusinessResult, error)
	lookup  func() (*provisioning.PlainColdLookupResult, error)
	exec    func(context.Context, p.ExecutionRequest, io.Writer, io.Writer) (provisioning.PlainExecResult, error)
}

func (s *gatewayServiceFake) CreateBusiness(_ context.Context, in provisioning.PlainColdBusinessInput) (provisioning.PlainColdBusinessResult, error) {
	s.creates.Add(1)
	return s.create(in)
}

func (s *gatewayServiceFake) Lookup(context.Context, string) (*provisioning.PlainColdLookupResult, error) {
	s.lookups.Add(1)
	return s.lookup()
}

func (s *gatewayServiceFake) Exec(ctx context.Context, _, _ string, in p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
	s.execs.Add(1)
	return s.exec(ctx, in, stdout, stderr)
}

func (*gatewayServiceFake) Ready(context.Context) error { return nil }

func gatewaySettings() *deployment.HTTPSettings {
	return &deployment.HTTPSettings{APIKey: []byte("mounted-secret"), Env: map[string]string{"PATH": "/usr/bin:/bin", "BASE": "operator"}, ExecTimeoutSeconds: 30, MaxExecTimeoutSeconds: 600, MaxActive: 32, RequestsPerSecond: 10000, UID: 1000, GID: 1001}
}

func gatewayFixture(t *testing.T, service *gatewayServiceFake) *Gateway {
	t.Helper()
	g, err := New(service, gatewaySettings())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return g
}

func gatewayRequest(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer mounted-secret")
	r.Header.Set("Content-Type", "application/json")
	return r
}

// These exercise real handler validation: malformed input must cause no service effects.
func TestGatewayAuthUnsupportedAndStrictBody(t *testing.T) {
	s := &gatewayServiceFake{}
	g := gatewayFixture(t, s)
	unauth := httptest.NewRecorder()
	g.ServeHTTP(unauth, httptest.NewRequest(http.MethodPost, "/api/v1/sandboxes", strings.NewReader(`{"mode":"ephemeral"}`)))
	if unauth.Code != 401 || s.creates.Load() != 0 {
		t.Fatal("unauthenticated create reached the service")
	}
	unsupported := httptest.NewRecorder()
	g.ServeHTTP(unsupported, gatewayRequest(http.MethodDelete, "/api/v1/sandboxes/"+gatewaySandboxID, ""))
	if unsupported.Code != 501 || !strings.Contains(unsupported.Body.String(), `"code":"unsupported"`) {
		t.Fatal("unmigrated API did not fail with typed unsupported")
	}
	for _, body := range []string{
		`{"mode":"ephemeral","mode":"persistent"}`,
		`{"Mode":"ephemeral"}`,
		`{"mode":"ephemeral","authority":"user"}`,
		`{"mode":"ephemeral"} {}`,
		`{"mode":"ephemeral","dependencies":[{"name":"x","manager":"pip"}]}`,
		`{"mode":"ephemeral","workspace_path":"s3://bucket/"}`,
		`{"mode":"ephemeral","timeout":-1}`,
	} {
		rw := httptest.NewRecorder()
		g.ServeHTTP(rw, gatewayRequest(http.MethodPost, "/api/v1/sandboxes", body))
		if rw.Code != 400 || s.creates.Load() != 0 {
			t.Fatal("invalid or unsupported create request reached the service")
		}
	}
	rw := httptest.NewRecorder()
	g.ServeHTTP(rw, gatewayRequest(http.MethodPost, "/api/v1/sandboxes", strings.Repeat(" ", (2<<20)+1)))
	if rw.Code != 413 || s.creates.Load() != 0 {
		t.Fatal("total request body budget was not enforced")
	}
}

func TestGatewayCreatePendingAndCurrentMetadata(t *testing.T) {
	s := &gatewayServiceFake{}
	g := gatewayFixture(t, s)
	s.create = func(in provisioning.PlainColdBusinessInput) (provisioning.PlainColdBusinessResult, error) {
		if in.IdempotencyKey != "stable-key" {
			t.Fatal("original create key was not passed to the service")
		}
		return provisioning.PlainColdBusinessResult{IdempotencyKey: "stable-key", PlainColdResult: provisioning.PlainColdResult{State: provisioning.PlainColdPending, SandboxID: gatewaySandboxID, RequestID: "original-request"}}, state.ErrOutcomeUnknown
	}
	r := gatewayRequest(http.MethodPost, "/api/v1/sandboxes", `{"mode":"persistent"}`)
	r.Header.Set("Idempotency-Key", "stable-key")
	rw := httptest.NewRecorder()
	g.ServeHTTP(rw, r)
	if rw.Code != 202 || rw.Header().Get("Idempotency-Key") != "stable-key" || rw.Header().Get("Retry-After") != "1" || !strings.Contains(rw.Body.String(), gatewaySandboxID) || s.lookups.Load() != 0 {
		t.Fatal("pending create lost original attribution or pretended it was healthy")
	}
	created := time.Date(2026, 10, 16, 1, 2, 3, 0, time.UTC)
	s.lookup = func() (*provisioning.PlainColdLookupResult, error) {
		return &provisioning.PlainColdLookupResult{SandboxID: gatewaySandboxID, Runtime: p.RuntimeReference{ID: "runtime-original"}, CreatedAt: created, ExpiresAt: created.Add(time.Minute), Business: &sandboxsnapshot.Snapshot{Requested: types.CreateSandboxRequest{Mode: "persistent"}, Resolved: sandboxsnapshot.Resolved{TTL: sandboxsnapshot.TTL{Kind: "finite", Seconds: 60}}}}, nil
	}
	rw = httptest.NewRecorder()
	g.ServeHTTP(rw, gatewayRequest(http.MethodGet, "/api/v1/sandboxes/"+gatewaySandboxID, ""))
	var response types.SandboxResponse
	if err := json.Unmarshal(rw.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rw.Code != 200 || response.ID != gatewaySandboxID || response.State != "running" || !response.CreatedAt.Equal(created) || response.Timeout != 60 || response.ExpiresAt == nil || !response.ExpiresAt.Equal(created.Add(time.Minute)) {
		t.Fatal("GET did not use current owned Lookup metadata")
	}
	s.lookup = func() (*provisioning.PlainColdLookupResult, error) { return nil, provisioning.ErrPlainColdUnavailable }
	rw = httptest.NewRecorder()
	g.ServeHTTP(rw, gatewayRequest(http.MethodGet, "/api/v1/sandboxes/"+gatewaySandboxID, ""))
	if rw.Code != 503 {
		t.Fatal("unhealthy runtime reported healthy")
	}
}

func TestGatewayCreateCapacityState(t *testing.T) {
	s := &gatewayServiceFake{create: func(provisioning.PlainColdBusinessInput) (provisioning.PlainColdBusinessResult, error) {
		return provisioning.PlainColdBusinessResult{IdempotencyKey: "capacity-key", PlainColdResult: provisioning.PlainColdResult{State: provisioning.PlainColdCapacity}}, nil
	}}
	g := gatewayFixture(t, s)
	rw := httptest.NewRecorder()
	g.ServeHTTP(rw, gatewayRequest(http.MethodPost, "/api/v1/sandboxes", `{"mode":"ephemeral"}`))
	if rw.Code != 429 || rw.Header().Get("Retry-After") != "1" || rw.Header().Get("Idempotency-Key") != "capacity-key" || !strings.Contains(rw.Body.String(), `"code":"capacity"`) || s.lookups.Load() != 0 {
		t.Fatal("explicit capacity state must remain a retryable capacity response")
	}
}

func TestGatewayCreateReusedIndependentOfCompletionState(t *testing.T) {
	for _, reused := range []bool{false, true} {
		s := &gatewayServiceFake{
			create: func(provisioning.PlainColdBusinessInput) (provisioning.PlainColdBusinessResult, error) {
				return provisioning.PlainColdBusinessResult{Reused: reused, IdempotencyKey: "stable-key", PlainColdResult: provisioning.PlainColdResult{State: provisioning.PlainColdCompleted, SandboxID: gatewaySandboxID}}, nil
			},
			lookup: func() (*provisioning.PlainColdLookupResult, error) {
				return &provisioning.PlainColdLookupResult{SandboxID: gatewaySandboxID, Business: &sandboxsnapshot.Snapshot{Requested: types.CreateSandboxRequest{Mode: "ephemeral"}, Resolved: sandboxsnapshot.Resolved{TTL: sandboxsnapshot.TTL{Kind: "finite", Seconds: 60}}}}, nil
			},
		}
		g := gatewayFixture(t, s)
		rw := httptest.NewRecorder()
		g.ServeHTTP(rw, gatewayRequest(http.MethodPost, "/api/v1/sandboxes", `{"mode":"ephemeral"}`))
		var response types.CreateSandboxResponse
		if err := json.Unmarshal(rw.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if rw.Code != 201 || response.Reused != reused || s.lookups.Load() != 1 {
			t.Fatal("current completion state was incorrectly used as replay attribution")
		}
	}
}

func TestGatewayExecValidationAndResolvedEnvironment(t *testing.T) {
	s := &gatewayServiceFake{}
	g := gatewayFixture(t, s)
	for _, tc := range []struct {
		body string
		key  bool
	}{
		{`{"language":"bash","code":"exit 0"}`, true},
		{`{"language":"bash","code":"exit 0","tty":true}`, false},
		{`{"language":"bash","code":"exit 0","line_buffered":true}`, false},
		{`{"language":"bash","code":"exit 0","timeout":-1}`, false},
		{`{"language":"bash","code":"exit 0","timeout":601}`, false},
		{`{"language":"bash","code":"exit 0","requires_network":true}`, false},
		{`{"language":"bash","code":"exit 0","env":{"PATH":"a","PATH":"b"}}`, false},
	} {
		r := gatewayRequest(http.MethodPost, gatewayExecPath, tc.body)
		if tc.key {
			r.Header["Idempotency-Key"] = nil // net/http considers presence replayable, even without a value.
		}
		rw := httptest.NewRecorder()
		g.ServeHTTP(rw, r)
		if rw.Code != 400 || s.execs.Load() != 0 {
			t.Fatal("invalid or replayable Exec reached the service")
		}
	}
	s.exec = func(_ context.Context, in p.ExecutionRequest, _, _ io.Writer) (provisioning.PlainExecResult, error) {
		if in.TimeoutSeconds != 30 || in.UID != 1000 || in.GID != 1001 || in.WorkDir != "/workspace" || len(in.Argv) != 3 || in.Argv[0] != "/bin/sh" || in.Argv[1] != "-c" || in.Argv[2] != "exit 7" || in.Env["BASE"] != "operator" || in.Env["PATH"] != "/request/bin" || in.Env["REQUEST"] != "yes" || len(in.Env) != 3 {
			t.Fatal("execution did not receive fully resolved fixed profile and exact environment")
		}
		return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown, Operation: state.OperationReference{OperationID: "original-operation"}, Effect: state.ExecEffectReference{CommandID: "original-command"}}, errors.New("private-key-sentinel")
	}
	rw := httptest.NewRecorder()
	g.ServeHTTP(rw, gatewayRequest(http.MethodPost, gatewayExecPath, `{"language":"bash","code":"exit 7","env":{"PATH":"/request/bin","REQUEST":"yes"}}`))
	if rw.Code != 502 || !strings.Contains(rw.Body.String(), "exec_unknown") || !strings.Contains(rw.Body.String(), "original-operation") || strings.Contains(rw.Body.String(), "private-key-sentinel") || strings.Contains(rw.Body.String(), "exit_code") || rw.Header().Get("Retry-After") != "" {
		t.Fatal("unknown execution lost references, leaked an error or fabricated a terminal result")
	}
}

func TestGatewaySyncCombinedOutputBudget(t *testing.T) {
	s := &gatewayServiceFake{}
	g := gatewayFixture(t, s)
	s.exec = func(ctx context.Context, _ p.ExecutionRequest, stdout, stderr io.Writer) (provisioning.PlainExecResult, error) {
		chunk := bytes.Repeat([]byte("x"), 700<<10)
		if _, err := stdout.Write(chunk); err != nil {
			t.Fatal(err)
		}
		if _, err := stderr.Write(chunk); err == nil {
			t.Fatal("combined stdout+stderr memory budget was not enforced")
		}
		select {
		case <-ctx.Done():
		default:
			t.Fatal("overflow did not cancel the original execution context")
		}
		return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, ctx.Err()
	}
	rw := httptest.NewRecorder()
	g.ServeHTTP(rw, gatewayRequest(http.MethodPost, gatewayExecPath, `{"language":"bash","code":"emit"}`))
	if rw.Code != 502 || strings.Contains(rw.Body.String(), "exit_code") {
		t.Fatal("output overflow fabricated success")
	}
}

func TestGatewayStreamRequiresRealDeadlineBeforeEffects(t *testing.T) {
	s := &gatewayServiceFake{}
	g := gatewayFixture(t, s)
	rw := httptest.NewRecorder()
	g.ServeHTTP(rw, gatewayRequest(http.MethodPost, gatewayExecPath+"/stream", `{"language":"bash","code":"exit 0"}`))
	if rw.Code != 503 || s.execs.Load() != 0 {
		t.Fatal("stream accepted a writer without a real socket deadline")
	}
}

func TestGatewaySocketBackpressureDisconnectAndJoin(t *testing.T) {
	s := &gatewayServiceFake{}
	g := gatewayFixture(t, s)
	joined := make(chan struct{})
	var writes atomic.Int32
	s.exec = func(ctx context.Context, _ p.ExecutionRequest, stdout, _ io.Writer) (provisioning.PlainExecResult, error) {
		defer close(joined)
		chunk := bytes.Repeat([]byte("x"), 32<<10)
		for i := 0; i < 2048; i++ {
			if _, err := stdout.Write(chunk); err != nil {
				return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, err
			}
			writes.Add(1)
		}
		return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, ctx.Err()
	}
	server := httptest.NewServer(g)
	t.Cleanup(server.Close)
	conn, err := net.Dial("tcp", server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	r, err := http.NewRequest(http.MethodPost, server.URL+gatewayExecPath+"/stream", strings.NewReader(`{"language":"bash","code":"emit"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer mounted-secret")
	if err := r.Write(conn); err != nil {
		t.Fatal(err)
	}
	if _, err := http.ReadResponse(bufio.NewReader(conn), r); err != nil {
		t.Fatal(err)
	}
	select {
	case <-joined:
		t.Fatal("slow client allowed unbounded output consumption")
	case <-time.After(75 * time.Millisecond):
	}
	if writes.Load() >= 2048 {
		t.Fatal("all output was buffered despite the blocked socket")
	}
	conn.Close()
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("disconnect did not join the original synchronous Exec writer")
	}
	g.Close()
}

func TestGatewayServeCancellationJoinsHandlers(t *testing.T) {
	s := &gatewayServiceFake{}
	g := gatewayFixture(t, s)
	joined := make(chan struct{})
	s.exec = func(ctx context.Context, _ p.ExecutionRequest, stdout, _ io.Writer) (provisioning.PlainExecResult, error) {
		defer close(joined)
		if _, err := stdout.Write([]byte("started")); err != nil {
			return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, err
		}
		<-ctx.Done()
		return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, ctx.Err()
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, listener, g) }()
	r, err := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+gatewayExecPath+"/stream", strings.NewReader(`{"language":"bash","code":"wait"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer mounted-secret")
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve returned without bounded cancellation and actual handler join")
	}
	select {
	case <-joined:
	default:
		t.Fatal("Serve completed before the original Exec owner")
	}
}

func TestGatewayAdmissionCloseAndGlobalRate(t *testing.T) {
	s := &gatewayServiceFake{}
	settings := gatewaySettings()
	settings.MaxActive = 1
	g, err := New(s, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(g.Close)
	started, joined := make(chan struct{}), make(chan struct{})
	s.exec = func(ctx context.Context, _ p.ExecutionRequest, _, _ io.Writer) (provisioning.PlainExecResult, error) {
		close(started)
		<-ctx.Done()
		close(joined)
		return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown}, ctx.Err()
	}
	handlerDone := make(chan struct{})
	go func() {
		defer close(handlerDone)
		g.ServeHTTP(httptest.NewRecorder(), gatewayRequest(http.MethodPost, gatewayExecPath, `{"language":"bash","code":"wait"}`))
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first execution did not start")
	}
	rw := httptest.NewRecorder()
	g.ServeHTTP(rw, gatewayRequest(http.MethodPost, gatewayExecPath, `{"language":"bash","code":"wait"}`))
	if rw.Code != 429 || s.execs.Load() != 1 {
		t.Fatal("second request bypassed the total admission budget")
	}
	g.Close()
	select {
	case <-joined:
	default:
		t.Fatal("Close did not join the original Exec")
	}
	<-handlerDone
	rw = httptest.NewRecorder()
	g.ServeHTTP(rw, gatewayRequest(http.MethodPost, gatewayExecPath, `{"language":"bash","code":"wait"}`))
	if rw.Code != 503 || s.execs.Load() != 1 {
		t.Fatal("closed gateway admitted another execution")
	}
	settings = gatewaySettings()
	settings.RequestsPerSecond = 1
	rateLimited, err := New(s, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer rateLimited.Close()
	for i, want := range []int{501, 429} {
		rw = httptest.NewRecorder()
		rateLimited.ServeHTTP(rw, gatewayRequest(http.MethodDelete, "/api/v1/sandboxes/"+gatewaySandboxID, ""))
		if rw.Code != want {
			t.Fatalf("global token bucket request %d status=%d want=%d", i, rw.Code, want)
		}
	}
}

func TestGatewayUnknownReasonFromActualErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"deadline", context.DeadlineExceeded, "deadline_exceeded"},
		{"canceled", context.Canceled, "canceled"},
		{"guard", state.ErrGuardExpired, "guard_expired"},
		{"conflict", state.ErrConflict, "conflict"},
		{"opaque", errors.New("private-key-sentinel"), "outcome_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &gatewayServiceFake{exec: func(context.Context, p.ExecutionRequest, io.Writer, io.Writer) (provisioning.PlainExecResult, error) {
				return provisioning.PlainExecResult{State: provisioning.PlainExecUnknown, Operation: state.OperationReference{OperationID: "original-operation"}, Effect: state.ExecEffectReference{CommandID: "original-command"}}, errors.Join(errors.New("private-key-sentinel"), tc.err)
			}}
			g := gatewayFixture(t, s)
			server := httptest.NewServer(g)
			defer server.Close()
			for _, suffix := range []string{"", "/stream"} {
				r, err := http.NewRequest(http.MethodPost, server.URL+gatewayExecPath+suffix, strings.NewReader(`{"language":"bash","code":"exit 0"}`))
				if err != nil {
					t.Fatal(err)
				}
				r.Header.Set("Authorization", "Bearer mounted-secret")
				response, err := http.DefaultClient.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				wire, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatal(err)
				}
				body := string(wire)
				if !strings.Contains(body, `"reason":"`+tc.want+`"`) || !strings.Contains(body, "original-operation") || !strings.Contains(body, "original-command") || strings.Contains(body, "exit_code") || strings.Contains(body, "private-key-sentinel") || response.Header.Get("Retry-After") != "" {
					t.Fatalf("unknown %s response lost true error attribution or fabricated success: %s", suffix, body)
				}
				if suffix == "" && response.StatusCode != 502 || suffix != "" && (response.StatusCode != 200 || !strings.Contains(body, "event: error") || strings.Contains(body, "event: done")) {
					t.Fatal("unknown response changed code/state semantics")
				}
			}
		})
	}
}
