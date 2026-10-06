package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"time"
)

const (
	execStartTicketDomain       = "sandbox-exec-start-ticket:v1\x00"
	maxExecStartTicketWireBytes = 4096
)

var execStartContextSchema = objectSchema(map[string]*wireSchema{
	"namespace": stringField, "authority_id": stringField, "target": stringField, "restore_epoch": stringField,
	"issuer_certificate_id": stringField, "issuer_certificate_digest": stringField,
	"command_id": stringField, "operation_id": stringField, "request_id": stringField, "operation_digest": stringField,
	"sandbox_id": stringField, "workspace_hash": stringField,
	"generation": numberField, "data_gate_epoch": numberField, "control_revision": numberField, "admission_revision": numberField, "lease_id": numberField,
	"runtime": runtimeSchema, "expires_at": utcField,
})
var execStartClaimsSchema = objectSchema(map[string]*wireSchema{
	"version": numberField, "purpose": stringField, "context": execStartContextSchema,
	"descriptor_digest": stringField, "not_before": utcField, "not_after": utcField,
})
var execStartSchema = &wireSchema{kind: 'o', maxBytes: maxExecStartTicketWireBytes, fields: map[string]*wireSchema{
	"claims": execStartClaimsSchema, "signature": stringField,
}}

// SignExecStartTicket signs caller-supplied context and time with a verified
// issuer's independent delegate. It does not derive operation authorization.
func SignExecStartTicket(key ed25519.PrivateKey, issuer CommandIssuerIdentity, claims ExecStartTicketClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err := validateExecStartClaims(claims); err != nil {
		return nil, err
	}
	if err := validateExecStartIssuerIdentity(issuer); err != nil {
		return nil, err
	}
	if !bytes.Equal(private[ed25519.SeedSize:], issuer.publicKey) {
		return nil, fmt.Errorf("exec start signer does not match issuer delegate")
	}
	if err := matchExecStartIssuer(claims, issuer); err != nil {
		return nil, err
	}
	signed, err := signingBytes(execStartTicketDomain, claims)
	if err != nil {
		return nil, err
	}
	return encodeWire(execStartEnvelope{Claims: claims, Signature: ed25519.Sign(private, signed)}, maxExecStartTicketWireBytes)
}

// VerifyExecStartTicket freshly authenticates the issuer against pinned trust,
// then checks the actual descriptor, exact original operation, and bounded clock.
// Its evidence is attribution; consumers must independently authorize effects.
func (v *ManagementVerifier) VerifyExecStartTicket(ticketWire, issuerWire []byte, expected ExecStartContext, descriptor ExecutionDescriptor, now time.Time) (ExecStartEvidence, error) {
	if err := validateExecStartContext(expected); err != nil {
		return ExecStartEvidence{}, err
	}
	if v == nil || len(v.roots) == 0 {
		return ExecStartEvidence{}, fmt.Errorf("unconfigured management verifier")
	}
	if execStartBinding(expected) != v.binding {
		return ExecStartEvidence{}, fmt.Errorf("exec start trust binding mismatch")
	}
	if !validHash(descriptor.Digest()) {
		return ExecStartEvidence{}, fmt.Errorf("missing execution descriptor evidence")
	}
	issuer, err := v.VerifyCommandIssuerCertificate(issuerWire, now)
	if err != nil {
		return ExecStartEvidence{}, err
	}
	if expected.IssuerCertificateID != issuer.certificateID || expected.IssuerCertificateDigest != issuer.digest {
		return ExecStartEvidence{}, fmt.Errorf("exec start expected issuer mismatch")
	}
	var envelope execStartEnvelope
	if err := decodeWire(ticketWire, execStartSchema, &envelope); err != nil {
		return ExecStartEvidence{}, err
	}
	claims := envelope.Claims
	if err := validateExecStartClaims(claims); err != nil {
		return ExecStartEvidence{}, err
	}
	if !equalExecStartContext(claims.Context, expected) || claims.DescriptorDigest != descriptor.Digest() {
		return ExecStartEvidence{}, fmt.Errorf("exec start operation or payload mismatch")
	}
	if err := matchExecStartIssuer(claims, issuer); err != nil {
		return ExecStartEvidence{}, err
	}
	if len(envelope.Signature) != ed25519.SignatureSize {
		return ExecStartEvidence{}, fmt.Errorf("invalid exec start signature length")
	}
	signed, err := signingBytes(execStartTicketDomain, claims)
	if err != nil {
		return ExecStartEvidence{}, err
	}
	if !ed25519.Verify(issuer.publicKey, signed, envelope.Signature) {
		return ExecStartEvidence{}, fmt.Errorf("invalid exec start delegate signature")
	}
	// Fresh issuer authentication above also established a known UTC observation.
	if now.Add(-time.Second).Before(claims.NotBefore) || !now.Add(time.Second).Before(claims.NotAfter) {
		return ExecStartEvidence{}, fmt.Errorf("exec start outside authentication window")
	}
	normalized, err := encodeWire(envelope, maxExecStartTicketWireBytes)
	if err != nil {
		return ExecStartEvidence{}, err
	}
	return ExecStartEvidence{wire: normalized, digest: wireDigest(normalized), context: claims.Context, descriptorDigest: claims.DescriptorDigest, notBefore: claims.NotBefore, notAfter: claims.NotAfter}, nil
}

func execStartBinding(c ExecStartContext) TrustBinding {
	return TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}
}
func validateExecStartContext(c ExecStartContext) error {
	if err := validateBinding(execStartBinding(c)); err != nil {
		return err
	}
	if !validUUID(c.IssuerCertificateID) || !validHash(c.IssuerCertificateDigest) || !validUUID(c.CommandID) || !validUUID(c.OperationID) || !validID(c.RequestID) || !validHash(c.OperationDigest) || !validID(c.SandboxID) || !validHash(c.WorkspaceHash) || c.Generation <= 0 || c.DataGateEpoch <= 0 || c.ControlRevision <= 0 || c.AdmissionRevision <= 0 || c.LeaseID <= 0 || !validRuntime(c.Runtime) || !validUTC(c.ExpiresAt) {
		return fmt.Errorf("invalid exec start context")
	}
	return nil
}
func validateExecStartClaims(c ExecStartTicketClaims) error {
	if err := validateExecStartContext(c.Context); err != nil {
		return err
	}
	if c.Version != 1 || c.Purpose != "operation_exec_start" || !validHash(c.DescriptorDigest) {
		return fmt.Errorf("invalid exec start claims")
	}
	if !validUTC(c.NotBefore) || !validUTC(c.NotAfter) || !c.NotBefore.Before(c.NotAfter) || c.NotAfter.Sub(c.NotBefore) > 30*time.Second || c.NotAfter.After(c.Context.ExpiresAt) {
		return fmt.Errorf("invalid exec start interval")
	}
	return nil
}
func equalExecStartContext(actual, expected ExecStartContext) bool {
	if !actual.ExpiresAt.Equal(expected.ExpiresAt) {
		return false
	}
	// Compare time by UTC instant, avoiding time.Time's representation identity.
	actual.ExpiresAt = time.Time{}
	expected.ExpiresAt = time.Time{}
	return actual == expected
}
func matchExecStartIssuer(c ExecStartTicketClaims, issuer CommandIssuerIdentity) error {
	if c.Context.IssuerCertificateID != issuer.certificateID || c.Context.IssuerCertificateDigest != issuer.digest || c.NotBefore.Before(issuer.notBefore) || c.NotAfter.After(issuer.notAfter) {
		return fmt.Errorf("exec start issuer identity or interval mismatch")
	}
	return nil
}

// Only opaque identity produced by certificate verification is accepted. Check
// its internal consistency before using the copied delegate key for signing;
// this is not a replacement for verifier-side fresh pinned-root authentication.
func validateExecStartIssuerIdentity(issuer CommandIssuerIdentity) error {
	if len(issuer.publicKey) != ed25519.PublicKeySize || !validHash(issuer.digest) || issuer.digest != wireDigest(issuer.wire) || !validUUID(issuer.certificateID) || !validUTC(issuer.notBefore) || !validUTC(issuer.notAfter) || !issuer.notBefore.Before(issuer.notAfter) {
		return fmt.Errorf("missing or inconsistent command issuer evidence")
	}
	var envelope commandIssuerEnvelope
	if err := decodeWire(issuer.wire, commandIssuerSchema, &envelope); err != nil {
		return err
	}
	c := envelope.Claims
	if err := validateCommandIssuerCertificate(c); err != nil {
		return err
	}
	if len(envelope.Signature) != ed25519.SignatureSize || c.CertificateID != issuer.certificateID || !bytes.Equal(c.PublicKey, issuer.publicKey) || !c.NotBefore.Equal(issuer.notBefore) || !c.NotAfter.Equal(issuer.notAfter) {
		return fmt.Errorf("inconsistent command issuer evidence")
	}
	return nil
}
