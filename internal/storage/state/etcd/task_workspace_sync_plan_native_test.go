package etcd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func workspaceSyncNativeFixture(t *testing.T) (*quiescenceNativeFixture, *PreparedTaskQuiescence, *quiescenceWireTarget, TaskWorkspaceSyncPlan) {
	t.Helper()
	base := newTaskDeliveryFixture(t, true)
	archiveTrackCleanup(base.b) // Before finishQuiescenceFixture's first Release.
	f := finishQuiescenceFixture(t, base)
	archiveTrackCleanup(f.b)
	prepared, target := finalizationNativePrepared(t, f)
	workspace := workspaceSyncIdentity(t, "s3", "test-storage", "bucket", "team/project/")
	require.Equal(t, workspace.Hash(), prepared.pin.task.WorkspaceHash)
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspace, Exclude: []string{".agent/cache", ".agent"}, DeleteExtraneous: true})
	require.NoError(t, err)
	return f, prepared, target, plan
}

func TestTaskWorkspaceSyncPlanNativeMismatchThenOriginal(t *testing.T) {
	f, prepared, target, plan := workspaceSyncNativeFixture(t)
	queries := target.queries.Load()
	original := f.b.client.KV
	transactions := 0
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		transactions++
		return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	for _, targetIdentity := range []WorkspaceIdentity{
		workspaceSyncIdentity(t, "s3", "other-storage", "bucket", "team/project/"),
		workspaceSyncIdentity(t, "minio", "test-storage", "bucket", "team/project/"),
		workspaceSyncIdentity(t, "s3", "test-storage", "other-bucket", "team/project/"),
		workspaceSyncIdentity(t, "s3", "test-storage", "bucket", "other/project/"),
	} {
		wrong, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: targetIdentity, DeleteExtraneous: true})
		require.NoError(t, err)
		refused, err := f.b.ReserveTaskWorkspaceSync(f.ctx, f.c, prepared, target.destination, wrong)
		require.ErrorIs(t, err, ErrIdentityMismatch)
		require.Equal(t, OutcomeUnknown, refused.Outcome)
		require.Empty(t, refused.Reference.AttemptID)
		require.Nil(t, f.c.finalSyncDraft)
	}
	copyPrepared := *prepared
	for _, fault := range []string{"copied-prepared", "foreign-backend", "foreign-claim", "zero-plan", "nil-context"} {
		b, c, p, ctx, input := f.b, f.c, prepared, f.ctx, plan
		switch fault {
		case "copied-prepared":
			p = &copyPrepared
		case "foreign-backend":
			b = new(Backend)
		case "foreign-claim":
			c = new(TaskClaim)
		case "zero-plan":
			input = TaskWorkspaceSyncPlan{}
		case "nil-context":
			ctx = nil
		}
		_, err := b.ReserveTaskWorkspaceSync(ctx, c, p, target.destination, input)
		require.ErrorIs(t, err, ErrInvalidRecord, fault)
	}
	require.Zero(t, transactions, "refusal must precede native calls")
	require.Equal(t, queries, target.queries.Load(), "refusal must precede destination query")
	key, err := f.b.namespace.taskFinalSyncAttemptKey(f.c.reference.Task)
	require.NoError(t, err)
	absent, err := f.raw.Get(f.ctx, key)
	require.NoError(t, err)
	require.Empty(t, absent.Kvs)
	f.b.client.KV = original
	writes := finalSyncCapture(t, f, prepared, nil, nil)
	reserved, err := f.b.ReserveTaskWorkspaceSync(f.ctx, f.c, prepared, target.destination, plan)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, reserved.Outcome)
	entry, err := f.b.LoadTaskFinalSyncAttempt(f.ctx, f.c.reference.Task)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Equal(t, reserved.Reference, entry.Reference)
	metadata, err := plan.FinalSyncPlan()
	require.NoError(t, err)
	require.Equal(t, metadata, entry.Record.Plan)
	require.NoError(t, plan.MatchesAttempt(*entry.Record))
	native, err := f.raw.Get(f.ctx, key)
	require.NoError(t, err)
	require.Len(t, native.Kvs, 1)
	require.Zero(t, native.Kvs[0].Lease)
	queries = target.queries.Load()
	for _, changed := range []TaskWorkspaceSyncPlanOptions{
		{Workspace: plan.workspace, Exclude: []string{".agent"}},
		{Workspace: plan.workspace, Exclude: []string{".agent", "extra"}, DeleteExtraneous: true},
	} {
		policy, err := NewTaskWorkspaceSyncPlan(changed)
		require.NoError(t, err)
		conflict, err := f.b.ReserveTaskWorkspaceSync(f.ctx, f.c, prepared, target.destination, policy)
		require.ErrorIs(t, err, ErrConflict)
		require.Equal(t, reserved.Reference, conflict.Reference)
		require.ErrorIs(t, policy.MatchesAttempt(*entry.Record), ErrIdentityMismatch)
	}
	again, err := f.b.ReserveTaskWorkspaceSync(f.ctx, f.c, prepared, target.destination, plan)
	require.NoError(t, err)
	require.Equal(t, reserved, again)
	require.Equal(t, queries, target.queries.Load())
	require.Equal(t, 1, *writes)
	require.Zero(t, target.effects.Load())
}

func TestTaskWorkspaceSyncPlanNativeCommittedResponseLoss(t *testing.T) {
	f, prepared, target, plan := workspaceSyncNativeFixture(t)
	writes := finalSyncCapture(t, f, prepared, nil, func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, response.Succeeded)
		return nil, context.DeadlineExceeded
	})
	first, err := f.b.ReserveTaskWorkspaceSync(f.ctx, f.c, prepared, target.destination, plan)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, first.Outcome)
	require.NoError(t, first.Reference.Validate())
	again, err := f.b.ReserveTaskWorkspaceSync(f.ctx, f.c, prepared, target.destination, plan)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, again.Outcome)
	require.Equal(t, first.Reference, again.Reference)
	entry, err := f.b.LoadTaskFinalSyncAttempt(f.ctx, f.c.reference.Task)
	require.NoError(t, err)
	require.Equal(t, first.Reference, entry.Reference)
	require.NoError(t, plan.MatchesAttempt(*entry.Record))
	require.Equal(t, 1, *writes)
	require.Zero(t, target.effects.Load(), "metadata diagnostics supply no sender or settlement")
}
