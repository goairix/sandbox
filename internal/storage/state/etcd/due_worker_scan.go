package etcd

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// A finite, serial original consumer. A turn performs one real bounded page
// refill or one real metadata admission/settlement; it never sleeps or creates
// authority. The later resident actor owns renewal, cache accounting and waits.
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
	if !scan.started && scan.checkpoint != nil {
		page, err = scan.backend.resumeDuePage(ctx, scan.partition, scan.ranges, scan.checkpoint)
	} else {
		page, err = scan.backend.ReadDuePage(ctx, scan.partition, scan.ranges, scan.cursor)
	}
	if err != nil {
		return err
	}
	if scan.checkpoint == nil {
		next := scan.recordForPage(page)
		if len(page.Candidates) == 0 && page.Next == nil {
			next.Phase = "watch"
		}
		if err = scan.save(ctx, next); err != nil {
			return err
		}
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
		if resultErr != nil && !errors.Is(resultErr, ErrConflict) && !errors.Is(resultErr, ErrOperationAdmissionClosed) {
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
	candidate := scan.page[scan.index]
	pending := &dueWorkerAdmission{origin: scan.backend, partition: scan.partition, claims: scan.claims, checkpoint: scan.checkpoint}
	result, err := scan.backend.admitExpiredTask(ctx, scan.partition, scan.claims, candidate, uuid.NewString(), scan.ttl, pending)
	scan.checkpoint = pending.checkpoint
	if result.GuardCleanupError != nil {
		return errors.Join(ErrOutcomeUnknown, result.GuardCleanupError, err)
	}
	if errors.Is(err, ErrOutcomeUnknown) {
		return err
	}
	if err == nil && result.Outcome == OutcomeCommitted {
		return scan.settle(ctx, candidate)
	}
	if scan.checkpoint.record.Pending != nil {
		outcome, resolveErr := scan.resolvePending(ctx)
		if resolveErr != nil {
			return resolveErr
		}
		if outcome == OutcomeCommitted {
			return scan.settle(ctx, candidate)
		}
		// Aborted is a positive exact-reference result, not a retry instruction.
		// Retain this candidate for a later separately charged turn.
		return err
	}
	if pending.rejected {
		return scan.settle(ctx, candidate)
	}
	if pending.early {
		return ErrOperationAdmissionClosed
	}
	if err == nil {
		return ErrOutcomeUnknown
	}
	return err
}
