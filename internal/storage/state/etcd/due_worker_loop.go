package etcd

import (
	"context"
	"errors"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

func (w *DueWorker) run() {
	w.finish(w.loop())
}

func (w *DueWorker) loop() error {
	for {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if err := w.renewIfNeeded(w.ctx); err != nil {
			return err
		}
		wake, err := w.turn(w.ctx)
		if err != nil {
			return err
		}
		if wake.IsZero() {
			continue
		}

		// A budget/timer wait cannot postpone renewal of the same original
		// lease. No wait holds native ownership or the lifecycle lock.
		w.backend.archiveLife.mu.Lock()
		renewAt := w.session.deadline.Add(-2 * time.Duration(w.session.ttlSeconds) * time.Second / 3)
		w.backend.archiveLife.mu.Unlock()
		if renewAt.Before(wake) {
			wake = renewAt
		}
		timer := time.NewTimer(max(time.Until(wake), 0))
		select {
		case <-w.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return w.ctx.Err()
		case <-timer.C:
		}
	}
}

func (w *DueWorker) renewIfNeeded(ctx context.Context) error {
	b := w.backend
	b.archiveLife.mu.Lock()
	err := w.session.live()
	// Renew after roughly one third of the original TTL, leaving two thirds
	// for a finite native turn and cleanup. This is deliberately earlier than
	// the minimum margin; it cannot extend authority after a slow physical join.
	at := w.session.deadline.Add(-2 * time.Duration(w.session.ttlSeconds) * time.Second / 3)
	b.archiveLife.mu.Unlock()
	if err != nil || time.Now().Before(at) {
		return err
	}
	w.pauseDiscovery("replaying")
	if err = b.RenewWorkerSession(ctx, w.session); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	w.status.Renewals++
	b.archiveLife.mu.Unlock()
	return nil
}

// Readiness is computed on a bucket copy. Only the existing real admission
// charges credit, so scheduling neither pre-consumes nor refunds a token.
func dueWorkerCreditAt(bucket workerBucket, now time.Time, amount uint32) time.Time {
	if bucket.take(now, amount) {
		return now
	}
	missing := uint64(amount) - bucket.tokens
	nanos := (missing*uint64(time.Second) - bucket.remainder + bucket.rate - 1) / bucket.rate
	return now.Add(time.Duration(nanos))
}

func (w *DueWorker) readyClaim(now time.Time) (*WorkerBudgetShare, time.Time) {
	b := w.backend
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	var earliest time.Time
	for n := 0; n < len(w.claims); n++ {
		i := (w.claimNext + n) % len(w.claims)
		share := w.claims[i]
		if !b.validWorkerBudget(share) || share.session != w.session || share.lost || share.released || share.release != nil || share.dueOwner != nil || w.session.operation != nil {
			continue
		}
		at := dueWorkerCreditAt(share.bucket, now, 1)
		if !at.After(now) {
			w.claimNext = (i + 1) % len(w.claims)
			return share, now
		}
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	if earliest.IsZero() {
		earliest = now.Add(100 * time.Millisecond)
	}
	return nil, earliest
}

func (w *DueWorker) turn(ctx context.Context) (time.Time, error) {
	// Finish the sole retained page before considering another partition.
	// A complete overdue prefix can therefore make real progress even when
	// the fixed-R set is much larger than the bounded future cache.
	for _, scan := range w.scans {
		if len(scan.page) > 0 {
			return w.scanTurn(ctx, scan)
		}
	}
	for n := 0; n < len(w.scans); n++ {
		i := (w.partNext + n) % len(w.scans)
		scan := w.scans[i]
		if !scan.listed {
			w.partNext = (i + 1) % len(w.scans)
			return w.scanTurn(ctx, scan)
		}
	}

	b := w.backend
	b.archiveLife.mu.Lock()
	if w.windowAt.IsZero() {
		w.windowAt = time.Now().Add(30 * time.Second)
	}
	rollover := !time.Now().Before(w.windowAt)
	b.archiveLife.mu.Unlock()
	if rollover {
		// resetView keeps each exact CAS handle. Old cache coverage remains
		// owned until a fresh Range establishes the replacement window, and
		// cannot be scheduled while any scan is incomplete.
		for _, scan := range w.scans {
			scan.resetView()
		}
		b.archiveLife.mu.Lock()
		w.windowAt = time.Time{}
		b.archiveLife.mu.Unlock()
		w.publishStatus("listing", nil)
		return time.Time{}, nil
	}

	if candidate, ok := w.cache.peek(); ok {
		// This observation only decides when to try. The unchanged actual
		// admission observes the trusted clock again before Grant and commit.
		// A saturated discovery window cannot be inverted to recover UTC.
		w.pauseDiscovery("replaying")
		lower, err := b.observeExpiryLowerBound(ctx)
		if err != nil {
			return time.Time{}, err
		}
		if !candidate.ExpiresAt.After(lower) {
			share, at := w.readyClaim(time.Now())
			if share == nil {
				w.publishStatus("backoff", nil)
				return at, nil
			}
			for _, scan := range w.scans {
				if scan.partition.partition != candidate.Partition {
					continue
				}
				scan.claims = share
				err = scan.admitCandidate(ctx, candidate, false)
				if err == nil {
					w.cache.settled(candidate)
					w.publishStatus("watching", nil)
					return time.Time{}, nil
				}
				if !errors.Is(err, ErrConflict) && !errors.Is(err, ErrOperationAdmissionClosed) || errors.Is(err, ErrOutcomeUnknown) {
					return time.Time{}, err
				}
				break
			}
		}
	}

	// A finite shared stream is fully stopped/joined by runDueWatchBatch
	// before this actor can renew or admit. Even an immediate idle bridge
	// yields 100ms, preventing a hot loop on an unchanged native revision.
	w.pauseDiscovery("replaying")
	_, err := b.runDueWatchBatch(ctx, w.cache)
	if err != nil {
		return w.rebuild(err)
	}
	w.publishStatus("watching", nil)
	return time.Now().Add(100 * time.Millisecond), nil
}

func (w *DueWorker) scanTurn(ctx context.Context, scan *dueWorkerScan) (time.Time, error) {
	if scan.index >= len(scan.page) {
		for i := range scan.page {
			scan.page[i] = DueCandidateRecord{}
		}
		scan.page, scan.index = nil, 0
		if scan.started && scan.cursor == nil {
			if err := scan.completeList(ctx); err != nil {
				return time.Time{}, err
			}
			if err := w.cache.addPage(scan); err != nil {
				return w.rebuild(err)
			}
			w.publishStatus("listing", nil)
			return time.Time{}, nil
		}
		w.backend.archiveLife.mu.Lock()
		at := dueWorkerCreditAt(scan.ranges.bucket, time.Now(), maxMutationBytes)
		w.backend.archiveLife.mu.Unlock()
		if time.Now().Before(at) {
			w.publishStatus("backoff", nil)
			return at, nil
		}

		w.publishStatus("listing", nil)
		err := w.backend.RunWorkerRelist(ctx, scan.partition, w.relist, func(owned context.Context) error {
			return scan.refill(owned)
		})
		if err != nil {
			if errors.Is(err, ErrOperationAdmissionClosed) {
				w.publishStatus("backoff", err)
				return time.Now().Add(time.Second), nil
			}
			return w.rebuild(err)
		}
		// An empty actual final page still registers this partition's R and
		// real list completion, so Watch can establish its initial bridge.
		if len(scan.page) == 0 {
			if scan.cursor == nil {
				if err = scan.completeList(ctx); err != nil {
					return time.Time{}, err
				}
			}
			if err = w.cache.addPage(scan); err != nil {
				return w.rebuild(err)
			}
		}
		w.publishStatus("listing", nil)
		return time.Time{}, nil
	}

	candidate := scan.page[scan.index]
	if scan.frontierBlocked {
		return w.cacheTail(ctx, scan)
	}
	// Scheduling uses an independent trusted lower bound, including at the
	// maximum valid date. The actual admission still performs its own rechecks.
	lower, err := w.backend.observeExpiryLowerBound(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if candidate.ExpiresAt.After(lower) {
		scan.frontierBlocked = true
		return w.cacheTail(ctx, scan)
	}
	share, at := w.readyClaim(time.Now())
	if share == nil {
		w.publishStatus("backoff", nil)
		return at, nil
	}
	scan.claims = share
	before := scan.index
	err = scan.step(ctx)
	if err == nil {
		for i := before; i < scan.index; i++ {
			scan.page[i] = DueCandidateRecord{}
		}
		w.publishStatus("listing", nil)
		return time.Time{}, nil
	}
	if errors.Is(err, ErrOperationAdmissionClosed) {
		scan.frontierBlocked = true
		return w.cacheTail(ctx, scan)
	}
	if errors.Is(err, ErrConflict) && !errors.Is(err, ErrOutcomeUnknown) {
		w.publishStatus("backoff", err)
		return time.Now().Add(100 * time.Millisecond), nil
	}
	return time.Time{}, err
}

func (w *DueWorker) cacheTail(ctx context.Context, scan *dueWorkerScan) (time.Time, error) {
	// Erase settled prefix records before slicing, so the retained backing
	// array cannot keep an uncharged source graph alive behind this tail.
	for i := 0; i < scan.index; i++ {
		scan.page[i] = DueCandidateRecord{}
	}
	scan.page, scan.index = scan.page[scan.index:], 0
	err := w.cache.addPage(scan)
	scan.page = nil
	if err != nil {
		return w.rebuild(err)
	}
	if scan.cursor == nil {
		if err = scan.completeList(ctx); err != nil {
			return time.Time{}, err
		}
	}
	w.publishStatus("listing", nil)
	return time.Time{}, nil
}

func (w *DueWorker) rebuild(cause error) (time.Time, error) {
	b := w.backend
	b.archiveLife.mu.Lock()
	healthy := !w.session.lost && !w.session.released && w.session.release == nil && w.ctx.Err() == nil && !b.archiveLife.closed
	b.archiveLife.mu.Unlock()
	if !healthy || errors.Is(cause, ErrOutcomeUnknown) || errors.Is(cause, ErrGuardExpired) || errors.Is(cause, ErrIdentityMismatch) {
		return time.Time{}, cause
	}
	// Known compaction and invalid discovery are local rebuilds only while
	// the original session is still live. No unknown metadata write is retried.
	if !errors.Is(cause, rpctypes.ErrCompacted) && errors.Is(cause, context.Canceled) {
		return time.Time{}, cause
	}
	w.cache.invalidate("discovery rebuild")
	for _, scan := range w.scans {
		if scan.checkpoint != nil && scan.checkpoint.record.Pending != nil {
			return time.Time{}, errors.Join(ErrOutcomeUnknown, cause)
		}
		scan.resetView()
	}
	b.archiveLife.mu.Lock()
	w.windowAt = time.Time{}
	b.archiveLife.mu.Unlock()
	w.publishStatus("invalid", cause)
	return time.Now().Add(time.Second), nil
}
