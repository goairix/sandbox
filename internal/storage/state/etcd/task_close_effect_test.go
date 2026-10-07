package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"reflect"
	"testing"
	"time"
)

type taskCloseProvider struct {
	wire                 []byte
	key                  ed25519.PrivateKey
	identity             controlprotocol.CommandIssuerIdentity
	certCalls, signCalls int
	sign                 func(context.Context, string, controlprotocol.TaskCloseDataTicketClaims) ([]byte, error)
}

func (p *taskCloseProvider) Certificate(ctx context.Context) ([]byte, error) {
	p.certCalls++
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("unbounded certificate")
	}
	return p.wire, ctx.Err()
}
func (p *taskCloseProvider) SignCloseData(ctx context.Context, d string, c controlprotocol.TaskCloseDataTicketClaims) ([]byte, error) {
	p.signCalls++
	if p.sign != nil {
		return p.sign(ctx, d, c)
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("unbounded signer")
	}
	if d != p.identity.Digest() {
		return nil, errors.New("wrong issuer digest")
	}
	return controlprotocol.SignTaskCloseDataTicket(p.key, p.identity, c)
}

type taskCloseFixture struct {
	b   *Backend
	raw *clientv3.Client
	c   *TaskClaim
	p   *taskCloseProvider
	now time.Time
	ctx context.Context
}

func newTaskCloseFixture(t *testing.T, expired bool) *taskCloseFixture {
	t.Helper()
	base, raw, in, keys, ctx := taskDestroyFixture(t)
	if expired {
		r, e := raw.Get(ctx, keys[1])
		require.NoError(t, e)
		var c SandboxControlRecord
		require.NoError(t, decodeDomainRecord(r.Kvs[0], &c))
		c.ExpiresAt = time.Now().UTC().Add(-time.Hour)
		v, e := encodeDomainRecord(c)
		require.NoError(t, e)
		put, e := raw.Put(ctx, keys[1], v)
		require.NoError(t, e)
		in.ExpectedControlRevision = put.Header.Revision
	}
	s, ref := taskDestroyPrepare(t, base, ctx, in)
	out, e := base.CommitStage(ctx, s)
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, out)
	now := time.Now().UTC()
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	key := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789012"))
	wire, e := controlprotocol.SignCommandIssuerCertificate(root, controlprotocol.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: base.namespace.Root(), AuthorityID: base.publicationAuthorityID, Target: base.publicationTarget, RestoreEpoch: base.restoreEpoch, PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, e)
	p := &taskCloseProvider{wire: wire, key: key}
	id, e := decodeIdentity(base.identityValue, base.namespace)
	require.NoError(t, e)
	id.RestoreEpoch = base.restoreEpoch
	clockCalls := 0
	b, e := New(ctx, Options{Endpoints: raw.Endpoints(), Namespace: base.namespace, Identity: id, AllowInsecureLoopback: true, RequestTimeout: 3 * time.Second, TaskIssuer: p, Clock: testAuthorityClock(func(context.Context) (ClockObservation, error) { clockCalls++; return ClockObservation{UTC: now}, nil }), PublicationTrust: &RuntimePublicationTrust{AuthorityID: base.publicationAuthorityID, Target: base.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}})
	require.NoError(t, e)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.Zero(t, clockCalls)
	require.Zero(t, p.certCalls)
	require.Zero(t, p.signCalls)
	p.identity, e = b.taskVerifier.VerifyCommandIssuerCertificate(wire, now)
	require.NoError(t, e)
	c, e := b.ClaimTask(ctx, ref, "close-worker", 30*time.Second)
	require.NoError(t, e)
	t.Cleanup(func() { taskReleaseClaim(t, b, c) })
	return &taskCloseFixture{b, raw, c, p, now, ctx}
}
func TestTaskCloseRejectsBeforeRPC(t *testing.T) {
	b := new(Backend)
	for _, c := range []*TaskClaim{nil, {}} {
		r, e := b.PrepareTaskCloseData(context.Background(), c)
		require.ErrorIs(t, e, ErrInvalidRecord)
		require.Nil(t, r.Prepared)
		require.Equal(t, OutcomeUnknown, r.Outcome)
	}
	var p *PreparedTaskCloseData
	require.Equal(t, TaskCloseDataReference{}, p.Reference())
}
func TestTaskCloseNativeSuccess(t *testing.T) {
	f := newTaskCloseFixture(t, true)
	before := append([]taskFence(nil), f.c.fences...)
	r, e := f.b.PrepareTaskCloseData(f.ctx, f.c)
	require.NoError(t, e)
	require.NoError(t, r.GuardCleanupError)
	require.Equal(t, OutcomeCommitted, r.Outcome)
	require.NotNil(t, r.Prepared)
	require.Equal(t, r.Reference, r.Prepared.Reference())
	require.NoError(t, r.Reference.Validate())
	d := f.c.closeDraft
	require.Equal(t, f.c.birth, d.claims.Context.ControlRevision)
	require.Greater(t, d.claims.Context.ClaimCreateRevision, d.claims.Context.ControlRevision)
	require.NotEqual(t, d.task.ControlRevision, d.claims.Context.ControlRevision)
	require.True(t, d.task.ExpiresAt.Before(f.now))
	require.Equal(t, f.c.Reference().ClaimID, d.claims.Context.ClaimID)
	require.Len(t, d.stage.mutation.Comparisons, 44)
	require.Len(t, d.stage.mutation.Writes, 1)
	require.Equal(t, 59, len(d.stage.mutation.Comparisons)+len(d.stage.mutation.Writes)+stageProtocolOperations)
	deadline, command, ticket, stage := d.deadline, d.commandID, d.ticket.Wire(), d.stage
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
	again, e := f.b.PrepareTaskCloseData(f.ctx, f.c)
	require.NoError(t, e)
	require.NotNil(t, again.Prepared)
	require.Equal(t, r.Reference, again.Reference)
	require.Equal(t, deadline, d.deadline)
	require.Equal(t, command, d.commandID)
	require.Equal(t, ticket, d.ticket.Wire())
	require.Same(t, stage, d.stage)
	require.Equal(t, 1, f.p.signCalls)
	require.Equal(t, 1, f.p.certCalls)
	entry, e := f.b.LoadTaskCloseData(f.ctx, f.c.Reference().Task)
	require.NoError(t, e)
	require.NotNil(t, entry)
	require.Equal(t, r.Reference, entry.Reference)
	require.Equal(t, OutcomeCommitted, entry.Outcome)
	require.Equal(t, d.task, entry.Record.Task)
	original := bytes.Clone(entry.Record.Ticket)
	clear(entry.Record.Ticket)
	copy, e := f.b.LoadTaskCloseData(f.ctx, f.c.Reference().Task)
	require.NoError(t, e)
	require.Equal(t, original, []byte(copy.Record.Ticket))
	digest, err := snapshotDigest(copy.Record.Ticket)
	require.NoError(t, err)
	require.Equal(t, d.ticket.Digest(), digest)
	require.Equal(t, digest, copy.Record.TicketDigest)
	require.Equal(t, entry.Revision, copy.Revision)
	f.b.taskIssuer = nil
	f.b.authorityClock = nil
	f.b.taskVerifier = nil
	history, e := f.b.LoadTaskCloseData(f.ctx, f.c.Reference().Task)
	require.NoError(t, e)
	require.Equal(t, r.Reference, history.Reference)
	for _, v := range before {
		got, e := f.raw.Get(f.ctx, v.key)
		require.NoError(t, e)
		require.Len(t, got.Kvs, 1)
		require.True(t, v.matches(got.Kvs[0]))
	}
}
func TestTaskCloseNativeSigning(t *testing.T) {
	for _, fault := range []string{"transient", "payload", "expired", "cancel", "parent", "copied", "foreign", "deadline", "fence", "uncertainty"} {
		t.Run(fault, func(t *testing.T) {
			f := newTaskCloseFixture(t, false)
			ctx := f.ctx
			if fault == "copied" {
				copy := new(TaskClaim)
				reflect.ValueOf(copy).Elem().Set(reflect.ValueOf(f.c).Elem())
				r, e := f.b.PrepareTaskCloseData(ctx, copy)
				require.ErrorIs(t, e, ErrInvalidRecord)
				require.Nil(t, r.Prepared)
				return
			}
			if fault == "foreign" {
				r, e := new(Backend).PrepareTaskCloseData(ctx, f.c)
				require.ErrorIs(t, e, ErrInvalidRecord)
				require.Nil(t, r.Prepared)
				return
			}
			var first controlprotocol.TaskCloseDataTicketClaims
			call, cancel := context.WithCancel(ctx)
			defer cancel()
			parent, pcancel := context.WithCancel(ctx)
			defer pcancel()
			f.c.parentCtx = parent
			f.p.sign = func(ctx context.Context, d string, c controlprotocol.TaskCloseDataTicketClaims) ([]byte, error) {
				first = c
				switch fault {
				case "transient":
					return nil, errors.New("transient signer")
				case "payload":
					c.Context.CommandID = uuid.NewString()
				case "expired":
					f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
						return ClockObservation{UTC: f.now.Add(time.Minute)}, nil
					})
				case "cancel":
					cancel()
				case "parent":
					pcancel()
				case "fence":
					_, err := f.raw.Put(f.ctx, f.c.fences[5].key, f.c.fences[5].value)
					require.NoError(t, err)
				case "uncertainty":
					f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
						return ClockObservation{UTC: f.now, Uncertainty: 2 * time.Second}, nil
					})
				case "deadline":
					f.c.closeDraft.deadline = time.Now().Add(-time.Second)
				}
				return controlprotocol.SignTaskCloseDataTicket(f.p.key, f.p.identity, c)
			}
			r, e := f.b.PrepareTaskCloseData(call, f.c)
			require.Error(t, e)
			require.Nil(t, r.Prepared)
			d := f.c.closeDraft
			require.NotNil(t, d)
			require.Nil(t, d.stage)
			require.Empty(t, d.ticket.Wire())
			if fault == "transient" {
				deadline, id := d.deadline, d.commandID
				require.NoError(t, f.b.RenewTaskClaim(ctx, f.c))
				f.p.sign = nil
				r, e = f.b.PrepareTaskCloseData(ctx, f.c)
				require.NoError(t, e)
				require.NotNil(t, r.Prepared)
				require.Equal(t, first, d.claims)
				require.Equal(t, deadline, d.deadline)
				require.Equal(t, id, d.commandID)
				require.Equal(t, 1, f.p.certCalls)
				require.Equal(t, 2, f.p.signCalls)
			}
		})
	}
}
