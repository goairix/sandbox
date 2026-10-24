package etcd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestMutationUpdateRequiresLiveTokenAndRevision(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-mutation-update")
	require.NoError(t, repository.Publish(ctx, record))
	active, err := repository.Activate(ctx, record.SandboxID, 1, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)
	_, operation, err := repository.BeginOperation(ctx, record.SandboxID, "mutation", state.ActiveOperationMutation, 5*time.Second)
	require.NoError(t, err)
	wrong := *operation
	wrong.Token = "different"
	_, err = repository.UpdateOperation(ctx, wrong, active.Revision, json.RawMessage(`{"step":1}`))
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	updated, err := repository.UpdateOperation(ctx, *operation, active.Revision, json.RawMessage(`{"step":1}`))
	require.NoError(t, err)
	require.Equal(t, uint64(3), updated.Revision)
	_, err = repository.UpdateOperation(ctx, *operation, active.Revision, json.RawMessage(`{"step":2}`))
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
	require.NoError(t, repository.EndOperation(ctx, *operation))
	_, err = repository.UpdateOperation(ctx, *operation, updated.Revision, json.RawMessage(`{"step":3}`))
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
}

func TestExclusiveUpdateWaitsForOtherOperationsToDrain(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-exclusive-update")
	require.NoError(t, repository.Publish(ctx, record))
	_, err = repository.Activate(ctx, record.SandboxID, 1, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)
	_, data, err := repository.BeginOperation(ctx, record.SandboxID, "data", state.ActiveOperationData, 5*time.Second)
	require.NoError(t, err)
	exclusiveRecord, exclusive, err := repository.BeginOperation(ctx, record.SandboxID, "exclusive", state.ActiveOperationExclusive, 5*time.Second)
	require.NoError(t, err)
	_, err = repository.UpdateOperation(ctx, *exclusive, exclusiveRecord.Revision, json.RawMessage(`{"workspace_transition":"mounting"}`))
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
	require.NoError(t, repository.EndOperation(ctx, *data))
	updated, err := repository.UpdateOperation(ctx, *exclusive, exclusiveRecord.Revision, json.RawMessage(`{"workspace_transition":"mounting"}`))
	require.NoError(t, err)
	require.Equal(t, uint64(4), updated.Revision)
	require.NoError(t, repository.EndOperation(ctx, *exclusive))
	current, err := repository.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	require.Equal(t, state.ActiveSandboxCleanupPending, current.Phase)
	require.Equal(t, "workspace_transition_incomplete", current.CleanupCheckpoint)
}

func TestWorkspaceDetachRevokesControllerWithSnapshot(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-detach-controller")
	require.NoError(t, repository.Publish(ctx, record))
	active, err := repository.Activate(ctx, record.SandboxID, 1, json.RawMessage(`{"workspace":{"mounted":true}}`))
	require.NoError(t, err)
	controller := strings.TrimPrefix(repository.controllerKey(record.SandboxID), s.prefix)
	require.NoError(t, s.Set(ctx, controller, []byte("owner"), 0))
	_, operation, err := repository.BeginOperation(ctx, record.SandboxID, "mutation", state.ActiveOperationMutation, 5*time.Second)
	require.NoError(t, err)
	kept, err := repository.UpdateOperation(ctx, *operation, active.Revision, json.RawMessage(`{"workspace":{"mounted":true},"step":1}`))
	require.NoError(t, err)
	exists, err := s.Exists(ctx, controller)
	require.NoError(t, err)
	require.True(t, exists)
	_, err = repository.UpdateOperation(ctx, *operation, kept.Revision, json.RawMessage(`{"workspace":null,"step":2}`))
	require.NoError(t, err)
	exists, err = s.Exists(ctx, controller)
	require.NoError(t, err)
	require.False(t, exists)
}

func TestMutationUpdateLostReplyLeavesOneRevision(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-update-lost-reply")
	require.NoError(t, repository.Publish(ctx, record))
	active, err := repository.Activate(ctx, record.SandboxID, 1, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)
	_, operation, err := repository.BeginOperation(ctx, record.SandboxID, "mutation", state.ActiveOperationMutation, 5*time.Second)
	require.NoError(t, err)
	realKV := s.client.KV
	fault := &lostReplyKV{KV: realKV}
	fault.drop.Store(true)
	s.client.KV = fault
	_, err = repository.UpdateOperation(ctx, *operation, active.Revision, json.RawMessage(`{"step":1}`))
	s.client.KV = realKV
	require.ErrorContains(t, err, "reply lost after commit")
	current, err := repository.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	require.Equal(t, uint64(3), current.Revision)
	require.JSONEq(t, `{"step":1}`, string(current.Snapshot))
	_, err = repository.UpdateOperation(ctx, *operation, active.Revision, json.RawMessage(`{"step":2}`))
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
}
