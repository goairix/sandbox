package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"time"
)

const (
	commandIssuerCertificateDomain    = "sandbox-command-issuer-certificate:v1\x00"
	maxManagementCertificateWireBytes = 4096
)

var commandIssuerClaimsSchema = objectSchema(map[string]*wireSchema{
	"version": numberField, "root_key_id": stringField, "certificate_id": stringField,
	"role": stringField, "namespace": stringField, "authority_id": stringField,
	"target": stringField, "restore_epoch": stringField, "public_key": stringField,
	"not_before": utcField, "not_after": utcField,
})
var commandIssuerSchema = &wireSchema{kind: 'o', maxBytes: maxManagementCertificateWireBytes, fields: map[string]*wireSchema{
	"claims": commandIssuerClaimsSchema, "signature": stringField,
}}

// NewManagementVerifier copies one or two deployment-pinned authority roots.
// Rotation and removal require a new verifier with updated deployment trust.
func NewManagementVerifier(binding TrustBinding, roots []ed25519.PublicKey) (*ManagementVerifier, error) {
	if err := validateBinding(binding); err != nil {
		return nil, err
	}
	if len(roots) < 1 || len(roots) > 2 {
		return nil, fmt.Errorf("management requires one or two pinned roots")
	}
	v := &ManagementVerifier{binding: binding, roots: make(map[string]ed25519.PublicKey, len(roots))}
	for _, root := range roots {
		if len(root) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid management root key length")
		}
		key := append(ed25519.PublicKey(nil), root...)
		v.roots[wireDigest(key)] = key
	}
	return v, nil
}

// SignCommandIssuerCertificate derives root identity and certifies an
// independent delegate using the command issuer domain.
func SignCommandIssuerCertificate(key ed25519.PrivateKey, c CommandIssuerCertificateClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	c.PublicKey = append([]byte(nil), c.PublicKey...)
	c.RootKeyID = wireDigest(private[ed25519.SeedSize:])
	if err := validateCommandIssuerCertificate(c); err != nil {
		return nil, err
	}
	if bytes.Equal(c.PublicKey, private[ed25519.SeedSize:]) {
		return nil, fmt.Errorf("command issuer delegate must be independent of signing root")
	}
	signed, err := signingBytes(commandIssuerCertificateDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(commandIssuerEnvelope{Claims: c, Signature: ed25519.Sign(private, signed)}, maxManagementCertificateWireBytes)
}

// VerifyCommandIssuerCertificate uses a trusted caller-supplied UTC observation
// and a conservative one-second window. It offers no historical authorization.
func (v *ManagementVerifier) VerifyCommandIssuerCertificate(wire []byte, now time.Time) (CommandIssuerIdentity, error) {
	var envelope commandIssuerEnvelope
	if err := decodeWire(wire, commandIssuerSchema, &envelope); err != nil {
		return CommandIssuerIdentity{}, err
	}
	c := envelope.Claims
	if err := validateCommandIssuerCertificate(c); err != nil {
		return CommandIssuerIdentity{}, err
	}
	root, err := v.managementCertificateRoot(TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}, c.RootKeyID, c.PublicKey, envelope.Signature, c.NotBefore, c.NotAfter, now)
	if err != nil {
		return CommandIssuerIdentity{}, err
	}
	signed, err := signingBytes(commandIssuerCertificateDomain, c)
	if err != nil {
		return CommandIssuerIdentity{}, err
	}
	if !ed25519.Verify(root, signed, envelope.Signature) {
		return CommandIssuerIdentity{}, fmt.Errorf("invalid command issuer certificate signature")
	}
	normalized, err := encodeWire(envelope, maxManagementCertificateWireBytes)
	if err != nil {
		return CommandIssuerIdentity{}, err
	}
	return CommandIssuerIdentity{publicKey: append(ed25519.PublicKey(nil), c.PublicKey...), wire: normalized, digest: wireDigest(normalized), certificateID: c.CertificateID, notBefore: c.NotBefore, notAfter: c.NotAfter}, nil
}

func validateCommandIssuerCertificate(c CommandIssuerCertificateClaims) error {
	if err := validateBinding(TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}); err != nil {
		return err
	}
	if c.Version != 1 || !validHash(c.RootKeyID) || !validUUID(c.CertificateID) || c.Role != "command_issuer" || len(c.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid command issuer certificate claims")
	}
	if !validUTC(c.NotBefore) || !validUTC(c.NotAfter) || !c.NotBefore.Before(c.NotAfter) {
		return fmt.Errorf("invalid command issuer certificate interval")
	}
	return nil
}

// managementCertificateRoot checks deployment binding, pinned trust, delegate
// separation, lengths and freshness. Role-specific callers must first validate
// their claims, then verify their own fixed signature domain with the returned
// root. The returned key belongs to the immutable verifier and stays internal.
func (v *ManagementVerifier) managementCertificateRoot(binding TrustBinding, rootKeyID string, delegate, signature []byte, notBefore, notAfter, now time.Time) (ed25519.PublicKey, error) {
	if v == nil || len(v.roots) == 0 {
		return nil, fmt.Errorf("unconfigured management verifier")
	}
	if binding != v.binding {
		return nil, fmt.Errorf("management certificate trust binding mismatch")
	}
	root, ok := v.roots[rootKeyID]
	if !ok {
		return nil, fmt.Errorf("untrusted management certificate root")
	}
	if len(root) != ed25519.PublicKeySize || len(delegate) != ed25519.PublicKeySize || len(signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("invalid management certificate key or signature length")
	}
	for _, pinned := range v.roots {
		if bytes.Equal(delegate, pinned) {
			return nil, fmt.Errorf("management delegate must be independent of every pinned root")
		}
	}
	if !validUTC(now) {
		return nil, fmt.Errorf("management clock must be known UTC")
	}
	if now.Add(-time.Second).Before(notBefore) || !now.Add(time.Second).Before(notAfter) {
		return nil, fmt.Errorf("management certificate outside authentication window")
	}
	return root, nil
}
