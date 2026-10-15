package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"fmt"
	"time"
)

const nodeObserverCertificateDomain = "sandbox-node-observer-certificate:v1\x00"
const maxNodeObserverCertificateBytes = 2048

type NodeObserverCertificateClaims struct {
	Version       uint32    `json:"version"`
	RootKeyID     string    `json:"root_key_id"`
	CertificateID string    `json:"certificate_id"`
	Role          string    `json:"role"`
	Namespace     string    `json:"namespace"`
	AuthorityID   string    `json:"authority_id"`
	Target        string    `json:"target"`
	RestoreEpoch  string    `json:"restore_epoch"`
	NodeUID       string    `json:"node_uid"`
	NodeBootID    string    `json:"node_boot_id"`
	PublicKey     []byte    `json:"public_key"`
	NotBefore     time.Time `json:"not_before"`
	NotAfter      time.Time `json:"not_after"`
}

type NodeObserverIdentity struct {
	claims NodeObserverCertificateClaims
	wire   []byte
	digest string
}

func (i NodeObserverIdentity) Wire() []byte { return bytes.Clone(i.wire) }
func (i NodeObserverIdentity) PublicKey() ed25519.PublicKey {
	return bytes.Clone(i.claims.PublicKey)
}
func (i NodeObserverIdentity) Digest() string        { return i.digest }
func (i NodeObserverIdentity) NodeUID() string       { return i.claims.NodeUID }
func (i NodeObserverIdentity) NodeBootID() string    { return i.claims.NodeBootID }
func (i NodeObserverIdentity) CertificateID() string { return i.claims.CertificateID }
func (i NodeObserverIdentity) NotBefore() time.Time  { return i.claims.NotBefore }
func (i NodeObserverIdentity) NotAfter() time.Time   { return i.claims.NotAfter }

type nodeObserverCertificateEnvelope struct {
	Claims    NodeObserverCertificateClaims `json:"claims"`
	Signature []byte                        `json:"signature"`
}

var nodeObserverCertificateSchema = &wireSchema{kind: 'o', maxBytes: maxNodeObserverCertificateBytes, fields: map[string]*wireSchema{
	"claims": objectSchema(map[string]*wireSchema{
		"version": numberField, "root_key_id": stringField, "certificate_id": stringField, "role": stringField,
		"namespace": stringField, "authority_id": stringField, "target": stringField, "restore_epoch": stringField,
		"node_uid": stringField, "node_boot_id": stringField, "public_key": stringField, "not_before": utcField, "not_after": utcField,
	}), "signature": stringField,
}}

func validateNodeObserverCertificate(c NodeObserverCertificateClaims) error {
	if err := validateBinding(TrustBinding{c.Namespace, c.AuthorityID, c.Target, c.RestoreEpoch}); err != nil {
		return err
	}
	if c.Version != 1 || c.Role != "node_observer" || !validHash(c.RootKeyID) || !validUUID(c.CertificateID) || !validUUID(c.NodeUID) || !validUUID(c.NodeBootID) || len(c.PublicKey) != ed25519.PublicKeySize || !validUTC(c.NotBefore) || !validUTC(c.NotAfter) || !c.NotBefore.Before(c.NotAfter) {
		return fmt.Errorf("invalid node observer certificate")
	}
	return nil
}

func SignNodeObserverCertificate(key ed25519.PrivateKey, c NodeObserverCertificateClaims) ([]byte, error) {
	private, err := copyPrivateKey(key)
	if err != nil {
		return nil, err
	}
	defer clear(private)
	c.RootKeyID = wireDigest(private[ed25519.SeedSize:])
	c.PublicKey = bytes.Clone(c.PublicKey)
	if err := validateNodeObserverCertificate(c); err != nil {
		return nil, err
	}
	if bytes.Equal(c.PublicKey, private[ed25519.SeedSize:]) {
		return nil, fmt.Errorf("observer delegate must differ from root")
	}
	signed, err := signingBytes(nodeObserverCertificateDomain, c)
	if err != nil {
		return nil, err
	}
	return encodeWire(nodeObserverCertificateEnvelope{c, ed25519.Sign(private, signed)}, maxNodeObserverCertificateBytes)
}

func (v *ManagementVerifier) VerifyNodeObserverCertificate(wire []byte, nodeUID, nodeBootID string, now time.Time) (NodeObserverIdentity, error) {
	fail := func(err error) (NodeObserverIdentity, error) { return NodeObserverIdentity{}, err }
	var e nodeObserverCertificateEnvelope
	if err := decodeWire(wire, nodeObserverCertificateSchema, &e); err != nil {
		return fail(err)
	}
	c := e.Claims
	if err := validateNodeObserverCertificate(c); err != nil {
		return fail(err)
	}
	if c.NodeUID != nodeUID || c.NodeBootID != nodeBootID {
		return fail(fmt.Errorf("observer node incarnation mismatch"))
	}
	root, err := v.managementCertificateRoot(TrustBinding{c.Namespace, c.AuthorityID, c.Target, c.RestoreEpoch}, c.RootKeyID, c.PublicKey, e.Signature, c.NotBefore, c.NotAfter, now)
	if err != nil {
		return fail(err)
	}
	signed, err := signingBytes(nodeObserverCertificateDomain, c)
	if err != nil || !ed25519.Verify(root, signed, e.Signature) {
		return fail(fmt.Errorf("invalid observer certificate signature"))
	}
	canonical, err := encodeWire(e, maxNodeObserverCertificateBytes)
	if err != nil || !bytes.Equal(canonical, wire) {
		return fail(fmt.Errorf("noncanonical observer certificate"))
	}
	return NodeObserverIdentity{claims: c, wire: canonical, digest: wireDigest(canonical)}, nil
}
