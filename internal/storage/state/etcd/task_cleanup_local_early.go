package etcd

import (
	"context"
	"errors"
	"reflect"
)

// The same registered job owns these loader results. They pin original
// history across preflight and Claim admission but grant no dispatch rights.
// In particular, state retains the old Run and Pending until its one CAS.
type cleanupEarlyRecovery struct {
	state      cleanupSlotState
	close      *TaskCloseDataEntry
	quiescence *TaskQuiescenceEntry
	attempt    *TaskQuiescenceAttemptEntry
}

func cleanupEarlyPhase(phase string) bool {
	return phase == "closed" || phase == "quiescent"
}

// Diagnostic phase only selects this bounded recovery path. Authentication
// comes from original immutable history, Query receipts and the new Claim.
func matchCleanupEarlyHistory(s *cleanupSlotState, bundle *taskClaimBundle, close *TaskCloseDataEntry, quiescence *TaskQuiescenceEntry, attempt *TaskQuiescenceAttemptEntry) error {
	if s == nil || bundle == nil || s.record.Validate() != nil || s.record.Phase != "occupied" || !cleanupEarlyPhase(s.record.Confirmed) || s.record.Run == nil || !s.record.Run.Joined || s.record.Archive != nil || s.record.Release != nil || s.slot == nil {
		return ErrInvalidRecord
	}
	r := s.record
	taskWire, err := encodeTaskRecord(bundle.task)
	if err != nil || bundle.task.Reference != r.Task || bundle.birth != r.TaskBirth || workerIdentityDigest(taskWire) != r.TaskDigest {
		return ErrIdentityMismatch
	}
	if close == nil || close.Record == nil || close.Outcome != OutcomeCommitted || close.Reference.Validate() != nil || close.Record.Validate() != nil {
		return ErrCorruptRecord
	}
	if close.Reference.Task != r.Task || close.Record.Task != bundle.task || close.Record.Context.ControlRevision != bundle.birth || close.Revision <= close.Record.Claim.CreateRevision || close.Reference.CommandID != close.Record.Context.CommandID || close.Reference.Stage != close.Record.Attempt.reference(close.Reference.Stage.Digest) {
		return ErrCorruptRecord
	}
	if quiescence == nil {
		if r.Confirmed != "closed" || attempt != nil {
			return ErrOutcomeUnknown
		}
	} else {
		q := quiescence.Record
		if q == nil || q.Validate() != nil || quiescence.Reference.Validate() != nil || quiescence.Outcome != OutcomeCommitted {
			return ErrCorruptRecord
		}
		if quiescence.Reference.Task != r.Task || q.Task != bundle.task || q.Context.CloseDataContext != close.Record.Context || q.Context.CloseDataTicketDigest != close.Record.TicketDigest || q.Context.CloseDataIntentRevision != close.Revision {
			return ErrCorruptRecord
		}
		if quiescence.Revision <= close.Revision || quiescence.Reference.CommandID != q.Context.Current.CommandID || quiescence.Reference.Stage != q.Attempt.reference(quiescence.Reference.Stage.Digest) || !taskQuiescenceHistoryReservation(*q, quiescence.Revision, attempt) {
			return ErrCorruptRecord
		}
		if attempt.Record.Validate() != nil || attempt.Reference != attempt.Record.reference() {
			return ErrCorruptRecord
		}
	}

	// A pending Stage is resolved only by its exact committed history. A bare
	// reservation or unrelated Stage cannot be erased by a new Run/Claim.
	if pending := r.Pending; pending != nil {
		var stage StageReference
		var claim TaskClaimReference
		switch pending.Purpose {
		case "close":
			stage, claim = close.Reference.Stage, close.Record.Claim
		case "quiescence":
			if quiescence != nil {
				stage, claim = quiescence.Reference.Stage, quiescence.Record.Claim
			} else if pending.AttemptID != "" {
				return ErrOutcomeUnknown
			}
		case "final-sync":
			if quiescence == nil || pending.AttemptID != "" {
				return ErrOutcomeUnknown
			}
			claim = quiescence.Record.Claim
		default:
			return ErrOutcomeUnknown
		}
		if pending.AttemptID != "" && (pending.StageID != stage.StageID || pending.RequestID != stage.RequestID || pending.AttemptID != stage.AttemptID || pending.Digest != stage.Digest) {
			return ErrConflict
		}
		if claim.ClaimID != "" && (pending.ClaimID != "" && pending.ClaimID != claim.ClaimID || pending.LeaseID != 0 && pending.LeaseID != claim.LeaseID) {
			return ErrConflict
		}
	}
	return nil
}

// Matching bytes alone would admit a delete/recreate ABA. The original E and
// immutable S native fences must remain exact until the new-Run CAS.
func matchCleanupEarlyOccupation(prior, current *cleanupSlotState) error {
	if prior == nil || current == nil || prior.slot == nil || current.slot == nil || prior.execution != current.execution || *prior.slot != *current.slot || !reflect.DeepEqual(prior.record, current.record) {
		return ErrConflict
	}
	return nil
}

// All original history RPCs, runtime Query and their session closes happen in
// the single physical job, outside the actor's native owner. The four-read
// quiescence loader retains its original protocol and finite request context.
func (b *Backend) readCleanupEarlyHistory(ctx context.Context, j *cleanupLocalJob, original *cleanupSlotState) (*cleanupEarlyRecovery, error) {
	if ctx == nil || j == nil || !j.earlyRecovery || j.owner != nil || j.target == nil || original == nil {
		return nil, ErrInvalidRecord
	}
	close, err := b.LoadTaskCloseData(ctx, j.task)
	if err != nil {
		return nil, err
	}
	attempt, err := b.LoadTaskQuiescenceAttempt(ctx, j.task)
	if err != nil {
		return nil, err
	}
	quiescence, err := b.LoadTaskQuiescence(ctx, j.task)
	if err != nil {
		return nil, err
	}
	if err = matchCleanupEarlyHistory(original, j.bundle, close, quiescence, attempt); err != nil {
		return nil, err
	}
	if err = b.requireCleanupEarlyArchiveAbsent(ctx, j.task); err != nil {
		return nil, err
	}
	closed, err := b.QueryTaskCloseData(ctx, close.Reference, j.target.destination)
	if err != nil || closed.Receipt == nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if quiescence != nil {
		result, err := b.QueryTaskQuiescence(ctx, j.task, j.target.destination)
		if err != nil {
			return nil, err
		}
		if err = b.waitCleanupQuiescence(j, result); err != nil {
			return nil, err
		}
	}
	return &cleanupEarlyRecovery{state: *original, close: close, quiescence: quiescence, attempt: attempt}, nil
}

func (b *Backend) requireCleanupEarlyArchiveAbsent(ctx context.Context, task TaskReference) error {
	projection, keys, err := b.cleanupEarlyHistoryProjection(task)
	if err != nil {
		return err
	}
	if err = validateCleanupWire(projection); err != nil {
		return err
	}
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	points, err := b.readExecEffectPoints(bounded, keys)
	if err != nil {
		return err
	}
	if err = bounded.Err(); err != nil {
		return err
	}
	if points[2] != nil || points[3] != nil {
		return ErrOutcomeUnknown
	}
	return nil
}

func (b *Backend) cleanupResumeEarly(j *cleanupLocalJob) error {
	history, err := b.readCleanupEarlyHistory(j.ctx, j, j.state)
	if err != nil {
		return err
	}
	j.early = history
	reply, err := j.worker.cleanupContinuation(j, &cleanupLocalContinuation{kind: "resume-early"})
	j.claim = reply.claim
	if err != nil {
		return err
	}
	return b.cleanupArchiveAndFinalize(j)
}

// A second authentic load/query after Claim admission must still be the same
// old commands and reservation. It cannot promote the preflight cache into
// authority, or silently switch an absent quiescence into new dispatch.
func (b *Backend) recheckCleanupEarlyHistory(j *cleanupLocalJob) error {
	if j == nil || j.early == nil || j.claim == nil {
		return ErrInvalidRecord
	}
	observed, err := b.readCleanupEarlyHistory(j.ctx, j, &j.early.state)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(j.early.close, observed.close) || !reflect.DeepEqual(j.early.quiescence, observed.quiescence) || !reflect.DeepEqual(j.early.attempt, observed.attempt) {
		return ErrConflict
	}
	return nil
}

// Pointer membership is checked before Kernel/profile or any native access.
// A copied job/history may not borrow a live actor owner to replace a Run.
func (b *Backend) validateCleanupEarlyOwner(owner *workerDueOwner, j *cleanupLocalJob) error {
	if b == nil || b.archiveLife == nil || owner == nil || j == nil || j.worker == nil || j.worker.backend != b || j.worker.self != j.worker || j.owner != owner || !j.started || !j.earlyRecovery || j.early == nil || j.state == nil || j.task != j.state.record.Task || j.runID != "" || j.claim != nil || j.pendingProgress != nil {
		return ErrInvalidRecord
	}
	if err := matchCleanupEarlyOccupation(&j.early.state, j.state); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	for _, registered := range j.worker.cleanupJobs {
		if registered == j {
			return nil
		}
	}
	return ErrInvalidRecord
}

func (b *Backend) beginCleanupEarlyRun(owner *workerDueOwner, j *cleanupLocalJob) error {
	if err := b.validateCleanupEarlyOwner(owner, j); err != nil {
		return err
	}
	if j.evidence == nil || b.cleanupPhysicalProfile() != nil {
		return ErrInvalidRecord
	}
	if err := matchCleanupEarlyHistory(j.state, j.bundle, j.early.close, j.early.quiescence, j.early.attempt); err != nil {
		return err
	}
	// The common transition generates exactly one candidate, validates its
	// complete slot wire inventory, writes 43 nodes and confirms exact E/S.
	// Unknown keeps that candidate; the old Run is never marked by this job.
	return b.commitCleanupRecoveryRun(owner, j, "recovery-early-run")
}
