package etcd

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Change only the target response: keep the original prepared object, native
// history, certificate and genuine pinned TLS server. This proves protocol
// attribution, not physical PID1 drain or deployed-runtime substitution.
func finalizationTargetResponse(t *testing.T, f *quiescenceNativeFixture, target *quiescenceWireTarget, kind byte) {
	t.Helper()
	d := f.c.quiescenceDraft
	rk := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789013"))
	wire := []byte("unknown")
	var err error
	if kind == tr.EventAccepted {
		wire, err = p.SignTaskUserQuiescenceAccepted(rk, p.TaskUserQuiescenceAcceptedClaims{Version: 1, State: "quiescence_accepted", Context: d.claims.Context, TicketDigest: d.ticket.Digest(), NotBefore: d.claims.NotBefore, NotAfter: d.claims.NotAfter})
	} else if kind == tr.EventReceipt {
		wire, err = p.SignTaskUserQuiescenceReceipt(rk, p.TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: d.claims.Context, TicketDigest: d.ticket.Digest(), NotBefore: d.claims.NotBefore, NotAfter: d.claims.NotAfter, ExecutionSetDigest: strings.Repeat("b", 64)})
	}
	require.NoError(t, err)
	target.response.Store(&quiescenceWireResponse{kind: kind, wire: wire})
}

type finalizationReadKV struct {
	clientv3.KV
	t             *testing.T
	b             *Backend
	c             *TaskClaim
	deadline      time.Time
	emptyReads    int
	rangeCount    int64
	rangeEntries  int
	before, after func()
}
type finalizationReadTxn struct {
	clientv3.Txn
	k       *finalizationReadKV
	cmps    []clientv3.Cmp
	yes, no []clientv3.Op
}

func (k *finalizationReadKV) Txn(ctx context.Context) clientv3.Txn {
	deadline, ok := ctx.Deadline()
	require.True(k.t, ok)
	require.False(k.t, deadline.After(k.deadline), "every native RPC uses saved deadline ceiling")
	return &finalizationReadTxn{Txn: k.KV.Txn(ctx), k: k}
}
func (x *finalizationReadTxn) If(c ...clientv3.Cmp) clientv3.Txn {
	x.cmps = c
	x.Txn = x.Txn.If(c...)
	return x
}
func (x *finalizationReadTxn) Then(o ...clientv3.Op) clientv3.Txn {
	x.yes = o
	x.Txn = x.Txn.Then(o...)
	return x
}
func (x *finalizationReadTxn) Else(o ...clientv3.Op) clientv3.Txn {
	x.no = o
	x.Txn = x.Txn.Else(o...)
	return x
}
func (x *finalizationReadTxn) Commit() (*clientv3.TxnResponse, error) {
	for _, ops := range [][]clientv3.Op{x.yes, x.no} {
		for _, op := range ops {
			require.True(x.k.t, op.IsGet(), "read-only preflight")
		}
	}
	prefix, err := x.k.b.taskOperationsPrefix(x.k.c.reference.Task)
	require.NoError(x.k.t, err)
	empty := len(x.yes) == 1 && string(x.yes[0].KeyBytes()) == prefix
	if empty {
		x.k.emptyReads++
		require.Len(x.k.t, x.cmps, 53)
		require.Len(x.k.t, x.no, 1)
		for _, ops := range [][]clientv3.Op{x.yes, x.no} {
			require.False(x.k.t, ops[0].IsSerializable())
			require.Equal(x.k.t, prefix, string(ops[0].KeyBytes()))
			require.Equal(x.k.t, clientv3.GetPrefixRangeEnd(prefix), string(ops[0].RangeBytes()))
			require.EqualValues(x.k.t, 1, ops[0].Limit())
		}
		if x.k.before != nil {
			x.k.before()
		}
	}
	r, err := x.Txn.Commit()
	if empty && err == nil && r != nil && len(r.Responses) == 1 && r.Responses[0].GetResponseRange() != nil {
		page := r.Responses[0].GetResponseRange()
		x.k.rangeCount, x.k.rangeEntries = page.Count, len(page.Kvs)
	}
	if empty && x.k.after != nil {
		x.k.after()
	}
	return r, err
}

func finalizationNativePrepared(t *testing.T, f *quiescenceNativeFixture, horizon ...time.Duration) (*PreparedTaskQuiescence, *quiescenceWireTarget) {
	t.Helper()
	r, err := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
	require.NoError(t, err)
	require.NotNil(t, r.Prepared)
	target := newQuiescenceWireTarget(t, f, r.Prepared, tr.EventReceipt)
	if len(horizon) != 0 {
		// Conservatively shorten only the claim's local deadline before the API
		// creates its original prerequisite. The original native lease is unchanged.
		f.c.mu.Lock()
		f.c.deadline = time.Now().Add(horizon[0])
		f.c.mu.Unlock()
	}
	prepared, err := f.b.PrepareAfterDualDrain(f.ctx, f.c, target.destination)
	require.NoError(t, err)
	require.NotNil(t, prepared)
	return prepared, target
}
func finalizationCapture(t *testing.T, f *quiescenceNativeFixture, prepared *PreparedTaskQuiescence) *finalizationReadKV {
	t.Helper()
	original := f.b.client.KV
	k := &finalizationReadKV{KV: original, t: t, b: f.b, c: f.c, deadline: prepared.deadline}
	f.b.client.KV = k
	t.Cleanup(func() { f.b.client.KV = original })
	return k
}

func TestTaskFinalizationNativeFreshReadOnly(t *testing.T) {
	// Reusing saved emptiness or issuing effects would fail these native assertions.
	f := newQuiescenceNativeFixture(t)
	prepared, target := finalizationNativePrepared(t, f)
	before, err := f.raw.Get(f.ctx, f.b.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	saved := *prepared
	queries, calls, certCalls := target.queries.Load(), f.issuer.calls, f.issuer.old.certCalls
	capture := finalizationCapture(t, f, prepared)
	lease := &creationFaultLease{Lease: f.b.client.Lease}
	f.b.client.Lease = lease
	t.Cleanup(func() { f.b.client.Lease = lease.Lease })
	require.NoError(t, f.b.RevalidateTaskQuiescence(f.ctx, f.c, prepared, target.destination))
	require.EqualValues(t, 2, target.queries.Load()-queries)
	require.Equal(t, 1, capture.emptyReads)
	require.Zero(t, target.effects.Load())
	require.Equal(t, calls, f.issuer.calls)
	require.Equal(t, certCalls, f.issuer.old.certCalls)
	require.Zero(t, lease.grants.Load())
	require.Zero(t, lease.keeps.Load())
	require.Zero(t, lease.revokes.Load())
	require.Equal(t, saved, *prepared)
	after, err := f.raw.Get(f.ctx, f.b.namespace.Root(), clientv3.WithPrefix())
	require.NoError(t, err)
	require.Equal(t, before.Kvs, after.Kvs, "native values/create/mod/leases unchanged")
	require.Equal(t, before.Header.Revision, after.Header.Revision, "fresh Range may retain same native revision")
}

func TestTaskFinalizationNativeNewOperation(t *testing.T) {
	base, operations := newQuiescenceOperationFixture(t, 1)
	f := finishQuiescenceFixture(t, base)
	op := operations[0].Capability
	_, err := f.raw.Delete(f.ctx, op.tokenKey)
	require.NoError(t, err) // test operator keeps original real lease alive
	prepared, target := finalizationNativePrepared(t, f)
	_, err = f.raw.Put(f.ctx, op.tokenKey, op.value, clientv3.WithLease(clientv3.LeaseID(op.record.Reference.LeaseID)))
	require.NoError(t, err)
	injected, err := f.raw.Get(f.ctx, op.tokenKey)
	require.NoError(t, err)
	require.Len(t, injected.Kvs, 1)
	var record OperationRecord
	require.NoError(t, decodeOperationRecord(injected.Kvs[0], &record))
	require.Equal(t, op.record, record)
	require.Equal(t, op.record.Reference.LeaseID, injected.Kvs[0].Lease)
	// Valid typed wire/original lease, but an operator re-Put has a new birth
	// after destroy. Production closed admission cannot admit this operation;
	// retain the decoder's native birth rejection instead of faking old birth.
	require.GreaterOrEqual(t, injected.Kvs[0].CreateRevision, f.c.birth)
	queries := target.queries.Load()
	capture := finalizationCapture(t, f, prepared)
	require.ErrorIs(t, f.b.RevalidateTaskQuiescence(f.ctx, f.c, prepared, target.destination), ErrCorruptRecord)
	require.Equal(t, 1, capture.emptyReads)
	require.EqualValues(t, 1, capture.rangeCount)
	require.Equal(t, 1, capture.rangeEntries)
	t.Logf("fresh full-prefix limit1 native Range count=%d entries=%d injected_create=%d frozen_destroy_birth=%d result=ErrCorruptRecord", capture.rangeCount, capture.rangeEntries, injected.Kvs[0].CreateRevision, f.c.birth)
	require.EqualValues(t, 1, target.queries.Load()-queries)
	require.Zero(t, target.effects.Load())
}

func finalizationSameBytes(t *testing.T, f *quiescenceNativeFixture, key string) {
	t.Helper()
	r, err := f.raw.Get(f.ctx, key)
	require.NoError(t, err)
	require.Len(t, r.Kvs, 1)
	w, err := f.raw.Put(f.ctx, key, string(r.Kvs[0].Value))
	require.NoError(t, err)
	require.Greater(t, w.Header.Revision, r.Kvs[0].ModRevision)
}

func TestTaskFinalizationNativePinMutation(t *testing.T) {
	for _, timing := range []string{"before-query", "at-range", "after-range"} {
		for _, fault := range []string{"receipt", "reservation"} {
			t.Run(timing+"/"+fault, func(t *testing.T) {
				f := newQuiescenceNativeFixture(t)
				prepared, target := finalizationNativePrepared(t, f)
				queries := target.queries.Load()
				key := prepared.pin.receipt.key
				if fault == "reservation" {
					key = prepared.pin.reservation.key
				}
				mutate := func() { finalizationSameBytes(t, f, key) }
				capture := finalizationCapture(t, f, prepared)
				if timing == "before-query" {
					mutate()
				}
				if timing == "at-range" {
					capture.before = mutate
				}
				if timing == "after-range" {
					capture.after = mutate
				}
				require.Error(t, f.b.RevalidateTaskQuiescence(f.ctx, f.c, prepared, target.destination))
				if timing == "before-query" {
					require.Equal(t, queries, target.queries.Load())
					require.Zero(t, capture.emptyReads)
				} else {
					require.Equal(t, 1, capture.emptyReads)
				}
				require.Zero(t, target.effects.Load())
			})
		}
	}
}

func TestTaskFinalizationNativeRangeCancellation(t *testing.T) {
	for _, fault := range []string{"claim", "cancel", "saved-deadline"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			var horizon []time.Duration
			if fault == "saved-deadline" {
				horizon = []time.Duration{time.Second}
			}
			prepared, target := finalizationNativePrepared(t, f, horizon...)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			if fault == "saved-deadline" {
				require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
				require.True(t, f.c.deadline.After(prepared.deadline))
			}
			capture := finalizationCapture(t, f, prepared)
			capture.before = func() {
				switch fault {
				case "claim":
					_, err := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
					require.NoError(t, err)
				case "cancel":
					cancel()
				case "saved-deadline":
					<-time.After(time.Until(prepared.deadline) + time.Millisecond)
				}
			}
			require.Error(t, f.b.RevalidateTaskQuiescence(ctx, f.c, prepared, target.destination))
			require.Equal(t, 1, capture.emptyReads)
			require.True(t, f.c.lost, "in-call cancellation follows existing conservative live policy")
			if fault == "saved-deadline" {
				require.True(t, time.Now().Before(f.c.deadline), "saved deadline expired while renewed claim deadline remains live")
			}
			require.Zero(t, target.effects.Load())
		})
	}
}

func TestTaskFinalizationNativeCurrentTarget(t *testing.T) {
	for _, fault := range []string{"unavailable", "accepted", "changed", "unknown", "changed-after-range"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			prepared, target := finalizationNativePrepared(t, f)
			capture := finalizationCapture(t, f, prepared)
			switch fault {
			case "unavailable":
				target.dial = func(context.Context) (net.Conn, error) { return nil, errors.New("finalization target unavailable") }
			case "accepted":
				finalizationTargetResponse(t, f, target, tr.EventAccepted)
			case "changed":
				finalizationTargetResponse(t, f, target, tr.EventReceipt)
			case "unknown":
				finalizationTargetResponse(t, f, target, tr.EventError)
			case "changed-after-range":
				capture.after = func() { finalizationTargetResponse(t, f, target, tr.EventReceipt) }
			}
			if fault == "changed" {
				q, err := f.b.QueryTaskQuiescence(f.ctx, f.c.reference.Task, target.destination)
				require.NoError(t, err, "changed receipt remains valid under current runtime certificate")
				require.NotNil(t, q.Receipt)
				require.NotEqual(t, prepared.receipt.Wire(), q.Receipt.Wire())
			}
			err := f.b.RevalidateTaskQuiescence(f.ctx, f.c, prepared, target.destination)
			if fault == "changed" || fault == "changed-after-range" {
				require.ErrorIs(t, err, ErrConflict)
			} else {
				require.Error(t, err)
			}
			if fault == "changed-after-range" {
				require.Equal(t, 1, capture.emptyReads)
			} else {
				require.Zero(t, capture.emptyReads)
			}
			require.Zero(t, target.effects.Load())
		})
	}
}

func TestTaskFinalizationNativeExpiredSavedDeadlineRenewedClaim(t *testing.T) {
	f := newQuiescenceNativeFixture(t)
	prepared, target := finalizationNativePrepared(t, f, time.Second)
	saved := prepared.deadline
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
	require.True(t, f.c.deadline.After(saved))
	<-time.After(time.Until(saved) + time.Millisecond)
	queries := target.queries.Load()
	capture := finalizationCapture(t, f, prepared)
	require.Error(t, f.b.RevalidateTaskQuiescence(f.ctx, f.c, prepared, target.destination))
	require.Equal(t, queries, target.queries.Load())
	require.Zero(t, capture.emptyReads)
	require.False(t, f.c.lost)
	require.Equal(t, saved, prepared.deadline)
	f.b.client.KV = capture.KV
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c), "expired prerequisite must not poison renewed original claim")
}
