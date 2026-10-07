package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestTaskCheckpointCAS(t *testing.T) {
	t.Run("unknown-original-resolver", func(t *testing.T) {
		b, _, ref, ctx := taskClaimFixture(t)
		c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
		require.NoError(t, e)
		defer taskReleaseClaim(t, b, c)
		before, e := b.readDomain(ctx, c.checkpointKey)
		require.NoError(t, e)
		in := TaskCheckpointInput{StageID: "checkpoint", ExpectedRevision: before[0].ModRevision, State: TaskCheckpointPending, DetailDigest: strings.Repeat("d", 64)}
		stage, e := b.PrepareTaskCheckpoint(ctx, c, in, 30*time.Second)
		require.NoError(t, e)
		defer b.ReleaseStage(ctx, stage)
		kv := b.client.KV
		b.client.KV = &faultKV{KV: kv, match: putsKey(c.checkpointKey), after: func(r *clientv3.TxnResponse, e error) (*clientv3.TxnResponse, error) {
			require.NoError(t, e)
			require.True(t, r.Succeeded)
			return nil, errors.New("lost committed checkpoint reply")
		}}
		out, e := b.CommitStage(ctx, stage)
		require.ErrorIs(t, e, ErrOutcomeUnknown)
		require.Equal(t, OutcomeUnknown, out)
		b.client.KV = kv
		out, e = b.ResolveStage(ctx, stage.Reference())
		require.NoError(t, e)
		require.Equal(t, OutcomeCommitted, out)
		cp, e := b.LoadTaskCheckpoint(ctx, ref)
		require.NoError(t, e)
		require.Equal(t, stage.Reference(), cp.Attempt.reference(stage.Reference().Digest))
	})
	for _, kind := range []string{"recreate", "lease", "reference", "attempt", "empty-history", "physical-state"} {
		t.Run("current-checkpoint/"+kind, func(t *testing.T) {
			b, raw, ref, ctx := taskClaimFixture(t)
			c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
			require.NoError(t, e)
			defer taskReleaseClaim(t, b, c)
			before, e := raw.Get(ctx, c.checkpointKey)
			require.NoError(t, e)
			var record TaskCheckpointRecord
			require.NoError(t, json.Unmarshal(before.Kvs[0].Value, &record))
			id := clientv3.LeaseID(0)
			switch kind {
			case "recreate":
				_, e = raw.Delete(ctx, c.checkpointKey)
				require.NoError(t, e)
			case "lease":
				g, e := raw.Grant(ctx, 30)
				require.NoError(t, e)
				id = g.ID
				defer raw.Revoke(ctx, id)
			case "reference":
				record.Reference.SandboxID = "different"
			case "attempt":
				record.Attempt.AttemptID = "bad"
			case "physical-state":
				record.State = "closed"
			}
			wire, e := json.Marshal(record)
			require.NoError(t, e)
			_, e = raw.Put(ctx, c.checkpointKey, string(wire), clientv3.WithLease(id))
			require.NoError(t, e)
			changed, e := raw.Get(ctx, c.checkpointKey)
			require.NoError(t, e)
			lease := &creationFaultLease{Lease: b.client.Lease}
			b.client.Lease = lease
			stage, e := b.PrepareTaskCheckpoint(ctx, c, TaskCheckpointInput{StageID: "checkpoint", ExpectedRevision: changed.Kvs[0].ModRevision, State: TaskCheckpointPending, DetailDigest: strings.Repeat("e", 64)}, 30*time.Second)
			require.Error(t, e)
			require.Nil(t, stage)
			require.Zero(t, lease.grants.Load())
		})
	}

	t.Run("stale-cas-and-competing-attempts", func(t *testing.T) {
		b, _, ref, ctx := taskClaimFixture(t)
		c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
		require.NoError(t, e)
		defer taskReleaseClaim(t, b, c)
		values, e := b.readDomain(ctx, c.checkpointKey)
		require.NoError(t, e)
		in := TaskCheckpointInput{StageID: "checkpoint", ExpectedRevision: values[0].ModRevision + 1, State: TaskCheckpointPending, DetailDigest: strings.Repeat("b", 64)}
		lease := &creationFaultLease{Lease: b.client.Lease}
		b.client.Lease = lease
		s, e := b.PrepareTaskCheckpoint(ctx, c, in, 30*time.Second)
		require.ErrorIs(t, e, ErrConflict)
		require.Nil(t, s)
		require.Zero(t, lease.grants.Load())
		for _, bad := range []TaskCheckpointInput{{StageID: "checkpoint", ExpectedRevision: in.ExpectedRevision, State: "closed", DetailDigest: in.DetailDigest}, {StageID: "checkpoint", ExpectedRevision: in.ExpectedRevision, State: TaskCheckpointPending, DetailDigest: ""}, {StageID: "checkpoint", State: TaskCheckpointPending, DetailDigest: in.DetailDigest}} {
			s, e = b.PrepareTaskCheckpoint(ctx, c, bad, time.Second)
			require.ErrorIs(t, e, ErrInvalidRecord)
			require.Nil(t, s)
		}
		require.Zero(t, lease.grants.Load())
		in.ExpectedRevision--
		first, e := b.PrepareTaskCheckpoint(ctx, c, in, 30*time.Second)
		require.NoError(t, e)
		defer b.ReleaseStage(ctx, first)
		second, e := b.PrepareTaskCheckpoint(ctx, c, in, 30*time.Second)
		require.NoError(t, e)
		defer b.ReleaseStage(ctx, second)
		out, e := b.CommitStage(ctx, first)
		require.NoError(t, e)
		require.Equal(t, OutcomeCommitted, out)
		out, e = b.CommitStage(ctx, second)
		require.Error(t, e)
		require.NotEqual(t, OutcomeCommitted, out)
		out, e = b.ResolveStage(ctx, second.Reference())
		require.NoError(t, e)
		require.Equal(t, OutcomeAborted, out)
		require.NoError(t, b.RenewTaskClaim(ctx, c))
	})

	t.Run("two-updates-and-new-claim", func(t *testing.T) {
		b, raw, ref, ctx := taskClaimFixture(t)
		c, e := b.ClaimTask(ctx, ref, "worker", 30*time.Second)
		require.NoError(t, e)
		defer taskReleaseClaim(t, b, c)
		key, e := b.namespace.taskCheckpointKey(ref)
		require.NoError(t, e)
		for _, state := range []TaskCheckpointState{TaskCheckpointNeedsReconciliation, TaskCheckpointPending} {
			before, e := raw.Get(ctx, key)
			require.NoError(t, e)
			require.Len(t, before.Kvs, 1)
			stage, e := b.PrepareTaskCheckpoint(ctx, c, TaskCheckpointInput{StageID: "checkpoint", ExpectedRevision: before.Kvs[0].ModRevision, State: state, DetailDigest: strings.Repeat("a", 64)}, 30*time.Second)
			require.NoError(t, e)
			out, e := b.CommitStage(ctx, stage)
			require.NoError(t, e)
			require.Equal(t, OutcomeCommitted, out)
			require.NoError(t, b.ReleaseStage(ctx, stage))
			after, e := raw.Get(ctx, key)
			require.NoError(t, e)
			require.Equal(t, before.Kvs[0].CreateRevision, after.Kvs[0].CreateRevision)
			require.Greater(t, after.Kvs[0].ModRevision, before.Kvs[0].ModRevision)
			record, e := b.LoadTaskCheckpoint(ctx, ref)
			require.NoError(t, e)
			require.Equal(t, c.Reference().ClaimID, record.ClaimID)
			require.Equal(t, stage.Reference(), record.Attempt.reference(stage.Reference().Digest))
			require.Equal(t, state, record.State)
			require.NoError(t, b.RenewTaskClaim(ctx, c), "checkpoint ModRevision must not poison fixed claim fences")
		}
		require.NoError(t, b.ReleaseTaskClaim(ctx, c))
		next, e := b.ClaimTask(ctx, ref, "next-worker", 30*time.Second)
		require.NoError(t, e)
		require.NotEqual(t, c.Reference().ClaimID, next.Reference().ClaimID)
		require.NoError(t, b.ReleaseTaskClaim(ctx, next))
	})
}

func TestTaskCheckpointLostClaim(t *testing.T) {
	for _, kind := range []string{"release", "native-revoke", "natural-expiry", "parent-cancel"} {
		t.Run(kind, func(t *testing.T) {
			b, raw, ref, parent := taskClaimFixture(t)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			ttl := 30 * time.Second
			if kind == "natural-expiry" {
				ttl = time.Second
			}
			c, e := b.ClaimTask(ctx, ref, "worker", ttl)
			require.NoError(t, e)
			defer taskReleaseClaim(t, b, c)
			before, e := raw.Get(parent, c.checkpointKey)
			require.NoError(t, e)
			require.Len(t, before.Kvs, 1)
			in := TaskCheckpointInput{StageID: "checkpoint", ExpectedRevision: before.Kvs[0].ModRevision, State: TaskCheckpointNeedsReconciliation, DetailDigest: strings.Repeat("c", 64)}
			stage, e := b.PrepareTaskCheckpoint(ctx, c, in, 30*time.Second)
			require.NoError(t, e)
			defer b.ReleaseStage(parent, stage)
			switch kind {
			case "release":
				require.NoError(t, b.ReleaseTaskClaim(parent, c))
			case "native-revoke":
				_, e = raw.Revoke(parent, clientv3.LeaseID(c.Reference().LeaseID))
				require.NoError(t, e)
			case "natural-expiry":
				require.Eventually(t, func() bool { v, e := raw.Get(parent, c.guardKey); return e == nil && len(v.Kvs) == 0 }, 8*time.Second, 50*time.Millisecond)
			case "parent-cancel":
				cancel()
			}
			denied, e := b.PrepareTaskCheckpoint(parent, c, in, 30*time.Second)
			require.Error(t, e)
			require.Nil(t, denied)
			require.Error(t, b.RenewTaskClaim(parent, c))
			if kind == "parent-cancel" {
				// The existing Stage protocol does not remotely withdraw on cancellation.
				out, e := b.CommitStage(parent, stage)
				require.NoError(t, e)
				require.Equal(t, OutcomeCommitted, out)
				return
			}
			next, e := b.ClaimTask(parent, ref, "next", 30*time.Second)
			require.NoError(t, e)
			defer taskReleaseClaim(t, b, next)
			out, e := b.CommitStage(parent, stage)
			require.Error(t, e)
			require.NotEqual(t, OutcomeCommitted, out)
			out, e = b.ResolveStage(parent, stage.Reference())
			require.NoError(t, e)
			require.Equal(t, OutcomeAborted, out)
			after, e := raw.Get(parent, c.checkpointKey)
			require.NoError(t, e)
			require.Equal(t, before.Kvs, after.Kvs)
			for _, f := range c.fences {
				v, e := raw.Get(parent, f.key)
				require.NoError(t, e)
				require.Len(t, v.Kvs, 1)
				require.True(t, f.matches(v.Kvs[0]))
			}
			require.Error(t, b.RenewTaskClaim(parent, c))
			require.NoError(t, b.RenewTaskClaim(parent, next))
		})
	}
}
