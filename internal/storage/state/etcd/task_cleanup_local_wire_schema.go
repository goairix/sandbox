package etcd

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
)

// The original codecs validate each future schema before whole-record caps
// enter protobuf projections. Zero signatures are inert dimensional inputs:
// no verifier, signer, destination or Prepared effect consumes these records.
func (w *cleanupLocalWireBuilder) effectSchemas() error {
	var certificate taskArchiveCertificateWire
	if json.Unmarshal(w.in.issuer.Record.Certificate, &certificate) != nil {
		return fmt.Errorf("issuer certificate schema: %w", ErrInvalidRecord)
	}
	before := certificate.Claims.NotBefore
	after, session := before.Add(20*time.Second), before.Add(45*time.Second)
	if certificate.Claims.NotAfter.Before(session) {
		return fmt.Errorf("archive ticket session window: %w", ErrInvalidConfiguration)
	}

	context := w.closeContext()
	ticket, err := json.Marshal(taskCloseTicketWire{Claims: p.TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: context, NotBefore: before, NotAfter: after}, Signature: make([]byte, 64)})
	if err != nil {
		return fmt.Errorf("close ticket schema: %w", err)
	}
	closeAttempt := StageAttemptLocator{Namespace: w.closeStage.Namespace, RestoreEpoch: w.closeStage.RestoreEpoch, Partition: w.closeStage.Partition, RequestID: w.closeStage.RequestID, StageID: w.closeStage.StageID, AttemptID: w.closeStage.AttemptID}
	closeRecord := TaskCloseDataRecord{Version: 1, Task: w.in.bundle.task, Claim: w.claim, Context: context, TicketDigest: workspaceSyncIntentDigest(string(ticket)), IssuerCertificateID: w.in.issuer.Record.CertificateID, IssuerCertificateDigest: w.in.issuer.Record.CertificateDigest, IssuerRevision: w.in.issuer.Revision, Ticket: ticket, Attempt: closeAttempt}
	if _, err := encodeTaskCloseDataRecord(closeRecord); err != nil {
		return fmt.Errorf("close-commit record schema: %w", err)
	}

	closed, err := json.Marshal(taskQuiescenceCloseReceiptWire{Claims: p.TaskDataClosedReceiptClaims{Version: 1, State: "data_closed", Context: context, TicketDigest: closeRecord.TicketDigest, NotBefore: before, NotAfter: after}, Signature: make([]byte, 64)})
	if err != nil {
		return fmt.Errorf("close receipt schema: %w", err)
	}
	qc := p.TaskUserQuiescenceContext{Current: context, CloseDataContext: context, CloseDataTicketDigest: closeRecord.TicketDigest, CloseDataReceiptDigest: workspaceSyncIntentDigest(string(closed)), CloseDataIntentRevision: math.MaxInt64 - 7}
	qt, err := json.Marshal(taskQuiescenceTicketWire{Claims: p.TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: qc, NotBefore: before, NotAfter: after}, Signature: make([]byte, 64)})
	if err != nil {
		return fmt.Errorf("quiescence ticket schema: %w", err)
	}
	qAttempt := closeAttempt
	qAttempt.StageID = w.quiescenceStage.StageID
	q := TaskQuiescenceRecord{Version: 1, Task: w.in.bundle.task, Claim: w.claim, Context: qc, TicketDigest: workspaceSyncIntentDigest(string(qt)), IssuerCertificateID: w.in.issuer.Record.CertificateID, IssuerCertificateDigest: w.in.issuer.Record.CertificateDigest, IssuerRevision: w.in.issuer.Revision, Ticket: qt, CloseDataReceipt: closed, Attempt: qAttempt}
	if _, err := encodeTaskQuiescenceRecord(q); err != nil {
		return fmt.Errorf("quiesce-commit record schema: %w", err)
	}
	reservation := TaskQuiescenceAttemptRecord{Version: 1, Task: w.in.bundle.task, Claim: w.claim, DestroyingRevision: w.in.bundle.birth, CommandID: context.CommandID, Attempt: qAttempt}
	if _, err := encodeTaskQuiescenceAttempt(reservation); err != nil {
		return fmt.Errorf("quiescence reservation schema: %w", err)
	}

	quiesced, err := json.Marshal(taskArchiveQuiescenceReceiptWire{Claims: p.TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: qc, TicketDigest: q.TicketDigest, NotBefore: before, NotAfter: after, RegisteredCount: 64, NeverSpawnedCount: 32, LocalTerminalCount: 32, ExecutionSetDigest: cleanupWirePaddingDigest()}, Signature: make([]byte, 64)})
	if err != nil {
		return fmt.Errorf("quiescence receipt schema: %w", err)
	}

	// Archive is a distinct command in the original lineage validator. Flip a
	// UUID payload byte to retain its exact width and version while guaranteeing
	// that this inert projection differs from both earlier command IDs.
	archiveID, err := uuid.Parse(context.CommandID)
	if err != nil {
		return fmt.Errorf("archive command ID schema: %w", err)
	}
	archiveID[15] ^= 1
	archiveContext := context
	archiveContext.CommandID = archiveID.String()

	final := TaskFinalSyncAttemptRecord{Version: 1, Task: w.in.bundle.task, Claim: w.claim, DestroyingRevision: w.in.bundle.birth, Quiescence: TaskQuiescenceReference{Task: w.claim.Task, CommandID: context.CommandID, Stage: w.quiescenceStage}, QuiescenceRevision: math.MaxInt64 - 5, ReceiptDigest: workspaceSyncIntentDigest(string(quiesced)), Plan: TaskFinalSyncPlan{Mode: "sync", Digest: w.in.plan.digest}, AttemptID: archiveContext.CommandID}
	if _, err := encodeTaskFinalSyncAttempt(final); err != nil {
		return fmt.Errorf("final-sync reservation schema: %w", err)
	}
	claims := p.TaskWorkspaceArchiveTicketClaims{Version: 1, Purpose: "task_workspace_archive", Context: p.TaskWorkspaceArchiveContext{Current: archiveContext, QuiescenceContext: qc, QuiescenceTicketDigest: q.TicketDigest, QuiescenceReceiptDigest: final.ReceiptDigest, PlanDigest: w.in.plan.digest, QuiescenceIntentRevision: final.QuiescenceRevision, FinalSyncRevision: math.MaxInt64 - 4}, NotBefore: before, NotAfter: after, SessionNotAfter: session}
	archiveTicket, err := json.Marshal(taskArchiveTicketWire{Claims: claims, Signature: make([]byte, 64)})
	if err != nil {
		return fmt.Errorf("archive ticket schema: %w", err)
	}
	archive := taskArchiveCommandRecord{Version: 1, FinalSync: taskFinalSyncWire(final), FinalSyncRevision: claims.Context.FinalSyncRevision, Claims: claims, IssuerRevision: w.in.issuer.Revision, Certificate: w.in.issuer.Record.Certificate, Ticket: archiveTicket, QuiescenceReceipt: quiesced}
	if _, err := encodeTaskArchiveCommand(archive); err != nil {
		return fmt.Errorf("archive-write record schema: %w", err)
	}

	checkpoint := TaskCheckpointRecord{Version: 1, Reference: w.claim.Task, ClaimID: w.claim.ClaimID, DetailDigest: cleanupWirePaddingDigest(), State: TaskCheckpointNeedsReconciliation, Attempt: closeAttempt}
	checkpoint.Attempt.StageID = strings.Repeat("s", 128)
	if _, err := encodeTaskCheckpointRecord(checkpoint); err != nil {
		return fmt.Errorf("task-checkpoint v1 schema: %w", err)
	}

	// Recovery advances the existing v1 checkpoint to its fixed v2 schema.
	// Check both codecs; a whole4096-byte value alone cannot detect an invalid
	// prospective Task/attempt/claim combination.
	payload := TaskWorkspaceSyncCheckpoint{Version: 1, FinalSyncAttemptID: archiveContext.CommandID, PlanDigest: w.in.plan.digest, ArchiveSHA256: cleanupWirePaddingDigest(), ArchiveBytes: math.MaxInt64, ArchiveEntries: math.MaxUint64, TicketDigest: cleanupWirePaddingDigest(), TargetDigest: cleanupWirePaddingDigest(), EntryOrdinal: math.MaxUint64, BodyEnd: math.MaxInt64, PathDigest: cleanupWirePaddingDigest(), BodySHA256: cleanupWirePaddingDigest(), IntentID: context.CommandID, IntentBirth: math.MaxInt64 - 1, EvidenceDigest: cleanupWirePaddingDigest()}
	encoded, err := encodeOperationMetadata(payload, 4096)
	if err != nil {
		return fmt.Errorf("recovery checkpoint payload schema: %w", err)
	}
	checkpoint.Version, checkpoint.State, checkpoint.Attempt = 2, TaskCheckpointPending, w.in.bundle.initial
	checkpoint.DetailDigest, checkpoint.WorkspaceSync = workspaceSyncIntentDigest(encoded), &payload
	if _, err := encodeTaskCheckpointRecord(checkpoint); err != nil {
		return fmt.Errorf("task-checkpoint v2 schema: %w", err)
	}
	return w.syncSchemas()
}

func (w *cleanupLocalWireBuilder) syncSchemas() error {
	plan, worker := w.in.plan, w.in.state.record.Run.Worker
	ref := TaskWorkspaceSyncIntentReference{Task: w.claim.Task, BudgetID: w.b.workspaceSyncBudget.options.BudgetID, IntentID: w.in.state.record.Run.ID, Slot: w.b.workspaceSyncBudget.options.SlotsPerPartition - 1}
	// Filesystem diagnostics cannot manufacture an opened target or a witness.
	local := &taskWorkspaceSyncLocalAttribution{Worker: worker, Root: "/" + strings.Repeat("r", 4095), Dev: math.MaxUint64, Ino: math.MaxUint64, MountID: math.MaxUint64, Filesystem: 0xef53, FilesystemName: "ext4", UID: worker.UID, GID: worker.GID}
	attribution := taskWorkspaceSyncIntentAttribution{Version: 2, Reference: ref, BudgetRevision: w.b.workspaceSyncBudget.create, Kind: "put_object", FinalSyncAttemptID: ref.IntentID, CommandID: ref.IntentID, TicketDigest: cleanupWirePaddingDigest(), ArchiveSHA256: cleanupWirePaddingDigest(), PlanDigest: plan.digest, WorkspaceHash: plan.workspace.hash, Provider: plan.workspace.provider, StorageIdentityHash: plan.workspace.storageIdentityHash, Bucket: plan.workspace.bucket, Prefix: plan.workspace.prefix, RelativePath: strings.Repeat("f", 4096), Mode: 0777, Size: math.MaxInt64, Offset: math.MaxInt64, BodySHA256: cleanupWirePaddingDigest(), Local: local}
	intent, err := encodeWorkspaceSyncIntent(TaskWorkspaceSyncIntentRecord{taskWorkspaceSyncIntentAttribution: attribution, Claim: w.claim}, int(w.b.workspaceSyncBudget.options.MaxIntentBytes))
	if err != nil {
		return fmt.Errorf("workspace sync intent schema: %w", err)
	}
	for _, phase := range []string{"free", "occupied", "terminal"} {
		slot := taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: ref.BudgetID, Partition: ref.Task.Partition, Slot: ref.Slot, State: phase}
		if phase != "free" {
			slot.IntentID, slot.IntentDigest = ref.IntentID, workspaceSyncIntentDigest(intent)
		}
		if phase == "terminal" {
			slot.IntentBirth, slot.EvidenceDigest = math.MaxInt64-1, cleanupWirePaddingDigest()
		}
		if _, err := encodeWorkspaceSyncRecyclingSlot(slot); err != nil {
			return fmt.Errorf("workspace sync %s slot schema: %w", phase, err)
		}
	}
	return nil
}
