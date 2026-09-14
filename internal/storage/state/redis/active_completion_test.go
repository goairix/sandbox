package redis

import (
	"context"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

func activeCompletionFixture(t *testing.T) (*ActiveSandboxRepository, state.ActiveSandboxRecord, state.ActiveSandboxControllerLease) {
	t.Helper()
	repository, _ := activeRepositoryForTest(t)
	ctx := context.Background()
	initial := activeRecordForTest("sandbox-atomic-completion")
	require.NoError(t, repository.Publish(ctx, initial))
	t.Cleanup(func() { require.NoError(t, repository.forceDelete(ctx, initial.SandboxID)) })
	_, err := repository.Activate(ctx, initial.SandboxID, initial.Revision, initial.Snapshot)
	require.NoError(t, err)
	lease, acquired, err := repository.AcquireController(ctx, state.ActiveSandboxControllerLease{
		SandboxID: initial.SandboxID, Token: "cleanup-controller", InstanceID: "api-a", Generation: initial.Generation,
	}, time.Second)
	require.NoError(t, err)
	require.True(t, acquired)
	current, _, _, err := repository.BeginDestroy(ctx, initial.SandboxID)
	require.NoError(t, err)
	return repository, *current, *lease
}

func TestActiveControllerAtomicCompletionDistinguishesHistoricalAbsence(t *testing.T) {
	repository, record, lease := activeCompletionFixture(t)
	ctx := context.Background()
	wrong := lease
	wrong.Token = "replacement-token"
	completed, err := repository.DeleteControllerWithResult(ctx, wrong, record.Revision)
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	require.False(t, completed)
	completed, err = repository.DeleteControllerWithResult(ctx, lease, record.Revision+1)
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
	require.False(t, completed)
	completed, err = repository.DeleteControllerWithResult(ctx, lease, record.Revision)
	require.NoError(t, err)
	require.True(t, completed, "the atomic command reports actual acknowledged deletion")
	completed, err = repository.DeleteControllerWithResult(ctx, lease, record.Revision)
	require.NoError(t, err)
	require.False(t, completed, "a later idempotent absence ACK is not another completion")
	require.NoError(t, repository.DeleteController(ctx, lease, record.Revision), "the error-only contract retains idempotent success")
}

func TestActiveControllerAtomicCompletionRejectsUnconfirmedDurability(t *testing.T) {
	repository, record, lease := activeCompletionFixture(t)
	ctx := context.Background()
	repository.store.durability, repository.store.ackReplicas, repository.store.ackTimeout = DurabilityReplicaAck, 1, time.Millisecond
	completed, err := repository.DeleteControllerWithResult(ctx, lease, record.Revision)
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.False(t, completed, "a committed deletion with failed ACK cannot charge metrics")
	completed, err = repository.DeleteControllerWithResult(ctx, lease, record.Revision)
	require.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	require.False(t, completed, "historical absence must still honor the existing barrier ACK")
	require.ErrorIs(t, repository.DeleteController(ctx, lease, record.Revision), state.ErrDurabilityUnconfirmed)
}
