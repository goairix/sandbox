package etcd

import (
	"encoding/json"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Canonical metadata exercises only attribution and registry matching. It is
// not a runtime receipt, genuine worker or permission to send any command.
func cleanupEarlyMetadata(t *testing.T, quiescent bool) (*cleanupSlotState, *taskClaimBundle, *cleanupEarlyRecovery) {
	t.Helper()
	closeRecord := taskCloseRecordFixture(t)
	_, input := cleanupLocalWireFixture(t)
	r := cleanupSlotSchemaRecord()
	r.Task = closeRecord.Task.Reference
	r.TaskBirth = closeRecord.Task.ControlRevision + 1
	_, intent, _, _ := taskTestRecords()
	r.InitialAttempt = intent.Attempt
	taskWire, err := encodeTaskRecord(closeRecord.Task)
	require.NoError(t, err)
	r.TaskDigest = workerIdentityDigest(taskWire)
	r.Slot = cleanupCandidate(r.TaskDigest, r.Selection)
	r.Incarnation = cleanupSlotTestIncarnation(r)
	r.Phase, r.Confirmed = "occupied", "closed"
	run := *input.state.record.Run
	run.Joined = true
	r.Run = &run
	r.Pending = &cleanupExecutionPending{Purpose: "quiescence", ClaimID: closeRecord.Claim.ClaimID, LeaseID: closeRecord.Claim.LeaseID}
	closeEntry := &TaskCloseDataEntry{
		Reference: TaskCloseDataReference{Task: r.Task, CommandID: closeRecord.Context.CommandID, Stage: closeRecord.Attempt.reference(strings.Repeat("a", 64))},
		Record:    &closeRecord, Revision: 20, Outcome: OutcomeCommitted,
	}
	history := &cleanupEarlyRecovery{close: closeEntry}
	if quiescent {
		var ticket taskCloseTicketWire
		require.NoError(t, json.Unmarshal(closeRecord.Ticket, &ticket))
		receipt := quiesceRaw(t, p.TaskDataClosedReceiptClaims{Version: 1, State: "data_closed", Context: closeRecord.Context, TicketDigest: closeRecord.TicketDigest, NotBefore: ticket.Claims.NotBefore, NotAfter: ticket.Claims.NotAfter}, "sandbox-task-data-closed-receipt:v1\x00")
		qc := p.TaskUserQuiescenceContext{Current: closeRecord.Context, CloseDataContext: closeRecord.Context, CloseDataTicketDigest: closeRecord.TicketDigest, CloseDataReceiptDigest: workerIdentityDigest(string(receipt)), CloseDataIntentRevision: closeEntry.Revision}
		qt := quiesceRaw(t, p.TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: qc, NotBefore: ticket.Claims.NotBefore, NotAfter: ticket.Claims.NotAfter}, "sandbox-task-quiesce-users-ticket:v1\x00")
		attempt := closeRecord.Attempt
		attempt.StageID, attempt.AttemptID = "task_quiesce_users", uuid.NewString()
		q := TaskQuiescenceRecord{
			Version: 1, Task: closeRecord.Task, Claim: closeRecord.Claim, Context: qc,
			TicketDigest: workerIdentityDigest(string(qt)), Ticket: qt, CloseDataReceipt: receipt,
			IssuerCertificateID: closeRecord.IssuerCertificateID, IssuerCertificateDigest: closeRecord.IssuerCertificateDigest, IssuerRevision: closeRecord.IssuerRevision,
			Attempt: attempt,
		}
		require.NoError(t, q.Validate())
		reservation := TaskQuiescenceAttemptRecord{Version: 1, Task: q.Task, Claim: q.Claim, DestroyingRevision: q.Context.Current.ControlRevision, CommandID: q.Context.Current.CommandID, Attempt: q.Attempt}
		require.NoError(t, reservation.Validate())
		history.quiescence = &TaskQuiescenceEntry{Reference: TaskQuiescenceReference{Task: r.Task, CommandID: q.Context.Current.CommandID, Stage: attempt.reference(strings.Repeat("b", 64))}, Record: &q, Revision: 22, Outcome: OutcomeCommitted}
		history.attempt = &TaskQuiescenceAttemptEntry{Reference: reservation.reference(), Record: &reservation, Revision: 21}
		r.Confirmed, r.Pending.Purpose = "quiescent", "final-sync"
	}
	require.NoError(t, r.Validate())
	ns := namespaceFromTaskCloseTest(t, r.Task)
	eKey, err := ns.cleanupExecutionKey(r.Task)
	require.NoError(t, err)
	sKey, err := ns.cleanupExecutionSlotKey(r.Slot)
	require.NoError(t, err)
	eWire, err := encodeCleanupExecution(r)
	require.NoError(t, err)
	sWire, err := encodeCleanupExecutionSlot(cleanupSlotRecordFromExecution(r))
	require.NoError(t, err)
	state := &cleanupSlotState{record: r, execution: taskFence{key: eKey, value: eWire, create: 8, mod: 25}, slot: &taskFence{key: sKey, value: sWire, create: 9, mod: 9}}
	history.state = *state
	return state, &taskClaimBundle{task: closeRecord.Task, birth: r.TaskBirth}, history
}

func TestCleanupLocalEarlyRecoveryHistory(t *testing.T) {
	for _, quiescent := range []bool{false, true} {
		state, bundle, h := cleanupEarlyMetadata(t, quiescent)
		require.NoError(t, matchCleanupEarlyHistory(state, bundle, h.close, h.quiescence, h.attempt))
		require.NoError(t, matchCleanupEarlyOccupation(&h.state, state))
	}
	for name, change := range map[string]func(*cleanupSlotState, *taskClaimBundle, *cleanupEarlyRecovery){
		"task-birth":    func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) { b.birth++ },
		"task-record":   func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) { b.task.Generation++ },
		"unjoined":      func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) { s.record.Run.Joined = false },
		"claimed-mode":  func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) { s.record.Confirmed = "claimed" },
		"drained-mode":  func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) { s.record.Confirmed = "drained" },
		"missing-close": func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) { h.close = nil },
		"close-birth":   func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) { h.close.Revision = b.birth },
		"close-outcome": func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) {
			h.close.Outcome = OutcomeUnknown
		},
		"close-reference-task": func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) {
			h.close.Reference.Task.TaskID = uuid.NewString()
		},
		"quiescence-reference-task": func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) {
			h.quiescence.Reference.Task.TaskID = uuid.NewString()
		},
		"missing-quiescence": func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) { h.quiescence = nil },
		"missing-attempt":    func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) { h.attempt = nil },
		"attempt-reference": func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) {
			h.attempt.Reference.CommandID = uuid.NewString()
		},
		"pending-purpose": func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) {
			s.record.Pending.Purpose = "archive"
		},
		"pending-stage": func(s *cleanupSlotState, b *taskClaimBundle, h *cleanupEarlyRecovery) {
			s.record.Pending.Purpose = "quiescence"
			stage := h.quiescence.Reference.Stage
			s.record.Pending.StageID, s.record.Pending.RequestID = stage.StageID, stage.RequestID
			s.record.Pending.AttemptID, s.record.Pending.Digest = uuid.NewString(), stage.Digest
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, b, h := cleanupEarlyMetadata(t, true)
			change(s, b, h)
			require.Error(t, matchCleanupEarlyHistory(s, b, h.close, h.quiescence, h.attempt))
		})
	}
	t.Run("exact-pending-stage", func(t *testing.T) {
		s, b, h := cleanupEarlyMetadata(t, true)
		stage := h.quiescence.Reference.Stage
		s.record.Pending.Purpose = "quiescence"
		s.record.Pending.StageID, s.record.Pending.RequestID = stage.StageID, stage.RequestID
		s.record.Pending.AttemptID, s.record.Pending.Digest = stage.AttemptID, stage.Digest
		require.NoError(t, matchCleanupEarlyHistory(s, b, h.close, h.quiescence, h.attempt))
		h.quiescence.Reference.Stage.Digest = strings.Repeat("c", 64)
		require.Error(t, matchCleanupEarlyHistory(s, b, h.close, h.quiescence, h.attempt))
	})
	t.Run("reservation-only", func(t *testing.T) {
		s, b, h := cleanupEarlyMetadata(t, true)
		s.record.Confirmed = "closed"
		h.quiescence = nil
		require.Error(t, matchCleanupEarlyHistory(s, b, h.close, h.quiescence, h.attempt))
	})
	for _, field := range []string{"execution", "slot"} {
		t.Run(field+"-aba", func(t *testing.T) {
			s, _, h := cleanupEarlyMetadata(t, true)
			copy := *s
			slot := *s.slot
			copy.slot = &slot
			if field == "execution" {
				copy.execution.create++
			} else {
				copy.slot.create++
				copy.slot.mod++
			}
			require.Error(t, matchCleanupEarlyOccupation(&h.state, &copy))
		})
	}
	t.Run("foreign-registry", func(t *testing.T) {
		s, bundle, h := cleanupEarlyMetadata(t, true)
		b := &Backend{archiveLife: &taskArchiveBackend{}}
		w := &DueWorker{backend: b}
		w.self = w
		owner := &workerDueOwner{}
		job := &cleanupLocalJob{worker: w, task: s.record.Task, owner: owner, started: true, earlyRecovery: true, early: h, state: s, bundle: bundle}
		w.cleanupJobs[0] = job
		require.NoError(t, b.validateCleanupEarlyOwner(owner, job), "registry matching alone grants no native or physical authority")
		foreign := *job
		require.Error(t, b.validateCleanupEarlyOwner(owner, &foreign))
		job.owner = nil
		require.Error(t, b.validateCleanupEarlyOwner(owner, job))
	})
}
