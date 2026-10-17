package etcd

import (
	"bytes"
	"fmt"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// This owned native entry is historical attribution, not a transferable job,
// Claim, Kernel or permission to skip a fresh Run's close/drain prerequisites.
type cleanupDetachedNoEffectEntry struct {
	origin *Backend
	self   *cleanupDetachedNoEffectEntry
	record p.DetachedNoEffectSettlement
	kv     *mvccpb.KeyValue
}

type cleanupDetachedNoEffectResult struct {
	Outcome Outcome
	Entry   *cleanupDetachedNoEffectEntry
}

func (n Namespace) taskDetachedNoEffectKey(ref TaskReference) (string, error) {
	if ref.Validate() != nil || ref.Namespace != n.Root() {
		return "", ErrInvalidRecord
	}
	return n.Key("p", fmt.Sprintf("%02x", ref.Partition), "tasks", ref.TaskID, "detached-noeffect")
}

func detachedNoEffectClaim(ref TaskClaimReference) p.DetachedNoEffectClaim {
	return p.DetachedNoEffectClaim{ClaimID: ref.ClaimID, WorkerID: ref.WorkerID, CreateRevision: ref.CreateRevision, LeaseID: ref.LeaseID}
}

func detachedNoEffectStamp(f taskFence) p.RecordStamp {
	return p.RecordStamp{CreateRevision: f.create, ModRevision: f.mod, SHA256: workspaceSyncIntentDigest(f.value)}
}

func detachedNoEffectStampMatches(stamp p.RecordStamp, f taskFence) bool {
	return stamp == detachedNoEffectStamp(f)
}

func (b *Backend) detachedNoEffectEntry(kv *mvccpb.KeyValue, revision int64, ref TaskReference) (*cleanupDetachedNoEffectEntry, error) {
	if kv == nil {
		return nil, nil
	}
	key, err := b.namespace.taskDetachedNoEffectKey(ref)
	if err != nil {
		return nil, err
	}
	if string(kv.Key) != key || kv.Lease != 0 || kv.Version != 1 || kv.CreateRevision <= 0 || kv.CreateRevision != kv.ModRevision || kv.ModRevision > revision {
		return nil, ErrCorruptRecord
	}
	r, err := p.DecodeDetachedNoEffectSettlement(kv.Value)
	if err != nil || r.QuiescenceContext.Namespace != ref.Namespace || r.QuiescenceContext.RestoreEpoch != ref.RestoreEpoch || r.QuiescenceContext.TaskID != ref.TaskID || r.QuiescenceContext.SandboxID != ref.SandboxID || r.Partition != ref.Partition || kv.CreateRevision <= r.Execution.ModRevision || kv.CreateRevision < r.EmptyRevision {
		return nil, ErrCorruptRecord
	}
	for _, stamp := range []p.RecordStamp{r.Task, r.Snapshot, r.CreationIntent, r.Publication, r.Quiescence, r.QuiescenceReceipt, r.QuiescenceReservation, r.FinalSync, r.Occupation} {
		if kv.CreateRevision <= stamp.ModRevision {
			return nil, ErrCorruptRecord
		}
	}
	owned := *kv
	owned.Key, owned.Value = bytes.Clone(kv.Key), bytes.Clone(kv.Value)
	entry := &cleanupDetachedNoEffectEntry{origin: b, record: r, kv: &owned}
	entry.self = entry
	return entry, nil
}

// The original E is retained inside an authenticated native settlement. These
// reconstructed envelopes validate its historical canonical schema only; they
// are never presented as current native KVs or used to mint positive Mod pins.
// E was already occupied, hence its original Version was greater than one;
// the existing decoder needs only that distinction, not the exact write count.
func (b *Backend) detachedNoEffectHistoricalOccupation(entry *cleanupDetachedNoEffectEntry, ref TaskReference) (*cleanupSlotState, error) {
	if entry == nil || entry.self != entry || entry.origin != b || entry.kv == nil {
		return nil, ErrInvalidRecord
	}
	r := entry.record
	eKey, err := b.namespace.cleanupExecutionKey(ref)
	if err != nil {
		return nil, err
	}
	sKey, err := b.namespace.cleanupExecutionSlotKey(r.Slot)
	if err != nil {
		return nil, err
	}
	e := &mvccpb.KeyValue{Key: []byte(eKey), Value: bytes.Clone(r.ExecutionValue), CreateRevision: r.Execution.CreateRevision, ModRevision: r.Execution.ModRevision, Version: 2}
	var execution cleanupExecutionRecord
	if err = decodeCleanupExecution(e, entry.kv.CreateRevision, ref, &execution); err != nil {
		return nil, err
	}
	sValue, err := encodeCleanupExecutionSlot(cleanupSlotRecordFromExecution(execution))
	if err != nil {
		return nil, err
	}
	s := &mvccpb.KeyValue{Key: []byte(sKey), Value: []byte(sValue), CreateRevision: r.Occupation.CreateRevision, ModRevision: r.Occupation.ModRevision, Version: 1}
	if workspaceSyncIntentDigest(sValue) != r.Occupation.SHA256 {
		return nil, ErrCorruptRecord
	}
	state, err := b.cleanupStateFromPoints(ref, e, s, entry.kv.CreateRevision)
	if err != nil {
		return nil, err
	}
	worker, err := detachedNoEffectWorkerDigest(state.record)
	want := cleanupExecutionPending{Purpose: "detached-noeffect", ClaimID: r.WriterClaim.ClaimID, LeaseID: r.WriterClaim.LeaseID, AttemptID: r.AttemptID, Digest: r.BindingDigest}
	if err != nil || state.record.Phase != "occupied" || state.record.Confirmed != "drained" || state.record.TaskBirth != r.TaskBirth || state.record.Incarnation != r.Incarnation || state.record.Slot != r.Slot || state.record.Run == nil || state.record.Run.Joined || state.record.Run.ID != r.RunID || worker != r.WorkerDigest || state.record.Pending == nil || *state.record.Pending != want || state.record.Archive != nil || state.record.Release != nil {
		return nil, ErrCorruptRecord
	}
	return state, nil
}

// A present fact belongs to its historical writer, not necessarily today's
// Run/Claim. Authenticate its original E/Pending and permanent sources, then
// permit a later legitimate current E of the same immutable occupation. Only
// absence recovery may rely on a current actual Joined marker; this validator
// grants neither a new Run nor effect authority from a copied history tuple.
func (b *Backend) validateCleanupDetachedNoEffectFact(h *cleanupDetachedNoEffectHistory, entry *cleanupDetachedNoEffectEntry) error {
	if h == nil || h.state == nil {
		return ErrInvalidRecord
	}
	old, err := b.validateCleanupDetachedNoEffectSourceBindings(h, entry)
	if err != nil {
		return err
	}
	if h.state.execution.create != old.execution.create || h.state.record.Incarnation != old.record.Incarnation || h.state.record.TaskBirth != old.record.TaskBirth || h.state.execution.mod < old.execution.mod || h.state.execution.mod == old.execution.mod && h.state.execution != old.execution {
		return ErrConflict
	}
	if h.state.slot != nil && (old.slot == nil || *h.state.slot != *old.slot) {
		return ErrConflict
	}
	return nil
}

// Source attribution is independent of a current E/S: those may be advanced
// or retired after legitimate termination/release. It validates only the
// original E sealed in the fact and actual permanent source bindings. The live
// wrapper separately preserves all original current-occupation fences above.
func (b *Backend) validateCleanupDetachedNoEffectSourceBindings(h *cleanupDetachedNoEffectHistory, entry *cleanupDetachedNoEffectEntry) (*cleanupSlotState, error) {
	if b == nil || b.detachedCleanup == nil || h == nil || h.self != h || h.origin != b || h.quiescence == nil || h.quiescence.Record == nil || h.quiescence.reservation == nil || h.quiescence.reservation.Record == nil || h.finalSync == nil || h.finalSync.Record == nil || entry == nil || entry.self != entry || entry.origin != b {
		return nil, ErrInvalidRecord
	}
	old, err := b.detachedNoEffectHistoricalOccupation(entry, h.task.Reference)
	if err != nil {
		return nil, err
	}
	r := entry.record
	if r.QuiescenceContext != h.quiescence.Record.Context.Current || r.CreationIntentID != h.task.CreationIntentID || r.ContractDigest != b.detachedCleanup.contractDigest || r.FinalSyncClaim != detachedNoEffectClaim(h.finalSync.Record.Claim) || r.FinalSyncAttemptID != h.finalSync.Record.AttemptID || r.ReceiptDigest != h.finalSync.Record.ReceiptDigest {
		return nil, ErrCorruptRecord
	}
	for _, binding := range []struct {
		stamp p.RecordStamp
		fence taskFence
	}{{r.Task, h.taskFence}, {r.Snapshot, h.snapshotFence}, {r.CreationIntent, h.intentFence}, {r.Publication, h.publicationFence}, {r.FinalSync, h.finalSyncFence}, {r.Quiescence, h.quiescencePin.intent}, {r.QuiescenceReceipt, h.quiescencePin.receipt}} {
		if !detachedNoEffectStampMatches(binding.stamp, binding.fence) {
			return nil, ErrConflict
		}
	}
	reservation, err := encodeTaskQuiescenceAttempt(*h.quiescence.reservation.Record)
	if err != nil {
		return nil, err
	}
	reservationStamp := p.RecordStamp{CreateRevision: h.quiescence.reservation.Revision, ModRevision: h.quiescence.reservation.Revision, SHA256: workspaceSyncIntentDigest(reservation)}
	stage := h.quiescence.Reference.Stage
	stageBinding := p.TerminationQuiescenceStage{RequestID: stage.RequestID, StageID: stage.StageID, AttemptID: stage.AttemptID, Digest: stage.Digest}
	if r.QuiescenceReservation != reservationStamp || r.QuiescenceStage != stageBinding {
		return nil, ErrCorruptRecord
	}
	return old, nil
}
