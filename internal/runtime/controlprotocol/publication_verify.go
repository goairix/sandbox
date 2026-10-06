package controlprotocol

import (
	"crypto/ed25519"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// NewPublicationVerifier pins one or two deployment-configured authority roots.
// Root rotation is a deployment decision and cannot be requested by a proof.
func NewPublicationVerifier(binding TrustBinding, roots []ed25519.PublicKey) (*PublicationVerifier, error) {
	if err := validateBinding(binding); err != nil {
		return nil, err
	}
	if len(roots) < 1 || len(roots) > 2 {
		return nil, fmt.Errorf("publication requires one or two pinned roots")
	}
	v := &PublicationVerifier{binding: binding, roots: make(map[string]ed25519.PublicKey, len(roots))}
	for _, root := range roots {
		if len(root) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid publication root key length")
		}
		key := append(ed25519.PublicKey(nil), root...)
		v.roots[wireDigest(key)] = key
	}
	return v, nil
}

func (v *PublicationVerifier) VerifyCertificate(wire []byte, context CertificateContext, now time.Time) (CertificateEvidence, error) {
	return v.verifyCertificate(wire, context, &now)
}

// VerifyHistoricalCertificate verifies authenticity and context without granting
// a fresh time-sensitive authorization. It is intended for read-only recovery.
func (v *PublicationVerifier) VerifyHistoricalCertificate(wire []byte, context CertificateContext) (CertificateEvidence, error) {
	return v.verifyCertificate(wire, context, nil)
}

func (v *PublicationVerifier) verifyCertificate(wire []byte, context CertificateContext, now *time.Time) (CertificateEvidence, error) {
	var envelope certificateEnvelope
	if err := decodeWire(wire, certificateSchema, &envelope); err != nil {
		return CertificateEvidence{}, err
	}
	if err := v.authenticateCertificate(envelope, context, now); err != nil {
		return CertificateEvidence{}, err
	}
	normalized, err := encodeWire(envelope, maxCertificateWireBytes)
	if err != nil {
		return CertificateEvidence{}, err
	}
	return CertificateEvidence{runtime: envelope.Claims.Runtime, workspaceMode: envelope.Claims.WorkspaceMode, wire: normalized, digest: wireDigest(normalized)}, nil
}

func (v *PublicationVerifier) Verify(wire []byte, context PublicationContext, now time.Time) (PublicationEvidence, error) {
	return v.verify(wire, context, &now)
}

// VerifyHistorical preserves signatures, schema, context and signed time-window
// constraints, while ignoring current time. Its evidence is read-only.
func (v *PublicationVerifier) VerifyHistorical(wire []byte, context PublicationContext) (PublicationEvidence, error) {
	return v.verify(wire, context, nil)
}

func (v *PublicationVerifier) verify(wire []byte, context PublicationContext, now *time.Time) (PublicationEvidence, error) {
	var envelope readyEnvelope
	if err := decodeWire(wire, readySchema, &envelope); err != nil {
		return PublicationEvidence{}, err
	}
	if err := v.authenticateCertificate(envelope.Certificate, context.Certificate, now); err != nil {
		return PublicationEvidence{}, err
	}
	cert := envelope.Certificate.Claims
	if err := validateReady(envelope.Claims, cert); err != nil {
		return PublicationEvidence{}, err
	}
	certificate, err := encodeWire(envelope.Certificate, maxCertificateWireBytes)
	if err != nil {
		return PublicationEvidence{}, err
	}
	digest := wireDigest(certificate)
	ready := envelope.Claims
	if !validRuntime(context.Runtime) || !validHash(context.CertificateDigest) || cert.Runtime != context.Runtime || digest != context.CertificateDigest || ready.CertificateDigest != digest || ready.Claim != context.Claim || ready.DataGateEpoch != context.DataGateEpoch || ready.MountAttempt != context.MountAttempt || ready.MountOperationID != context.MountOperationID {
		return PublicationEvidence{}, fmt.Errorf("publication context mismatch")
	}
	if len(envelope.Signature) != ed25519.SignatureSize {
		return PublicationEvidence{}, fmt.Errorf("invalid ready signature length")
	}
	signed, err := signingBytes(readyDomain, ready)
	if err != nil {
		return PublicationEvidence{}, err
	}
	if !ed25519.Verify(cert.PublicKey, signed, envelope.Signature) {
		return PublicationEvidence{}, fmt.Errorf("invalid ready signature")
	}
	if now != nil {
		// The certificate check already establishes a known UTC clock and expiry.
		latest := now.Add(2 * time.Second)
		if ready.ObservedAt.After(latest) || latest.Sub(ready.ObservedAt) > 5*time.Second || !now.Add(time.Second).Before(ready.ValidUntil) {
			return PublicationEvidence{}, fmt.Errorf("ready proof outside authorization window")
		}
	}
	normalized, err := encodeWire(envelope, maxReadyWireBytes)
	if err != nil {
		return PublicationEvidence{}, err
	}
	return PublicationEvidence{runtime: cert.Runtime, mountAttempt: ready.MountAttempt, wire: normalized, digest: wireDigest(normalized)}, nil
}

func (v *PublicationVerifier) authenticateCertificate(envelope certificateEnvelope, context CertificateContext, now *time.Time) error {
	if v == nil || len(v.roots) == 0 {
		return fmt.Errorf("unconfigured publication verifier")
	}
	claims := envelope.Claims
	if err := validateCertificate(claims); err != nil {
		return err
	}
	if err := validateContext(context); err != nil {
		return err
	}
	if certificateContext(claims) != context || (TrustBinding{claims.Namespace, claims.AuthorityID, claims.Target, claims.RestoreEpoch}) != v.binding {
		return fmt.Errorf("certificate context mismatch")
	}
	root, ok := v.roots[claims.RootKeyID]
	if !ok {
		return fmt.Errorf("untrusted runtime certificate root")
	}
	if len(root) != ed25519.PublicKeySize || len(envelope.Signature) != ed25519.SignatureSize {
		return fmt.Errorf("invalid certificate key or signature length")
	}
	signed, err := signingBytes(certificateDomain, claims)
	if err != nil {
		return err
	}
	if !ed25519.Verify(root, signed, envelope.Signature) {
		return fmt.Errorf("invalid runtime certificate signature")
	}
	if now != nil {
		if !validUTC(*now) {
			return fmt.Errorf("publication clock must be known UTC")
		}
		if now.Add(-time.Second).Before(claims.NotBefore) || !now.Add(time.Second).Before(claims.NotAfter) || !now.Add(time.Second).Before(claims.ExpiresAt) {
			return fmt.Errorf("certificate outside authorization window")
		}
	}
	return nil
}

func certificateContext(c RuntimeCertificateClaims) CertificateContext {
	return CertificateContext{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, IntentID: c.IntentID, SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, OperationID: c.OperationID, PayloadDigest: c.PayloadDigest, Snapshot: c.Snapshot, ExpiresAt: c.ExpiresAt}
}

func validateCertificate(c RuntimeCertificateClaims) error {
	if err := validateContext(certificateContext(c)); err != nil {
		return err
	}
	if c.Version != 1 || !validHash(c.RootKeyID) || !validRuntime(c.Runtime) || (c.WorkspaceMode != "plain" && c.WorkspaceMode != "fuse") || len(c.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid runtime certificate claims")
	}
	if !validUTC(c.NotBefore) || !validUTC(c.NotAfter) || !c.NotBefore.Before(c.NotAfter) || c.NotAfter.After(c.ExpiresAt) {
		return fmt.Errorf("invalid runtime certificate interval")
	}
	return nil
}

func validateContext(c CertificateContext) error {
	if err := validateBinding(TrustBinding{c.Namespace, c.AuthorityID, c.Target, c.RestoreEpoch}); err != nil {
		return err
	}
	if !validID(c.IntentID) || !validID(c.SandboxID) || !validHash(c.WorkspaceHash) || c.Generation <= 0 || !validUUID(c.OperationID) || !validHash(c.PayloadDigest) || !validID(c.Snapshot.Version) || !validHash(c.Snapshot.Digest) || !validUTC(c.ExpiresAt) {
		return fmt.Errorf("invalid runtime certificate context")
	}
	return nil
}

func validateBinding(b TrustBinding) error {
	if !validNamespace(b.Namespace) || !validOpaque(b.AuthorityID) || !validOpaque(b.Target) || !validID(b.RestoreEpoch) {
		return fmt.Errorf("invalid runtime trust binding")
	}
	return nil
}

func validateReady(r ReadyReceiptClaims, c RuntimeCertificateClaims) error {
	if r.Version != 1 || !validHash(r.CertificateDigest) || !validUUID(r.Claim.ClaimID) || r.Claim.CreateRevision <= 0 || r.Claim.LeaseID <= 0 || r.DataGateEpoch <= 0 || r.GateState != "open" {
		return fmt.Errorf("invalid ready receipt claims")
	}
	switch r.MountAttempt {
	case 0:
		if r.MountOperationID != "" || c.WorkspaceMode != "plain" {
			return fmt.Errorf("invalid plain publication mount")
		}
	case 1:
		if !validUUID(r.MountOperationID) || c.WorkspaceMode != "fuse" {
			return fmt.Errorf("invalid FUSE publication mount")
		}
	default:
		return fmt.Errorf("invalid mount attempt")
	}
	if !validUTC(r.ObservedAt) || !validUTC(r.ValidUntil) || r.ObservedAt.Before(c.NotBefore) || !r.ObservedAt.Before(r.ValidUntil) || r.ValidUntil.After(c.NotAfter) || r.ValidUntil.Sub(r.ObservedAt) > 5*time.Second {
		return fmt.Errorf("invalid ready receipt interval")
	}
	return nil
}

func validUTC(t time.Time) bool { return !t.IsZero() && t.Location() == time.UTC }
func validRuntime(r RuntimeReference) bool {
	return validOpaque(r.ID) && validOpaque(r.UID) && validOpaque(r.BootID)
}
func validOpaque(s string) bool {
	if len(s) == 0 || len(s) > 128 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validID(s string) bool { return len(s) <= 128 && validSegment(s) }
func validSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func validNamespace(s string) bool {
	if len(s) < 2 || len(s) > 512 || !strings.HasPrefix(s, "/") || !strings.HasSuffix(s, "/") {
		return false
	}
	parts := strings.Split(s[1:len(s)-1], "/")
	if len(parts) < 3 {
		return false
	}
	for _, part := range parts {
		if !validSegment(part) {
			return false
		}
	}
	return len(parts[len(parts)-2]) <= 128 && len(parts[len(parts)-1]) <= 128
}
func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
func validUUID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id != uuid.Nil && id.String() == s
}
