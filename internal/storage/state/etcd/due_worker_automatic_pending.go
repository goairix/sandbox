package etcd

import (
	"context"
	"errors"
	"time"
)

func (w *DueWorker) automaticSelectPending(now time.Time) (bool, time.Time) {
	a := w.automatic
	if a.pageReady {
		a.active, a.backfill, a.phase = a.pendingPart, false, "pending-select"
		return true, now
	}
	var earliest time.Time
	for n := 0; n < len(a.partitions); n++ {
		i := (a.pendingNext + n) % len(a.partitions)
		part := &a.partitions[i]
		if now.Before(part.nextSweep) {
			if earliest.IsZero() || part.nextSweep.Before(earliest) {
				earliest = part.nextSweep
			}
			continue
		}
		a.active, a.backfill, a.phase = i, false, "pending-page"
		a.pendingNext = (i + 1) % len(a.partitions)
		return true, now
	}
	return false, earliest
}

// Every read/Claim is its existing bounded original primitive. Only a Task
// whose freshly authenticated birth matches the hint reaches the old registry;
// neither the hint nor a completed backfill marker is physical permission.
func (w *DueWorker) automaticPendingTurn(ctx context.Context) (time.Time, error) {
	a, b := w.automatic, w.backend
	part := &a.partitions[a.active]
	scan := part.scan
	switch a.phase {
	case "pending-page":
		if at := w.automaticRangeAt(part); time.Now().Before(at) {
			return at, nil
		}
		var page cleanupPendingPage
		err := b.RunWorkerRelist(ctx, scan.partition, w.relist, func(owned context.Context) error {
			var err error
			page, err = b.readCleanupPendingPage(owned, scan.partition, scan.ranges, part.cursor)
			return err
		})
		if err != nil {
			return time.Time{}, err
		}
		a.page, a.index, a.pageReady, a.pendingPart = page, 0, true, a.active
		part.pendingRevision = page.Revision
		a.rejected += uint64(page.Rejected)
		a.phase = "pending-select"
		if len(page.Candidates) == 0 {
			w.automaticCompletePage()
		}
	case "pending-select":
		if a.index >= len(a.page.Candidates) {
			w.automaticCompletePage()
			return time.Now(), nil
		}
		a.candidate = a.page.Candidates[a.index]
		duplicate, full := w.automaticRegistry(a.candidate.Reference)
		if duplicate {
			w.automaticAdvancePending()
		} else if full {
			// Preserve the unconsumed page tail while offering backfill a turn.
			w.automaticFinishPhase()
			return time.Now().Add(100 * time.Millisecond), nil
		} else {
			a.phase = "pending-birth"
		}
	case "pending-birth":
		if at := w.automaticRangeAt(part); time.Now().Before(at) {
			return at, nil
		}
		birth, err := b.readCleanupBirth(ctx, scan.partition, scan.ranges, a.candidate.Reference.TaskID)
		if err != nil {
			return time.Time{}, err
		}
		if birth.record.Reference != a.candidate.Reference || workerIdentityDigest(birth.task.value) != a.candidate.TaskDigest {
			return time.Time{}, ErrCorruptRecord
		}
		if birth.pending == nil {
			w.automaticAdvancePending()
			return time.Now(), nil
		}
		a.birth = birth
		a.phase = "pending-enqueue"
		if birth.detachedReleaseRecord != nil {
			// A v2 value is a metadata hint, never signed release permission.
			// Only the cold registry path may authenticate it in paid turns;
			// an unfinished Run retains P without entering physical admission.
			if !b.cleanupColdQuotaEnabled() || !cleanupDetachedBirthCandidate(birth) {
				refusal := ErrInvalidRecord
				if !b.cleanupColdQuotaEnabled() {
					refusal = ErrInvalidConfiguration
				}
				a.lastError = dueWorkerError(refusal)
				w.automaticAdvancePending()
				return time.Now(), nil
			}
			a.phase = "pending-delete"
			return time.Now(), nil
		}
		if birth.release != nil && (birth.execution == nil || birth.execution.Phase == "settled") {
			a.phase = "pending-release"
		}
	case "pending-release":
		if at := w.automaticRangeAt(part); time.Now().Before(at) {
			return at, nil
		}
		release, err := b.readCleanupBirthRelease(ctx, scan.partition, scan.ranges, a.birth)
		if err != nil {
			if !w.automaticHealthy(err) {
				return time.Time{}, err
			}
			a.lastError = dueWorkerError(err)
			a.phase = "pending-enqueue"
			return time.Now(), nil
		}
		a.release, a.phase = release, "pending-delete"
	case "pending-delete":
		if b.cleanupColdQuotaEnabled() {
			_, err := w.enqueueCleanupColdQuota(a.birth, a.release)
			if errors.Is(err, ErrConflict) {
				duplicate, full := w.automaticRegistry(a.candidate.Reference)
				if !duplicate && full {
					w.automaticFinishPhase()
					return time.Now().Add(100 * time.Millisecond), nil
				}
				if !duplicate {
					return time.Time{}, err
				}
			} else if err != nil {
				return time.Time{}, err
			}
			w.automaticAdvancePending()
			return time.Now(), nil
		}
		share, at := w.readyClaim(time.Now())
		if share == nil {
			return at, nil
		}
		if err := b.deleteCleanupPending(ctx, scan.partition, share, a.birth, a.release); err != nil {
			return time.Time{}, err
		}
		w.automaticAdvancePending()
	case "pending-enqueue":
		// Registry work authenticates its own birth/occupation. Free the cold
		// proof before enqueue or waiting for capacity; retain only the hint.
		a.birth, a.release = nil, nil
		_, err := w.enqueueCleanupLocal(w.ctx, a.candidate.Reference)
		if errors.Is(err, ErrConflict) {
			duplicate, full := w.automaticRegistry(a.candidate.Reference)
			if !duplicate && full {
				w.automaticFinishPhase()
				return time.Now().Add(100 * time.Millisecond), nil
			}
			if !duplicate {
				return time.Time{}, err
			}
		} else if err != nil {
			return time.Time{}, err
		}
		w.automaticAdvancePending()
	default:
		return time.Time{}, ErrInvalidRecord
	}
	return time.Now(), nil
}

// Duplicate jobs advance the stream, but a full registry preserves the next
// candidate. The existing actor owns all job execution and completion joins.
func (w *DueWorker) automaticRegistry(task TaskReference) (duplicate, full bool) {
	b := w.backend
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	capacity := 32
	if b.localCleanup != nil {
		capacity = int(b.localCleanup.MaxActive)
	}
	active := 0
	for _, job := range w.cleanupJobs {
		if job != nil {
			active++
			duplicate = duplicate || job.task == task
		}
	}
	return duplicate, active >= capacity
}

func (w *DueWorker) automaticAdvancePending() {
	a := w.automatic
	if a.pageReady && a.index < len(a.page.Candidates) {
		a.page.Candidates[a.index] = cleanupPendingRecord{}
		a.index++
	}
	if a.pageReady && a.index == len(a.page.Candidates) {
		w.automaticCompletePage()
		return
	}
	w.automaticFinishPhase()
}

// Retain only the original continuation at a page boundary. Completion's next
// fresh revision is at least 30s later; job success never accelerates this clock.
func (w *DueWorker) automaticCompletePage() {
	a := w.automatic
	part := &a.partitions[a.pendingPart]
	part.cursor = a.page.Next
	if part.cursor == nil {
		part.nextSweep = time.Now().Add(30 * time.Second)
	}
	a.page, a.pageReady, a.index, a.pendingPart = cleanupPendingPage{}, false, 0, -1
	w.automaticFinishPhase()
}
