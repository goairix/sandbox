package controltransport

import p "github.com/goairix/sandbox/internal/runtime/controlprotocol"

// TaskQuiesceEnvelope is a fixed-purpose finite request, never an execution
// descriptor. Queries carry attribution only and cannot replay an effect.
type TaskQuiesceEnvelope struct {
	Version                  uint32                      `json:"version"`
	Purpose                  string                      `json:"purpose"`
	Context                  p.TaskUserQuiescenceContext `json:"context"`
	TicketDigest             string                      `json:"ticket_digest"`
	Ticket                   []byte                      `json:"ticket"`
	IssuerCertificate        []byte                      `json:"issuer_certificate"`
	OriginalCloseDataReceipt []byte                      `json:"original_close_data_receipt"`
}

// RuntimeRequest preserves the existing codecs with narrowly typed task
// branches. At most one task pointer can be nonnil.
type RuntimeRequest struct {
	Exec        Envelope
	CloseData   *TaskCloseEnvelope
	Quiesce     *TaskQuiesceEnvelope
	Archive     *TaskWorkspaceArchiveEnvelope
	Publication *RuntimePublicationEnvelope
}
