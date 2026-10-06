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
