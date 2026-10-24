package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func validActiveRecord(id string) state.ActiveSandboxRecord {
	now := time.Now().UTC()
	return state.ActiveSandboxRecord{
		Version: 1, SandboxID: id, Phase: state.ActiveSandboxPublishing,
		Revision: 1, Generation: 1, RuntimeID: "pod", RuntimeUID: "uid",
		Snapshot: json.RawMessage(`{"ready":false}`), CreatedAt: now, UpdatedAt: now,
	}
}

func TestActivePublishAndTransitionAcrossClients(t *testing.T) {
	firstStore := testStore(t)
	secondStore, err := New(context.Background(), Options{
		Endpoints: []string{os.Getenv("TEST_ETCD_ENDPOINT")},
		Prefix:    firstStore.prefix, Timeout: 3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, secondStore.Close()) })
	first, err := NewActiveSandboxRepository(firstStore, "cell-a")
	require.NoError(t, err)
	second, err := NewActiveSandboxRepository(secondStore, "cell-a")
	require.NoError(t, err)
	ctx := context.Background()
	record := validActiveRecord("sandbox-1")

	require.NoError(t, first.Publish(ctx, record))
	require.ErrorIs(t, second.Publish(ctx, record), state.ErrActiveSandboxConflict)
	loaded, err := second.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	require.Equal(t, record, *loaded)

	activated, err := second.Activate(ctx, record.SandboxID, record.Revision, json.RawMessage(`{"ready":true}`))
	require.NoError(t, err)
	require.Equal(t, state.ActiveSandboxActive, activated.Phase)
	require.Equal(t, uint64(2), activated.Revision)
	_, err = first.Update(ctx, record.SandboxID, record.Revision, json.RawMessage(`{"stale":true}`))
	require.ErrorIs(t, err, state.ErrActiveSandboxConflict)

	updated, err := first.Update(ctx, record.SandboxID, activated.Revision, json.RawMessage(`{"ready":true,"step":2}`))
	require.NoError(t, err)
	require.Equal(t, uint64(3), updated.Revision)
	require.JSONEq(t, `{"ready":true,"step":2}`, string(updated.Snapshot))
}

func TestActivePublishLostReplyDoesNotRepublish(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	realKV := s.client.KV
	fault := &lostReplyKV{KV: realKV}
	fault.drop.Store(true)
	s.client.KV = fault
	record := validActiveRecord("sandbox-2")
	err = repository.Publish(context.Background(), record)
	s.client.KV = realKV
	require.ErrorContains(t, err, "reply lost after commit")
	loaded, err := repository.Load(context.Background(), record.SandboxID)
	require.NoError(t, err)
	require.Equal(t, record, *loaded)
	require.ErrorIs(t, repository.Publish(context.Background(), record), state.ErrActiveSandboxConflict)
}

func TestActiveRecordMissingAndInvalid(t *testing.T) {
	s := testStore(t)
	repository, err := NewActiveSandboxRepository(s, "cell-a")
	require.NoError(t, err)
	loaded, err := repository.Load(context.Background(), "missing")
	require.NoError(t, err)
	require.Nil(t, loaded)
	_, err = repository.Load(context.Background(), "../invalid")
	require.ErrorIs(t, err, state.ErrActiveSandboxCorrupt)
	bad := validActiveRecord("sandbox-3")
	bad.Phase = state.ActiveSandboxActive
	require.True(t, errors.Is(repository.Publish(context.Background(), bad), state.ErrActiveSandboxCorrupt))
}
