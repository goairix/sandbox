package deployment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"sync"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type issuerTestClock struct{ now time.Time }

func (c issuerTestClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	return p.ClockObservation{UTC: c.now}, ctx.Err()
}

func mountedIssuerFixture(t *testing.T) (*ExecIssuer, ed25519.PrivateKey, []byte, launcherconfig.Config, p.ExecStartTicketClaims, p.ExecutionDescriptor, *p.ManagementVerifier, time.Time) {
	t.Helper()
	root, rootKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Now().UTC()
	binding := p.TrustBinding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "epoch"}
	config := launcherconfig.Config{Binding: launcherconfig.Binding(binding), Roots: []ed25519.PublicKey{root}}
	certificate, err := p.SignCommandIssuerCertificate(rootKey, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, err)
	verifier, err := p.NewManagementVerifier(binding, config.Roots)
	require.NoError(t, err)
	identity, err := verifier.VerifyCommandIssuerCertificate(certificate, now)
	require.NoError(t, err)
	descriptor, err := p.NewExecutionDescriptor(p.ExecutionRequest{Argv: []string{"/bin/true"}, Env: map[string]string{}, UID: 1000, GID: 1000, WorkDir: "/", TimeoutSeconds: 45})
	require.NoError(t, err)
	c := p.ExecStartContext{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IssuerCertificateID: identity.CertificateID(), IssuerCertificateDigest: identity.Digest(), CommandID: uuid.NewString(), OperationID: uuid.NewString(), RequestID: "request", OperationDigest: strings.Repeat("a", 64), SandboxID: "sandbox", WorkspaceHash: strings.Repeat("b", 64), Generation: 1, DataGateEpoch: 1, ControlRevision: 2, AdmissionRevision: 3, LeaseID: 4, Runtime: p.RuntimeReference{ID: "pod", UID: "uid", BootID: "boot"}, ExpiresAt: now.Add(50 * time.Second)}
	claims := p.ExecStartTicketClaims{Version: 1, Purpose: "operation_exec_start", Context: c, DescriptorDigest: descriptor.Digest(), NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(20 * time.Second)}
	issuer, err := NewExecIssuer(context.Background(), key, certificate, config, issuerTestClock{now})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, issuer.Close()) })
	return issuer, key, certificate, config, claims, descriptor, verifier, now
}

func TestMountedExecIssuerSignsOwnedStartAndRenew(t *testing.T) {
	issuer, key, certificate, config, claims, descriptor, verifier, now := mountedIssuerFixture(t)
	wantCertificate := bytes.Clone(certificate)
	clear(key)
	clear(certificate)
	clear(config.Roots[0])
	ctx := context.Background()
	wire, err := issuer.Certificate(ctx)
	require.NoError(t, err)
	require.Equal(t, wantCertificate, wire)
	clear(wire)
	wire, err = issuer.Certificate(ctx)
	require.NoError(t, err)
	require.Equal(t, wantCertificate, wire)
	start, err := issuer.SignStart(ctx, claims.Context.IssuerCertificateDigest, claims)
	require.NoError(t, err)
	_, err = verifier.VerifyExecStartTicket(start, wantCertificate, claims.Context, descriptor, now)
	require.NoError(t, err)
	renewClaims := p.ExecRenewTicketClaims(claims)
	renewClaims.Purpose = "operation_exec_renew"
	renew, err := issuer.SignRenew(ctx, claims.Context.IssuerCertificateDigest, renewClaims)
	require.NoError(t, err)
	_, err = verifier.VerifyExecRenewTicket(renew, wantCertificate, claims.Context, descriptor.Digest(), now)
	require.NoError(t, err)
}

func TestMountedExecIssuerRejectsWrongSelectionAndPurpose(t *testing.T) {
	issuer, _, _, _, claims, _, _, _ := mountedIssuerFixture(t)
	for _, change := range []func(*p.ExecStartTicketClaims){
		func(c *p.ExecStartTicketClaims) { c.Purpose = "task_close_data" },
		func(c *p.ExecStartTicketClaims) { c.Context.Target = "foreign" },
		func(c *p.ExecStartTicketClaims) { c.Context.IssuerCertificateID = uuid.NewString() },
		func(c *p.ExecStartTicketClaims) { c.Context.IssuerCertificateDigest = strings.Repeat("f", 64) },
		func(c *p.ExecStartTicketClaims) { c.NotBefore = c.NotBefore.Add(-2 * time.Minute) },
	} {
		bad := claims
		change(&bad)
		wire, err := issuer.SignStart(context.Background(), claims.Context.IssuerCertificateDigest, bad)
		require.Error(t, err)
		require.Nil(t, wire)
		bad.Purpose = "operation_exec_renew"
		// The renew signer must independently check binding and selected issuer.
		if bad.Context != claims.Context || !bad.NotBefore.Equal(claims.NotBefore) {
			wire, err = issuer.SignRenew(context.Background(), claims.Context.IssuerCertificateDigest, p.ExecRenewTicketClaims(bad))
			require.Error(t, err)
			require.Nil(t, wire)
		}
	}
	renew := p.ExecRenewTicketClaims(claims)
	renew.Purpose = "operation_exec_renew"
	for _, digest := range []string{"", strings.Repeat("f", 64)} {
		wire, err := issuer.SignStart(context.Background(), digest, claims)
		require.Error(t, err)
		require.Nil(t, wire)
		wire, err = issuer.SignRenew(context.Background(), digest, renew)
		require.Error(t, err)
		require.Nil(t, wire)
	}
	renew.Purpose = "operation_exec_start"
	wire, err := issuer.SignRenew(context.Background(), claims.Context.IssuerCertificateDigest, renew)
	require.Error(t, err)
	require.Nil(t, wire)
}

func TestMountedExecIssuerCancellationAndClose(t *testing.T) {
	issuer, key, certificate, config, claims, _, _, now := mountedIssuerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := issuer.Certificate(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = issuer.SignStart(ctx, claims.Context.IssuerCertificateDigest, claims)
	require.ErrorIs(t, err, context.Canceled)
	renew := p.ExecRenewTicketClaims(claims)
	renew.Purpose = "operation_exec_renew"
	_, err = issuer.SignRenew(ctx, claims.Context.IssuerCertificateDigest, renew)
	require.ErrorIs(t, err, context.Canceled)
	_, err = NewExecIssuer(ctx, key, certificate, config, issuerTestClock{now})
	require.ErrorIs(t, err, context.Canceled)
	_, wrong, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, err = NewExecIssuer(context.Background(), wrong, certificate, config, issuerTestClock{now})
	require.Error(t, err)
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = issuer.SignStart(context.Background(), claims.Context.IssuerCertificateDigest, claims)
		}()
	}
	require.NoError(t, issuer.Close())
	wg.Wait()
	require.Equal(t, make([]byte, ed25519.PrivateKeySize), []byte(issuer.key), "owned mounted key is cleared after signing owners join")
	require.NotEqual(t, make([]byte, ed25519.PrivateKeySize), []byte(key), "closing owned signer cannot clear caller backing")
	_, err = issuer.Certificate(context.Background())
	require.Error(t, err)
	_, err = issuer.SignStart(context.Background(), claims.Context.IssuerCertificateDigest, claims)
	require.Error(t, err)
	require.NoError(t, issuer.Close())
}
