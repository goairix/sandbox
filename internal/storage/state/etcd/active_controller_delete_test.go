package etcd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestDeleteControllerRequiresDrainAndCurrentHolder(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-controller-delete")
	require.NoError(t, repository.Publish(ctx, record))
	_, err = repository.Activate(ctx, record.SandboxID, record.Revision, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)
	_, operation, err := repository.BeginOperation(ctx, record.SandboxID, "in-flight", state.ActiveOperationData, 5*time.Second)
	require.NoError(t, err)
	destroying, live, won, err := repository.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, int64(1), live)

	holder := state.ActiveSandboxControllerLease{
		SandboxID: record.SandboxID, Token: "holder", InstanceID: "api-1", Generation: record.Generation,
	}
	lease, won, err := repository.AcquireController(ctx, holder, 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	removed, err := repository.DeleteControllerWithResult(ctx, *lease, destroying.Revision)
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)
	require.False(t, removed)
	require.NoError(t, repository.EndOperation(ctx, *operation))

	wrong := *lease
	wrong.Token = "wrong"
	removed, err = repository.DeleteControllerWithResult(ctx, wrong, destroying.Revision)
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	require.False(t, removed)
	removed, err = repository.DeleteControllerWithResult(ctx, *lease, destroying.Revision)
	require.NoError(t, err)
	require.True(t, removed)
	current, err := repository.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	require.Nil(t, current)
	indexes, err := s.client.Get(ctx, s.prefix+"active/"+repository.scope+"/index/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Empty(t, indexes.Kvs)
	removed, err = repository.DeleteControllerWithResult(ctx, *lease, destroying.Revision)
	require.NoError(t, err)
	require.False(t, removed)
}

func TestDeleteControllerRejectsExpiredHolder(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-expired-controller-delete")
	require.NoError(t, repository.Publish(ctx, record))
	destroying, _, _, err := repository.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	holder := state.ActiveSandboxControllerLease{
		SandboxID: record.SandboxID, Token: "old", InstanceID: "api-1", Generation: record.Generation,
	}
	lease, won, err := repository.AcquireController(ctx, holder, time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.Eventually(t, func() bool {
		response, err := s.client.Get(ctx, repository.controllerKey(record.SandboxID))
		return err == nil && len(response.Kvs) == 0
	}, 4*time.Second, 50*time.Millisecond)
	removed, err := repository.DeleteControllerWithResult(ctx, *lease, destroying.Revision)
	require.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
	require.False(t, removed)
	current, err := repository.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	require.NotNil(t, current)
}
