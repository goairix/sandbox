package sandbox

import (
	"context"
	"os"
	"testing"
	"time"

	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWarmPoolRolloutWithRealRedis(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set")
	}
	ctx := context.Background()
	// Dedicated DB avoids interference with Redis repository test packages.
	store, err := redisstate.New(ctx, redisstate.Options{Addr: addr, DB: 14})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	scope := "rollout-real-" + randSuffix(24)
	t.Run("ordinary_restart_and_contract_retirement", func(t *testing.T) {
		rt := newSharedPoolRuntime()
		cfg := PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"}
		first := NewPool(rt, cfg)
		first.EnableShared(store, scope)
		require.NoError(t, first.Start(ctx))
		require.NoError(t, first.WarmUp(ctx))
		original := rt.snapshotIDs()
		first.Drain(ctx)
		second := NewPool(rt, cfg)
		second.EnableShared(store, scope)
		require.NoError(t, second.Start(ctx))
		require.NoError(t, second.WarmUp(ctx))
		assert.Equal(t, original, rt.snapshotIDs())
		second.Drain(ctx)
		cfg.Image = "sandbox:v2"
		third := NewPool(rt, cfg)
		third.EnableShared(store, scope)
		t.Cleanup(func() { third.Drain(ctx); require.NoError(t, third.DrainRelease(ctx)) })
		require.NoError(t, third.Start(ctx))
		require.NoError(t, third.WarmUp(ctx))
		for id := range original {
			_, exists := rt.snapshotIDs()[id]
			assert.False(t, exists)
		}
		assert.Equal(t, 1, third.Size())
	})
	t.Run("fuse_restart_and_contract_retirement", func(t *testing.T) {
		rt, repo := newFUSEMockRuntime(), redisstate.NewFUSEPoolRepository(store)
		cfg := fusePoolConfig()
		cfg.InventoryStore, cfg.InventoryScope, cfg.RefillInterval = store, scope, time.Hour
		spec := fixedFUSESpec("")
		spec.PoolContract = scope
		key, err := ComputeFUSEPoolKey(spec)
		require.NoError(t, err)
		spec.WorkspaceFUSE.PoolKey = key
		first := NewFUSEPool(rt, repo, cfg, spec)
		require.NoError(t, first.Start(ctx))
		original, err := repo.ListByPoolKey(ctx, key)
		require.NoError(t, err)
		require.Len(t, original, 1)
		require.NoError(t, first.Stop(ctx))
		cfg.MaintainerToken = "api-b"
		second := NewFUSEPool(rt, repo, cfg, spec)
		require.NoError(t, second.Start(ctx))
		require.NoError(t, second.Stop(ctx))
		current, err := repo.ListByPoolKey(ctx, key)
		require.NoError(t, err)
		require.Len(t, current, 1)
		assert.Equal(t, original[0].RuntimeUID, current[0].RuntimeUID)
		cfg.MaintainerToken, spec.Image = "api-c", "sandbox:v2"
		newKey, err := ComputeFUSEPoolKey(spec)
		require.NoError(t, err)
		spec.WorkspaceFUSE.PoolKey = newKey
		third := NewFUSEPool(rt, repo, cfg, spec)
		t.Cleanup(func() { require.NoError(t, third.DrainRelease(ctx)) })
		require.NoError(t, third.Start(ctx))
		remaining, err := repo.ListByPoolKey(ctx, key)
		require.NoError(t, err)
		assert.Empty(t, remaining)
		assert.True(t, rt.wasRemoved(original[0].RuntimeID))
	})
}
