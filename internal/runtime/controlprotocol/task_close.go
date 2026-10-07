package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"time"
)

const taskCloseDataTicketDomain = "sandbox-task-close-data-ticket:v1\x00"
const maxTaskCloseDataTicketWireBytes = 4096

type taskCloseDataEnvelope struct {
	Claims    TaskCloseDataTicketClaims `json:"claims"`
	Signature []byte                    `json:"signature"`
}

var taskCloseDataSchema = &wireSchema{kind: 'o', maxBytes: maxTaskCloseDataTicketWireBytes, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{"version": numberField, "purpose": stringField, "context": taskCloseContextSchema, "not_before": utcField, "not_after": utcField}), "signature": stringField,
}}

func validateTaskCloseDataClaims(c TaskCloseDataTicketClaims) error {
	if err := validateTaskCloseContext(c.Context); err != nil {
		return err
	}
	if c.Version != 1 || c.Purpose != "task_close_data" {
		return fmt.Errorf("invalid task close claims")
	}
	return validateTaskCloseWindow(c.NotBefore, c.NotAfter)
}
func matchTaskCloseIssuer(c TaskCloseDataTicketClaims, i CommandIssuerIdentity) error {
	if c.Context.IssuerCertificateID != i.certificateID || c.Context.IssuerCertificateDigest != i.digest || c.NotBefore.Before(i.notBefore) || c.NotAfter.After(i.notAfter) {
		return fmt.Errorf("task close issuer identity or interval mismatch")
	}
	var envelope commandIssuerEnvelope
	if err := decodeWire(i.wire, commandIssuerSchema, &envelope); err != nil {
		return err
	}
	cert := envelope.Claims
	if taskCloseBinding(c.Context) != (TrustBinding{Namespace: cert.Namespace, AuthorityID: cert.AuthorityID, Target: cert.Target, RestoreEpoch: cert.RestoreEpoch}) {
		return fmt.Errorf("task close issuer binding mismatch")
	}
	return nil
}

// SignTaskCloseDataTicket signs exactly supplied attribution with the verified
// issuer delegate. It neither derives metadata nor authorizes an effect.
func SignTaskCloseDataTicket(key ed25519.PrivateKey, issuer CommandIssuerIdentity, c TaskCloseDataTicketClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if err = validateTaskCloseDataClaims(c); err != nil {
		return nil, err
	}
	if err = validateExecStartIssuerIdentity(issuer); err != nil {
		return nil, err
	}
	if !bytes.Equal(private[ed25519.SeedSize:], issuer.publicKey) {
		return nil, fmt.Errorf("task close signer does not match issuer delegate")
	}
	if err = matchTaskCloseIssuer(c, issuer); err != nil {
		return nil, err
	}
	signed, err := signingBytes(taskCloseDataTicketDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(taskCloseDataEnvelope{c, ed25519.Sign(private, signed)}, maxTaskCloseDataTicketWireBytes)
}
func (v *ManagementVerifier) VerifyTaskCloseDataTicket(wire, issuerWire []byte, expected TaskCloseDataContext, now time.Time) (TaskCloseDataEvidence, error) {
	fail := func(err error) (TaskCloseDataEvidence, error) { return TaskCloseDataEvidence{}, err }
	if err := validateTaskCloseContext(expected); err != nil {
		return fail(err)
	}
	if v == nil || taskCloseBinding(expected) != v.binding {
		return fail(fmt.Errorf("task close expected binding mismatch"))
	}
	issuer, err := v.VerifyCommandIssuerCertificate(issuerWire, now)
	if err != nil {
		return fail(err)
	}
	var envelope taskCloseDataEnvelope
	if err = decodeWire(wire, taskCloseDataSchema, &envelope); err != nil {
		return fail(err)
	}
	c := envelope.Claims
	if err = validateTaskCloseDataClaims(c); err != nil {
		return fail(err)
	}
	if c.Context != expected {
		return fail(fmt.Errorf("task close context mismatch"))
	}
	if err = matchTaskCloseIssuer(c, issuer); err != nil {
		return fail(err)
	}
	signed, err := signingBytes(taskCloseDataTicketDomain, c)
	if err != nil {
		return fail(err)
	}
	if len(envelope.Signature) != ed25519.SignatureSize || !ed25519.Verify(issuer.publicKey, signed, envelope.Signature) {
		return fail(fmt.Errorf("invalid task close signature"))
	}
	if now.Add(-time.Second).Before(c.NotBefore) || !now.Add(time.Second).Before(c.NotAfter) {
		return fail(fmt.Errorf("task close outside authentication window"))
	}
	normalized, err := encodeWire(envelope, maxTaskCloseDataTicketWireBytes)
	if err != nil {
		return fail(err)
	}
	return TaskCloseDataEvidence{wire: normalized, digest: wireDigest(normalized), claims: c}, nil
}
