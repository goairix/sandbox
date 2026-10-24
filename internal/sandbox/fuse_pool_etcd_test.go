package sandbox

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
	etcdstate "github.com/goairix/sandbox/internal/storage/state/etcd"
)

func TestFUSEPoolBusinessColdAcquireReturnAndDrainOnEtcd(t *testing.T) {
	endpoint := os.Getenv("TEST_ETCD_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_ETCD_ENDPOINT is not set")
	}
	store, err := etcdstate.New(context.Background(), etcdstate.Options{
		Endpoints: []string{endpoint}, Prefix: "/sandbox-pool-business-test/" + uuid.NewString() + "/",
		Timeout: 3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	repo := etcdstate.NewFUSEPoolRepository(store)
	cfg := fusePoolConfig()
	cfg.MinSize, cfg.MaxSize = 0, 1
	cfg.PrepareTimeout = 10 * time.Second
	cfg.RefillInterval = time.Minute
	pool := NewFUSEPool(newFUSEMockRuntime(), repo, cfg, fixedFUSESpec("pool-key"))
	ctx := context.Background()
	require.NoError(t, pool.Reconcile(ctx))

	reserved, err := pool.Acquire(ctx, "pool-key")
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolReserved, reserved.State)
	inventory, err := repo.ReadInventory(ctx, "pool-key")
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolInventory{Reserved: 1}, inventory)

	require.NoError(t, pool.ReturnPrepared(ctx, *reserved))
	inventory, err = repo.ReadInventory(ctx, "pool-key")
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolInventory{Prepared: 1}, inventory)
	require.NoError(t, pool.Drain(ctx, "pool-key"))
	inventory, err = repo.ReadInventory(ctx, "pool-key")
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolInventory{}, inventory)
	require.NoError(t, pool.Stop(ctx))
}
