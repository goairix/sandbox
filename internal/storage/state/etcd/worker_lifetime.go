package etcd

import (
	"context"
	"errors"
	"time"
)

// The existing archiveLife.mu is the only admission/state lock. No worker
// lock spans a native call, a callback, cancellation join or client shutdown.
type workerBackend struct {
	origin   *Backend
	options  WorkerControlOptions
	profile  taskFence
	sessions [32]*WorkerSession
	relists  [4]*workerRelistOwner
}

type workerCall struct {
	session    *WorkerSession
	ctx        context.Context
	cancel     context.CancelFunc
	done       chan struct{}
	started    bool
	stopParent func() bool
	parentDone chan struct{}
}

type workerRelease struct {
	done chan struct{}
	err  error
}

func (b *Backend) validWorkerBackend() bool {
	return b != nil && b.client != nil && b.archiveLife != nil && b.workerControl != nil && b.workerControl.origin == b
}

// Requires archiveLife.mu. Pointer membership is checked even when the
// diagnostic fields happen to match a live original session.
func (b *Backend) validWorkerSession(s *WorkerSession) bool {
	return b.validWorkerBackend() && s != nil && s.origin == b && s.self == s && s.slot >= 0 && s.slot < len(b.workerControl.sessions) && b.workerControl.sessions[s.slot] == s && s.ctx != nil
}

func (s *WorkerSession) live() error {
	if s.lost || s.released {
		return ErrGuardExpired
	}
	if err := s.ctx.Err(); err != nil {
		s.lost = true
		return err
	}
	if s.reference.LeaseID <= 0 || !time.Now().Before(s.deadline) {
		s.lost = true
		s.cancel()
		return ErrGuardExpired
	}
	return nil
}

func (b *Backend) beginWorkerCall(ctx context.Context, s *WorkerSession) (*workerCall, error) {
	if ctx == nil || !b.validWorkerBackend() {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	return b.beginWorkerCallLocked(ctx, s)
}

// Requires archiveLife.mu. Domain consumers reserve their physical owner,
// original token bucket and session call in this same admission section.
func (b *Backend) beginWorkerCallLocked(ctx context.Context, s *WorkerSession) (*workerCall, error) {
	if !b.validWorkerSession(s) {
		return nil, ErrInvalidRecord
	}
	if b.archiveLife.closed {
		return nil, context.Canceled
	}
	if s.release != nil {
		return nil, ErrGuardExpired
	}
	if err := s.live(); err != nil {
		return nil, err
	}
	if s.operation != nil {
		return nil, ErrConflict
	}
	owned, cancel := context.WithCancel(ctx)
	parentDone := make(chan struct{})
	stop := context.AfterFunc(s.ctx, func() { cancel(); close(parentDone) })
	call := &workerCall{session: s, ctx: owned, cancel: cancel, done: make(chan struct{}), stopParent: stop, parentDone: parentDone}
	s.operation = call
	return call, nil
}

func (call *workerCall) finish() {
	s := call.session
	b := s.origin
	if call.stopParent != nil && !call.stopParent() {
		<-call.parentDone
	}
	b.archiveLife.mu.Lock()
	call.cancel()
	if s.operation == call {
		s.operation = nil
	}
	close(call.done)
	b.archiveLife.mu.Unlock()
}

func (call *workerCall) postcheck() error {
	s := call.session
	b := s.origin
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if err := call.ctx.Err(); err != nil {
		s.lost = true
		s.cancel()
		return err
	}
	if b.archiveLife.closed {
		s.lost = true
		s.cancel()
		return context.Canceled
	}
	return s.live()
}

func (call *workerCall) lose() {
	s := call.session
	s.origin.archiveLife.mu.Lock()
	s.lost = true
	s.cancel()
	s.origin.archiveLife.mu.Unlock()
}

// Release remains an actual owner even after cancellation. Concurrent explicit
// Release and the existing backend Close share this one original cleanup.
func (b *Backend) releaseWorkerSession(ctx context.Context, s *WorkerSession) error {
	return b.releaseWorkerSessionCore(ctx, s, nil)
}

// self is nonnil only after that original actor has closed workDone. External
// releases join final done; the actor skips only its own final-done wait.
func (b *Backend) releaseWorkerSessionCore(_ context.Context, s *WorkerSession, self *DueWorker) error {
	b.archiveLife.mu.Lock()
	worker := b.stopDueWorker(s, nil, nil)
	if s.release != nil {
		r := s.release
		b.archiveLife.mu.Unlock()
		<-r.done
		if worker != nil && worker != self {
			<-worker.done
		}
		return r.err
	}
	r := &workerRelease{done: make(chan struct{})}
	s.release = r
	s.lost = true
	s.cancel()
	call := s.operation
	owners := b.stopWorkerRelists(s, nil, nil)
	dueOwners := b.stopWorkerDueOwners(s, nil, nil)
	watchOwner := b.stopDueWatchOwner(s, nil, nil)
	var children []*workerRelease
	for _, p := range s.partitions {
		if p != nil && p.release != nil {
			children = append(children, p.release)
		}
	}
	for _, share := range s.budgets {
		if share != nil && share.release != nil {
			children = append(children, share.release)
		}
	}
	b.archiveLife.mu.Unlock()
	if worker != nil {
		<-worker.workDone
	}
	if call != nil {
		<-call.done
	}
	for _, owner := range owners {
		<-owner.done
	}
	for _, owner := range dueOwners {
		<-owner.done
	}
	if watchOwner != nil {
		<-watchOwner.done
	}
	for _, child := range children {
		<-child.done
	}
	b.archiveLife.mu.Lock()
	cleaned, cleanupErr, lease := s.cleaned, s.cleanupErr, s.reference.LeaseID
	b.archiveLife.mu.Unlock()
	err := cleanupErr
	if !cleaned && lease > 0 {
		// Admission already checked the caller context. Its later cancellation
		// must not cancel the once-only original cleanup after physical joins;
		// workerRevoke installs its own requestTimeout bound and is joined here.
		err = errors.Join(err, b.workerRevoke(context.Background(), lease))
	}
	b.archiveLife.mu.Lock()
	s.cleaned, s.released, s.cleanupErr = true, true, err
	if s.dueCache != nil {
		s.dueCache.invalidateLocked("session released")
		s.dueCache = nil
	}
	// Controller sessions remain discoverable by a late Close until final
	// worker publication. Non-controller originals retain immediate removal.
	if worker == nil && b.workerControl.sessions[s.slot] == s {
		b.workerControl.sessions[s.slot] = nil
	}
	r.err = err
	close(r.done)
	b.archiveLife.mu.Unlock()
	if worker != nil && worker != self {
		<-worker.done
	}
	return err
}

// Called under the existing Close admission seal. The returned originals are
// joined and revoked before its existing client.Close is reached.
func (b *Backend) stopWorkerControl() []*WorkerSession {
	if b.workerControl == nil {
		return nil
	}
	var sessions []*WorkerSession
	for _, s := range b.workerControl.sessions {
		if s != nil {
			s.lost = true
			s.cancel()
			b.stopWorkerRelists(s, nil, nil)
			b.stopWorkerDueOwners(s, nil, nil)
			b.stopDueWatchOwner(s, nil, nil)
			b.stopDueWorker(s, nil, nil)
			if s.operation != nil {
				s.operation.cancel()
			}
			sessions = append(sessions, s)
		}
	}
	return sessions
}
