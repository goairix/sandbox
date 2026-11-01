package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type sharedManagerRepository struct {
	*sharedDataTestRepository
	snapshotTimeout time.Duration
	readErr         error
}

func (r *sharedManagerRepository) decorate(v *state.ActiveDataView) *state.ActiveDataView {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	v.Record.CreatedAt = now
	v.Record.UpdatedAt = now
	v.Record.Snapshot, _ = json.Marshal(&Sandbox{ID: v.Record.SandboxID, RuntimeID: v.Record.RuntimeID, RuntimeUID: v.Record.RuntimeUID, State: StateReady, Timeout: r.snapshotTimeout})
	return v
}
func (r *sharedManagerRepository) OpenDataOperation(ctx context.Context, id, token string, ttl time.Duration) (*state.ActiveDataView, error) {
	v, err := r.sharedDataTestRepository.OpenDataOperation(ctx, id, token, ttl)
	if err != nil {
		return nil, err
	}
	return r.decorate(v), nil
}
func (r *sharedManagerRepository) ReadDataOperation(ctx context.Context, l state.ActiveDataLease, active bool) (*state.ActiveDataView, error) {
	r.mu.Lock()
	err := r.readErr
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	v, err := r.sharedDataTestRepository.ReadDataOperation(ctx, l, active)
	if err != nil {
		return nil, err
	}
	return r.decorate(v), nil
}
func (r *sharedManagerRepository) WatchActiveChanges(context.Context, int64) (<-chan state.ActiveChangeBatch, error) {
	return make(chan state.ActiveChangeBatch), nil
}
func (r *sharedManagerRepository) ObserveOperations(context.Context, string) (state.ActiveOperationObservation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return state.ActiveOperationObservation{Count: int64(len(r.leases)), Revision: 2}, nil
}
func (r *sharedManagerRepository) BeginOperation(ctx context.Context, id, token string, kind state.ActiveOperationKind, ttl time.Duration) (*state.ActiveSandboxRecord, *state.ActiveSandboxOperation, error) {
	v, err := r.OpenDataOperation(ctx, id, token, ttl)
	if err != nil {
		return nil, nil, err
	}
	return v.Record, &v.Lease.Operation, nil
}

func newSharedManagerTest(t *testing.T, enabled bool) (*Manager, *sharedManagerRepository, *fileV2Runtime) {
	t.Helper()
	repo := &sharedManagerRepository{sharedDataTestRepository: newSharedDataTestRepository(), snapshotTimeout: time.Hour}
	rt := &fileV2Runtime{mockRuntime: newMockRuntime()}
	rt.sandboxes["pod"] = &runtime.SandboxInfo{RuntimeID: "pod", RuntimeUID: "uid", State: "running"}
	m := NewManager(rt, nil, nil, ManagerConfig{RuntimeType: "kubernetes", ActiveSandboxes: repo, InstanceID: "api", SharedDataOperations: enabled})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, m.Stop(ctx))
	})
	return m, repo, rt
}
func TestSharedDataManagerReadsFreshSnapshot(t *testing.T) {
	m, r, _ := newSharedManagerTest(t, true)
	sb, _, release, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	require.Equal(t, time.Hour, sb.Timeout)
	release()
	r.mu.Lock()
	r.snapshotTimeout = 2 * time.Hour
	r.mu.Unlock()
	sb, _, release, err = m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	require.Equal(t, 2*time.Hour, sb.Timeout)
	release()
	r.mu.Lock()
	require.Equal(t, 1, r.opens)
	r.mu.Unlock()
}
func TestSharedDataManagerRejectsChangedRuntimeUID(t *testing.T) {
	m, _, rt := newSharedManagerTest(t, true)
	rt.sandboxes["pod"].RuntimeUID = "replacement"
	_, _, _, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.ErrorIs(t, err, ErrSandboxNotReady)
}
func TestSharedDataManagerOnlyCapacityFallsBack(t *testing.T) {
	m, r, _ := newSharedManagerTest(t, true)
	r.readErr = errors.New("store unavailable")
	_, _, _, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.Error(t, err)
	r.mu.Lock()
	require.Equal(t, 1, r.opens, "store failure must not retry on the legacy path")
	r.mu.Unlock()
}
func TestSharedDataManagerDisabledUsesIndividualOperations(t *testing.T) {
	m, r, _ := newSharedManagerTest(t, false)
	for range 2 {
		_, _, release, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
		require.NoError(t, err)
		release()
	}
	r.mu.Lock()
	require.Equal(t, 2, r.opens)
	require.Equal(t, 2, r.ends)
	r.mu.Unlock()
	require.Nil(t, m.sharedData)
}
func TestSharedDataManagerBeginShutdownKeepsInflightLease(t *testing.T) {
	m, r, _ := newSharedManagerTest(t, true)
	_, ctx, release, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	m.BeginShutdown()
	require.NoError(t, context.Cause(ctx))
	_, _, _, err = m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.ErrorIs(t, err, ErrSandboxNotReady)
	r.mu.Lock()
	require.Zero(t, r.ends)
	r.mu.Unlock()
	release()
}
func TestSharedDataManagerStopTimeoutKeepsProtection(t *testing.T) {
	m, r, _ := newSharedManagerTest(t, true)
	_, opCtx, release, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, m.Stop(ctx), context.DeadlineExceeded)
	require.Error(t, context.Cause(opCtx))
	r.mu.Lock()
	require.Zero(t, r.ends)
	r.mu.Unlock()
	release()
}
func TestSharedDataManagerNoCapabilityKeepsLegacyPath(t *testing.T) {
	m, r, _ := newSharedManagerTest(t, false)
	// Hide optional capabilities without changing the authoritative repository.
	m.activeSandboxes = struct{ state.ActiveSandboxRepository }{r}
	m.config.SharedDataOperations = true
	_, _, release, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	release()
	require.Nil(t, m.sharedData)
}
func TestSharedDataManagerHealthDuringShutdownRejected(t *testing.T) {
	m, _, _ := newSharedManagerTest(t, true)
	m.BeginShutdown()
	_, _, _, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.ErrorIs(t, err, ErrSandboxNotReady)
}
func TestSharedDataFileStreamReleaseWaitsForJoin(t *testing.T) {
	m, r, rt := newSharedManagerTest(t, true)
	barrier := &fileV2CloseBarrier{Reader: strings.NewReader("abc"), started: make(chan struct{}), release: make(chan struct{})}
	rt.stream = barrier
	stream, err := m.ReadFileRangeV2(context.Background(), "s", "/workspace/a", 0, -1)
	require.NoError(t, err)
	m.BeginShutdown()
	closed := make(chan error, 1)
	go func() { closed <- stream.Close() }()
	<-barrier.started
	r.mu.Lock()
	require.Zero(t, r.ends)
	r.mu.Unlock()
	close(barrier.release)
	require.NoError(t, <-closed)
}
func TestSharedDataFileStreamCallerAbandons(t *testing.T) {
	m, r, _ := newSharedManagerTest(t, true)
	stream, err := m.ReadFileRangeV2(context.Background(), "s", "/workspace/a", 0, -1)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, m.Stop(ctx), context.DeadlineExceeded)
	r.mu.Lock()
	require.Zero(t, r.ends)
	r.mu.Unlock()
	require.NoError(t, stream.Close())
}

var _ io.ReadCloser = (*fileV2CloseBarrier)(nil)

type cancelInspectKey struct{}
type cancelInspectRuntime struct {
	runtime.Runtime
	entered chan struct{}
}

func (r cancelInspectRuntime) GetSandbox(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	if ctx.Value(cancelInspectKey{}) != nil {
		close(r.entered)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return r.Runtime.GetSandbox(ctx, id)
}
func TestSharedDataManagerRequestCancelDoesNotCancelPeer(t *testing.T) {
	m, _, rt := newSharedManagerTest(t, true)
	_, peer, release, err := m.beginDistributedOperation(context.Background(), "s", state.ActiveOperationData)
	require.NoError(t, err)
	defer release()
	entered := make(chan struct{})
	m.runtime = cancelInspectRuntime{Runtime: rt, entered: entered}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), cancelInspectKey{}, true))
	done := make(chan error, 1)
	go func() {
		_, _, release, err := m.beginDistributedOperation(ctx, "s", state.ActiveOperationData)
		if err == nil {
			release()
		}
		done <- err
	}()
	<-entered
	cancel()
	require.Error(t, <-done)
	require.NoError(t, context.Cause(peer), "one canceled runtime inspection must not cancel the shared peer")
}
