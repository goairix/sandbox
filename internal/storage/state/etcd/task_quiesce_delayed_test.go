package etcd

import (
	"context"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"testing"
	"time"
)

func TestTaskQuiesceNativeDelayedClaim(t *testing.T) {
	for _, fault := range []string{"revoke", "new-claim", "expiry"} {
		t.Run(fault, func(t *testing.T) {
			f := newQuiescenceNativeFixture(t)
			if fault == "expiry" {
				require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.c))
				c, e := f.b.ClaimTask(f.ctx, f.c.reference.Task, "short-worker", 4*time.Second)
				require.NoError(t, e)
				f.c = c
				t.Cleanup(func() { taskReleaseClaim(t, f.b, c) })
			}
			original := f.b.client.KV
			defer func() { f.b.client.KV = original }()
			key, e := f.b.namespace.taskQuiescenceKey(f.c.reference.Task)
			require.NoError(t, e)
			var cmps []clientv3.Cmp
			var ops []clientv3.Op
			var fresh *TaskClaim
			// Interception retains actual original comparisons/writes. The replay uses a
			// fresh bounded IO context only; it cannot change any authority bytes/Lease.
			f.b.client.KV = &publicationCaptureKV{KV: original, capture: func(c []clientv3.Cmp, o []clientv3.Op) {
				if !putsKey(key)(o) {
					return
				}
				require.Nil(t, cmps)
				cmps = c
				ops = o
				d := f.c.quiescenceDraft
				require.Equal(t, d.attemptReference.Attempt.reference(d.reference.Stage.Digest), d.stage.Reference())
				require.Contains(t, c, d.reservation.comparison())
				require.Len(t, c, 58)
				if fault == "expiry" {
					stage := f.c.quiescenceDraft.stage
					require.NotNil(t, stage)
					// Keep only the original Stage guard Lease alive so its loss cannot mask
					// refusal by the original task claim/guard comparisons after natural expiry.
					stop := time.Now().Add(6 * time.Second)
					for {
						ctx, cancel := context.WithTimeout(context.Background(), time.Second)
						_, e := f.raw.KeepAliveOnce(ctx, stage.leaseID)
						cancel()
						require.NoError(t, e)
						ctx, cancel = context.WithTimeout(context.Background(), time.Second)
						ttl, e := f.raw.TimeToLive(ctx, clientv3.LeaseID(f.c.reference.LeaseID))
						cancel()
						require.NoError(t, e)
						if ttl.TTL < 0 {
							break
						}
						require.True(t, time.Now().Before(stop))
						time.Sleep(100 * time.Millisecond)
					}
				} else {
					_, e := f.raw.Revoke(f.ctx, clientv3.LeaseID(f.c.reference.LeaseID))
					require.NoError(t, e)
				}
				if fault == "new-claim" {
					var e error
					fresh, e = f.b.ClaimTask(f.ctx, f.c.reference.Task, "replacement-worker", 30*time.Second)
					require.NoError(t, e)
					t.Cleanup(func() { taskReleaseClaim(t, f.b, fresh) })
				}
			}}
			result, e := f.b.PrepareTaskQuiescence(f.ctx, f.c, f.target.destination)
			require.Error(t, e)
			require.Nil(t, result.Prepared)
			require.NotEmpty(t, cmps)
			require.NotEmpty(t, ops)
			f.b.client.KV = original
			stage := f.c.quiescenceDraft.stage
			require.NotNil(t, stage)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			guard, e := f.raw.Get(ctx, stage.guardKey)
			require.NoError(t, e)
			require.Len(t, guard.Kvs, 1)
			require.Equal(t, int64(stage.leaseID), guard.Kvs[0].Lease)
			replay, e := f.raw.Txn(ctx).If(cmps...).Then(ops...).Else(clientv3.OpGet(key)).Commit()
			require.NoError(t, e)
			require.False(t, replay.Succeeded)
			require.Empty(t, replay.Responses[0].GetResponseRange().Kvs)
			out, e := f.b.ResolveStage(ctx, result.Reference.Stage)
			require.NoError(t, e)
			require.Equal(t, OutcomeAborted, out)
			require.NoError(t, f.b.ReleaseStage(ctx, stage))
			if fresh != nil {
				require.NotEqual(t, f.c.reference.ClaimID, fresh.reference.ClaimID)
				require.NotEqual(t, f.c.reference.LeaseID, fresh.reference.LeaseID)
			}
		})
	}
}
