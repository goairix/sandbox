package etcd

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func coldMaterialFixture(t *testing.T) (*Backend, ColdBootstrapMaterial, RuntimeDispatchRecord, string, time.Time) {
	t.Helper()
	n, err := NewNamespace("/cold-material", "authority", "cell")
	require.NoError(t, err)
	now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	nb, na := now.Add(-time.Minute), now.Add(10*time.Minute)
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	runtimeKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32)).Public().(ed25519.PublicKey)
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32)).Public().(ed25519.PublicKey)
	binding := p.TrustBinding{Namespace: n.Root(), AuthorityID: "runtime-authority", Target: "kubernetes", RestoreEpoch: "restore"}
	b := &Backend{namespace: n, restoreEpoch: binding.RestoreEpoch, publicationAuthorityID: binding.AuthorityID, enrolledPublication: &EnrolledPublicationOptions{ContractDigest: strings.Repeat("b", 64)}}
	b.enrolledVerifier, err = p.NewManagementVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	b.publicationVerifier, err = p.NewPublicationVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	d := RuntimeDispatchRecord{Kind: RuntimeDispatchCreate, IntentID: uuid.NewString(), SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), RestoreEpoch: binding.RestoreEpoch, Generation: 2, DataGateEpoch: 1, OperationID: uuid.NewString(), Target: binding.Target, PayloadDigest: strings.Repeat("c", 64), Snapshot: SnapshotReference{Version: "snapshot", Digest: strings.Repeat("d", 64)}, ExpiresAt: na}
	runtime := p.RuntimeReference{ID: "sandbox-pod", UID: uuid.NewString(), BootID: uuid.NewString()}
	identity := p.RuntimeIdentityContext{SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, Generation: d.Generation, Runtime: runtime}
	m := ColdBootstrapMaterial{Birth: ct.Birth{BootID: runtime.BootID, RuntimePublicKey: runtimeKey, UID: 1000, GID: 1000, ContractDigest: b.enrolledPublication.ContractDigest}}
	m.RuntimeCertificate, err = p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: runtimeKey, NotBefore: nb, NotAfter: na, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, Generation: d.Generation, Runtime: runtime})
	require.NoError(t, err)
	m.IssuerCertificate, err = p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey, NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	m.PublicationCertificate, err = p.SignRuntimeCertificate(root, p.RuntimeCertificateClaims{Version: 1, Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, Generation: d.Generation, OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, Snapshot: p.SnapshotReference(d.Snapshot), ExpiresAt: na, Runtime: runtime, WorkspaceMode: "plain", PublicKey: runtimeKey, NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	m.Activation, err = p.SignTargetActivation(root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: identity, DataGateEpoch: d.DataGateEpoch, UID: m.Birth.UID, GID: m.Birth.GID, ContractDigest: m.Birth.ContractDigest, RuntimeCertificateDigest: workspaceSyncIntentDigest(string(m.RuntimeCertificate)), IssuerCertificateDigest: workspaceSyncIntentDigest(string(m.IssuerCertificate)), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	return b, m, d, runtime.UID, now
}

func TestColdBootstrapMaterialAuthentication(t *testing.T) {
	b, m, d, uid, now := coldMaterialFixture(t)
	require.NoError(t, b.validateColdBootstrapMaterial(m, d, uid, now), "independently root-signed material must match the original observed UID and dispatch")
	for _, change := range []func(*ColdBootstrapMaterial, *RuntimeDispatchRecord, *string){
		func(m *ColdBootstrapMaterial, _ *RuntimeDispatchRecord, _ *string) { m.Birth.BootID = uuid.NewString() },
		func(m *ColdBootstrapMaterial, _ *RuntimeDispatchRecord, _ *string) {
			m.Birth.RuntimePublicKey = bytes.Repeat([]byte{9}, 32)
		},
		func(m *ColdBootstrapMaterial, _ *RuntimeDispatchRecord, _ *string) { m.Birth.NetworkAllowed = true },
		func(m *ColdBootstrapMaterial, _ *RuntimeDispatchRecord, _ *string) { m.Activation = []byte(`{}`) },
		func(_ *ColdBootstrapMaterial, d *RuntimeDispatchRecord, _ *string) { d.OperationID = uuid.NewString() },
		func(_ *ColdBootstrapMaterial, d *RuntimeDispatchRecord, _ *string) { d.DataGateEpoch++ },
		func(_ *ColdBootstrapMaterial, d *RuntimeDispatchRecord, _ *string) { d.Generation++ },
		func(_ *ColdBootstrapMaterial, _ *RuntimeDispatchRecord, uid *string) { *uid = uuid.NewString() },
	} {
		other, dispatch, originalUID := m, d, uid
		change(&other, &dispatch, &originalUID)
		require.Error(t, b.validateColdBootstrapMaterial(other, dispatch, originalUID, now))
	}
	require.Error(t, b.validateColdBootstrapMaterial(m, d, uid, now.Add(time.Hour)), "current recovery must not accept expired activation")
	var frame bytes.Buffer
	require.NoError(t, ct.WriteBootstrap(&frame, m.BootstrapRequest()))
	require.LessOrEqual(t, frame.Len(), ct.MaxBootstrap)
	request := m.BootstrapRequest()
	request.Activation[0] ^= 1
	require.NotEqual(t, request.Activation[0], m.Activation[0], "outbound request owns public bytes")
}

func TestColdBootstrapMaterialFrameBounds(t *testing.T) {
	_, m, _, _, _ := coldMaterialFixture(t)
	// A bounded canonical record can still exceed the actual base64 frame.
	// These strings exercise only input rejection; authentication remains the
	// independently signed fixture above.
	wire, err := json.Marshal(strings.Repeat("x", 4300))
	require.NoError(t, err)
	m.Activation, m.RuntimeCertificate, m.IssuerCertificate = wire, wire, wire
	_, err = coldEncode(m)
	require.NoError(t, err, "the persisted representation alone fits")
	var frame bytes.Buffer
	require.Error(t, ct.WriteBootstrap(&frame, m.BootstrapRequest()))
	require.ErrorIs(t, coldMaterialBounds(m), ErrInvalidRecord)
	require.Zero(t, frame.Len(), "oversized frame must fail before writing")
}
