package etcd

import (
	"context"
	"errors"
)

// Requires archiveLife.mu. Cancellation only seals work; the returned original
// remains registered until its workDone and final done phases actually finish.
func (b *Backend) stopDueWorker(s *WorkerSession, p *WorkerPartitionClaim, share *WorkerBudgetShare) *DueWorker {
	if s == nil || s.dueWorker == nil {
		return nil
	}
	w := s.dueWorker
	if w.backend != b || w.self != w || w.session != s || p != nil && p.session != s || share != nil && share.session != s {
		return nil
	}
	w.cancel()
	if s.operation != nil {
		// Startup checkpoint/resolution calls can inherit s.ctx directly;
		// cancel the registered native owner as well as the actor context.
		s.operation.cancel()
	}
	return w
}

// finish is called by the sole actor (or its synchronous startup failure).
// workDone is published before entering session cleanup, so an external release
// can join work without waiting on final done, which itself needs that cleanup.
func (w *DueWorker) finish(workErr error) {
	w.cancel()
	if errors.Is(workErr, context.Canceled) && !errors.Is(workErr, ErrOutcomeUnknown) {
		workErr = nil
	}
	w.publishStatus("stopping", workErr)
	w.backend.archiveLife.mu.Lock()
	close(w.workDone)
	w.backend.archiveLife.mu.Unlock()
	cleanupErr := w.backend.releaseWorkerSessionCore(context.Background(), w.session, w)

	b := w.backend
	b.archiveLife.mu.Lock()
	w.err = errors.Join(workErr, cleanupErr)
	w.status.Running, w.status.Synchronized = false, false
	w.status.Phase = "stopped"
	if errors.Is(w.err, ErrOutcomeUnknown) {
		w.status.Phase = "unknown"
	} else if w.err != nil {
		w.status.Phase = "invalid"
	}
	w.status.LastError = dueWorkerError(w.err)
	for i := range w.status.Partitions {
		w.status.Partitions[i].Synchronized = false
		w.status.Partitions[i].Phase = w.status.Phase
	}
	// Removal and final done are one admission-lock publication. A Close
	// entering after session cleanup must still discover and join this actor.
	if b.workerControl.sessions[w.session.slot] == w.session {
		b.workerControl.sessions[w.session.slot] = nil
	}
	close(w.done)
	b.archiveLife.mu.Unlock()
}

// A session cleanup can wait for child r.done before final worker done. Publish
// the child's physical completion first, then join the worker outside the lock;
// reversing these phases creates a child/session/actor cleanup cycle.
func (b *Backend) finishDueWorkerChildRelease(r *workerRelease, err error, w *DueWorker) {
	b.finishWorkerChildRelease(r, err)
	if w != nil {
		<-w.done
	}
}
