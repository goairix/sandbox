package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type quiescenceWireTarget struct {
	destination      *tr.Destination
	dial             func(context.Context) (net.Conn, error)
	effects, queries atomic.Int32
	lose             atomic.Bool
}

// Real native metadata and genuine mTLS/EOF are exercised. The fixed response
// is protocol-created crypto attribution only: no PID1, coordinator, namespace
// absence or durable accepted-work claim is proved by this test handler.
func newQuiescenceWireTarget(t *testing.T, f *quiescenceNativeFixture, prepared *PreparedTaskUserQuiescence, kind byte) *quiescenceWireTarget {
	t.Helper()
	draft := prepared.draft
	c := draft.claims.Context
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	rk := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789013"))
	rw, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, Role: "runtime_receipt", CertificateID: uuid.NewString(), Namespace: c.Current.Namespace, AuthorityID: c.Current.AuthorityID, Target: c.Current.Target, RestoreEpoch: c.Current.RestoreEpoch, PublicKey: rk.Public().(ed25519.PublicKey), NotBefore: f.now.Add(-time.Minute), NotAfter: f.now.Add(time.Hour), SandboxID: c.Current.SandboxID, WorkspaceHash: c.Current.WorkspaceHash, Generation: c.Current.Generation, Runtime: c.Current.Runtime})
	require.NoError(t, err)
	rc, err := p.NewManagementTLSCredential(rk, rw, "runtime_receipt")
	require.NoError(t, err)
	ic, err := p.NewManagementTLSCredential(f.issuer.old.key, draft.certificate, "command_issuer")
	require.NoError(t, err)
	clock := &taskDeliveryClock{}
	clock.now.Store(f.now.UnixNano())
	cfg, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: f.b.taskQuiescenceVerifier, Clock: clock, Credential: rc, PeerCertificate: draft.certificate, Server: true})
	require.NoError(t, err)
	wire := []byte("unknown")
	if kind == tr.EventAccepted {
		wire, err = p.SignTaskUserQuiescenceAccepted(rk, p.TaskUserQuiescenceAcceptedClaims{Version: 1, State: "quiescence_accepted", Context: c, TicketDigest: draft.ticket.Digest(), NotBefore: draft.claims.NotBefore, NotAfter: draft.claims.NotAfter})
	} else if kind == tr.EventReceipt {
		wire, err = p.SignTaskUserQuiescenceReceipt(rk, p.TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: c, TicketDigest: draft.ticket.Digest(), NotBefore: draft.claims.NotBefore, NotAfter: draft.claims.NotAfter, ExecutionSetDigest: strings.Repeat("a", 64)})
	}
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	target := &quiescenceWireTarget{}
	target.dial = func(ctx context.Context) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "tcp", listener.Addr().String())
	}
	target.destination, err = tr.NewDestination(tr.DestinationOptions{Identity: p.RuntimeIdentityContext{SandboxID: c.Current.SandboxID, WorkspaceHash: c.Current.WorkspaceHash, Generation: c.Current.Generation, Runtime: c.Current.Runtime}, RuntimeCertificate: rw, Credential: ic, Verifier: f.b.taskQuiescenceVerifier, Clock: clock, Dial: func(ctx context.Context) (net.Conn, error) { return target.dial(ctx) }})
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(4 * time.Second))
				conn := tls.Server(raw, cfg)
				if conn.Handshake() != nil {
					return
				}
				request, err := tr.ReadRuntimeRequest(conn)
				if err != nil || request.Quiesce == nil {
					return
				}
				e := request.Quiesce
				if e.Context != c || e.TicketDigest != draft.ticket.Digest() {
					return
				}
				if e.Purpose == "task_quiesce_users" {
					target.effects.Add(1)
					ev, err := f.b.taskQuiescenceVerifier.VerifyTaskUserQuiescenceTicket(e.Ticket, e.IssuerCertificate, c, f.now)
					if err != nil || ev.Digest() != e.TicketDigest || !bytes.Equal(e.OriginalCloseDataReceipt, draft.prior.proof) {
						return
					}
					if target.lose.Load() {
						return
					}
				} else {
					target.queries.Add(1)
				}
				_ = tr.WriteEvent(conn, kind, wire)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("wire target unjoined")
		}
	})
	return target
}

func TestTaskQuiesceTransportNativeDeliveryLostACK(t *testing.T) {
	f := newQuiescenceNativeFixture(t)
	r, err := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
	require.NoError(t, err)
	require.NotNil(t, r.Prepared)
	target := newQuiescenceWireTarget(t, f, r.Prepared, tr.EventAccepted)
	target.lose.Store(true)
	got, err := f.b.DeliverTaskQuiescence(f.ctx, r.Prepared, target.destination)
	require.ErrorIs(t, err, ErrDeliveryUnknown)
	require.Equal(t, r.Reference, got.Reference)
	require.Nil(t, got.Accepted)
	target.lose.Store(false)
	got, err = f.b.DeliverTaskQuiescence(f.ctx, r.Prepared, target.destination)
	require.ErrorIs(t, err, ErrDeliveryUnknown)
	require.Equal(t, r.Reference, got.Reference)
	got, err = f.b.QueryTaskQuiescence(f.ctx, r.Reference.Task, target.destination)
	require.NoError(t, err)
	require.NotNil(t, got.Accepted)
	require.Nil(t, got.Receipt)
	require.Equal(t, r.Reference, got.Reference)
	require.EqualValues(t, 1, target.effects.Load())
	require.EqualValues(t, 1, target.queries.Load())
	copied := *r.Prepared
	_, err = f.b.DeliverTaskQuiescence(f.ctx, &copied, target.destination)
	require.ErrorIs(t, err, ErrInvalidRecord)
	_, err = new(Backend).DeliverTaskQuiescence(f.ctx, r.Prepared, target.destination)
	require.ErrorIs(t, err, ErrInvalidRecord)
}

func TestTaskQuiesceTransportNativeHandshakeClaimLoss(t *testing.T) {
	f := newQuiescenceNativeFixture(t)
	r, err := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
	require.NoError(t, err)
	require.NotNil(t, r.Prepared)
	target := newQuiescenceWireTarget(t, f, r.Prepared, tr.EventAccepted)
	original := target.dial
	target.dial = func(ctx context.Context) (net.Conn, error) {
		conn, err := original(ctx)
		if err == nil {
			_, e := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
			require.NoError(t, e)
		}
		return conn, err
	}
	got, err := f.b.DeliverTaskQuiescence(f.ctx, r.Prepared, target.destination)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, got.Accepted)
	require.Nil(t, got.Receipt)
	require.Zero(t, target.effects.Load())
	require.False(t, r.Prepared.draft.deliveryAttempted)
}

func TestTaskQuiesceTransportNativeBeforeBytesClaimLoss(t *testing.T) {
	f := newQuiescenceNativeFixture(t)
	r, err := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
	require.NoError(t, err)
	require.NotNil(t, r.Prepared)
	target := newQuiescenceWireTarget(t, f, r.Prepared, tr.EventAccepted)
	original := f.b.client.KV
	defer func() { f.b.client.KV = original }()
	checks := 0
	f.b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
		if len(ops) == 1 && ops[0].IsGet() && string(ops[0].KeyBytes()) == f.b.identityKey {
			checks++
			return checks == 3
		}
		return false
	}, before: func() {
		_, err := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
		require.NoError(t, err)
	}}
	got, err := f.b.DeliverTaskQuiescence(f.ctx, r.Prepared, target.destination)
	require.ErrorIs(t, err, ErrConflict)
	require.Equal(t, 3, checks)
	require.Nil(t, got.Accepted)
	require.Nil(t, got.Receipt)
	require.Zero(t, target.effects.Load())
	require.False(t, r.Prepared.draft.deliveryAttempted)
}

func TestTaskDualDrainNativeConjunction(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty-local-unknown", true: "terminal-nonempty-then-zero"}[terminal], func(t *testing.T) {
			count := 0
			if terminal {
				count = 1
			}
			base, ops := newQuiescenceOperationFixture(t, count)
			f := finishQuiescenceFixture(t, base)
			r, err := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
			require.NoError(t, err)
			require.NotNil(t, r.Prepared)
			kind := tr.EventAccepted
			if terminal {
				kind = tr.EventReceipt
			}
			target := newQuiescenceWireTarget(t, f, r.Prepared, kind)
			got, err := f.b.PrepareAfterDualDrain(f.ctx, f.c, target.destination)
			require.Error(t, err)
			require.Nil(t, got)
			if !terminal {
				return
			}
			_, err = f.raw.Revoke(f.ctx, clientv3.LeaseID(ops[0].Reference.LeaseID))
			require.NoError(t, err) // original test owner only
			got, err = f.b.PrepareAfterDualDrain(f.ctx, f.c, target.destination)
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Positive(t, got.emptyRevision)
			require.Same(t, f.c, got.claim)
			old := f.c
			require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, old))
			fresh, err := f.b.ClaimTask(f.ctx, old.reference.Task, "new-dual-claim", 30*time.Second)
			require.NoError(t, err)
			defer taskReleaseClaim(t, f.b, fresh)
			got, err = f.b.PrepareAfterDualDrain(f.ctx, fresh, target.destination)
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Same(t, fresh, got.claim)
			refused, err := f.b.PrepareTaskQuiescence(f.ctx, fresh, f.target.destination)
			require.Error(t, err)
			require.Nil(t, refused.Prepared)
		})
	}
}

func TestTaskDualDrainNativeFinalBarrierMutation(t *testing.T) {
	for _, fault := range []string{"receipt", "reservation", "claim", "late-cancel"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			r, err := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
			require.NoError(t, err)
			require.NotNil(t, r.Prepared)
			target := newQuiescenceWireTarget(t, f, r.Prepared, tr.EventReceipt)
			prefix, err := f.b.taskOperationsPrefix(f.c.reference.Task)
			require.NoError(t, err)
			original := f.b.client.KV
			defer func() { f.b.client.KV = original }()
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			fired := false
			f.b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
				return len(ops) == 1 && ops[0].IsGet() && string(ops[0].KeyBytes()) == prefix
			}, before: func() {
				if fired {
					return
				}
				fired = true
				switch fault {
				case "receipt":
					_, key, e := f.b.stageKeys(r.Reference.Stage)
					require.NoError(t, e)
					v, e := f.raw.Get(f.ctx, key)
					require.NoError(t, e)
					_, e = f.raw.Put(f.ctx, key, string(v.Kvs[0].Value))
					require.NoError(t, e)
				case "reservation":
					key, e := f.b.namespace.taskQuiescenceAttemptKey(f.c.reference.Task)
					require.NoError(t, e)
					v, e := f.raw.Get(f.ctx, key)
					require.NoError(t, e)
					_, e = f.raw.Put(f.ctx, key, string(v.Kvs[0].Value))
					require.NoError(t, e)
				case "claim":
					_, e := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
					require.NoError(t, e)
				case "late-cancel":
					cancel()
				}
			}}
			got, err := f.b.PrepareAfterDualDrain(ctx, f.c, target.destination)
			require.True(t, fired, "must reach actual NEW empty Range")
			require.Error(t, err)
			require.Nil(t, got)
		})
	}
}
