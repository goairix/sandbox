package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func nodeObserverCertificateFixture(t *testing.T) (taskCloseFixture, ed25519.PrivateKey, NodeObserverCertificateClaims, []byte) {
	f := taskCloseSetup(t)
	public, key := managementTestKey(t)
	i := nodeTerminationIntentFixture(t)
	b := f.claims.Binding
	c := NodeObserverCertificateClaims{Version: 1, CertificateID: "1a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", Role: "node_observer", Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, NodeUID: i.NodeUID, NodeBootID: i.NodeBootID, PublicKey: public, NotBefore: f.issuer.NotBefore(), NotAfter: f.issuer.NotAfter()}
	w, err := SignNodeObserverCertificate(f.root, c)
	require.NoError(t, err)
	return f, key, c, w
}

func TestNodeObserverProtocolCertificate(t *testing.T) {
	f, key, c, w := nodeObserverCertificateFixture(t)
	i, err := f.verifier.VerifyNodeObserverCertificate(w, c.NodeUID, c.NodeBootID, f.now)
	require.NoError(t, err)
	require.Equal(t, key.Public(), i.PublicKey())
	require.Equal(t, wireDigest(w), i.Digest())
	clear(i.Wire())
	require.Equal(t, w, i.Wire())
	for _, field := range []string{"role", "node_uid", "node_boot_id", "namespace"} {
		var v map[string]json.RawMessage
		var envelope map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(w, &envelope))
		require.NoError(t, json.Unmarshal(envelope["claims"], &v))
		v[field] = json.RawMessage(`"other"`)
		claims, err := json.Marshal(v)
		require.NoError(t, err)
		bad := taskCloseRaw(t, f.root, json.RawMessage(claims), nodeObserverCertificateDomain)
		_, err = f.verifier.VerifyNodeObserverCertificate(bad, c.NodeUID, c.NodeBootID, f.now)
		require.Error(t, err)
	}
	for _, bad := range [][]byte{append([]byte(" "), w...), bytes.Replace(w, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), bytes.Replace(w, []byte(`"node_uid":`), []byte(`"node_uid":null,"ignored":`), 1), taskCloseRaw(t, f.root, c, commandIssuerCertificateDomain)} {
		_, err := f.verifier.VerifyNodeObserverCertificate(bad, c.NodeUID, c.NodeBootID, f.now)
		require.Error(t, err)
	}
	c.Role = "runtime_receipt"
	_, err = SignNodeObserverCertificate(f.root, c)
	require.Error(t, err)
}

func TestNodeObserverProtocolTicket(t *testing.T) {
	f := taskCloseSetup(t)
	i := nodeTerminationIntentFixture(t)
	i.Current.IssuerCertificateID = f.issuer.CertificateID()
	i.Current.IssuerCertificateDigest = f.issuer.Digest()
	c := TaskRuntimeTerminationTicketClaims{Version: 1, Purpose: "task_runtime_terminate", Current: i.Current, Partition: i.Partition, IntentCreateRevision: 13, IntentModRevision: 13, IntentDigest: wireDigest([]byte("intent")), EnrollmentDigest: i.EnrollmentDigest, ObserverCertificateDigest: i.ObserverCertificateDigest, NodeUID: i.NodeUID, NodeBootID: i.NodeBootID, NotBefore: f.ticket.NotBefore, NotAfter: f.ticket.NotAfter}
	w, err := SignTaskRuntimeTerminationTicket(f.issuerKey, f.issuer, c)
	require.NoError(t, err)
	e, err := f.verifier.VerifyTaskRuntimeTerminationTicket(w, f.issuerWire, c, f.now)
	require.NoError(t, err)
	require.Equal(t, w, e.Wire())
	require.Equal(t, c, e.Claims())
	c.NodeBootID = "2a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
	_, err = f.verifier.VerifyTaskRuntimeTerminationTicket(w, f.issuerWire, c, f.now)
	require.Error(t, err)
	c.Purpose = "task_close_data"
	_, err = SignTaskRuntimeTerminationTicket(f.issuerKey, f.issuer, c)
	require.Error(t, err)
}
