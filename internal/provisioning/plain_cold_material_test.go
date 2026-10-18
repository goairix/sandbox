package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"k8s.io/client-go/rest"
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

// These are certification-window tests, not evidence of runtime installation.
// The real HTTP TTL fixture must separately cross the original wall-clock TTL.
func TestPlainColdTTLActivationReusesValidReceiptIdentity(t *testing.T) {
	s, birth, dispatch, now := plainColdMaterialFixture(t, 30*time.Minute)
	m, err := s.material(context.Background(), birth, dispatch, "pod", "pod-uid")
	require.NoError(t, err)
	current, err := s.verifier.VerifyTargetActivation(m.Activation, m.RuntimeCertificate, m.IssuerCertificate, p.BirthContext(birth), now)
	require.NoError(t, err)
	baseline := now.Add(time.Second)
	expires := baseline.Add(30 * time.Second)
	next, err := s.ttlActivation(baseline, expires, current)
	require.NoError(t, err)
	require.Equal(t, current.RuntimeCertificate(), next.RuntimeCertificate(), "ordinary short TTL updates must not rotate a usable TLS pin")
	require.Equal(t, current.Identity(), next.Identity())
	require.Equal(t, current.Birth(), next.Birth())
	require.Equal(t, current.DataGateEpoch(), next.DataGateEpoch())
	require.True(t, next.NotBefore().After(current.NotBefore()))
	require.True(t, next.NotAfter().Equal(expires))
	require.True(t, next.NotAfter().Before(current.NotAfter()), "a new baseline may legally shorten the old business deadline")
	_, err = s.ttlActivation(current.NotAfter().Add(2*time.Second), expires.Add(time.Hour), current)
	require.Error(t, err, "an expired activation cannot be revived by signing a fresh candidate")
}

func TestPlainColdTTLActivationRefreshesSameKeyWithinIssuer(t *testing.T) {
	s, birth, dispatch, now := plainColdMaterialFixture(t, 30*time.Minute)
	m, err := s.material(context.Background(), birth, dispatch, "pod", "pod-uid")
	require.NoError(t, err)
	current, err := s.verifier.VerifyTargetActivation(m.Activation, m.RuntimeCertificate, m.IssuerCertificate, p.BirthContext(birth), now)
	require.NoError(t, err)
	baseline := now.Add(2 * time.Second)
	next, err := s.ttlActivation(baseline, baseline.Add(time.Hour), current)
	require.NoError(t, err)
	require.NotEqual(t, current.RuntimeCertificate(), next.RuntimeCertificate())
	original, err := s.verifier.VerifyRuntimeIdentityCertificate(current.RuntimeCertificate(), current.Identity(), baseline)
	require.NoError(t, err)
	renewed, err := s.verifier.VerifyRuntimeIdentityCertificate(next.RuntimeCertificate(), next.Identity(), baseline)
	require.NoError(t, err)
	require.Equal(t, original.PublicKey(), renewed.PublicKey())
	require.True(t, original.NotBefore().Equal(renewed.NotBefore()), "fixed candidate pin must also cover the original TLS leaf")
	require.True(t, renewed.NotAfter().Equal(baseline.Add(15*time.Minute)))
	require.True(t, next.NotAfter().Equal(renewed.NotAfter()))
	require.Equal(t, current.IssuerCertificate(), next.IssuerCertificate())
}

// A credential refresh preserves a prior user TTL decision. These signed
// material assertions do not stand in for installation or a service operation;
// the Backend's v3 tests cover the separate persisted authentication clock.
func TestPlainColdTTLRefreshPreservesPriorUserExpiry(t *testing.T) {
	s, birth, dispatch, now := plainColdMaterialFixture(t, 45*time.Minute)
	m, err := s.material(context.Background(), birth, dispatch, "pod", "pod-uid")
	require.NoError(t, err)
	initial, err := s.verifier.VerifyTargetActivation(m.Activation, m.RuntimeCertificate, m.IssuerCertificate, p.BirthContext(birth), now)
	require.NoError(t, err)
	userBaseline, timeout := now.Add(2*time.Second), 1800
	expires := userBaseline.Add(time.Duration(timeout) * time.Second)
	current, err := s.ttlActivation(userBaseline, expires, initial)
	require.NoError(t, err)
	refreshAt := userBaseline.Add(10 * time.Second)
	refreshed, err := s.ttlActivation(refreshAt, expires, current)
	require.NoError(t, err)
	require.Equal(t, current.Identity(), refreshed.Identity())
	require.Equal(t, current.Birth(), refreshed.Birth())
	require.Equal(t, current.DataGateEpoch(), refreshed.DataGateEpoch())
	require.True(t, refreshed.NotBefore().After(current.NotBefore()))
	require.True(t, refreshed.NotAfter().After(current.NotAfter()), "only the finite credential window advances")
	preservedBaseline := expires.Add(-time.Duration(timeout) * time.Second)
	require.Equal(t, userBaseline, preservedBaseline)
	require.Equal(t, expires, preservedBaseline.Add(time.Duration(timeout)*time.Second))
	require.NotEqual(t, expires, refreshAt.Add(time.Duration(timeout)*time.Second), "UpdateTTL(oldTimeout) would incorrectly slide the business TTL")
	_, err = s.verifier.VerifyTargetActivation(refreshed.Wire(), refreshed.RuntimeCertificate(), refreshed.IssuerCertificate(), refreshed.Birth(), preservedBaseline)
	require.Error(t, err, "historical authentication must not reuse the preserved business baseline")
	_, err = s.verifier.VerifyTargetActivation(refreshed.Wire(), refreshed.RuntimeCertificate(), refreshed.IssuerCertificate(), refreshed.Birth(), refreshAt)
	require.NoError(t, err)
}

// Selection consumes only loader-authenticated metadata in production. These
// private DTO cases test interval policy, never installation or admission.
func TestPlainColdCredentialRefreshIntervalSelection(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	control := &state.SandboxControlRecord{Phase: state.PhaseActive, Runtime: &state.RuntimeReference{ID: "pod"}, ExpiresAt: now.Add(time.Hour)}
	loaded := &plainColdLoadedTarget{control: control, capsule: plainColdCapsule{TTL: time.Hour}}
	baseline, seconds, err := credentialRefreshParameters(loaded)
	require.NoError(t, err)
	require.Equal(t, now, baseline)
	require.Equal(t, 3600, seconds)

	// An intervening user TTL change overrides the original capsule's timeout.
	loaded.ttlCurrent = &state.SandboxTTLCurrent{Baseline: now.Add(30 * time.Minute), Timeout: 1800, ExpiresAt: control.ExpiresAt}
	baseline, seconds, err = credentialRefreshParameters(loaded)
	require.NoError(t, err)
	require.Equal(t, loaded.ttlCurrent.Baseline, baseline)
	require.Equal(t, loaded.ttlCurrent.Timeout, seconds)
	loaded.ttlPending = &state.SandboxTTLPending{Baseline: baseline, Timeout: seconds, ExpiresAt: control.ExpiresAt}
	_, _, err = credentialRefreshParameters(loaded)
	require.NoError(t, err, "only the exact preserved pending interval is selectable for query")
	for name, change := range map[string]func(*state.SandboxTTLPending){
		"user-timeout":  func(r *state.SandboxTTLPending) { r.Timeout++ },
		"user-expiry":   func(r *state.SandboxTTLPending) { r.ExpiresAt = r.ExpiresAt.Add(time.Second) },
		"user-baseline": func(r *state.SandboxTTLPending) { r.Baseline = r.Baseline.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			pending := *loaded.ttlPending
			change(&pending)
			input := *loaded
			input.ttlPending = &pending
			_, _, err := credentialRefreshParameters(&input)
			require.ErrorIs(t, err, state.ErrOutcomeUnknown)
		})
	}
	loaded.ttlCurrent, loaded.ttlPending = nil, nil
	loaded.capsule.TTL = time.Hour + time.Nanosecond
	_, _, err = credentialRefreshParameters(loaded)
	require.ErrorIs(t, err, state.ErrInvalidRecord, "legacy fractional timeout must not be rounded")
	loaded.control.Phase = state.PhaseDestroying
	_, _, err = credentialRefreshParameters(loaded)
	require.ErrorIs(t, err, ErrPlainColdUnavailable)
}

func TestPlainColdCredentialRefreshAdmissionAndClose(t *testing.T) {
	var missing *PlainColdService
	_, err := missing.RefreshCredentials(context.Background(), "sandbox", "request")
	require.ErrorIs(t, err, ErrPlainColdProfile)
	s, _, _ := coldBusinessFixture(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.RefreshCredentials(canceled, "sandbox", "request")
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, s.active)

	// Reuse the original cancellation-only native KV endpoint. No publication,
	// signer, lease or runtime model exists here, so this cannot prove install.
	ns, err := state.NewNamespace("/sandbox", "business", "cell")
	require.NoError(t, err)
	identity := state.Identity{SchemaVersion: 1, Prefix: "/sandbox", AuthorityID: "business", Cell: "cell", ClusterID: 1, StorageID: "storage", RuntimeID: "launcher", RestoreEpoch: "restore"}
	wire, err := json.Marshal(identity)
	require.NoError(t, err)
	endpoint := &businessLookupCancellationServer{identity: wire, root: ns.Root(), entered: make(chan struct{}), joined: make(chan struct{})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	rpc := grpc.NewServer()
	pb.RegisterKVServer(rpc, endpoint)
	pb.RegisterMaintenanceServer(rpc, endpoint)
	served := make(chan error, 1)
	go func() { served <- rpc.Serve(listener) }()
	t.Cleanup(func() {
		rpc.Stop()
		require.NoError(t, <-served)
	})
	backend, err := state.New(context.Background(), state.Options{Endpoints: []string{"http://" + listener.Addr().String()}, Namespace: ns, Identity: identity, AllowInsecureLoopback: true, RequestTimeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	api := kube.EnrolledBridgeOptions{Config: &rest.Config{Host: "https://127.0.0.1:1"}, Namespace: "runtimes", MaxActive: 1, RequestTimeout: time.Second}
	s.driver, err = kube.NewEnrolledCreateClient(kube.EnrolledCreateOptions{API: api, Image: "registry.example/sandbox:v1@sha256:" + strings.Repeat("a", 64), NodeName: "node", NodeUID: uuid.NewString(), NodeBootID: uuid.NewString()})
	require.NoError(t, err)
	s.bridge, err = kube.NewEnrolledBridgeClient(api)
	require.NoError(t, err)
	s.backend, s.closeDone = backend, make(chan struct{})
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	done := make(chan error, 1)
	go func() {
		_, err := s.RefreshCredentials(context.Background(), "sandbox", "request")
		done <- err
	}()
	select {
	case <-endpoint.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("credential refresh did not enter its original Backend lookup")
	}
	_, err = s.RefreshCredentials(context.Background(), "sandbox", "other")
	require.ErrorIs(t, err, state.ErrConflict, "refresh shares the original finite service admission")
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not retire the original refresh call")
	}
	require.NoError(t, <-closed)
	select {
	case <-endpoint.joined:
	case <-time.After(time.Second):
		t.Fatal("original Backend RPC did not join after cancellation")
	}
	_, err = s.RefreshCredentials(context.Background(), "sandbox", "after-close")
	require.ErrorIs(t, err, state.ErrConflict)
	require.Empty(t, s.active)
}
