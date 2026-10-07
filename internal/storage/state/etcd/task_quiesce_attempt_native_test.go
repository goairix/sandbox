package etcd

import (
	"context"
	"crypto/ed25519"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"testing"
	"time"
)

func quiescenceColdBackend(t *testing.T, f *quiescenceNativeFixture, configured bool) *Backend {
	t.Helper()
	id, e := decodeIdentity(f.b.identityValue, f.b.namespace)
	require.NoError(t, e)
	id.RestoreEpoch = f.b.restoreEpoch
	options := Options{Endpoints: f.raw.Endpoints(), Namespace: f.b.namespace, Identity: id, AllowInsecureLoopback: true, RequestTimeout: 3 * time.Second}
	if configured {
		root := ed25519.NewKeyFromSeed(make([]byte, 32))
		options.TaskQuiescenceIssuer = f.issuer
		options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: f.b.publicationAuthorityID, Target: f.b.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
		options.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: f.now}, nil })
	}
	b, e := New(f.ctx, options)
	require.NoError(t, e)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	return b
}
func TestTaskQuiesceNativeReservationReconcile(t *testing.T) {
	for _, fault := range []string{"reply-loss", "absent-unknown", "malformed-reply", "competing"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			b := f.b
			original := b.client.KV
			defer func() { b.client.KV = original }()
			key, e := b.namespace.taskQuiescenceAttemptKey(f.c.reference.Task)
			require.NoError(t, e)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			writes := 0
			b.client.KV = &quiescenceIssuerRecordingKV{commit: func(cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
				if putsKey(key)(yes) {
					writes++
					require.Equal(t, 49, len(cmps)+len(yes)+len(no))
					require.Equal(t, f.c.comparisons(), cmps[4:44])
					require.Zero(t, f.issuer.old.certCalls)
					require.Zero(t, f.issuer.calls)
					require.Zero(t, lease.grants.Load())
					if fault == "absent-unknown" {
						return nil, context.DeadlineExceeded
					}
					if fault == "competing" {
						d := f.c.quiescenceDraft
						l := d.attemptReference.Attempt
						l.AttemptID = uuid.NewString()
						value, x := encodeTaskQuiescenceAttempt(TaskQuiescenceAttemptRecord{Version: 1, Task: d.task, Claim: f.c.reference, DestroyingRevision: f.c.birth, CommandID: uuid.NewString(), Attempt: l})
						require.NoError(t, x)
						_, x = f.raw.Put(f.ctx, key, value)
						require.NoError(t, x)
					}
				}
				r, e := original.Txn(f.ctx).If(cmps...).Then(yes...).Else(no...).Commit()
				if putsKey(key)(yes) && fault != "competing" {
					require.NoError(t, e)
					require.True(t, r.Succeeded)
					if fault == "malformed-reply" {
						r.Responses = nil
						return r, nil
					}
					return nil, context.DeadlineExceeded
				}
				return r, e
			}}
			first, e := b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
			require.Error(t, e)
			require.Nil(t, first.Prepared)
			require.NoError(t, first.AttemptReference.Validate())
			require.Equal(t, 1, writes)
			require.Zero(t, f.issuer.old.certCalls)
			require.Zero(t, f.issuer.calls)
			require.Zero(t, lease.grants.Load())
			if fault == "competing" {
				require.ErrorIs(t, e, ErrConflict)
				return
			}
			for i := 0; i < 2; i++ {
				retry, e := b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
				require.Equal(t, first.AttemptReference, retry.AttemptReference)
				require.Equal(t, 1, writes)
				if fault == "absent-unknown" {
					require.ErrorIs(t, e, ErrOutcomeUnknown)
					require.Nil(t, retry.Prepared)
					require.Zero(t, lease.grants.Load())
					require.Zero(t, f.issuer.old.certCalls)
					continue
				}
				require.NoError(t, e)
				require.NotNil(t, retry.Prepared)
				require.Equal(t, OutcomeCommitted, retry.Outcome)
				require.Equal(t, first.AttemptReference.Attempt, retry.Prepared.draft.attemptReference.Attempt)
				require.Equal(t, int64(1), lease.grants.Load())
				require.Equal(t, 1, f.issuer.calls)
			}
			cold := quiescenceColdBackend(t, f, false)
			entry, e := cold.LoadTaskQuiescenceAttempt(f.ctx, f.c.reference.Task)
			require.NoError(t, e)
			if fault == "absent-unknown" {
				require.Nil(t, entry)
			} else {
				require.Equal(t, first.AttemptReference, entry.Reference)
				require.Positive(t, entry.Revision)
				entry.Record.CommandID = "mutated"
				again, e := cold.LoadTaskQuiescenceAttempt(f.ctx, f.c.reference.Task)
				require.NoError(t, e)
				require.Equal(t, first.AttemptReference.CommandID, again.Record.CommandID)
			}
		})
	}
}
func TestTaskQuiesceNativeReservationNoReplacement(t *testing.T) {
	for _, state := range []string{"abort", "begin", "commit"} {
		for _, mode := range []string{"new-claim", "cold"} {
			t.Run(state+"/"+mode, func(t *testing.T) {
				f := newQuiescenceNativeFixture(t)
				b := f.b
				original := b.client.KV
				defer func() { b.client.KV = original }()
				key, _ := b.namespace.taskQuiescenceKey(f.c.reference.Task)
				b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
					if state == "begin" {
						return len(ops) == 1 && ops[0].IsPut() && strings.Contains(string(ops[0].KeyBytes()), "/attempts/")
					}
					return putsKey(key)(ops)
				}, before: func() {
					if state == "abort" {
						out, e := b.ResolveStage(f.ctx, f.c.quiescenceDraft.reference.Stage)
						require.NoError(t, e)
						require.Equal(t, OutcomeAborted, out)
					}
				}, after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
					require.NoError(t, e)
					if state == "abort" {
						require.False(t, r.Succeeded)
						return r, nil
					}
					require.True(t, r.Succeeded)
					return nil, context.DeadlineExceeded
				}}
				first, e := b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
				if state == "abort" {
					require.NoError(t, e)
					require.Equal(t, OutcomeAborted, first.Outcome)
				} else {
					require.ErrorIs(t, e, ErrOutcomeUnknown)
				}
				require.Nil(t, first.Prepared)
				b.client.KV = original
				reserved, e := b.LoadTaskQuiescenceAttempt(f.ctx, f.c.reference.Task)
				require.NoError(t, e)
				require.Equal(t, first.AttemptReference, reserved.Reference)
				require.NoError(t, b.ReleaseTaskClaim(f.ctx, f.c))
				if mode == "cold" {
					b = quiescenceColdBackend(t, f, true)
				}
				fresh, e := b.ClaimTask(f.ctx, f.c.reference.Task, "reservation-reconciler", 30*time.Second)
				require.NoError(t, e)
				defer taskReleaseClaim(t, b, fresh)
				beforeIntent, e := f.raw.Get(f.ctx, key)
				require.NoError(t, e)
				certs, signs := f.issuer.old.certCalls, f.issuer.calls
				lease := &creationFaultLease{Lease: b.client.Lease}
				b.client.Lease = lease
				refused, e := b.PrepareTaskQuiescence(f.ctx, fresh, f.target.destination)
				require.ErrorIs(t, e, ErrConflict)
				require.Nil(t, refused.Prepared)
				require.Zero(t, lease.grants.Load())
				require.Zero(t, lease.keeps.Load())
				require.Zero(t, lease.revokes.Load())
				require.Equal(t, certs, f.issuer.old.certCalls)
				require.Equal(t, signs, f.issuer.calls)
				afterIntent, e := f.raw.Get(f.ctx, key)
				require.NoError(t, e)
				require.Equal(t, beforeIntent.Kvs, afterIntent.Kvs)
				again, e := b.LoadTaskQuiescenceAttempt(f.ctx, f.c.reference.Task)
				require.NoError(t, e)
				require.Equal(t, reserved.Reference, again.Reference)
				require.Equal(t, reserved.Revision, again.Revision)
				if state != "commit" {
					history, e := b.LoadTaskQuiescence(f.ctx, f.c.reference.Task)
					require.NoError(t, e)
					require.Nil(t, history)
				}
			})
		}
	}
}
func TestTaskQuiesceNativeReservationFence(t *testing.T) {
	for _, fault := range []string{"same-bytes", "delete", "lease", "recreate", "restore"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			original := f.b.client.KV
			defer func() { f.b.client.KV = original }()
			key, _ := f.b.namespace.taskQuiescenceKey(f.c.reference.Task)
			reservationKey, _ := f.b.namespace.taskQuiescenceAttemptKey(f.c.reference.Task)
			fired := false
			f.b.client.KV = &faultKV{KV: original, match: putsKey(key), before: func() {
				fired = true
				d := f.c.quiescenceDraft
				require.Positive(t, d.reservation.revision)
				r, e := f.raw.Get(f.ctx, reservationKey)
				require.NoError(t, e)
				require.Len(t, r.Kvs, 1)
				require.Equal(t, d.reservation.revision, r.Kvs[0].ModRevision)
				switch fault {
				case "same-bytes":
					_, e = f.raw.Put(f.ctx, reservationKey, string(r.Kvs[0].Value))
				case "delete":
					_, e = f.raw.Delete(f.ctx, reservationKey)
				case "recreate":
					_, e = f.raw.Delete(f.ctx, reservationKey)
					require.NoError(t, e)
					_, e = f.raw.Put(f.ctx, reservationKey, string(r.Kvs[0].Value))
				case "lease":
					l, x := f.raw.Grant(f.ctx, 20)
					require.NoError(t, x)
					t.Cleanup(func() { _, _ = f.raw.Revoke(context.Background(), l.ID) })
					_, e = f.raw.Put(f.ctx, reservationKey, string(r.Kvs[0].Value), clientv3.WithLease(l.ID))
				case "restore":
					_, e = f.raw.Put(f.ctx, f.b.restoreKey, "different")
					deferRestore(t, f)
				}
				require.NoError(t, e)
			}}
			r, e := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
			require.True(t, fired)
			require.Error(t, e)
			require.Nil(t, r.Prepared)
			require.NotEqual(t, OutcomeCommitted, r.Outcome)
			g, e := f.raw.Get(f.ctx, key)
			require.NoError(t, e)
			require.Empty(t, g.Kvs)
		})
	}
}
func TestTaskQuiesceNativeReservationDelayedPacket(t *testing.T) {
	for _, fault := range []string{"revoke", "new-claim"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			original := f.b.client.KV
			defer func() { f.b.client.KV = original }()
			key, _ := f.b.namespace.taskQuiescenceAttemptKey(f.c.reference.Task)
			var savedCmps []clientv3.Cmp
			var savedYes, savedNo []clientv3.Op
			f.b.client.KV = &quiescenceIssuerRecordingKV{commit: func(cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
				if putsKey(key)(yes) {
					require.Nil(t, savedCmps)
					savedCmps = cmps
					savedYes = yes
					savedNo = no
					return nil, context.DeadlineExceeded
				}
				return original.Txn(f.ctx).If(cmps...).Then(yes...).Else(no...).Commit()
			}}
			first, e := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
			require.ErrorIs(t, e, ErrOutcomeUnknown)
			require.NotEmpty(t, savedCmps)
			require.Zero(t, f.issuer.old.certCalls)
			f.b.client.KV = original
			_, e = f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
			require.NoError(t, e)
			var winner TaskQuiescenceAttemptReference
			if fault == "new-claim" {
				c, e := f.b.ClaimTask(f.ctx, f.c.reference.Task, "delayed-reservation-new", 30*time.Second)
				require.NoError(t, e)
				defer taskReleaseClaim(t, f.b, c)
				r, e := f.b.PrepareTaskQuiescence(f.ctx, c, f.target.destination)
				require.NoError(t, e)
				require.NotNil(t, r.Prepared)
				winner = r.AttemptReference
				require.NotEqual(t, first.AttemptReference, winner)
			}
			replay, e := f.raw.Txn(f.ctx).If(savedCmps...).Then(savedYes...).Else(savedNo...).Commit()
			require.NoError(t, e)
			require.False(t, replay.Succeeded)
			entry, e := f.b.LoadTaskQuiescenceAttempt(f.ctx, f.c.reference.Task)
			require.NoError(t, e)
			if fault == "revoke" {
				require.Nil(t, entry)
				require.Zero(t, f.issuer.calls)
			} else {
				require.Equal(t, winner, entry.Reference)
				require.Equal(t, 1, f.issuer.calls)
			}
		})
	}
}
func TestTaskQuiesceNativeReservationHistory(t *testing.T) {
	for _, fault := range []string{"missing", "association", "recreate", "final-rewrite"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			r, e := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
			require.NoError(t, e)
			require.NotNil(t, r.Prepared)
			entry, e := f.b.LoadTaskQuiescence(f.ctx, r.Reference.Task)
			require.NoError(t, e)
			key, _ := f.b.namespace.taskQuiescenceAttemptKey(r.Reference.Task)
			g, e := f.raw.Get(f.ctx, key)
			require.NoError(t, e)
			require.Len(t, g.Kvs, 1)
			mutate := func() {
				var e error
				switch fault {
				case "missing":
					_, e = f.raw.Delete(f.ctx, key)
				case "association":
					var record TaskQuiescenceAttemptRecord
					require.NoError(t, decodeTaskQuiescenceAttempt(g.Kvs[0], &record))
					record.CommandID = uuid.NewString()
					v, x := encodeTaskQuiescenceAttempt(record)
					require.NoError(t, x)
					_, e = f.raw.Delete(f.ctx, key)
					require.NoError(t, e)
					_, e = f.raw.Put(f.ctx, key, v)
				case "recreate":
					_, e = f.raw.Delete(f.ctx, key)
					require.NoError(t, e)
					_, e = f.raw.Put(f.ctx, key, string(g.Kvs[0].Value))
				case "final-rewrite":
					_, e = f.raw.Put(f.ctx, key, string(g.Kvs[0].Value))
				}
				require.NoError(t, e)
			}
			if fault != "final-rewrite" {
				mutate()
				history, e := f.b.LoadTaskQuiescence(f.ctx, r.Reference.Task)
				require.Error(t, e)
				require.Nil(t, history)
				return
			}
			original := f.b.client.KV
			defer func() { f.b.client.KV = original }()
			fired := false
			f.b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool { return len(ops) == 1 && ops[0].IsGet() && len(ops[0].RangeBytes()) > 0 }, before: func() { fired = true; mutate() }}
			rev, empty, e := f.b.observeTaskOperationsEmpty(f.ctx, f.c, entry)
			require.True(t, fired)
			require.ErrorIs(t, e, ErrConflict)
			require.Zero(t, rev)
			require.False(t, empty)
		})
	}
}
func TestTaskQuiesceNativeStageCompatibility(t *testing.T) {
	f := newQuiescenceNativeFixture(t)
	key, e := f.b.namespace.Key("model", "generic-stage")
	require.NoError(t, e)
	m := Mutation{Writes: []Write{{Key: key, Value: []byte("generic")}}}
	a, e := f.b.BeginStage(f.ctx, f.c.reference.Task.Partition, "generic-request", "generic-stage", m, 10*time.Second)
	require.NoError(t, e)
	b, e := f.b.BeginStage(f.ctx, f.c.reference.Task.Partition, "generic-request", "generic-stage", m, 10*time.Second)
	require.NoError(t, e)
	require.NotEqual(t, a.reference.AttemptID, b.reference.AttemptID)
	require.Equal(t, a.reference.Digest, b.reference.Digest)
	require.Nil(t, a.reservation)
	require.Nil(t, b.reservation)
	out, e := f.b.ResolveStage(f.ctx, a.Reference())
	require.NoError(t, e)
	require.Equal(t, OutcomeAborted, out)
	out, e = f.b.CommitStage(f.ctx, a)
	require.NoError(t, e)
	require.Equal(t, OutcomeAborted, out)
	out, e = f.b.CommitStage(f.ctx, b)
	require.NoError(t, e)
	require.Equal(t, OutcomeCommitted, out)
	require.NoError(t, f.b.ReleaseStage(f.ctx, a))
	require.NoError(t, f.b.ReleaseStage(f.ctx, b))
}
