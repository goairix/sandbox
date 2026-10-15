package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"time"
)

const taskRuntimeTerminationTicketDomain = "sandbox-task-runtime-termination-ticket:v1\x00"

type TaskRuntimeTerminationTicketClaims struct {
	Version                   uint32               `json:"version"`
	Purpose                   string               `json:"purpose"`
	Current                   TaskCloseDataContext `json:"current"`
	Partition                 uint8                `json:"partition"`
	IntentCreateRevision      int64                `json:"intent_create_revision"`
	IntentModRevision         int64                `json:"intent_mod_revision"`
	IntentDigest              string               `json:"intent_digest"`
	EnrollmentDigest          string               `json:"enrollment_digest"`
	ObserverCertificateDigest string               `json:"observer_certificate_digest"`
	NodeUID                   string               `json:"node_uid"`
	NodeBootID                string               `json:"node_boot_id"`
	NotBefore                 time.Time            `json:"not_before"`
	NotAfter                  time.Time            `json:"not_after"`
}

type TaskRuntimeTerminationEvidence struct {
	claims TaskRuntimeTerminationTicketClaims
	wire   []byte
	digest string
}

func (e TaskRuntimeTerminationEvidence) Claims() TaskRuntimeTerminationTicketClaims { return e.claims }
func (e TaskRuntimeTerminationEvidence) Wire() []byte                               { return bytes.Clone(e.wire) }
func (e TaskRuntimeTerminationEvidence) Digest() string                             { return e.digest }

type taskRuntimeTerminationTicketEnvelope struct {
	Claims    TaskRuntimeTerminationTicketClaims `json:"claims"`
	Signature []byte                             `json:"signature"`
}

var taskRuntimeTerminationTicketSchema = &wireSchema{kind: 'o', maxBytes: 8192, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{
		"version": numberField, "purpose": stringField, "current": taskCloseContextSchema, "partition": numberField,
		"intent_create_revision": numberField, "intent_mod_revision": numberField, "intent_digest": stringField,
		"enrollment_digest": stringField, "observer_certificate_digest": stringField, "node_uid": stringField, "node_boot_id": stringField,
		"not_before": utcField, "not_after": utcField,
	}), "signature": stringField,
}}

func validateTaskRuntimeTerminationTicket(c TaskRuntimeTerminationTicketClaims) error {
	if err := validateTaskCloseContext(c.Current); err != nil {
		return err
	}
	hash, _ := hex.DecodeString(c.Current.WorkspaceHash)
	if c.Version != 1 || c.Purpose != "task_runtime_terminate" || c.IntentCreateRevision <= c.Current.ClaimCreateRevision || c.IntentModRevision != c.IntentCreateRevision || !validHash(c.IntentDigest) || !validHash(c.EnrollmentDigest) || !validHash(c.ObserverCertificateDigest) || !validUUID(c.NodeUID) || !validUUID(c.NodeBootID) || len(hash) != 32 || hash[0] != c.Partition {
		return fmt.Errorf("invalid runtime termination ticket")
	}
	return validateTaskCloseWindow(c.NotBefore, c.NotAfter)
}

func SignTaskRuntimeTerminationTicket(key ed25519.PrivateKey, issuer CommandIssuerIdentity, c TaskRuntimeTerminationTicketClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	defer clear(private)
	if err = validateTaskRuntimeTerminationTicket(c); err != nil {
		return nil, err
	}
	if err = validateExecStartIssuerIdentity(issuer); err != nil {
		return nil, err
	}
	if !bytes.Equal(private[ed25519.SeedSize:], issuer.publicKey) {
		return nil, fmt.Errorf("runtime termination signer mismatch")
	}
	if err = matchTaskCloseIssuer(TaskCloseDataTicketClaims{Context: c.Current, NotBefore: c.NotBefore, NotAfter: c.NotAfter}, issuer); err != nil {
		return nil, err
	}
	signed, err := signingBytes(taskRuntimeTerminationTicketDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(taskRuntimeTerminationTicketEnvelope{c, ed25519.Sign(private, signed)}, 8192)
}

func (v *ManagementVerifier) VerifyTaskRuntimeTerminationTicket(wire, issuerWire []byte, expected TaskRuntimeTerminationTicketClaims, now time.Time) (TaskRuntimeTerminationEvidence, error) {
	fail := func(err error) (TaskRuntimeTerminationEvidence, error) { return TaskRuntimeTerminationEvidence{}, err }
	if err := validateTaskRuntimeTerminationTicket(expected); err != nil {
		return fail(err)
	}
	if v == nil || v.binding != taskCloseBinding(expected.Current) {
		return fail(fmt.Errorf("runtime termination trust mismatch"))
	}
	issuer, err := v.VerifyCommandIssuerCertificate(issuerWire, now)
	if err != nil {
		return fail(err)
	}
	var e taskRuntimeTerminationTicketEnvelope
	if err := decodeWire(wire, taskRuntimeTerminationTicketSchema, &e); err != nil {
		return fail(err)
	}
	c := e.Claims
	if err := validateTaskRuntimeTerminationTicket(c); err != nil {
		return fail(err)
	}
	if c != expected {
		return fail(fmt.Errorf("runtime termination persisted intent mismatch"))
	}
	if err := matchTaskCloseIssuer(TaskCloseDataTicketClaims{Context: c.Current, NotBefore: c.NotBefore, NotAfter: c.NotAfter}, issuer); err != nil {
		return fail(err)
	}
	signed, err := signingBytes(taskRuntimeTerminationTicketDomain, c)
	if err != nil || len(e.Signature) != ed25519.SignatureSize || !ed25519.Verify(issuer.publicKey, signed, e.Signature) {
		return fail(fmt.Errorf("invalid runtime termination ticket signature"))
	}
	if !validUTC(now) || now.Add(-time.Second).Before(c.NotBefore) || !now.Add(time.Second).Before(c.NotAfter) {
		return fail(fmt.Errorf("runtime termination ticket outside authentication window"))
	}
	canonical, err := encodeWire(e, 8192)
	if err != nil || !bytes.Equal(canonical, wire) {
		return fail(fmt.Errorf("noncanonical runtime termination ticket"))
	}
	return TaskRuntimeTerminationEvidence{c, canonical, wireDigest(canonical)}, nil
}
