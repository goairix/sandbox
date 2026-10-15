package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"time"
)

const NodeEnrollmentFrameBytes = 16384
const nodeEnrollmentChallengeDomain = "runtime_node_enrollment_challenge:v1\x00"

// NodeEnrollmentRequest has no PID, namespace or cgroup input. IssuerCertificate
// allows the unchanged activation verifier to check both certified delegates.
type NodeEnrollmentRequest struct {
	Version            uint32 `json:"version"`
	Purpose            string `json:"purpose"`
	RuntimeCertificate []byte `json:"runtime_certificate"`
	Activation         []byte `json:"activation"`
	IssuerCertificate  []byte `json:"issuer_certificate"`
}

type NodeEnrollmentChallenge struct {
	Version             uint32           `json:"version"`
	Purpose             string           `json:"purpose"`
	ObserverCertificate []byte           `json:"observer_certificate"`
	Nonce               string           `json:"nonce"`
	RequestDigest       string           `json:"request_digest"`
	Runtime             RuntimeReference `json:"runtime"`
	NodeUID             string           `json:"node_uid"`
	NodeBootID          string           `json:"node_boot_id"`
}

type NodeEnrollmentResponse struct {
	Version   uint32 `json:"version"`
	Purpose   string `json:"purpose"`
	Signature []byte `json:"signature"`
}

var nodeEnrollmentRequestSchema = &wireSchema{kind: 'o', maxBytes: NodeEnrollmentFrameBytes, fields: map[string]*wireSchema{
	"version": numberField, "purpose": stringField, "runtime_certificate": stringField, "activation": stringField, "issuer_certificate": stringField,
}}
var nodeEnrollmentChallengeSchema = &wireSchema{kind: 'o', maxBytes: NodeEnrollmentFrameBytes, fields: map[string]*wireSchema{
	"version": numberField, "purpose": stringField, "observer_certificate": stringField, "nonce": stringField,
	"request_digest": stringField, "runtime": runtimeSchema, "node_uid": stringField, "node_boot_id": stringField,
}}
var nodeEnrollmentResponseSchema = &wireSchema{kind: 'o', maxBytes: NodeEnrollmentFrameBytes, fields: map[string]*wireSchema{
	"version": numberField, "purpose": stringField, "signature": stringField,
}}

func EncodeNodeEnrollmentRequest(r NodeEnrollmentRequest) ([]byte, error) {
	if r.Version != 1 || r.Purpose != "runtime_node_enrollment" || len(r.RuntimeCertificate) == 0 || len(r.RuntimeCertificate) > 4096 || len(r.Activation) == 0 || len(r.Activation) > 16384 || len(r.IssuerCertificate) == 0 || len(r.IssuerCertificate) > 4096 {
		return nil, fmt.Errorf("invalid node enrollment request")
	}
	return encodeWire(r, NodeEnrollmentFrameBytes)
}

func DecodeNodeEnrollmentRequest(wire []byte) (NodeEnrollmentRequest, error) {
	var r NodeEnrollmentRequest
	if err := decodeWire(wire, nodeEnrollmentRequestSchema, &r); err != nil {
		return r, err
	}
	canonical, err := EncodeNodeEnrollmentRequest(r)
	if err != nil || !bytes.Equal(canonical, wire) {
		return NodeEnrollmentRequest{}, fmt.Errorf("noncanonical node enrollment request")
	}
	return r, nil
}

// VerifyNodeEnrollmentRequest first authenticates the exact root activation
// bytes. Only then may its certified birth fields be used to call the existing
// activation verifier. Kernel peer and approved Pod profile checks belong to the
// observer; this method grants no enrollment or process authority.
func (v *ManagementVerifier) VerifyNodeEnrollmentRequest(r NodeEnrollmentRequest, now time.Time) (TargetActivationEvidence, error) {
	fail := func(err error) (TargetActivationEvidence, error) { return TargetActivationEvidence{}, err }
	if _, err := EncodeNodeEnrollmentRequest(r); err != nil {
		return fail(err)
	}
	var e targetActivationEnvelope
	if err := decodeWire(r.Activation, targetActivationSchema, &e); err != nil {
		return fail(err)
	}
	c := e.Claims.claims()
	if err := validateTargetActivation(c); err != nil {
		return fail(err)
	}
	if v == nil || c.Binding != v.binding {
		return fail(fmt.Errorf("node enrollment activation trust mismatch"))
	}
	root := v.roots[c.RootKeyID]
	signed, err := signingBytes(targetActivationDomain, activationClaimsWire(c))
	if err != nil || len(root) != ed25519.PublicKeySize || len(e.Signature) != ed25519.SignatureSize || !ed25519.Verify(root, signed, e.Signature) {
		return fail(fmt.Errorf("node enrollment activation signature invalid"))
	}
	canonical, err := encodeWire(e, maxTargetActivationWireBytes)
	if err != nil || !bytes.Equal(canonical, r.Activation) {
		return fail(fmt.Errorf("node enrollment activation noncanonical"))
	}
	runtime, err := v.VerifyRuntimeIdentityCertificate(r.RuntimeCertificate, c.Identity, now)
	if err != nil {
		return fail(err)
	}
	birth := BirthContext{BootID: c.Identity.Runtime.BootID, RuntimePublicKey: runtime.PublicKey(), UID: c.UID, GID: c.GID, NetworkAllowed: c.NetworkAllowed, ContractDigest: c.ContractDigest}
	return v.VerifyTargetActivation(r.Activation, r.RuntimeCertificate, r.IssuerCertificate, birth, now)
}

func EncodeNodeEnrollmentChallenge(c NodeEnrollmentChallenge) ([]byte, error) {
	nonce, err := hex.DecodeString(c.Nonce)
	if c.Version != 1 || c.Purpose != "runtime_node_enrollment_challenge" || len(c.ObserverCertificate) == 0 || len(c.ObserverCertificate) > maxNodeObserverCertificateBytes || err != nil || len(nonce) != 32 || hex.EncodeToString(nonce) != c.Nonce || !validHash(c.RequestDigest) || !validRuntime(c.Runtime) || !validUUID(c.NodeUID) || !validUUID(c.NodeBootID) {
		return nil, fmt.Errorf("invalid node enrollment challenge")
	}
	return encodeWire(c, NodeEnrollmentFrameBytes)
}

func DecodeNodeEnrollmentChallenge(wire []byte) (NodeEnrollmentChallenge, error) {
	var c NodeEnrollmentChallenge
	if err := decodeWire(wire, nodeEnrollmentChallengeSchema, &c); err != nil {
		return c, err
	}
	canonical, err := EncodeNodeEnrollmentChallenge(c)
	if err != nil || !bytes.Equal(canonical, wire) {
		return NodeEnrollmentChallenge{}, fmt.Errorf("noncanonical node enrollment challenge")
	}
	return c, nil
}

// NodeEnrollmentChallengeSigningBytes exposes only this fixed domain/DTO for
// the Supervisor's private fixed-purpose signer (and its in-package fixture).
func NodeEnrollmentChallengeSigningBytes(c NodeEnrollmentChallenge) ([]byte, error) {
	w, err := EncodeNodeEnrollmentChallenge(c)
	if err != nil {
		return nil, err
	}
	return append([]byte(nodeEnrollmentChallengeDomain), w...), nil
}

func EncodeNodeEnrollmentResponse(r NodeEnrollmentResponse) ([]byte, error) {
	if r.Version != 1 || r.Purpose != "runtime_node_enrollment_response" || len(r.Signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("invalid node enrollment response")
	}
	return encodeWire(r, NodeEnrollmentFrameBytes)
}

func DecodeNodeEnrollmentResponse(wire []byte) (NodeEnrollmentResponse, error) {
	var r NodeEnrollmentResponse
	if err := decodeWire(wire, nodeEnrollmentResponseSchema, &r); err != nil {
		return r, err
	}
	canonical, err := EncodeNodeEnrollmentResponse(r)
	if err != nil || !bytes.Equal(canonical, wire) {
		return NodeEnrollmentResponse{}, fmt.Errorf("noncanonical node enrollment response")
	}
	return r, nil
}

func VerifyNodeEnrollmentResponse(c NodeEnrollmentChallenge, r NodeEnrollmentResponse, runtime RuntimeReceiptIdentity) error {
	if _, err := EncodeNodeEnrollmentResponse(r); err != nil {
		return err
	}
	if runtime.Context().Runtime != c.Runtime {
		return fmt.Errorf("node enrollment runtime mismatch")
	}
	signed, err := NodeEnrollmentChallengeSigningBytes(c)
	if err != nil || !ed25519.Verify(runtime.PublicKey(), signed, r.Signature) {
		return fmt.Errorf("invalid node enrollment runtime challenge signature")
	}
	return nil
}
