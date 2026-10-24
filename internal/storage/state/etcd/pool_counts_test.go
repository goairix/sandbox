package etcd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestPoolMetadataCountsEveryStateTransition(t *testing.T) {
	store := testStore(t)
	repo := NewFUSEPoolRepository(store)
	ctx := context.Background()
	assertCounts := func(want poolMetadata) {
		t.Helper()
		read, err := store.client.Get(ctx, repo.metadataKey("shared-pool"))
		require.NoError(t, err)
		require.Len(t, read.Kvs, 1)
		got, err := decodePoolMetadata(read.Kvs[0].Value, "shared-pool")
		require.NoError(t, err)
		want.PoolKey = "shared-pool"
		want.Generation = got.Generation
		require.Equal(t, want, got)
	}

	locked, err := repo.TryRefillLock(ctx, "shared-pool", "refill", time.Minute)
	require.NoError(t, err)
	require.True(t, locked)
	require.NoError(t, repo.CreatePreparingWithAdmission(ctx, preparingPoolRecord("preparation"), "refill", 1, time.Minute))
	assertCounts(poolMetadata{Total: 1, Preparing: 1})
	_, err = repo.BindPreparingRuntime(ctx, "preparation", "runtime", "uid", "refill", 1)
	require.NoError(t, err)
	_, err = repo.Transition(ctx, "preparation", state.FUSEPoolPreparing, state.FUSEPoolPrepared, "maintainer", 2)
	require.NoError(t, err)
	assertCounts(poolMetadata{Total: 1, Prepared: 1})
	reserved, err := repo.ReservePrepared(ctx, "shared-pool", "reservation", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	assertCounts(poolMetadata{Total: 1, Reserved: 1})
	binding, err := repo.Transition(ctx, "preparation", state.FUSEPoolReserved, state.FUSEPoolBinding, "reservation", reserved.Revision)
	require.NoError(t, err)
	assertCounts(poolMetadata{Total: 1, Binding: 1})
	consumed, err := repo.Transition(ctx, "preparation", state.FUSEPoolBinding, state.FUSEPoolConsumed, "reservation", binding.Revision)
	require.NoError(t, err)
	assertCounts(poolMetadata{Total: 1, Consumed: 1})
	claimed, err := repo.ClaimCleanup(ctx, "preparation", state.FUSEPoolConsumed, consumed.MaintainerToken,
		consumed.ReservationToken, consumed.Revision, consumed.RuntimeID, consumed.RuntimeUID, "cleanup", time.Minute)
	require.NoError(t, err)
	assertCounts(poolMetadata{Total: 1, Cleanup: 1})
	deleted, err := repo.DeleteCleanup(ctx, "preparation", "cleanup", claimed.Revision)
	require.NoError(t, err)
	require.True(t, deleted)
	assertCounts(poolMetadata{})
}
