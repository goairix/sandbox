package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

// This host harness exercises completion publication and original session
// cleanup through the generated KV/Lease connection. It never starts physical
// admission: the synthetic completed job is only a private lifetime seam.
func cleanupResultWorker(t *testing.T, automatic bool) *DueWorker {
	t.Helper()
	b, _ := workerTestBackend(t)
	session, err := b.OpenWorkerSession(context.Background(), "cleanup-result")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(session.ctx)
	w := &DueWorker{
		backend: b, session: session, ctx: ctx, cancel: cancel,
		options:  DueWorkerOptions{AutomaticCleanup: automatic},
		workDone: make(chan struct{}), done: make(chan struct{}),
		cleanupWake: make(chan struct{}, 1),
	}
	w.self = w
	b.archiveLife.mu.Lock()
	session.dueWorker = w
	b.archiveLife.mu.Unlock()
	t.Cleanup(func() {
		select {
		case <-w.Done():
		default:
			w.finish(nil)
		}
	})
	return w
}

// The job has already left the physical chain. stopCaller remains the real
// completion hook, so a test can delay its actual return without canceling it.
func cleanupResultJob(w *DueWorker, slot int) *cleanupLocalJob {
	ctx, cancel := context.WithCancel(w.ctx)
	j := &cleanupLocalJob{
		worker: w, ctx: ctx, cancel: cancel, done: make(chan struct{}),
		started: true, stopCaller: func() {},
	}
	w.backend.archiveLife.mu.Lock()
	w.cleanupJobs[slot] = j
	w.backend.archiveLife.mu.Unlock()
	return j
}

// Removing a completed automatic job must not erase its failure before Stop.
// Explicit callers still own the exact original per-job result/error.
func TestDueWorkerCleanupResultCompleted(t *testing.T) {
	dependency := errors.New("archive dependency refused")
	for _, test := range []struct {
		name      string
		jobErr    error
		wantError bool
		wantCause error
	}{
		{name: "success"},
		{name: "canceled", jobErr: context.Canceled},
		{name: "nested-canceled", jobErr: fmt.Errorf("stopping: %w", errors.Join(context.Canceled, context.Canceled))},
		{name: "mixed-unknown", jobErr: errors.Join(context.Canceled, ErrOutcomeUnknown), wantError: true, wantCause: ErrOutcomeUnknown},
		{name: "mixed-dependency", jobErr: errors.Join(context.Canceled, dependency), wantError: true},
		{name: "deadline", jobErr: context.DeadlineExceeded, wantError: true, wantCause: context.DeadlineExceeded},
		{name: "dependency", jobErr: dependency, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			w := cleanupResultWorker(t, true)
			job := cleanupResultJob(w, 0)
			w.backend.finishCleanupJob(job, test.jobErr)
			workerTestReceive(t, job.done)
			require.Equal(t, test.jobErr, job.err)
			w.backend.archiveLife.mu.Lock()
			registered := w.cleanupJobs[0]
			w.backend.archiveLife.mu.Unlock()
			require.Nil(t, registered)

			w.finish(nil)
			terminal := w.Stop()
			if !test.wantError {
				require.NoError(t, terminal)
				return
			}
			require.Error(t, terminal)
			if test.wantCause != nil {
				require.ErrorIs(t, terminal, test.wantCause)
			}
			require.NotEmpty(t, w.Status().LastError)
			require.Equal(t, terminal, w.Stop(), "repeated Stop retains the joined result")
		})
	}
}

func TestDueWorkerCleanupResultNonAutomatic(t *testing.T) {
	w := cleanupResultWorker(t, false)
	job := cleanupResultJob(w, 0)
	failure := errors.Join(ErrOutcomeUnknown, errors.New("explicit dependency"))
	w.backend.finishCleanupJob(job, failure)
	require.Equal(t, failure, job.err)
	w.finish(nil)
	require.NoError(t, w.Stop(), "nonautomatic Stop keeps its existing actor/session contract")
}

// Successful retries cannot erase a physically returned unknown outcome.
// Repeated failures reuse one error object, never a retained graph per Task.
func TestDueWorkerCleanupResultRecoveryHistory(t *testing.T) {
	w := cleanupResultWorker(t, true)
	failure := errors.Join(ErrOutcomeUnknown, errors.New(strings.Repeat("dependency", 1024)))
	for i := 0; i < 10000; i++ {
		job := cleanupResultJob(w, 0)
		w.backend.finishCleanupJob(job, failure)
	}
	job := cleanupResultJob(w, 0)
	w.backend.finishCleanupJob(job, nil)
	w.publishStatus("listing", nil)
	require.Equal(t, uint64(10000), w.Status().CleanupFailures)
	require.NoError(t, w.ctx.Err(), "failed jobs must not cancel unrelated automatic work")
	require.NotEmpty(t, w.Status().LastError, "healthy discovery must retain historical diagnostics")
	w.finish(nil)
	terminal := w.Stop()
	require.ErrorIs(t, terminal, ErrOutcomeUnknown)
	require.LessOrEqual(t, len(terminal.Error()), 512, "terminal history must not grow per completed job")
}

// Cancellation is a request, not physical completion. The real completion
// hook must return before job removal, worker Done or the terminal error.
func TestDueWorkerCleanupResultActualReturn(t *testing.T) {
	w := cleanupResultWorker(t, true)
	job := cleanupResultJob(w, 0)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	job.stopCaller = func() {
		close(entered)
		<-release
	}
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		w.backend.finishCleanupJob(job, errors.Join(context.Canceled, ErrOutcomeUnknown))
	}()
	workerTestReceive(t, entered)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		w.finish(nil)
	}()
	defer func() {
		unblock()
		<-returned
		<-closed
	}()
	workerTestReceive(t, job.ctx.Done())
	for _, done := range []<-chan struct{}{job.done, w.workDone, w.Done()} {
		select {
		case <-done:
			t.Fatal("completion published before the original callback returned")
		default:
		}
	}
	unblock()
	workerTestReceive(t, returned)
	workerTestReceive(t, closed)
	require.ErrorIs(t, w.Stop(), ErrOutcomeUnknown)
}

func TestDueWorkerCleanupResultConcurrent(t *testing.T) {
	w := cleanupResultWorker(t, true)
	var jobs [32]*cleanupLocalJob
	for i := range jobs {
		jobs[i] = cleanupResultJob(w, i)
	}
	var joined sync.WaitGroup
	for _, job := range jobs {
		joined.Add(1)
		go func() {
			defer joined.Done()
			w.backend.finishCleanupJob(job, errors.Join(context.Canceled, ErrOutcomeUnknown))
		}()
	}
	w.finish(nil)
	joined.Wait()
	require.ErrorIs(t, w.Stop(), ErrOutcomeUnknown)
	for _, job := range jobs {
		workerTestReceive(t, job.done)
	}
}

// Filtering actor cancellation must not discard a dependency joined with it.
func TestDueWorkerCleanupResultMixedActorCancellation(t *testing.T) {
	w := cleanupResultWorker(t, true)
	w.finish(errors.Join(context.Canceled, errors.New("actor dependency refused")))
	require.Error(t, w.Stop())
	require.Contains(t, w.Stop().Error(), "actor dependency refused")
}

// A dependency may classify itself as canceled while still being a distinct
// leaf. Neither the ledger nor shutdown may discard that dependency failure.
type cleanupResultCanceledDependency struct{}

func (cleanupResultCanceledDependency) Error() string { return "dependent transport canceled" }
func (cleanupResultCanceledDependency) Is(target error) bool {
	return target == context.Canceled
}

func TestDueWorkerCleanupResultCauseFlags(t *testing.T) {
	causes := []error{
		context.Canceled, context.DeadlineExceeded, ErrOutcomeUnknown,
		ErrInvalidConfiguration, ErrIdentityMismatch, ErrConflict, ErrGuardExpired,
		ErrInvalidMutation, ErrCorruptReceipt, ErrSnapshotReadCapacity, ErrInvalidRecord,
	}
	for _, cause := range causes {
		t.Run(cause.Error(), func(t *testing.T) {
			// The independent dependency makes even the canceled case a real
			// failure; matching one sentinel cannot authorize ignoring it.
			failure := cleanupResultFailure(errors.Join(cause, errors.New("dependency")))
			require.ErrorIs(t, &failure, cause)
			require.False(t, cleanupOnlyCanceled(&failure))
			for _, other := range causes {
				if other != cause {
					require.NotErrorIs(t, &failure, other)
				}
			}
		})
	}
	alias := cleanupResultFailure(cleanupResultCanceledDependency{})
	require.Equal(t, uint64(1), alias.failures)
	require.ErrorIs(t, &alias, context.Canceled)
	require.False(t, cleanupOnlyCanceled(&alias))
}

func TestDueWorkerCleanupResultBoundedLedger(t *testing.T) {
	w := cleanupResultWorker(t, true)
	first := errors.New(strings.Repeat("first dependency", 1024))
	w.backend.finishCleanupJob(cleanupResultJob(w, 0), first)
	w.backend.archiveLife.mu.Lock()
	immutable := w.cleanupResultErrorLocked()
	w.cleanupResults.failures = ^uint64(0) - 1
	w.backend.archiveLife.mu.Unlock()
	w.backend.finishCleanupJob(cleanupResultJob(w, 0), ErrOutcomeUnknown)
	w.backend.finishCleanupJob(cleanupResultJob(w, 0), ErrIdentityMismatch)
	w.backend.finishCleanupJob(cleanupResultJob(w, 0), nil)
	w.publishStatus("listing", nil)
	require.Equal(t, ^uint64(0), w.Status().CleanupFailures, "diagnostic count saturates rather than wrapping clean")
	require.NotErrorIs(t, immutable, ErrOutcomeUnknown, "terminal copies never borrow mutable ledger flags")
	require.Contains(t, immutable.Error(), "failures=1:")

	w.finish(nil)
	terminal := w.Stop()
	require.ErrorIs(t, terminal, ErrOutcomeUnknown)
	require.ErrorIs(t, terminal, ErrIdentityMismatch)
	require.Contains(t, terminal.Error(), strings.Repeat("first dependency", 16))
	require.NotContains(t, terminal.Error(), strings.Repeat("first dependency", 17))
	// Two fixed snapshots, the retained status rendering and conservative
	// three-error join backing storage fit the approved 1KiB charge. Error()
	// call temporaries remain within the already reserved transient workspace.
	retained := 2*unsafe.Sizeof(cleanupJobResult{}) + uintptr(len(terminal.Error())) + 3*unsafe.Sizeof(error(nil)) + 32
	require.LessOrEqual(t, retained, uintptr(1024))
	require.LessOrEqual(t, retained, uintptr(cleanupResultReservedBytes))
	require.Less(t, 3670016+cleanupResultReservedBytes, 4<<20)
}
