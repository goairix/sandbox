package etcd

import (
	"context"
	"errors"
	"time"

	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

// ReceiveTaskWorkspaceArchive delivers the exact original committed command
// once and receives its bounded private archive under current native authority.
func (b *Backend) ReceiveTaskWorkspaceArchive(ctx context.Context, prepared *PreparedTaskWorkspaceArchive) (result *ReceivedTaskWorkspaceArchive, resultErr error) {
	if b == nil || b.archiveStore == nil {
		return nil, ErrInvalidConfiguration
	}
	if ctx == nil || prepared == nil || !b.validTaskClaim(prepared.claim) {
		return nil, ErrInvalidRecord
	}
	c := prepared.claim
	// Backend admission and the original seal are installed together. Neither
	// lock spans native checks, transport, receiving or physical owner joins.
	b.archiveLife.mu.Lock()
	if b.archiveLife.closed {
		b.archiveLife.mu.Unlock()
		return nil, errTaskArchive
	}
	c.mu.Lock()
	if !b.validPreparedTaskWorkspaceArchive(prepared) {
		c.mu.Unlock()
		b.archiveLife.mu.Unlock()
		return nil, ErrInvalidRecord
	}
	d := prepared.draft
	if c.archiveDelivery != nil || b.archiveStore.recovery != nil && c.archiveRecoveryAttempt || !d.writeAttempted || d.revision <= 0 || d.issuerRevision <= 0 || d.ticket.Digest() == "" || d.finalSync.revision <= 0 {
		c.mu.Unlock()
		b.archiveLife.mu.Unlock()
		return nil, ErrConflict
	}
	if err := taskArchiveLive(ctx, c, d); err != nil {
		c.mu.Unlock()
		b.archiveLife.mu.Unlock()
		return nil, err
	}
	cmps := b.taskArchiveComparisons(c, d)
	deadline := c.deadline
	c.mu.Unlock()
	prefix, err := b.taskOperationsPrefix(c.reference.Task)
	if err != nil {
		b.archiveLife.mu.Unlock()
		return nil, err
	}
	owned, cancel := context.WithDeadline(ctx, d.claims.SessionNotAfter)
	var r *taskArchiveSpool
	if b.archiveStore.recovery != nil {
		seed, seedErr := originalArchiveRecoverySeed(c, d)
		if seedErr != nil {
			cancel()
			b.archiveLife.mu.Unlock()
			return nil, seedErr
		}
		c.mu.Lock()
		if !c.sealArchiveRecoveryAttempt() {
			c.mu.Unlock()
			cancel()
			b.archiveLife.mu.Unlock()
			return nil, ErrConflict
		}
		c.mu.Unlock()
		// The store seals admission and registers active/ownerDone under its
		// own lock. Close can cancel and physically join that initializer even
		// before a delivery owner is published. Never hold life.mu across the
		// metadata gate or pending durable IO.
		b.archiveLife.mu.Unlock()
		r, err = b.archiveStore.reserveRecovery(owned, seed)
		if err == nil {
			var bounded context.Context
			var finish func()
			bounded, finish, err = r.beginRecoveryMetadata(owned)
			if err == nil {
				err = b.refuseWorkspaceSyncLocalFinalization(bounded, c)
				finish()
			}
			if err != nil {
				err = errors.Join(err, r.close())
			}
		}
		b.archiveLife.mu.Lock()
		c.mu.Lock()
		c.finishArchiveRecoveryInitialization(r, err)
		c.mu.Unlock()
	} else {
		r, err = b.archiveStore.reserve(owned)
	}
	if err != nil {
		cancel()
		b.archiveLife.mu.Unlock()
		return nil, err
	}
	if b.archiveLife.closed {
		b.archiveLife.mu.Unlock()
		cancel()
		return nil, errors.Join(errTaskArchive, r.close())
	}
	o := &taskArchiveDelivery{backend: b, claim: c, prepared: prepared, spool: r, ctx: owned, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), sessionAfter: d.claims.SessionNotAfter, comparisons: cmps, prefix: prefix}
	o.deadline.Store(&deadline)
	c.mu.Lock()
	if !b.validPreparedTaskWorkspaceArchive(prepared) || c.archiveDelivery != nil || taskArchiveLive(ctx, c, d) != nil {
		c.mu.Unlock()
		b.archiveLife.mu.Unlock()
		cancel()
		return nil, errors.Join(ErrConflict, r.close())
	}
	latest := c.deadline
	o.deadline.Store(&latest)
	c.archiveDelivery = o // one attempt, never cleared or reconstructed
	if b.archiveLife.owners == nil {
		b.archiveLife.owners = make(map[*taskArchiveDelivery]struct{})
	}
	b.archiveLife.owners[o] = struct{}{}
	c.mu.Unlock()
	b.archiveLife.mu.Unlock()
	monitorCtx, stopMonitor := context.WithCancel(owned)
	monitorStop := make(chan struct{})
	monitorDone := make(chan struct{})
	expiryDone := make(chan struct{})
	go func() { defer close(monitorDone); o.monitor(owned, monitorStop) }()
	go func() { defer close(expiryDone); o.observeExpiry(monitorCtx) }()
	defer func() {
		close(monitorStop)
		stopMonitor()
		<-monitorDone
		<-expiryDone
		if failure := o.failureError(); failure != nil {
			result, resultErr = nil, errors.Join(resultErr, failure)
		}
		if result != nil {
			// Graceful monitor stop does not cancel an in-flight native check.
			// Join it, retain real failures, then independently check authority.
			if err := o.currentGuard(owned); err != nil {
				result, resultErr = nil, errors.Join(resultErr, err)
			}
		}
		if result == nil {
			cancel()
			resultErr = errors.Join(resultErr, r.close())
		}
		b.archiveLife.mu.Lock()
		delete(b.archiveLife.owners, o)
		b.archiveLife.mu.Unlock()
		close(o.done)
	}()
	e := taskArchiveExpected{verifier: b.taskWorkspaceArchiveVerifier, destination: d.destination, envelope: t.TaskWorkspaceArchiveEnvelope{Version: 1, Purpose: "task_workspace_archive", Context: d.claims.Context, TicketDigest: d.ticket.Digest(), Ticket: d.ticket.Wire(), IssuerCertificate: append([]byte(nil), d.certificate...), OriginalQuiescenceReceipt: d.prepared.receipt.Wire()}, context: d.claims.Context, digest: d.ticket.Digest(), before: d.claims.NotBefore, after: d.claims.NotAfter, sessionAfter: d.claims.SessionNotAfter, now: b.observePublicationClock}
	e.beforeOpen = o.startGuard
	e.beforeSend = o.startGuard
	e.beforeWitness = o.currentGuard
	w, err := r.receive(owned, e)
	if err != nil {
		return nil, errors.Join(err, r.close())
	}
	// Session.Close has physically returned; recheck current native authority
	// before sealing the externally returned original result.
	if err = o.currentGuard(owned); err != nil {
		return nil, errors.Join(err, r.close())
	}
	if r.recovery != nil {
		if err = r.sealOriginalReceived(owned, w); err != nil {
			return nil, errors.Join(err, r.close())
		}
		if err = o.currentGuard(owned); err != nil {
			return nil, errors.Join(err, r.close())
		}
	}
	b.archiveLife.mu.Lock()
	closed := b.archiveLife.closed
	b.archiveLife.mu.Unlock()
	if closed || owned.Err() != nil {
		return nil, errors.Join(errTaskArchive, owned.Err(), r.close())
	}
	result = &ReceivedTaskWorkspaceArchive{origin: b, owner: o, witness: w}
	result.self = result
	return result, nil
}

func (o *taskArchiveDelivery) observeExpiry(ctx context.Context) {
	for {
		remaining := time.Until(*o.deadline.Load())
		if remaining <= 0 {
			o.fail(ErrGuardExpired)
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-o.claim.parentCtx.Done():
			timer.Stop()
			o.fail(o.claim.parentCtx.Err())
			return
		case <-o.wake:
			timer.Stop()
		case <-timer.C:
			// A renewal published concurrently with expiry wins only if its
			// existing old-deadline checks actually completed successfully.
			if !time.Now().Before(*o.deadline.Load()) {
				o.fail(ErrGuardExpired)
				return
			}
		}
	}
}

func (o *taskArchiveDelivery) monitor(ctx context.Context, stop <-chan struct{}) {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-timer.C:
			select {
			case <-stop:
				return
			default:
			}
			if err := o.currentGuard(ctx); err != nil {
				o.fail(err)
				return
			}
			// A delayed check must not consume queued ticks and then burst.
			timer.Reset(250 * time.Millisecond)
		}
	}
}

func (b *Backend) closeTaskArchiveBackend() error {
	if b == nil {
		return ErrInvalidRecord
	}
	if b.archiveLife == nil {
		if b.client != nil {
			return b.client.Close()
		}
		return nil
	}
	b.archiveLife.mu.Lock()
	if b.archiveLife.closeDone != nil {
		done := b.archiveLife.closeDone
		b.archiveLife.mu.Unlock()
		<-done
		b.archiveLife.mu.Lock()
		err := b.archiveLife.closeErr
		b.archiveLife.mu.Unlock()
		return err
	}
	b.archiveLife.closed = true
	b.archiveLife.closeDone = make(chan struct{})
	workers := b.stopWorkerControl()
	owners := make([]*taskArchiveDelivery, 0, len(b.archiveLife.owners))
	for o := range b.archiveLife.owners {
		owners = append(owners, o)
	}
	enrolled := make([]*runtimeEnrolledPublicationOwner, 0, len(b.archiveLife.enrolled))
	for o := range b.archiveLife.enrolled {
		enrolled = append(enrolled, o)
	}
	// A foreground finish may retire ctx/cancel after this lock is released.
	// Snapshot function values and original join channels under the same lock,
	// rather than retaining owner pointers whose executable fields may change.
	type releaseWait struct {
		cancel context.CancelFunc
		done   <-chan struct{}
	}
	releases := make([]releaseWait, 0, len(b.archiveLife.runtimeReleases))
	for _, owner := range b.archiveLife.runtimeReleases {
		if owner != nil {
			releases = append(releases, releaseWait{cancel: owner.cancel, done: owner.done})
		}
	}
	var snapshotCalls []*snapshotReadCall
	var snapshotMisses []*snapshotReadMiss
	if b.snapshotReads != nil {
		for call := range b.snapshotReads.calls {
			call.cancel()
			snapshotCalls = append(snapshotCalls, call)
		}
		for _, miss := range b.snapshotReads.misses {
			miss.cancel()
			snapshotMisses = append(snapshotMisses, miss)
		}
	}
	b.archiveLife.mu.Unlock()
	for _, o := range releases {
		o.cancel()
	}
	for _, o := range enrolled {
		o.cancel()
	}
	for _, o := range owners {
		o.cancel()
	}
	if b.archiveStore != nil {
		b.archiveStore.sealAndCancel()
	}
	for _, call := range snapshotCalls {
		<-call.done
	}
	for _, miss := range snapshotMisses {
		<-miss.done
	}
	if b.snapshotReads != nil {
		b.archiveLife.mu.Lock()
		b.snapshotReads.entries = nil
		b.snapshotReads.lru.Init()
		b.snapshotReads.used = 0
		b.archiveLife.mu.Unlock()
	}
	for _, o := range owners {
		<-o.done
	}
	for _, o := range enrolled {
		<-o.done
	}
	for _, o := range releases {
		<-o.done
	}
	var err error
	for _, s := range workers {
		err = errors.Join(err, b.releaseWorkerSession(context.Background(), s))
	}
	if b.archiveStore != nil {
		err = errors.Join(err, b.archiveStore.close())
	}
	if b.client != nil {
		err = errors.Join(err, b.client.Close())
	}
	b.archiveLife.mu.Lock()
	b.archiveLife.closeErr = err
	close(b.archiveLife.closeDone)
	b.archiveLife.mu.Unlock()
	return err
}
