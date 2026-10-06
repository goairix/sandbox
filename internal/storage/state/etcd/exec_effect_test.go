package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
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
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("unbounded certificate provider")
	}
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
	originalDeadline := f.cap.execDraft.deadline
	claims := f.cap.execDraft.claims
	ticket := f.cap.execDraft.ticket.Wire()
	stage := f.cap.execDraft.stage
	require.NoError(t, f.b.RenewOperation(context.Background(), f.cap))
	retry, err := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
	require.NoError(t, err)
	require.NotNil(t, retry.Prepared)
	require.Equal(t, result.Reference, retry.Reference)
	require.Equal(t, claims, f.cap.execDraft.claims)
	require.Equal(t, originalDeadline, f.cap.execDraft.deadline)
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

func execEffectRawTicket(t *testing.T, key ed25519.PrivateKey, claims controlprotocol.ExecStartTicketClaims) []byte {
	t.Helper()
	body, err := json.Marshal(claims)
	require.NoError(t, err)
	wire, err := json.Marshal(struct {
		Claims    controlprotocol.ExecStartTicketClaims `json:"claims"`
		Signature []byte                                `json:"signature"`
	}{claims, ed25519.Sign(key, append([]byte("sandbox-exec-start-ticket:v1\x00"), body...))})
	require.NoError(t, err)
	return wire
}

// Correct cryptographic signatures over the wrong claims must not turn the
// provider into an authority to choose the actual payload or operation context.
func TestPrepareExecEffectProviderCannotChooseClaims(t *testing.T) {
	for _, defect := range []string{"payload", "operation", "runtime", "lease", "admission", "namespace", "authority", "target", "epoch", "issuer", "purpose", "time", "signature", "root"} {
		t.Run(defect, func(t *testing.T) {
			f := newExecEffectFixture(t)
			if defect == "root" {
				var envelope struct {
					Claims controlprotocol.CommandIssuerCertificateClaims `json:"claims"`
				}
				require.NoError(t, json.Unmarshal(f.provider.wire, &envelope))
				wire, err := controlprotocol.SignCommandIssuerCertificate(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{99}, 32)), envelope.Claims)
				require.NoError(t, err)
				f.provider.wire = wire
			}
			f.provider.sign = func(ctx context.Context, digest string, c controlprotocol.ExecStartTicketClaims) ([]byte, error) {
				require.Equal(t, f.provider.identity.Digest(), digest)
				switch defect {
				case "payload":
					c.DescriptorDigest = strings.Repeat("a", 64)
				case "operation":
					c.Context.OperationID = uuid.NewString()
				case "runtime":
					c.Context.Runtime.UID = "foreign"
				case "lease":
					c.Context.LeaseID++
				case "admission":
					c.Context.AdmissionRevision++
				case "namespace":
					c.Context.Namespace = "/other/scope/cell/"
				case "authority":
					c.Context.AuthorityID = "foreign"
				case "target":
					c.Context.Target = "foreign"
				case "epoch":
					c.Context.RestoreEpoch = "foreign"
				case "issuer":
					c.Context.IssuerCertificateID = uuid.NewString()
				case "purpose":
					c.Purpose = "operation_exec_renew"
				case "time":
					c.NotAfter = c.NotAfter.Add(-time.Second)
				}
				key := f.provider.key
				if defect == "signature" {
					key = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{77}, 32))
				}
				return execEffectRawTicket(t, key, c), nil
			}
			lease := &creationFaultLease{Lease: f.b.client.Lease}
			f.b.client.Lease = lease
			result, err := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
			require.Error(t, err)
			require.Nil(t, result.Prepared)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			require.Zero(t, lease.grants.Load())
			require.Empty(t, f.cap.execDraft.ticket.Wire())
		})
	}
}

func TestPrepareExecEffectOriginalClaimsAndCopies(t *testing.T) {
	f := newExecEffectFixture(t)
	request := execEffectRequest()
	original := execEffectRequest()
	var returned []byte
	f.provider.sign = func(ctx context.Context, digest string, c controlprotocol.ExecStartTicketClaims) ([]byte, error) {
		op := f.cap.record
		r := op.Reference
		expected := controlprotocol.ExecStartContext{Namespace: r.Namespace, AuthorityID: f.b.publicationAuthorityID, Target: f.b.publicationTarget, RestoreEpoch: r.RestoreEpoch, IssuerCertificateID: f.provider.identity.CertificateID(), IssuerCertificateDigest: f.provider.identity.Digest(), CommandID: f.cap.execDraft.commandID, OperationID: r.OperationID, RequestID: r.RequestID, OperationDigest: r.Digest, SandboxID: r.SandboxID, WorkspaceHash: op.WorkspaceHash, Generation: op.Generation, DataGateEpoch: op.DataGateEpoch, ControlRevision: op.ControlRevision, AdmissionRevision: f.cap.admitRevision, LeaseID: r.LeaseID, Runtime: controlprotocol.RuntimeReference{ID: "id", UID: "uid", BootID: "boot"}, ExpiresAt: op.ExpiresAt}
		require.Equal(t, expected, c.Context)
		request.Argv[1] = "external change"
		request.Env["SECRET"] = "external change"
		request.Stdin[0] = '!'
		wire := execEffectRawTicket(t, f.provider.key, c)
		var pretty bytes.Buffer
		require.NoError(t, json.Indent(&pretty, wire, "", "  "))
		returned = pretty.Bytes()
		f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
			returned[0] = '!'
			return ClockObservation{UTC: f.now}, nil
		})
		return returned, nil
	}
	result, err := f.b.PrepareExecEffect(context.Background(), f.cap, request)
	require.NoError(t, err)
	require.NotNil(t, result.Prepared)
	require.Equal(t, original, f.cap.execDraft.descriptor.Request())
	require.NotEqual(t, returned, f.cap.execDraft.ticket.Wire())
	require.True(t, json.Valid(f.cap.execDraft.ticket.Wire()))
	typ := reflect.TypeOf(PreparedExecEffect{})
	for i := 0; i < typ.NumField(); i++ {
		require.False(t, typ.Field(i).IsExported())
	}
	methods := reflect.TypeOf(result.Prepared)
	require.Equal(t, 1, methods.NumMethod())
	require.Equal(t, "Reference", methods.Method(0).Name)
	var zero PreparedExecEffect
	require.Equal(t, ExecEffectReference{}, zero.Reference())
	require.Equal(t, ExecEffectReference{}, (*PreparedExecEffect)(nil).Reference())
}

func TestPrepareExecEffectRetainsIssuerAfterSignerFailure(t *testing.T) {
	f := newExecEffectFixture(t)
	f.provider.sign = func(context.Context, string, controlprotocol.ExecStartTicketClaims) ([]byte, error) {
		return nil, errors.New("retry signer")
	}
	first, err := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
	require.ErrorContains(t, err, "retry signer")
	require.Nil(t, first.Prepared)
	d := f.cap.execDraft
	claims := d.claims
	issuer := d.issuer.Record
	revision := d.issuer.Revision
	var envelope struct {
		Claims controlprotocol.CommandIssuerCertificateClaims `json:"claims"`
	}
	require.NoError(t, json.Unmarshal(f.provider.wire, &envelope))
	envelope.Claims.CertificateID = uuid.NewString()
	f.provider.wire, err = controlprotocol.SignCommandIssuerCertificate(ed25519.NewKeyFromSeed(make([]byte, 32)), envelope.Claims)
	require.NoError(t, err)
	f.provider.sign = nil
	result, err := f.b.PrepareExecEffect(context.Background(), f.cap, execEffectRequest())
	require.NoError(t, err)
	require.NotNil(t, result.Prepared)
	require.Equal(t, claims, d.claims)
	require.Equal(t, issuer, d.issuer.Record)
	require.Equal(t, revision, d.issuer.Revision)
	require.Equal(t, 1, f.provider.certificateCalls)
}

func TestPrepareExecEffectWindowBounds(t *testing.T) {
	for _, limit := range []string{"issuer before", "issuer after", "business expiry"} {
		t.Run(limit, func(t *testing.T) {
			f := newExecEffectFixture(t)
			var envelope struct {
				Claims controlprotocol.CommandIssuerCertificateClaims `json:"claims"`
			}
			require.NoError(t, json.Unmarshal(f.provider.wire, &envelope))
			switch limit {
			case "issuer before":
				envelope.Claims.NotBefore = f.now.Add(-1500 * time.Millisecond)
			case "issuer after":
				envelope.Claims.NotAfter = f.now.Add(6 * time.Second)
			case "business expiry":
				f.cap.record.ExpiresAt = f.now.Add(5 * time.Second)
			}
			var err error
			f.provider.wire, err = controlprotocol.SignCommandIssuerCertificate(ed25519.NewKeyFromSeed(make([]byte, 32)), envelope.Claims)
			require.NoError(t, err)
			f.provider.identity, err = f.b.execVerifier.VerifyCommandIssuerCertificate(f.provider.wire, f.now)
			require.NoError(t, err)
			d := draftForTest(t, f)
			require.NoError(t, f.b.signExecEffect(context.Background(), f.cap, d))
			switch limit {
			case "issuer before":
				require.Equal(t, f.now.Add(-1500*time.Millisecond), d.claims.NotBefore)
			case "issuer after":
				require.Equal(t, f.now.Add(6*time.Second), d.claims.NotAfter)
			case "business expiry":
				require.Equal(t, f.now.Add(5*time.Second), d.claims.NotAfter)
			}
		})
	}
}
