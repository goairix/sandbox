// Package controlprotocol authenticates runtime publication assertions. Its
// evidence proves who signed a statement; a trusted producer must observe the
// actual durable gate and mount state before signing that statement.
package controlprotocol

import (
	"crypto/ed25519"
	"time"
)

type RuntimeReference struct {
	ID     string `json:"id"`
	UID    string `json:"uid"`
	BootID string `json:"boot_id"`
}

type SnapshotReference struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
}

type ClaimReference struct {
	ClaimID        string `json:"claim_id"`
	CreateRevision int64  `json:"create_revision"`
	LeaseID        int64  `json:"lease_id"`
}

type CertificateContext struct {
	Namespace, AuthorityID, Target, RestoreEpoch string
	IntentID, SandboxID, WorkspaceHash           string
	Generation                                   int64
	OperationID, PayloadDigest                   string
	Snapshot                                     SnapshotReference
	ExpiresAt                                    time.Time
}

type PublicationContext struct {
	Certificate       CertificateContext
	Runtime           RuntimeReference
	CertificateDigest string
	Claim             ClaimReference
	DataGateEpoch     int64
	MountAttempt      uint8
	MountOperationID  string
}

type TrustBinding struct{ Namespace, AuthorityID, Target, RestoreEpoch string }

type RuntimeCertificateClaims struct {
	Version       uint32            `json:"version"`
	RootKeyID     string            `json:"root_key_id"`
	Namespace     string            `json:"namespace"`
	AuthorityID   string            `json:"authority_id"`
	Target        string            `json:"target"`
	RestoreEpoch  string            `json:"restore_epoch"`
	IntentID      string            `json:"intent_id"`
	SandboxID     string            `json:"sandbox_id"`
	WorkspaceHash string            `json:"workspace_hash"`
	Generation    int64             `json:"generation"`
	OperationID   string            `json:"operation_id"`
	PayloadDigest string            `json:"payload_digest"`
	Snapshot      SnapshotReference `json:"snapshot"`
	ExpiresAt     time.Time         `json:"expires_at"`
	Runtime       RuntimeReference  `json:"runtime"`
	WorkspaceMode string            `json:"workspace_mode"`
	PublicKey     []byte            `json:"public_key"`
	NotBefore     time.Time         `json:"not_before"`
	NotAfter      time.Time         `json:"not_after"`
}

type ReadyReceiptClaims struct {
	Version           uint32         `json:"version"`
	CertificateDigest string         `json:"certificate_digest"`
	Claim             ClaimReference `json:"claim"`
	DataGateEpoch     int64          `json:"data_gate_epoch"`
	GateState         string         `json:"gate_state"`
	MountAttempt      uint8          `json:"mount_attempt"`
	MountOperationID  string         `json:"mount_operation_id"`
	ObservedAt        time.Time      `json:"observed_at"`
	ValidUntil        time.Time      `json:"valid_until"`
}

// PublicationVerifier pins immutable authority trust, independent of requests.
type PublicationVerifier struct {
	binding TrustBinding
	roots   map[string]ed25519.PublicKey
}

// PublicationEvidence contains authenticated, context-bound, copied data.
// Historical verification does not authorize a new publication.
type PublicationEvidence struct {
	runtime      RuntimeReference
	mountAttempt uint8
	wire         []byte
	digest       string
}

type CertificateEvidence struct {
	runtime       RuntimeReference
	workspaceMode string
	wire          []byte
	digest        string
}

func (e PublicationEvidence) Runtime() RuntimeReference { return e.runtime }
func (e PublicationEvidence) MountAttempt() uint8       { return e.mountAttempt }
func (e PublicationEvidence) Wire() []byte              { return append([]byte(nil), e.wire...) }
func (e PublicationEvidence) Digest() string            { return e.digest }
func (e CertificateEvidence) Runtime() RuntimeReference { return e.runtime }
func (e CertificateEvidence) WorkspaceMode() string     { return e.workspaceMode }
func (e CertificateEvidence) Wire() []byte              { return append([]byte(nil), e.wire...) }
func (e CertificateEvidence) Digest() string            { return e.digest }

type certificateEnvelope struct {
	Claims    RuntimeCertificateClaims `json:"claims"`
	Signature []byte                   `json:"signature"`
}

type readyEnvelope struct {
	Certificate certificateEnvelope `json:"certificate"`
	Claims      ReadyReceiptClaims  `json:"claims"`
	Signature   []byte              `json:"signature"`
}
