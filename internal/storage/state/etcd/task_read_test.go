package etcd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func taskReadFixture(t *testing.T) (*Backend, *clientv3.Client, TaskRecord, TaskCheckpointRecord, context.Context) {
	t.Helper()
	b, raw := integrationBackend(t)
	task, _, _, checkpoint := taskTestRecords()
	task.Reference.Namespace, task.Reference.RestoreEpoch = b.namespace.Root(), b.restoreEpoch
	checkpoint.Reference = task.Reference
	checkpoint.Attempt.Namespace, checkpoint.Attempt.RestoreEpoch = b.namespace.Root(), b.restoreEpoch
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return b, raw, task, checkpoint, ctx
}

// Raw puts provision diagnostic records only. No task authority API, owner,
// control, intent or link is created or inferred from these historical reads.
func putTaskReadFixture(t *testing.T, ctx context.Context, b *Backend, raw *clientv3.Client, ref TaskReference, task TaskRecord, checkpoint TaskCheckpointRecord, options ...clientv3.OpOption) {
	t.Helper()
	taskKey, err := b.namespace.taskKey(ref)
	require.NoError(t, err)
	checkpointKey, err := b.namespace.taskCheckpointKey(ref)
	require.NoError(t, err)
	taskValue, err := encodeTaskRecord(task)
	require.NoError(t, err)
	checkpointValue, err := encodeTaskCheckpointRecord(checkpoint)
	require.NoError(t, err)
	_, err = raw.Txn(ctx).Then(clientv3.OpPut(taskKey, taskValue, options...), clientv3.OpPut(checkpointKey, checkpointValue, options...)).Commit()
	require.NoError(t, err)
}

func TestTaskReadCopies(t *testing.T) {
	ownedFixtureContainers(t)
	b, raw, task, checkpoint, ctx := taskReadFixture(t)
	got, err := b.LoadTask(ctx, task.Reference)
	require.NoError(t, err)
	require.Nil(t, got)
	cp, err := b.LoadTaskCheckpoint(ctx, task.Reference)
	require.NoError(t, err)
	require.Nil(t, cp)
	putTaskReadFixture(t, ctx, b, raw, task.Reference, task, checkpoint)
	got, err = b.LoadTask(ctx, task.Reference)
	require.NoError(t, err)
	require.Equal(t, task, *got)
	got.Runtime.UID, got.Reference.SandboxID, got.Snapshot.Digest = "changed", "changed", "changed"
	again, err := b.LoadTask(ctx, task.Reference)
	require.NoError(t, err)
	require.NotSame(t, got, again)
	require.Equal(t, task, *again)
	cp, err = b.LoadTaskCheckpoint(ctx, task.Reference)
	require.NoError(t, err)
	require.Equal(t, checkpoint, *cp)
	cp.Attempt.RequestID, cp.Reference.SandboxID = "changed", "changed"
	againCP, err := b.LoadTaskCheckpoint(ctx, task.Reference)
	require.NoError(t, err)
	require.NotSame(t, cp, againCP)
	require.Equal(t, checkpoint, *againCP)

	// A later permanent checkpoint is diagnostic and permits Mod > Create.
	checkpoint.ClaimID, checkpoint.DetailDigest = taskClaimTestID, strings.Repeat("b", 64)
	checkpoint.State = TaskCheckpointNeedsReconciliation
	key, err := b.namespace.taskCheckpointKey(task.Reference)
	require.NoError(t, err)
	value, err := encodeTaskCheckpointRecord(checkpoint)
	require.NoError(t, err)
	_, err = raw.Put(ctx, key, value)
	require.NoError(t, err)
	cp, err = b.LoadTaskCheckpoint(ctx, task.Reference)
	require.NoError(t, err)
	require.Equal(t, checkpoint, *cp)

	// Identical bytes cannot restore the immutable task's original revision.
	key, err = b.namespace.taskKey(task.Reference)
	require.NoError(t, err)
	value, err = encodeTaskRecord(task)
	require.NoError(t, err)
	_, err = raw.Put(ctx, key, value)
	require.NoError(t, err)
	got, err = b.LoadTask(ctx, task.Reference)
	require.ErrorIs(t, err, ErrCorruptRecord)
	require.Nil(t, got)
}

func TestTaskReadIdentity(t *testing.T) {
	ownedFixtureContainers(t)
	t.Run("reference-and-context", func(t *testing.T) {
		b, _, task, _, ctx := taskReadFixture(t)
		for _, mutate := range []func(*TaskReference){func(r *TaskReference) { r.Namespace = "/codex-test/other/cell/" }, func(r *TaskReference) { r.RestoreEpoch = "other" }} {
			ref := task.Reference
			mutate(&ref)
			got, err := b.LoadTask(ctx, ref)
			require.ErrorIs(t, err, ErrIdentityMismatch)
			require.Nil(t, got)
			cp, err := b.LoadTaskCheckpoint(ctx, ref)
			require.ErrorIs(t, err, ErrIdentityMismatch)
			require.Nil(t, cp)
		}
		bad := task.Reference
		bad.TaskID = "bad"
		_, err := b.LoadTask(ctx, bad)
		require.ErrorIs(t, err, ErrInvalidRecord)
		_, err = b.LoadTaskCheckpoint(ctx, bad)
		require.ErrorIs(t, err, ErrInvalidRecord)
		_, err = b.LoadTask(nil, task.Reference)
		require.ErrorIs(t, err, ErrInvalidRecord)
		_, err = b.LoadTaskCheckpoint(nil, task.Reference)
		require.ErrorIs(t, err, ErrInvalidRecord)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		got, err := b.LoadTask(canceled, task.Reference)
		require.Error(t, err)
		require.Nil(t, got)
		cp, err := b.LoadTaskCheckpoint(canceled, task.Reference)
		require.Error(t, err)
		require.Nil(t, cp)
	})
	t.Run("full-record-reference", func(t *testing.T) {
		b, raw, task, checkpoint, ctx := taskReadFixture(t)
		for _, mutate := range []func(*TaskReference){func(r *TaskReference) { r.Namespace = "/codex-test/other/cell/" }, func(r *TaskReference) { r.RestoreEpoch = "other" }, func(r *TaskReference) { r.SandboxID = "other" }, func(r *TaskReference) { r.TaskID = taskClaimTestID }, func(r *TaskReference) { r.Partition = 0xac }} {
			bad, cp := task, checkpoint
			mutate(&bad.Reference)
			cp.Reference = bad.Reference
			cp.Attempt.Namespace, cp.Attempt.RestoreEpoch, cp.Attempt.Partition = bad.Reference.Namespace, bad.Reference.RestoreEpoch, bad.Reference.Partition
			if bad.Reference.Partition != task.Reference.Partition {
				bad.WorkspaceHash = "ac" + strings.Repeat("1", 62)
			}
			// Recreate only test records to isolate value mismatch from immutable revision rejection.
			key, _ := b.namespace.taskKey(task.Reference)
			_, err := raw.Delete(ctx, key)
			require.NoError(t, err)
			putTaskReadFixture(t, ctx, b, raw, task.Reference, bad, cp)
			got, err := b.LoadTask(ctx, task.Reference)
			require.ErrorIs(t, err, ErrCorruptRecord)
			require.Nil(t, got)
			gotCP, err := b.LoadTaskCheckpoint(ctx, task.Reference)
			require.ErrorIs(t, err, ErrCorruptRecord)
			require.Nil(t, gotCP)
		}
	})
	t.Run("leased-records", func(t *testing.T) {
		b, raw, task, cp, ctx := taskReadFixture(t)
		lease, err := raw.Grant(ctx, 30)
		require.NoError(t, err)
		t.Cleanup(func() {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := raw.Revoke(c, lease.ID)
			require.NoError(t, err)
		})
		putTaskReadFixture(t, ctx, b, raw, task.Reference, task, cp, clientv3.WithLease(lease.ID))
		got, err := b.LoadTask(ctx, task.Reference)
		require.ErrorIs(t, err, ErrCorruptRecord)
		require.Nil(t, got)
		gotCP, err := b.LoadTaskCheckpoint(ctx, task.Reference)
		require.ErrorIs(t, err, ErrCorruptRecord)
		require.Nil(t, gotCP)
	})
	for _, field := range []string{"operator-identity", "restore-epoch"} {
		t.Run(field, func(t *testing.T) {
			b, raw, task, cp, ctx := taskReadFixture(t)
			putTaskReadFixture(t, ctx, b, raw, task.Reference, task, cp)
			key, value := b.identityKey, strings.Replace(b.identityValue, "test-storage", "changed-storage", 1)
			if field == "restore-epoch" {
				key, value = b.restoreKey, "changed-epoch"
			}
			_, err := raw.Put(ctx, key, value)
			require.NoError(t, err)
			// Even exact absent points must not mask a changed operator identity.
			for _, id := range []string{taskTestID, taskClaimTestID} {
				ref := task.Reference
				ref.TaskID = id
				got, err := b.LoadTask(ctx, ref)
				require.ErrorIs(t, err, ErrIdentityMismatch)
				require.Nil(t, got)
				gotCP, err := b.LoadTaskCheckpoint(ctx, ref)
				require.ErrorIs(t, err, ErrIdentityMismatch)
				require.Nil(t, gotCP)
			}
		})
	}
}
