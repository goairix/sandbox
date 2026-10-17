package deployment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

var (
	_ state.TaskCommandIssuer            = (*CleanupIssuer)(nil)
	_ state.TaskQuiescenceIssuer         = (*CleanupIssuer)(nil)
	_ state.TaskWorkspaceArchiveIssuer   = (*CleanupIssuer)(nil)
	_ state.TaskRuntimeTerminationIssuer = (*CleanupIssuer)(nil)
)

type cleanupIssuerFixture struct {
	issuer      *CleanupIssuer
	key         ed25519.PrivateKey
	certificate []byte
	config      launcherconfig.Config
	verifier    *p.ManagementVerifier
	now         time.Time
	close       p.TaskCloseDataTicketClaims
	quiesce     p.TaskUserQuiescenceTicketClaims
	archive     p.TaskWorkspaceArchiveTicketClaims
	termination p.TaskRuntimeTerminationTicketClaims
}

// Signed tickets below are protocol attribution only. No fixture claims a
// live TaskClaim, drain, archive manifest, observer proof, or runtime permission.
func newCleanupIssuerFixture(t *testing.T) cleanupIssuerFixture {
	t.Helper()
	_, key, certificate, config, exec, _, verifier, now := mountedIssuerFixture(t)
	issuer, err := NewCleanupIssuer(context.Background(), key, certificate, config, issuerTestClock{now})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, issuer.Close()) })
	c := exec.Context
	current := p.TaskCloseDataContext{
		Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch,
		IssuerCertificateID: c.IssuerCertificateID, IssuerCertificateDigest: c.IssuerCertificateDigest,
		CommandID: uuid.NewString(), TaskID: uuid.NewString(), TaskDigest: strings.Repeat("a", 64), ClaimID: uuid.NewString(), WorkerID: "cleanup-worker",
		SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: 1, DataGateEpoch: 1, ControlRevision: 2, ClaimCreateRevision: 3, LeaseID: 4, Runtime: c.Runtime,
	}
	f := cleanupIssuerFixture{issuer: issuer, key: key, certificate: certificate, config: config, verifier: verifier, now: now}
	f.close = p.TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: current, NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(20 * time.Second)}
	f.quiesce = p.TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: p.TaskUserQuiescenceContext{Current: current, CloseDataContext: current, CloseDataTicketDigest: strings.Repeat("b", 64), CloseDataReceiptDigest: strings.Repeat("c", 64), CloseDataIntentRevision: 4}, NotBefore: f.close.NotBefore, NotAfter: f.close.NotAfter}
	archiveCurrent := current
	archiveCurrent.CommandID = uuid.NewString()
	f.archive = p.TaskWorkspaceArchiveTicketClaims{Version: 1, Purpose: "task_workspace_archive", Context: p.TaskWorkspaceArchiveContext{Current: archiveCurrent, QuiescenceContext: f.quiesce.Context, QuiescenceTicketDigest: strings.Repeat("d", 64), QuiescenceReceiptDigest: strings.Repeat("e", 64), PlanDigest: strings.Repeat("f", 64), QuiescenceIntentRevision: 5, FinalSyncRevision: 6}, NotBefore: f.close.NotBefore, NotAfter: f.close.NotAfter, SessionNotAfter: now.Add(40 * time.Second)}
	f.termination = p.TaskRuntimeTerminationTicketClaims{Version: 1, Purpose: "task_runtime_terminate", Current: current, Partition: 0xbb, IntentCreateRevision: 7, IntentModRevision: 7, IntentDigest: strings.Repeat("a", 64), EnrollmentDigest: strings.Repeat("b", 64), ObserverCertificateDigest: strings.Repeat("c", 64), NodeUID: uuid.NewString(), NodeBootID: uuid.NewString(), NotBefore: f.close.NotBefore, NotAfter: f.close.NotAfter}
	return f
}

func (f cleanupIssuerFixture) sign(ctx context.Context, kind, digest string) ([]byte, error) {
	switch kind {
	case "close":
		return f.issuer.SignCloseData(ctx, digest, f.close)
	case "quiesce":
		return f.issuer.SignQuiesceUsers(ctx, digest, f.quiesce)
	case "archive":
		return f.issuer.SignWorkspaceArchive(ctx, digest, f.archive)
	default:
		return f.issuer.SignRuntimeTermination(ctx, digest, f.termination)
	}
}

func TestMountedCleanupIssuerSigns(t *testing.T) {
	f := newCleanupIssuerFixture(t)
	want := bytes.Clone(f.certificate)
	clear(f.key)
	clear(f.certificate)
	clear(f.config.Roots[0])
	wire, err := f.issuer.Certificate(context.Background())
	require.NoError(t, err)
	require.Equal(t, want, wire)
	clear(wire)
	wire, err = f.issuer.Certificate(context.Background())
	require.NoError(t, err)
	require.Equal(t, want, wire)

	for _, kind := range []string{"close", "quiesce", "archive", "termination"} {
		t.Run(kind, func(t *testing.T) {
			wire, err := f.sign(context.Background(), kind, f.close.Context.IssuerCertificateDigest)
			require.NoError(t, err)
			switch kind {
			case "close":
				_, err = f.verifier.VerifyTaskCloseDataTicket(wire, want, f.close.Context, f.now)
			case "quiesce":
				_, err = f.verifier.VerifyTaskUserQuiescenceTicket(wire, want, f.quiesce.Context, f.now)
			case "archive":
				_, err = f.verifier.VerifyTaskWorkspaceArchiveTicket(wire, want, f.archive.Context, f.now)
			default:
				_, err = f.verifier.VerifyTaskRuntimeTerminationTicket(wire, want, f.termination, f.now)
			}
			require.NoError(t, err)
		})
	}
}

func TestMountedCleanupIssuerRefusal(t *testing.T) {
	f := newCleanupIssuerFixture(t)
	for _, kind := range []string{"close", "quiesce", "archive", "termination"} {
		t.Run(kind, func(t *testing.T) {
			for name, change := range map[string]func(*cleanupIssuerFixture){
				"purpose": func(g *cleanupIssuerFixture) {
					g.close.Purpose, g.quiesce.Purpose, g.archive.Purpose, g.termination.Purpose = "operation_exec_start", "operation_exec_start", "operation_exec_start", "operation_exec_start"
				},
				"binding": func(g *cleanupIssuerFixture) {
					g.close.Context.Target, g.quiesce.Context.Current.Target, g.archive.Context.Current.Target, g.termination.Current.Target = "foreign", "foreign", "foreign", "foreign"
				},
				"issuer-id": func(g *cleanupIssuerFixture) {
					id := uuid.NewString()
					g.close.Context.IssuerCertificateID, g.quiesce.Context.Current.IssuerCertificateID, g.archive.Context.Current.IssuerCertificateID, g.termination.Current.IssuerCertificateID = id, id, id, id
				},
				"issuer-digest": func(g *cleanupIssuerFixture) {
					digest := strings.Repeat("0", 64)
					g.close.Context.IssuerCertificateDigest, g.quiesce.Context.Current.IssuerCertificateDigest, g.archive.Context.Current.IssuerCertificateDigest, g.termination.Current.IssuerCertificateDigest = digest, digest, digest, digest
				},
				"certificate-window": func(g *cleanupIssuerFixture) {
					before, after := f.now.Add(2*time.Minute), f.now.Add(2*time.Minute+20*time.Second)
					g.close.NotBefore, g.quiesce.NotBefore, g.archive.NotBefore, g.termination.NotBefore = before, before, before, before
					g.close.NotAfter, g.quiesce.NotAfter, g.archive.NotAfter, g.termination.NotAfter = after, after, after, after
					g.archive.SessionNotAfter = after.Add(10 * time.Second)
				},
			} {
				t.Run(name, func(t *testing.T) {
					bad := f
					change(&bad)
					wire, err := bad.sign(context.Background(), kind, f.close.Context.IssuerCertificateDigest)
					require.Error(t, err)
					require.Nil(t, wire)
				})
			}
			for _, digest := range []string{"", strings.Repeat("0", 64)} {
				wire, err := f.sign(context.Background(), kind, digest)
				require.Error(t, err)
				require.Nil(t, wire)
			}
		})
	}
	bad := f
	bad.archive.SessionNotAfter = f.now.Add(2 * time.Minute)
	wire, err := bad.sign(context.Background(), "archive", f.close.Context.IssuerCertificateDigest)
	require.Error(t, err, "the full archive session must fit the issuer certificate")
	require.Nil(t, wire)
	wrong := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{42}, ed25519.SeedSize))
	_, err = NewCleanupIssuer(context.Background(), wrong, f.certificate, f.config, issuerTestClock{f.now})
	require.Error(t, err)
	var absentClock *issuerTestClock
	_, err = NewCleanupIssuer(context.Background(), f.key, f.certificate, f.config, absentClock)
	require.Error(t, err)
	_, err = NewCleanupIssuer(context.Background(), f.key, f.certificate, f.config, issuerTestClock{f.now.Add(2 * time.Minute)})
	require.Error(t, err)
}

func TestMountedCleanupIssuerClose(t *testing.T) {
	f := newCleanupIssuerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewCleanupIssuer(ctx, f.key, f.certificate, f.config, issuerTestClock{f.now})
	require.ErrorIs(t, err, context.Canceled)
	_, err = f.issuer.Certificate(ctx)
	require.ErrorIs(t, err, context.Canceled)
	for _, kind := range []string{"close", "quiesce", "archive", "termination"} {
		_, err = f.sign(ctx, kind, f.close.Context.IssuerCertificateDigest)
		require.ErrorIs(t, err, context.Canceled)
	}
	var owners sync.WaitGroup
	for _, kind := range []string{"close", "quiesce", "archive", "termination"} {
		owners.Add(1)
		go func(kind string) {
			defer owners.Done()
			_, _ = f.sign(context.Background(), kind, f.close.Context.IssuerCertificateDigest)
		}(kind)
	}
	require.NoError(t, f.issuer.Close())
	owners.Wait()
	require.Equal(t, make([]byte, ed25519.PrivateKeySize), []byte(f.issuer.key))
	require.NotEqual(t, make([]byte, ed25519.PrivateKeySize), []byte(f.key))
	_, err = f.issuer.Certificate(context.Background())
	require.Error(t, err)
	for _, kind := range []string{"close", "quiesce", "archive", "termination"} {
		wire, err := f.sign(context.Background(), kind, f.close.Context.IssuerCertificateDigest)
		require.Error(t, err)
		require.Nil(t, wire)
	}
	require.NoError(t, f.issuer.Close())
}
