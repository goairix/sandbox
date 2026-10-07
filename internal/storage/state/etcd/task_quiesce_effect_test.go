package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

type quiescenceProvider struct {
	old   *taskCloseProvider
	calls int
	sign  func(context.Context, string, p.TaskUserQuiescenceTicketClaims) ([]byte, error)
}

func (q *quiescenceProvider) Certificate(ctx context.Context) ([]byte, error) {
	return q.old.Certificate(ctx)
}
func (q *quiescenceProvider) SignQuiesceUsers(ctx context.Context, digest string, c p.TaskUserQuiescenceTicketClaims) ([]byte, error) {
	q.calls++
	if q.sign != nil {
		return q.sign(ctx, digest, c)
	}
	if digest != q.old.identity.Digest() {
		return nil, ErrIdentityMismatch
	}
	return p.SignTaskUserQuiescenceTicket(q.old.key, q.old.identity, c)
}

type quiescenceNativeFixture struct {
	*taskCloseFixture
	target *taskDeliveryTarget
	issuer *quiescenceProvider
	old    TaskCloseDataReference
}

func newQuiescenceNativeFixture(t *testing.T) *quiescenceNativeFixture {
	t.Helper()
	return finishQuiescenceFixture(t, newTaskDeliveryFixture(t, true))
}
func finishQuiescenceFixture(t *testing.T, f *taskCloseFixture) *quiescenceNativeFixture {
	t.Helper()
	closed, err := f.b.PrepareTaskCloseData(f.ctx, f.c)
	require.NoError(t, err)
	require.NotNil(t, closed.Prepared)
	target := newTaskDeliveryTarget(t, f, closed.Prepared)
	proof, err := f.b.DeliverTaskCloseData(f.ctx, closed.Prepared, target.destination)
	require.NoError(t, err)
	require.NotNil(t, proof.Receipt)
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.c))
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	identity, err := decodeIdentity(f.b.identityValue, f.b.namespace)
	require.NoError(t, err)
	identity.RestoreEpoch = f.b.restoreEpoch
	wire, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: f.b.namespace.Root(), AuthorityID: f.b.publicationAuthorityID, Target: f.b.publicationTarget, RestoreEpoch: f.b.restoreEpoch, PublicKey: f.p.key.Public().(ed25519.PublicKey), NotBefore: f.now.Add(-time.Minute), NotAfter: f.now.Add(time.Minute)})
	require.NoError(t, err)
	issuerIdentity, err := f.b.taskVerifier.VerifyCommandIssuerCertificate(wire, f.now)
	require.NoError(t, err)
	q := &quiescenceProvider{old: &taskCloseProvider{wire: wire, key: f.p.key, identity: issuerIdentity}}
	b, err := New(f.ctx, Options{Endpoints: f.raw.Endpoints(), Namespace: f.b.namespace, Identity: identity, AllowInsecureLoopback: true, RequestTimeout: 3 * time.Second, TaskQuiescenceIssuer: q, PublicationTrust: &RuntimePublicationTrust{AuthorityID: f.b.publicationAuthorityID, Target: f.b.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, Clock: testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: f.now}, nil })})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	c, err := b.ClaimTask(f.ctx, f.c.Reference().Task, "quiescence-worker", 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { taskReleaseClaim(t, b, c) })
	f.b = b
	f.c = c
	return &quiescenceNativeFixture{f, target, q, closed.Reference}
}
func TestTaskQuiesceMetadataRejectBeforeRPC(t *testing.T) {
	b := new(Backend)
	for _, c := range []*TaskClaim{nil, {}} {
		r, err := b.PrepareTaskQuiescence(context.Background(), c, nil)
		require.ErrorIs(t, err, ErrInvalidRecord)
		require.Nil(t, r.Prepared)
		require.Equal(t, OutcomeUnknown, r.Outcome)
	}
	var prepared *PreparedTaskUserQuiescence
	require.Equal(t, TaskQuiescenceReference{}, prepared.Reference())
}
func quiescenceNativeSuccess(t *testing.T) {
	f := newQuiescenceNativeFixture(t)
	original := append([]taskFence{}, f.c.fences...)
	r, err := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
	require.NoError(t, err)
	require.NoError(t, r.GuardCleanupError)
	require.Equal(t, OutcomeCommitted, r.Outcome)
	require.NotNil(t, r.Prepared)
	require.Equal(t, r.Reference, r.Prepared.Reference())
	require.NoError(t, r.Reference.Validate())
	d := f.c.quiescenceDraft
	require.Greater(t, d.claims.Context.Current.ClaimCreateRevision, d.claims.Context.CloseDataContext.ClaimCreateRevision)
	require.Len(t, d.stage.mutation.Comparisons, 49)
	require.Len(t, d.stage.mutation.Writes, 1)
	require.Equal(t, 64, len(d.stage.mutation.Comparisons)+len(d.stage.mutation.Writes)+stageProtocolOperations)
	require.True(t, d.task.ExpiresAt.Before(f.now))
	deadline, command, ticket, stage := d.deadline, d.commandID, d.ticket.Wire(), d.stage
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
	again, err := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
	require.NoError(t, err)
	require.NotNil(t, again.Prepared)
	require.Equal(t, r.Reference, again.Reference)
	require.Equal(t, deadline, d.deadline)
	require.Equal(t, command, d.commandID)
	require.Equal(t, ticket, d.ticket.Wire())
	require.Same(t, stage, d.stage)
	require.Equal(t, 1, f.issuer.calls)
	entry, err := f.b.LoadTaskQuiescence(f.ctx, f.c.Reference().Task)
	require.NoError(t, err)
	require.Equal(t, r.Reference, entry.Reference)
	require.Equal(t, OutcomeCommitted, entry.Outcome)
	require.Equal(t, f.old.CommandID, entry.Record.Context.CloseDataContext.CommandID)
	owned := bytes.Clone(entry.Record.CloseDataReceipt)
	clear(entry.Record.CloseDataReceipt)
	copy, err := f.b.LoadTaskQuiescence(f.ctx, f.c.Reference().Task)
	require.NoError(t, err)
	require.Equal(t, owned, []byte(copy.Record.CloseDataReceipt))
	require.Equal(t, original, f.c.fences)
}
