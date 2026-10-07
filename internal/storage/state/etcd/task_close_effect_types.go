package etcd

import (
	"encoding/json"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

// TaskCloseDataRecord is permanent structural history, never a live claim or
// permission to close a target. Its canonical stored wire uses a private Claim
// DTO; generic json.Marshal of this diagnostic struct is not the storage codec.
type TaskCloseDataRecord struct {
	Version                 uint32                               `json:"version"`
	Task                    TaskRecord                           `json:"task"`
	Claim                   TaskClaimReference                   `json:"claim"`
	Context                 controlprotocol.TaskCloseDataContext `json:"context"`
	TicketDigest            string                               `json:"ticket_digest"`
	IssuerCertificateID     string                               `json:"issuer_certificate_id"`
	IssuerCertificateDigest string                               `json:"issuer_certificate_digest"`
	IssuerRevision          int64                                `json:"issuer_revision"`
	Ticket                  json.RawMessage                      `json:"ticket"`
	Attempt                 StageAttemptLocator                  `json:"attempt"`
}
type TaskCloseDataReference struct {
	Task      TaskReference
	CommandID string
	Stage     StageReference
}
type TaskCloseDataEntry struct {
	Reference TaskCloseDataReference
	Record    *TaskCloseDataRecord
	Revision  int64
	Outcome   Outcome
}

func (r TaskCloseDataReference) Validate() error {
	l := StageAttemptLocator{Namespace: r.Stage.Namespace, Partition: r.Stage.Partition, RequestID: r.Stage.RequestID, StageID: r.Stage.StageID, AttemptID: r.Stage.AttemptID, RestoreEpoch: r.Stage.RestoreEpoch}
	if r.Task.Validate() != nil || !validPreparationUUID(r.CommandID) || !validHexDigest(r.Stage.Digest) || !validTaskCloseAttempt(l, r.Task, l.RequestID) {
		return ErrInvalidRecord
	}
	return nil
}
func validTaskCloseAttempt(l StageAttemptLocator, r TaskReference, claimID string) bool {
	return taskAttemptMatches(l, r) && validPreparationUUID(l.AttemptID) && validPreparationUUID(claimID) && l.RequestID == claimID && l.StageID == "task_close_data"
}
