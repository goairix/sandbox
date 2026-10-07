package etcd

import (
	"context"
	"crypto/sha256"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"testing"
	"time"
)

// Retain the real caller context, native transaction and fixture TLS target.
// Only the selected native write response is intercepted for response-loss.
type finalSyncRecordingKV struct {
	clientv3.KV
	commit func(context.Context, []clientv3.Cmp, []clientv3.Op, []clientv3.Op) (*clientv3.TxnResponse, error)
}
type finalSyncRecordingTxn struct {
	clientv3.Txn
	ctx     context.Context
	k       *finalSyncRecordingKV
	cmps    []clientv3.Cmp
	yes, no []clientv3.Op
}

func (k *finalSyncRecordingKV) Txn(ctx context.Context) clientv3.Txn {
	return &finalSyncRecordingTxn{Txn: k.KV.Txn(ctx), ctx: ctx, k: k}
}
func (x *finalSyncRecordingTxn) If(c ...clientv3.Cmp) clientv3.Txn  { x.cmps = c; return x }
func (x *finalSyncRecordingTxn) Then(o ...clientv3.Op) clientv3.Txn { x.yes = o; return x }
func (x *finalSyncRecordingTxn) Else(o ...clientv3.Op) clientv3.Txn { x.no = o; return x }
func (x *finalSyncRecordingTxn) Commit() (*clientv3.TxnResponse, error) {
	return x.k.commit(x.ctx, x.cmps, x.yes, x.no)
}

func finalSyncPlan() TaskFinalSyncPlan { return TaskFinalSyncPlan{"sync", strings.Repeat("c", 64)} }
func finalSyncCapture(t *testing.T, f *quiescenceNativeFixture, p *PreparedTaskQuiescence, before func(), after func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error)) *int {
	t.Helper()
	original := f.b.client.KV
	writes := new(int)
	key, err := f.b.namespace.taskFinalSyncAttemptKey(f.c.reference.Task)
	require.NoError(t, err)
	emptyReads := 0
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if len(yes) == 1 && yes[0].IsGet() && len(yes[0].RangeBytes()) > 0 {
			emptyReads++
			require.Len(t, cmps, 53)
			require.EqualValues(t, 1, yes[0].Limit())
			require.False(t, yes[0].IsSerializable())
			require.Len(t, no, 1)
		}
		writing := putsKey(key)(yes)
		if writing {
			*writes++
			require.Equal(t, 1, emptyReads, "first write has one fresh whole-prefix observation")
			deadline, ok := ctx.Deadline()
			require.True(t, ok)
			require.False(t, deadline.After(p.deadline))
			require.Len(t, cmps, 54)
			require.Equal(t, 58, len(cmps)+len(yes)+len(no))
			require.Equal(t, taskEmptyComparisons(f.b, f.c, p.pin.intent, p.pin.receipt, p.pin.reservation), cmps[:53])
			require.Equal(t, clientv3.Compare(clientv3.CreateRevision(key), "=", 0), cmps[53])
			require.Len(t, yes, 1)
			require.True(t, yes[0].IsPut())
			require.Len(t, no, 3)
			require.Equal(t, key, string(no[2].KeyBytes()))
			require.True(t, f.c.finalSyncDraft.writeAttempted)
			keys := []string{f.b.identityKey, f.b.restoreKey, key}
			wire := f.c.finalSyncDraft.value
			write := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: []*pb.RequestOp{taskProtoPut(key, wire, 0)}, Failure: taskProtoPoints(keys)}
			read := &pb.TxnRequest{Compare: taskProtoComparisons(cmps[:53]), Success: taskProtoPoints(keys), Failure: taskProtoPoints(keys)}
			reply := taskProtoResponse(keys, []string{f.b.identityValue, f.b.restoreEpoch, wire})
			require.NoError(t, taskCheckWire(f.b.namespace, []*pb.TxnRequest{write, read}, []*pb.TxnResponse{taskProtoPutResponse(1), reply}))
			t.Logf("native final-sync write nodes=58 read nodes=59 write_bytes=%d read_bytes=%d conservative_read_reply_bytes=%d wire_bytes=%d wire_sha256=%x", write.Size(), read.Size(), reply.Size(), len(wire), sha256.Sum256([]byte(wire)))
			if before != nil {
				before()
			}
		}
		r, e := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		if writing && after != nil {
			return after(r, e)
		}
		return r, e
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	return writes
}

func TestTaskFinalSyncNativePermanentOriginal(t *testing.T) {
	f := newQuiescenceNativeFixture(t)
	p, target := finalizationNativePrepared(t, f)
	queries, signs, certs := target.queries.Load(), f.issuer.calls, f.issuer.old.certCalls
	lease := &creationFaultLease{Lease: f.b.client.Lease}
	f.b.client.Lease = lease
	t.Cleanup(func() { f.b.client.Lease = lease.Lease })
	writes := finalSyncCapture(t, f, p, nil, nil)
	first, err := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, first.Outcome)
	require.NoError(t, first.Reference.Validate())
	require.EqualValues(t, 2, target.queries.Load()-queries)
	for i := 0; i < 2; i++ {
		again, err := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
		require.NoError(t, err)
		require.Equal(t, first, again)
	}
	require.Equal(t, 1, *writes)
	require.EqualValues(t, 2, target.queries.Load()-queries)
	entry, err := f.b.LoadTaskFinalSyncAttempt(f.ctx, f.c.reference.Task)
	require.NoError(t, err)
	require.Equal(t, first.Reference, entry.Reference)
	require.Greater(t, entry.Revision, f.c.reference.CreateRevision)
	require.Equal(t, p.pin.intent.create, entry.Record.QuiescenceRevision)
	require.Equal(t, p.pin.reference, entry.Record.Quiescence)
	require.Equal(t, digestTaskQuiescenceProof(p.receipt.Wire()), entry.Record.ReceiptDigest)
	key, err := f.b.namespace.taskFinalSyncAttemptKey(f.c.reference.Task)
	require.NoError(t, err)
	native, err := f.raw.Get(f.ctx, key)
	require.NoError(t, err)
	require.Len(t, native.Kvs, 1)
	require.Zero(t, native.Kvs[0].Lease)
	require.Equal(t, native.Kvs[0].CreateRevision, native.Kvs[0].ModRevision)
	require.EqualValues(t, 1, native.Kvs[0].Version)
	changed := finalSyncPlan()
	changed.Mode = "none"
	_, err = f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, changed)
	require.ErrorIs(t, err, ErrConflict)
	require.Equal(t, 1, *writes)
	freshPrepared, err := f.b.PrepareAfterDualDrain(f.ctx, f.c, target.destination)
	require.NoError(t, err)
	_, err = f.b.ReserveTaskFinalSync(f.ctx, f.c, freshPrepared, target.destination, finalSyncPlan())
	require.ErrorIs(t, err, ErrConflict)
	cold := quiescenceColdBackend(t, f, false)
	history, err := cold.LoadTaskFinalSyncAttempt(f.ctx, f.c.reference.Task)
	require.NoError(t, err)
	require.Equal(t, entry, history)
	require.Zero(t, target.effects.Load())
	require.Equal(t, signs, f.issuer.calls)
	require.Equal(t, certs, f.issuer.old.certCalls)
	require.Zero(t, lease.grants.Load())
	require.Zero(t, lease.keeps.Load())
	require.Zero(t, lease.revokes.Load())
}

func TestTaskFinalSyncNativeResponseLoss(t *testing.T) {
	for _, fault := range []string{"committed", "absent", "malformed"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			p, target := finalizationNativePrepared(t, f)
			original := f.b.client.KV
			key, err := f.b.namespace.taskFinalSyncAttemptKey(f.c.reference.Task)
			require.NoError(t, err)
			writes := 0
			f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
				writing := putsKey(key)(yes)
				if writing {
					writes++
					require.Equal(t, 58, len(cmps)+len(yes)+len(no))
					if fault == "absent" {
						return nil, context.DeadlineExceeded
					}
				}
				r, e := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
				if writing {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					if fault == "malformed" {
						r.Responses = nil
						return r, nil
					}
					return nil, context.DeadlineExceeded
				}
				return r, e
			}}
			t.Cleanup(func() { f.b.client.KV = original })
			first, err := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
			require.ErrorIs(t, err, ErrOutcomeUnknown)
			require.Equal(t, OutcomeUnknown, first.Outcome)
			require.NoError(t, first.Reference.Validate())
			for i := 0; i < 2; i++ {
				again, err := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
				require.Equal(t, first.Reference, again.Reference)
				if fault == "absent" {
					require.ErrorIs(t, err, ErrOutcomeUnknown)
					require.Equal(t, OutcomeUnknown, again.Outcome)
				} else {
					require.NoError(t, err)
					require.Equal(t, OutcomeCommitted, again.Outcome)
				}
			}
			require.Equal(t, 1, writes)
			history, err := f.b.LoadTaskFinalSyncAttempt(f.ctx, f.c.reference.Task)
			require.NoError(t, err)
			if fault == "absent" {
				require.Nil(t, history)
			} else {
				require.Equal(t, first.Reference, history.Reference)
			}
			require.Zero(t, target.effects.Load())
		})
	}
}

func TestTaskFinalSyncNativeFences(t *testing.T) {
	for _, fault := range []string{"receipt", "quiescence-intent", "quiescence-reservation", "control", "restore", "claim", "competing"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			p, target := finalizationNativePrepared(t, f)
			key, err := f.b.namespace.taskFinalSyncAttemptKey(f.c.reference.Task)
			require.NoError(t, err)
			writes := finalSyncCapture(t, f, p, func() {
				switch fault {
				case "receipt":
					finalizationSameBytes(t, f, p.pin.receipt.key)
				case "quiescence-intent":
					finalizationSameBytes(t, f, p.pin.intent.key)
				case "quiescence-reservation":
					finalizationSameBytes(t, f, p.pin.reservation.key)
				case "control":
					finalizationSameBytes(t, f, f.c.fences[4].key)
				case "restore":
					_, e := f.raw.Put(f.ctx, f.b.restoreKey, uuid.NewString())
					require.NoError(t, e)
				case "claim":
					_, e := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
					require.NoError(t, e)
				case "competing":
					r := f.c.finalSyncDraft.record
					r.AttemptID = uuid.NewString()
					w, e := encodeTaskFinalSyncAttempt(r)
					require.NoError(t, e)
					_, e = f.raw.Put(f.ctx, key, w)
					require.NoError(t, e)
				}
			}, nil)
			r, err := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
			require.Error(t, err)
			require.NotEqual(t, OutcomeCommitted, r.Outcome)
			require.Equal(t, 1, *writes)
			require.Zero(t, target.effects.Load())
			if fault != "competing" {
				native, e := f.raw.Get(f.ctx, key)
				require.NoError(t, e)
				require.Empty(t, native.Kvs)
			}
		})
	}
}

func TestTaskFinalSyncNativeKnownRevisionABA(t *testing.T) {
	for _, fault := range []string{"delete", "recreate", "same-bytes-put"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			p, target := finalizationNativePrepared(t, f)
			writes := finalSyncCapture(t, f, p, nil, nil)
			first, err := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
			require.NoError(t, err)
			key, err := f.b.namespace.taskFinalSyncAttemptKey(f.c.reference.Task)
			require.NoError(t, err)
			v, err := f.raw.Get(f.ctx, key)
			require.NoError(t, err)
			_, err = f.raw.Delete(f.ctx, key)
			require.NoError(t, err)
			if fault != "delete" {
				_, err = f.raw.Put(f.ctx, key, string(v.Kvs[0].Value))
				require.NoError(t, err)
				if fault == "same-bytes-put" {
					_, err = f.raw.Put(f.ctx, key, string(v.Kvs[0].Value))
					require.NoError(t, err)
				}
			}
			again, err := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
			require.Error(t, err)
			require.Equal(t, first.Reference, again.Reference)
			require.NotEqual(t, OutcomeCommitted, again.Outcome)
			require.Equal(t, 1, *writes)
		})
	}
}

func TestTaskFinalSyncNativeHistoricalQuiescenceNewClaim(t *testing.T) {
	f := newQuiescenceNativeFixture(t)
	oldPrepared, target := finalizationNativePrepared(t, f)
	old := f.c
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, old))
	fresh, err := f.b.ClaimTask(f.ctx, old.reference.Task, "historical-worker", 30*time.Second)
	require.NoError(t, err)
	defer taskReleaseClaim(t, f.b, fresh)
	p, err := f.b.PrepareAfterDualDrain(f.ctx, fresh, target.destination)
	require.NoError(t, err)
	require.Equal(t, oldPrepared.pin.reference, p.pin.reference)
	require.Less(t, p.pin.intent.create, fresh.reference.CreateRevision)
	r, err := f.b.ReserveTaskFinalSync(f.ctx, fresh, p, target.destination, finalSyncPlan())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, r.Outcome)
	entry, err := f.b.LoadTaskFinalSyncAttempt(f.ctx, fresh.reference.Task)
	require.NoError(t, err)
	require.Equal(t, fresh.reference, entry.Record.Claim)
	require.Equal(t, old.reference.ClaimID, entry.Record.Quiescence.Stage.RequestID)
	require.NotEqual(t, fresh.reference.ClaimID, entry.Record.Quiescence.Stage.RequestID)
}

func TestTaskFinalSyncNativeLoadStructuralLinkage(t *testing.T) {
	for _, fault := range []string{"historical-reference", "task", "destroy", "newer-history-claim"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			p, target := finalizationNativePrepared(t, f)
			_, err := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
			require.NoError(t, err)
			r := f.c.finalSyncDraft.record
			switch fault {
			case "historical-reference":
				r.Quiescence.CommandID = uuid.NewString()
			case "task":
				r.Task.ExpiresAt = r.Task.ExpiresAt.Add(time.Second)
			case "destroy":
				r.DestroyingRevision++
			case "newer-history-claim":
				r.Claim.CreateRevision--
			}
			wire, err := encodeTaskFinalSyncAttempt(r)
			require.NoError(t, err)
			key, err := f.b.namespace.taskFinalSyncAttemptKey(r.Task.Reference)
			require.NoError(t, err)
			_, err = f.raw.Delete(f.ctx, key)
			require.NoError(t, err)
			_, err = f.raw.Put(f.ctx, key, wire)
			require.NoError(t, err)
			_, err = f.b.LoadTaskFinalSyncAttempt(f.ctx, r.Task.Reference)
			require.ErrorIs(t, err, ErrCorruptRecord)
		})
	}
}

func TestTaskFinalSyncNativeCommittedLostOriginalDiagnostics(t *testing.T) {
	for _, fault := range []string{"revoked", "saved-deadline"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			var horizon []time.Duration
			if fault == "saved-deadline" {
				horizon = []time.Duration{2 * time.Second}
			}
			p, target := finalizationNativePrepared(t, f, horizon...)
			writes := finalSyncCapture(t, f, p, nil, nil)
			first, err := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
			require.NoError(t, err)
			if fault == "revoked" {
				_, err = f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
				require.NoError(t, err)
			} else {
				require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
				<-time.After(time.Until(p.deadline) + time.Millisecond)
			}
			again, err := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
			require.Error(t, err)
			require.Equal(t, first, again)
			require.Equal(t, 1, *writes)
			require.Zero(t, target.effects.Load())
		})
	}
}

func TestTaskFinalSyncNativeNoReplacement(t *testing.T) {
	f := newQuiescenceNativeFixture(t)
	p, target := finalizationNativePrepared(t, f)
	first, err := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
	require.NoError(t, err)
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.c))
	fresh, err := f.b.ClaimTask(f.ctx, f.c.reference.Task, "next-worker", 30*time.Second)
	require.NoError(t, err)
	defer taskReleaseClaim(t, f.b, fresh)
	nextPrepared, err := f.b.PrepareAfterDualDrain(f.ctx, fresh, target.destination)
	require.NoError(t, err)
	r, err := f.b.ReserveTaskFinalSync(f.ctx, fresh, nextPrepared, target.destination, finalSyncPlan())
	require.ErrorIs(t, err, ErrConflict)
	require.NotEqual(t, OutcomeCommitted, r.Outcome)
	entry, err := f.b.LoadTaskFinalSyncAttempt(f.ctx, fresh.reference.Task)
	require.NoError(t, err)
	require.Equal(t, first.Reference, entry.Reference)
}

func TestTaskFinalSyncNativeCancellationAndDeadline(t *testing.T) {
	for _, fault := range []string{"cancel", "expired", "revoked", "commit-cancel"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			var horizon []time.Duration
			if fault == "expired" {
				horizon = []time.Duration{time.Second}
			}
			p, target := finalizationNativePrepared(t, f, horizon...)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			if fault == "cancel" {
				cancel()
			}
			if fault == "expired" {
				require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
				<-time.After(time.Until(p.deadline) + time.Millisecond)
			}
			if fault == "revoked" {
				_, e := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
				require.NoError(t, e)
			}
			var after func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error)
			if fault == "commit-cancel" {
				after = func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) { cancel(); return r, e }
			}
			writes := finalSyncCapture(t, f, p, nil, after)
			r, err := f.b.ReserveTaskFinalSync(ctx, f.c, p, target.destination, finalSyncPlan())
			require.Error(t, err)
			if fault == "commit-cancel" {
				require.Equal(t, OutcomeCommitted, r.Outcome)
				require.Equal(t, 1, *writes)
				diagnostic, e := f.b.ReserveTaskFinalSync(f.ctx, f.c, p, target.destination, finalSyncPlan())
				require.Equal(t, OutcomeCommitted, diagnostic.Outcome)
				require.Error(t, e)
				require.Equal(t, 1, *writes)
			} else {
				require.NotEqual(t, OutcomeCommitted, r.Outcome)
				require.Zero(t, *writes)
			}
		})
	}
}
