package controlprotocol

import (
	"crypto/ed25519"
	"time"
)

// ManagementVerifier pins deployment-configured authority roots and binding.
// Certificates cannot select or change this trust.
type ManagementVerifier struct {
	binding TrustBinding
	roots   map[string]ed25519.PublicKey
}

// CommandIssuerCertificateClaims attributes a delegate to one management role.
// It does not grant an operation, task, or transport capability.
type CommandIssuerCertificateClaims struct {
	Version       uint32    `json:"version"`
	RootKeyID     string    `json:"root_key_id"`
	CertificateID string    `json:"certificate_id"`
	Role          string    `json:"role"`
	Namespace     string    `json:"namespace"`
	AuthorityID   string    `json:"authority_id"`
	Target        string    `json:"target"`
	RestoreEpoch  string    `json:"restore_epoch"`
	PublicKey     []byte    `json:"public_key"`
	NotBefore     time.Time `json:"not_before"`
	NotAfter      time.Time `json:"not_after"`
}

// CommandIssuerIdentity contains copied, freshly authenticated identity evidence.
// It attributes an issuer; later protocols must independently authorize effects.
type CommandIssuerIdentity struct {
	publicKey     ed25519.PublicKey
	wire          []byte
	digest        string
	certificateID string
	notBefore     time.Time
	notAfter      time.Time
}

func (e CommandIssuerIdentity) Wire() []byte { return append([]byte(nil), e.wire...) }
func (e CommandIssuerIdentity) PublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), e.publicKey...)
}
func (e CommandIssuerIdentity) Digest() string        { return e.digest }
func (e CommandIssuerIdentity) CertificateID() string { return e.certificateID }
func (e CommandIssuerIdentity) NotBefore() time.Time  { return e.notBefore }
func (e CommandIssuerIdentity) NotAfter() time.Time   { return e.notAfter }

type commandIssuerEnvelope struct {
	Claims    CommandIssuerCertificateClaims `json:"claims"`
	Signature []byte                         `json:"signature"`
}

// RuntimeIdentityContext identifies an exact incarnation from trusted state.
// Ongoing receipt attribution does not use the business publication expiry.
type RuntimeIdentityContext struct {
	SandboxID, WorkspaceHash string
	Generation               int64
	Runtime                  RuntimeReference
}

// RuntimeIdentityCertificateClaims certifies one runtime receipt delegate.
// It attributes identity without granting execution or operation capability.
type RuntimeIdentityCertificateClaims struct {
	Version       uint32           `json:"version"`
	RootKeyID     string           `json:"root_key_id"`
	CertificateID string           `json:"certificate_id"`
	Role          string           `json:"role"`
	Namespace     string           `json:"namespace"`
	AuthorityID   string           `json:"authority_id"`
	Target        string           `json:"target"`
	RestoreEpoch  string           `json:"restore_epoch"`
	PublicKey     []byte           `json:"public_key"`
	NotBefore     time.Time        `json:"not_before"`
	NotAfter      time.Time        `json:"not_after"`
	SandboxID     string           `json:"sandbox_id"`
	WorkspaceHash string           `json:"workspace_hash"`
	Generation    int64            `json:"generation"`
	Runtime       RuntimeReference `json:"runtime"`
}

// RuntimeReceiptIdentity contains copied, freshly authenticated evidence.
type RuntimeReceiptIdentity struct {
	publicKey           ed25519.PublicKey
	wire                []byte
	digest              string
	certificateID       string
	notBefore, notAfter time.Time
	context             RuntimeIdentityContext
}

func (e RuntimeReceiptIdentity) Wire() []byte { return append([]byte(nil), e.wire...) }
func (e RuntimeReceiptIdentity) PublicKey() ed25519.PublicKey {
	return append(ed25519.PublicKey(nil), e.publicKey...)
}
func (e RuntimeReceiptIdentity) Digest() string                  { return e.digest }
func (e RuntimeReceiptIdentity) CertificateID() string           { return e.certificateID }
func (e RuntimeReceiptIdentity) NotBefore() time.Time            { return e.notBefore }
func (e RuntimeReceiptIdentity) NotAfter() time.Time             { return e.notAfter }
func (e RuntimeReceiptIdentity) Context() RuntimeIdentityContext { return e.context }

type runtimeIdentityEnvelope struct {
	Claims    RuntimeIdentityCertificateClaims `json:"claims"`
	Signature []byte                           `json:"signature"`
}
