package etcd

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTaskCheckpointCAS(t *testing.T) {
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
