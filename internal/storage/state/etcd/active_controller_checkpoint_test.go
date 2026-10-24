package etcd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestControllerCheckpointRequiresCurrentLeasedHolder(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-controller-checkpoint")
	require.NoError(t, repository.Publish(ctx, record))
	destroying, _, _, err := repository.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	request := state.ActiveSandboxControllerLease{
		SandboxID: record.SandboxID, Token: "holder", InstanceID: "api-1", Generation: record.Generation,
	}
	lease, won, err := repository.AcquireController(ctx, request, 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	other := *lease
	other.Token = "other"
	_, err = repository.CheckpointController(ctx, other, destroying.Revision, "wrong")
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	checkpointed, err := repository.CheckpointController(ctx, *lease, destroying.Revision, "runtime_stopped")
	require.NoError(t, err)
	require.Equal(t, state.ActiveSandboxCleanupPending, checkpointed.Phase)
	require.Equal(t, "runtime_stopped", checkpointed.CleanupCheckpoint)
	_, err = repository.CheckpointController(ctx, *lease, destroying.Revision, "old_revision")
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
	other = *lease
	other.InstanceID = "api-2"
	_, err = repository.CheckpointController(ctx, other, checkpointed.Revision, "wrong_instance")
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	require.NoError(t, repository.ReleaseController(ctx, *lease))
	_, err = repository.CheckpointController(ctx, *lease, checkpointed.Revision, "late")
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
}
