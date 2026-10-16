package etcd

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The blocker deliberately ignores cancellation until its physical callback
// returns. A canceled context alone is therefore insufficient to pass a join.
type dueWorkerLifetimeBlock struct {
	entered chan context.Context
	release chan struct{}
	once    sync.Once
}

func newDueWorkerLifetimeBlock() *dueWorkerLifetimeBlock {
	return &dueWorkerLifetimeBlock{entered: make(chan context.Context, 1), release: make(chan struct{})}
}

func (block *dueWorkerLifetimeBlock) hold(ctx context.Context) {
	block.entered <- ctx
	<-block.release
}

func (block *dueWorkerLifetimeBlock) unblock() {
	block.once.Do(func() { close(block.release) })
}

func dueWorkerLifetimeStillJoined(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("release returned before actual owned work: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestDueWorkerLifetimeStopJoinsClock(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	block := newDueWorkerLifetimeBlock()
	f.b.authorityClock = testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
		block.hold(ctx)
		return ClockObservation{UTC: f.now}, ctx.Err()
	})
	w := f.start(t, dueWorkerActorOptions())
	t.Cleanup(block.unblock) // Registered after Stop cleanup: release first on Fatal.
	owned := workerTestReceive(t, block.entered)
	s := f.original(t, w)
	before, _, revokes := f.server.counts()
	done := make(chan error, 1)
	go func() { done <- w.Stop() }()
	workerTestReceive(t, owned.Done())
	dueWorkerLifetimeStillJoined(t, done)
	_, _, blockedRevokes := f.server.counts()
	require.Equal(t, revokes, blockedRevokes, "cleanup cannot run while the clock still owns native work")

	block.unblock()
	_ = workerTestReceive(t, done)
	workerTestReceive(t, w.Done())
	require.Nil(t, f.server.point(s.guard.key))
	after, _, finalRevokes := f.server.counts()
	require.Equal(t, before, after)
	require.Equal(t, revokes+1, finalRevokes)
	require.False(t, w.Status().Running)
}

func TestDueWorkerLifetimeStopCloseJoinAdmission(t *testing.T) {
	for _, phase := range []string{"grant", "commit"} {
		t.Run(phase, func(t *testing.T) {
			f := newDueWorkerActorFixture(t)
			block := newDueWorkerLifetimeBlock()
			originalLease := f.b.client.Lease
			var workerRevokes atomic.Int32
			var originalID atomic.Int64
			wrapper := &workerTestLease{Lease: originalLease, revoke: func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
				if int64(id) == originalID.Load() {
					workerRevokes.Add(1)
				}
				return originalLease.Revoke(ctx, id)
			}}
			if phase == "grant" {
				wrapper.grant = func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
					if ttl == 30 {
						block.hold(ctx)
					}
					return originalLease.Grant(ctx, ttl)
				}
			} else {
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(f.keys[1]), before: func() {
					// The actual transaction remains blocked after pending and
					// Stage Grant, before any metadata mutation is dispatched.
					block.hold(context.Background())
				}}
			}
			f.b.client.Lease = wrapper
			w := f.start(t, dueWorkerActorOptions())
			t.Cleanup(block.unblock)
			originalID.Store(w.Status().Session.LeaseID)
			workerTestReceive(t, block.entered)
			pending := dueCheckpointTestRaw(t, f.duePageFixture)
			require.NotNil(t, pending.Pending)
			s := f.original(t, w)
			stopDone, closeDone := make(chan error, 1), make(chan error, 1)
			go func() { stopDone <- w.Stop() }()
			go func() { closeDone <- f.b.Close() }()
			dueWorkerLifetimeStillJoined(t, stopDone)
			dueWorkerLifetimeStillJoined(t, closeDone)
			require.Zero(t, workerRevokes.Load())

			block.unblock()
			_ = workerTestReceive(t, stopDone)
			require.NoError(t, workerTestReceive(t, closeDone))
			workerTestReceive(t, w.Done())
			require.Equal(t, int32(1), workerRevokes.Load())
			require.Nil(t, f.server.point(s.guard.key))
			require.Empty(t, f.tasks())
			require.NotNil(t, dueCheckpointTestRaw(t, f.duePageFixture).Pending, "cancellation must retain the exact unresolved request")
		})
	}
}

func TestDueWorkerLifetimeNativeWatchJoined(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	entered, stopped := make(chan struct{}, 16), make(chan struct{}, 16)
	f.server.mu.Lock()
	f.server.watch = func(stream pb.Watch_WatchServer) error {
		defer func() { stopped <- struct{}{} }()
		if _, err := dueWatchTestCreates(stream, 1, dueWatchTestHeader(f.server)); err != nil {
			return err
		}
		entered <- struct{}{}
		// No progress certificate is invented. Stop must cancel/join the
		// actual generated stream, even while it waits for real coverage.
		<-stream.Context().Done()
		return nil
	}
	f.server.mu.Unlock()
	w := f.start(t, dueWorkerActorOptions())
	workerTestReceive(t, entered)
	s := f.original(t, w)
	_ = w.Stop()
	workerTestReceive(t, w.Done())
	workerTestReceive(t, stopped)
	f.b.archiveLife.mu.Lock()
	nativeOwner := s.dueWatch
	f.b.archiveLife.mu.Unlock()
	require.Nil(t, nativeOwner)
	require.Nil(t, f.server.point(s.guard.key))
}

func TestDueWorkerLifetimeChildReleaseStopCloseNoCycle(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	block := newDueWorkerLifetimeBlock()
	f.b.authorityClock = testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
		block.hold(ctx)
		return ClockObservation{UTC: f.now}, ctx.Err()
	})
	w := f.start(t, dueWorkerActorOptions())
	t.Cleanup(block.unblock)
	workerTestReceive(t, block.entered)
	s := f.original(t, w)
	f.b.archiveLife.mu.Lock()
	p := s.partitions[0xaa]
	var share *WorkerBudgetShare
	for _, candidate := range s.budgets {
		if candidate != nil && candidate.kind == WorkerBudgetRange {
			share = candidate
			break
		}
	}
	f.b.archiveLife.mu.Unlock()
	require.NotNil(t, p)
	require.NotNil(t, share)
	_, _, beforeRevokes := f.server.counts()

	partitionDone, budgetDone := make(chan error, 1), make(chan error, 1)
	sessionDone, stopDone, closeDone := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	go func() { partitionDone <- f.b.ReleaseWorkerPartition(context.Background(), p) }()
	go func() { budgetDone <- f.b.ReleaseWorkerBudget(context.Background(), share) }()
	go func() { sessionDone <- f.b.ReleaseWorkerSession(context.Background(), s) }()
	go func() { stopDone <- w.Stop() }()
	go func() { closeDone <- f.b.Close() }()
	for _, done := range []<-chan error{partitionDone, budgetDone, sessionDone, stopDone, closeDone} {
		dueWorkerLifetimeStillJoined(t, done)
	}

	// Session cleanup may wait child r.done; child cleanup must publish that
	// channel before joining final worker done, or this five-way join cycles.
	block.unblock()
	_ = workerTestReceive(t, partitionDone)
	_ = workerTestReceive(t, budgetDone)
	require.NoError(t, workerTestReceive(t, sessionDone))
	_ = workerTestReceive(t, stopDone)
	require.NoError(t, workerTestReceive(t, closeDone))
	workerTestReceive(t, w.Done())
	f.b.archiveLife.mu.Lock()
	partitionRelease, budgetRelease := p.release, share.release
	f.b.archiveLife.mu.Unlock()
	if partitionRelease != nil {
		workerTestReceive(t, partitionRelease.done)
	}
	if budgetRelease != nil {
		workerTestReceive(t, budgetRelease.done)
	}
	_, _, afterRevokes := f.server.counts()
	require.Equal(t, beforeRevokes+1, afterRevokes)
}

func TestDueWorkerLifetimeStartupFailureCleanupJoined(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	holder, err := f.b.OpenWorkerSession(context.Background(), "existing-partition-owner")
	require.NoError(t, err)
	_, err = f.b.ClaimWorkerPartition(context.Background(), holder, 0xaa)
	require.NoError(t, err)
	block := newDueWorkerLifetimeBlock()
	t.Cleanup(block.unblock)
	original := f.b.client.Lease
	var cleanupCalls atomic.Int32
	f.b.client.Lease = &workerTestLease{Lease: original, revoke: func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
		if int64(id) != holder.Reference().LeaseID {
			cleanupCalls.Add(1)
			block.hold(ctx)
		}
		return original.Revoke(ctx, id)
	}}
	startDone := make(chan error, 1)
	go func() {
		_, err := f.b.StartDueWorker(context.Background(), dueWorkerActorOptions())
		startDone <- err
	}()
	workerTestReceive(t, block.entered)
	dueWorkerLifetimeStillJoined(t, startDone)
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.b.Close() }()
	dueWorkerLifetimeStillJoined(t, closeDone)

	block.unblock()
	require.Error(t, workerTestReceive(t, startDone))
	require.NoError(t, workerTestReceive(t, closeDone))
	require.Equal(t, int32(1), cleanupCalls.Load())
}

func TestDueWorkerLifetimeCloseAfterSessionCleanupBeforeFinalDone(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	s, err := f.b.OpenWorkerSession(context.Background(), "controlled-final-publication")
	require.NoError(t, err)
	owned, cancel := context.WithCancel(s.ctx)
	w := &DueWorker{backend: f.b, session: s, ctx: owned, cancel: cancel, workDone: make(chan struct{}), done: make(chan struct{}), status: DueWorkerStatus{Session: s.Reference(), Running: true, Phase: "stopping"}}
	w.self = w
	f.b.archiveLife.mu.Lock()
	s.dueWorker = w
	close(w.workDone)
	f.b.archiveLife.mu.Unlock()
	var finalOnce sync.Once
	publishFinal := func() {
		finalOnce.Do(func() {
			// This controlled same-package fixture holds only final local
			// publication. Native work and the actual release core are not
			// replaced; final removal/done use their shared lifecycle lock.
			f.b.archiveLife.mu.Lock()
			w.status.Running, w.status.Phase = false, "stopped"
			if f.b.workerControl.sessions[s.slot] == s {
				f.b.workerControl.sessions[s.slot] = nil
			}
			close(w.done)
			f.b.archiveLife.mu.Unlock()
		})
	}
	t.Cleanup(publishFinal)
	_, _, before := f.server.counts()
	require.NoError(t, f.b.releaseWorkerSessionCore(context.Background(), s, w))
	workerTestReceive(t, s.release.done)
	require.Nil(t, f.server.point(s.guard.key))
	_, _, after := f.server.counts()
	require.Equal(t, before+1, after, "actual original lease cleanup is already complete")
	select {
	case <-w.Done():
		t.Fatal("controlled final publication completed prematurely")
	default:
	}

	// Close enters strictly after the core has returned. It must still find
	// this original controller and join final done before closing the client.
	closeDone := make(chan error, 1)
	go func() { closeDone <- f.b.Close() }()
	dueWorkerLifetimeStillJoined(t, closeDone)
	publishFinal()
	require.NoError(t, workerTestReceive(t, closeDone))
	workerTestReceive(t, w.Done())
	_, _, finalRevokes := f.server.counts()
	require.Equal(t, after, finalRevokes, "late Close joins rather than repeats original cleanup")
}
