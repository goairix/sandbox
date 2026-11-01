package sandbox

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

// The external store double keeps real local coordination under test; repository
// fencing and lease behavior are separately exercised against actual etcd.
type sharedDataTestRepository struct {
	openEntered  chan struct{}
	renewEntered chan struct{}
	state.ActiveSandboxRepository
	mu                  sync.Mutex
	phase               state.ActiveSandboxPhase
	generation          int64
	uid                 string
	opens, ends, renews int
	leases              map[string]bool
	openBlock           <-chan struct{}
	renewBlock          <-chan struct{}
	renewErr            error
}

func newSharedDataTestRepository() *sharedDataTestRepository {
	return &sharedDataTestRepository{phase: state.ActiveSandboxActive, generation: 1, uid: "uid", leases: make(map[string]bool)}
}
func (r *sharedDataTestRepository) view(lease state.ActiveDataLease) *state.ActiveDataView {
	return &state.ActiveDataView{Lease: lease, Record: &state.ActiveSandboxRecord{Version: 1, SandboxID: lease.Operation.SandboxID, Generation: r.generation, RuntimeID: "pod", RuntimeUID: r.uid, Phase: r.phase, Revision: 2, Snapshot: []byte(`{"ready":true}`)}}
}
func (r *sharedDataTestRepository) OpenDataOperation(ctx context.Context, id, token string, ttl time.Duration) (*state.ActiveDataView, error) {
	if r.openEntered != nil {
		r.openEntered <- struct{}{}
	}
	if r.openBlock != nil {
		select {
		case <-r.openBlock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.opens++
	r.leases[token] = true
	return r.view(state.ActiveDataLease{Operation: state.ActiveSandboxOperation{SandboxID: id, Token: token, Generation: r.generation, Kind: state.ActiveOperationData, ExpiresAt: time.Now().Add(ttl)}, LeaseID: int64(r.opens), RequestedTTL: ttl}), nil
}
func (r *sharedDataTestRepository) ReadDataOperation(ctx context.Context, l state.ActiveDataLease, active bool) (*state.ActiveDataView, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.leases[l.Operation.Token] {
		return nil, state.ErrActiveSandboxStaleToken
	}
	if active && r.phase != state.ActiveSandboxActive {
		return nil, state.ErrActiveSandboxAdmissionClosed
	}
	return r.view(l), nil
}
func (r *sharedDataTestRepository) KeepAliveDataOperation(ctx context.Context, l state.ActiveDataLease) (*state.ActiveDataView, time.Duration, error) {
	if r.renewEntered != nil {
		r.renewEntered <- struct{}{}
	}
	if r.renewBlock != nil {
		select {
		case <-r.renewBlock:
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.renews++
	if r.renewErr != nil {
		return nil, 0, r.renewErr
	}
	if !r.leases[l.Operation.Token] {
		return nil, 0, state.ErrActiveSandboxStaleToken
	}
	return r.view(l), 30 * time.Second, nil
}
func (r *sharedDataTestRepository) EndOperation(_ context.Context, op state.ActiveSandboxOperation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ends++
	delete(r.leases, op.Token)
	return nil
}
func sharedTestRegistry(t *testing.T, options sharedDataOptions) (*sharedDataTestRepository, *sharedDataOperations) {
	t.Helper()
	repo := newSharedDataTestRepository()
	registry := newSharedDataOperations(context.Background(), repo, repo, nil, "api-a", options)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, registry.Stop(ctx))
	})
	return repo, registry
}
func TestSharedDataGroupSingleRegistration(t *testing.T) {
	repo, r := sharedTestRegistry(t, sharedDataOptions{})
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, release, err := r.Acquire(context.Background(), "s")
			if err == nil {
				release()
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	repo.mu.Lock()
	require.Equal(t, 1, repo.opens)
	require.Zero(t, repo.ends)
	repo.mu.Unlock()
}
func TestSharedDataGroupFirstCallerCancellation(t *testing.T) {
	repo := newSharedDataTestRepository()
	block := make(chan struct{})
	repo.openBlock = block
	r := newSharedDataOperations(context.Background(), repo, repo, nil, "api", sharedDataOptions{})
	defer func() { require.NoError(t, r.Stop(context.Background())) }()
	repo.openEntered = make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, release, err := r.Acquire(ctx, "s")
		if err == nil {
			release()
		}
		result <- err
	}()
	<-repo.openEntered
	cancel()
	require.ErrorIs(t, <-result, context.Canceled)
	close(block)
	_, _, release, err := r.Acquire(context.Background(), "s")
	require.NoError(t, err)
	release()
}
func TestSharedDataGroupCapacityFallback(t *testing.T) {
	_, r := sharedTestRegistry(t, sharedDataOptions{})
	for i := range 1024 {
		_, _, release, err := r.Acquire(context.Background(), stringID(i))
		require.NoError(t, err)
		release()
	}
	_, _, _, err := r.Acquire(context.Background(), "overflow")
	require.ErrorIs(t, err, errSharedDataCapacity)
}
func stringID(i int) string { return fmt.Sprintf("s-%d", i) }
func TestSharedDataGroupPauseExpiresAdmission(t *testing.T) {
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	_, r := sharedTestRegistry(t, sharedDataOptions{now: now})
	_, opCtx, release, err := r.Acquire(context.Background(), "s")
	require.NoError(t, err)
	defer release()
	offset.Store(int64(31 * time.Second))
	_, _, _, err = r.Acquire(context.Background(), "s")
	require.ErrorIs(t, err, ErrSandboxNotReady)
	require.Error(t, context.Cause(opCtx))
}
func TestSharedDataGroupIdleAcquireRace(t *testing.T) {
	var offset atomic.Int64
	_, r := sharedTestRegistry(t, sharedDataOptions{now: func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }})
	_, _, release, err := r.Acquire(context.Background(), "s")
	require.NoError(t, err)
	release()
	offset.Store(int64(16 * time.Second))
	_, _, release, err = r.Acquire(context.Background(), "s")
	if err == nil {
		release()
	} else {
		require.ErrorIs(t, err, ErrSandboxNotReady)
	}
}
func TestSharedDataGroupOldGenerationCannotJoin(t *testing.T) {
	repo, r := sharedTestRegistry(t, sharedDataOptions{})
	_, ctx, release, err := r.Acquire(context.Background(), "s")
	require.NoError(t, err)
	defer release()
	repo.mu.Lock()
	repo.generation++
	repo.mu.Unlock()
	_, _, _, err = r.Acquire(context.Background(), "s")
	require.ErrorIs(t, err, ErrSandboxNotReady)
	require.Error(t, context.Cause(ctx))
}
func TestSharedDataGroupRestartUsesNewToken(t *testing.T) {
	repo, r := sharedTestRegistry(t, sharedDataOptions{})
	_, _, release, err := r.Acquire(context.Background(), "s")
	require.NoError(t, err)
	release()
	other := newSharedDataOperations(context.Background(), repo, repo, nil, "api-a", sharedDataOptions{})
	defer func() { require.NoError(t, other.Stop(context.Background())) }()
	_, _, release, err = other.Acquire(context.Background(), "s")
	require.NoError(t, err)
	release()
	repo.mu.Lock()
	require.Len(t, repo.leases, 2)
	repo.mu.Unlock()
}
func TestSharedDataGroupDrainingRenewsUntilReleased(t *testing.T) {
	repo, r := sharedTestRegistry(t, sharedDataOptions{})
	_, _, release, err := r.Acquire(context.Background(), "s")
	require.NoError(t, err)
	r.CloseAdmission()
	r.mu.Lock()
	g := r.groups["s"]
	r.mu.Unlock()
	g.renew()
	repo.mu.Lock()
	require.Equal(t, 1, repo.renews)
	require.Zero(t, repo.ends)
	repo.mu.Unlock()
	release()
}
func TestSharedDataGroupLostLeaseDoesNotAffectSuccessor(t *testing.T) {
	repo, r := sharedTestRegistry(t, sharedDataOptions{})
	_, ctx, release, err := r.Acquire(context.Background(), "s")
	require.NoError(t, err)
	defer release()
	repo.mu.Lock()
	repo.renewErr = state.ErrActiveSandboxStaleToken
	repo.mu.Unlock()
	r.mu.Lock()
	g := r.groups["s"]
	r.mu.Unlock()
	g.renew()
	require.Error(t, context.Cause(ctx))
	repo.mu.Lock()
	require.Equal(t, 1, repo.opens)
	repo.mu.Unlock()
}
func TestSharedDataGroupKeepAliveResponseLate(t *testing.T) {
	var offset atomic.Int64
	repo, r := sharedTestRegistry(t, sharedDataOptions{now: func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }})
	_, ctx, release, err := r.Acquire(context.Background(), "s")
	require.NoError(t, err)
	defer release()
	block := make(chan struct{})
	repo.renewBlock = block
	repo.renewEntered = make(chan struct{}, 1)
	r.mu.Lock()
	g := r.groups["s"]
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { g.renew(); close(done) }()
	<-repo.renewEntered
	offset.Store(int64(31 * time.Second))
	close(block)
	<-done
	_, _, _, err = r.Acquire(context.Background(), "s")
	require.Error(t, err)
	require.Error(t, context.Cause(ctx))
}

func TestSharedDataGroupRenewalHonorsConfirmedDeadline(t *testing.T) {
	repo, r := sharedTestRegistry(t, sharedDataOptions{ttl: 100 * time.Millisecond, renewInterval: time.Hour})
	_, ctx, release, err := r.Acquire(context.Background(), "s")
	require.NoError(t, err)
	defer release()
	block := make(chan struct{})
	repo.renewBlock = block
	defer close(block)
	r.mu.Lock()
	g := r.groups["s"]
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { g.renew(); close(done) }()
	select {
	case <-done:
	case <-time.After(400 * time.Millisecond):
		t.Fatal("blocked renewal outlived the locally confirmed protection deadline")
	}
	require.Error(t, context.Cause(ctx))
}
