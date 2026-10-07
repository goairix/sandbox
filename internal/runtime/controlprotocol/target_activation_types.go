package controlprotocol

import "time"

// BirthContext is independently supplied local birth and original user/security
// configuration. It must never be populated from an untrusted activation claim.
type BirthContext struct {
	BootID           string
	RuntimePublicKey []byte
	UID, GID         uint32
	NetworkAllowed   bool
	ContractDigest   string
}

// TargetActivationClaims certifies a creation-bound runtime tuple. It does not
// authorize an operation, open a live gate, or grant permission to execute.
type TargetActivationClaims struct {
	Version                  uint32                 `json:"version"`
	Role                     string                 `json:"role"`
	RootKeyID                string                 `json:"root_key_id"`
	ActivationID             string                 `json:"activation_id"`
	Binding                  TrustBinding           `json:"binding"`
	Identity                 RuntimeIdentityContext `json:"identity"`
	DataGateEpoch            int64                  `json:"data_gate_epoch"`
	UID                      uint32                 `json:"uid"`
	GID                      uint32                 `json:"gid"`
	NetworkAllowed           bool                   `json:"network_allowed"`
	ContractDigest           string                 `json:"contract_digest"`
	RuntimeCertificateDigest string                 `json:"runtime_certificate_digest"`
	IssuerCertificateDigest  string                 `json:"issuer_certificate_digest"`
	NotBefore                time.Time              `json:"not_before"`
	NotAfter                 time.Time              `json:"not_after"`
}

// TargetActivationEvidence is pure root certification evidence. A separate live
// supervisor must independently install it and authorize every execution.
type TargetActivationEvidence struct {
	claims                                      TargetActivationClaims
	birth                                       BirthContext
	wire, runtimeCertificate, issuerCertificate []byte
	digest                                      string
}

func (e TargetActivationEvidence) Identity() RuntimeIdentityContext { return e.claims.Identity }
func (e TargetActivationEvidence) Birth() BirthContext {
	b := e.birth
	b.RuntimePublicKey = append([]byte(nil), b.RuntimePublicKey...)
	return b
}
func (e TargetActivationEvidence) Binding() TrustBinding { return e.claims.Binding }
func (e TargetActivationEvidence) DataGateEpoch() int64  { return e.claims.DataGateEpoch }
func (e TargetActivationEvidence) ActivationID() string  { return e.claims.ActivationID }
func (e TargetActivationEvidence) Digest() string        { return e.digest }
func (e TargetActivationEvidence) Wire() []byte          { return append([]byte(nil), e.wire...) }
func (e TargetActivationEvidence) RuntimeCertificate() []byte {
	return append([]byte(nil), e.runtimeCertificate...)
}
func (e TargetActivationEvidence) IssuerCertificate() []byte {
	return append([]byte(nil), e.issuerCertificate...)
}
func (e TargetActivationEvidence) NotBefore() time.Time { return e.claims.NotBefore }
func (e TargetActivationEvidence) NotAfter() time.Time  { return e.claims.NotAfter }

type trustBindingWire struct {
	Namespace    string `json:"namespace"`
	AuthorityID  string `json:"authority_id"`
	Target       string `json:"target"`
	RestoreEpoch string `json:"restore_epoch"`
}
type runtimeIdentityContextWire struct {
	SandboxID     string           `json:"sandbox_id"`
	WorkspaceHash string           `json:"workspace_hash"`
	Generation    int64            `json:"generation"`
	Runtime       RuntimeReference `json:"runtime"`
}

// Private wire DTOs preserve existing public structs' serialization.
type targetActivationClaimsWire struct {
	TargetActivationClaims
	Binding  trustBindingWire           `json:"binding"`
	Identity runtimeIdentityContextWire `json:"identity"`
}

func activationClaimsWire(c TargetActivationClaims) targetActivationClaimsWire {
	return targetActivationClaimsWire{TargetActivationClaims: c, Binding: trustBindingWire(c.Binding), Identity: runtimeIdentityContextWire(c.Identity)}
}
func (w targetActivationClaimsWire) claims() TargetActivationClaims {
	c := w.TargetActivationClaims
	c.Binding = TrustBinding(w.Binding)
	c.Identity = RuntimeIdentityContext(w.Identity)
	return c
}

type targetActivationEnvelope struct {
	Claims    targetActivationClaimsWire `json:"claims"`
	Signature []byte                     `json:"signature"`
}
