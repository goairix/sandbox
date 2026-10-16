package etcd

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This host fixture owns a real registered actor job and real E/S transactions.
// Its Run.Worker is diagnostic metadata only: no Kernel, TaskClaim or physical
// work is fabricated. The test owns and joins its one metadata continuation;
// native bound-recovery separately proves actual effect-bearing work joins.
func cleanupProgressJob(t *testing.T) (*cleanupActorFixture, *cleanupLocalJob, string, string, *atomic.Int64) {
	t.Helper()
	f := newCleanupSlotTestFixture(t)
	require.NoError(t, f.b.ReleaseWorkerSession(f.ctx, f.session))
	f.b.authorityClock = testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: time.Now().UTC()}, ctx.Err()
	})
	f.b.localCleanup = &LocalCleanupOptions{MaxActive: 32}
	executionKey, err := f.b.namespace.cleanupExecutionKey(f.ref)
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var intercepted, returned, dropped atomic.Bool
	committed := new(atomic.Int64)
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool {
		for _, op := range ops {
			if string(op.KeyBytes()) != executionKey {
				continue
			}
			if op.IsGet() && intercepted.CompareAndSwap(false, true) {
				return true
			}
			if op.IsPut() && strings.Contains(string(op.ValueBytes()), `"confirmed":"archive-bound"`) && dropped.CompareAndSwap(false, true) {
				return true
			}
		}
		return false
	}, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if returned.CompareAndSwap(false, true) {
			close(entered)
			<-release
			return response, err
		}
		if err != nil || response == nil || !response.Succeeded {
			return response, err
		}
		committed.Store(response.Header.Revision)
		// The server's real successful CAS is retained. Only its ACK is lost;
		// the production actor still owns and joins this actual native return.
		return nil, context.DeadlineExceeded
	}}
	lease := &creationFaultLease{Lease: f.b.client.Lease}
	f.b.client.Lease = lease
	w, err := f.b.StartDueWorker(f.ctx, DueWorkerOptions{WorkerID: "cleanup-progress", Partitions: []uint8{f.ref.Partition}, RangeSlots: []uint16{0}, ClaimSlots: []uint16{0}, RelistSlot: 0, StageTTL: 30 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() {
		unblock()
		_ = w.Stop()
		workerTestReceive(t, w.Done())
	})
	a := &cleanupActorFixture{cleanupSlotTestFixture: f, w: w, lease: lease, initialGrants: lease.grants.Load()}
	j := a.queue(t, f.ctx, f.ref)
	workerTestReceive(t, entered)
	// Park this registered metadata job before the actor can begin another
	// admission turn. No physical goroutine is started by this host fixture.
	f.b.archiveLife.mu.Lock()
	j.started = true
	f.b.archiveLife.mu.Unlock()
	t.Cleanup(func() { f.b.finishCleanupJob(j, nil) })
	unblock()

	// A second real metadata job must finish before we initialize the parked
	// job's diagnostic Run. This joins the first actor turn without sleeps or
	// racing cleanupTurnCompleted's access to its admission state.
	missing := f.ref
	missing.TaskID = uuid.NewString()
	barrier := a.queue(t, f.ctx, missing)
	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	select {
	case <-barrier.done:
	case <-deadline.C:
		t.Fatal("original metadata actor did not finish the barrier job")
	}
	require.Error(t, barrier.err)
	e, s, record := cleanupLocalRetainedRows(t, a, "archive")
	points, err := f.raw.Txn(f.ctx).Then(clientv3.OpGet(e), clientv3.OpGet(s)).Commit()
	require.NoError(t, err)
	j.state, err = f.b.cleanupStateFromPoints(f.ref, points.Responses[0].GetResponseRange().Kvs[0], points.Responses[1].GetResponseRange().Kvs[0], points.Header.Revision)
	require.NoError(t, err)
	j.runID = record.Run.ID
	return a, j, e, s, committed
}

func TestCleanupLocalProgressUnknownJoin(t *testing.T) {
	for _, scenario := range []string{"committed-progress", "foreign-job", "slot-aba", "unexpected-progress"} {
		t.Run(scenario, func(t *testing.T) {
			f, job, e, s, committed := cleanupProgressJob(t)
			prior := job.state
			beforeSlot, err := f.raw.Get(f.ctx, s)
			require.NoError(t, err)
			archive := &cleanupExecutionArchive{StoreID: strings.Repeat("a", 64), Slot: 0, BindingDigest: strings.Repeat("b", 64)}
			err = job.progress("archive-bound", nil, archive)
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Positive(t, committed.Load())
			require.Same(t, prior, job.state, "unknown ACK cannot invent an observed revision")
			points, err := f.raw.Txn(f.ctx).Then(clientv3.OpGet(e), clientv3.OpGet(s)).Commit()
			require.NoError(t, err)
			observed, err := f.b.cleanupStateFromPoints(f.ref, points.Responses[0].GetResponseRange().Kvs[0], points.Responses[1].GetResponseRange().Kvs[0], points.Header.Revision)
			require.NoError(t, err)
			require.Equal(t, committed.Load(), observed.execution.mod)
			require.Greater(t, observed.execution.mod, prior.execution.mod)
			require.Equal(t, "archive-bound", observed.record.Confirmed)
			require.False(t, observed.record.Run.Joined)
			require.Equal(t, prior.record.Run, observed.record.Run)
			require.Equal(t, archive, observed.record.Archive)

			// The original actor has replied after actual native return. This
			// fixture has no other effects or transports to join.
			job.effectsJoined = true
			candidate := job
			switch scenario {
			case "foreign-job":
				foreign := *job
				foreign.state = observed
				candidate = &foreign
			case "slot-aba":
				_, err = f.raw.Delete(f.ctx, s)
				require.NoError(t, err)
				_, err = f.raw.Put(f.ctx, s, string(beforeSlot.Kvs[0].Value))
				require.NoError(t, err)
			case "unexpected-progress":
				changed := observed.record
				changed.Archive = &cleanupExecutionArchive{StoreID: strings.Repeat("c", 64), Slot: 0, BindingDigest: archive.BindingDigest}
				wire, encodeErr := encodeCleanupExecution(changed)
				require.NoError(t, encodeErr)
				_, err = f.raw.Put(f.ctx, e, wire)
				require.NoError(t, err)
			}
			before, err := f.raw.Txn(f.ctx).Then(clientv3.OpGet(e), clientv3.OpGet(s)).Commit()
			require.NoError(t, err)
			cleanup, stop := context.WithTimeout(f.ctx, 2*time.Second)
			err = f.b.joinCleanupRun(cleanup, candidate)
			stop()
			after, readErr := f.raw.Txn(f.ctx).Then(clientv3.OpGet(e), clientv3.OpGet(s)).Commit()
			require.NoError(t, readErr)
			require.Equal(t, before.Responses[1].GetResponseRange().Kvs, after.Responses[1].GetResponseRange().Kvs, "joining never mutates or releases the original slot")
			if scenario != "committed-progress" {
				require.Error(t, err, "only the registered original job can reconcile its exact submitted progress and original occupation")
				require.Equal(t, before.Responses[0].GetResponseRange().Kvs, after.Responses[0].GetResponseRange().Kvs)
			} else {
				require.NoError(t, err, "original live job must resolve its committed progress before the joined CAS")
				require.True(t, job.state.record.Run.Joined)
				require.Equal(t, "archive-bound", job.state.record.Confirmed)
				require.Equal(t, archive, job.state.record.Archive)
				require.Equal(t, prior.execution.create, job.state.execution.create)
				require.Greater(t, job.state.execution.mod, committed.Load())
				joined := observed.record
				joinedRun := *joined.Run
				joinedRun.Joined = true
				joined.Run = &joinedRun
				expected, encodeErr := encodeCleanupExecution(joined)
				require.NoError(t, encodeErr)
				require.Equal(t, expected, string(after.Responses[0].GetResponseRange().Kvs[0].Value))
				require.Equal(t, beforeSlot.Kvs, after.Responses[1].GetResponseRange().Kvs)
			}
			f.assertNoPhysicalGrant(t)
		})
	}
}
