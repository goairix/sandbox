package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"time"
)

const runtimeIdentityCertificateDomain = "sandbox-runtime-identity-certificate:v1\x00"

var runtimeIdentityClaimsSchema = objectSchema(map[string]*wireSchema{
	"version": numberField, "root_key_id": stringField, "certificate_id": stringField,
	"role": stringField, "namespace": stringField, "authority_id": stringField,
	"target": stringField, "restore_epoch": stringField, "public_key": stringField,
	"not_before": utcField, "not_after": utcField, "sandbox_id": stringField,
	"workspace_hash": stringField, "generation": numberField, "runtime": runtimeSchema,
})
var runtimeIdentitySchema = &wireSchema{kind: 'o', maxBytes: maxManagementCertificateWireBytes, fields: map[string]*wireSchema{
	"claims": runtimeIdentityClaimsSchema, "signature": stringField,
}}

// SignRuntimeIdentityCertificate derives root identity and certifies an exact
// runtime delegate using the independent runtime receipt signature domain.
func SignRuntimeIdentityCertificate(key ed25519.PrivateKey, c RuntimeIdentityCertificateClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	c.PublicKey = append([]byte(nil), c.PublicKey...)
	c.RootKeyID = wireDigest(private[ed25519.SeedSize:])
	if err := validateRuntimeIdentityCertificate(c); err != nil {
		return nil, err
	}
	if bytes.Equal(c.PublicKey, private[ed25519.SeedSize:]) {
		return nil, fmt.Errorf("runtime receipt delegate must be independent of signing root")
	}
	signed, err := signingBytes(runtimeIdentityCertificateDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(runtimeIdentityEnvelope{Claims: c, Signature: ed25519.Sign(private, signed)}, maxManagementCertificateWireBytes)
}

// VerifyRuntimeIdentityCertificate authenticates fresh receipt identity against
// an exact context from trusted state and a trusted UTC observation. Its ongoing
// interval is independent of publication business expiry; it grants no effects.
func (v *ManagementVerifier) VerifyRuntimeIdentityCertificate(wire []byte, context RuntimeIdentityContext, now time.Time) (RuntimeReceiptIdentity, error) {
	var envelope runtimeIdentityEnvelope
	if err := decodeWire(wire, runtimeIdentitySchema, &envelope); err != nil {
		return RuntimeReceiptIdentity{}, err
	}
	c := envelope.Claims
	if err := validateRuntimeIdentityCertificate(c); err != nil {
		return RuntimeReceiptIdentity{}, err
	}
	if err := validateRuntimeIdentityContext(context); err != nil {
		return RuntimeReceiptIdentity{}, err
	}
	if runtimeIdentityContext(c) != context {
		return RuntimeReceiptIdentity{}, fmt.Errorf("runtime identity context mismatch")
	}
	root, err := v.managementCertificateRoot(TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}, c.RootKeyID, c.PublicKey, envelope.Signature, c.NotBefore, c.NotAfter, now)
	if err != nil {
		return RuntimeReceiptIdentity{}, err
	}
	signed, err := signingBytes(runtimeIdentityCertificateDomain, c)
	if err != nil {
		return RuntimeReceiptIdentity{}, err
	}
	if !ed25519.Verify(root, signed, envelope.Signature) {
		return RuntimeReceiptIdentity{}, fmt.Errorf("invalid runtime identity certificate signature")
	}
	normalized, err := encodeWire(envelope, maxManagementCertificateWireBytes)
	if err != nil {
		return RuntimeReceiptIdentity{}, err
	}
	return RuntimeReceiptIdentity{publicKey: append(ed25519.PublicKey(nil), c.PublicKey...), wire: normalized, digest: wireDigest(normalized), certificateID: c.CertificateID, notBefore: c.NotBefore, notAfter: c.NotAfter, context: context}, nil
}

func runtimeIdentityContext(c RuntimeIdentityCertificateClaims) RuntimeIdentityContext {
	return RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}
}
func validateRuntimeIdentityContext(c RuntimeIdentityContext) error {
	if !validID(c.SandboxID) || !validHash(c.WorkspaceHash) || c.Generation <= 0 || !validRuntime(c.Runtime) {
		return fmt.Errorf("invalid runtime identity context")
	}
	return nil
}
func validateRuntimeIdentityCertificate(c RuntimeIdentityCertificateClaims) error {
	if err := validateBinding(TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}); err != nil {
		return err
	}
	if c.Version != 1 || !validHash(c.RootKeyID) || !validUUID(c.CertificateID) || c.Role != "runtime_receipt" || len(c.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid runtime identity certificate claims")
	}
	if !validUTC(c.NotBefore) || !validUTC(c.NotAfter) || !c.NotBefore.Before(c.NotAfter) {
		return fmt.Errorf("invalid runtime identity certificate interval")
	}
	return validateRuntimeIdentityContext(runtimeIdentityContext(c))
}
