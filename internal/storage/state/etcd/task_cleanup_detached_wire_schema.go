package etcd

import (
	"encoding/json"
	"math"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// Each schema is encoded in a separate frame and immediately discarded.
// Unsigned tickets/proofs and future stamps are inert dimensions: no verifier,
// live authority, signer, provider or native API receives these model records.
func (d *cleanupDetachedWireBuilder) validateSchemas() error {
	if err := d.effectSchemas(); err != nil {
		return err
	}
	if err := d.providerSchema(); err != nil {
		return err
	}
	if _, err := d.settlementSchema(); err != nil {
		return err
	}
	r := d.terminationSchema()
	if _, err := p.EncodeTaskRuntimeTerminationIntent(r); err != nil {
		return err
	}
	r.Phase, r.TerminalProof, r.CleanupDigest = "cleanup-complete", make([]byte, 4096), cleanupWirePaddingDigest()
	if _, err := p.EncodeTaskRuntimeTerminationIntent(r); err != nil {
		return err
	}
	seal, err := newDetachedOwnerReleaseRecord(r, math.MaxInt64-3, math.MaxInt64-1, taskCloseClaimWire{d.w.claim.Task, d.w.claim.ClaimID, d.w.claim.WorkerID, d.w.claim.CreateRevision, d.w.claim.LeaseID})
	if err != nil {
		return err
	}
	_, err = encodeDetachedOwnerRelease(seal)
	return err
}

func (d *cleanupDetachedWireBuilder) effectSchemas() error {
	w := &d.w
	var certificate taskArchiveCertificateWire
	if err := json.Unmarshal(d.in.issuer.Record.Certificate, &certificate); err != nil {
		return err
	}
	before, after := certificate.Claims.NotBefore, certificate.Claims.NotBefore.Add(20*time.Second)
	context := w.closeContext()
	ticket, err := json.Marshal(taskCloseTicketWire{Claims: p.TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: context, NotBefore: before, NotAfter: after}, Signature: make([]byte, 64)})
	if err != nil {
		return err
	}
	closeAttempt := StageAttemptLocator{Namespace: w.closeStage.Namespace, RestoreEpoch: w.closeStage.RestoreEpoch, Partition: w.closeStage.Partition, RequestID: w.closeStage.RequestID, StageID: w.closeStage.StageID, AttemptID: w.closeStage.AttemptID}
	closed := TaskCloseDataRecord{Version: 1, Task: d.in.bundle.task, Claim: w.claim, Context: context, TicketDigest: workspaceSyncIntentDigest(string(ticket)), IssuerCertificateID: d.in.issuer.Record.CertificateID, IssuerCertificateDigest: d.in.issuer.Record.CertificateDigest, IssuerRevision: math.MaxInt64 - 30, Ticket: ticket, Attempt: closeAttempt}
	if _, err = encodeTaskCloseDataRecord(closed); err != nil {
		return err
	}
	receipt, err := json.Marshal(taskQuiescenceCloseReceiptWire{Claims: p.TaskDataClosedReceiptClaims{Version: 1, State: "data_closed", Context: context, TicketDigest: closed.TicketDigest, NotBefore: before, NotAfter: after}, Signature: make([]byte, 64)})
	if err != nil {
		return err
	}
	qcontext := p.TaskUserQuiescenceContext{Current: context, CloseDataContext: context, CloseDataTicketDigest: closed.TicketDigest, CloseDataReceiptDigest: workspaceSyncIntentDigest(string(receipt)), CloseDataIntentRevision: math.MaxInt64 - 14}
	qticket, err := json.Marshal(taskQuiescenceTicketWire{Claims: p.TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: qcontext, NotBefore: before, NotAfter: after}, Signature: make([]byte, 64)})
	if err != nil {
		return err
	}
	qAttempt := closeAttempt
	qAttempt.StageID = w.quiescenceStage.StageID
	q := TaskQuiescenceRecord{Version: 1, Task: d.in.bundle.task, Claim: w.claim, Context: qcontext, TicketDigest: workspaceSyncIntentDigest(string(qticket)), IssuerCertificateID: closed.IssuerCertificateID, IssuerCertificateDigest: closed.IssuerCertificateDigest, IssuerRevision: closed.IssuerRevision, Ticket: qticket, CloseDataReceipt: receipt, Attempt: qAttempt}
	if _, err = encodeTaskQuiescenceRecord(q); err != nil {
		return err
	}
	reservation := TaskQuiescenceAttemptRecord{Version: 1, Task: d.in.bundle.task, Claim: w.claim, DestroyingRevision: d.in.bundle.birth, CommandID: context.CommandID, Attempt: qAttempt}
	if _, err = encodeTaskQuiescenceAttempt(reservation); err != nil {
		return err
	}
	quiesced, err := json.Marshal(taskArchiveQuiescenceReceiptWire{Claims: p.TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: qcontext, TicketDigest: q.TicketDigest, NotBefore: before, NotAfter: after, RegisteredCount: 64, NeverSpawnedCount: 32, LocalTerminalCount: 32, ExecutionSetDigest: cleanupWirePaddingDigest()}, Signature: make([]byte, 64)})
	if err != nil {
		return err
	}
	final := TaskFinalSyncAttemptRecord{Version: 1, Task: d.in.bundle.task, Claim: w.claim, DestroyingRevision: d.in.bundle.birth, Quiescence: TaskQuiescenceReference{Task: w.claim.Task, CommandID: context.CommandID, Stage: w.quiescenceStage}, QuiescenceRevision: math.MaxInt64 - 12, ReceiptDigest: workspaceSyncIntentDigest(string(quiesced)), Plan: TaskFinalSyncPlan{Mode: "none", Digest: d.b.detachedCleanup.contractDigest}, AttemptID: w.dimensions.runID}
	_, err = encodeTaskFinalSyncAttempt(final)
	return err
}

func (d *cleanupDetachedWireBuilder) activeExecution() cleanupExecutionRecord {
	r := d.execution
	r.Phase, r.Confirmed = "occupied", "drained"
	r.Run = &cleanupExecutionRun{ID: d.w.dimensions.runID, Worker: d.w.dimensions.worker}
	r.Pending = &cleanupExecutionPending{Purpose: "detached-noeffect", ClaimID: d.w.claim.ClaimID, LeaseID: d.w.claim.LeaseID, AttemptID: d.w.dimensions.runID, Digest: cleanupWirePaddingDigest()}
	return r
}

func detachedWireStamp(revision int64) p.RecordStamp {
	return p.RecordStamp{CreateRevision: revision, ModRevision: revision, SHA256: cleanupWirePaddingDigest()}
}

func (d *cleanupDetachedWireBuilder) settlementSchema() ([]byte, error) {
	r := d.activeExecution()
	e, err := encodeCleanupExecution(r)
	if err != nil {
		return nil, err
	}
	worker, err := detachedNoEffectWorkerDigest(r)
	if err != nil {
		return nil, err
	}
	claim := detachedNoEffectClaim(d.w.claim)
	q := detachedWireStamp(math.MaxInt64 - 12)
	stamp := p.DetachedNoEffectSettlement{
		Version: 1, Domain: "sandbox.detached-noeffect.v1", Profile: "detached_enrolled_plain", QuiescenceContext: d.w.closeContext(), Partition: r.Task.Partition, TaskBirth: r.TaskBirth, CreationIntentID: d.in.bundle.task.CreationIntentID,
		Incarnation: r.Incarnation, RunID: r.Run.ID, Slot: r.Slot, WorkerDigest: worker, WriterClaim: claim, FinalSyncClaim: claim, FinalSyncAttemptID: d.w.dimensions.runID, AttemptID: d.w.dimensions.runID,
		ContractDigest: d.b.detachedCleanup.contractDigest, ReceiptDigest: cleanupWirePaddingDigest(), EmptyRevision: math.MaxInt64 - 5,
		Task: p.RecordStamp{CreateRevision: r.TaskBirth, ModRevision: r.TaskBirth, SHA256: r.TaskDigest}, Snapshot: detachedWireStamp(1), CreationIntent: detachedWireStamp(1), Publication: detachedWireStamp(1), Quiescence: q, QuiescenceReceipt: q,
		QuiescenceReservation: detachedWireStamp(math.MaxInt64 - 13), FinalSync: detachedWireStamp(math.MaxInt64 - 11), Execution: p.RecordStamp{CreateRevision: math.MaxInt64 - 16, ModRevision: math.MaxInt64 - 7, SHA256: workspaceSyncIntentDigest(e)}, Occupation: detachedWireStamp(math.MaxInt64 - 15),
		QuiescenceStage: p.TerminationQuiescenceStage{RequestID: d.w.quiescenceStage.RequestID, StageID: d.w.quiescenceStage.StageID, AttemptID: d.w.quiescenceStage.AttemptID, Digest: d.w.quiescenceStage.Digest}, ExecutionValue: []byte(e),
	}
	stamp.BindingDigest, err = p.DetachedNoEffectBindingDigest(stamp)
	if err != nil {
		return nil, err
	}
	r.Pending.Digest = stamp.BindingDigest
	e, err = encodeCleanupExecution(r)
	if err != nil {
		return nil, err
	}
	stamp.ExecutionValue, stamp.Execution.SHA256 = []byte(e), workspaceSyncIntentDigest(e)
	return p.EncodeDetachedNoEffectSettlement(stamp)
}

func (d *cleanupDetachedWireBuilder) terminationSchema() p.TaskRuntimeTerminationIntent {
	f, w := d.in.bundle.fences, &d.w
	publication := d.in.publication.Record
	q := detachedWireStamp(math.MaxInt64 - 12)
	return p.TaskRuntimeTerminationIntent{Version: 2, Phase: "reserved", Current: w.closeContext(), Partition: w.claim.Task.Partition, TaskBirth: d.in.bundle.birth, CreationIntentID: d.in.bundle.task.CreationIntentID,
		EnrollmentDigest: publication.EnrollmentDigest, ObserverCertificateDigest: publication.ObserverCertificateDigest, NodeUID: publication.NodeUID, NodeBootID: publication.NodeBootID, Action: "kill_pid1_and_observe_cgroup_empty",
		Authority:       p.TerminationAuthorityBindings{Task: localRuntimeFenceStamp(f[0]), CleanupIntent: localRuntimeFenceStamp(f[1]), TaskLink: localRuntimeFenceStamp(f[2]), Placement: localRuntimeFenceStamp(f[3]), Control: localRuntimeFenceStamp(f[4]), Owner: localRuntimeFenceStamp(f[5]), Fence: localRuntimeFenceStamp(f[6]), RuntimeIndex: localRuntimeFenceStamp(f[7]), Quiescence: q, QuiescenceReceipt: q, QuiescenceReservation: detachedWireStamp(math.MaxInt64 - 13), FinalSync: detachedWireStamp(math.MaxInt64 - 11), Issuer: detachedWireStamp(math.MaxInt64 - 30), QuiescenceStage: p.TerminationQuiescenceStage{RequestID: w.quiescenceStage.RequestID, StageID: w.quiescenceStage.StageID, AttemptID: w.quiescenceStage.AttemptID, Digest: w.quiescenceStage.Digest}},
		DetachedEffects: &p.DetachedRuntimeEffectBindings{Version: 1, Profile: "detached_enrolled_plain", ContractDigest: d.b.detachedCleanup.contractDigest, Settlement: detachedWireStamp(math.MaxInt64 - 4)},
	}
}
