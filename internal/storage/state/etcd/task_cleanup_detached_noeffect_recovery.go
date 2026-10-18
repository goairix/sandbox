package etcd

import (
	"context"
	"errors"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// One original registered job owns this preflight and its single Run candidate.
// Only compact source pins and actual E/S survive; the authenticated full
// history remains temporary. Attempted is irreversible before native send.
type cleanupDetachedNoEffectRecovery struct {
	origin                     *Backend
	self                       *cleanupDetachedNoEffectRecovery
	job                        *cleanupLocalJob
	kernel                     *launcher.KernelBoundary
	worker                     launcher.WorkspaceSyncWorkerReference
	prior                      *cleanupSlotState
	pins                       [8]cleanupDetachedNoEffectPin
	runID, claimID, value      string
	attempted, confirmed, lost bool
}

// Recovery authenticates a historical Joined Run, not this process's ownership
// of that old worker. The new Run separately requires the current real Kernel.
func (b *Backend) validateCleanupDetachedRecoveryJob(j *cleanupLocalJob) error {
	if b == nil || b.archiveLife == nil || b.detachedCleanup == nil || b.localCleanup == nil || b.localCleanup.Worker == nil || j == nil || j.worker == nil || j.worker.backend != b || j.worker.self != j.worker || j.ctx == nil || !j.started || j.state == nil || j.task != j.state.record.Task || j.claim != nil || j.pendingProgress != nil {
		return ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	registered := false
	for _, original := range j.worker.cleanupJobs {
		if original == j {
			registered = true
			break
		}
	}
	b.archiveLife.mu.Unlock()
	if !registered {
		return ErrInvalidRecord
	}
	return j.ctx.Err()
}

// Actual native E.Joined is mandatory even if a lease disappeared or the
// caller presents a schema-valid copied Joined DTO. Present settlement may
// belong to an older Run; its authentication grants no fresh effect permission.
func (b *Backend) prepareCleanupDetachedNoEffectRecovery(ctx context.Context, j *cleanupLocalJob) (*cleanupDetachedNoEffectRecovery, error) {
	if ctx == nil {
		return nil, ErrInvalidRecord
	}
	if err := b.validateCleanupDetachedRecoveryJob(j); err != nil {
		return nil, err
	}
	if j.owner != nil {
		return nil, ErrConflict
	}
	if d := j.detachedRecovery; d != nil {
		if d.self != d || d.origin != b || d.job != j || d.lost {
			return nil, ErrConflict
		}
		return d, nil
	}
	h, err := b.readCleanupDetachedNoEffectHistory(ctx, j.task)
	if err != nil {
		return nil, err
	}
	s := h.state
	if s == nil || s.slot == nil || s.record.Phase != "occupied" || s.record.Run == nil || !s.record.Run.Joined || s.record.Archive != nil || s.record.Release != nil || s.execution != j.state.execution || j.state.slot == nil || *s.slot != *j.state.slot || j.runID != "" && j.runID != s.record.Run.ID || j.evidence == nil {
		return nil, ErrConflict
	}
	// Only an authenticated present fact permits post-drain termination phases
	// to resume with a new live Claim. An absent fact keeps the original strict
	// drained requirement and all Q/none pins. Present history grants no new
	// runtime send: the existing reservation consumer remains Query-only.
	if s.record.Confirmed != "drained" && (h.settlement == nil || j.detachedTarget == nil || s.record.Confirmed != "terminated" && s.record.Confirmed != "owner-released") {
		return nil, ErrConflict
	}
	if p := s.record.Pending; p != nil {
		empty := p.StageID == "" && p.RequestID == "" && p.AttemptID == "" && p.Digest == ""
		postFact := h.settlement != nil && j.detachedTarget != nil && empty && (p.Purpose == "termination" || p.Purpose == "owner-release")
		if !(empty && (p.Purpose == "final-sync" || p.Purpose == "claim") || p.Purpose == "detached-noeffect" && p.valid() || postFact) {
			return nil, ErrConflict
		}
	}
	if err = b.retireCleanupDetachedNoEffectStages(j, h); err != nil {
		return nil, err
	}

	worker, err := b.localCleanup.Worker.WorkspaceSyncWorkerReference()
	if err != nil {
		return nil, err
	}
	d := &cleanupDetachedNoEffectRecovery{origin: b, job: j, kernel: b.localCleanup.Worker, worker: worker, prior: s, runID: uuid.NewString(), claimID: uuid.NewString()}
	d.self = d
	r := s.record
	r.Run = &cleanupExecutionRun{ID: d.runID, Worker: worker}
	r.Pending = &cleanupExecutionPending{Purpose: "claim", ClaimID: d.claimID}
	d.value, err = encodeCleanupExecution(r)
	if err != nil {
		return nil, err
	}
	for i, pin := range []taskFence{h.quiescencePin.intent, h.quiescencePin.receipt, h.quiescenceReservationFence, h.finalSyncFence, h.snapshotFence, h.intentFence, h.publicationFence} {
		d.pins[i] = cleanupDetachedNoEffectPin{key: pin.key, stamp: detachedNoEffectStamp(pin)}
	}
	key, err := b.namespace.taskDetachedNoEffectKey(j.task)
	if err != nil {
		return nil, err
	}
	d.pins[7].key = key
	if h.settlement != nil {
		d.pins[7].stamp = detachedNoEffectStamp(ownTaskFence(h.settlement.kv))
	}
	// Count the complete candidate before attaching it. Preparation cannot
	// publish an oversized retained graph and reject it only after a write.
	if _, err = cleanupDetachedNoEffectGraphBytes(b, j, j.worker, j.ctx, d.kernel, j.detachedNoEffect, d); err != nil {
		return nil, err
	}
	j.detachedRecovery = d
	return d, nil
}

// The physical caller has already regained ownership from the original actor
// and authenticated current native Joined E/S. Once that original Claim is
// permanently lost, its completed and cleaned Stage mutation trees have no
// remaining lawful consumer. Retire those Stage pointers and the completed
// close/Q send/authorization payloads, keeping both draft identities and pins.
// The original Claim, prepared drain and settlement submission remain intact.
// None's complete draft must survive: its original write can still be queried
// with a dead Claim. No Stage object is mutated or guard/lease recreated.
func (b *Backend) retireCleanupDetachedNoEffectStages(j *cleanupLocalJob, h *cleanupDetachedNoEffectHistory) error {
	if j == nil || j.state == nil {
		return ErrInvalidRecord
	}
	d := j.detachedNoEffect
	if d == nil {
		return nil
	}
	if d.self != d || d.origin != b || d.job != j ||
		!b.validTaskClaim(d.claim) || d.claim.parentCtx != j.ctx ||
		d.prepared == nil || d.prepared.self != d.prepared || d.prepared.origin != b ||
		d.prepared.claim != d.claim || d.prepared.claimReference != d.claim.reference ||
		d.record.WriterClaim != detachedNoEffectClaim(d.claim.reference) ||
		d.state == nil || d.state.slot == nil || d.state.record.Run == nil || d.state.record.Run.Joined ||
		h == nil || h.self != h || h.origin != b || h.state == nil || h.state.slot == nil ||
		h.state.record.Run == nil || !h.state.record.Run.Joined ||
		!j.effectsJoined || j.owner != nil || j.claim != nil {
		return ErrInvalidRecord
	}
	s := h.state
	if s.execution != j.state.execution || s.execution.key != d.state.execution.key ||
		s.execution.create != d.state.execution.create || s.execution.mod <= d.state.execution.mod ||
		*s.slot != *d.state.slot || s.record.TaskBirth != d.record.TaskBirth ||
		s.record.Incarnation != d.record.Incarnation || s.record.Run.ID != d.record.RunID ||
		s.record.Run.ID != d.state.record.Run.ID || s.record.Run.Worker != d.state.record.Run.Worker ||
		d.prepared.pin != h.quiescencePin {
		return ErrConflict
	}

	// Registry/lifecycle validation happened before taking c.mu. Public
	// Prepare/Deliver calls use the same Claim lock and c.live; lost cannot be
	// reversed, so clearing stage cannot reopen their new-attempt path. Guard
	// cleanup must already have succeeded rather than being inferred from age.
	c := d.claim
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.lost {
		return ErrConflict
	}
	closeDraft, quiescenceDraft := c.closeDraft, c.quiescenceDraft
	retireClose := closeDraft != nil && closeDraft.outcome == OutcomeCommitted && closeDraft.cleaned && closeDraft.stage != nil
	retireQuiescence := quiescenceDraft != nil && quiescenceDraft.outcome == OutcomeCommitted && quiescenceDraft.cleaned && quiescenceDraft.stage != nil
	if retireClose && (closeDraft.stage.origin != b || closeDraft.stage.reference != closeDraft.reference.Stage) || retireQuiescence && (quiescenceDraft.stage.origin != b || quiescenceDraft.stage.reference != quiescenceDraft.reference.Stage) {
		return ErrInvalidRecord
	}
	if retireClose {
		closeDraft.stage = nil
	}
	if retireQuiescence {
		quiescenceDraft.stage = nil
	}

	// Close authorization checks Claim liveness before consuming these caches;
	// its native history remains available independently. Match the actual Q's
	// close command before retiring its completed send/authorization payloads.
	// A prior failed preparation may already have removed the Stage pointer.
	if closeDraft != nil && closeDraft.outcome == OutcomeCommitted && closeDraft.cleaned &&
		h.quiescence != nil && h.quiescence.Record != nil &&
		closeDraft.commandID == h.quiescence.Record.Context.CloseDataContext.CommandID &&
		closeDraft.reference.CommandID == h.quiescence.Record.Context.CloseDataContext.CommandID {
		closeDraft.certificate = nil
		closeDraft.issuer = nil
		closeDraft.issuerValue = ""
		closeDraft.claims = p.TaskCloseDataTicketClaims{}
		closeDraft.ticket = p.TaskCloseDataEvidence{}
		closeDraft.recordValue = ""
	}

	// An earlier over-cap preparation may already have retired Stage. Payload
	// retirement is therefore independently idempotent, and requires the exact
	// authenticated Q reference retained by the original prepared drain.
	if quiescenceDraft != nil &&
		quiescenceDraft.outcome == OutcomeCommitted && quiescenceDraft.cleaned &&
		quiescenceDraft.reference == d.prepared.pin.reference {
		quiescenceDraft.certificate = nil
		quiescenceDraft.issuer = nil
		quiescenceDraft.issuerValue = ""
		quiescenceDraft.claims = p.TaskUserQuiescenceTicketClaims{}
		quiescenceDraft.ticket = p.TaskUserQuiescenceEvidence{}
		quiescenceDraft.recordValue = ""
		if prior := quiescenceDraft.prior; prior != nil {
			prior.entry = nil
			prior.intent.value = ""
			prior.proof = nil
		}
	}
	return nil
}

func (b *Backend) validateCleanupDetachedRecoveryOwner(owner *workerDueOwner, j *cleanupLocalJob, d *cleanupDetachedNoEffectRecovery) error {
	if err := b.validateCleanupDetachedRecoveryJob(j); err != nil {
		return err
	}
	if d == nil || d.self != d || d.origin != b || d.job != j || j.detachedRecovery != d ||
		d.lost || d.kernel != b.localCleanup.Worker || d.prior == nil || d.prior.slot == nil ||
		d.prior.record.Run == nil || !d.prior.record.Run.Joined ||
		owner == nil || j.owner != owner || owner.partition != j.partition ||
		owner.call == nil || owner.call.session != j.worker.session ||
		j.state.slot == nil || *j.state.slot != *d.prior.slot {
		return ErrInvalidRecord
	}
	// A native commit can be confirmed before its actor reply is accepted.
	// Retain that original candidate across the handoff boundary rather than
	// requiring the cache to revert to old Joined E. This exception is available
	// only after the irreversible send latch and confirmation; begin therefore
	// still performs actual E/S arbitration and can never gain another Put.
	current, prior := j.state.execution, d.prior.execution
	confirmedCandidate := d.attempted && d.confirmed &&
		current.key == prior.key && current.create == prior.create && current.lease == prior.lease &&
		current.mod > prior.mod && current.value == d.value &&
		j.state.record.Run != nil && j.state.record.Run.ID == d.runID
	if current != prior && !confirmedCandidate {
		return ErrInvalidRecord
	}

	worker, err := d.kernel.WorkspaceSyncWorkerReference()
	if err != nil || worker != d.worker {
		return errors.Join(ErrInvalidRecord, err)
	}
	return nil
}

// The original actor alone consumes this private gate with its own charged
// owner. No local inventory/descriptor or managed Claim admission is borrowed.
func (b *Backend) beginCleanupDetachedNoEffectRecoveryRun(owner *workerDueOwner, j *cleanupLocalJob) error {
	if j == nil {
		return ErrInvalidRecord
	}
	d := j.detachedRecovery
	if err := b.validateCleanupDetachedRecoveryOwner(owner, j, d); err != nil {
		return err
	}
	if d.attempted {
		return b.arbitrateCleanupDetachedRecoveryRun(owner, j, d)
	}
	return b.commitCleanupRecoveryRunForWorker(owner, j, "detached-recovery-run", d.worker, d)
}

// Repetition after Unknown reads this original Run candidate only. Unchanged
// old Joined E remains Unknown, while a foreign E conflicts. Exact canonical
// candidate plus original native E birth/S proves that sole CAS committed.
func (b *Backend) arbitrateCleanupDetachedRecoveryRun(owner *workerDueOwner, j *cleanupLocalJob, d *cleanupDetachedNoEffectRecovery) error {
	observed, err := b.readCleanupKnownState(owner, d.prior)
	if err != nil {
		return err
	}
	if observed.execution == d.prior.execution {
		return ErrOutcomeUnknown
	}
	if observed.execution.value != d.value || observed.execution.mod <= d.prior.execution.mod {
		d.lost = true
		return ErrConflict
	}
	j.state = observed
	d.confirmed = true
	return nil
}

// The buffered reply has already transferred ownership back to this physical
// caller. Clearing before reply would release Unknown state too early; clearing
// in the actor after send would race its receiver. No actor borrows these roots
// after its replied physical phase.
func (b *Backend) acceptCleanupDetachedRecoveryReply(j *cleanupLocalJob) error {
	d := j.detachedRecovery
	if d == nil || d.self != d || d.origin != b || d.job != j || !d.confirmed || j.state == nil || j.state.record.Run == nil || j.state.record.Run.ID != d.runID || j.state.execution.value != d.value {
		return ErrInvalidRecord
	}
	j.detachedNoEffect = nil
	j.detachedRecovery = nil
	j.effectsJoined = false
	return nil
}

func cleanupDetachedRecoveryComparisons(d *cleanupDetachedNoEffectRecovery) []clientv3.Cmp {
	cmps := make([]clientv3.Cmp, 0, 8)
	for i, pin := range d.pins {
		if i == 7 && pin.stamp.ModRevision == 0 {
			cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(pin.key), "=", 0))
		} else {
			cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(pin.key), "=", pin.stamp.ModRevision))
		}
	}
	return cmps
}
