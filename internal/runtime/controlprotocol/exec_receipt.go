package controlprotocol

import (
	"crypto/ed25519"
	"fmt"
	"time"
)

// LocalExecReceiptClaims is a runtime's local assertion. It never proves remote
// settlement or authorizes business End. Result scalars require trusted observation.
type LocalExecReceiptClaims struct {
	Version           uint32           `json:"version"`
	State             string           `json:"state"`
	Context           ExecStartContext `json:"context"`
	DescriptorDigest  string           `json:"descriptor_digest"`
	TicketDigest      string           `json:"ticket_digest"`
	NotBefore         time.Time        `json:"not_before"`
	NotAfter          time.Time        `json:"not_after"`
	AuthorityDeadline time.Time        `json:"authority_deadline"`
	RootPID           int              `json:"root_pid"`
	RootWaitStatus    uint32           `json:"root_wait_status"`
	DrainConfirmed    bool             `json:"drain_confirmed"`
	Reason            string           `json:"reason"`
}
type LocalExecReceiptEvidence struct {
	wire   []byte
	claims LocalExecReceiptClaims
}

func (e LocalExecReceiptEvidence) Wire() []byte                 { return append([]byte(nil), e.wire...) }
func (e LocalExecReceiptEvidence) Context() ExecStartContext    { return e.claims.Context }
func (e LocalExecReceiptEvidence) State() string                { return e.claims.State }
func (e LocalExecReceiptEvidence) DescriptorDigest() string     { return e.claims.DescriptorDigest }
func (e LocalExecReceiptEvidence) TicketDigest() string         { return e.claims.TicketDigest }
func (e LocalExecReceiptEvidence) NotBefore() time.Time         { return e.claims.NotBefore }
func (e LocalExecReceiptEvidence) NotAfter() time.Time          { return e.claims.NotAfter }
func (e LocalExecReceiptEvidence) AuthorityDeadline() time.Time { return e.claims.AuthorityDeadline }
func (e LocalExecReceiptEvidence) RootPID() int                 { return e.claims.RootPID }
func (e LocalExecReceiptEvidence) RootWaitStatus() uint32       { return e.claims.RootWaitStatus }
func (e LocalExecReceiptEvidence) DrainConfirmed() bool         { return e.claims.DrainConfirmed }
func (e LocalExecReceiptEvidence) Reason() string               { return e.claims.Reason }

const localExecReceiptDomain = "sandbox-local-exec-receipt:v1\x00"
const maxLocalExecReceiptWireBytes = 8192

var localExecReceiptSchema = &wireSchema{kind: 'o', maxBytes: maxLocalExecReceiptWireBytes, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{"version": numberField, "state": stringField, "context": execStartContextSchema, "descriptor_digest": stringField, "ticket_digest": stringField, "not_before": utcField, "not_after": utcField, "authority_deadline": utcField, "root_pid": numberField, "root_wait_status": numberField, "drain_confirmed": {kind: 'b'}, "reason": stringField}), "signature": stringField,
}}

type localExecReceiptEnvelope struct {
	Claims    LocalExecReceiptClaims `json:"claims"`
	Signature []byte                 `json:"signature"`
}

func validateLocalExecReceipt(c LocalExecReceiptClaims) error {
	if err := validateExecStartClaims(ExecStartTicketClaims{Version: c.Version, Purpose: "operation_exec_start", Context: c.Context, DescriptorDigest: c.DescriptorDigest, NotBefore: c.NotBefore, NotAfter: c.NotAfter}); err != nil {
		return err
	}
	if !validHash(c.TicketDigest) || !validUTC(c.AuthorityDeadline) || c.AuthorityDeadline.Before(c.NotAfter) || c.AuthorityDeadline.After(c.Context.ExpiresAt) {
		return fmt.Errorf("invalid receipt authority deadline or digest")
	}
	switch c.State {
	case "accepted":
		if c.RootPID != 0 || c.RootWaitStatus != 0 || c.DrainConfirmed || c.Reason != "" {
			return fmt.Errorf("accepted receipt contains result")
		}
	case "local_terminal":
		status := c.RootWaitStatus
		exited := status <= 0xffff && status&0xff == 0
		signaled := status&0xffffff00 == 0 && status&0x7f > 0 && status&0x7f < 0x7f
		if c.RootPID < 1 || c.RootPID > 2147483647 || !c.DrainConfirmed || (!exited && !signaled) || !validOpaque(c.Reason) {
			return fmt.Errorf("terminal receipt lacks attributable terminal result")
		}
	case "unknown":
		if c.RootPID != 0 || c.RootWaitStatus != 0 || c.DrainConfirmed || !validOpaque(c.Reason) {
			return fmt.Errorf("invalid unknown receipt")
		}
	default:
		return fmt.Errorf("invalid receipt state")
	}
	return nil
}

// SignLocalExecReceipt requires callers to provide actual trusted supervisor
// assertions. A signature alone cannot establish that physical observation.
func SignLocalExecReceipt(key ed25519.PrivateKey, c LocalExecReceiptClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err = validateLocalExecReceipt(c); err != nil {
		return nil, err
	}
	b, err := signingBytes(localExecReceiptDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(localExecReceiptEnvelope{c, ed25519.Sign(private, b)}, maxLocalExecReceiptWireBytes)
}

// VerifyLocalExecReceipt pins all original accepted attribution, including its
// durable deadline. Old authority windows are diagnostic, not live permission.
func (v *ManagementVerifier) VerifyLocalExecReceipt(wire, runtimeCertificate []byte, expected ExecStartContext, descriptorDigest, ticketDigest string, notBefore, notAfter, authorityDeadline, now time.Time) (LocalExecReceiptEvidence, error) {
	fail := func(err error) (LocalExecReceiptEvidence, error) { return LocalExecReceiptEvidence{}, err }
	if err := validateExecStartContext(expected); err != nil {
		return fail(err)
	}
	if v == nil || execStartBinding(expected) != v.binding {
		return fail(fmt.Errorf("receipt expected binding mismatch"))
	}
	identity := RuntimeIdentityContext{SandboxID: expected.SandboxID, WorkspaceHash: expected.WorkspaceHash, Generation: expected.Generation, Runtime: expected.Runtime}
	runtime, err := v.VerifyRuntimeIdentityCertificate(runtimeCertificate, identity, now)
	if err != nil {
		return fail(err)
	}
	var e localExecReceiptEnvelope
	if err = decodeWire(wire, localExecReceiptSchema, &e); err != nil {
		return fail(err)
	}
	c := e.Claims
	if err = validateLocalExecReceipt(c); err != nil {
		return fail(err)
	}
	if !equalExecStartContext(c.Context, expected) || c.DescriptorDigest != descriptorDigest || c.TicketDigest != ticketDigest || !c.NotBefore.Equal(notBefore) || !c.NotAfter.Equal(notAfter) || !c.AuthorityDeadline.Equal(authorityDeadline) {
		return fail(fmt.Errorf("receipt accepted attribution mismatch"))
	}
	if c.NotBefore.Before(runtime.notBefore) || c.AuthorityDeadline.After(runtime.notAfter) {
		return fail(fmt.Errorf("receipt authority outside runtime certificate"))
	}
	b, err := signingBytes(localExecReceiptDomain, c)
	if err != nil {
		return fail(err)
	}
	if len(e.Signature) != ed25519.SignatureSize || !ed25519.Verify(runtime.publicKey, b, e.Signature) {
		return fail(fmt.Errorf("invalid local receipt signature"))
	}
	normalized, err := encodeWire(e, maxLocalExecReceiptWireBytes)
	if err != nil {
		return fail(err)
	}
	return LocalExecReceiptEvidence{wire: normalized, claims: c}, nil
}
