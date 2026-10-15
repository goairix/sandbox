package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"time"
)

const enrolledReadyDomain = "runtime_ready_enrolled:v2\x00"

// EnrolledRuntimeEvidence contains untrusted copied diagnostic material. It
// cannot reconstruct a Supervisor, creation claim, or execution capability.
type EnrolledRuntimeEvidence struct {
	Request             NodeEnrollmentRequest
	ObserverCertificate []byte
	Receipt             []byte
}

func (e EnrolledRuntimeEvidence) Clone() EnrolledRuntimeEvidence {
	e.Request.Activation = bytes.Clone(e.Request.Activation)
	e.Request.RuntimeCertificate = bytes.Clone(e.Request.RuntimeCertificate)
	e.Request.IssuerCertificate = bytes.Clone(e.Request.IssuerCertificate)
	e.ObserverCertificate = bytes.Clone(e.ObserverCertificate)
	e.Receipt = bytes.Clone(e.Receipt)
	return e
}

type EnrolledReadyReceiptClaims struct {
	ReadyReceiptClaims
	EnrollmentDigest          string `json:"enrollment_digest"`
	ObserverCertificateDigest string `json:"observer_certificate_digest"`
	ActivationDigest          string `json:"activation_digest"`
}

type enrolledReadyEnvelope struct {
	Certificate certificateEnvelope        `json:"certificate"`
	Claims      EnrolledReadyReceiptClaims `json:"claims"`
	Signature   []byte                     `json:"signature"`
}

type EnrolledPublicationEvidence struct {
	wire        []byte
	ready       EnrolledReadyReceiptClaims
	activation  TargetActivationEvidence
	receipt     NodeEnrollmentReceiptClaims
	certificate CertificateEvidence
}

func (e EnrolledPublicationEvidence) Wire() []byte                         { return bytes.Clone(e.wire) }
func (e EnrolledPublicationEvidence) Digest() string                       { return wireDigest(e.wire) }
func (e EnrolledPublicationEvidence) Ready() EnrolledReadyReceiptClaims    { return e.ready }
func (e EnrolledPublicationEvidence) Activation() TargetActivationEvidence { return e.activation }
func (e EnrolledPublicationEvidence) EnrollmentReceipt() NodeEnrollmentReceiptClaims {
	return e.receipt
}
func (e EnrolledPublicationEvidence) Certificate() CertificateEvidence { return e.certificate }

var enrolledReadySchema = &wireSchema{kind: 'o', maxBytes: maxReadyWireBytes, fields: map[string]*wireSchema{
	"certificate": certificateSchema,
	"claims": objectSchema(map[string]*wireSchema{
		"version": numberField, "certificate_digest": stringField, "claim": claimSchema, "data_gate_epoch": numberField, "gate_state": stringField, "mount_attempt": numberField, "mount_operation_id": stringField, "observed_at": utcField, "valid_until": utcField,
		"enrollment_digest": stringField, "observer_certificate_digest": stringField, "activation_digest": stringField,
	}),
	"signature": stringField,
}}

func validateEnrolledReady(r EnrolledReadyReceiptClaims, c RuntimeCertificateClaims) error {
	legacy := r.ReadyReceiptClaims
	legacy.Version = 1
	if r.Version != 2 || c.WorkspaceMode != "plain" || r.MountAttempt != 0 || r.MountOperationID != "" || !validHash(r.EnrollmentDigest) || !validHash(r.ObserverCertificateDigest) || !validHash(r.ActivationDigest) {
		return fmt.Errorf("invalid enrolled ready claims")
	}
	return validateReady(legacy, c)
}

// SignEnrolledReadyReceipt signs this fixed DTO and domain only. The live
// Supervisor must independently own enrollment, journal, gate and private key.
func SignEnrolledReadyReceipt(key ed25519.PrivateKey, certificate []byte, claims EnrolledReadyReceiptClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	defer clear(private)
	var cert certificateEnvelope
	if err := decodeWire(certificate, certificateSchema, &cert); err != nil {
		return nil, err
	}
	if err := validateCertificate(cert.Claims); err != nil {
		return nil, err
	}
	canonical, err := encodeWire(cert, maxCertificateWireBytes)
	if err != nil || !bytes.Equal(canonical, certificate) || len(cert.Signature) != ed25519.SignatureSize || !bytes.Equal(private[32:], cert.Claims.PublicKey) || claims.CertificateDigest != wireDigest(canonical) {
		return nil, fmt.Errorf("enrolled ready certificate/key mismatch")
	}
	if err := validateEnrolledReady(claims, cert.Claims); err != nil {
		return nil, err
	}
	signed, err := signingBytes(enrolledReadyDomain, claims)
	if err != nil {
		return nil, err
	}
	return encodeWire(enrolledReadyEnvelope{cert, claims, ed25519.Sign(private, signed)}, maxReadyWireBytes)
}

// VerifyEnrolledPublicationCertificate independently authenticates the creation
// certificate and joins its exact runtime/key to a root-verified activation.
// Creation attribution still requires the Backend's original claim/context.
func (v *ManagementVerifier) VerifyEnrolledPublicationCertificate(wire []byte, activation TargetActivationEvidence, now time.Time) (CertificateEvidence, error) {
	if v == nil || len(activation.wire) == 0 {
		return CertificateEvidence{}, fmt.Errorf("missing enrolled activation")
	}
	request := NodeEnrollmentRequest{1, "runtime_node_enrollment", activation.RuntimeCertificate(), activation.Wire(), activation.IssuerCertificate()}
	fresh, err := v.VerifyNodeEnrollmentRequest(request, now)
	if err != nil {
		return CertificateEvidence{}, err
	}
	if fresh.Digest() != activation.Digest() {
		return CertificateEvidence{}, fmt.Errorf("enrolled activation mismatch")
	}
	p := &PublicationVerifier{binding: v.binding, roots: v.roots}
	var cert certificateEnvelope
	if err := decodeWire(wire, certificateSchema, &cert); err != nil {
		return CertificateEvidence{}, err
	}
	e, err := p.verifyCertificate(wire, certificateContext(cert.Claims), &now)
	if err != nil {
		return CertificateEvidence{}, err
	}
	i := fresh.Identity()
	if !bytes.Equal(e.Wire(), wire) || e.WorkspaceMode() != "plain" || cert.Claims.Runtime != i.Runtime || cert.Claims.SandboxID != i.SandboxID || cert.Claims.WorkspaceHash != i.WorkspaceHash || cert.Claims.Generation != i.Generation || !bytes.Equal(e.PublicKey(), fresh.Birth().RuntimePublicKey) {
		return CertificateEvidence{}, fmt.Errorf("enrolled publication identity/key mismatch")
	}
	return e, nil
}

func (v *PublicationVerifier) VerifyEnrolledPublication(wire []byte, context PublicationContext, enrollment EnrolledRuntimeEvidence, contract string, now time.Time) (EnrolledPublicationEvidence, error) {
	return v.verifyEnrolledPublication(wire, context, enrollment, contract, &now)
}

// Historical verification authenticates the retained signed observation times;
// its result is diagnostic and never current publication authority.
func (v *PublicationVerifier) VerifyEnrolledPublicationHistorical(wire []byte, context PublicationContext, enrollment EnrolledRuntimeEvidence, contract string) (EnrolledPublicationEvidence, error) {
	return v.verifyEnrolledPublication(wire, context, enrollment, contract, nil)
}

func (v *PublicationVerifier) verifyEnrolledPublication(wire []byte, context PublicationContext, enrollment EnrolledRuntimeEvidence, contract string, now *time.Time) (EnrolledPublicationEvidence, error) {
	fail := func(err error) (EnrolledPublicationEvidence, error) { return EnrolledPublicationEvidence{}, err }
	if v == nil || !validHash(contract) {
		return fail(fmt.Errorf("unconfigured enrolled publication"))
	}
	var e enrolledReadyEnvelope
	if err := decodeWire(wire, enrolledReadySchema, &e); err != nil {
		return fail(err)
	}
	canonical, err := encodeWire(e, maxReadyWireBytes)
	if err != nil || !bytes.Equal(canonical, wire) {
		return fail(fmt.Errorf("noncanonical enrolled ready"))
	}
	if err := v.authenticateCertificate(e.Certificate, context.Certificate, now); err != nil {
		return fail(err)
	}
	if err := validateEnrolledReady(e.Claims, e.Certificate.Claims); err != nil {
		return fail(err)
	}
	cw, err := encodeWire(e.Certificate, maxCertificateWireBytes)
	if err != nil {
		return fail(err)
	}
	r := e.Claims
	if context.Runtime != e.Certificate.Claims.Runtime || context.CertificateDigest != wireDigest(cw) || r.CertificateDigest != context.CertificateDigest || r.Claim != context.Claim || r.DataGateEpoch != context.DataGateEpoch || context.MountAttempt != 0 || context.MountOperationID != "" {
		return fail(fmt.Errorf("enrolled publication context mismatch"))
	}
	receipt, err := DecodeNodeEnrollmentReceipt(enrollment.Receipt)
	if err != nil {
		return fail(err)
	}
	m := &ManagementVerifier{binding: v.binding, roots: v.roots}
	// Authenticate every management interval at the actual signed enrollment
	// time even for a cold read, then independently check fresh time if required.
	a, err := m.VerifyNodeEnrollmentRequest(enrollment.Request, receipt.Claims.ObservedAt)
	if err != nil {
		return fail(err)
	}
	if !bytes.Equal(enrollment.Request.RuntimeCertificate, a.RuntimeCertificate()) || !bytes.Equal(enrollment.Request.IssuerCertificate, a.IssuerCertificate()) {
		return fail(fmt.Errorf("noncanonical enrolled management certificates"))
	}
	if a.Birth().ContractDigest != contract {
		return fail(fmt.Errorf("unapproved enrolled birth contract"))
	}
	i, rc := a.Identity(), receipt.Claims
	if rc.Runtime != i.Runtime || rc.SandboxID != i.SandboxID || rc.WorkspaceHash != i.WorkspaceHash || rc.Generation != i.Generation || rc.ActivationDigest != a.Digest() || rc.CreationIntentID != context.Certificate.IntentID || r.ActivationDigest != a.Digest() || r.EnrollmentDigest != rc.EnrollmentDigest || r.ObserverCertificateDigest != rc.ObserverCertificateDigest || r.DataGateEpoch != a.DataGateEpoch() || r.ObservedAt.Before(rc.ObservedAt) {
		return fail(fmt.Errorf("enrolled publication receipt attribution mismatch"))
	}
	if _, err := m.VerifyNodeEnrollmentReceipt(enrollment.Receipt, enrollment.ObserverCertificate, rc, rc.ObservedAt); err != nil {
		return fail(err)
	}
	ce, err := m.VerifyEnrolledPublicationCertificate(cw, a, r.ObservedAt)
	if err != nil {
		return fail(err)
	}
	observer, err := m.VerifyNodeObserverCertificate(enrollment.ObserverCertificate, rc.NodeUID, rc.NodeBootID, r.ObservedAt)
	if err != nil {
		return fail(err)
	}
	runtime, err := m.VerifyRuntimeIdentityCertificate(enrollment.Request.RuntimeCertificate, i, r.ObservedAt)
	if err != nil {
		return fail(err)
	}
	issuer, err := m.VerifyCommandIssuerCertificate(enrollment.Request.IssuerCertificate, r.ObservedAt)
	if err != nil {
		return fail(err)
	}
	for _, end := range []time.Time{a.NotAfter(), observer.NotAfter(), runtime.NotAfter(), issuer.NotAfter()} {
		if r.ValidUntil.After(end) {
			return fail(fmt.Errorf("enrolled ready exceeds management interval"))
		}
	}
	signed, err := signingBytes(enrolledReadyDomain, r)
	if err != nil || len(e.Signature) != ed25519.SignatureSize || !ed25519.Verify(ce.PublicKey(), signed, e.Signature) {
		return fail(fmt.Errorf("invalid enrolled ready signature"))
	}
	if now != nil {
		if _, err := m.VerifyNodeEnrollmentRequest(enrollment.Request, *now); err != nil {
			return fail(err)
		}
		if _, err := m.VerifyNodeObserverCertificate(enrollment.ObserverCertificate, rc.NodeUID, rc.NodeBootID, *now); err != nil {
			return fail(err)
		}
		latest := now.Add(2 * time.Second)
		if r.ObservedAt.After(latest) || latest.Sub(r.ObservedAt) > 5*time.Second || !now.Add(time.Second).Before(r.ValidUntil) {
			return fail(fmt.Errorf("enrolled ready outside fresh window"))
		}
	}
	return EnrolledPublicationEvidence{wire: canonical, ready: r, activation: a, receipt: rc, certificate: ce}, nil
}
