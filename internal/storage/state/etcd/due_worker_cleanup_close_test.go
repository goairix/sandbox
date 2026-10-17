package etcd

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// This host test proves local native-call ownership, not physical admission.
// The fixture's bootstrap session is released before the real DueWorker starts;
// its current shares are never refilled or advanced by test code.
func TestDueWorkerCleanupCloseCapacity(t *testing.T) {
	for _, mode := range []string{"stop", "backend-close"} {
		t.Run(mode, func(t *testing.T) {
			f := newCleanupActorFixture(t)
			jobs := []*cleanupLocalJob{f.queue(t, f.ctx, f.ref)}
			owner := workerTestReceive(t, f.probe.entered)
			for i := 1; i < 32; i++ {
				ref := f.ref
				ref.TaskID = uuid.NewString()
				jobs = append(jobs, f.queue(t, f.ctx, ref))
			}
			ref := f.ref
			ref.TaskID = uuid.NewString()
			overflow, err := f.w.enqueueCleanupLocal(f.ctx, ref)
			require.ErrorIs(t, err, ErrConflict)
			require.Nil(t, overflow)

			f.b.archiveLife.mu.Lock()
			registered, admitting := 0, 0
			for _, job := range f.w.cleanupJobs {
				if job != nil {
					registered++
					if job.owner != nil {
						admitting++
					}
				}
			}
			operation, shareOwner := f.w.session.operation, owner.share.dueOwner
			f.b.archiveLife.mu.Unlock()
			require.Equal(t, 32, registered)
			require.Equal(t, 1, admitting)
			require.Same(t, owner.call, operation)
			require.Same(t, owner, shareOwner)
			require.Same(t, f.w.session, owner.call.session)
			require.Same(t, f.w.scans[0].partition, owner.partition)
			f.assertNoPhysicalGrant(t)

			closed := make(chan error, 1)
			closeDone := make(chan struct{})
			go func() {
				defer close(closeDone)
				if mode == "stop" {
					closed <- f.w.Stop()
				} else {
					closed <- f.b.Close()
				}
			}()
			// A Fatal assertion must release the actual native-return barrier
			// and join the close goroutine before fixture Backend cleanup runs.
			defer func() {
				f.probe.unblock()
				<-closeDone
			}()
			workerTestReceive(t, owner.call.ctx.Done())
			for name, done := range map[string]<-chan struct{}{
				"close": closeDone, "owner": owner.done, "work": f.w.workDone, "worker": f.w.Done(),
			} {
				select {
				case <-done:
					t.Fatalf("%s completed before local native return", name)
				default:
				}
			}
			for _, job := range jobs {
				select {
				case <-job.done:
					t.Fatal("registered job removed before active native return")
				default:
				}
			}

			f.probe.unblock()
			closeErr := workerTestReceive(t, closed)
			t.Logf("close capacity mode=%s actual close returned error=%v", mode, closeErr)
			workerTestReceive(t, closeDone)
			workerTestReceive(t, owner.done)
			workerTestReceive(t, f.w.workDone)
			workerTestReceive(t, f.w.Done())
			for _, job := range jobs {
				workerTestReceive(t, job.done)
			}
			f.b.archiveLife.mu.Lock()
			remaining := f.w.cleanupJobs
			f.b.archiveLife.mu.Unlock()
			for _, job := range remaining {
				require.Nil(t, job)
			}
			require.False(t, f.w.Status().Running)
			require.False(t, f.w.Status().Synchronized)
			f.assertNoPhysicalGrant(t)
			t.Logf("close capacity mode=%s original native owner + all32 jobDone + workDone + finalDone actually joined", mode)
		})
	}
}
