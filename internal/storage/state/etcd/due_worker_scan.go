package etcd

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

// A finite, serial original consumer. A turn performs one real bounded page
// refill or one real metadata admission/settlement; it never sleeps or creates
// authority. The resident actor owns renewal, cache accounting and waits.
type dueWorkerScan struct {
	backend    *Backend
	partition  *WorkerPartitionClaim
	ranges     *WorkerBudgetShare
	claims     *WorkerBudgetShare
	ttl        time.Duration
	checkpoint *dueCheckpoint
	cursor     *DueCursor
	page       []DueCandidateRecord
	index      int
	started    bool
	listed     bool
	failure    error
	// A fresh view retains the previous checkpoint CAS handle. frontierBlocked
	// separates enumeration after an early hole from the settled list prefix.
	fresh           bool
	frontierBlocked bool
	pending         StageReference
}

func newDueWorkerScan(b *Backend, p *WorkerPartitionClaim, ranges, claims *WorkerBudgetShare, ttl time.Duration) (*dueWorkerScan, error) {
	if !b.validWorkerBackend() || ttl <= 0 || ttl > 24*time.Hour {
		return nil, ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	valid := b.validWorkerPartition(p) && b.validWorkerBudget(ranges) && b.validWorkerBudget(claims) && ranges.session == p.session && claims.session == p.session && ranges.kind == WorkerBudgetRange && claims.kind == WorkerBudgetClaim
	var ctx context.Context
	if valid {
		ctx = p.session.ctx
	}
	b.archiveLife.mu.Unlock()
	if !valid {
		return nil, ErrInvalidRecord
	}
	saved, err := b.readDueCheckpoint(ctx, p)
	if err != nil {
		return nil, err
	}
	scan := &dueWorkerScan{backend: b, partition: p, ranges: ranges, claims: claims, ttl: ttl, checkpoint: saved}
	if saved != nil && saved.record.Pending != nil {
		if _, err = scan.resolvePending(ctx); err != nil {
			return nil, err
		}
	} else if saved != nil && saved.record.Phase == "watch" && (saved.record.WriterSessionID != p.session.reference.SessionID || saved.record.WriterPartitionRevision != p.fence.create) {
		// A replacement owner needs a current fixed view, rather than replaying
		// arbitrarily old unrelated history from a completed predecessor view.
		// Keep the original checkpoint handle: refill must still CAS every old
		// fence and preserve its birth before a new Watch bridge can publish.
		// An initially pending attempt takes the branch above even if resolving
		// it clears Pending; that arbitration must retain its original fixed R.
		scan.resetView()
	}
	return scan, nil
}

func (scan *dueWorkerScan) recordForPage(page DuePage) dueCheckpointRecord {
	b, p := scan.backend, scan.partition
	return dueCheckpointRecord{Version: 1, Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, IdentityDigest: workerIdentityDigest(b.identityValue), ProfileID: workerProfileID, ProfileDigest: workerIdentityDigest(b.workerControl.profile.value), Partition: p.partition, WriterSessionID: p.session.reference.SessionID, WriterPartitionRevision: p.fence.create, Revision: page.Revision, ReplayFrom: page.Revision + 1, WindowEnd: page.WindowEnd, Phase: "list"}
}

func (scan *dueWorkerScan) refill(ctx context.Context) error {
	var page DuePage
	var err error
	if !scan.started && scan.checkpoint != nil && !scan.fresh {
		page, err = scan.backend.resumeDuePage(ctx, scan.partition, scan.ranges, scan.checkpoint)
	} else {
		page, err = scan.backend.ReadDuePage(ctx, scan.partition, scan.ranges, scan.cursor)
	}
	if err != nil {
		return err
	}
	if scan.fresh && scan.checkpoint != nil && page.WindowEnd.Before(scan.checkpoint.record.WindowEnd) {
		// A regressed clock cannot replace the older, wider discovery window.
		// No cursor or durable record from this rejected view is published.
		return ErrOperationAdmissionClosed
	}
	if scan.checkpoint == nil || scan.fresh {
		next := scan.recordForPage(page)
		if len(page.Candidates) == 0 && page.Next == nil {
			next.Phase = "watch"
		}
		if err = scan.save(ctx, next); err != nil {
			return err
		}
		scan.fresh = false
	}
	scan.page, scan.index, scan.cursor, scan.started = page.Candidates, 0, page.Next, true
	if len(page.Candidates) == 0 && page.Next == nil {
		scan.listed = true
	}
	return nil
}

func (scan *dueWorkerScan) step(ctx context.Context) (resultErr error) {
	if scan == nil || ctx == nil {
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if scan.failure != nil {
		return scan.failure
	}
	defer func() {
		if resultErr != nil && !errors.Is(resultErr, ErrConflict) && !errors.Is(resultErr, ErrOperationAdmissionClosed) && !errors.Is(resultErr, rpctypes.ErrCompacted) {
			scan.failure = resultErr
		}
	}()
	if scan.listed {
		return nil
	}
	if scan.index >= len(scan.page) {
		if scan.started && scan.cursor == nil {
			scan.listed = true
			scan.page = nil
			return nil
		}
		return scan.refill(ctx)
	}
	return scan.admitCandidate(ctx, scan.page[scan.index], true)
}

// Both fixed-R streaming and proved Watch scheduling use this exact admission
// and pre-Grant pending protocol. Only a list candidate may settle its frontier.
func (scan *dueWorkerScan) admitCandidate(ctx context.Context, candidate DueCandidateRecord, listCandidate bool) error {
	pending := &dueWorkerAdmission{origin: scan.backend, partition: scan.partition, claims: scan.claims, checkpoint: scan.checkpoint}
	result, err := scan.backend.admitExpiredTask(ctx, scan.partition, scan.claims, candidate, uuid.NewString(), scan.ttl, pending)
	scan.checkpoint = pending.checkpoint
	scan.pending = result.Reference
	if result.GuardCleanupError != nil {
		return errors.Join(ErrOutcomeUnknown, result.GuardCleanupError, err)
	}
	if errors.Is(err, ErrOutcomeUnknown) {
		return err
	}
	if err == nil && result.Outcome == OutcomeCommitted {
		return scan.settleCandidate(ctx, candidate, listCandidate)
	}
	if scan.checkpoint.record.Pending != nil {
		outcome, resolveErr := scan.resolvePending(ctx)
		if resolveErr != nil {
			// Even cancellation cannot reinterpret an unresolved persisted
			// request as a known abort or authorize another Stage attempt.
			return errors.Join(ErrOutcomeUnknown, resolveErr)
		}
		if outcome == OutcomeCommitted {
			return scan.settleCandidate(ctx, candidate, listCandidate)
		}
		// Aborted settles this exact attempt, not the candidate. Retain the
		// candidate for a later separately charged, revalidated attempt.
		if err == nil {
			// CommitStage may return OutcomeAborted,nil for an existing exact
			// receipt. Nil here would falsely mean settled to a cache consumer.
			return ErrConflict
		}
		return err
	}
	if pending.rejected {
		return scan.settleCandidate(ctx, candidate, listCandidate)
	}
	if pending.early {
		scan.pending = StageReference{}
		return ErrOperationAdmissionClosed
	}
	if err == nil {
		return ErrOutcomeUnknown
	}
	return err
}

// resetView discards only local continuation. The next fresh Range must CAS
// the existing record before admission; unresolved requests are never reset.
func (scan *dueWorkerScan) resetView() {
	if scan.checkpoint != nil && scan.checkpoint.record.Pending != nil {
		return
	}
	scan.cursor, scan.page, scan.index = nil, nil, 0
	scan.started, scan.listed, scan.fresh = false, false, true
	scan.frontierBlocked, scan.failure = false, nil
}
