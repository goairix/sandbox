package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"time"
)

const taskQuiescenceTicketDomain = "sandbox-task-quiesce-users-ticket:v1\x00"
const maxTaskQuiescenceTicketWireBytes = 8192

type taskQuiescenceTicketEnvelope struct {
	Claims    TaskUserQuiescenceTicketClaims `json:"claims"`
	Signature []byte                         `json:"signature"`
}

var taskQuiescenceTicketSchema = &wireSchema{kind: 'o', maxBytes: maxTaskQuiescenceTicketWireBytes, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{"version": numberField, "purpose": stringField, "context": taskQuiescenceContextSchema, "not_before": utcField, "not_after": utcField}), "signature": stringField,
}}

func validateTaskQuiescenceTicketClaims(c TaskUserQuiescenceTicketClaims) error {
	if err := validateTaskQuiescenceContext(c.Context); err != nil {
		return err
	}
	if c.Version != 1 || c.Purpose != "task_quiesce_users" {
		return fmt.Errorf("invalid task quiescence claims")
	}
	return validateTaskCloseWindow(c.NotBefore, c.NotAfter)
}
func matchTaskQuiescenceIssuer(c TaskUserQuiescenceTicketClaims, i CommandIssuerIdentity) error {
	return matchTaskCloseIssuer(TaskCloseDataTicketClaims{Context: c.Context.Current, NotBefore: c.NotBefore, NotAfter: c.NotAfter}, i)
}

// SignTaskUserQuiescenceTicket signs exactly supplied attribution with the verified
// issuer delegate. It neither derives metadata nor authorizes an effect.
func SignTaskUserQuiescenceTicket(key ed25519.PrivateKey, issuer CommandIssuerIdentity, c TaskUserQuiescenceTicketClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err = validateTaskQuiescenceTicketClaims(c); err != nil {
		return nil, err
	}
	if err = validateExecStartIssuerIdentity(issuer); err != nil {
		return nil, err
	}
	if !bytes.Equal(private[ed25519.SeedSize:], issuer.publicKey) {
		return nil, fmt.Errorf("task quiescence signer does not match issuer delegate")
	}
	if err = matchTaskQuiescenceIssuer(c, issuer); err != nil {
		return nil, err
	}
	signed, err := signingBytes(taskQuiescenceTicketDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(taskQuiescenceTicketEnvelope{c, ed25519.Sign(private, signed)}, maxTaskQuiescenceTicketWireBytes)
}
func (v *ManagementVerifier) VerifyTaskUserQuiescenceTicket(wire, issuerWire []byte, expected TaskUserQuiescenceContext, now time.Time) (TaskUserQuiescenceEvidence, error) {
	fail := func(err error) (TaskUserQuiescenceEvidence, error) { return TaskUserQuiescenceEvidence{}, err }
	if err := validateTaskQuiescenceContext(expected); err != nil {
		return fail(err)
	}
	if v == nil || taskCloseBinding(expected.Current) != v.binding {
		return fail(fmt.Errorf("task quiescence expected binding mismatch"))
	}
	issuer, err := v.VerifyCommandIssuerCertificate(issuerWire, now)
	if err != nil {
		return fail(err)
	}
	var envelope taskQuiescenceTicketEnvelope
	if err = decodeWire(wire, taskQuiescenceTicketSchema, &envelope); err != nil {
		return fail(err)
	}
	c := envelope.Claims
	if err = validateTaskQuiescenceTicketClaims(c); err != nil {
		return fail(err)
	}
	if c.Context != expected {
		return fail(fmt.Errorf("task quiescence context mismatch"))
	}
	if err = matchTaskQuiescenceIssuer(c, issuer); err != nil {
		return fail(err)
	}
	signed, err := signingBytes(taskQuiescenceTicketDomain, c)
	if err != nil {
		return fail(err)
	}
	if len(envelope.Signature) != ed25519.SignatureSize || !ed25519.Verify(issuer.publicKey, signed, envelope.Signature) {
		return fail(fmt.Errorf("invalid task quiescence signature"))
	}
	if now.Add(-time.Second).Before(c.NotBefore) || !now.Add(time.Second).Before(c.NotAfter) {
		return fail(fmt.Errorf("task quiescence outside authentication window"))
	}
	normalized, err := encodeWire(envelope, maxTaskQuiescenceTicketWireBytes)
	if err != nil {
		return fail(err)
	}
	return TaskUserQuiescenceEvidence{wire: normalized, digest: wireDigest(normalized), claims: c}, nil
}
