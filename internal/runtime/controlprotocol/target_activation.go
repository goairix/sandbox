package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"time"
)

const targetActivationDomain = "sandbox-target-activation:v1\x00"
const maxTargetActivationWireBytes = 16384

var trustBindingSchema = objectSchema(map[string]*wireSchema{"namespace": stringField, "authority_id": stringField, "target": stringField, "restore_epoch": stringField})
var runtimeIdentityContextSchema = objectSchema(map[string]*wireSchema{"sandbox_id": stringField, "workspace_hash": stringField, "generation": numberField, "runtime": runtimeSchema})
var targetActivationSchema = &wireSchema{kind: 'o', maxBytes: maxTargetActivationWireBytes, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{"version": numberField, "role": stringField, "root_key_id": stringField, "activation_id": stringField, "binding": trustBindingSchema, "identity": runtimeIdentityContextSchema, "data_gate_epoch": numberField, "uid": numberField, "gid": numberField, "network_allowed": {kind: 'b'}, "contract_digest": stringField, "runtime_certificate_digest": stringField, "issuer_certificate_digest": stringField, "not_before": utcField, "not_after": utcField}), "signature": stringField,
}}

// SignTargetActivation is for an external creation-bound root attester. Signing
// caller claims alone does not establish their truth or activate a supervisor.
func SignTargetActivation(key ed25519.PrivateKey, claims TargetActivationClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	claims.RootKeyID = wireDigest(private[ed25519.SeedSize:])
	if err := validateTargetActivation(claims); err != nil {
		return nil, err
	}
	c := activationClaimsWire(claims)
	signed, err := signingBytes(targetActivationDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(targetActivationEnvelope{Claims: c, Signature: ed25519.Sign(private, signed)}, maxTargetActivationWireBytes)
}

// VerifyTargetActivation authenticates the root claim before using its runtime
// tuple, then matches independently installed local birth and both delegates.
// The result is certification evidence only; no durable gate or effect changes.
func (v *ManagementVerifier) VerifyTargetActivation(wire, runtimeCertificate, issuerCertificate []byte, birth BirthContext, now time.Time) (TargetActivationEvidence, error) {
	fail := func(err error) (TargetActivationEvidence, error) { return TargetActivationEvidence{}, err }
	var e targetActivationEnvelope
	if err := decodeWire(wire, targetActivationSchema, &e); err != nil {
		return fail(err)
	}
	c := e.Claims.claims()
	if err := validateTargetActivation(c); err != nil {
		return fail(err)
	}
	if v == nil || len(v.roots) == 0 || v.binding != c.Binding {
		return fail(fmt.Errorf("activation trust binding unavailable or mismatched"))
	}
	root := v.roots[c.RootKeyID]
	if len(root) != ed25519.PublicKeySize || len(e.Signature) != ed25519.SignatureSize {
		return fail(fmt.Errorf("untrusted activation root or signature"))
	}
	signed, err := signingBytes(targetActivationDomain, activationClaimsWire(c))
	if err != nil {
		return fail(err)
	}
	if !ed25519.Verify(root, signed, e.Signature) {
		return fail(fmt.Errorf("invalid activation signature"))
	}
	if !validUTC(now) || now.Add(-time.Second).Before(c.NotBefore) || !now.Add(time.Second).Before(c.NotAfter) {
		return fail(fmt.Errorf("activation outside authentication window"))
	}
	if !validUUID(birth.BootID) || birth.BootID != c.Identity.Runtime.BootID || len(birth.RuntimePublicKey) != ed25519.PublicKeySize || birth.UID != c.UID || birth.GID != c.GID || birth.NetworkAllowed != c.NetworkAllowed || birth.ContractDigest != c.ContractDigest {
		return fail(fmt.Errorf("activation does not match independent local birth"))
	}
	runtime, err := v.VerifyRuntimeIdentityCertificate(runtimeCertificate, c.Identity, now)
	if err != nil {
		return fail(err)
	}
	issuer, err := v.VerifyCommandIssuerCertificate(issuerCertificate, now)
	if err != nil {
		return fail(err)
	}
	if !bytes.Equal(runtime.publicKey, birth.RuntimePublicKey) || bytes.Equal(runtime.publicKey, issuer.publicKey) || runtime.digest != c.RuntimeCertificateDigest || issuer.digest != c.IssuerCertificateDigest {
		return fail(fmt.Errorf("activation delegates or certificate digests mismatch"))
	}
	if c.NotBefore.Before(runtime.notBefore) || c.NotBefore.Before(issuer.notBefore) || c.NotAfter.After(runtime.notAfter) || c.NotAfter.After(issuer.notAfter) {
		return fail(fmt.Errorf("activation interval exceeds delegate certificates"))
	}
	normalized, err := encodeWire(targetActivationEnvelope{Claims: activationClaimsWire(c), Signature: e.Signature}, maxTargetActivationWireBytes)
	if err != nil {
		return fail(err)
	}
	birth.RuntimePublicKey = append([]byte(nil), birth.RuntimePublicKey...)
	return TargetActivationEvidence{claims: c, birth: birth, wire: normalized, runtimeCertificate: runtime.Wire(), issuerCertificate: issuer.Wire(), digest: wireDigest(normalized)}, nil
}
func validateTargetActivation(c TargetActivationClaims) error {
	if err := validateBinding(c.Binding); err != nil {
		return err
	}
	if err := validateRuntimeIdentityContext(c.Identity); err != nil {
		return err
	}
	if c.Version != 1 || c.Role != "target_activation" || !validHash(c.RootKeyID) || !validUUID(c.ActivationID) || !validUUID(c.Identity.Runtime.BootID) || c.DataGateEpoch <= 0 || c.UID == 0 || c.UID > 2147483647 || c.GID == 0 || c.GID > 2147483647 || !validHash(c.ContractDigest) || !validHash(c.RuntimeCertificateDigest) || !validHash(c.IssuerCertificateDigest) {
		return fmt.Errorf("invalid target activation claims")
	}
	if !validUTC(c.NotBefore) || !validUTC(c.NotAfter) || !c.NotBefore.Before(c.NotAfter) || c.NotAfter.Sub(c.NotBefore) > time.Hour {
		return fmt.Errorf("invalid target activation interval")
	}
	return nil
}
