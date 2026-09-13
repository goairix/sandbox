package redis

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type operationUpdater interface {
	UpdateOperation(context.Context, state.ActiveSandboxOperation, uint64, json.RawMessage) (*state.ActiveSandboxRecord, error)
}

func TestWorkspaceExclusiveAdmissionAndFencedUpdate(t *testing.T) {
	repo, _ := activeRepositoryForTest(t)
	ctx := context.Background()
	record := activeRecordForTest("sandbox-exclusive")
	require.NoError(t, repo.Publish(ctx, record))
	t.Cleanup(func() { _ = repo.forceDelete(ctx, record.SandboxID) })
	_, err := repo.Activate(ctx, record.SandboxID, 1, record.Snapshot)
	require.NoError(t, err)
	_, data, err := repo.BeginOperation(ctx, record.SandboxID, "reader", state.ActiveOperationData, time.Second)
	require.NoError(t, err)
	active, exclusive, err := repo.BeginOperation(ctx, record.SandboxID, "exclusive", state.ActiveOperationKind("exclusive"), time.Second)
	require.NoError(t, err, "workspace exclusive admission must be supported")
	for _, kind := range []state.ActiveOperationKind{state.ActiveOperationData, state.ActiveOperationMutation, "exclusive"} {
		_, _, err = repo.BeginOperation(ctx, record.SandboxID, "late-"+string(kind), kind, time.Second)
		require.Error(t, err, "exclusive must close admission")
	}
	_, err = repo.RenewOperation(ctx, *data, time.Second)
	require.NoError(t, err, "draining readers must retain renewal")
	updater, ok := any(repo).(operationUpdater)
	require.True(t, ok, "snapshot writes must validate the exact live operation token")
	_, err = updater.UpdateOperation(ctx, *exclusive, active.Revision, active.Snapshot)
	require.Error(t, err, "must drain readers before workspace update")
	require.NoError(t, repo.EndOperation(ctx, *data))
	updated, err := updater.UpdateOperation(ctx, *exclusive, active.Revision, active.Snapshot)
	require.NoError(t, err)
	require.NoError(t, repo.EndOperation(ctx, *exclusive))
	_, err = updater.UpdateOperation(ctx, *exclusive, updated.Revision, active.Snapshot)
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	_, late, err := repo.BeginOperation(ctx, record.SandboxID, "reopened", state.ActiveOperationData, time.Second)
	require.NoError(t, err)
	require.NoError(t, repo.EndOperation(ctx, *late))
}

func TestWorkspaceExpiredExclusiveCannotWriteOrReleaseSuccessor(t *testing.T) {
	repo, _ := activeRepositoryForTest(t)
	ctx := context.Background()
	record := activeRecordForTest("sandbox-exclusive-expiry")
	require.NoError(t, repo.Publish(ctx, record))
	t.Cleanup(func() { _ = repo.forceDelete(ctx, record.SandboxID) })
	_, err := repo.Activate(ctx, record.SandboxID, 1, record.Snapshot)
	require.NoError(t, err)
	_, expired, err := repo.BeginOperation(ctx, record.SandboxID, "old", "exclusive", 20*time.Millisecond)
	require.NoError(t, err)
	require.Eventually(t, func() bool { count, e := repo.LiveOperations(ctx, record.SandboxID); return e == nil && count == 0 }, time.Second, 5*time.Millisecond)
	active, successor, err := repo.BeginOperation(ctx, record.SandboxID, "new", "exclusive", time.Second)
	require.NoError(t, err)
	updater, ok := any(repo).(operationUpdater)
	require.True(t, ok)
	_, err = updater.UpdateOperation(ctx, *expired, active.Revision, active.Snapshot)
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	require.NoError(t, repo.EndOperation(ctx, *expired))
	_, err = updater.UpdateOperation(ctx, *successor, active.Revision, active.Snapshot)
	require.NoError(t, err)
}

func TestCleanupCheckpointAtomicallyFencesControllerToken(t *testing.T) {
	repo, _ := activeRepositoryForTest(t)
	ctx := context.Background()
	record := activeRecordForTest("sandbox-checkpoint-controller")
	require.NoError(t, repo.Publish(ctx, record))
	t.Cleanup(func() { _ = repo.forceDelete(ctx, record.SandboxID) })
	_, err := repo.Activate(ctx, record.SandboxID, 1, record.Snapshot)
	require.NoError(t, err)
	lease, acquired, err := repo.AcquireController(ctx, state.ActiveSandboxControllerLease{SandboxID: record.SandboxID, Token: "first", InstanceID: "replica-a", Generation: record.Generation}, time.Second)
	require.NoError(t, err)
	require.True(t, acquired)
	current, _, _, err := repo.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	wrong := *lease
	wrong.Token = "wrong"
	_, err = repo.CheckpointController(ctx, wrong, current.Revision, "final-output")
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	updated, err := repo.CheckpointController(ctx, *lease, current.Revision, "final-output")
	require.NoError(t, err)
	require.NoError(t, repo.ReleaseController(ctx, *lease))
	successor, acquired, err := repo.AcquireController(ctx, state.ActiveSandboxControllerLease{SandboxID: record.SandboxID, Token: "second", InstanceID: "replica-b", Generation: record.Generation}, time.Second)
	require.NoError(t, err)
	require.True(t, acquired)
	_, err = repo.CheckpointController(ctx, *lease, updated.Revision, "stale-cleanup")
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	removed, err := repo.CheckpointController(ctx, *successor, updated.Revision, "removed")
	require.NoError(t, err)
	require.ErrorIs(t, repo.DeleteController(ctx, *lease, removed.Revision), state.ErrActiveSandboxStaleToken)
	require.NoError(t, repo.DeleteController(ctx, *successor, removed.Revision))
}
