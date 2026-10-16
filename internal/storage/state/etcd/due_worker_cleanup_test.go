package etcd

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Host coverage stops at original metadata admission. The real actor, original
// partition/share, native client and permanent Task come from the live fixture;
// the fixed queue configuration contains no KernelBoundary or physical success
// hook. Public execution must reject this incomplete startup profile.
type cleanupActorFixture struct {
	*cleanupSlotTestFixture
	w             *DueWorker
	lease         *creationFaultLease
	initialGrants int64
	probe         *cleanupActorProbe
}

// The probe observes actual original Claim owners and delays one native return.
// It never supplies a response: every selected request reaches the real etcd
// cluster. Cancellation cannot unblock the delayed return by itself.
type cleanupActorProbe struct {
	mu      sync.Mutex
	owners  map[*workerDueOwner]int
	order   []*workerDueOwner
	entered chan *workerDueOwner
	release chan struct{}
	once    sync.Once
	blocked bool
}

func (p *cleanupActorProbe) unblock() { p.once.Do(func() { close(p.release) }) }

func newCleanupActorFixture(t *testing.T) *cleanupActorFixture {
	t.Helper()
	f := newCleanupSlotTestFixture(t)
	require.NoError(t, f.b.ReleaseWorkerSession(f.ctx, f.session))
	f.b.authorityClock = testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: time.Now().UTC()}, ctx.Err()
	})
	// This private queue-only configuration is deliberately unusable by the
	// public execution entry. It cannot satisfy inventory or TaskClaim gates.
	f.b.localCleanup = &LocalCleanupOptions{MaxActive: 32}
	p := &cleanupActorProbe{owners: make(map[*workerDueOwner]int), entered: make(chan *workerDueOwner, 1), release: make(chan struct{})}
	var selected *workerDueOwner
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool {
		f.b.archiveLife.mu.Lock()
		var owner *workerDueOwner
		for _, session := range f.b.workerControl.sessions {
			if session == nil || session.dueWorker == nil {
				continue
			}
			for _, share := range session.dueWorker.claims {
				if share.dueOwner != nil {
					owner = share.dueOwner
				}
			}
		}
		f.b.archiveLife.mu.Unlock()
		if owner == nil {
			return false
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.owners[owner] == 0 {
			p.order = append(p.order, owner)
		}
		p.owners[owner]++
		if p.blocked {
			return false
		}
		p.blocked, selected = true, owner
		return true
	}, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		p.entered <- selected
		<-p.release
		return response, err
	}}
	lease := &creationFaultLease{Lease: f.b.client.Lease}
	f.b.client.Lease = lease
	w, err := f.b.StartDueWorker(f.ctx, DueWorkerOptions{WorkerID: "cleanup-actor", Partitions: []uint8{f.ref.Partition}, RangeSlots: []uint16{0}, ClaimSlots: []uint16{0}, RelistSlot: 0, StageTTL: 30 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() {
		p.unblock()
		_ = w.Stop()
		workerTestReceive(t, w.Done())
	})
	return &cleanupActorFixture{cleanupSlotTestFixture: f, w: w, lease: lease, initialGrants: lease.grants.Load(), probe: p}
}

func (f *cleanupActorFixture) queue(t *testing.T, ctx context.Context, ref TaskReference) *cleanupLocalJob {
	t.Helper()
	job, err := f.w.enqueueCleanupLocal(ctx, ref)
	require.NoError(t, err)
	require.NotNil(t, job)
	return job
}

func (f *cleanupActorFixture) assertNoPhysicalGrant(t *testing.T) {
	t.Helper()
	require.Equal(t, f.initialGrants, f.lease.grants.Load(), "host queue metadata cannot authorize a TaskClaim Lease")
}

func TestDueWorkerCleanupSharedBudget(t *testing.T) {
	f := newCleanupActorFixture(t)
	job := f.queue(t, f.ctx, f.ref)
	owner := workerTestReceive(t, f.probe.entered)
	f.b.archiveLife.mu.Lock()
	session, partition, share := owner.call.session, owner.partition, owner.share
	operation, tokens := f.w.session.operation, owner.share.bucket.tokens
	// Check nonrefund at the bucket's own immutable instant, without a wall
	// clock sleep that could legitimately replenish a credit.
	refunded := owner.share.bucket.take(owner.share.bucket.last, 1)
	f.b.archiveLife.mu.Unlock()
	require.Same(t, f.w.session, session)
	require.Same(t, f.w.scans[0].partition, partition)
	require.Same(t, f.w.claims[0], share)
	require.Same(t, owner.call, operation)
	require.Zero(t, tokens)
	require.False(t, refunded)
	second, stop, err := f.b.beginExpiredTaskOwner(f.ctx, owner.partition, owner.share)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, second)
	require.Nil(t, stop)
	f.assertNoPhysicalGrant(t)

	f.probe.unblock()
	// Whole admission spans four charged turns: 4*2 RPCs at q=1s,
	// three credit waits and one second of slack give a finite 12s bound.
	joined := time.NewTimer(12 * time.Second)
	defer joined.Stop()
	select {
	case <-job.done:
	case <-joined.C:
		t.Fatal("cleanup admission did not join within its bounded turns")
	}
	require.Error(t, job.err, "unsupported host target is refused after original metadata work")
	f.probe.mu.Lock()
	counts := make([]int, 0, len(f.probe.owners))
	for _, count := range f.probe.owners {
		counts = append(counts, count)
	}
	f.probe.mu.Unlock()
	require.NotEmpty(t, counts)
	for _, count := range counts {
		require.LessOrEqual(t, count, 2, "each metadata turn must yield with its own charged owner")
	}
	f.assertNoPhysicalGrant(t)
}

func TestDueWorkerCleanupFairness(t *testing.T) {
	f := newCleanupActorFixture(t)
	jobs := []*cleanupLocalJob{f.queue(t, f.ctx, f.ref)}
	owner := workerTestReceive(t, f.probe.entered)
	for i := 1; i < 32; i++ {
		ref := f.ref
		ref.TaskID = uuid.NewString()
		jobs = append(jobs, f.queue(t, f.ctx, ref))
	}
	duplicate, err := f.w.enqueueCleanupLocal(f.ctx, f.ref)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, duplicate)
	other := f.ref
	other.TaskID = uuid.NewString()
	overflow, err := f.w.enqueueCleanupLocal(f.ctx, other)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, overflow, "queued plus admitting must share the same fixed32 table")
	f.assertNoPhysicalGrant(t)

	// Cancellation must not remove the currently executing native owner or
	// free a table entry until its real response has returned.
	f.w.cancel()
	workerTestReceive(t, owner.call.ctx.Done())
	select {
	case <-jobs[0].done:
		t.Fatal("cancellation freed the active job before native return")
	default:
	}
	f.probe.unblock()
	workerTestReceive(t, f.w.Done())
	for _, job := range jobs {
		workerTestReceive(t, job.done)
	}
	f.assertNoPhysicalGrant(t)
}

func TestDueWorkerCleanupStopJoin(t *testing.T) {
	for _, stopKind := range []string{"caller", "worker", "backend", "partition"} {
		t.Run(stopKind, func(t *testing.T) {
			f := newCleanupActorFixture(t)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			job := f.queue(t, ctx, f.ref)
			owner := workerTestReceive(t, f.probe.entered)
			stopped := make(chan error, 1)
			go func() {
				switch stopKind {
				case "caller":
					cancel()
					<-job.done
					stopped <- job.err
				case "worker":
					stopped <- f.w.Stop()
				case "backend":
					stopped <- f.b.Close()
				case "partition":
					stopped <- f.b.ReleaseWorkerPartition(context.Background(), owner.partition)
				}
			}()
			workerTestReceive(t, owner.call.ctx.Done())
			select {
			case <-job.done:
				t.Fatal("job joined before its received continuation returned")
			default:
			}
			select {
			case <-stopped:
				t.Fatal("stop returned before original native call joined")
			default:
			}
			select {
			case <-f.w.workDone:
				t.Fatal("worker workDone preceded actual cleanup join")
			default:
			}
			f.probe.unblock()
			_ = workerTestReceive(t, stopped)
			workerTestReceive(t, job.done)
			workerTestReceive(t, owner.done)
			f.assertNoPhysicalGrant(t)
		})
	}
}

func TestDueWorkerCleanupClaimRollover(t *testing.T) {
	// Positive two-Claim rollover requires the genuine Linux worker and a
	// bound local archive. Here an unstarted original job cannot request that
	// continuation or turn queue membership into physical permission.
	f := newCleanupActorFixture(t)
	job := f.queue(t, f.ctx, f.ref)
	_ = workerTestReceive(t, f.probe.entered)
	claim, err := f.w.requestCleanupClaim(job)
	require.Error(t, err)
	require.Nil(t, claim)
	f.assertNoPhysicalGrant(t)
	f.probe.unblock()
	// Whole admission spans four charged turns: 4*2 RPCs at q=1s,
	// three credit waits and one second of slack give a finite 12s bound.
	joined := time.NewTimer(12 * time.Second)
	defer joined.Stop()
	select {
	case <-job.done:
	case <-joined.C:
		t.Fatal("cleanup admission did not join within its bounded turns")
	}
}
