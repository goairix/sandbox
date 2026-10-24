package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestBeginDestroyClosesOperationAdmission(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-destroy")
	require.NoError(t, repository.Publish(ctx, record))
	_, err = repository.Activate(ctx, record.SandboxID, 1, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)

	_, operation, err := repository.BeginOperation(ctx, record.SandboxID, "op-1", state.ActiveOperationData, 5*time.Second)
	require.NoError(t, err)
	require.Equal(t, record.SandboxID, operation.SandboxID)
	require.Equal(t, state.ActiveOperationData, operation.Kind)
	require.Greater(t, operation.ExpiresAt, time.Now())

	destroying, live, won, err := repository.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	require.True(t, won)
	require.Equal(t, state.ActiveSandboxDestroying, destroying.Phase)
	require.Equal(t, int64(1), live)
	_, _, err = repository.BeginOperation(ctx, record.SandboxID, "op-2", state.ActiveOperationData, 5*time.Second)
	require.ErrorIs(t, err, state.ErrActiveSandboxAdmissionClosed)

	require.NoError(t, repository.EndOperation(ctx, *operation))
	live, err = repository.LiveOperations(ctx, record.SandboxID)
	require.NoError(t, err)
	require.Zero(t, live)
	again, live, won, err := repository.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	require.False(t, won)
	require.Zero(t, live)
	require.Equal(t, destroying.Revision, again.Revision)
}

func TestExpiredOperationDoesNotBlockDestroy(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-expire")
	require.NoError(t, repository.Publish(ctx, record))
	_, err = repository.Activate(ctx, record.SandboxID, 1, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)
	_, _, err = repository.BeginOperation(ctx, record.SandboxID, "op-short", state.ActiveOperationData, time.Second)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		live, err := repository.LiveOperations(ctx, record.SandboxID)
		return err == nil && live == 0
	}, 4*time.Second, 50*time.Millisecond)
	_, live, won, err := repository.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	require.True(t, won)
	require.Zero(t, live)
}

func TestDuplicateOperationTokenIsFenced(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-duplicate-op")
	require.NoError(t, repository.Publish(ctx, record))
	_, err = repository.Activate(ctx, record.SandboxID, 1, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)
	_, _, err = repository.BeginOperation(ctx, record.SandboxID, "op-1", state.ActiveOperationData, 5*time.Second)
	require.NoError(t, err)
	_, _, err = repository.BeginOperation(ctx, record.SandboxID, "op-1", state.ActiveOperationData, 5*time.Second)
	require.True(t, errors.Is(err, state.ErrActiveSandboxConflict))
}

func TestBeginDestroyLostReplyIsResolvedByReadingCurrentPhase(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-lost-destroy-reply")
	require.NoError(t, repository.Publish(ctx, record))
	_, err = repository.Activate(ctx, record.SandboxID, 1, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)
	realKV := s.client.KV
	fault := &lostReplyKV{KV: realKV}
	fault.drop.Store(true)
	s.client.KV = fault
	_, _, _, err = repository.BeginDestroy(ctx, record.SandboxID)
	s.client.KV = realKV
	require.ErrorContains(t, err, "reply lost after commit")

	current, live, won, err := repository.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	require.False(t, won)
	require.Zero(t, live)
	require.Equal(t, state.ActiveSandboxDestroying, current.Phase)
	require.Equal(t, uint64(3), current.Revision)
}
