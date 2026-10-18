package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync/atomic"

	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

type coldQuotaWorkerPreparePhase uint8

const (
	coldQuotaWorkerPrepareWantReady coldQuotaWorkerPreparePhase = iota
	coldQuotaWorkerPrepareWantSlot
	coldQuotaWorkerPrepareDone
)

// coldQuotaWorkerPrepare resumes only fresh preparation's two bounded reads.
// The caller must first authenticate history and complete ordinary recovery;
// this cursor supplies no recovery, physical-release or mutation authority.
// History is borrowed from its original shared owner and must remain immutable
// until this cursor and the resulting preparation are no longer in use.
type coldQuotaWorkerPrepare struct {
	self         *coldQuotaWorkerPrepare
	backend      *Backend
	owner        *runtimeEnrolledPublicationOwner
	ownerContext context.Context
	history      *coldQuotaReturnHistory

	session   *WorkerSession
	partition *WorkerPartitionClaim
	ranges    *WorkerBudgetShare
	task      TaskReference
	taskBirth int64
	slotOwner ColdSlotOwner
	quota     ColdSlotReference
	readyKey  string
	slotKey   string
	freeKey   string
	returnKey string

	// Only the busy winner owns phase and partial publication. It holds that
	// ownership through actual native return and typed validation, even when
	// cancellation has already fired. No lock spans native work or hashing.
	busy      uint32
	phase     coldQuotaWorkerPreparePhase
	ready     *mvccpb.KeyValue
	prepared  *coldQuotaReturnPrepared
	reference StageReference
}

// newColdQuotaWorkerPrepare binds one original authenticated history without
// performing a read, charging credit or allocating an attempt. Return and slot
// keys are derived here rather than accepting a caller's alternate address.
// The exact enrolled owner remains registered through subsequent continuation.
func (b *Backend) newColdQuotaWorkerPrepare(o *runtimeEnrolledPublicationOwner, history *coldQuotaReturnHistory, session *WorkerSession, partition *WorkerPartitionClaim, ranges *WorkerBudgetShare) (*coldQuotaWorkerPrepare, error) {
	ctx, err := b.validateColdQuotaWorkerPrepareInput(o, history, session, partition, ranges)
	if err != nil {
		return nil, err
	}
	binding, err := coldQuotaReturnRelease(history)
	if err != nil {
		return nil, err
	}
	key, err := b.namespace.coldRuntimeQuotaReturnKey(history.reservation.Owner.Partition, history.reservation.Owner.IntentID)
	if err != nil {
		return nil, err
	}
	slot, free, err := b.namespace.coldSlotKeys(history.reservation.Quota)
	if err != nil {
		return nil, err
	}
	ready, err := b.namespace.Key("preparation", "inventory", "ready")
	if err != nil {
		return nil, err
	}

	state := &coldQuotaWorkerPrepare{
		backend: b, owner: o, ownerContext: ctx, history: history,
		session: session, partition: partition, ranges: ranges,
		task: binding.task, taskBirth: binding.birth,
		slotOwner: history.reservation.Owner, quota: history.reservation.Quota,
		readyKey: ready, slotKey: slot, freeKey: free, returnKey: key,
	}
	state.self = state
	return state, nil
}

// stepColdQuotaWorkerPrepare executes at most one Range-only W20 read. P1
// saves the exact validated ready KV and yields; P2 reads the original slot and
// free marker together, then freezes one ordinary preparation. Credit refusal
// or typed failure retains the outstanding phase and never rereads paid P1.
// Done returns that same unused pointer after validating the original bindings;
// a consumed continuation or attempted journal cannot be resent through done.
func (b *Backend) stepColdQuotaWorkerPrepare(o *runtimeEnrolledPublicationOwner, history *coldQuotaReturnHistory, state *coldQuotaWorkerPrepare, session *WorkerSession, partition *WorkerPartitionClaim, ranges *WorkerBudgetShare) (*coldQuotaReturnPrepared, error) {
	if state == nil || state.self != state || state.backend != b {
		return nil, ErrInvalidRecord
	}
	if !atomic.CompareAndSwapUint32(&state.busy, 0, 1) {
		return nil, ErrConflict
	}
	defer atomic.StoreUint32(&state.busy, 0)

	if err := b.validateColdQuotaWorkerPrepareState(o, history, state, session, partition, ranges); err != nil {
		return nil, err
	}

	switch state.phase {
	case coldQuotaWorkerPrepareWantReady:
		points, err := b.readColdQuotaWorkerPoints(o, session, partition, ranges, []string{state.readyKey}, []int{16384})
		if err != nil {
			return nil, err
		}
		if err := b.decodeColdReady(points[0]); err != nil {
			return nil, err
		}
		if err := b.validateColdQuotaWorkerPrepareState(o, history, state, session, partition, ranges); err != nil {
			return nil, err
		}
		state.ready = points[0]
		state.phase = coldQuotaWorkerPrepareWantSlot
		return nil, nil

	case coldQuotaWorkerPrepareWantSlot:
		points, err := b.readColdQuotaWorkerPoints(o, session, partition, ranges, []string{state.slotKey, state.freeKey}, []int{16384, 16384})
		if err != nil {
			return nil, err
		}
		if err := b.validateColdQuotaReturnSlot(history, points[0], points[1]); err != nil {
			return nil, err
		}
		if err := b.validateColdQuotaWorkerPrepareState(o, history, state, session, partition, ranges); err != nil {
			return nil, err
		}

		// A UUID appears only after both original business reads passed and
		// their original owner/context bindings survived actual native return.
		locator := StageAttemptLocator{Namespace: b.namespace.Root(), Partition: state.slotOwner.Partition, RequestID: state.slotOwner.RequestID, StageID: "cold_runtime_quota_return", AttemptID: uuid.NewString(), RestoreEpoch: b.restoreEpoch}
		prepared, _, err := b.freezeColdQuotaReturnPrepared(o, history, state.ready, points[0], points[1], state.returnKey, locator)
		if err != nil {
			return nil, err
		}
		if err := b.validateColdQuotaWorkerPrepareState(o, history, state, session, partition, ranges); err != nil {
			return nil, err
		}
		state.prepared, state.reference = prepared, prepared.reference
		state.ready = nil // The prepared mutation now owns the copied ready comparisons.
		state.phase = coldQuotaWorkerPrepareDone
		return prepared, nil

	case coldQuotaWorkerPrepareDone:
		return state.prepared, nil

	default:
		return nil, ErrInvalidRecord
	}
}

// validateColdQuotaWorkerPrepareInput checks only original local membership
// and authenticated-history shape. It never authenticates a replacement graph,
// reads credit, or infers physical death from a diagnostic Task/reference. The
// later driver must separately bind that history to its registered job origin.
func (b *Backend) validateColdQuotaWorkerPrepareInput(o *runtimeEnrolledPublicationOwner, history *coldQuotaReturnHistory, session *WorkerSession, partition *WorkerPartitionClaim, ranges *WorkerBudgetShare) (context.Context, error) {
	if !b.validWorkerBackend() {
		return nil, ErrInvalidRecord
	}
	ctx, err := b.coldQuotaReturnOwnerContext(o)
	if err != nil {
		return nil, err
	}
	if err := b.validateColdQuotaWorkerPostreadMembers(session, partition, ranges); err != nil {
		return nil, err
	}
	if _, _, err := b.coldProfile(); err != nil {
		return nil, err
	}
	if history == nil || history.publication == nil || history.publication.entry == nil {
		return nil, ErrInvalidRecord
	}
	binding, err := coldQuotaReturnRelease(history)
	if err != nil {
		return nil, ErrInvalidRecord
	}
	task, owner := binding.task, history.reservation.Owner
	// Quota allocation has its own partition; the original quota may legally
	// differ from the task/worker owner partition. Its exact reference and slot
	// keys remain sealed separately rather than equating those two domains.
	if b.validateTaskReference(task) != nil || partition.partition != owner.Partition || task.Partition != owner.Partition || task.SandboxID != owner.SandboxID {
		return nil, ErrInvalidRecord
	}
	for _, point := range []*mvccpb.KeyValue{history.publication.journal, history.publication.receipt, history.reservedKV, history.workKV, history.settledKV, binding.kv} {
		if point == nil || point.ModRevision <= 0 {
			return nil, ErrInvalidRecord
		}
	}
	return ctx, nil
}

// validateColdQuotaWorkerPrepareState preserves constructor seals before and
// after each read, including cached done. A live replacement context/member or
// copied history cannot extend this cursor's original ownership. The compact
// attribution seals detect changes without cloning authenticated source data.
func (b *Backend) validateColdQuotaWorkerPrepareState(o *runtimeEnrolledPublicationOwner, history *coldQuotaReturnHistory, state *coldQuotaWorkerPrepare, session *WorkerSession, partition *WorkerPartitionClaim, ranges *WorkerBudgetShare) error {
	if state.self != state || state.backend != b || state.owner != o || state.history != history || state.session != session || state.partition != partition || state.ranges != ranges {
		return ErrInvalidRecord
	}
	ctx, err := b.validateColdQuotaWorkerPrepareInput(o, history, session, partition, ranges)
	if err != nil {
		return err
	}
	binding, err := coldQuotaReturnRelease(history)
	if err != nil {
		return err
	}
	if ctx != state.ownerContext || binding.task != state.task || binding.birth != state.taskBirth || history.reservation.Owner != state.slotOwner || history.reservation.Quota != state.quota {
		return ErrInvalidRecord
	}
	key, err := b.namespace.coldRuntimeQuotaReturnKey(state.slotOwner.Partition, state.slotOwner.IntentID)
	if err != nil || key != state.returnKey {
		return ErrInvalidRecord
	}
	slot, free, err := b.namespace.coldSlotKeys(history.reservation.Quota)
	if err != nil || slot != state.slotKey || free != state.freeKey {
		return ErrInvalidRecord
	}
	ready, err := b.namespace.Key("preparation", "inventory", "ready")
	if err != nil || ready != state.readyKey {
		return ErrInvalidRecord
	}
	if state.phase == coldQuotaWorkerPrepareWantSlot {
		if err := b.decodeColdReady(state.ready); err != nil {
			return err
		}
	}
	if state.phase == coldQuotaWorkerPrepareDone {
		return b.validateColdQuotaWorkerPrepareDone(o, history, state)
	}
	return nil
}

// validateColdQuotaWorkerPrepareDone checks the single retained preparation
// without copying its mutation or reopening its one-shot journal/continuation.
// Hashing the original frozen JSON detects mutation while preserving its exact
// UUID, digest, history pointer and consumed-state ownership.
func (b *Backend) validateColdQuotaWorkerPrepareDone(o *runtimeEnrolledPublicationOwner, history *coldQuotaReturnHistory, state *coldQuotaWorkerPrepare) error {
	prepared := state.prepared
	if prepared == nil || prepared.self != prepared || prepared.backend != b || prepared.owner != o || prepared.history != history || prepared.reference != state.reference || prepared.returnKey != state.returnKey {
		return ErrInvalidRecord
	}
	if atomic.LoadUint32(&prepared.consumed) != 0 || atomic.LoadUint32(&prepared.journalAttempted) != 0 {
		return ErrConflict
	}
	encoded, err := json.Marshal(prepared.mutation)
	if err != nil || len(encoded)+16*maxKeyBytes > maxMutationBytes {
		return ErrInvalidMutation
	}
	digest := sha256.Sum256(encoded)
	if hex.EncodeToString(digest[:]) != state.reference.Digest {
		return ErrInvalidMutation
	}
	return nil
}
