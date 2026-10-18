package etcd

import (
	"context"
	"errors"
	"reflect"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// The same registered job owns these loader results. They pin original
// history across preflight and Claim admission but grant no dispatch rights.
// In particular, state retains the old Run and Pending until its one CAS.
type cleanupEarlyRecovery struct {
	state      cleanupSlotState
	close      *TaskCloseDataEntry
	quiescence *TaskQuiescenceEntry
	attempt    *TaskQuiescenceAttemptEntry
	// Detached recovery additionally fences every old effect key and the
	// absence of the prior task Claim. Mod pins come only from native reads.
	detachedPins []cleanupDetachedNoEffectPin
}

func cleanupEarlyPhase(phase string) bool {
	return phase == "claimed" || phase == "closed" || phase == "quiescent"
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

	// A lost progress write can leave claimed after close actually committed.
	// Only its complete original pending coordinates may select that history;
	// the phase alone, or an empty pending field, cannot authorize recovery.
	if r.Confirmed == "claimed" {
		stage, claim := close.Reference.Stage, close.Record.Claim
		want := cleanupExecutionPending{
			Purpose: "close", ClaimID: claim.ClaimID, LeaseID: claim.LeaseID,
			StageID: stage.StageID, RequestID: stage.RequestID,
			AttemptID: stage.AttemptID, Digest: stage.Digest,
		}
		if r.Pending == nil || *r.Pending != want {
			return ErrConflict
		}
	}

	if quiescence == nil {
		if (r.Confirmed != "claimed" && r.Confirmed != "closed") || attempt != nil {
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
	var observed *cleanupEarlyRecovery
	var err error
	if j.detachedTarget != nil {
		if len(j.early.detachedPins) == 0 {
			observed, err = b.readCleanupDetachedCompletedDrain(j)
		} else {
			observed, err = b.readCleanupDetachedEarlyHistory(j)
		}
	} else {
		observed, err = b.readCleanupEarlyHistory(j.ctx, j, &j.early.state)
	}
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
	if j.detachedTarget == nil {
		if err := matchCleanupEarlyHistory(j.state, j.bundle, j.early.close, j.early.quiescence, j.early.attempt); err != nil {
			return err
		}
	} else if len(j.early.detachedPins) != 6 {
		return ErrInvalidRecord
	}
	// The common transition generates exactly one candidate, validates its
	// complete slot wire inventory and confirms exact E/S. Local retains its
	// original43 nodes; detached additionally pins six native effect/Claim keys.
	// Unknown keeps that candidate; the old Run is never marked by this job.
	return b.commitCleanupRecoveryRun(owner, j, "recovery-early-run")
}

// Detached recovery uses the existing Stage/history/Query protocol. An absent
// close is retryable only after actual Joined E/S, actual Claim absence and a
// same-CAS absence fence on every effect key. A committed but unreceived close
// remains Query-Unknown; a legacy orphan Q reservation is never overwritten.
func (b *Backend) readCleanupDetachedEarlyHistory(j *cleanupLocalJob) (*cleanupEarlyRecovery, error) {
	if j == nil || j.detachedTarget == nil || j.owner != nil || !j.earlyRecovery || j.target == nil || j.bundle == nil {
		return nil, ErrInvalidRecord
	}
	original := j.state
	if j.early != nil {
		original = &j.early.state
	}
	if original == nil || original.slot == nil || original.record.Run == nil || !original.record.Run.Joined || original.record.Archive != nil || original.record.Release != nil || original.record.Task != j.task || original.record.TaskBirth != j.bundle.birth {
		return nil, ErrInvalidRecord
	}
	closeKey, err := b.namespace.taskCloseDataKey(j.task)
	if err != nil {
		return nil, err
	}
	qKey, err := b.namespace.taskQuiescenceKey(j.task)
	if err != nil {
		return nil, err
	}
	reservationKey, err := b.namespace.taskQuiescenceAttemptKey(j.task)
	if err != nil {
		return nil, err
	}
	noneKey, err := b.namespace.taskFinalSyncAttemptKey(j.task)
	if err != nil {
		return nil, err
	}
	factKey, err := b.namespace.taskDetachedNoEffectKey(j.task)
	if err != nil {
		return nil, err
	}
	claimKey, err := b.namespace.taskClaimKey(j.task)
	if err != nil {
		return nil, err
	}
	keys := []string{b.identityKey, b.restoreKey, closeKey, qKey, reservationKey, noneKey, factKey, claimKey}
	points, _, err := b.readCleanupDetachedNoEffectPoints(j.ctx, "early-history", keys, []int{maxRecordBytes, 256, 32768, 32768, 8192, 8192, 32768, 4096})
	if err != nil {
		return nil, err
	}
	if points[5] != nil || points[6] != nil {
		return nil, ErrOutcomeUnknown
	}
	if j.claim == nil {
		if points[7] != nil {
			return nil, ErrConflict
		}
	} else if points[7] == nil || string(points[7].Value) != j.claim.value || points[7].Lease != j.claim.reference.LeaseID || points[7].CreateRevision != j.claim.reference.CreateRevision {
		return nil, ErrConflict
	}
	h := &cleanupEarlyRecovery{state: *original}
	for i, kv := range points[2:] {
		pin := cleanupDetachedNoEffectPin{key: keys[i+2]}
		if kv != nil {
			pin.stamp = detachedNoEffectStamp(ownTaskFence(kv))
		}
		h.detachedPins = append(h.detachedPins, pin)
	}
	h.close, err = b.LoadTaskCloseData(j.ctx, j.task)
	if err != nil {
		return nil, err
	}
	h.attempt, err = b.LoadTaskQuiescenceAttempt(j.ctx, j.task)
	if err != nil {
		return nil, err
	}
	h.quiescence, err = b.LoadTaskQuiescence(j.ctx, j.task)
	if err != nil {
		return nil, err
	}
	if h.close == nil {
		if points[2] != nil || h.quiescence != nil || points[3] != nil || h.attempt != nil || points[4] != nil || (original.record.Confirmed != "reserved" && original.record.Confirmed != "claimed") {
			return nil, ErrOutcomeUnknown
		}
	} else {
		c := h.close
		if points[2] == nil || c.Record == nil || c.Outcome != OutcomeCommitted || c.Revision != points[2].ModRevision || c.Record.Task != j.bundle.task || c.Record.Context.ControlRevision != j.bundle.birth {
			return nil, ErrCorruptRecord
		}
		closed, err := b.QueryTaskCloseData(j.ctx, c.Reference, j.target.destination)
		if err != nil || closed.Receipt == nil {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
	}
	if h.quiescence == nil {
		if points[3] != nil || h.attempt != nil || points[4] != nil || original.record.Confirmed == "quiescent" || original.record.Confirmed == "drained" {
			return nil, ErrOutcomeUnknown
		}
	} else {
		q := h.quiescence
		if h.close == nil || points[3] == nil || points[4] == nil || q.Record == nil || q.Outcome != OutcomeCommitted || q.Revision != points[3].ModRevision || q.Record.Task != j.bundle.task || q.Record.Context.CloseDataContext != h.close.Record.Context || q.Record.Context.CloseDataIntentRevision != h.close.Revision || q.Record.Context.CloseDataTicketDigest != h.close.Record.TicketDigest || !taskQuiescenceHistoryReservation(*q.Record, q.Revision, h.attempt) || h.attempt.Revision != points[4].ModRevision {
			return nil, ErrCorruptRecord
		}
		queried, err := b.QueryTaskQuiescence(j.ctx, j.task, j.target.destination)
		if err != nil {
			return nil, err
		}
		if err = b.waitCleanupQuiescence(j, queried); err != nil {
			return nil, err
		}
	}
	if pending := original.record.Pending; pending != nil && pending.AttemptID != "" {
		ref := StageReference{Namespace: j.task.Namespace, RestoreEpoch: j.task.RestoreEpoch, Partition: j.task.Partition, StageID: pending.StageID, RequestID: pending.RequestID, AttemptID: pending.AttemptID, Digest: pending.Digest}
		if h.close != nil && ref == h.close.Reference.Stage || h.quiescence != nil && ref == h.quiescence.Reference.Stage {
			return h, nil
		}
		// Only the original abandoned Stage may be resolved. A new UUID does
		// not replace its uncertain receipt, and resolution never sends a command.
		outcome, err := b.ResolveStage(j.ctx, ref)
		if err != nil || outcome != OutcomeAborted {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
	}
	return h, nil
}

func (b *Backend) resumeCleanupDetached(j *cleanupLocalJob) error {
	none, err := b.LoadTaskFinalSyncAttempt(j.ctx, j.task)
	if err != nil {
		return err
	}
	if none != nil {
		// Existing none is permanent and belongs to its historical Claim. A
		// replacement Claim never re-reserves it. Absent settlement still needs
		// original read-only close/Q queries and a fresh current dual drain.
		history, err := b.readCleanupDetachedNoEffectHistory(j.ctx, j.task)
		if err != nil {
			return err
		}
		if history.settlement == nil {
			j.early, err = b.readCleanupDetachedCompletedDrain(j)
			if err != nil {
				return err
			}
		}
		if _, err = b.prepareCleanupDetachedNoEffectRecovery(j.ctx, j); err != nil {
			return err
		}
		if _, err = j.worker.cleanupContinuation(j, &cleanupLocalContinuation{kind: "resume-detached-noeffect"}); err != nil {
			return err
		}
		var reply cleanupLocalReply
		reply, err = j.worker.cleanupContinuation(j, &cleanupLocalContinuation{kind: "claim-detached"})
		j.claim = reply.claim
		return err
	}
	j.early, err = b.readCleanupDetachedEarlyHistory(j)
	if err != nil {
		return err
	}
	reply, err := j.worker.cleanupContinuation(j, &cleanupLocalContinuation{kind: "resume-early"})
	j.claim = reply.claim
	return err
}

// None is already authenticated but is not itself an effect settlement. Query
// only the original close/Q, preserving their Stage/Claim/attempt identity.
// The new managed Claim must still obtain a fresh dual-drain observation.
func (b *Backend) readCleanupDetachedCompletedDrain(j *cleanupLocalJob) (*cleanupEarlyRecovery, error) {
	history, err := b.readCleanupDetachedNoEffectHistory(j.ctx, j.task)
	if err != nil {
		return nil, err
	}
	closed, err := b.LoadTaskCloseData(j.ctx, j.task)
	if err != nil || closed == nil || closed.Record == nil || closed.Outcome != OutcomeCommitted || closed.Record.Task != j.bundle.task || history.quiescence.Record.Context.CloseDataContext != closed.Record.Context || history.quiescence.Record.Context.CloseDataIntentRevision != closed.Revision {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	result, err := b.QueryTaskCloseData(j.ctx, closed.Reference, j.target.destination)
	if err != nil || result.Receipt == nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	quiescence, err := b.QueryTaskQuiescence(j.ctx, j.task, j.target.destination)
	if err != nil {
		return nil, err
	}
	if err = b.waitCleanupQuiescence(j, quiescence); err != nil {
		return nil, err
	}
	attempt, err := b.LoadTaskQuiescenceAttempt(j.ctx, j.task)
	if err != nil {
		return nil, err
	}
	return &cleanupEarlyRecovery{state: *j.state, close: closed, quiescence: history.quiescence, attempt: attempt}, nil
}

// Kept here with early recovery rather than granting a generic absent-key
// inventory API. All six pins belong to this one original history preflight.
func cleanupDetachedEarlyComparisons(h *cleanupEarlyRecovery) []clientv3.Cmp {
	cmps := make([]clientv3.Cmp, 0, len(h.detachedPins))
	for _, pin := range h.detachedPins {
		if pin.stamp.ModRevision == 0 {
			cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(pin.key), "=", 0))
		} else {
			cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(pin.key), "=", pin.stamp.ModRevision))
		}
	}
	return cmps
}
