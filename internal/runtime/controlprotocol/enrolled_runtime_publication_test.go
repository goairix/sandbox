package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnrolledPublicationProtocol(t *testing.T) {
	f := newActivationFixture(t)
	activationWire, err := SignTargetActivation(f.root, f.claims)
	require.NoError(t, err)
	a, err := f.verifier.VerifyTargetActivation(activationWire, f.runtimeWire, f.issuerWire, f.birth, f.now)
	require.NoError(t, err)
	_, observerKey := managementTestKey(t)
	b := a.Binding()
	oc := NodeObserverCertificateClaims{Version: 1, CertificateID: "11111111-1111-4111-8111-111111111111", Role: "node_observer", Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, NodeUID: "22222222-2222-4222-8222-222222222222", NodeBootID: "33333333-3333-4333-8333-333333333333", PublicKey: observerKey[32:], NotBefore: f.now.Add(-time.Minute), NotAfter: f.now.Add(time.Hour)}
	ow, err := SignNodeObserverCertificate(f.root, oc)
	require.NoError(t, err)
	oi, err := f.verifier.VerifyNodeObserverCertificate(ow, oc.NodeUID, oc.NodeBootID, f.now)
	require.NoError(t, err)
	intent := "44444444-4444-4444-8444-444444444444"
	i := a.Identity()
	rc := NodeEnrollmentReceiptClaims{Version: 1, Purpose: "runtime_node_enrolled", EnrollmentDigest: strings.Repeat("e", 64), ObserverCertificateDigest: oi.Digest(), NodeUID: oc.NodeUID, NodeBootID: oc.NodeBootID, ActivationDigest: a.Digest(), Runtime: i.Runtime, SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, CreationIntentID: intent, ObservedAt: f.now}
	r, err := SignNodeEnrollmentReceipt(observerKey, oi, rc)
	require.NoError(t, err)
	e := EnrolledRuntimeEvidence{Request: NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", Activation: activationWire, RuntimeCertificate: f.runtimeWire, IssuerCertificate: f.issuerWire}, ObserverCertificate: ow, Receipt: r.Wire()}
	c := RuntimeCertificateClaims{Version: 1, Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, IntentID: intent, SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, OperationID: "55555555-5555-4555-8555-555555555555", PayloadDigest: strings.Repeat("a", 64), Snapshot: SnapshotReference{Version: "v1", Digest: strings.Repeat("b", 64)}, ExpiresAt: f.now.Add(time.Hour), Runtime: i.Runtime, WorkspaceMode: "plain", PublicKey: f.runtimeKey[32:], NotBefore: f.now.Add(-time.Minute), NotAfter: f.now.Add(time.Hour)}
	cw, err := SignRuntimeCertificate(f.root, c)
	require.NoError(t, err)
	ce, err := f.verifier.VerifyEnrolledPublicationCertificate(cw, a, f.now)
	require.NoError(t, err)
	public := ce.PublicKey()
	public[0] ^= 1
	require.Equal(t, ed25519.PublicKey(f.runtimeKey[32:]), ce.PublicKey())
	claim := ClaimReference{ClaimID: "66666666-6666-4666-8666-666666666666", CreateRevision: 7, LeaseID: 8}
	ready := EnrolledReadyReceiptClaims{ReadyReceiptClaims: ReadyReceiptClaims{Version: 2, CertificateDigest: wireDigest(cw), Claim: claim, DataGateEpoch: a.DataGateEpoch(), GateState: "open", ObservedAt: f.now, ValidUntil: f.now.Add(5 * time.Second)}, EnrollmentDigest: rc.EnrollmentDigest, ObserverCertificateDigest: oi.Digest(), ActivationDigest: a.Digest()}
	proof, err := SignEnrolledReadyReceipt(f.runtimeKey, cw, ready)
	require.NoError(t, err)
	v, err := NewPublicationVerifier(b, []ed25519.PublicKey{ed25519.PublicKey(f.root[32:])})
	require.NoError(t, err)
	context := PublicationContext{Certificate: certificateContext(c), Runtime: i.Runtime, CertificateDigest: wireDigest(cw), Claim: claim, DataGateEpoch: a.DataGateEpoch()}
	got, err := v.VerifyEnrolledPublication(proof, context, e, f.birth.ContractDigest, f.now)
	require.NoError(t, err)
	require.Equal(t, ready, got.Ready())
	_, err = v.Verify(proof, context, f.now)
	require.Error(t, err)
	_, err = v.VerifyEnrolledPublicationHistorical(proof, context, e, f.birth.ContractDigest)
	require.NoError(t, err)
	for _, wire := range [][]byte{append([]byte(" "), proof...), bytes.Replace(proof, []byte(`"version":2`), []byte(`"version":2,"version":2`), 1), bytes.Replace(proof, []byte(`"enrollment_digest":"`+rc.EnrollmentDigest+`"`), []byte(`"enrollment_digest":null`), 1), bytes.Replace(proof, []byte(`"claims":{`), []byte(`"claims":{"unknown":1,`), 1), bytes.Repeat([]byte(" "), 8193)} {
		_, err := v.VerifyEnrolledPublication(wire, context, e, f.birth.ContractDigest, f.now)
		require.Error(t, err)
	}
	for _, mutate := range []func(*RuntimeCertificateClaims){func(c *RuntimeCertificateClaims) { c.PublicKey = observerKey[32:] }, func(c *RuntimeCertificateClaims) { c.Runtime.BootID = "other" }, func(c *RuntimeCertificateClaims) { c.WorkspaceMode = "fuse" }} {
		bad := c
		mutate(&bad)
		wire, err := SignRuntimeCertificate(f.root, bad)
		require.NoError(t, err)
		_, err = f.verifier.VerifyEnrolledPublicationCertificate(wire, a, f.now)
		require.Error(t, err)
	}
	badContext := context
	badContext.Claim.LeaseID++
	_, err = v.VerifyEnrolledPublication(proof, badContext, e, f.birth.ContractDigest, f.now)
	require.Error(t, err)
	_, err = v.VerifyEnrolledPublication(proof, context, e, strings.Repeat("d", 64), f.now)
	require.Error(t, err)
	badReceipt := rc
	badReceipt.CreationIntentID = "77777777-7777-4777-8777-777777777777"
	r, err = SignNodeEnrollmentReceipt(observerKey, oi, badReceipt)
	require.NoError(t, err)
	e.Receipt = r.Wire()
	_, err = v.VerifyEnrolledPublication(proof, context, e, f.birth.ContractDigest, f.now)
	require.Error(t, err)
}
