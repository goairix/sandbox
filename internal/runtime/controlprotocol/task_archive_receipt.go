package controlprotocol

import (
	"crypto/ed25519"
	"fmt"
	"time"
)

const taskArchiveReceiptDomain = "sandbox-task-workspace-archive-complete:v1\x00"
const maxTaskArchiveReceiptWireBytes = 16384

type taskArchiveReceiptEnvelope struct {
	Claims    TaskWorkspaceArchiveReceiptClaims `json:"claims"`
	Signature []byte                            `json:"signature"`
}

var taskArchiveReceiptSchema = &wireSchema{kind: 'o', maxBytes: maxTaskArchiveReceiptWireBytes, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{"version": numberField, "state": stringField, "context": taskArchiveContextSchema, "ticket_digest": stringField, "not_before": utcField, "not_after": utcField, "session_not_after": utcField, "archive_bytes": numberField, "entry_count": numberField, "sha256": stringField}), "signature": stringField,
}}

func validateTaskArchiveReceiptClaims(c TaskWorkspaceArchiveReceiptClaims) error {
	if err := validateTaskArchiveContext(c.Context); err != nil {
		return err
	}
	if c.Version != 1 || c.State != "archive_complete" || !validHash(c.TicketDigest) || !validHash(c.SHA256) || c.ArchiveBytes < 1536 || c.ArchiveBytes > 1<<40 || c.ArchiveBytes%512 != 0 || c.EntryCount == 0 || c.EntryCount > 1000000 {
		return fmt.Errorf("invalid task archive receipt summary")
	}
	return validateTaskArchiveWindow(c.NotBefore, c.NotAfter, c.SessionNotAfter)
}

// SignTaskWorkspaceArchiveReceipt asserts attribution only. A future original
// producer must establish actual archive completion before calling it.
func SignTaskWorkspaceArchiveReceipt(key ed25519.PrivateKey, c TaskWorkspaceArchiveReceiptClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err = validateTaskArchiveReceiptClaims(c); err != nil {
		return nil, err
	}
	signed, err := signingBytes(taskArchiveReceiptDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(taskArchiveReceiptEnvelope{c, ed25519.Sign(private, signed)}, maxTaskArchiveReceiptWireBytes)
}

// VerifyTaskWorkspaceArchiveReceipt authenticates historical attribution against
// the exact currently certified runtime. Expiry never renews a sending session.
func (v *ManagementVerifier) VerifyTaskWorkspaceArchiveReceipt(wire, runtimeCertificate []byte, expected TaskWorkspaceArchiveContext, ticketDigest string, notBefore, notAfter, sessionNotAfter, now time.Time) (TaskWorkspaceArchiveReceiptEvidence, error) {
	fail := func(err error) (TaskWorkspaceArchiveReceiptEvidence, error) {
		return TaskWorkspaceArchiveReceiptEvidence{}, err
	}
	if err := validateTaskArchiveContext(expected); err != nil {
		return fail(err)
	}
	if !validHash(ticketDigest) {
		return fail(fmt.Errorf("invalid expected task archive ticket digest"))
	}
	if err := validateTaskArchiveWindow(notBefore, notAfter, sessionNotAfter); err != nil {
		return fail(err)
	}
	if v == nil || taskCloseBinding(expected.Current) != v.binding {
		return fail(fmt.Errorf("task archive expected binding mismatch"))
	}
	identity := RuntimeIdentityContext{SandboxID: expected.Current.SandboxID, WorkspaceHash: expected.Current.WorkspaceHash, Generation: expected.Current.Generation, Runtime: expected.Current.Runtime}
	runtime, err := v.VerifyRuntimeIdentityCertificate(runtimeCertificate, identity, now)
	if err != nil {
		return fail(err)
	}
	var envelope taskArchiveReceiptEnvelope
	if err = decodeWire(wire, taskArchiveReceiptSchema, &envelope); err != nil {
		return fail(err)
	}
	c := envelope.Claims
	if err = validateTaskArchiveReceiptClaims(c); err != nil {
		return fail(err)
	}
	if c.Context != expected || c.TicketDigest != ticketDigest || !c.NotBefore.Equal(notBefore) || !c.NotAfter.Equal(notAfter) || !c.SessionNotAfter.Equal(sessionNotAfter) {
		return fail(fmt.Errorf("task archive original attribution mismatch"))
	}
	if c.NotBefore.Before(runtime.notBefore) || c.SessionNotAfter.After(runtime.notAfter) {
		return fail(fmt.Errorf("task archive session outside runtime certificate"))
	}
	signed, err := signingBytes(taskArchiveReceiptDomain, c)
	if err != nil {
		return fail(err)
	}
	if len(envelope.Signature) != ed25519.SignatureSize || !ed25519.Verify(runtime.publicKey, signed, envelope.Signature) {
		return fail(fmt.Errorf("invalid task archive receipt signature"))
	}
	normalized, err := encodeWire(envelope, maxTaskArchiveReceiptWireBytes)
	if err != nil {
		return fail(err)
	}
	return TaskWorkspaceArchiveReceiptEvidence{wire: normalized, claims: c}, nil
}
