package etcd

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A real actor is held at its original native return while this metadata-only
// job is registered. No Kernel, target, successful opener or permission is
// fabricated. The test barrier delays only the preparation owner's return;
// genuine Linux target open/Close is covered by the native consumer scenarios.
func cleanupTargetMetadataJob(t *testing.T, f *cleanupActorFixture) *cleanupLocalJob {
	t.Helper()
	ref := f.ref
	ref.TaskID = uuid.NewString()
	j := f.queue(t, f.ctx, ref)
	f.b.archiveLife.mu.Lock()
	j.started, j.step = true, "target"
	j.bundle = &taskClaimBundle{task: TaskRecord{Reference: ref}}
	j.target = &cleanupLocalTarget{}
	f.b.archiveLife.mu.Unlock()
	return j
}

func TestDueWorkerCleanupTargetPreparationJoin(t *testing.T) {
	for _, mode := range []string{"stop", "backend-close"} {
		t.Run(mode, func(t *testing.T) {
			f := newCleanupActorFixture(t)
			_ = f.queue(t, f.ctx, f.ref)
			owner := workerTestReceive(t, f.probe.entered)
			j := cleanupTargetMetadataJob(t, f)

			foreign := *j
			require.ErrorIs(t, f.b.prepareCleanupLocalTarget(&foreign), ErrInvalidRecord)
			f.b.archiveLife.mu.Lock()
			j.owner = owner
			f.b.archiveLife.mu.Unlock()
			require.ErrorIs(t, f.b.prepareCleanupLocalTarget(j), ErrInvalidRecord)
			f.b.archiveLife.mu.Lock()
			j.owner = nil
			f.b.archiveLife.mu.Unlock()
			require.ErrorIs(t, f.b.prepareCleanupLocalTarget(j), ErrInvalidConfiguration, "registered metadata alone cannot open a physical target")

			returned := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(returned) }) }
			jobReturned := make(chan struct{})
			go func() {
				defer close(jobReturned)
				<-returned
				// Run the actual failure tail after the test-owned preparation
				// return. There is no physical success or persistent Run here.
				f.b.runCleanupLocal(j)
			}()
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
			defer func() {
				f.probe.unblock()
				release()
				<-jobReturned
				<-closeDone
			}()
			workerTestReceive(t, j.ctx.Done())
			f.probe.unblock()
			workerTestReceive(t, owner.done)
			for name, done := range map[string]<-chan struct{}{"job": j.done, "close": closeDone, "work": f.w.workDone, "worker": f.w.Done()} {
				select {
				case <-done:
					t.Fatalf("%s returned before preparation owner", name)
				default:
				}
			}
			release()
			workerTestReceive(t, jobReturned)
			workerTestReceive(t, j.done)
			workerTestReceive(t, closed)
			workerTestReceive(t, closeDone)
			workerTestReceive(t, f.w.Done())
			require.Empty(t, j.runID)
			require.Nil(t, j.state)
			f.assertNoPhysicalGrant(t)
		})
	}
	t.Run("actor-progress-and-renewal", func(t *testing.T) {
		f := newCleanupActorFixture(t)
		_ = f.queue(t, f.ctx, f.ref)
		owner := workerTestReceive(t, f.probe.entered)
		preparing := cleanupTargetMetadataJob(t, f)
		returned := make(chan struct{})
		joined := make(chan struct{})
		var once sync.Once
		release := func() { once.Do(func() { close(returned) }) }
		go func() {
			defer close(joined)
			<-returned
			f.b.runCleanupLocal(preparing)
		}()
		defer func() {
			f.probe.unblock()
			release()
			<-joined
		}()
		ref := f.ref
		ref.TaskID = uuid.NewString()
		next := f.queue(t, f.ctx, ref)
		before, keeps := f.w.Status().Renewals, f.lease.keeps.Load()
		f.b.archiveLife.mu.Lock()
		// Only shorten local authority. The actual server Lease is untouched;
		// original renewIfNeeded must perform real KeepAliveOnce to extend it.
		deadline := time.Now().Add(20 * time.Second)
		if deadline.Before(f.w.session.deadline) {
			f.w.session.deadline = deadline
		}
		f.b.archiveLife.mu.Unlock()
		f.probe.unblock()
		workerTestReceive(t, owner.done)
		require.Eventually(t, func() bool {
			select {
			case <-next.done:
				return f.w.Status().Renewals > before
			default:
				return false
			}
		}, 12*time.Second, 5*time.Millisecond)
		require.Error(t, next.err, "unknown Task metadata must be read and refused by the real actor")
		require.Greater(t, f.lease.keeps.Load(), keeps)
		select {
		case <-preparing.done:
			t.Fatal("preparation owner returned before its test-owned barrier")
		default:
		}
		f.b.archiveLife.mu.Lock()
		nativeOwner := preparing.owner
		f.b.archiveLife.mu.Unlock()
		require.Nil(t, nativeOwner)
		release()
		workerTestReceive(t, joined)
		workerTestReceive(t, preparing.done)
		f.assertNoPhysicalGrant(t)
	})
}

func TestDueWorkerCleanupAdmissionContinuation(t *testing.T) {
	f := newCleanupActorFixture(t)
	_ = f.queue(t, f.ctx, f.ref)
	owner := workerTestReceive(t, f.probe.entered)
	j := cleanupTargetMetadataJob(t, f)
	reply := make(chan error, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		_, err := f.w.cleanupContinuation(j, &cleanupLocalContinuation{kind: "admit"})
		reply <- err
		f.b.finishCleanupJob(j, err)
	}()
	defer func() {
		j.cancel()
		f.probe.unblock()
		_ = f.w.Stop()
		<-returned
	}()
	require.Eventually(t, func() bool {
		f.b.archiveLife.mu.Lock()
		defer f.b.archiveLife.mu.Unlock()
		return j.request != nil
	}, time.Second, time.Millisecond)
	f.b.archiveLife.mu.Lock()
	step, started, nativeOwner := j.step, j.started, j.owner
	f.b.archiveLife.mu.Unlock()
	require.Equal(t, "publication", step, "admit continues existing metadata admission, not progress replay")
	require.True(t, started)
	require.Nil(t, nativeOwner)
	j.cancel()
	select {
	case <-returned:
		t.Fatal("accepted continuation detached on caller cancellation")
	default:
	}
	select {
	case <-j.done:
		t.Fatal("accepted job published done before its actor reply")
	default:
	}
	f.probe.unblock()
	workerTestReceive(t, owner.done)
	require.ErrorIs(t, workerTestReceive(t, reply), context.Canceled)
	workerTestReceive(t, returned)
	workerTestReceive(t, j.done)
	require.Empty(t, j.runID)
	require.Nil(t, j.state)
	f.assertNoPhysicalGrant(t)
}
