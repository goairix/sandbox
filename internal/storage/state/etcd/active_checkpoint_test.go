package etcd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestCheckpointRequiresDestroyPhaseAndExactRevision(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-checkpoint")
	require.NoError(t, repository.Publish(ctx, record))
	active, err := repository.Activate(ctx, record.SandboxID, 1, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)
	_, err = repository.Checkpoint(ctx, record.SandboxID, active.Revision, "must-reject")
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
	destroying, _, _, err := repository.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	_, err = repository.Checkpoint(ctx, record.SandboxID, active.Revision, "stale")
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
	checkpointed, err := repository.Checkpoint(ctx, record.SandboxID, destroying.Revision, "runtime_stopped")
	require.NoError(t, err)
	require.Equal(t, state.ActiveSandboxCleanupPending, checkpointed.Phase)
	require.Equal(t, destroying.Revision+1, checkpointed.Revision)
	require.Equal(t, "runtime_stopped", checkpointed.CleanupCheckpoint)
}

func TestCheckpointLostReplyIsVisibleOnReadback(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-checkpoint-lost-reply")
	require.NoError(t, repository.Publish(ctx, record))
	destroying, _, _, err := repository.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	realKV := s.client.KV
	fault := &lostReplyKV{KV: realKV}
	fault.drop.Store(true)
	s.client.KV = fault
	_, err = repository.Checkpoint(ctx, record.SandboxID, destroying.Revision, "runtime_stopped")
	s.client.KV = realKV
	require.ErrorContains(t, err, "reply lost after commit")
	current, err := repository.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	require.Equal(t, destroying.Revision+1, current.Revision)
	require.Equal(t, "runtime_stopped", current.CleanupCheckpoint)
}
