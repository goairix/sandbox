package etcdgateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/provisioning"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/stretchr/testify/require"
)

// Existing gateway fixtures have no destroy behavior. Keeping their default
// refusal here lets the new Service interface expose omissions at RED without
// adding production fallback or changing existing unsupported-route assertions.
func (*gatewayServiceFake) Destroy(context.Context, string, string) (provisioning.PlainColdDestroyResult, error) {
	return provisioning.PlainColdDestroyResult{}, provisioning.ErrPlainColdUnavailable
}

type gatewayDestroyService struct {
	*gatewayServiceFake
	calls   atomic.Int32
	destroy func(context.Context, string, string) (provisioning.PlainColdDestroyResult, error)
}

func (s *gatewayDestroyService) Destroy(ctx context.Context, id, requestID string) (provisioning.PlainColdDestroyResult, error) {
	s.calls.Add(1)
	return s.destroy(ctx, id, requestID)
}

func destroyGatewayFixture(t *testing.T, fn func(context.Context, string, string) (provisioning.PlainColdDestroyResult, error)) (*Gateway, *gatewayDestroyService) {
	t.Helper()
	s := &gatewayDestroyService{gatewayServiceFake: &gatewayServiceFake{}, destroy: fn}
	g, err := New(s, gatewaySettings())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, g.Close()) })
	return g, s
}

func gatewayDestroyResult() provisioning.PlainColdDestroyResult {
	return provisioning.PlainColdDestroyResult{
		State: provisioning.PlainColdDestroyPending, SandboxID: gatewaySandboxID, RequestID: "destroy-request", Generation: 7,
		Task:  state.TaskReference{Namespace: "/sandbox/v1/authority/cell/", RestoreEpoch: "restore", Partition: 7, SandboxID: gatewaySandboxID, TaskID: "00000000-0000-4000-8000-000000000002"},
		Stage: state.StageReference{Namespace: "/sandbox/v1/authority/cell/", RestoreEpoch: "restore", Partition: 7, RequestID: "destroy-request", StageID: "destroy", AttemptID: "00000000-0000-4000-8000-000000000003", Digest: strings.Repeat("a", 64)},
	}
}

func TestGatewayDestroyAcceptedAndOriginalAttribution(t *testing.T) {
	want := gatewayDestroyResult()
	g, service := destroyGatewayFixture(t, func(_ context.Context, id, requestID string) (provisioning.PlainColdDestroyResult, error) {
		require.Equal(t, gatewaySandboxID, id)
		require.Equal(t, want.RequestID, requestID)
		return want, nil
	})
	r := gatewayRequest(http.MethodDelete, "/api/v1/sandboxes/"+gatewaySandboxID, "")
	r.Header.Set("X-Request-ID", want.RequestID)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	require.Equal(t, http.StatusAccepted, w.Code)
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	require.Equal(t, want.RequestID, w.Header().Get("X-Request-ID"))
	var wire struct {
		Code       string                             `json:"code"`
		State      provisioning.PlainColdDestroyState `json:"state"`
		SandboxID  string                             `json:"sandbox_id"`
		RequestID  string                             `json:"request_id"`
		Generation int64                              `json:"generation"`
		Task       state.TaskReference                `json:"task"`
		Stage      state.StageReference               `json:"stage"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &wire))
	require.Equal(t, "destroy_pending", wire.Code)
	require.Equal(t, want.State, wire.State)
	require.Equal(t, want.SandboxID, wire.SandboxID)
	require.Equal(t, want.RequestID, wire.RequestID)
	require.Equal(t, want.Generation, wire.Generation)
	require.Equal(t, want.Task, wire.Task)
	require.Equal(t, want.Stage, wire.Stage)
	require.NotContains(t, w.Body.String(), "sandbox destroyed")
	require.EqualValues(t, 1, service.calls.Load())
	require.Zero(t, service.lookups.Load(), "DELETE must not require live health or TTL")
	require.Zero(t, service.execs.Load())
}

func TestGatewayDestroyErrorsAndUnknownPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"absent", provisioning.ErrPlainColdNotFound, 404, "not_found"},
		{"closing", provisioning.ErrPlainColdDestroyConflict, 409, "destroy_conflict"},
		{"unsupported-cleanup", provisioning.ErrPlainColdDestroyProfile, 501, "unsupported_cleanup_profile"},
		{"admission", state.ErrConflict, 503, "unavailable"},
		{"unknown", state.ErrOutcomeUnknown, 503, "outcome_unknown"},
		{"unknown-and-conflict", errors.Join(state.ErrOutcomeUnknown, provisioning.ErrPlainColdDestroyConflict), 503, "outcome_unknown"},
		{"unknown-and-absent", errors.Join(state.ErrOutcomeUnknown, provisioning.ErrPlainColdNotFound), 503, "outcome_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := gatewayDestroyResult()
			want.State = provisioning.PlainColdDestroyRejected
			if errors.Is(tc.err, state.ErrOutcomeUnknown) {
				want.State = provisioning.PlainColdDestroyUnknown
			}
			g, service := destroyGatewayFixture(t, func(context.Context, string, string) (provisioning.PlainColdDestroyResult, error) {
				return want, tc.err
			})
			r := gatewayRequest(http.MethodDelete, "/api/v1/sandboxes/"+gatewaySandboxID, "")
			r.Header.Set("X-Request-ID", want.RequestID)
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			require.Equal(t, tc.status, w.Code)
			require.Contains(t, w.Body.String(), `"code":"`+tc.code+`"`)
			if errors.Is(tc.err, state.ErrOutcomeUnknown) {
				require.Contains(t, w.Body.String(), want.Task.TaskID)
				require.Contains(t, w.Body.String(), want.Stage.AttemptID)
				require.Contains(t, w.Body.String(), want.RequestID)
				require.NotContains(t, w.Body.String(), `"code":"destroy_pending"`)
			}
			require.EqualValues(t, 1, service.calls.Load(), "HTTP must not retry unknown or conflicts")
		})
	}
}

func TestGatewayDestroyValidationAndBudgets(t *testing.T) {
	for _, name := range []string{"auth", "uuid", "duplicate-request-id", "empty-request-id", "idempotency", "rate", "lane"} {
		t.Run(name, func(t *testing.T) {
			g, service := destroyGatewayFixture(t, func(context.Context, string, string) (provisioning.PlainColdDestroyResult, error) {
				t.Error("refused DELETE reached service")
				return gatewayDestroyResult(), nil
			})
			r := gatewayRequest(http.MethodDelete, "/api/v1/sandboxes/"+gatewaySandboxID, "")
			status := 400
			var admitted *gatewayCall
			switch name {
			case "auth":
				r.Header.Del("Authorization")
				status = 401
			case "uuid":
				r.URL.Path = "/api/v1/sandboxes/not-a-uuid"
			case "duplicate-request-id":
				r.Header["X-Request-Id"] = []string{"first", "second"}
			case "empty-request-id":
				r.Header.Set("X-Request-ID", "")
			case "idempotency":
				r.Header.Set("Idempotency-Key", "not-a-destroy-dedup-key")
			case "rate":
				g.tokens = 0
				g.lastToken = time.Now().Add(time.Hour)
				status = 429
			case "lane":
				g.settings.MaxActive = 1
				var got int
				admitted, got = g.enter(context.Background(), businessLane)
				require.Zero(t, got)
				defer g.leave(admitted)
				status = 429
			}
			w := httptest.NewRecorder()
			g.ServeHTTP(w, r)
			require.Equal(t, status, w.Code)
			require.Zero(t, service.calls.Load())
		})
	}
}

func TestGatewayDestroyCloseJoinsOriginalHandler(t *testing.T) {
	entered, cancelled, release, callDone := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	g, service := destroyGatewayFixture(t, func(ctx context.Context, _, _ string) (provisioning.PlainColdDestroyResult, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return provisioning.PlainColdDestroyResult{}, ctx.Err()
	})
	t.Cleanup(unblock)
	go func() {
		defer close(callDone)
		g.ServeHTTP(httptest.NewRecorder(), gatewayRequest(http.MethodDelete, "/api/v1/sandboxes/"+gatewaySandboxID, ""))
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("DELETE was not admitted")
	}
	closed := make(chan error, 1)
	go func() { closed <- g.Close() }()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel original DELETE handler")
	}
	select {
	case <-closed:
		t.Fatal("Close returned before original DELETE service call")
	default:
	}
	unblock()
	require.NoError(t, <-closed)
	<-callDone
	require.EqualValues(t, 1, service.calls.Load())
}
