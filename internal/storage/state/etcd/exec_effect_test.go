package etcd

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type execEffectProvider struct {
	wire                        []byte
	key                         ed25519.PrivateKey
	identity                    controlprotocol.CommandIssuerIdentity
	certificateCalls, signCalls int
	sign                        func(context.Context, string, controlprotocol.ExecStartTicketClaims) ([]byte, error)
}

func (p *execEffectProvider) Certificate(ctx context.Context) ([]byte, error) {
	p.certificateCalls++
	return p.wire, ctx.Err()
}
func (p *execEffectProvider) SignStart(ctx context.Context, digest string, c controlprotocol.ExecStartTicketClaims) ([]byte, error) {
	p.signCalls++
	if p.sign != nil {
		return p.sign(ctx, digest, c)
	}
	if digest != p.identity.Digest() {
		return nil, errors.New("wrong certificate digest")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("unbounded signer")
	}
	return controlprotocol.SignExecStartTicket(p.key, p.identity, c)
}

type execProducerFixture struct {
	b        *Backend
	raw      *clientv3.Client
	cap      *OperationCapability
	provider *execEffectProvider
	now      time.Time
	keys     []string
}

func newExecEffectFixture(t *testing.T) *execProducerFixture {
	t.Helper()
	base, raw, in, keys := operationControlFixture(t, "plain")
	now := time.Now().UTC()
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	key := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789012"))
	wire, err := controlprotocol.SignCommandIssuerCertificate(root, controlprotocol.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: base.namespace.Root(), AuthorityID: base.publicationAuthorityID, Target: base.publicationTarget, RestoreEpoch: base.restoreEpoch, PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, err)
	provider := &execEffectProvider{wire: wire, key: key}
	identity, err := decodeIdentity(base.identityValue, base.namespace)
	require.NoError(t, err)
	identity.RestoreEpoch = base.restoreEpoch
	clockCalls := 0
	b, err := New(context.Background(), Options{Endpoints: raw.Endpoints(), Namespace: base.namespace, Identity: identity, AllowInsecureLoopback: true, RequestTimeout: 3 * time.Second, ExecIssuer: provider, Clock: testAuthorityClock(func(context.Context) (ClockObservation, error) { clockCalls++; return ClockObservation{UTC: now}, nil }), PublicationTrust: &RuntimePublicationTrust{AuthorityID: base.publicationAuthorityID, Target: base.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.Zero(t, provider.certificateCalls)
	require.Zero(t, provider.signCalls)
	require.Zero(t, clockCalls)
	provider.identity, err = b.execVerifier.VerifyCommandIssuerCertificate(wire, now)
	require.NoError(t, err)
	parent, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	admitted, err := b.BeginOperation(parent, BeginOperationInput{SandboxID: in.SandboxID, RequestID: "exec-request", Kind: OperationData})
	require.NoError(t, err)
	require.NotNil(t, admitted.Capability)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = raw.Revoke(ctx, clientv3.LeaseID(admitted.Reference.LeaseID))
	})
	return &execProducerFixture{b, raw, admitted.Capability, provider, now, keys}
}
func execEffectRequest() controlprotocol.ExecutionRequest {
	return controlprotocol.ExecutionRequest{Argv: []string{"/bin/tool", "private-argument"}, Env: map[string]string{"SECRET": "private-env"}, UID: 1000, GID: 1001, WorkDir: "/work", TimeoutSeconds: 10, Stdin: []byte("private-input")}
}
func draftForTest(t *testing.T, f *execProducerFixture) *execEffectDraft {
	t.Helper()
	descriptor, err := controlprotocol.NewExecutionDescriptor(execEffectRequest())
	require.NoError(t, err)
	return &execEffectDraft{descriptor: descriptor, deadline: f.cap.deadline, commandID: uuid.NewString(), outcome: OutcomeUnknown}
}

// Losing the original context, accepting a modified signed payload, or reminting
// claims after a transient signer error must break these real protocol tests.
func TestPrepareExecEffectSigning(t *testing.T) {
	f := newExecEffectFixture(t)
	d := draftForTest(t, f)
	var first controlprotocol.ExecStartTicketClaims
	f.provider.sign = func(ctx context.Context, digest string, c controlprotocol.ExecStartTicketClaims) ([]byte, error) {
		require.Equal(t, f.provider.identity.Digest(), digest)
		require.Equal(t, f.cap.record.Reference.OperationID, c.Context.OperationID)
		require.Equal(t, f.cap.admitRevision, c.Context.AdmissionRevision)
		first = c
		return nil, errors.New("transient signer")
	}
	err := f.b.signExecEffect(context.Background(), f.cap, d)
	require.ErrorContains(t, err, "transient signer")
	require.Equal(t, uint32(1), first.Version)
	require.Equal(t, "operation_exec_start", first.Purpose)
	require.Equal(t, d.commandID, first.Context.CommandID)
	require.WithinDuration(t, f.now.Add(-2*time.Second), first.NotBefore, 20*time.Millisecond)
	require.Equal(t, 30*time.Second, first.NotAfter.Sub(first.NotBefore))
	f.provider.sign = nil
	require.NoError(t, f.b.signExecEffect(context.Background(), f.cap, d))
	require.Equal(t, first, d.claims)
	require.NotEmpty(t, d.ticket.Wire())
	require.NoError(t, f.b.signExecEffect(context.Background(), f.cap, d))
	require.Equal(t, 2, f.provider.signCalls)
	require.Equal(t, 1, f.provider.certificateCalls)
	require.Equal(t, f.cap.record.Runtime.ID, d.ticket.Context().Runtime.ID)
}
func TestPrepareExecEffectSigningRejects(t *testing.T) {
	for _, defect := range []string{"payload", "context", "purpose", "signature", "short", "expired", "uncertainty"} {
		t.Run(defect, func(t *testing.T) {
			f := newExecEffectFixture(t)
			d := draftForTest(t, f)
			if defect == "short" {
				d.deadline = time.Now().Add(2 * time.Second)
			}
			f.provider.sign = func(ctx context.Context, digest string, c controlprotocol.ExecStartTicketClaims) ([]byte, error) {
				switch defect {
				case "payload":
					c.DescriptorDigest = string(make([]byte, 64))
				case "context":
					c.Context.CommandID = uuid.NewString()
				case "purpose":
					c.Purpose = "operation_exec_renew"
				}
				if defect == "expired" {
					f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
						return ClockObservation{UTC: f.now.Add(time.Minute)}, nil
					})
				}
				if defect == "uncertainty" {
					f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
						return ClockObservation{UTC: f.now, Uncertainty: 2 * time.Second}, nil
					})
				}
				wire, err := controlprotocol.SignExecStartTicket(f.provider.key, f.provider.identity, c)
				if defect == "signature" && err == nil {
					wire[len(wire)-5] ^= 1
				}
				return wire, err
			}
			require.Error(t, f.b.signExecEffect(context.Background(), f.cap, d))
			require.Empty(t, d.ticket.Wire())
		})
	}
}
