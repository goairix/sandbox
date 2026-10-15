package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"time"
)

const nodeEnrollmentReceiptDomain = "sandbox-node-enrollment-receipt:v1\x00"
const nodeTerminationProofDomain = "sandbox-node-termination-proof:v1\x00"

type NodeEnrollmentReceiptClaims struct {
	Version                   uint32           `json:"version"`
	Purpose                   string           `json:"purpose"`
	EnrollmentDigest          string           `json:"enrollment_digest"`
	ObserverCertificateDigest string           `json:"observer_certificate_digest"`
	NodeUID                   string           `json:"node_uid"`
	NodeBootID                string           `json:"node_boot_id"`
	ActivationDigest          string           `json:"activation_digest"`
	Runtime                   RuntimeReference `json:"runtime"`
	SandboxID                 string           `json:"sandbox_id"`
	WorkspaceHash             string           `json:"workspace_hash"`
	Generation                int64            `json:"generation"`
	CreationIntentID          string           `json:"creation_intent_id"`
	ObservedAt                time.Time        `json:"observed_at"`
}

type NodeEnrollmentReceipt struct {
	Claims    NodeEnrollmentReceiptClaims `json:"claims"`
	Signature []byte                      `json:"signature"`
}

func (r NodeEnrollmentReceipt) Wire() []byte {
	w, _ := encodeWire(r, 4096)
	return w
}

func DecodeNodeEnrollmentReceipt(wire []byte) (NodeEnrollmentReceipt, error) {
	var r NodeEnrollmentReceipt
	if err := decodeWire(wire, nodeEnrollmentReceiptSchema, &r); err != nil {
		return r, err
	}
	if err := validateEnrollmentReceipt(r.Claims); err != nil || len(r.Signature) != ed25519.SignatureSize || !bytes.Equal(r.Wire(), wire) {
		return NodeEnrollmentReceipt{}, fmt.Errorf("invalid canonical node enrollment receipt")
	}
	return r, nil
}

var nodeEnrollmentReceiptSchema = &wireSchema{kind: 'o', maxBytes: 4096, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{"version": numberField, "purpose": stringField, "enrollment_digest": stringField, "observer_certificate_digest": stringField, "node_uid": stringField, "node_boot_id": stringField, "activation_digest": stringField, "runtime": runtimeSchema, "sandbox_id": stringField, "workspace_hash": stringField, "generation": numberField, "creation_intent_id": stringField, "observed_at": utcField}), "signature": stringField,
}}

func validateEnrollmentReceipt(c NodeEnrollmentReceiptClaims) error {
	if c.Version != 1 || c.Purpose != "runtime_node_enrolled" || !validHash(c.EnrollmentDigest) || !validHash(c.ObserverCertificateDigest) || !validHash(c.ActivationDigest) || !validUUID(c.NodeUID) || !validUUID(c.NodeBootID) || !validRuntime(c.Runtime) || !validID(c.SandboxID) || !validHash(c.WorkspaceHash) || c.Generation <= 0 || !validUUID(c.CreationIntentID) || !validUTC(c.ObservedAt) {
		return fmt.Errorf("invalid node enrollment receipt")
	}
	return nil
}

func observerSigner(key ed25519.PrivateKey, identity NodeObserverIdentity, certificateDigest, nodeUID, bootID string, at time.Time) (ed25519.PrivateKey, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	if len(identity.wire) == 0 || identity.digest != wireDigest(identity.wire) || identity.digest != certificateDigest || identity.NodeUID() != nodeUID || identity.NodeBootID() != bootID || !bytes.Equal(identity.PublicKey(), private[ed25519.SeedSize:]) || !validUTC(at) || at.Add(-time.Second).Before(identity.NotBefore()) || !at.Add(time.Second).Before(identity.NotAfter()) {
		clear(private)
		return nil, fmt.Errorf("observer signer identity/interval mismatch")
	}
	return private, nil
}

func SignNodeEnrollmentReceipt(key ed25519.PrivateKey, identity NodeObserverIdentity, c NodeEnrollmentReceiptClaims) (NodeEnrollmentReceipt, error) {
	if err := validateEnrollmentReceipt(c); err != nil {
		return NodeEnrollmentReceipt{}, err
	}
	private, err := observerSigner(key, identity, c.ObserverCertificateDigest, c.NodeUID, c.NodeBootID, c.ObservedAt)
	if err != nil {
		return NodeEnrollmentReceipt{}, err
	}
	defer clear(private)
	signed, err := signingBytes(nodeEnrollmentReceiptDomain, c)
	if err != nil {
		return NodeEnrollmentReceipt{}, err
	}
	r := NodeEnrollmentReceipt{c, ed25519.Sign(private, signed)}
	if len(r.Wire()) == 0 {
		return NodeEnrollmentReceipt{}, fmt.Errorf("node enrollment receipt exceeds bound")
	}
	return r, nil
}

func (v *ManagementVerifier) VerifyNodeEnrollmentReceipt(wire, certificate []byte, expected NodeEnrollmentReceiptClaims, now time.Time) (NodeEnrollmentReceipt, error) {
	var r NodeEnrollmentReceipt
	if err := decodeWire(wire, nodeEnrollmentReceiptSchema, &r); err != nil {
		return r, err
	}
	if err := validateEnrollmentReceipt(r.Claims); err != nil {
		return NodeEnrollmentReceipt{}, err
	}
	if r.Claims != expected || !bytes.Equal(r.Wire(), wire) {
		return NodeEnrollmentReceipt{}, fmt.Errorf("node enrollment receipt context/canonical mismatch")
	}
	observer, err := v.VerifyNodeObserverCertificate(certificate, expected.NodeUID, expected.NodeBootID, now)
	if err != nil || observer.Digest() != expected.ObserverCertificateDigest {
		return NodeEnrollmentReceipt{}, fmt.Errorf("node enrollment observer certificate mismatch")
	}
	signed, err := signingBytes(nodeEnrollmentReceiptDomain, r.Claims)
	if err != nil || len(r.Signature) != ed25519.SignatureSize || !ed25519.Verify(observer.PublicKey(), signed, r.Signature) || r.Claims.ObservedAt.Before(observer.NotBefore()) || !r.Claims.ObservedAt.Before(observer.NotAfter()) {
		return NodeEnrollmentReceipt{}, fmt.Errorf("node enrollment receipt signature invalid")
	}
	return r, nil
}

type NodeTerminationQuery struct {
	Version              uint32 `json:"version"`
	Purpose              string `json:"purpose"`
	Partition            uint8  `json:"partition"`
	TaskID               string `json:"task_id"`
	CommandID            string `json:"command_id"`
	IntentCreateRevision int64  `json:"intent_create_revision"`
	IntentDigest         string `json:"intent_digest"`
	EnrollmentDigest     string `json:"enrollment_digest"`
}

func EncodeNodeTerminationQuery(q NodeTerminationQuery) ([]byte, error) {
	if q.Version != 1 || q.Purpose != "task_runtime_terminate_query" || !validUUID(q.TaskID) || !validUUID(q.CommandID) || q.IntentCreateRevision <= 0 || !validHash(q.IntentDigest) || !validHash(q.EnrollmentDigest) {
		return nil, fmt.Errorf("invalid node termination query")
	}
	return encodeWire(q, 2048)
}

func DecodeNodeTerminationQuery(wire []byte) (NodeTerminationQuery, error) {
	var q NodeTerminationQuery
	if len(wire) == 0 || len(wire) > 2048 || json.Unmarshal(wire, &q) != nil {
		return q, fmt.Errorf("invalid node termination query")
	}
	canonical, err := EncodeNodeTerminationQuery(q)
	if err != nil || !bytes.Equal(canonical, wire) {
		return NodeTerminationQuery{}, fmt.Errorf("noncanonical node termination query")
	}
	return q, nil
}

type NodeTerminationProofClaims struct {
	Version                   uint32               `json:"version"`
	Purpose                   string               `json:"purpose"`
	Current                   TaskCloseDataContext `json:"current"`
	Partition                 uint8                `json:"partition"`
	IntentCreateRevision      int64                `json:"intent_create_revision"`
	IntentDigest              string               `json:"intent_digest"`
	EnrollmentDigest          string               `json:"enrollment_digest"`
	ObserverCertificateDigest string               `json:"observer_certificate_digest"`
	TicketDigest              string               `json:"ticket_digest"`
	NodeUID                   string               `json:"node_uid"`
	NodeBootID                string               `json:"node_boot_id"`
	PodUID                    string               `json:"pod_uid"`
	PodResourceVersion        string               `json:"pod_resource_version"`
	ProviderDeletingAt        time.Time            `json:"provider_deleting_at"`
	PID1Dead                  bool                 `json:"pid1_dead"`
	CgroupEmpty               bool                 `json:"cgroup_empty"`
	ObservedAt                time.Time            `json:"observed_at"`
}

type nodeTerminationProofEnvelope struct {
	Claims    NodeTerminationProofClaims `json:"claims"`
	Signature []byte                     `json:"signature"`
}

func validateTerminationProof(c NodeTerminationProofClaims) error {
	if err := validateTaskCloseContext(c.Current); err != nil {
		return err
	}
	if _, err := EncodeTaskRuntimeTerminationClaim(c.Current, c.Partition); err != nil || c.IntentCreateRevision <= c.Current.ClaimCreateRevision {
		return fmt.Errorf("physical proof original intent/partition mismatch")
	}
	q := NodeTerminationQuery{1, "task_runtime_terminate_query", c.Partition, c.Current.TaskID, c.Current.CommandID, c.IntentCreateRevision, c.IntentDigest, c.EnrollmentDigest}
	if _, err := EncodeNodeTerminationQuery(q); err != nil {
		return err
	}
	if c.Version != 1 || c.Purpose != "task_runtime_terminated" || !validHash(c.ObserverCertificateDigest) || !validHash(c.TicketDigest) || !validUUID(c.NodeUID) || !validUUID(c.NodeBootID) || c.PodUID != c.Current.Runtime.UID || !validOpaque(c.PodResourceVersion) || !c.PID1Dead || !c.CgroupEmpty || !validUTC(c.ProviderDeletingAt) || !validUTC(c.ObservedAt) || c.ObservedAt.Before(c.ProviderDeletingAt) {
		return fmt.Errorf("invalid physical node termination proof")
	}
	return nil
}

func SignNodeTerminationProof(key ed25519.PrivateKey, identity NodeObserverIdentity, c NodeTerminationProofClaims) ([]byte, error) {
	if err := validateTerminationProof(c); err != nil {
		return nil, err
	}
	private, err := observerSigner(key, identity, c.ObserverCertificateDigest, c.NodeUID, c.NodeBootID, c.ObservedAt)
	if err != nil {
		return nil, err
	}
	defer clear(private)
	signed, err := signingBytes(nodeTerminationProofDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(nodeTerminationProofEnvelope{c, ed25519.Sign(private, signed)}, 4096)
}

func (v *ManagementVerifier) VerifyNodeTerminationProof(wire, certificate []byte, query NodeTerminationQuery) (NodeTerminationProofClaims, error) {
	fail := func(err error) (NodeTerminationProofClaims, error) { return NodeTerminationProofClaims{}, err }
	if len(wire) == 0 || len(wire) > 4096 {
		return fail(fmt.Errorf("node termination proof exceeds bound"))
	}
	var e nodeTerminationProofEnvelope
	if err := json.Unmarshal(wire, &e); err != nil {
		return fail(err)
	}
	c := e.Claims
	if err := validateTerminationProof(c); err != nil {
		return fail(err)
	}
	canonical, err := encodeWire(e, 4096)
	if err != nil || !bytes.Equal(canonical, wire) || (NodeTerminationQuery{1, "task_runtime_terminate_query", c.Partition, c.Current.TaskID, c.Current.CommandID, c.IntentCreateRevision, c.IntentDigest, c.EnrollmentDigest}) != query {
		return fail(fmt.Errorf("node termination proof lookup/canonical mismatch"))
	}
	observer, err := v.VerifyNodeObserverCertificate(certificate, c.NodeUID, c.NodeBootID, c.ObservedAt)
	if err != nil || observer.Digest() != c.ObserverCertificateDigest {
		return fail(fmt.Errorf("node termination proof observer mismatch"))
	}
	signed, err := signingBytes(nodeTerminationProofDomain, c)
	if err != nil || len(e.Signature) != ed25519.SignatureSize || !ed25519.Verify(observer.PublicKey(), signed, e.Signature) {
		return fail(fmt.Errorf("node termination physical proof signature invalid"))
	}
	return c, nil
}

type NodeTerminationResult struct {
	Version             uint32 `json:"version"`
	Purpose             string `json:"purpose"`
	State               string `json:"state"`
	Proof               []byte `json:"proof,omitempty"`
	ObserverCertificate []byte `json:"observer_certificate,omitempty"`
}

func EncodeNodeTerminationResult(r NodeTerminationResult) ([]byte, error) {
	if r.Version != 1 || r.Purpose != "task_runtime_termination_result" || (r.State != "absent" && r.State != "unknown" && r.State != "terminal") || (r.State == "terminal") != (len(r.Proof) > 0) || (r.State == "terminal") != (len(r.ObserverCertificate) > 0) || len(r.Proof) > 4096 || len(r.ObserverCertificate) > maxNodeObserverCertificateBytes {
		return nil, fmt.Errorf("invalid node termination result")
	}
	return encodeWire(r, NodeEnrollmentFrameBytes)
}

func DecodeNodeTerminationResult(wire []byte) (NodeTerminationResult, error) {
	var r NodeTerminationResult
	if len(wire) == 0 || len(wire) > NodeEnrollmentFrameBytes || json.Unmarshal(wire, &r) != nil {
		return r, fmt.Errorf("invalid node termination result frame")
	}
	canonical, err := EncodeNodeTerminationResult(r)
	if err != nil || !bytes.Equal(canonical, wire) {
		return NodeTerminationResult{}, fmt.Errorf("noncanonical node termination result")
	}
	return r, nil
}
