package etcd

import (
	"context"
	"errors"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/goairix/sandbox/internal/runtime/launcher"
)

func (s *taskArchiveStore) registerRecovery(ctx context.Context, slot uint32) (*taskWorkspaceSyncRecoverySource, error) {
	if s == nil || s.self != s || s.recovery == nil || ctx == nil || uint64(slot) >= s.limits.Jobs {
		return nil, ErrInvalidRecord
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.recovery.slots[slot].diagnostic
	if s.closed || ctx.Err() != nil || !d.Occupied || d.Quarantined || d.State != 3 || s.recovery.slots[slot].owner != nil {
		return nil, ErrConflict
	}
	for r := range s.spools {
		if r.recovery != nil && r.recovery.slot == slot {
			return nil, ErrConflict
		}
	}
	owned, cancel := context.WithCancel(ctx)
	r := &taskWorkspaceSyncRecoverySource{store: s, slot: slot, ctx: owned, cancel: cancel, done: make(chan struct{})}
	r.self = r
	s.recovery.slots[slot].owner = r
	go r.own()
	return r, nil
}

func (s *taskWorkspaceSyncRecoverySource) own() {
	<-s.ctx.Done()
	s.mu.Lock()
	s.sealed = true
	s.mu.Unlock()
	s.ops.Wait()
	if s.target != nil {
		s.err = errors.Join(s.err, s.target.Close())
	}
	if s.file != nil {
		s.err = errors.Join(s.err, s.file.Close())
	}
	s.index = nil
	s.store.mu.Lock()
	// The slot stays charged. Only its live owner is cleared after real joins.
	s.store.recovery.slots[s.slot].owner = nil
	close(s.done)
	s.store.mu.Unlock()
}

func (s *taskWorkspaceSyncRecoverySource) begin(ctx context.Context) error {
	if s == nil || s.self != s || ctx == nil {
		return ErrInvalidRecord
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed || s.busy || s.ctx.Err() != nil || ctx.Err() != nil {
		return ErrGuardExpired
	}
	s.busy = true
	s.ops.Add(1)
	return nil
}

func (s *taskWorkspaceSyncRecoverySource) end() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
	s.ops.Done()
}

func (s *taskWorkspaceSyncRecoverySource) Close() error {
	if s == nil || s.self != s {
		return ErrInvalidRecord
	}
	s.cancel()
	<-s.done
	return s.err
}

func (s *taskWorkspaceSyncRecoverySource) load(ctx context.Context) error {
	r := s.store.recovery
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return s.ctx.Err()
	case <-r.gate:
	}
	defer func() { r.gate <- struct{}{} }()
	if err := errors.Join(ctx.Err(), s.ctx.Err(), taskArchiveRecoveryCheckRoot(s.store.root, r.directory, r.root)); err != nil {
		return err
	}
	f, _, err := taskArchiveRecoveryOpen(s.store.root, recoverySlotName(s.slot)+".json")
	if err != nil {
		return err
	}
	buf := s.store.recoveryBuffers()
	in, err := recoveryRead(f, buf.input[:])
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	m, err := decodeTaskArchiveRecovery(in, buf.output[:])
	if err != nil || m.Slot != s.slot || m.State != "bound" || m.StoreID != r.storeID || m.Root != r.root || m.LimitsDigest != r.limitsDigest {
		return errors.Join(ErrIdentityMismatch, err)
	}
	s.file, s.identity, err = taskArchiveRecoveryOpen(s.store.root, recoverySlotName(s.slot)+".tar")
	if err != nil {
		return err
	}
	s.index = make([]byte, int(s.store.indexBytes))
	if err := s.store.verifyColdTar(ctx, s.file, recoverySlotName(s.slot)+".tar", s.identity, m, s.index); err != nil {
		return err
	}
	s.manifest = m
	s.plan, err = m.Binding.Plan.plan()
	return err
}

func (b *Backend) prepareTaskWorkspaceSyncRecovery(ctx context.Context, c *TaskClaim, slot uint32, destination *controltransport.Destination, worker *launcher.KernelBoundary) (_ *taskWorkspaceSyncRecoverySource, resultErr error) {
	if b == nil || ctx == nil || !b.validTaskClaim(c) || b.archiveStore == nil || b.archiveStore.recovery == nil || b.workspaceSyncBudget == nil || b.workspaceSyncBudget.profile != 2 || destination == nil || worker == nil {
		return nil, ErrInvalidRecord
	}
	if err := taskArchiveRecoverySupported(); err != nil {
		return nil, err
	}
	s, err := b.archiveStore.registerRecovery(ctx, slot)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, s.Close())
		}
	}()
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	err = s.load(s.ctx)
	if err == nil {
		var q *PreparedTaskQuiescence
		q, err = b.PrepareAfterDualDrain(s.ctx, c, destination)
		if err == nil {
			s.authority, err = b.pinTaskArchiveRecovery(s.ctx, c, s.manifest, q)
		}
		if err == nil {
			_, err = s.authority.queryProducer(s.ctx, destination)
		}
	}
	s.end()
	if err != nil {
		return nil, err
	}
	go s.monitor()
	return s, nil
}

func (s *taskWorkspaceSyncRecoverySource) monitor() {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-timer.C:
			if err := s.begin(s.ctx); err != nil {
				if s.ctx.Err() != nil {
					return
				}
				timer.Reset(250 * time.Millisecond)
				continue
			}
			err := s.authority.guard(s.ctx)
			s.end()
			if err != nil {
				s.cancel()
				return
			}
			timer.Reset(250 * time.Millisecond)
		}
	}
}
