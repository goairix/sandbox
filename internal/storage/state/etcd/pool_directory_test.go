package etcd

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestPoolDirectoryListsKeysAndDrainsExactRefillLocks(t *testing.T) {
	store := testStore(t)
	repo := NewFUSEPoolRepository(store)
	ctx := context.Background()
	for _, poolKey := range []string{"pool-a", "pool-b"} {
		locked, err := repo.TryRefillLock(ctx, poolKey, "refill", time.Minute)
		require.NoError(t, err)
		require.True(t, locked)
		record := preparingPoolRecord(poolKey + "-preparation")
		record.PoolKey = poolKey
		require.NoError(t, repo.CreatePreparingWithAdmission(ctx, record, "refill", 1, time.Minute))
	}
	poolKeys, err := repo.ListPoolKeys(ctx)
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"pool-a", "pool-b"}, poolKeys)
	require.NoError(t, repo.DrainRefillLocks(ctx))
	for _, poolKey := range poolKeys {
		lock, err := store.client.Get(ctx, repo.refillKey(poolKey))
		require.NoError(t, err)
		require.Empty(t, lock.Kvs)
	}
}

func TestPoolLegacyFullListAndServerTimeFailClosed(t *testing.T) {
	repo := NewFUSEPoolRepository(testStore(t))
	_, err := repo.ListByPoolKey(context.Background(), "shared-pool")
	require.True(t, errors.Is(err, state.ErrFUSEPoolUnboundedRead))
	_, err = repo.ServerTime(context.Background())
	require.True(t, errors.Is(err, state.ErrFUSEPoolNoServerTime))
}

func TestPoolDirectoryPagesAndRejectsWrongDigest(t *testing.T) {
	store := testStore(t)
	repo := NewFUSEPoolRepository(store)
	ctx := context.Background()
	for i := 0; i < 67; i++ {
		poolKey := fmt.Sprintf("pool-%03d", i)
		_, err := store.client.Put(ctx, repo.directoryKey(poolKey), poolKey)
		require.NoError(t, err)
	}
	keys, err := repo.ListPoolKeys(ctx)
	require.NoError(t, err)
	require.Len(t, keys, 67)
	_, err = store.client.Put(ctx, repo.directoryKey("wrong-key"), "different-key")
	require.NoError(t, err)
	_, err = repo.ListPoolKeys(ctx)
	require.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
}

func TestPoolDirectoryIncludesRefillBeforeFirstRecord(t *testing.T) {
	store := testStore(t)
	repo := NewFUSEPoolRepository(store)
	ctx := context.Background()
	locked, err := repo.TryRefillLock(ctx, "new-pool", "refill", time.Minute)
	require.NoError(t, err)
	require.True(t, locked)
	keys, err := repo.ListPoolKeys(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"new-pool"}, keys)
	require.NoError(t, repo.DrainRefillLocks(ctx))
	lock, err := store.client.Get(ctx, repo.refillKey("new-pool"))
	require.NoError(t, err)
	require.Empty(t, lock.Kvs)
}

func TestPoolRefillDoesNotSilentlyRepairMissingDirectory(t *testing.T) {
	store := testStore(t)
	repo := NewFUSEPoolRepository(store)
	ctx := context.Background()
	locked, err := repo.TryRefillLock(ctx, "shared-pool", "first", time.Minute)
	require.NoError(t, err)
	require.True(t, locked)
	require.NoError(t, repo.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation"), "first", 1, time.Minute))
	require.NoError(t, repo.UnlockRefill(ctx, "shared-pool", "first"))
	_, err = store.client.Delete(ctx, repo.directoryKey("shared-pool"))
	require.NoError(t, err)
	locked, err = repo.TryRefillLock(ctx, "shared-pool", "second", time.Minute)
	require.False(t, locked)
	require.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
}
