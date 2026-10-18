package etcd

import (
	"context"
	"sync/atomic"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

type coldQuotaWorkerRecoveryPhase uint8

const (
	coldQuotaRecoveryWantFirstReturn coldQuotaWorkerRecoveryPhase = iota
	coldQuotaRecoveryWantJournal
	coldQuotaRecoveryWantResolve
	coldQuotaRecoveryWantAbortedReceipt
	coldQuotaRecoveryWantCommittedReturn
	coldQuotaRecoveryWantCommittedReceipt
	coldQuotaRecoveryDone
)

// coldQuotaWorkerRecoveryResult contains ordinary historical metadata only.
// An entry is committed replay data; a nil entry is ready for the existing
// fresh builder, with either no previous attempt or both authenticated original
// journal/aborted snapshots. The snapshots are borrowed immutable data, never
// preparation, scheduling, replacement or physical-cleanup authority.
type coldQuotaWorkerRecoveryResult struct {
	entry             *ColdRuntimeQuotaReturnEntry
	previous, aborted *mvccpb.KeyValue
}

// coldQuotaWorkerRecovery borrows already authenticated immutable history from
// one original enrolled owner. It never clones that graph or reconstructs a
// Commit permit. The compact seals prevent rebinding across paid turns; only
// the nonblocking busy winner owns mutable phase/proof state through actual
// native return, typed decoding and the final ownership check.
type coldQuotaWorkerRecovery struct {
	self         *coldQuotaWorkerRecovery
	backend      *Backend
	owner        *runtimeEnrolledPublicationOwner
	ownerContext context.Context
	history      *coldQuotaReturnHistory
	session      *WorkerSession
	partition    *WorkerPartitionClaim
	ranges       *WorkerBudgetShare
	claims       *WorkerBudgetShare

	returnKey  string
	journalKey string
	task       TaskReference
	taskBirth  int64
	quotaOwner ColdSlotOwner

	busy  uint32
	phase coldQuotaWorkerRecoveryPhase
	stamp coldQuotaReturnProofStamp

	// Canonical pending attribution is saved before Resolve can dispatch. An
	// unknown result never clears it, including when the original session is
	// lost and this cursor can no longer continue. Durable history remains for
	// a future actor; this cursor cannot mint a different attempt.
	reference StageReference
	journal   *mvccpb.KeyValue
	aborted   *mvccpb.KeyValue
	committed bool
}

// newColdQuotaWorkerRecovery freezes only original binding data and key names.
// The caller retains history unchanged and owns enrollment through all steps.
// Construction checks live original membership without inspecting credit or
// performing any native read, arbitration or mutation.
func (b *Backend) newColdQuotaWorkerRecovery(o *runtimeEnrolledPublicationOwner, h *coldQuotaReturnHistory, session *WorkerSession, partition *WorkerPartitionClaim, ranges, claims *WorkerBudgetShare) (*coldQuotaWorkerRecovery, error) {
	if !b.validWorkerBackend() {
		return nil, ErrInvalidRecord
	}
	ctx, err := b.coldQuotaReturnOwnerContext(o)
	if err != nil {
		return nil, err
	}
	if err := b.validateColdQuotaWorkerRecoveryMembers(session, partition, ranges, claims); err != nil {
		return nil, err
	}
	returnKey, journalKey, err := b.coldQuotaWorkerRecoveryKeys(h, partition)
	if err != nil {
		return nil, err
	}
	binding, err := coldQuotaReturnRelease(h)
	if err != nil {
		return nil, err
	}

	state := &coldQuotaWorkerRecovery{
		backend: b, owner: o, ownerContext: ctx, history: h,
		session: session, partition: partition, ranges: ranges, claims: claims,
		returnKey: returnKey, journalKey: journalKey,
		task: binding.task, taskBirth: binding.birth,
		quotaOwner: h.reservation.Owner,
	}
	state.self = state
	return state, nil
}

// stepColdQuotaWorkerRecovery performs at most one paid metadata read or one
// paid Resolve, then yields nil,nil until ordinary result data is complete.
// Every refusal/error retains the exact outstanding phase and original partial
// proof. In particular arbitration's outcome is never sufficient: aborted
// requires its next paid receipt read, and committed requires the next paid
// return and receipt reads. Done still validates original ownership/membership.
func (b *Backend) stepColdQuotaWorkerRecovery(o *runtimeEnrolledPublicationOwner, h *coldQuotaReturnHistory, state *coldQuotaWorkerRecovery, session *WorkerSession, partition *WorkerPartitionClaim, ranges, claims *WorkerBudgetShare) (*coldQuotaWorkerRecoveryResult, error) {
	if state == nil || state.self != state || state.backend != b {
		return nil, ErrInvalidRecord
	}
	if !atomic.CompareAndSwapUint32(&state.busy, 0, 1) {
		return nil, ErrConflict
	}
	defer atomic.StoreUint32(&state.busy, 0)

	validate := func() error {
		return b.validateColdQuotaWorkerRecoveryState(o, h, state, session, partition, ranges, claims)
	}
	if err := validate(); err != nil {
		return nil, err
	}

	switch state.phase {
	case coldQuotaRecoveryWantFirstReturn, coldQuotaRecoveryWantCommittedReturn:
		// The immutable return protocol caps a value at 16 KiB. It is checked
		// before the journal on first discovery and never reads a mutable slot.
		points, err := b.readColdQuotaWorkerPoints(o, session, partition, ranges, []string{state.returnKey}, []int{16384})
		if err != nil {
			return nil, err
		}
		if points[0] == nil && state.phase == coldQuotaRecoveryWantFirstReturn {
			if err := validate(); err != nil {
				return nil, err
			}
			state.phase = coldQuotaRecoveryWantJournal
			return nil, nil
		}
		stamp, err := b.decodeColdQuotaReturnProof(h, points[0])
		if err != nil {
			return nil, err
		}
		if err := validate(); err != nil {
			return nil, err
		}
		state.stamp = stamp
		state.phase = coldQuotaRecoveryWantCommittedReceipt
		return nil, nil

	case coldQuotaRecoveryWantJournal:
		points, err := b.readColdQuotaWorkerPoints(o, session, partition, ranges, []string{state.journalKey}, []int{coldQuotaAttemptMaxBytes})
		if err != nil {
			return nil, err
		}
		if points[0] == nil {
			if err := validate(); err != nil {
				return nil, err
			}
			state.phase = coldQuotaRecoveryDone
			return state.result(), nil
		}
		record, err := b.decodeColdRuntimeQuotaAttempt(h, points[0])
		if err != nil {
			return nil, err
		}
		if err := validate(); err != nil {
			return nil, err
		}
		state.journal, state.reference = points[0], record.Reference
		state.phase = coldQuotaRecoveryWantResolve
		return nil, nil

	case coldQuotaRecoveryWantResolve:
		ref, outcome, err := b.resolveColdRuntimeQuotaAttemptWorker(o, h, state.journal, session, partition, ranges, claims)
		if err != nil {
			return nil, err
		}
		if ref != state.reference {
			return nil, ErrCorruptRecord
		}
		var next coldQuotaWorkerRecoveryPhase
		switch outcome {
		case OutcomeAborted:
			next = coldQuotaRecoveryWantAbortedReceipt
		case OutcomeCommitted:
			next = coldQuotaRecoveryWantCommittedReturn
		default:
			return nil, ErrOutcomeUnknown
		}
		if err := validate(); err != nil {
			return nil, err
		}
		state.phase = next
		return nil, nil

	case coldQuotaRecoveryWantAbortedReceipt:
		_, key, err := b.stageKeys(state.reference)
		if err != nil {
			return nil, err
		}
		points, err := b.readColdQuotaWorkerPoints(o, session, partition, ranges, []string{key}, []int{coldQuotaAttemptMaxBytes})
		if err != nil {
			return nil, err
		}
		if err := coldQuotaAttemptAbortedReceipt(state.journal, points[0], state.reference); err != nil {
			return nil, err
		}
		if err := validate(); err != nil {
			return nil, err
		}
		state.aborted = points[0]
		state.phase = coldQuotaRecoveryDone
		return state.result(), nil

	case coldQuotaRecoveryWantCommittedReceipt:
		key, err := b.runtimeDispatchReceiptKey(state.stamp.attempt)
		if err != nil {
			return nil, err
		}
		points, err := b.readColdQuotaWorkerPoints(o, session, partition, ranges, []string{key}, []int{coldQuotaAttemptMaxBytes})
		if err != nil {
			return nil, err
		}
		ref, _, err := b.decodeColdQuotaReturnReceipt(state.stamp, points[0])
		if err != nil {
			return nil, err
		}
		// Early replay intentionally precedes pending attribution. Once a
		// journal was observed, its full reference (including digest) is final.
		if state.journal != nil && ref != state.reference {
			return nil, ErrCorruptReceipt
		}
		if err := validate(); err != nil {
			return nil, err
		}
		state.committed = true
		state.phase = coldQuotaRecoveryDone
		return state.result(), nil

	case coldQuotaRecoveryDone:
		return state.result(), nil

	default:
		return nil, ErrInvalidRecord
	}
}

// result copies the wrapper and committed entry so caller edits cannot affect
// cached done. Original authenticated journal/receipt snapshots stay borrowed;
// retaining them pins later comparison inputs without copying source history.
func (state *coldQuotaWorkerRecovery) result() *coldQuotaWorkerRecoveryResult {
	if state.committed {
		entry := state.stamp.entry
		return &coldQuotaWorkerRecoveryResult{entry: &entry}
	}
	return &coldQuotaWorkerRecoveryResult{previous: state.journal, aborted: state.aborted}
}

// validateColdQuotaWorkerRecoveryState runs before every step and again before
// publishing any decoded partial or terminal result. Replacing a canceled owner
// context, history pointer, compact source binding or original live member can
// never extend this cursor's lifetime, even when its result is already cached.
func (b *Backend) validateColdQuotaWorkerRecoveryState(o *runtimeEnrolledPublicationOwner, h *coldQuotaReturnHistory, state *coldQuotaWorkerRecovery, session *WorkerSession, partition *WorkerPartitionClaim, ranges, claims *WorkerBudgetShare) error {
	if !b.validWorkerBackend() || state.owner != o || state.history != h || state.session != session || state.partition != partition || state.ranges != ranges || state.claims != claims {
		return ErrInvalidRecord
	}
	ctx, err := b.coldQuotaReturnOwnerContext(o)
	if err != nil {
		return err
	}
	if ctx != state.ownerContext {
		return ErrInvalidRecord
	}
	if err := b.validateColdQuotaWorkerRecoveryMembers(session, partition, ranges, claims); err != nil {
		return err
	}
	returnKey, journalKey, err := b.coldQuotaWorkerRecoveryKeys(h, partition)
	if err != nil {
		return err
	}
	binding, err := coldQuotaReturnRelease(h)
	if err != nil {
		return err
	}
	if state.returnKey != returnKey || state.journalKey != journalKey || state.task != binding.task || state.taskBirth != binding.birth || state.quotaOwner != h.reservation.Owner {
		return ErrInvalidRecord
	}
	return nil
}

// coldQuotaWorkerRecoveryKeys checks local shape and binding only. Authentication
// remains the caller's preceding responsibility; no metadata is fetched here.
// Six borrowed snapshots must exist because later typed decoders consume them.
func (b *Backend) coldQuotaWorkerRecoveryKeys(h *coldQuotaReturnHistory, partition *WorkerPartitionClaim) (string, string, error) {
	if h == nil || h.publication == nil || h.publication.entry == nil || partition == nil {
		return "", "", ErrInvalidRecord
	}
	binding, err := coldQuotaReturnRelease(h)
	if err != nil {
		return "", "", err
	}
	for _, point := range []*mvccpb.KeyValue{h.publication.journal, h.publication.receipt, h.reservedKV, h.workKV, h.settledKV, binding.kv} {
		if !operationPermanentKV(point) || point.Version <= 0 {
			return "", "", ErrInvalidRecord
		}
	}
	owner, task := h.reservation.Owner, binding.task
	if owner.Partition != partition.partition || task.Partition != owner.Partition || task.Namespace != b.namespace.Root() || task.RestoreEpoch != b.restoreEpoch || task.SandboxID != owner.SandboxID || task.Validate() != nil || binding.birth <= 0 {
		return "", "", ErrInvalidRecord
	}
	for _, ref := range []StageReference{h.publication.entry.Reference, h.settlement} {
		if ref.Partition != owner.Partition || ref.RequestID != owner.RequestID {
			return "", "", ErrInvalidRecord
		}
		if _, _, err := b.stageKeys(ref); err != nil {
			return "", "", err
		}
	}
	returnKey, err := b.namespace.coldRuntimeQuotaReturnKey(owner.Partition, owner.IntentID)
	if err != nil {
		return "", "", err
	}
	journalKey, err := b.namespace.coldRuntimeQuotaAttemptKey(owner.Partition, owner.IntentID)
	if err != nil {
		return "", "", err
	}
	return returnKey, journalKey, nil
}

// validateColdQuotaWorkerRecoveryMembers snapshots original registry liveness
// under its sole owning mutex. It never reads either bucket. Actual paid
// admission atomically rechecks lifetime and owns native call registration;
// no lifecycle lock spans decoding, an RPC, or its cancellation join.
func (b *Backend) validateColdQuotaWorkerRecoveryMembers(session *WorkerSession, partition *WorkerPartitionClaim, ranges, claims *WorkerBudgetShare) error {
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerSession(session) || !b.validWorkerPartition(partition) || !b.validWorkerBudget(ranges) || !b.validWorkerBudget(claims) || partition.session != session || ranges.session != session || claims.session != session || ranges.kind != WorkerBudgetRange || claims.kind != WorkerBudgetClaim {
		return ErrInvalidRecord
	}
	if session.lost || session.released || session.release != nil || partition.lost || partition.released || partition.release != nil || ranges.lost || ranges.released || ranges.release != nil || claims.lost || claims.released || claims.release != nil || session.reference.LeaseID <= 0 || !time.Now().Before(session.deadline) {
		return ErrGuardExpired
	}
	return session.ctx.Err()
}
