package etcd

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestPoolStatePagesAreBoundedAndScoped(t *testing.T) {
	store := testStore(t)
	repo := NewFUSEPoolRepository(store)
	ctx := context.Background()
	for _, poolKey := range []string{"shared-pool", "other-pool"} {
		locked, err := repo.TryRefillLock(ctx, poolKey, "refill", 10*time.Second)
		require.NoError(t, err)
		require.True(t, locked)
	}
	for i := 0; i < 5; i++ {
		record := preparingPoolRecord(fmt.Sprintf("preparation-%d", i))
		if i == 4 {
			record.PoolKey = "other-pool"
		}
		require.NoError(t, repo.CreatePreparingWithAdmission(ctx, record, "refill", 5, time.Minute))
	}

	var ids []string
	var cursor string
	for {
		page, next, err := repo.ListStatePage(ctx, "shared-pool", state.FUSEPoolPreparing, cursor, 2)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page), 2)
		for _, record := range page {
			require.Equal(t, "shared-pool", record.PoolKey)
			require.Equal(t, state.FUSEPoolPreparing, record.State)
			ids = append(ids, record.PreparationID)
		}
		if next == "" {
			break
		}
		require.NotEqual(t, cursor, next)
		cursor = next
	}
	require.Len(t, ids, 4)
	require.ElementsMatch(t, []string{"preparation-0", "preparation-1", "preparation-2", "preparation-3"}, ids)

	page, next, err := repo.ListStatePage(ctx, "shared-pool", state.FUSEPoolConsumed, "", 2)
	require.NoError(t, err)
	require.Empty(t, page)
	require.Empty(t, next)
}

func TestPoolStatePageRejectsInvalidCursorAndLimit(t *testing.T) {
	repo := NewFUSEPoolRepository(testStore(t))
	for _, tc := range []struct {
		state  state.FUSEPoolState
		cursor string
		limit  int
	}{
		{state.FUSEPoolPreparing, "bad-cursor", 1},
		{state.FUSEPoolPreparing, "", 0},
		{state.FUSEPoolPreparing, "", 65},
		{"unknown", "", 1},
	} {
		_, _, err := repo.ListStatePage(context.Background(), "shared-pool", tc.state, tc.cursor, tc.limit)
		require.ErrorIs(t, err, state.ErrFUSEPoolInvalidRecord)
	}
}

func TestPoolStatePageRejectsIndexRecordDrift(t *testing.T) {
	store := testStore(t)
	repo := NewFUSEPoolRepository(store)
	ctx := context.Background()
	locked, err := repo.TryRefillLock(ctx, "shared-pool", "refill", 10*time.Second)
	require.NoError(t, err)
	require.True(t, locked)
	require.NoError(t, repo.CreatePreparingWithAdmission(ctx, preparingPoolRecord("preparation"), "refill", 1, time.Minute))

	_, err = store.client.Put(ctx, repo.preparingKey("shared-pool", "preparation"), "different-id")
	require.NoError(t, err)
	_, _, err = repo.ListStatePage(ctx, "shared-pool", state.FUSEPoolPreparing, "", 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolCorrupt)
}
