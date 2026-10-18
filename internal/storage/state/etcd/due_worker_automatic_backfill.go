package etcd

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (w *DueWorker) automaticSelectBackfill(now time.Time) (bool, time.Time) {
	a := w.automatic
	var earliest time.Time
	for n := 0; n < len(a.partitions); n++ {
		i := (a.backfillNext + n) % len(a.partitions)
		part := &a.partitions[i]
		if part.backfill != nil && part.backfill.record.Phase == "complete" {
			continue
		}
		if now.Before(part.backfillAt) {
			if earliest.IsZero() || part.backfillAt.Before(earliest) {
				earliest = part.backfillAt
			}
			continue
		}
		a.active, a.backfill, a.phase = i, true, "backfill-read"
		if part.backfill != nil {
			a.phase = "backfill-page"
		}
		a.backfillNext = (i + 1) % len(a.partitions)
		return true, now
	}
	return false, earliest
}

// A phase covers one original bounded primitive, never an entire migration.
// Completion is durable only after the accepted P+B CAS. Known compaction
// refreshes the read view while keeping the authenticated immutable cutoff.
func (w *DueWorker) automaticBackfillTurn(ctx context.Context) (time.Time, error) {
	a, b := w.automatic, w.backend
	part := &a.partitions[a.active]
	scan := part.scan
	switch a.phase {
	case "backfill-read", "backfill-refresh-read":
		if at := w.automaticRangeAt(part); time.Now().Before(at) {
			return at, nil
		}
		observed, err := b.readCleanupBackfill(ctx, scan.partition, scan.ranges)
		if err != nil {
			return time.Time{}, err
		}
		if prior := part.backfill; prior != nil {
			if observed.state == nil || observed.state.fence.create != prior.fence.create || observed.state.record.LegacyCutoffRevision != prior.record.LegacyCutoffRevision || observed.state.record.Frontier < prior.record.Frontier {
				return time.Time{}, ErrCorruptRecord
			}
		}
		refresh := a.phase == "backfill-refresh-read"
		a.observation, part.backfill = observed, observed.state
		if part.backfill != nil && part.backfill.record.Phase == "complete" {
			w.automaticFinishPhase()
		} else if part.backfill == nil || refresh {
			a.phase = "backfill-refresh"
		} else {
			a.observation, a.phase = nil, "backfill-page"
		}
	case "backfill-refresh":
		share, at := w.readyClaim(time.Now())
		if share == nil {
			return at, nil
		}
		state, err := b.refreshCleanupBackfill(ctx, scan.partition, share, a.observation)
		if err != nil {
			return time.Time{}, err
		}
		part.backfill = state
		a.observation, a.phase = nil, "backfill-page"
	case "backfill-page":
		if at := w.automaticRangeAt(part); time.Now().Before(at) {
			return at, nil
		}
		var page *cleanupLegacyPage
		err := b.RunWorkerRelist(ctx, scan.partition, w.relist, func(owned context.Context) error {
			var err error
			page, err = b.readCleanupLegacyPage(owned, scan.partition, scan.ranges, part.backfill, nil)
			return err
		})
		if err != nil {
			return time.Time{}, err
		}
		a.legacy, a.phase = page, "backfill-advance"
		for _, row := range page.rows {
			if row.birth <= part.backfill.record.LegacyCutoffRevision {
				a.phase = "backfill-birth"
				break
			}
		}
	case "backfill-birth":
		if at := w.automaticRangeAt(part); time.Now().Before(at) {
			return at, nil
		}
		var taskID string
		for _, row := range a.legacy.rows {
			if row.birth <= part.backfill.record.LegacyCutoffRevision {
				taskID = row.taskID
				break
			}
		}
		birth, err := b.readCleanupBirth(ctx, scan.partition, scan.ranges, taskID)
		if err != nil {
			var refusal *cleanupBirthOptionalRefusal
			if !errors.As(err, &refusal) || !w.automaticHealthy(err) || b.validateCleanupBirthIndexEvidence(scan.partition, refusal.index) != nil {
				return time.Time{}, fmt.Errorf("legacy birth or pending evidence cannot establish coverage: %w", err)
			}
			// The reader still refused execution. Only this legacy domain may
			// use its separately sealed mandatory birth to add discovery P;
			// ordinary pending discovery keeps the unchanged strict refusal.
			a.lastError = dueWorkerError(fmt.Errorf("optional cleanup metadata refused; discovery indexing only: %w", err))
			a.indexBirth, a.phase = refusal.index, "backfill-index-advance"
			a.birth, a.release = nil, nil
			return time.Now(), nil
		}
		a.birth, a.phase = birth, "backfill-advance"
		if birth.releaseRecord != nil && (birth.execution == nil || birth.execution.Phase == "settled") {
			a.phase = "backfill-release"
		}
	case "backfill-release":
		if at := w.automaticRangeAt(part); time.Now().Before(at) {
			return at, nil
		}
		release, err := b.readCleanupBirthRelease(ctx, scan.partition, scan.ranges, a.birth)
		if err != nil {
			if !w.automaticHealthy(err) {
				return time.Time{}, err
			}
			a.lastError = dueWorkerError(err)
			// Canonical unsealed/settled history is covered conservatively by
			// publishing P, never by interpreting refusal as terminal proof.
			release = nil
		}
		a.release, a.phase = release, "backfill-advance"
	case "backfill-advance":
		share, at := w.readyClaim(time.Now())
		if share == nil {
			return at, nil
		}
		state, err := b.advanceCleanupBackfill(ctx, scan.partition, share, a.legacy, a.birth, a.release)
		if err != nil {
			return time.Time{}, err
		}
		part.backfill = state
		w.automaticFinishPhase()
	case "backfill-index-advance":
		share, at := w.readyClaim(time.Now())
		if share == nil {
			return at, nil
		}
		state, err := b.advanceCleanupBackfillIndex(ctx, scan.partition, share, a.legacy, a.indexBirth)
		if err != nil {
			return time.Time{}, err
		}
		part.backfill = state
		w.automaticFinishPhase()
	default:
		return time.Time{}, ErrInvalidRecord
	}
	return time.Now(), nil
}
