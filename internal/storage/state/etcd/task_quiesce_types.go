package etcd

import (
	"encoding/json"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// TaskQuiescenceRecord is permanent structural history, never a live claim or
// permission to close a target. Its canonical stored wire uses a private Claim
// DTO; generic json.Marshal of this diagnostic struct is not the storage codec.
type TaskQuiescenceRecord struct {
	Version                 uint32                                    `json:"version"`
	Task                    TaskRecord                                `json:"task"`
	Claim                   TaskClaimReference                        `json:"claim"`
	Context                 controlprotocol.TaskUserQuiescenceContext `json:"context"`
	TicketDigest            string                                    `json:"ticket_digest"`
	IssuerCertificateID     string                                    `json:"issuer_certificate_id"`
	IssuerCertificateDigest string                                    `json:"issuer_certificate_digest"`
	IssuerRevision          int64                                     `json:"issuer_revision"`
	CloseDataReceipt        json.RawMessage                           `json:"close_data_receipt"`
	Ticket                  json.RawMessage                           `json:"ticket"`
	Attempt                 StageAttemptLocator                       `json:"attempt"`
}
type TaskQuiescenceReference struct {
	Task      TaskReference
	CommandID string
	Stage     StageReference
}
type TaskQuiescenceEntry struct {
	Reference TaskQuiescenceReference
	Record    *TaskQuiescenceRecord
	Revision  int64
	Outcome   Outcome
}

func (r TaskQuiescenceReference) Validate() error {
	l := StageAttemptLocator{Namespace: r.Stage.Namespace, Partition: r.Stage.Partition, RequestID: r.Stage.RequestID, StageID: r.Stage.StageID, AttemptID: r.Stage.AttemptID, RestoreEpoch: r.Stage.RestoreEpoch}
	if r.Task.Validate() != nil || !validPreparationUUID(r.CommandID) || !validHexDigest(r.Stage.Digest) || !validTaskQuiescenceAttempt(l, r.Task, l.RequestID) {
		return ErrInvalidRecord
	}
	return nil
}
func validTaskQuiescenceAttempt(l StageAttemptLocator, r TaskReference, claimID string) bool {
	return taskAttemptMatches(l, r) && validPreparationUUID(l.AttemptID) && validPreparationUUID(claimID) && l.RequestID == claimID && l.StageID == "task_quiesce_users"
}
