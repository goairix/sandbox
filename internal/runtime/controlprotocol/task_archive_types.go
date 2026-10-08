package controlprotocol

import (
	"fmt"
	"time"
)

// TaskWorkspaceArchiveContext attributes one fixed final-sync attempt.
type TaskWorkspaceArchiveContext struct {
	Current                  TaskCloseDataContext      `json:"current"`
	QuiescenceContext        TaskUserQuiescenceContext `json:"quiescence_context"`
	QuiescenceTicketDigest   string                    `json:"quiescence_ticket_digest"`
	QuiescenceReceiptDigest  string                    `json:"quiescence_receipt_digest"`
	PlanDigest               string                    `json:"plan_digest"`
	QuiescenceIntentRevision int64                     `json:"quiescence_intent_revision"`
	FinalSyncRevision        int64                     `json:"final_sync_revision"`
}

type TaskWorkspaceArchiveTicketClaims struct {
	Version         uint32                      `json:"version"`
	Purpose         string                      `json:"purpose"`
	Context         TaskWorkspaceArchiveContext `json:"context"`
	NotBefore       time.Time                   `json:"not_before"`
	NotAfter        time.Time                   `json:"not_after"`
	SessionNotAfter time.Time                   `json:"session_not_after"`
}

// TaskWorkspaceArchiveEvidence is copied attribution and a fixed window,
// never live claim, producer, remote settlement, or safe-release authority.
type TaskWorkspaceArchiveEvidence struct {
	wire   []byte
	digest string
	claims TaskWorkspaceArchiveTicketClaims
}

func (e TaskWorkspaceArchiveEvidence) Wire() []byte                         { return append([]byte(nil), e.wire...) }
func (e TaskWorkspaceArchiveEvidence) Context() TaskWorkspaceArchiveContext { return e.claims.Context }
func (e TaskWorkspaceArchiveEvidence) NotBefore() time.Time                 { return e.claims.NotBefore }
func (e TaskWorkspaceArchiveEvidence) NotAfter() time.Time                  { return e.claims.NotAfter }
func (e TaskWorkspaceArchiveEvidence) SessionNotAfter() time.Time           { return e.claims.SessionNotAfter }
func (e TaskWorkspaceArchiveEvidence) Digest() string                       { return e.digest }

// TaskWorkspaceArchiveReceiptClaims asserts a producer's terminal summary.
// Verification does not prove received bytes, stream EOF, or safe release.
type TaskWorkspaceArchiveReceiptClaims struct {
	Version         uint32                      `json:"version"`
	State           string                      `json:"state"`
	Context         TaskWorkspaceArchiveContext `json:"context"`
	TicketDigest    string                      `json:"ticket_digest"`
	NotBefore       time.Time                   `json:"not_before"`
	NotAfter        time.Time                   `json:"not_after"`
	SessionNotAfter time.Time                   `json:"session_not_after"`
	ArchiveBytes    uint64                      `json:"archive_bytes"`
	EntryCount      uint64                      `json:"entry_count"`
	SHA256          string                      `json:"sha256"`
}

// TaskWorkspaceArchiveReceiptEvidence is historical signed attribution only.
type TaskWorkspaceArchiveReceiptEvidence struct {
	wire   []byte
	claims TaskWorkspaceArchiveReceiptClaims
}

func (e TaskWorkspaceArchiveReceiptEvidence) Wire() []byte { return append([]byte(nil), e.wire...) }
func (e TaskWorkspaceArchiveReceiptEvidence) Context() TaskWorkspaceArchiveContext {
	return e.claims.Context
}
func (e TaskWorkspaceArchiveReceiptEvidence) NotBefore() time.Time { return e.claims.NotBefore }
func (e TaskWorkspaceArchiveReceiptEvidence) NotAfter() time.Time  { return e.claims.NotAfter }
func (e TaskWorkspaceArchiveReceiptEvidence) SessionNotAfter() time.Time {
	return e.claims.SessionNotAfter
}
func (e TaskWorkspaceArchiveReceiptEvidence) State() string        { return e.claims.State }
func (e TaskWorkspaceArchiveReceiptEvidence) TicketDigest() string { return e.claims.TicketDigest }
func (e TaskWorkspaceArchiveReceiptEvidence) ArchiveBytes() uint64 { return e.claims.ArchiveBytes }
func (e TaskWorkspaceArchiveReceiptEvidence) EntryCount() uint64   { return e.claims.EntryCount }
func (e TaskWorkspaceArchiveReceiptEvidence) SHA256() string       { return e.claims.SHA256 }

var taskArchiveContextSchema = objectSchema(map[string]*wireSchema{
	"current": taskCloseContextSchema, "quiescence_context": taskQuiescenceContextSchema,
	"quiescence_ticket_digest": stringField, "quiescence_receipt_digest": stringField,
	"plan_digest": stringField, "quiescence_intent_revision": numberField, "final_sync_revision": numberField,
})

func validateTaskArchiveContext(c TaskWorkspaceArchiveContext) error {
	q := c.QuiescenceContext
	if err := validateTaskQuiescenceContext(q); err != nil {
		return err
	}
	// Reuse the same task/runtime/claim lineage rules for the next attempt.
	if err := validateTaskQuiescenceContext(TaskUserQuiescenceContext{Current: c.Current, CloseDataContext: q.Current, CloseDataTicketDigest: c.QuiescenceTicketDigest, CloseDataReceiptDigest: c.QuiescenceReceiptDigest, CloseDataIntentRevision: c.QuiescenceIntentRevision}); err != nil {
		return err
	}
	if !validHash(c.PlanDigest) || c.Current.CommandID == q.Current.CommandID || c.Current.CommandID == q.CloseDataContext.CommandID || c.QuiescenceIntentRevision <= q.Current.ClaimCreateRevision || c.QuiescenceIntentRevision <= q.CloseDataIntentRevision || c.FinalSyncRevision <= c.Current.ClaimCreateRevision || c.FinalSyncRevision <= c.QuiescenceIntentRevision {
		return fmt.Errorf("invalid task archive lineage or revision order")
	}
	return nil
}

func validateTaskArchiveWindow(before, after, sessionAfter time.Time) error {
	if err := validateTaskCloseWindow(before, after); err != nil {
		return err
	}
	if !validUTC(sessionAfter) || !after.Before(sessionAfter) || sessionAfter.Sub(before) > time.Hour {
		return fmt.Errorf("invalid task archive session interval")
	}
	return nil
}
