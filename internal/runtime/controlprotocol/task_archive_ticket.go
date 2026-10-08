package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"time"
)

const taskArchiveTicketDomain = "sandbox-task-workspace-archive-ticket:v1\x00"
const maxTaskArchiveTicketWireBytes = 8192

type taskArchiveTicketEnvelope struct {
	Claims    TaskWorkspaceArchiveTicketClaims `json:"claims"`
	Signature []byte                           `json:"signature"`
}

var taskArchiveTicketSchema = &wireSchema{kind: 'o', maxBytes: maxTaskArchiveTicketWireBytes, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{"version": numberField, "purpose": stringField, "context": taskArchiveContextSchema, "not_before": utcField, "not_after": utcField, "session_not_after": utcField}), "signature": stringField,
}}

func validateTaskArchiveTicketClaims(c TaskWorkspaceArchiveTicketClaims) error {
	if err := validateTaskArchiveContext(c.Context); err != nil {
		return err
	}
	if c.Version != 1 || c.Purpose != "task_workspace_archive" {
		return fmt.Errorf("invalid task archive ticket claims")
	}
	return validateTaskArchiveWindow(c.NotBefore, c.NotAfter, c.SessionNotAfter)
}

func matchTaskArchiveIssuer(c TaskWorkspaceArchiveTicketClaims, issuer CommandIssuerIdentity) error {
	return matchTaskCloseIssuer(TaskCloseDataTicketClaims{Context: c.Context.Current, NotBefore: c.NotBefore, NotAfter: c.SessionNotAfter}, issuer)
}

// SignTaskWorkspaceArchiveTicket signs supplied attribution, not live metadata
// or producer authority. The fixed session must fit the verified issuer.
func SignTaskWorkspaceArchiveTicket(key ed25519.PrivateKey, issuer CommandIssuerIdentity, c TaskWorkspaceArchiveTicketClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err = validateTaskArchiveTicketClaims(c); err != nil {
		return nil, err
	}
	if err = validateExecStartIssuerIdentity(issuer); err != nil {
		return nil, err
	}
	if !bytes.Equal(private[ed25519.SeedSize:], issuer.publicKey) {
		return nil, fmt.Errorf("task archive signer does not match issuer delegate")
	}
	if err = matchTaskArchiveIssuer(c, issuer); err != nil {
		return nil, err
	}
	signed, err := signingBytes(taskArchiveTicketDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(taskArchiveTicketEnvelope{c, ed25519.Sign(private, signed)}, maxTaskArchiveTicketWireBytes)
}

func (v *ManagementVerifier) VerifyTaskWorkspaceArchiveTicket(wire, issuerWire []byte, expected TaskWorkspaceArchiveContext, now time.Time) (TaskWorkspaceArchiveEvidence, error) {
	fail := func(err error) (TaskWorkspaceArchiveEvidence, error) { return TaskWorkspaceArchiveEvidence{}, err }
	if err := validateTaskArchiveContext(expected); err != nil {
		return fail(err)
	}
	if v == nil || taskCloseBinding(expected.Current) != v.binding {
		return fail(fmt.Errorf("task archive expected binding mismatch"))
	}
	issuer, err := v.VerifyCommandIssuerCertificate(issuerWire, now)
	if err != nil {
		return fail(err)
	}
	var envelope taskArchiveTicketEnvelope
	if err = decodeWire(wire, taskArchiveTicketSchema, &envelope); err != nil {
		return fail(err)
	}
	c := envelope.Claims
	if err = validateTaskArchiveTicketClaims(c); err != nil {
		return fail(err)
	}
	if c.Context != expected {
		return fail(fmt.Errorf("task archive context mismatch"))
	}
	if err = matchTaskArchiveIssuer(c, issuer); err != nil {
		return fail(err)
	}
	signed, err := signingBytes(taskArchiveTicketDomain, c)
	if err != nil {
		return fail(err)
	}
	if len(envelope.Signature) != ed25519.SignatureSize || !ed25519.Verify(issuer.publicKey, signed, envelope.Signature) {
		return fail(fmt.Errorf("invalid task archive ticket signature"))
	}
	if now.Add(-time.Second).Before(c.NotBefore) || !now.Add(time.Second).Before(c.NotAfter) {
		return fail(fmt.Errorf("task archive outside authentication window"))
	}
	normalized, err := encodeWire(envelope, maxTaskArchiveTicketWireBytes)
	if err != nil {
		return fail(err)
	}
	return TaskWorkspaceArchiveEvidence{wire: normalized, digest: wireDigest(normalized), claims: c}, nil
}
