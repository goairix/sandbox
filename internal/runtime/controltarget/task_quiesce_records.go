package controltarget

import (
	"encoding/json"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"strings"
	"time"
)

const maxTaskQuiescenceRecordBytes = 16384

// TaskUserQuiescenceRecord is structural diagnostic history. Disk presence or
// State alone never proves durable acceptance or physical namespace quiescence.
type TaskUserQuiescenceRecord struct {
	Version            uint32                      `json:"version"`
	State              string                      `json:"state"`
	Context            p.TaskUserQuiescenceContext `json:"context"`
	TicketDigest       string                      `json:"ticket_digest"`
	NotBefore          time.Time                   `json:"not_before"`
	NotAfter           time.Time                   `json:"not_after"`
	ExecutionSetDigest string                      `json:"execution_set_digest"`
	RegisteredCount    uint32                      `json:"registered_count"`
	NeverSpawnedCount  uint32                      `json:"never_spawned_count"`
	LocalTerminalCount uint32                      `json:"local_terminal_count"`
}
type taskQuiescencePending struct {
	Version      uint32                      `json:"version"`
	State        string                      `json:"state"`
	Context      p.TaskUserQuiescenceContext `json:"context"`
	TicketDigest string                      `json:"ticket_digest"`
	NotBefore    time.Time                   `json:"not_before"`
	NotAfter     time.Time                   `json:"not_after"`
}

var journalQuiescenceContextSchema = journalObject(map[string]*journalSchema{
	"current": journalTaskCloseContextSchema, "close_data_context": journalTaskCloseContextSchema,
	"close_data_ticket_digest": journalStringField, "close_data_receipt_digest": journalStringField, "close_data_intent_revision": journalNumberField,
})
var journalQuiescencePendingSchema = journalObject(map[string]*journalSchema{
	"version": journalNumberField, "state": journalStringField, "context": journalQuiescenceContextSchema, "ticket_digest": journalStringField, "not_before": journalTimeField, "not_after": journalTimeField,
})
var journalQuiescenceTerminalSchema = func() *journalSchema {
	f := make(map[string]*journalSchema)
	for k, v := range journalQuiescencePendingSchema.fields {
		f[k] = v
	}
	f["execution_set_digest"] = journalStringField
	for _, k := range []string{"registered_count", "never_spawned_count", "local_terminal_count"} {
		f[k] = journalNumberField
	}
	return journalObject(f)
}()

func validateJournalQuiescenceContext(c p.TaskUserQuiescenceContext) error {
	a, b := c.Current, c.CloseDataContext
	if err := validateJournalTaskCloseContext(a); err != nil {
		return err
	}
	if err := validateJournalTaskCloseContext(b); err != nil {
		return err
	}
	if taskCloseIdentity(a) != taskCloseIdentity(b) || a.TaskID != b.TaskID || a.TaskDigest != b.TaskDigest || a.DataGateEpoch != b.DataGateEpoch || a.ControlRevision != b.ControlRevision || a.ClaimCreateRevision < b.ClaimCreateRevision || (a.ClaimCreateRevision == b.ClaimCreateRevision && (a.ClaimID != b.ClaimID || a.WorkerID != b.WorkerID || a.LeaseID != b.LeaseID)) || !journalHash(c.CloseDataTicketDigest) || !journalHash(c.CloseDataReceiptDigest) || c.CloseDataIntentRevision <= 0 {
		return ErrInvalidRecord
	}
	return nil
}
func (r TaskUserQuiescenceRecord) Validate() error {
	if err := validateJournalQuiescenceContext(r.Context); err != nil {
		return err
	}
	if r.Version != 1 || !journalHash(r.TicketDigest) || !journalUTC(r.NotBefore) || !journalUTC(r.NotAfter) || !r.NotBefore.Before(r.NotAfter) || r.NotAfter.Sub(r.NotBefore) > 30*time.Second {
		return ErrInvalidRecord
	}
	switch r.State {
	case "pending":
		if r.ExecutionSetDigest != "" || r.RegisteredCount != 0 || r.NeverSpawnedCount != 0 || r.LocalTerminalCount != 0 {
			return ErrInvalidRecord
		}
	case "users_quiesced":
		if !journalHash(r.ExecutionSetDigest) || r.RegisteredCount > 64 || r.NeverSpawnedCount > 64 || r.LocalTerminalCount > 64 || uint64(r.RegisteredCount) != uint64(r.NeverSpawnedCount)+uint64(r.LocalTerminalCount) {
			return ErrInvalidRecord
		}
	default:
		return ErrInvalidRecord
	}
	return nil
}
func encodeTaskQuiescence(r TaskUserQuiescenceRecord) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.State == "pending" {
		return encodeJournalWireLimit(taskQuiescencePending{r.Version, r.State, r.Context, r.TicketDigest, r.NotBefore, r.NotAfter}, maxTaskQuiescenceRecordBytes)
	}
	return encodeJournalWireLimit(r, maxTaskQuiescenceRecordBytes)
}
func decodeTaskQuiescence(w []byte) (TaskUserQuiescenceRecord, error) {
	var r TaskUserQuiescenceRecord
	var dispatch struct {
		State string `json:"state"`
	}
	if len(w) > maxTaskQuiescenceRecordBytes || json.Unmarshal(w, &dispatch) != nil {
		return r, ErrInvalidRecord
	}
	schema := journalQuiescencePendingSchema
	if dispatch.State == "users_quiesced" {
		schema = journalQuiescenceTerminalSchema
	}
	if err := decodeJournalWireLimit(w, schema, &r, maxTaskQuiescenceRecordBytes); err != nil {
		return TaskUserQuiescenceRecord{}, err
	}
	if err := r.Validate(); err != nil {
		return TaskUserQuiescenceRecord{}, err
	}
	return r, nil
}
func sameTaskQuiescence(a, b TaskUserQuiescenceRecord) bool {
	return a.Context == b.Context && a.TicketDigest == b.TicketDigest && a.NotBefore.Equal(b.NotBefore) && a.NotAfter.Equal(b.NotAfter)
}
func taskQuiescenceTempName(n string) bool {
	return strings.HasPrefix(n, ".users-quiesce.") && strings.HasSuffix(n, ".tmp") && journalUUID(strings.TrimSuffix(strings.TrimPrefix(n, ".users-quiesce."), ".tmp"))
}
