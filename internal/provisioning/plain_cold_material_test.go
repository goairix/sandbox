package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// plainColdMaterialFixture reuses the business request and trusted UTC clock
// fixtures without etcd or a runtime. Only material signs runtime certificates;
// the independently pinned root, issuer and runtime keys remain distinct.
func plainColdMaterialFixture(t *testing.T, issuerLifetime time.Duration) (*PlainColdService, ct.Birth, state.RuntimeDispatchRecord, time.Time) {
	t.Helper()
	s, workspace, request := coldBusinessFixture(t)
	now := time.Date(2026, time.October, 17, 12, 0, 0, 0, time.UTC)
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	issuer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	runtimeKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, ed25519.SeedSize))
	clockKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{4}, ed25519.SeedSize))
	config := launcherconfig.Config{
		Version: 2,
		Binding: launcherconfig.Binding{
			Namespace: "/codex-test/material/cell/", AuthorityID: "runtime",
			Target: "kubernetes", RestoreEpoch: "restore",
		},
		Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)},
		UID:   1000, GID: 1000, ContractDigest: strings.Repeat("b", 64),
		ClockSocket: ct.ClockSocket, ClockPublicKey: clockKey.Public().(ed25519.PublicKey),
		ClockAudience: "launcher", MaxActive: 1,
		Enrollment: launcherconfig.Enrollment{NodeUID: uuid.NewString(), NodeBootID: uuid.NewString()},
	}
	wire, err := json.Marshal(config)
	require.NoError(t, err)
	s.config, err = launcherconfig.Decode(wire)
	require.NoError(t, err)
	s.root, s.issuer = root, issuer
	s.clock = &cleanupProducerClock{now: now}
	s.verifier, err = p.NewManagementVerifier(s.config.TrustBinding(), s.config.Roots)
	require.NoError(t, err)
	s.issuerWire, err = p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{
		Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer",
		Namespace: config.Binding.Namespace, AuthorityID: config.Binding.AuthorityID,
		Target: config.Binding.Target, RestoreEpoch: config.Binding.RestoreEpoch,
		PublicKey: issuer.Public().(ed25519.PublicKey),
		NotBefore: now.Add(-10 * time.Second), NotAfter: now.Add(issuerLifetime),
	})
	require.NoError(t, err)
	birth := ct.Birth{
		BootID: uuid.NewString(), RuntimePublicKey: runtimeKey.Public().(ed25519.PublicKey),
		UID: config.UID, GID: config.GID, ContractDigest: config.ContractDigest,
	}
	dispatch := state.RuntimeDispatchRecord{
		Version: 1, IntentID: request.IntentID, SandboxID: request.SandboxID,
		WorkspaceHash: workspace.Hash(), RequestHash: strings.Repeat("c", 64),
		ConfigurationDigest: s.profile, RestoreEpoch: config.Binding.RestoreEpoch,
		Generation: 1, DataGateEpoch: 1,
		Snapshot:  state.SnapshotReference{Version: request.SnapshotVersion, Digest: strings.Repeat("d", 64)},
		ExpiresAt: now.Add(request.TTL), OperationID: uuid.NewString(),
		Kind: state.RuntimeDispatchCreate, Target: config.Binding.Target, PayloadDigest: strings.Repeat("e", 64),
		Claim: state.DispatchClaimReference{ClaimID: uuid.NewString(), WorkerID: "worker", CreateRevision: 1, LeaseID: 1},
		Attempt: state.StageAttemptLocator{
			Namespace: config.Binding.Namespace, Partition: workspace.Partition(), RequestID: request.RequestID,
			StageID: "runtime_dispatch", AttemptID: uuid.NewString(), RestoreEpoch: config.Binding.RestoreEpoch,
		},
	}
	require.NoError(t, dispatch.Validate())
	return s, birth, dispatch, now
}

// verifyPlainColdBusinessMaterial authenticates both signed business delegates
// before inspecting their windows. Expired publication and activation must stay
// rejected even when the receipt identity remains useful for cleanup.
func verifyPlainColdBusinessMaterial(t *testing.T, s *PlainColdService, m state.ColdBootstrapMaterial, birth ct.Birth, d state.RuntimeDispatchRecord, now, deadline time.Time) {
	t.Helper()
	activation, err := s.verifier.VerifyTargetActivation(m.Activation, m.RuntimeCertificate, m.IssuerCertificate, p.BirthContext(birth), now)
	require.NoError(t, err)
	require.True(t, activation.NotAfter().Equal(deadline))
	require.True(t, activation.NotBefore().Equal(now.Add(-10*time.Second)))
	_, err = s.verifier.VerifyTargetActivation(m.Activation, m.RuntimeCertificate, m.IssuerCertificate, p.BirthContext(birth), deadline.Add(2*time.Second))
	require.Error(t, err, "business activation cannot outlive TTL or issuer")

	verifier, err := p.NewPublicationVerifier(s.config.TrustBinding(), s.config.Roots)
	require.NoError(t, err)
	binding := s.config.TrustBinding()
	context := p.CertificateContext{
		Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch,
		IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, Generation: d.Generation,
		OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, Snapshot: p.SnapshotReference(d.Snapshot), ExpiresAt: d.ExpiresAt,
	}
	certificate, err := verifier.VerifyCertificate(m.PublicationCertificate, context, now)
	require.NoError(t, err)
	var authenticated struct {
		Claims p.RuntimeCertificateClaims `json:"claims"`
	}
	require.NoError(t, json.Unmarshal(certificate.Wire(), &authenticated))
	require.True(t, authenticated.Claims.NotAfter.Equal(deadline))
	require.True(t, authenticated.Claims.NotBefore.Equal(now.Add(-10*time.Second)))
	require.True(t, authenticated.Claims.ExpiresAt.Equal(d.ExpiresAt), "issuer limits do not rewrite business TTL")
	_, err = verifier.VerifyCertificate(m.PublicationCertificate, context, deadline.Add(2*time.Second))
	require.Error(t, err, "business publication cannot outlive TTL or issuer")
}

func TestPlainColdMaterialIdentityOutlivesBusinessTTL(t *testing.T) {
	s, birth, dispatch, now := plainColdMaterialFixture(t, 30*time.Minute)
	m, err := s.material(context.Background(), birth, dispatch, "pod", "pod-uid")
	require.NoError(t, err)
	verifyPlainColdBusinessMaterial(t, s, m, birth, dispatch, now, now.Add(time.Minute))

	// A cleanup started after the 60-second business TTL still needs to cover
	// its original 60-second archive session. This does not reopen business use.
	identity := p.RuntimeIdentityContext{
		SandboxID: dispatch.SandboxID, WorkspaceHash: dispatch.WorkspaceHash, Generation: dispatch.Generation,
		Runtime: p.RuntimeReference{ID: "pod", UID: "pod-uid", BootID: birth.BootID},
	}
	receipt, err := s.verifier.VerifyRuntimeIdentityCertificate(m.RuntimeCertificate, identity, now.Add(90*time.Second))
	require.NoError(t, err, "receipt identity must survive business expiry")
	require.False(t, receipt.NotAfter().Before(now.Add(150*time.Second)), "cover the complete later cleanup session")
	require.True(t, receipt.NotBefore().Equal(now.Add(-10*time.Second)))
	// coldNow includes the measured clock observation duration, bounded at one
	// second; retain the original 15-minute identity limit within that bound.
	require.WithinDuration(t, now.Add(15*time.Minute), receipt.NotAfter(), time.Second)
	_, err = s.verifier.VerifyRuntimeIdentityCertificate(m.RuntimeCertificate, identity, now.Add(15*time.Minute+2*time.Second))
	require.Error(t, err, "the identity interval remains finite")
}

func TestPlainColdMaterialWindowsRespectIssuer(t *testing.T) {
	for _, test := range []struct {
		name             string
		issuerLifetime   time.Duration
		businessLifetime time.Duration
	}{
		{name: "issuer shorter than identity", issuerLifetime: 120 * time.Second, businessLifetime: 60 * time.Second},
		{name: "issuer shorter than business", issuerLifetime: 45 * time.Second, businessLifetime: 45 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, birth, dispatch, now := plainColdMaterialFixture(t, test.issuerLifetime)
			m, err := s.material(context.Background(), birth, dispatch, "pod", "pod-uid")
			require.NoError(t, err)
			verifyPlainColdBusinessMaterial(t, s, m, birth, dispatch, now, now.Add(test.businessLifetime))

			identity := p.RuntimeIdentityContext{
				SandboxID: dispatch.SandboxID, WorkspaceHash: dispatch.WorkspaceHash, Generation: dispatch.Generation,
				Runtime: p.RuntimeReference{ID: "pod", UID: "pod-uid", BootID: birth.BootID},
			}
			receipt, err := s.verifier.VerifyRuntimeIdentityCertificate(m.RuntimeCertificate, identity, now)
			require.NoError(t, err)
			require.True(t, receipt.NotBefore().Equal(now.Add(-10*time.Second)))
			require.True(t, receipt.NotAfter().Equal(now.Add(test.issuerLifetime)), "identity must retain the tighter issuer bound")
			_, err = s.verifier.VerifyRuntimeIdentityCertificate(m.RuntimeCertificate, identity, now.Add(test.issuerLifetime+2*time.Second))
			require.Error(t, err, "identity cannot outlive its issuer")
		})
	}
}
