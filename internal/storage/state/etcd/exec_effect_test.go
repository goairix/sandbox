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

// This fails if Prepare skips the metadata commit, exposes a changed payload,
// remints on retry/renewal, or returns authorization without current fences.
func TestPrepareExecEffect(t *testing.T) {
	f := newExecEffectFixture(t)
	request := execEffectRequest()
	descriptor, err := controlprotocol.NewExecutionDescriptor(request)
	require.NoError(t, err)
	result, err := f.b.PrepareExecEffect(context.Background(), f.cap, request)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.NotNil(t, result.Prepared)
	require.Equal(t, result.Reference, result.Prepared.Reference())
	require.NoError(t, result.Reference.Validate())
	entry, err := f.b.LoadExecEffect(context.Background(), result.Reference)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Equal(t, descriptor.Digest(), entry.Record.DescriptorDigest)
	require.Equal(t, f.cap.record, entry.Record.Operation)
	require.Equal(t, f.cap.admitRevision, entry.Record.AdmissionRevision)
	request.Argv[1] = "mutated"
	request.Env["SECRET"] = "mutated"
	request.Stdin[0] = '!'
	require.Equal(t, execEffectRequest(), f.cap.execDraft.descriptor.Request())
	conflict, err := f.b.PrepareExecEffect(context.Background(), f.cap, request)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, conflict.Prepared)
	claims := f.cap.execDraft.claims
	ticket := f.cap.execDraft.ticket.Wire()
	stage := f.cap.execDraft.stage
	require.NoError(t, f.b.RenewOperation(context.Background(), f.cap))
	retry, err := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
	require.NoError(t, err)
	require.NotNil(t, retry.Prepared)
	require.Equal(t, result.Reference, retry.Reference)
	require.Equal(t, claims, f.cap.execDraft.claims)
	require.Equal(t, ticket, f.cap.execDraft.ticket.Wire())
	require.Same(t, stage, f.cap.execDraft.stage)
	require.Equal(t, 1, f.provider.signCalls)
	again, err := f.b.LoadExecEffect(context.Background(), result.Reference)
	require.NoError(t, err)
	require.Equal(t, entry.Revision, again.Revision)
	_, err = f.raw.Put(context.Background(), f.cap.fences[0].Key, string(f.cap.fences[0].Value))
	require.NoError(t, err)
	denied, err := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, denied.Prepared)
	require.Equal(t, OutcomeCommitted, denied.Outcome)
}
func TestPrepareExecEffectRejects(t *testing.T) {
	for _, defect := range []string{"nil context", "cancelled", "nil capability", "foreign", "public fake", "mutation", "lost", "parent", "deadline", "payload", "config"} {
		t.Run(defect, func(t *testing.T) {
			f := newExecEffectFixture(t)
			c := f.cap
			ctx := context.Background()
			request := execEffectRequest()
			switch defect {
			case "nil context":
				ctx = nil
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "nil capability":
				c = nil
			case "foreign":
				c.origin = &Backend{}
			case "public fake":
				c = &OperationCapability{record: OperationRecord{Reference: c.Reference()}}
			case "mutation":
				c.record.Reference.Kind = OperationMutation
			case "lost":
				c.lost = true
			case "parent":
				c.parentCtx = nil
			case "deadline":
				c.deadline = time.Now().Add(-time.Second)
			case "payload":
				request.UID = 0
			case "config":
				f.b.execIssuer = nil
			}
			result, err := f.b.PrepareExecEffect(ctx, c, request)
			require.Error(t, err)
			require.Nil(t, result.Prepared)
			require.Zero(t, f.provider.signCalls)
			require.Zero(t, f.provider.certificateCalls)
		})
	}
}

func TestPrepareExecEffectCleanup(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			f := newExecEffectFixture(t)
			lease := &creationFaultLease{Lease: f.b.client.Lease}
			if fail {
				lease.revoke = func(context.Context, clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
					return nil, errors.New("cleanup unavailable")
				}
			}
			f.b.client.Lease = lease
			result, err := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, result.Outcome)
			require.NotNil(t, result.Prepared)
			require.Equal(t, int64(1), lease.revokes.Load())
			require.Equal(t, int64(1), lease.grants.Load())
			require.Zero(t, lease.keeps.Load())
			if fail {
				require.ErrorContains(t, result.GuardCleanupError, "cleanup unavailable")
			} else {
				require.NoError(t, result.GuardCleanupError)
			}
			lease.revoke = nil
			retry, err := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
			require.NoError(t, err)
			require.NotNil(t, retry.Prepared)
			require.NoError(t, retry.GuardCleanupError)
			want := int64(1)
			if fail {
				want = 2
			}
			require.Equal(t, want, lease.revokes.Load())
			ttl, err := f.raw.TimeToLive(context.Background(), clientv3.LeaseID(f.cap.record.Reference.LeaseID))
			require.NoError(t, err)
			require.Positive(t, ttl.TTL)
		})
	}
}
