package etcd

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestBeginDestroyExpectedFencesInspectedSnapshot(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-expected-destroy")
	require.NoError(t, repository.Publish(ctx, record))
	active, err := repository.Activate(ctx, record.SandboxID, record.Revision, json.RawMessage(`{"workspace":"first"}`))
	require.NoError(t, err)
	stale := *active
	current, err := repository.Update(ctx, record.SandboxID, active.Revision, json.RawMessage(`{"workspace":"second"}`))
	require.NoError(t, err)
	_, err = repository.BeginDestroyExpected(ctx, stale)
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
	stillActive, err := repository.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	require.Equal(t, state.ActiveSandboxActive, stillActive.Phase)

	wrongRuntime := *current
	wrongRuntime.RuntimeUID = "different"
	_, err = repository.BeginDestroyExpected(ctx, wrongRuntime)
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
	destroying, err := repository.BeginDestroyExpected(ctx, *current)
	require.NoError(t, err)
	require.Equal(t, state.ActiveSandboxDestroying, destroying.Phase)
	_, err = repository.BeginDestroyExpected(ctx, *current)
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
}
