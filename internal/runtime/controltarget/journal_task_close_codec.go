package controltarget

import (
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// TaskDataCloseRecord is copied structural history. Even data_closed bytes can
// survive an uncertain directory fsync; State alone is never durable-close
// authority. A poisoned or cold journal cannot produce a signed close proof.
type TaskDataCloseRecord struct {
	Version      uint32                               `json:"version"`
	State        string                               `json:"state"`
	Context      controlprotocol.TaskCloseDataContext `json:"context"`
	TicketDigest string                               `json:"ticket_digest"`
	NotBefore    time.Time                            `json:"not_before"`
	NotAfter     time.Time                            `json:"not_after"`
}

var journalTaskCloseContextSchema = journalObject(map[string]*journalSchema{
	"namespace": journalStringField, "authority_id": journalStringField, "target": journalStringField, "restore_epoch": journalStringField,
	"issuer_certificate_id": journalStringField, "issuer_certificate_digest": journalStringField, "command_id": journalStringField,
	"task_id": journalStringField, "task_digest": journalStringField, "claim_id": journalStringField, "worker_id": journalStringField,
	"sandbox_id": journalStringField, "workspace_hash": journalStringField, "generation": journalNumberField, "data_gate_epoch": journalNumberField,
	"control_revision": journalNumberField, "claim_create_revision": journalNumberField, "lease_id": journalNumberField, "runtime": journalRuntimeSchema,
})
var journalTaskCloseSchema = journalObject(map[string]*journalSchema{
	"version": journalNumberField, "state": journalStringField, "context": journalTaskCloseContextSchema,
	"ticket_digest": journalStringField, "not_before": journalTimeField, "not_after": journalTimeField,
})

func taskCloseIdentity(c controlprotocol.TaskCloseDataContext) JournalIdentity {
	return JournalIdentity{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}
}

// Static validation is shared by cold diagnostics and the live consumer. Only
// VerifyTaskCloseDataTicket below authenticates the actual protocol signature.
func validateJournalTaskCloseContext(c controlprotocol.TaskCloseDataContext) error {
	if err := taskCloseIdentity(c).Validate(); err != nil {
		return err
	}
	if !journalUUID(c.IssuerCertificateID) || !journalHash(c.IssuerCertificateDigest) || !journalUUID(c.CommandID) || !journalUUID(c.TaskID) || !journalHash(c.TaskDigest) || !journalUUID(c.ClaimID) || !journalID(c.WorkerID) || c.DataGateEpoch <= 0 || c.ControlRevision <= 0 || c.ClaimCreateRevision <= c.ControlRevision || c.LeaseID <= 0 {
		return ErrInvalidRecord
	}
	return nil
}
func (r TaskDataCloseRecord) Validate() error {
	if err := validateJournalTaskCloseContext(r.Context); err != nil {
		return err
	}
	if r.Version != 1 || (r.State != "pending" && r.State != "data_closed") || !journalHash(r.TicketDigest) || !journalUTC(r.NotBefore) || !journalUTC(r.NotAfter) || !r.NotBefore.Before(r.NotAfter) || r.NotAfter.Sub(r.NotBefore) > 30*time.Second {
		return ErrInvalidRecord
	}
	return nil
}
func encodeTaskDataClose(r TaskDataCloseRecord) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return encodeJournalWire(r)
}
func decodeTaskDataClose(wire []byte) (TaskDataCloseRecord, error) {
	var r TaskDataCloseRecord
	if err := decodeJournalWire(wire, journalTaskCloseSchema, &r); err != nil {
		return TaskDataCloseRecord{}, err
	}
	if err := r.Validate(); err != nil {
		return TaskDataCloseRecord{}, err
	}
	return r, nil
}
func (j *Journal) taskCloseBinding(c controlprotocol.TaskCloseDataContext) error {
	if taskCloseIdentity(c) != j.gate.Identity || c.DataGateEpoch != j.gate.DataGateEpoch {
		return ErrIdentityMismatch
	}
	return nil
}
func sameTaskClose(a, b TaskDataCloseRecord) bool {
	return a.Context == b.Context && a.TicketDigest == b.TicketDigest && a.NotBefore.Equal(b.NotBefore) && a.NotAfter.Equal(b.NotAfter)
}
