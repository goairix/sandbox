package etcd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestDeleteRequiresClosedAndDrainedRecord(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-delete")
	require.NoError(t, repository.Publish(ctx, record))
	require.ErrorIs(t, repository.Delete(ctx, record.SandboxID, record.Revision, record.Generation), state.ErrActiveSandboxConflict)
	_, err = repository.Activate(ctx, record.SandboxID, record.Revision, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)
	_, operation, err := repository.BeginOperation(ctx, record.SandboxID, "live", state.ActiveOperationData, 5*time.Second)
	require.NoError(t, err)
	destroying, _, _, err := repository.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	require.ErrorIs(t, repository.Delete(ctx, record.SandboxID, destroying.Revision, destroying.Generation), state.ErrActiveSandboxConflict)
	require.NoError(t, repository.EndOperation(ctx, *operation))
	require.ErrorIs(t, repository.Delete(ctx, record.SandboxID, destroying.Revision+1, destroying.Generation), state.ErrActiveSandboxConflict)
	require.NoError(t, repository.Delete(ctx, record.SandboxID, destroying.Revision, destroying.Generation))
	require.NoError(t, repository.Delete(ctx, record.SandboxID, destroying.Revision, destroying.Generation))
	current, err := repository.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	require.Nil(t, current)
}
