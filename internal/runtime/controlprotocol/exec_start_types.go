package controlprotocol

import "time"

// ExecStartContext binds an original operation and its exact runtime incarnation.
// It must come from trusted operation state, rather than ticket contents.
type ExecStartContext struct {
	Namespace               string           `json:"namespace"`
	AuthorityID             string           `json:"authority_id"`
	Target                  string           `json:"target"`
	RestoreEpoch            string           `json:"restore_epoch"`
	IssuerCertificateID     string           `json:"issuer_certificate_id"`
	IssuerCertificateDigest string           `json:"issuer_certificate_digest"`
	CommandID               string           `json:"command_id"`
	OperationID             string           `json:"operation_id"`
	RequestID               string           `json:"request_id"`
	OperationDigest         string           `json:"operation_digest"`
	SandboxID               string           `json:"sandbox_id"`
	WorkspaceHash           string           `json:"workspace_hash"`
	Generation              int64            `json:"generation"`
	DataGateEpoch           int64            `json:"data_gate_epoch"`
	ControlRevision         int64            `json:"control_revision"`
	AdmissionRevision       int64            `json:"admission_revision"`
	LeaseID                 int64            `json:"lease_id"`
	Runtime                 RuntimeReference `json:"runtime"`
	ExpiresAt               time.Time        `json:"expires_at"`
}

// ExecStartTicketClaims authenticates only a bounded original-operation start.
type ExecStartTicketClaims struct {
	Version          uint32           `json:"version"`
	Purpose          string           `json:"purpose"`
	Context          ExecStartContext `json:"context"`
	DescriptorDigest string           `json:"descriptor_digest"`
	NotBefore        time.Time        `json:"not_before"`
	NotAfter         time.Time        `json:"not_after"`
}

// ExecStartEvidence proves signature, payload, context, and time matching.
// It does not prove active issuer, live lease, committed intent, or open gate.
type ExecStartEvidence struct {
	wire                []byte
	digest              string
	context             ExecStartContext
	descriptorDigest    string
	notBefore, notAfter time.Time
}

func (e ExecStartEvidence) Wire() []byte              { return append([]byte(nil), e.wire...) }
func (e ExecStartEvidence) Digest() string            { return e.digest }
func (e ExecStartEvidence) Context() ExecStartContext { return e.context }
func (e ExecStartEvidence) DescriptorDigest() string  { return e.descriptorDigest }
func (e ExecStartEvidence) NotBefore() time.Time      { return e.notBefore }
func (e ExecStartEvidence) NotAfter() time.Time       { return e.notAfter }

type execStartEnvelope struct {
	Claims    ExecStartTicketClaims `json:"claims"`
	Signature []byte                `json:"signature"`
}
