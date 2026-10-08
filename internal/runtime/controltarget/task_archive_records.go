package controltarget

import (
	"encoding/json"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"strings"
	"time"
)

const maxTaskArchiveRecordBytes = 16384

// TaskWorkspaceArchiveRecord is unsigned structural history, never authority.
type TaskWorkspaceArchiveRecord struct {
	Version         uint32                        `json:"version"`
	State           string                        `json:"state"`
	Context         p.TaskWorkspaceArchiveContext `json:"context"`
	TicketDigest    string                        `json:"ticket_digest"`
	NotBefore       time.Time                     `json:"not_before"`
	NotAfter        time.Time                     `json:"not_after"`
	SessionNotAfter time.Time                     `json:"session_not_after"`
	ArchiveBytes    uint64                        `json:"archive_bytes"`
	EntryCount      uint64                        `json:"entry_count"`
	SHA256          string                        `json:"sha256"`
}
type taskArchivePending struct {
	Version         uint32                        `json:"version"`
	State           string                        `json:"state"`
	Context         p.TaskWorkspaceArchiveContext `json:"context"`
	TicketDigest    string                        `json:"ticket_digest"`
	NotBefore       time.Time                     `json:"not_before"`
	NotAfter        time.Time                     `json:"not_after"`
	SessionNotAfter time.Time                     `json:"session_not_after"`
}

var journalArchiveContextSchema = journalObject(map[string]*journalSchema{
	"current": journalTaskCloseContextSchema, "quiescence_context": journalQuiescenceContextSchema,
	"quiescence_ticket_digest": journalStringField, "quiescence_receipt_digest": journalStringField,
	"plan_digest": journalStringField, "quiescence_intent_revision": journalNumberField, "final_sync_revision": journalNumberField,
})
var journalArchivePendingSchema = journalObject(map[string]*journalSchema{
	"version": journalNumberField, "state": journalStringField, "context": journalArchiveContextSchema, "ticket_digest": journalStringField, "not_before": journalTimeField, "not_after": journalTimeField, "session_not_after": journalTimeField,
})
var journalArchiveTerminalSchema = func() *journalSchema {
	f := make(map[string]*journalSchema)
	for k, v := range journalArchivePendingSchema.fields {
		f[k] = v
	}
	f["archive_bytes"] = journalNumberField
	f["entry_count"] = journalNumberField
	f["sha256"] = journalStringField
	return journalObject(f)
}()

func (r TaskWorkspaceArchiveRecord) Validate() error {
	if !journalHash(r.TicketDigest) {
		return ErrInvalidRecord
	}
	c := p.TaskWorkspaceArchiveTicketClaims{Version: r.Version, Purpose: "task_workspace_archive", Context: r.Context, NotBefore: r.NotBefore, NotAfter: r.NotAfter, SessionNotAfter: r.SessionNotAfter}
	if err := c.Validate(); err != nil {
		return err
	}
	switch r.State {
	case "pending":
		if r.ArchiveBytes != 0 || r.EntryCount != 0 || r.SHA256 != "" {
			return ErrInvalidRecord
		}
	case "archive_complete":
		return (p.TaskWorkspaceArchiveReceiptClaims{Version: r.Version, State: r.State, Context: r.Context, TicketDigest: r.TicketDigest, NotBefore: r.NotBefore, NotAfter: r.NotAfter, SessionNotAfter: r.SessionNotAfter, ArchiveBytes: r.ArchiveBytes, EntryCount: r.EntryCount, SHA256: r.SHA256}).Validate()
	default:
		return ErrInvalidRecord
	}
	return nil
}
func encodeTaskArchive(r TaskWorkspaceArchiveRecord) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	if r.State == "pending" {
		return encodeJournalWireLimit(taskArchivePending{r.Version, r.State, r.Context, r.TicketDigest, r.NotBefore, r.NotAfter, r.SessionNotAfter}, maxTaskArchiveRecordBytes)
	}
	return encodeJournalWireLimit(r, maxTaskArchiveRecordBytes)
}
func decodeTaskArchive(b []byte) (TaskWorkspaceArchiveRecord, error) {
	var r TaskWorkspaceArchiveRecord
	var d struct {
		State string `json:"state"`
	}
	if len(b) > maxTaskArchiveRecordBytes || json.Unmarshal(b, &d) != nil {
		return r, ErrInvalidRecord
	}
	s := journalArchivePendingSchema
	if d.State == "archive_complete" {
		s = journalArchiveTerminalSchema
	}
	if err := decodeJournalWireLimit(b, s, &r, maxTaskArchiveRecordBytes); err != nil {
		return TaskWorkspaceArchiveRecord{}, err
	}
	return r, r.Validate()
}
func sameTaskArchive(a, b TaskWorkspaceArchiveRecord) bool {
	return a.Context == b.Context && a.TicketDigest == b.TicketDigest && a.NotBefore.Equal(b.NotBefore) && a.NotAfter.Equal(b.NotAfter) && a.SessionNotAfter.Equal(b.SessionNotAfter)
}
func taskArchiveTempName(n string) bool {
	return strings.HasPrefix(n, ".workspace-archive.") && strings.HasSuffix(n, ".tmp") && journalUUID(strings.TrimSuffix(strings.TrimPrefix(n, ".workspace-archive."), ".tmp"))
}
