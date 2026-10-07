package etcd

import (
	"bytes"
	"encoding/json"
)

// ExecEffectRecord is permanent structural evidence of a metadata exec start.
// Its opaque ticket does not grant command execution authority.
type ExecEffectRecord struct {
	Version                 uint32              `json:"version"`
	Operation               OperationRecord     `json:"operation"`
	AdmissionRevision       int64               `json:"admission_revision"`
	CommandID               string              `json:"command_id"`
	DescriptorDigest        string              `json:"descriptor_digest"`
	TicketDigest            string              `json:"ticket_digest"`
	IssuerCertificateID     string              `json:"issuer_certificate_id"`
	IssuerCertificateDigest string              `json:"issuer_certificate_digest"`
	IssuerRevision          int64               `json:"issuer_revision"`
	Ticket                  json.RawMessage     `json:"ticket"`
	Attempt                 StageAttemptLocator `json:"attempt"`
}

// ExecEffectReference identifies one complete metadata attempt, without authority.
type ExecEffectReference struct {
	Operation OperationReference
	CommandID string
	Stage     StageReference
}

// ExecEffectEntry carries copied historical evidence, never a capability.
type ExecEffectEntry struct {
	Reference ExecEffectReference
	Outcome   Outcome
	Record    *ExecEffectRecord
	Revision  int64
}

func (r ExecEffectRecord) Validate() error {
	if r.Version != 1 || r.Operation.Reference.Kind != OperationData || r.AdmissionRevision <= 0 || r.IssuerRevision <= 0 || !validPreparationUUID(r.CommandID) || !validPreparationUUID(r.IssuerCertificateID) || !validHexDigest(r.DescriptorDigest) || !validHexDigest(r.TicketDigest) || !validHexDigest(r.IssuerCertificateDigest) || len(r.Ticket) == 0 || len(r.Ticket) > 4096 || bytes.Equal(bytes.TrimSpace(r.Ticket), []byte("null")) || !validExecEffectAttempt(r.Attempt, r.Operation.Reference) {
		return ErrInvalidRecord
	}
	if _, err := encodeOperationRecord(r.Operation); err != nil {
		return ErrInvalidRecord
	}
	digest, err := snapshotDigest(r.Ticket)
	if err != nil || digest != r.TicketDigest {
		return ErrInvalidRecord
	}
	return nil
}

func (r ExecEffectReference) Validate() error {
	locator := StageAttemptLocator{Namespace: r.Stage.Namespace, Partition: r.Stage.Partition, RequestID: r.Stage.RequestID, StageID: r.Stage.StageID, AttemptID: r.Stage.AttemptID, RestoreEpoch: r.Stage.RestoreEpoch}
	if r.Operation.Validate() != nil || r.Operation.Kind != OperationData || !validPreparationUUID(r.CommandID) || !validHexDigest(r.Stage.Digest) || !validExecEffectAttempt(locator, r.Operation) {
		return ErrInvalidRecord
	}
	return nil
}

func validExecEffectAttempt(l StageAttemptLocator, operation OperationReference) bool {
	return l.Validate() == nil && validPreparationUUID(l.AttemptID) && l.StageID == "operation_exec_start" && l.Namespace == operation.Namespace && l.Partition == operation.Partition && l.RequestID == operation.RequestID && l.RestoreEpoch == operation.RestoreEpoch
}

// PrepareExecEffectResult separates metadata outcome from current authority.
// A committed result can have no Prepared when fresh authorization fails.
type PrepareExecEffectResult struct {
	Outcome           Outcome
	Reference         ExecEffectReference
	Prepared          *PreparedExecEffect
	GuardCleanupError error
}

// PreparedExecEffect is opaque input for a future trusted transport. It grants
// no public ticket or payload access; that transport must reauthorize delivery.
type PreparedExecEffect struct {
	self       *PreparedExecEffect
	origin     *Backend
	capability *OperationCapability
	draft      *execEffectDraft
}

func (p *PreparedExecEffect) Reference() ExecEffectReference {
	if p == nil || p.draft == nil {
		return ExecEffectReference{}
	}
	return p.draft.reference
}
