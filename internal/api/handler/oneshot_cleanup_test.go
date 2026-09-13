package handler_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/internal/storage/state"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type slowCleanupRuntime struct {
	uploadRuntime
	removals atomic.Int32
}

func (r *slowCleanupRuntime) GetSandbox(context.Context, string) (*runtime.SandboxInfo, error) {
	return &runtime.SandboxInfo{RuntimeID: "runtime-a", RuntimeUID: "uid-a", State: "running"}, nil
}

func (r *slowCleanupRuntime) ExecStream(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	ch := make(chan runtime.StreamEvent, 2)
	ch <- runtime.StreamEvent{Type: runtime.StreamStdout, Content: "hello"}
	ch <- runtime.StreamEvent{Type: runtime.StreamDone, Content: "0"}
	close(ch)
	return ch, nil
}

func (r *slowCleanupRuntime) RemoveSandbox(ctx context.Context, _ string) error {
	r.removals.Add(1)
	select {
	case <-time.After(350 * time.Millisecond):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestOneShotStreamCompletesHTTPBeforeDurableCleanup(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("requires TEST_REDIS_ADDR")
	}
	initFileHandlerMetrics.Do(func() { require.NoError(t, metrics.InitNoop()) })
	store, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	repo, err := redisstate.NewActiveSandboxRepository(store, "oneshot-"+uuid.NewString())
	require.NoError(t, err)
	rt := &slowCleanupRuntime{}
	mgr := sandbox.NewManager(rt, nil, nil, sandbox.ManagerConfig{RuntimeType: "kubernetes", InstanceID: "test", ActiveSandboxes: repo})
	router := gin.New()
	router.POST("/execute/stream", handler.NewHandler(mgr).ExecuteOneShotStream)
	server := httptest.NewServer(router)
	defer server.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	start := time.Now()
	response, err := client.Post(server.URL+"/execute/stream", "application/json", strings.NewReader(`{"language":"bash","code":"echo hello"}`))
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, response.Body.Close())
	require.NoError(t, err, "real chunked HTTP stream must end cleanly")
	require.Contains(t, string(body), "event: done")
	require.Less(t, time.Since(start), 300*time.Millisecond, "response must not await runtime termination")
	var records []state.ActiveSandboxRecord
	var cursor uint64
	for {
		page, err := repo.Scan(context.Background(), cursor, 128)
		require.NoError(t, err)
		records = append(records, page.Records...)
		cursor = page.Cursor
		if cursor == 0 {
			break
		}
	}
	require.Len(t, records, 1)
	require.Equal(t, state.ActiveSandboxDestroying, records[0].Phase)
	require.Zero(t, rt.removals.Load())
	require.NoError(t, mgr.Destroy(context.Background(), records[0].SandboxID))
	require.NoError(t, mgr.Stop(context.Background()))
}

type failingDestroyRepository struct {
	state.ActiveSandboxRepository
	fail atomic.Bool
}

type disconnectStreamRuntime struct{ slowCleanupRuntime }

func (r *disconnectStreamRuntime) ExecStream(ctx context.Context, _ string, _ runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	ch := make(chan runtime.StreamEvent, 1)
	ch <- runtime.StreamEvent{Type: runtime.StreamStdout, Content: "started"}
	go func() { <-ctx.Done(); close(ch) }()
	return ch, nil
}

type observedDestroyRepository struct {
	*redisstate.ActiveSandboxRepository
	observed chan *state.ActiveSandboxRecord
	canceled atomic.Bool
}

func (r *observedDestroyRepository) BeginDestroy(ctx context.Context, id string) (*state.ActiveSandboxRecord, int64, bool, error) {
	r.canceled.Store(ctx.Err() != nil)
	record, live, won, err := r.ActiveSandboxRepository.BeginDestroy(ctx, id)
	select {
	case r.observed <- record:
	default:
	}
	return record, live, won, err
}

func TestOneShotClientDisconnectPersistsCleanupOutsideCanceledRequest(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("requires TEST_REDIS_ADDR")
	}
	initFileHandlerMetrics.Do(func() { require.NoError(t, metrics.InitNoop()) })
	store, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	base, err := redisstate.NewActiveSandboxRepository(store, "oneshot-disconnect-"+uuid.NewString())
	require.NoError(t, err)
	repo := &observedDestroyRepository{ActiveSandboxRepository: base, observed: make(chan *state.ActiveSandboxRecord, 1)}
	mgr := sandbox.NewManager(&disconnectStreamRuntime{}, nil, nil, sandbox.ManagerConfig{RuntimeType: "kubernetes", InstanceID: "test", ActiveSandboxes: repo})
	router := gin.New()
	router.POST("/execute/stream", handler.NewHandler(mgr).ExecuteOneShotStream)
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/execute/stream", strings.NewReader(`{"language":"bash","code":"sleep 30"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	require.NoError(t, err)
	var first [1]byte
	_, err = response.Body.Read(first[:])
	require.NoError(t, err)
	cancel()
	require.NoError(t, response.Body.Close())
	select {
	case record := <-repo.observed:
		require.NotNil(t, record)
		require.Equal(t, state.ActiveSandboxDestroying, record.Phase)
		require.False(t, repo.canceled.Load(), "durable cleanup must detach request cancellation")
		require.NoError(t, mgr.Destroy(context.Background(), record.SandboxID))
	case <-time.After(2 * time.Second):
		t.Fatal("client disconnect did not persist cleanup")
	}
	require.NoError(t, mgr.Stop(context.Background()))
}

func (r *failingDestroyRepository) BeginDestroy(ctx context.Context, id string) (*state.ActiveSandboxRecord, int64, bool, error) {
	if r.fail.Load() {
		return nil, 0, false, state.ErrDurabilityUnconfirmed
	}
	return r.ActiveSandboxRepository.BeginDestroy(ctx, id)
}

func (r *failingDestroyRepository) CheckpointController(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64, checkpoint string) (*state.ActiveSandboxRecord, error) {
	return r.ActiveSandboxRepository.(state.ActiveSandboxCheckpointRepository).CheckpointController(ctx, lease, revision, checkpoint)
}

func (r *failingDestroyRepository) DeleteController(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64) error {
	return r.ActiveSandboxRepository.(state.ActiveSandboxCheckpointRepository).DeleteController(ctx, lease, revision)
}

func TestOneShotStreamDoesNotReportDoneWhenCleanupIsNotDurable(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("requires TEST_REDIS_ADDR")
	}
	initFileHandlerMetrics.Do(func() { require.NoError(t, metrics.InitNoop()) })
	store, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	repo, err := redisstate.NewActiveSandboxRepository(store, "oneshot-failure-"+uuid.NewString())
	require.NoError(t, err)
	failing := &failingDestroyRepository{ActiveSandboxRepository: repo}
	failing.fail.Store(true)
	mgr := sandbox.NewManager(&slowCleanupRuntime{}, nil, nil, sandbox.ManagerConfig{RuntimeType: "kubernetes", InstanceID: "test", ActiveSandboxes: failing})
	router := gin.New()
	router.POST("/execute/stream", handler.NewHandler(mgr).ExecuteOneShotStream)
	server := httptest.NewServer(router)
	defer server.Close()
	response, err := http.Post(server.URL+"/execute/stream", "application/json", strings.NewReader(`{"language":"bash","code":"echo hello"}`))
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, response.Body.Close())
	require.NoError(t, err)
	require.NotContains(t, string(body), "event: done")
	require.Contains(t, string(body), "cleanup_pending")
	failing.fail.Store(false)
	var cursor uint64
	for {
		page, err := repo.Scan(context.Background(), cursor, 128)
		require.NoError(t, err)
		for _, record := range page.Records {
			require.NoError(t, mgr.Destroy(context.Background(), record.SandboxID))
		}
		cursor = page.Cursor
		if cursor == 0 {
			break
		}
	}
	require.NoError(t, mgr.Stop(context.Background()))
}
