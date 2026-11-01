package sandbox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type operationWaitTestRepository struct {
	*sharedManagerRepository
	count, reads atomic.Int64
	observeErr   error
	events       chan state.ActiveChangeBatch
	raceRead     bool
}

func (r *operationWaitTestRepository) WatchActiveChanges(context.Context, int64) (<-chan state.ActiveChangeBatch, error) {
	return r.events, nil
}
func (r *operationWaitTestRepository) ObserveOperations(context.Context, string) (state.ActiveOperationObservation, error) {
	n := r.reads.Add(1)
	count := r.count.Load()
	if n == 1 && r.raceRead {
		r.count.Store(0)
		r.events <- state.ActiveChangeBatch{Revision: 3, Changes: []state.ActiveChange{{SandboxID: "s", OperationChanged: true}}}
	}
	return state.ActiveOperationObservation{Count: count, Revision: 2}, r.observeErr
}
func newOperationWaitTest(t *testing.T) (*Manager, *operationWaitTestRepository) {
	t.Helper()
	r := &operationWaitTestRepository{sharedManagerRepository: &sharedManagerRepository{sharedDataTestRepository: newSharedDataTestRepository()}, events: make(chan state.ActiveChangeBatch, 1)}
	r.count.Store(2)
	m := NewManager(newMockRuntime(), nil, nil, ManagerConfig{RuntimeType: "kubernetes", ActiveSandboxes: r, SharedDataOperations: true})
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	return m, r
}
func TestSharedDataWaitNoNotificationRechecksAtMostOncePerSecond(t *testing.T) {
	m, r := newOperationWaitTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 1150*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, m.waitDistributedOperations(ctx, "s", nil), context.DeadlineExceeded)
	require.LessOrEqual(t, r.reads.Load(), int64(3), "initial read, unknown-history reset, then at most one periodic check")
	require.GreaterOrEqual(t, r.reads.Load(), int64(2))
}
func TestSharedDataWaitEventBetweenReadAndSubscribe(t *testing.T) {
	m, r := newOperationWaitTest(t)
	r.raceRead = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, m.waitDistributedOperations(ctx, "s", nil))
}
func TestSharedDataWaitReadErrorDoesNotDrain(t *testing.T) {
	m, r := newOperationWaitTest(t)
	r.count.Store(0)
	r.observeErr = errors.New("authoritative read unavailable")
	require.ErrorContains(t, m.waitDistributedOperations(context.Background(), "s", nil), "authoritative read unavailable")
}
func TestSharedDataWaitCoalescesSameSandbox(t *testing.T) {
	m, r := newOperationWaitTest(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 16)
	for range 16 {
		go func() { done <- m.waitDistributedOperations(ctx, "s", nil) }()
	}
	for range 16 {
		require.ErrorIs(t, <-done, context.DeadlineExceeded)
	}
	require.LessOrEqual(t, r.reads.Load(), int64(2), "waiters must share the authoritative observation")
}
