package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

func (b *Backend) PrepareTaskWorkspaceSyncSource(ctx context.Context, r *ReceivedTaskWorkspaceArchive, plan TaskWorkspaceSyncPlan) (*PreparedTaskWorkspaceSyncSource, error) {
	if b == nil || b.archiveLife == nil || b.archiveStore == nil || ctx == nil || r == nil || r.self != r || r.origin != b || r.owner == nil || r.owner.backend != b || r.witness == nil || r.witness.self != r.witness || r.owner.spool == nil || r.witness.origin != r.owner.spool || plan.validate() != nil {
		return nil, ErrInvalidRecord
	}
	o := r.owner
	c := o.claim
	if !b.validTaskClaim(c) {
		return nil, ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	c.mu.Lock()
	valid := !b.archiveLife.closed && c.archiveDelivery == o && b.validPreparedTaskWorkspaceArchive(o.prepared)
	if valid {
		d := o.prepared.draft
		w := r.witness.diagnostics
		value, err := encodeTaskFinalSyncAttempt(d.finalSync.record)
		valid = err == nil && value == d.finalSync.value && plan.MatchesAttempt(d.finalSync.record) == nil && d.claims.Context.PlanDigest == plan.digest && w.Context == d.claims.Context && w.TicketDigest == d.ticket.Digest() && w.Before.Equal(d.claims.NotBefore) && w.After.Equal(d.claims.NotAfter) && w.SessionAfter.Equal(d.claims.SessionNotAfter)
	}
	c.mu.Unlock()
	if !valid {
		b.archiveLife.mu.Unlock()
		return nil, ErrIdentityMismatch
	}
	spool := o.spool
	store := spool.store
	store.mu.Lock()
	select {
	case <-o.done:
	default:
		store.mu.Unlock()
		b.archiveLife.mu.Unlock()
		return nil, ErrConflict
	}
	if store != b.archiveStore || spool.self != spool || store.closed || spool.closing || spool.active || spool.witness != r.witness || spool.consumer != nil || ctx.Err() != nil || c.parentCtx.Err() != nil {
		store.mu.Unlock()
		b.archiveLife.mu.Unlock()
		return nil, errTaskArchive
	}
	owned, cancel := context.WithCancel(ctx)
	s := &PreparedTaskWorkspaceSyncSource{archive: r, plan: plan, diagnostics: r.witness.diagnostics, ctx: owned, cancel: cancel, done: make(chan struct{})}
	s.self = s
	spool.consumer = s // registration precedes all file IO; never cleared
	store.mu.Unlock()
	b.archiveLife.mu.Unlock()
	go s.own()
	if err := s.begin(ctx); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	var err error
	if store.recovery != nil {
		err = b.refuseWorkspaceSyncLocalFinalization(s.ctx, c)
	}
	if err == nil {
		err = s.verify(ctx)
	}
	s.end()
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	s.cursor = newWorkspaceSyncArchiveCursor(s.diagnostics.Bytes, s.diagnostics.Entries, spool.index, s.plan, func(ctx context.Context, data []byte, offset int64) (int, error) {
		return (taskWorkspaceSyncReadAt{s, ctx}).ReadAt(data, offset)
	})
	return s, nil
}

func (s *PreparedTaskWorkspaceSyncSource) begin(ctx context.Context) error {
	if s == nil || s.self != s || ctx == nil {
		return errTaskArchive
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed || s.busy || s.ctx.Err() != nil || ctx.Err() != nil {
		return errors.Join(errTaskArchive, s.ctx.Err(), ctx.Err())
	}
	s.busy = true
	s.ops.Add(1)
	return nil
}

// The original registered source can durably attribute a local target even
// when its archive contains no regular body. This grants no metadata effect.
func (s *PreparedTaskWorkspaceSyncSource) bindLocalFinalizationTarget(ctx context.Context, target *taskWorkspaceSyncLocalTarget) error {
	if err := s.begin(ctx); err != nil {
		return err
	}
	defer s.end()
	if s.broken || target == nil || target.self != target || target.plan.validate() != nil || target.plan.digest != s.plan.digest || target.plan.workspace != s.plan.workspace || s.archive.owner.spool.store.recovery == nil {
		return ErrInvalidRecord
	}
	if err := target.begin(ctx); err != nil {
		return err
	}
	defer target.ops.Done()
	if err := errors.Join(s.guard(ctx), target.check(ctx)); err != nil {
		return err
	}
	return s.archive.owner.spool.sealOriginalFinalizationBinding(ctx, s, target)
}

func (s *PreparedTaskWorkspaceSyncSource) end() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
	s.ops.Done()
}

func (s *PreparedTaskWorkspaceSyncSource) Close() error {
	if s == nil || s.self != s {
		return errTaskArchive
	}
	s.cancel()
	<-s.done
	return s.archive.owner.spool.close()
}

func (s *PreparedTaskWorkspaceSyncSource) verify(ctx context.Context) error {
	if err := s.guard(ctx); err != nil {
		return err
	}
	r := s.archive.owner.spool
	if err := taskArchiveCheck(r.store.root, r.name, r.file, r.identity); err != nil {
		return err
	}
	st, err := r.file.Stat()
	if err != nil || st.Size() < 1536 || uint64(st.Size()) != s.diagnostics.Bytes || s.diagnostics.Bytes > r.store.limits.JobBytes || s.diagnostics.Entries == 0 || s.diagnostics.Entries > r.store.limits.Entries {
		return errors.Join(errTaskArchive, err)
	}
	h := sha256.New()
	var buffer [32768]byte
	n, err := io.CopyBuffer(h, io.NewSectionReader(taskWorkspaceSyncReadAt{s, ctx}, 0, st.Size()), buffer[:])
	if err != nil || n != st.Size() || hex.EncodeToString(h.Sum(nil)) != s.diagnostics.SHA256 {
		return errors.Join(errTaskArchive, err)
	}
	if err := taskArchiveCheck(r.store.root, r.name, r.file, r.identity); err != nil {
		return err
	}
	return s.guard(ctx)
}

type taskWorkspaceSyncReadAt struct {
	source *PreparedTaskWorkspaceSyncSource
	ctx    context.Context
}

func (r taskWorkspaceSyncReadAt) ReadAt(data []byte, offset int64) (int, error) {
	s := r.source
	if err := s.local(r.ctx); err != nil {
		return 0, err
	}
	if offset < 0 || uint64(offset) > s.diagnostics.Bytes {
		return 0, errTaskArchive
	}
	if uint64(len(data)) > s.diagnostics.Bytes-uint64(offset) {
		return 0, io.ErrUnexpectedEOF
	}
	total := 0
	for len(data) > 0 {
		if e := s.local(r.ctx); e != nil {
			return total, e
		}
		chunk := data
		if len(chunk) > 32768 {
			chunk = chunk[:32768]
		}
		s.mu.Lock()
		completion := s.readComplete
		s.mu.Unlock()
		n, err := s.archive.owner.spool.file.ReadAt(chunk, offset+int64(total))
		if completion != nil {
			completion()
		}
		total += n
		if e := s.local(r.ctx); e != nil {
			return total, errors.Join(err, e)
		}
		if err != nil {
			return total, err
		}
		data = data[n:]
	}
	return total, nil
}
