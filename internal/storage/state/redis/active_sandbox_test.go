package redis

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func activeRepositoryForTest(t *testing.T) (*ActiveSandboxRepository, string) {
	t.Helper()
	skipIfNoRedis(t)
	store := testStore(t)
	scope := "scope-" + uuid.NewString()
	repo, err := NewActiveSandboxRepository(store, scope)
	require.NoError(t, err)
	return repo, scope
}

func activeRecordForTest(id string) state.ActiveSandboxRecord {
	now := time.Now().UTC()
	snapshot, _ := json.Marshal(map[string]any{"id": id, "runtime_id": "pod-" + id, "runtime_uid": "uid-" + id})
	return state.ActiveSandboxRecord{Version: 1, SandboxID: id, Phase: state.ActiveSandboxPublishing,
		Revision: 1, Generation: 1, RuntimeID: "pod-" + id, RuntimeUID: "uid-" + id,
		Snapshot: snapshot, CreatedAt: now, UpdatedAt: now}
}

func TestActiveSandboxRepositoryPublishActivateAndKeySlot(t *testing.T) {
	repo, _ := activeRepositoryForTest(t)
	ctx := context.Background()
	record := activeRecordForTest("sandbox-a")
	require.NoError(t, repo.Publish(ctx, record))
	assert.ErrorIs(t, repo.Publish(ctx, record), state.ErrActiveSandboxConflict)

	loaded, err := repo.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	require.Equal(t, record.RuntimeUID, loaded.RuntimeUID)

	active, err := repo.Activate(ctx, record.SandboxID, record.Revision, record.Snapshot)
	require.NoError(t, err)
	assert.Equal(t, state.ActiveSandboxActive, active.Phase)
	assert.Equal(t, uint64(2), active.Revision)

	keys := repo.keys(record.SandboxID)
	tag := "{" + repo.scopeDigest + ":" + record.SandboxID + "}"
	for _, key := range []string{keys.record, keys.operations, keys.mutation, keys.controller} {
		assert.Contains(t, key, tag)
	}
}

func TestActiveSandboxRepositoryOperationAndDestroyFencing(t *testing.T) {
	repo, _ := activeRepositoryForTest(t)
	ctx := context.Background()
	record := activeRecordForTest("sandbox-b")
	require.NoError(t, repo.Publish(ctx, record))
	_, err := repo.Activate(ctx, record.SandboxID, 1, record.Snapshot)
	require.NoError(t, err)

	_, data, err := repo.BeginOperation(ctx, record.SandboxID, "data-token", state.ActiveOperationData, time.Second)
	require.NoError(t, err)
	_, mutation, err := repo.BeginOperation(ctx, record.SandboxID, "mutation-token", state.ActiveOperationMutation, time.Second)
	require.NoError(t, err)
	_, _, err = repo.BeginOperation(ctx, record.SandboxID, "second-mutation", state.ActiveOperationMutation, time.Second)
	assert.ErrorIs(t, err, state.ErrActiveSandboxConflict)

	destroying, live, won, err := repo.BeginDestroy(ctx, record.SandboxID)
	require.NoError(t, err)
	assert.True(t, won)
	assert.Equal(t, int64(2), live)
	assert.Equal(t, state.ActiveSandboxDestroying, destroying.Phase)
	_, _, err = repo.BeginOperation(ctx, record.SandboxID, "late", state.ActiveOperationData, time.Second)
	assert.ErrorIs(t, err, state.ErrActiveSandboxAdmissionClosed)

	_, err = repo.RenewOperation(ctx, *data, time.Second)
	require.NoError(t, err)
	require.NoError(t, repo.EndOperation(ctx, *data))
	require.NoError(t, repo.EndOperation(ctx, *mutation))
	live, err = repo.LiveOperations(ctx, record.SandboxID)
	require.NoError(t, err)
	assert.Zero(t, live)

	require.NoError(t, repo.Delete(ctx, record.SandboxID, destroying.Revision, destroying.Generation))
	loaded, err := repo.Load(ctx, record.SandboxID)
	require.NoError(t, err)
	assert.Nil(t, loaded)
}

func TestActiveSandboxRepositoryExpiresOperationsAndFencesGeneration(t *testing.T) {
	repo, _ := activeRepositoryForTest(t)
	ctx := context.Background()
	record := activeRecordForTest("sandbox-c")
	require.NoError(t, repo.Publish(ctx, record))
	_, err := repo.Activate(ctx, record.SandboxID, 1, record.Snapshot)
	require.NoError(t, err)

	_, operation, err := repo.BeginOperation(ctx, record.SandboxID, "short", state.ActiveOperationData, 20*time.Millisecond)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		live, countErr := repo.LiveOperations(ctx, record.SandboxID)
		return countErr == nil && live == 0
	}, time.Second, 10*time.Millisecond)
	_, err = repo.RenewOperation(ctx, *operation, time.Second)
	assert.ErrorIs(t, err, state.ErrActiveSandboxStaleToken)
}

func TestActiveSandboxRepositoryControllerAndScan(t *testing.T) {
	repo, _ := activeRepositoryForTest(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		record := activeRecordForTest(fmt.Sprintf("sandbox-scan-%d", i))
		require.NoError(t, repo.Publish(ctx, record))
		_, err := repo.Activate(ctx, record.SandboxID, record.Revision, record.Snapshot)
		require.NoError(t, err)
		t.Cleanup(func() { _ = repo.forceDelete(context.Background(), record.SandboxID) })
	}
	page, err := repo.Scan(ctx, 0, 2)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(page.Records), 2)
	assert.NotEmpty(t, page.Records)

	now := time.Now().UTC()
	lease := state.ActiveSandboxControllerLease{SandboxID: "sandbox-scan-0", Token: "controller-a", InstanceID: "api-a", PodUID: "pod-a", Generation: 1, ExpiresAt: now.Add(time.Second)}
	acquired, ok, err := repo.AcquireController(ctx, lease, time.Second)
	require.NoError(t, err)
	assert.True(t, ok)
	other := lease
	other.Token = "controller-b"
	_, ok, err = repo.AcquireController(ctx, other, time.Second)
	require.NoError(t, err)
	assert.False(t, ok)
	renewed, err := repo.RenewController(ctx, *acquired, time.Second)
	require.NoError(t, err)
	assert.True(t, renewed.ExpiresAt.After(acquired.ExpiresAt))
	assert.ErrorIs(t, repo.ReleaseController(ctx, other), state.ErrActiveSandboxStaleToken)
	require.NoError(t, repo.ReleaseController(ctx, *renewed))
}

func TestActiveSandboxRepositoryReportsUnconfirmedReplicaAck(t *testing.T) {
	skipIfNoRedis(t)
	store, err := New(context.Background(), Options{
		Addr: os.Getenv("TEST_REDIS_ADDR"), Durability: DurabilityReplicaAck,
		AckReplicas: 1, AckTimeout: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	repo, err := NewActiveSandboxRepository(store, "ack-"+uuid.NewString())
	require.NoError(t, err)
	record := activeRecordForTest("sandbox-ack")
	err = repo.Publish(context.Background(), record)
	assert.ErrorIs(t, err, state.ErrDurabilityUnconfirmed)
	loaded, loadErr := repo.Load(context.Background(), record.SandboxID)
	require.NoError(t, loadErr)
	require.NotNil(t, loaded, "an unconfirmed reply is ambiguous, not proof that the write failed")
	t.Cleanup(func() { _ = repo.forceDelete(context.Background(), record.SandboxID) })
}
