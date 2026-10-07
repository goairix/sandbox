package controlprotocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// TaskUserQuiescenceContext links the current attempt to historical CloseData
// attribution. Neither context proves a live claim or physical quiescence.
type TaskUserQuiescenceContext struct {
	Current                 TaskCloseDataContext `json:"current"`
	CloseDataContext        TaskCloseDataContext `json:"close_data_context"`
	CloseDataTicketDigest   string               `json:"close_data_ticket_digest"`
	CloseDataReceiptDigest  string               `json:"close_data_receipt_digest"`
	CloseDataIntentRevision int64                `json:"close_data_intent_revision"`
}

var taskQuiescenceContextSchema = objectSchema(map[string]*wireSchema{
	"current": taskCloseContextSchema, "close_data_context": taskCloseContextSchema,
	"close_data_ticket_digest": stringField, "close_data_receipt_digest": stringField,
	"close_data_intent_revision": numberField,
})

func validateTaskQuiescenceContext(c TaskUserQuiescenceContext) error {
	if err := validateTaskCloseContext(c.Current); err != nil {
		return err
	}
	if err := validateTaskCloseContext(c.CloseDataContext); err != nil {
		return err
	}
	a, b := c.Current, c.CloseDataContext
	if !validHash(c.CloseDataTicketDigest) || !validHash(c.CloseDataReceiptDigest) || c.CloseDataIntentRevision <= 0 ||
		taskCloseBinding(a) != taskCloseBinding(b) || a.TaskID != b.TaskID || a.TaskDigest != b.TaskDigest || a.SandboxID != b.SandboxID || a.WorkspaceHash != b.WorkspaceHash || a.Generation != b.Generation || a.DataGateEpoch != b.DataGateEpoch || a.ControlRevision != b.ControlRevision || a.Runtime != b.Runtime || a.ClaimCreateRevision < b.ClaimCreateRevision ||
		(a.ClaimCreateRevision == b.ClaimCreateRevision && (a.ClaimID != b.ClaimID || a.WorkerID != b.WorkerID || a.LeaseID != b.LeaseID)) {
		return fmt.Errorf("invalid task quiescence linkage")
	}
	return nil
}

type TaskUserQuiescenceTicketClaims struct {
	Version   uint32                    `json:"version"`
	Purpose   string                    `json:"purpose"`
	Context   TaskUserQuiescenceContext `json:"context"`
	NotBefore time.Time                 `json:"not_before"`
	NotAfter  time.Time                 `json:"not_after"`
}

// TaskUserQuiescenceEvidence is copied signature attribution, never a cleanup capability.
type TaskUserQuiescenceEvidence struct {
	wire   []byte
	digest string
	claims TaskUserQuiescenceTicketClaims
}

func (e TaskUserQuiescenceEvidence) Wire() []byte                       { return append([]byte(nil), e.wire...) }
func (e TaskUserQuiescenceEvidence) Context() TaskUserQuiescenceContext { return e.claims.Context }
func (e TaskUserQuiescenceEvidence) NotBefore() time.Time               { return e.claims.NotBefore }
func (e TaskUserQuiescenceEvidence) NotAfter() time.Time                { return e.claims.NotAfter }
func (e TaskUserQuiescenceEvidence) Digest() string                     { return e.digest }

type TaskUserQuiescenceAcceptedClaims struct {
	Version      uint32                    `json:"version"`
	State        string                    `json:"state"`
	Context      TaskUserQuiescenceContext `json:"context"`
	TicketDigest string                    `json:"ticket_digest"`
	NotBefore    time.Time                 `json:"not_before"`
	NotAfter     time.Time                 `json:"not_after"`
}

// TaskUserQuiescenceAcceptedEvidence is copied signature attribution, never a cleanup capability.
type TaskUserQuiescenceAcceptedEvidence struct {
	wire   []byte
	claims TaskUserQuiescenceAcceptedClaims
}

func (e TaskUserQuiescenceAcceptedEvidence) Wire() []byte { return append([]byte(nil), e.wire...) }
func (e TaskUserQuiescenceAcceptedEvidence) Context() TaskUserQuiescenceContext {
	return e.claims.Context
}
func (e TaskUserQuiescenceAcceptedEvidence) NotBefore() time.Time { return e.claims.NotBefore }
func (e TaskUserQuiescenceAcceptedEvidence) NotAfter() time.Time  { return e.claims.NotAfter }
func (e TaskUserQuiescenceAcceptedEvidence) State() string        { return e.claims.State }
func (e TaskUserQuiescenceAcceptedEvidence) TicketDigest() string { return e.claims.TicketDigest }

type TaskUserQuiescenceReceiptClaims struct {
	Version            uint32                    `json:"version"`
	State              string                    `json:"state"`
	Context            TaskUserQuiescenceContext `json:"context"`
	TicketDigest       string                    `json:"ticket_digest"`
	NotBefore          time.Time                 `json:"not_before"`
	NotAfter           time.Time                 `json:"not_after"`
	ExecutionSetDigest string                    `json:"execution_set_digest"`
	RegisteredCount    uint32                    `json:"registered_count"`
	NeverSpawnedCount  uint32                    `json:"never_spawned_count"`
	LocalTerminalCount uint32                    `json:"local_terminal_count"`
}

// TaskUserQuiescenceReceiptEvidence is copied signature attribution, never a cleanup capability.
type TaskUserQuiescenceReceiptEvidence struct {
	wire   []byte
	claims TaskUserQuiescenceReceiptClaims
}

func (e TaskUserQuiescenceReceiptEvidence) Wire() []byte { return append([]byte(nil), e.wire...) }
func (e TaskUserQuiescenceReceiptEvidence) Context() TaskUserQuiescenceContext {
	return e.claims.Context
}
func (e TaskUserQuiescenceReceiptEvidence) NotBefore() time.Time { return e.claims.NotBefore }
func (e TaskUserQuiescenceReceiptEvidence) NotAfter() time.Time  { return e.claims.NotAfter }
func (e TaskUserQuiescenceReceiptEvidence) State() string        { return e.claims.State }
func (e TaskUserQuiescenceReceiptEvidence) TicketDigest() string { return e.claims.TicketDigest }
func (e TaskUserQuiescenceReceiptEvidence) ExecutionSetDigest() string {
	return e.claims.ExecutionSetDigest
}
func (e TaskUserQuiescenceReceiptEvidence) RegisteredCount() uint32 { return e.claims.RegisteredCount }
func (e TaskUserQuiescenceReceiptEvidence) NeverSpawnedCount() uint32 {
	return e.claims.NeverSpawnedCount
}
func (e TaskUserQuiescenceReceiptEvidence) LocalTerminalCount() uint32 {
	return e.claims.LocalTerminalCount
}

// TaskQuiescenceExecution binds one original execution disposition. Its producer
// must independently verify ownership, joins and any exact terminal receipt.
type TaskQuiescenceExecution struct {
	CommandID             string           `json:"command_id"`
	ExecContext           ExecStartContext `json:"exec_context"`
	TicketDigest          string           `json:"ticket_digest"`
	Disposition           string           `json:"disposition"`
	TerminalReceiptDigest string           `json:"terminal_receipt_digest"`
}

// DigestTaskQuiescenceExecutions validates and hashes an already ordered bounded
// list. It does not establish completeness, process absence or physical drain.
func DigestTaskQuiescenceExecutions(entries []TaskQuiescenceExecution) (string, error) {
	if len(entries) > 64 {
		return "", fmt.Errorf("too many quiescence executions")
	}
	// Copy the input and retain [] rather than null for the canonical empty set.
	owned := make([]TaskQuiescenceExecution, len(entries))
	copy(owned, entries)
	for i, e := range owned {
		if !validUUID(e.CommandID) || e.CommandID != e.ExecContext.CommandID || !validHash(e.TicketDigest) || (i > 0 && owned[i-1].CommandID >= e.CommandID) {
			return "", fmt.Errorf("invalid quiescence execution attribution or order")
		}
		if err := validateExecStartContext(e.ExecContext); err != nil {
			return "", err
		}
		if (e.Disposition == "never_spawned" && e.TerminalReceiptDigest == "") || (e.Disposition == "local_terminal" && validHash(e.TerminalReceiptDigest)) {
			continue
		}
		return "", fmt.Errorf("invalid quiescence execution disposition")
	}
	wire, err := json.Marshal(owned)
	if err != nil {
		return "", err
	}
	return wireDigest(wire), nil
}
