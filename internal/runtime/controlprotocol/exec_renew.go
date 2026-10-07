package controlprotocol

import (
	"crypto/ed25519"
	"fmt"
	"time"
)

// ExecRenewTicketClaims binds a bounded renewal to an existing accepted command.
type ExecRenewTicketClaims ExecStartTicketClaims

// ExecRenewEvidence is copied attribution, never a reconstructed start handle.
type ExecRenewEvidence struct {
	wire                []byte
	digest              string
	context             ExecStartContext
	descriptorDigest    string
	notBefore, notAfter time.Time
}

func (e ExecRenewEvidence) Wire() []byte              { return append([]byte(nil), e.wire...) }
func (e ExecRenewEvidence) Digest() string            { return e.digest }
func (e ExecRenewEvidence) Context() ExecStartContext { return e.context }
func (e ExecRenewEvidence) DescriptorDigest() string  { return e.descriptorDigest }
func (e ExecRenewEvidence) NotBefore() time.Time      { return e.notBefore }
func (e ExecRenewEvidence) NotAfter() time.Time       { return e.notAfter }

const execRenewTicketDomain = "sandbox-exec-renew-ticket:v1\x00"

type execRenewEnvelope struct {
	Claims    ExecRenewTicketClaims `json:"claims"`
	Signature []byte                `json:"signature"`
}

func validateExecRenewClaims(c ExecRenewTicketClaims) error {
	if c.Purpose != "operation_exec_renew" {
		return fmt.Errorf("invalid renewal purpose")
	}
	start := ExecStartTicketClaims(c)
	start.Purpose = "operation_exec_start"
	return validateExecStartClaims(start)
}

// SignExecRenewTicket signs caller claims; it does not establish live authority.
func SignExecRenewTicket(key ed25519.PrivateKey, c ExecRenewTicketClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err = validateExecRenewClaims(c); err != nil {
		return nil, err
	}
	b, err := signingBytes(execRenewTicketDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(execRenewEnvelope{c, ed25519.Sign(private, b)}, maxExecStartTicketWireBytes)
}

// VerifyExecRenewTicket requires the original accepted context independently of
// the wire and freshly authenticates its selected issuer.
func (v *ManagementVerifier) VerifyExecRenewTicket(wire, issuerCertificate []byte, expected ExecStartContext, descriptorDigest string, now time.Time) (ExecRenewEvidence, error) {
	fail := func(err error) (ExecRenewEvidence, error) { return ExecRenewEvidence{}, err }
	if err := validateExecStartContext(expected); err != nil {
		return fail(err)
	}
	if v == nil || execStartBinding(expected) != v.binding || !validHash(descriptorDigest) {
		return fail(fmt.Errorf("invalid renewal expected binding"))
	}
	issuer, err := v.VerifyCommandIssuerCertificate(issuerCertificate, now)
	if err != nil {
		return fail(err)
	}
	var e execRenewEnvelope
	if err = decodeWire(wire, execStartSchema, &e); err != nil {
		return fail(err)
	}
	c := e.Claims
	if err = validateExecRenewClaims(c); err != nil {
		return fail(err)
	}
	if !equalExecStartContext(c.Context, expected) || c.DescriptorDigest != descriptorDigest {
		return fail(fmt.Errorf("renewal context or descriptor mismatch"))
	}
	if err = matchExecStartIssuer(ExecStartTicketClaims(c), issuer); err != nil {
		return fail(err)
	}
	b, err := signingBytes(execRenewTicketDomain, c)
	if err != nil {
		return fail(err)
	}
	if len(e.Signature) != ed25519.SignatureSize || !ed25519.Verify(issuer.publicKey, b, e.Signature) {
		return fail(fmt.Errorf("invalid renewal signature"))
	}
	if now.Add(-time.Second).Before(c.NotBefore) || !now.Add(time.Second).Before(c.NotAfter) {
		return fail(fmt.Errorf("renewal outside authentication window"))
	}
	normalized, err := encodeWire(e, maxExecStartTicketWireBytes)
	if err != nil {
		return fail(err)
	}
	return ExecRenewEvidence{wire: normalized, digest: wireDigest(normalized), context: c.Context, descriptorDigest: c.DescriptorDigest, notBefore: c.NotBefore, notAfter: c.NotAfter}, nil
}
