package controlprotocol

import (
	"crypto/ed25519"
	"fmt"
	"time"
)

const taskDataClosedReceiptDomain = "sandbox-task-data-closed-receipt:v1\x00"
const maxTaskDataClosedReceiptWireBytes = 8192

type taskDataClosedEnvelope struct {
	Claims    TaskDataClosedReceiptClaims `json:"claims"`
	Signature []byte                      `json:"signature"`
}

var taskDataClosedSchema = &wireSchema{kind: 'o', maxBytes: maxTaskDataClosedReceiptWireBytes, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{"version": numberField, "state": stringField, "context": taskCloseContextSchema, "ticket_digest": stringField, "not_before": utcField, "not_after": utcField}), "signature": stringField,
}}

func validateTaskDataClosedClaims(c TaskDataClosedReceiptClaims) error {
	if err := validateTaskCloseContext(c.Context); err != nil {
		return err
	}
	if c.Version != 1 || c.State != "data_closed" || !validHash(c.TicketDigest) {
		return fmt.Errorf("invalid task data closed receipt")
	}
	return validateTaskCloseWindow(c.NotBefore, c.NotAfter)
}

// SignTaskDataClosedReceipt requires the caller to have durably closed exactly
// this admission gate. A signature cannot establish that physical observation.
func SignTaskDataClosedReceipt(key ed25519.PrivateKey, c TaskDataClosedReceiptClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err = validateTaskDataClosedClaims(c); err != nil {
		return nil, err
	}
	signed, err := signingBytes(taskDataClosedReceiptDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(taskDataClosedEnvelope{c, ed25519.Sign(private, signed)}, maxTaskDataClosedReceiptWireBytes)
}

// VerifyTaskDataClosedReceipt verifies historical attribution against a freshly
// authenticated exact runtime. Ticket expiry is allowed; no live authority is returned.
func (v *ManagementVerifier) VerifyTaskDataClosedReceipt(wire, runtimeCertificate []byte, expected TaskCloseDataContext, ticketDigest string, notBefore, notAfter, now time.Time) (TaskDataClosedReceiptEvidence, error) {
	fail := func(err error) (TaskDataClosedReceiptEvidence, error) { return TaskDataClosedReceiptEvidence{}, err }
	if err := validateTaskCloseContext(expected); err != nil {
		return fail(err)
	}
	if !validHash(ticketDigest) {
		return fail(fmt.Errorf("invalid expected task ticket digest"))
	}
	if err := validateTaskCloseWindow(notBefore, notAfter); err != nil {
		return fail(err)
	}
	if v == nil || taskCloseBinding(expected) != v.binding {
		return fail(fmt.Errorf("task closed expected binding mismatch"))
	}
	identity := RuntimeIdentityContext{SandboxID: expected.SandboxID, WorkspaceHash: expected.WorkspaceHash, Generation: expected.Generation, Runtime: expected.Runtime}
	runtime, err := v.VerifyRuntimeIdentityCertificate(runtimeCertificate, identity, now)
	if err != nil {
		return fail(err)
	}
	var envelope taskDataClosedEnvelope
	if err = decodeWire(wire, taskDataClosedSchema, &envelope); err != nil {
		return fail(err)
	}
	c := envelope.Claims
	if err = validateTaskDataClosedClaims(c); err != nil {
		return fail(err)
	}
	if c.Context != expected || c.TicketDigest != ticketDigest || !c.NotBefore.Equal(notBefore) || !c.NotAfter.Equal(notAfter) {
		return fail(fmt.Errorf("task closed original attribution mismatch"))
	}
	if c.NotBefore.Before(runtime.notBefore) || c.NotAfter.After(runtime.notAfter) {
		return fail(fmt.Errorf("task closed window outside runtime certificate"))
	}
	signed, err := signingBytes(taskDataClosedReceiptDomain, c)
	if err != nil {
		return fail(err)
	}
	if len(envelope.Signature) != ed25519.SignatureSize || !ed25519.Verify(runtime.publicKey, signed, envelope.Signature) {
		return fail(fmt.Errorf("invalid task data closed signature"))
	}
	normalized, err := encodeWire(envelope, maxTaskDataClosedReceiptWireBytes)
	if err != nil {
		return fail(err)
	}
	return TaskDataClosedReceiptEvidence{wire: normalized, claims: c}, nil
}
