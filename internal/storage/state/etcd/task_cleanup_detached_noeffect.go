package etcd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Only the registered physical owner accesses this original submission. The
// source graph is discarded after preparation: six compact native key/stamp
// pins, the original prepared drain and exact immutable wire survive Unknown.
// No copied job/draft/Claim can reconstruct this in-process authority.
type cleanupDetachedNoEffectDraft struct {
	origin   *Backend
	self     *cleanupDetachedNoEffectDraft
	job      *cleanupLocalJob
	claim    *TaskClaim
	prepared *PreparedTaskQuiescence
	state    *cleanupSlotState
	pins     [6]cleanupDetachedNoEffectPin
	record   p.DetachedNoEffectSettlement
	value    string
	deadline time.Time

	writeAttempted bool
	lost           bool
	revision       int64
}

type cleanupDetachedNoEffectPin struct {
	key   string
	stamp p.RecordStamp
}

func detachedNoEffectWorkerDigest(r cleanupExecutionRecord) (string, error) {
	if r.Run == nil || r.Run.Worker.Validate() != nil {
		return "", ErrInvalidRecord
	}
	wire, err := json.Marshal(r.Run.Worker)
	if err != nil {
		return "", err
	}
	hash := framedDigest("sandbox.detached-noeffect-worker.v1", string(wire))
	return hex.EncodeToString(hash[:]), nil
}

// Registry membership is checked before any native access. The genuine Kernel
// and Claim remain separate prerequisites: copied worker diagnostics and an
// E.Run read cannot supply either original pointer. This private metadata writer
// does not claim managed inventory/admission provenance for a public ClaimTask.
func (b *Backend) validateCleanupDetachedNoEffectJob(j *cleanupLocalJob) error {
	if b == nil || b.archiveLife == nil || j == nil || j.worker == nil || j.worker.backend != b || j.worker.self != j.worker || j.ctx == nil || !j.started || j.owner != nil || j.runID == "" || j.state == nil || j.state.record.Task != j.task || b.localCleanup == nil || b.localCleanup.Worker == nil {
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
	worker, err := b.localCleanup.Worker.WorkspaceSyncWorkerReference()
	if err != nil || j.state.record.Run == nil || j.state.record.Run.ID != j.runID || j.state.record.Run.Worker != worker {
		return errors.Join(ErrInvalidRecord, err)
	}
	return nil
}

// Caller holds c.mu and has validated the original registry job/Kernel outside
// that lock. Runtime release takes lifecycle before Claim, so this check must
// never reacquire archiveLife.mu. The original physical owner alone changes its
// job between accepted actor continuations; cancellation still invalidates c.
func (b *Backend) detachedNoEffectLive(ctx context.Context, j *cleanupLocalJob, c *TaskClaim, prepared *PreparedTaskQuiescence) error {
	if !b.validTaskClaim(c) || j == nil || j.state == nil || j.state.record.Run == nil || j.claim != c || c.parentCtx != j.ctx || prepared == nil || prepared.self != prepared || prepared.origin != b || prepared.claim != c || prepared.claimReference != c.reference || j.pendingProgress != nil || j.state.slot == nil || j.state.record.Phase != "occupied" || j.state.record.Confirmed != "drained" || j.state.record.Run.Joined || j.state.record.Archive != nil || j.state.record.Release != nil || b.validateCleanupState(j.state) != nil {
		return ErrInvalidRecord
	}
	if err := c.live(ctx); err != nil {
		return err
	}
	if !time.Now().Before(prepared.deadline) {
		return ErrGuardExpired
	}
	pending := j.state.record.Pending
	if pending == nil || pending.ClaimID != c.reference.ClaimID || pending.LeaseID != c.reference.LeaseID {
		return ErrConflict
	}
	// Only the empty final-sync handoff may start a settlement. A matching
	// Claim does not permit overwriting archive/termination or a foreign Stage.
	if pending.Purpose == "final-sync" && pending.StageID == "" && pending.RequestID == "" && pending.AttemptID == "" && pending.Digest == "" {
		return nil
	}
	d := j.detachedNoEffect
	if pending.Purpose == "detached-noeffect" && d != nil && d.self == d && d.origin == b && d.job == j && d.claim == c && d.prepared == prepared && !d.lost && pending.StageID == "" && pending.RequestID == "" && pending.AttemptID == d.record.AttemptID && pending.Digest == d.record.BindingDigest {
		return nil
	}
	return ErrConflict
}

// prepareCleanupDetachedNoEffect owns one original attempt through Pending's
// native ACK. It freshly revalidates dual drain, authenticates the plain source
// and old none history, then captures E's post-progress native Mod. None remains
// a plan: only this separate settlement writer may create the new fact.
func (b *Backend) prepareCleanupDetachedNoEffect(ctx context.Context, j *cleanupLocalJob, prepared *PreparedTaskQuiescence, destination *ct.Destination) (*cleanupDetachedNoEffectDraft, error) {
	if b == nil || b.detachedCleanup == nil || ctx == nil || j == nil || j.claim == nil {
		return nil, ErrInvalidRecord
	}
	if err := b.validateCleanupDetachedNoEffectJob(j); err != nil {
		return nil, err
	}
	c := j.claim
	c.mu.Lock()
	err := b.detachedNoEffectLive(ctx, j, c, prepared)
	if err == nil && j.detachedNoEffect != nil {
		d := j.detachedNoEffect
		if d.self != d || d.origin != b || d.job != j || d.claim != c || d.prepared != prepared || d.lost || d.value == "" {
			err = ErrConflict
		} else {
			c.mu.Unlock()
			return d, nil
		}
	}
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if err = b.RevalidateTaskQuiescence(ctx, c, prepared, destination); err != nil {
		return nil, err
	}
	// A replacement Run may use the authentic old none plan, but must never
	// reserve over its permanent key with a new Claim or attempt.
	finalSync, err := b.LoadTaskFinalSyncAttempt(ctx, j.task)
	if err != nil {
		return nil, err
	}
	if finalSync == nil {
		plan := TaskFinalSyncPlan{Mode: "none", Digest: b.detachedCleanup.contractDigest}
		result, err := b.ReserveTaskFinalSync(ctx, c, prepared, destination, plan)
		if err != nil || result.Outcome != OutcomeCommitted {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
	}
	history, err := b.readCleanupDetachedNoEffectHistory(ctx, j.task)
	if err != nil {
		return nil, err
	}
	if history.settlement != nil || history.state == nil || history.state.slot == nil || history.state.execution != j.state.execution || *history.state.slot != *j.state.slot || history.quiescencePin != prepared.pin || history.task != prepared.pin.task || history.finalSync.Record.ReceiptDigest != digestTaskQuiescenceProof(prepared.receipt.Wire()) || history.finalSync.Record.Claim.CreateRevision > c.reference.CreateRevision || history.finalSync.Record.Claim.CreateRevision == c.reference.CreateRevision && history.finalSync.Record.Claim != c.reference {
		return nil, ErrConflict
	}
	if err = b.validateCleanupDetachedNoEffectJob(j); err != nil {
		return nil, err
	}
	c.mu.Lock()
	err = b.detachedNoEffectLive(ctx, j, c, prepared)
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	r, err := b.cleanupDetachedNoEffectRecord(history, j, prepared)
	if err != nil {
		return nil, err
	}
	d := &cleanupDetachedNoEffectDraft{origin: b, job: j, claim: c, prepared: prepared, record: r, deadline: prepared.deadline}
	d.self = d
	// Publish the one candidate before its first progress dispatch. An unknown
	// progress ACK retains this same attempt; prepare cannot submit another.
	j.detachedNoEffect = d
	pending := &cleanupExecutionPending{Purpose: "detached-noeffect", ClaimID: c.reference.ClaimID, LeaseID: c.reference.LeaseID, AttemptID: r.AttemptID, Digest: r.BindingDigest}
	prior := j.state
	if err = j.progress("drained", pending, nil); err != nil {
		return nil, err
	}
	if j.state == nil || j.state.slot == nil || j.state.execution.create != prior.execution.create || j.state.execution.mod <= prior.execution.mod || *j.state.slot != *prior.slot || j.state.record.Run == nil || *j.state.record.Run != *prior.record.Run || j.state.record.Pending == nil || *j.state.record.Pending != *pending {
		return nil, ErrConflict
	}
	d.state = j.state
	d.record.Execution = detachedNoEffectStamp(j.state.execution)
	d.record.ExecutionValue = []byte(j.state.execution.value)
	wire, err := p.EncodeDetachedNoEffectSettlement(d.record)
	if err != nil {
		return nil, err
	}
	d.value = string(wire)
	for i, fence := range []taskFence{j.state.execution, *j.state.slot, history.finalSyncFence, history.snapshotFence, history.intentFence, history.publicationFence} {
		d.pins[i] = cleanupDetachedNoEffectPin{key: fence.key, stamp: detachedNoEffectStamp(fence)}
	}
	return d, nil
}

func (b *Backend) cleanupDetachedNoEffectRecord(h *cleanupDetachedNoEffectHistory, j *cleanupLocalJob, prepared *PreparedTaskQuiescence) (p.DetachedNoEffectSettlement, error) {
	if h == nil || h.self != h || h.origin != b || h.state == nil || h.state.slot == nil || h.quiescence == nil || h.quiescence.Record == nil || h.quiescence.reservation == nil || h.quiescence.reservation.Record == nil || h.finalSync == nil || h.finalSync.Record == nil {
		return p.DetachedNoEffectSettlement{}, ErrInvalidRecord
	}
	workerDigest, err := detachedNoEffectWorkerDigest(h.state.record)
	if err != nil {
		return p.DetachedNoEffectSettlement{}, err
	}
	reservation, err := encodeTaskQuiescenceAttempt(*h.quiescence.reservation.Record)
	if err != nil {
		return p.DetachedNoEffectSettlement{}, err
	}
	q := h.quiescencePin
	r := p.DetachedNoEffectSettlement{
		Version: 1, Domain: "sandbox.detached-noeffect.v1", Profile: "detached_enrolled_plain",
		QuiescenceContext: h.quiescence.Record.Context.Current, Partition: j.task.Partition, TaskBirth: h.state.record.TaskBirth,
		CreationIntentID: h.task.CreationIntentID, Incarnation: h.state.record.Incarnation, RunID: j.runID, Slot: h.state.record.Slot, WorkerDigest: workerDigest,
		WriterClaim: detachedNoEffectClaim(j.claim.reference), FinalSyncClaim: detachedNoEffectClaim(h.finalSync.Record.Claim),
		FinalSyncAttemptID: h.finalSync.Record.AttemptID, AttemptID: uuid.NewString(), ContractDigest: b.detachedCleanup.contractDigest,
		ReceiptDigest: digestTaskQuiescenceProof(prepared.receipt.Wire()), EmptyRevision: prepared.emptyRevision,
		Task: detachedNoEffectStamp(h.taskFence), Snapshot: detachedNoEffectStamp(h.snapshotFence), CreationIntent: detachedNoEffectStamp(h.intentFence), Publication: detachedNoEffectStamp(h.publicationFence),
		Quiescence: detachedNoEffectStamp(q.intent), QuiescenceReceipt: detachedNoEffectStamp(q.receipt),
		QuiescenceReservation: p.RecordStamp{CreateRevision: h.quiescence.reservation.Revision, ModRevision: h.quiescence.reservation.Revision, SHA256: workspaceSyncIntentDigest(reservation)},
		FinalSync:             detachedNoEffectStamp(h.finalSyncFence), Occupation: detachedNoEffectStamp(*h.state.slot),
		QuiescenceStage: p.TerminationQuiescenceStage{RequestID: q.reference.Stage.RequestID, StageID: q.reference.Stage.StageID, AttemptID: q.reference.Stage.AttemptID, Digest: q.reference.Stage.Digest},
	}
	r.BindingDigest, err = p.DetachedNoEffectBindingDigest(r)
	return r, err
}

func (b *Backend) validateCleanupDetachedNoEffectDraft(j *cleanupLocalJob, d *cleanupDetachedNoEffectDraft) error {
	if d == nil || d.self != d || d.origin != b || d.job != j || j == nil || j.detachedNoEffect != d || !b.validTaskClaim(d.claim) || d.prepared == nil || d.prepared.self != d.prepared || d.prepared.claim != d.claim || d.value == "" || d.lost {
		return ErrInvalidRecord
	}
	return b.validateCleanupDetachedNoEffectJob(j)
}

// commitCleanupDetachedNoEffect has exactly one native Put opportunity. The
// irrevocable latch precedes send, including cancellation/transport loss; every
// later invocation only queries this original draft and cannot create a UUID.
func (b *Backend) commitCleanupDetachedNoEffect(ctx context.Context, j *cleanupLocalJob, d *cleanupDetachedNoEffectDraft) (cleanupDetachedNoEffectResult, error) {
	result := cleanupDetachedNoEffectResult{Outcome: OutcomeUnknown}
	if ctx == nil {
		return result, ErrInvalidRecord
	}
	if err := b.validateCleanupDetachedNoEffectDraft(j, d); err != nil {
		return result, err
	}
	if d.writeAttempted {
		return b.arbitrateCleanupDetachedNoEffect(ctx, j, d)
	}
	c := d.claim
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := b.detachedNoEffectLive(ctx, j, c, d.prepared); err != nil {
		return result, err
	}
	if j.state != d.state || !detachedNoEffectStampMatches(d.record.Execution, j.state.execution) || j.state.record.Pending == nil || j.state.record.Pending.AttemptID != d.record.AttemptID || j.state.record.Pending.Digest != d.record.BindingDigest {
		return result, ErrConflict
	}
	key, err := b.namespace.taskDetachedNoEffectKey(j.task)
	if err != nil {
		return result, err
	}
	cmps := taskEmptyComparisons(b, c, d.prepared.pin.intent, d.prepared.pin.receipt, d.prepared.pin.reservation)
	for _, pin := range d.pins {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(pin.key), "=", pin.stamp.ModRevision))
	}
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	if _, err = b.detachedNoEffectWriteProjection(cmps, key, d.value); err != nil {
		return result, err
	}
	bounded, cancel := context.WithDeadline(ctx, d.deadline)
	defer cancel()
	rpc, finish := b.taskObservationContext(bounded, c)
	defer finish()
	if err = b.detachedNoEffectLive(rpc, j, c, d.prepared); err != nil {
		return result, err
	}
	d.writeAttempted = true
	response, err := b.client.Txn(rpc).If(cmps...).Then(clientv3.OpPut(key, d.value)).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey), clientv3.OpGet(key)).Commit()
	if err != nil || rpc.Err() != nil || b.operationResponseHeader(response) != nil {
		return result, errors.Join(ErrOutcomeUnknown, err, rpc.Err())
	}
	if (*pb.TxnResponse)(response).Size() > maxMutationBytes {
		return result, ErrOutcomeUnknown
	}
	if !response.Succeeded {
		if _, err = b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey, key}); err != nil {
			return result, err
		}
		return result, ErrConflict
	}
	if !operationPutResponses(response, 1) || response.Header.Revision < d.record.EmptyRevision {
		return result, ErrOutcomeUnknown
	}
	for _, pin := range d.pins {
		if response.Header.Revision <= pin.stamp.ModRevision {
			return result, ErrOutcomeUnknown
		}
	}
	d.revision = response.Header.Revision
	entry, err := b.detachedNoEffectEntry(&mvccpb.KeyValue{Key: []byte(key), Value: []byte(d.value), Lease: 0, Version: 1, CreateRevision: d.revision, ModRevision: d.revision}, d.revision, j.task)
	if err != nil {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	result.Outcome, result.Entry = OutcomeCommitted, entry
	return result, c.live(ctx)
}

// Arbitration is a ten-node read of identity/restore/original settlement. It
// intentionally omits live Claim comparisons: exact committed history remains
// diagnosable with a dead Claim, while absence independently remains Unknown.
func (b *Backend) arbitrateCleanupDetachedNoEffect(ctx context.Context, j *cleanupLocalJob, d *cleanupDetachedNoEffectDraft) (cleanupDetachedNoEffectResult, error) {
	result := cleanupDetachedNoEffectResult{Outcome: OutcomeUnknown}
	key, err := b.namespace.taskDetachedNoEffectKey(j.task)
	if err != nil {
		return result, err
	}
	points, revision, err := b.readCleanupDetachedNoEffectPoints(ctx, "arbitration", []string{b.identityKey, b.restoreKey, key}, []int{maxRecordBytes, maxRecordBytes, p.MaxDetachedNoEffectSettlementBytes})
	if err != nil {
		return result, err
	}
	// A different actual value conflicts with this original attempt before
	// its schema is interpreted. Exact bytes still require the strict native
	// envelope/codec checks below; foreign corruption cannot reopen the latch.
	if kv := points[2]; kv != nil && (string(kv.Value) != d.value || d.revision > 0 && kv.CreateRevision != d.revision) {
		d.lost = true
		return result, ErrConflict
	}
	entry, err := b.detachedNoEffectEntry(points[2], revision, j.task)
	if err != nil {
		return result, err
	}
	if entry == nil {
		return result, ErrOutcomeUnknown
	}
	d.revision = entry.kv.CreateRevision
	result.Outcome, result.Entry = OutcomeCommitted, entry
	d.claim.mu.Lock()
	defer d.claim.mu.Unlock()
	return result, d.claim.live(ctx)
}
