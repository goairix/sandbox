package controlprotocol

import (
	"crypto/ed25519"
	"fmt"
	"time"
)

const taskQuiescenceAcceptedDomain = "sandbox-task-quiesce-users-accepted:v1\x00"
const maxTaskQuiescenceAcceptedWireBytes = 12288

type taskQuiescenceAcceptedEnvelope struct {
	Claims    TaskUserQuiescenceAcceptedClaims `json:"claims"`
	Signature []byte                           `json:"signature"`
}

var taskQuiescenceAcceptedSchema = &wireSchema{kind: 'o', maxBytes: maxTaskQuiescenceAcceptedWireBytes, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{"version": numberField, "state": stringField, "context": taskQuiescenceContextSchema, "ticket_digest": stringField, "not_before": utcField, "not_after": utcField}), "signature": stringField,
}}

func validateTaskQuiescenceAcceptedClaims(c TaskUserQuiescenceAcceptedClaims) error {
	if err := validateTaskQuiescenceContext(c.Context); err != nil {
		return err
	}
	if c.Version != 1 || c.State != "quiescence_accepted" || !validHash(c.TicketDigest) {
		return fmt.Errorf("invalid task quiescence receipt")
	}
	return validateTaskCloseWindow(c.NotBefore, c.NotAfter)
}

// SignTaskUserQuiescenceAccepted signs typed attribution only. The trusted
// producer must establish the corresponding durable and physical preconditions.
func SignTaskUserQuiescenceAccepted(key ed25519.PrivateKey, c TaskUserQuiescenceAcceptedClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err = validateTaskQuiescenceAcceptedClaims(c); err != nil {
		return nil, err
	}
	signed, err := signingBytes(taskQuiescenceAcceptedDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(taskQuiescenceAcceptedEnvelope{c, ed25519.Sign(private, signed)}, maxTaskQuiescenceAcceptedWireBytes)
}

// VerifyTaskUserQuiescenceAccepted verifies historical attribution against a freshly
// authenticated exact runtime. Ticket expiry is allowed; no live authority is returned.
func (v *ManagementVerifier) VerifyTaskUserQuiescenceAccepted(wire, runtimeCertificate []byte, expected TaskUserQuiescenceContext, ticketDigest string, notBefore, notAfter, now time.Time) (TaskUserQuiescenceAcceptedEvidence, error) {
	fail := func(err error) (TaskUserQuiescenceAcceptedEvidence, error) {
		return TaskUserQuiescenceAcceptedEvidence{}, err
	}
	if err := validateTaskQuiescenceContext(expected); err != nil {
		return fail(err)
	}
	if !validHash(ticketDigest) {
		return fail(fmt.Errorf("invalid expected task ticket digest"))
	}
	if err := validateTaskCloseWindow(notBefore, notAfter); err != nil {
		return fail(err)
	}
	if v == nil || taskCloseBinding(expected.Current) != v.binding {
		return fail(fmt.Errorf("task quiescenced expected binding mismatch"))
	}
	identity := RuntimeIdentityContext{SandboxID: expected.Current.SandboxID, WorkspaceHash: expected.Current.WorkspaceHash, Generation: expected.Current.Generation, Runtime: expected.Current.Runtime}
	runtime, err := v.VerifyRuntimeIdentityCertificate(runtimeCertificate, identity, now)
	if err != nil {
		return fail(err)
	}
	var envelope taskQuiescenceAcceptedEnvelope
	if err = decodeWire(wire, taskQuiescenceAcceptedSchema, &envelope); err != nil {
		return fail(err)
	}
	c := envelope.Claims
	if err = validateTaskQuiescenceAcceptedClaims(c); err != nil {
		return fail(err)
	}
	if c.Context != expected || c.TicketDigest != ticketDigest || !c.NotBefore.Equal(notBefore) || !c.NotAfter.Equal(notAfter) {
		return fail(fmt.Errorf("task quiescenced original attribution mismatch"))
	}
	if c.NotBefore.Before(runtime.notBefore) || c.NotAfter.After(runtime.notAfter) {
		return fail(fmt.Errorf("task quiescenced window outside runtime certificate"))
	}
	signed, err := signingBytes(taskQuiescenceAcceptedDomain, c)
	if err != nil {
		return fail(err)
	}
	if len(envelope.Signature) != ed25519.SignatureSize || !ed25519.Verify(runtime.publicKey, signed, envelope.Signature) {
		return fail(fmt.Errorf("invalid task quiescence signature"))
	}
	normalized, err := encodeWire(envelope, maxTaskQuiescenceAcceptedWireBytes)
	if err != nil {
		return fail(err)
	}
	return TaskUserQuiescenceAcceptedEvidence{wire: normalized, claims: c}, nil
}

const taskQuiescenceReceiptDomain = "sandbox-task-users-quiesced-receipt:v1\x00"
const maxTaskQuiescenceReceiptWireBytes = 12288

type taskQuiescenceReceiptEnvelope struct {
	Claims    TaskUserQuiescenceReceiptClaims `json:"claims"`
	Signature []byte                          `json:"signature"`
}

var taskQuiescenceReceiptSchema = &wireSchema{kind: 'o', maxBytes: maxTaskQuiescenceReceiptWireBytes, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{"version": numberField, "state": stringField, "context": taskQuiescenceContextSchema, "ticket_digest": stringField, "not_before": utcField, "not_after": utcField, "execution_set_digest": stringField, "registered_count": numberField, "never_spawned_count": numberField, "local_terminal_count": numberField}), "signature": stringField,
}}

func validateTaskQuiescenceReceiptClaims(c TaskUserQuiescenceReceiptClaims) error {
	if err := validateTaskQuiescenceContext(c.Context); err != nil {
		return err
	}
	if c.Version != 1 || c.State != "users_quiesced" || !validHash(c.TicketDigest) || !validHash(c.ExecutionSetDigest) || c.RegisteredCount > 64 || c.NeverSpawnedCount > 64 || c.LocalTerminalCount > 64 || uint64(c.RegisteredCount) != uint64(c.NeverSpawnedCount)+uint64(c.LocalTerminalCount) {
		return fmt.Errorf("invalid task quiescence receipt")
	}
	return validateTaskCloseWindow(c.NotBefore, c.NotAfter)
}

// SignTaskUserQuiescenceReceipt signs typed attribution only. The trusted
// producer must establish the corresponding durable and physical preconditions.
func SignTaskUserQuiescenceReceipt(key ed25519.PrivateKey, c TaskUserQuiescenceReceiptClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err = validateTaskQuiescenceReceiptClaims(c); err != nil {
		return nil, err
	}
	signed, err := signingBytes(taskQuiescenceReceiptDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(taskQuiescenceReceiptEnvelope{c, ed25519.Sign(private, signed)}, maxTaskQuiescenceReceiptWireBytes)
}

// VerifyTaskUserQuiescenceReceipt verifies historical attribution against a freshly
// authenticated exact runtime. Ticket expiry is allowed; no live authority is returned.
func (v *ManagementVerifier) VerifyTaskUserQuiescenceReceipt(wire, runtimeCertificate []byte, expected TaskUserQuiescenceContext, ticketDigest string, notBefore, notAfter, now time.Time) (TaskUserQuiescenceReceiptEvidence, error) {
	fail := func(err error) (TaskUserQuiescenceReceiptEvidence, error) {
		return TaskUserQuiescenceReceiptEvidence{}, err
	}
	if err := validateTaskQuiescenceContext(expected); err != nil {
		return fail(err)
	}
	if !validHash(ticketDigest) {
		return fail(fmt.Errorf("invalid expected task ticket digest"))
	}
	if err := validateTaskCloseWindow(notBefore, notAfter); err != nil {
		return fail(err)
	}
	if v == nil || taskCloseBinding(expected.Current) != v.binding {
		return fail(fmt.Errorf("task quiescenced expected binding mismatch"))
	}
	identity := RuntimeIdentityContext{SandboxID: expected.Current.SandboxID, WorkspaceHash: expected.Current.WorkspaceHash, Generation: expected.Current.Generation, Runtime: expected.Current.Runtime}
	runtime, err := v.VerifyRuntimeIdentityCertificate(runtimeCertificate, identity, now)
	if err != nil {
		return fail(err)
	}
	var envelope taskQuiescenceReceiptEnvelope
	if err = decodeWire(wire, taskQuiescenceReceiptSchema, &envelope); err != nil {
		return fail(err)
	}
	c := envelope.Claims
	if err = validateTaskQuiescenceReceiptClaims(c); err != nil {
		return fail(err)
	}
	if c.Context != expected || c.TicketDigest != ticketDigest || !c.NotBefore.Equal(notBefore) || !c.NotAfter.Equal(notAfter) {
		return fail(fmt.Errorf("task quiescenced original attribution mismatch"))
	}
	if c.NotBefore.Before(runtime.notBefore) || c.NotAfter.After(runtime.notAfter) {
		return fail(fmt.Errorf("task quiescenced window outside runtime certificate"))
	}
	signed, err := signingBytes(taskQuiescenceReceiptDomain, c)
	if err != nil {
		return fail(err)
	}
	if len(envelope.Signature) != ed25519.SignatureSize || !ed25519.Verify(runtime.publicKey, signed, envelope.Signature) {
		return fail(fmt.Errorf("invalid task quiescence signature"))
	}
	normalized, err := encodeWire(envelope, maxTaskQuiescenceReceiptWireBytes)
	if err != nil {
		return fail(err)
	}
	return TaskUserQuiescenceReceiptEvidence{wire: normalized, claims: c}, nil
}
