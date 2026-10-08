package etcd

import (
	"context"
	"errors"
	"time"
)

func (s *PreparedTaskWorkspaceSyncSource) local(ctx context.Context) error {
	if ctx == nil || ctx.Err() != nil || s.ctx.Err() != nil || s.archive.owner.claim.parentCtx.Err() != nil {
		return ErrGuardExpired
	}
	o := s.archive.owner
	if !time.Now().Before(*o.deadline.Load()) {
		return ErrGuardExpired
	}
	c := o.claim
	c.mu.Lock()
	d := o.prepared.draft
	valid := c.archiveDelivery == o && o.backend.validPreparedTaskWorkspaceArchive(o.prepared) && !c.lost && !d.lost && !d.finalSync.lost
	c.mu.Unlock()
	if !valid {
		return ErrGuardExpired
	}
	return errors.Join(ctx.Err(), s.ctx.Err())
}

func (s *PreparedTaskWorkspaceSyncSource) guard(ctx context.Context) error {
	if err := s.local(ctx); err != nil {
		return err
	}
	o := s.archive.owner
	o.nativeMu.Lock()
	defer o.nativeMu.Unlock()
	if err := s.local(ctx); err != nil {
		return err
	}
	bounded, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	if err := o.nativeGuard(bounded); err != nil {
		s.cancel()
		return err
	}
	return s.local(ctx)
}

func (s *PreparedTaskWorkspaceSyncSource) own() {
	parentStop := context.AfterFunc(s.archive.owner.claim.parentCtx, s.cancel)
	defer parentStop()
	monitorDone := make(chan struct{})
	expiryDone := make(chan struct{})
	go func() { defer close(monitorDone); s.monitor() }()
	go func() { defer close(expiryDone); s.expiry() }()
	<-s.ctx.Done()
	s.mu.Lock()
	s.sealed = true
	s.mu.Unlock() // Add/Wait seal gate
	s.ops.Wait()
	<-monitorDone
	<-expiryDone
	close(s.done)
}

func (s *PreparedTaskWorkspaceSyncSource) monitor() {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
			if err := s.guard(s.ctx); err != nil {
				s.cancel()
				return
			}
			timer.Reset(250 * time.Millisecond)
		}
	}
}

func (s *PreparedTaskWorkspaceSyncSource) expiry() {
	o := s.archive.owner
	for {
		remaining := time.Until(*o.deadline.Load())
		if remaining <= 0 {
			s.cancel()
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-s.ctx.Done():
			timer.Stop()
			return
		case <-o.wake:
			timer.Stop()
		case <-timer.C:
			if !time.Now().Before(*o.deadline.Load()) {
				s.cancel()
				return
			}
		}
	}
}
