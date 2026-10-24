package sandbox

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	etcdstate "github.com/goairix/sandbox/internal/storage/state/etcd"
)

func TestOrdinaryPoolRetiresOldScopedGenerationOnEtcd(t *testing.T) {
	endpoint := os.Getenv("TEST_ETCD_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_ETCD_ENDPOINT is not set")
	}
	ctx := context.Background()
	store, err := etcdstate.New(ctx, etcdstate.Options{
		Endpoints: []string{endpoint}, Prefix: "/ordinary-rollout-test/" + uuid.NewString() + "/",
		Timeout: 3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	rt := newSharedPoolRuntime()
	current := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:new"})
	current.EnableShared(store, "fresh-namespace")
	old := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:old"})
	old.EnableShared(store, "fresh-namespace")
	for _, entry := range []struct {
		pool  *sharedOrdinaryPool
		id    string
		state ordinaryPoolState
		age   time.Duration
	}{
		{current.shared, "current", ordinaryPoolPrepared, 0},
		{old.shared, "obsolete", ordinaryPoolPreparing, 3 * ordinaryPoolStaleTTL},
	} {
		record := ordinaryPoolRecord{
			PreparationID: entry.id, PoolKey: entry.pool.poolKey, State: entry.state,
			UpdatedAt: time.Now().UTC().Add(-entry.age), Revision: 1,
		}
		if entry.state == ordinaryPoolPrepared {
			record.RuntimeID, record.RuntimeUID = "current-pod", "current-uid"
		}
		raw, err := json.Marshal(record)
		require.NoError(t, err)
		require.NoError(t, store.Set(ctx, entry.pool.recordKey(entry.id), raw, 0))
	}
	require.NoError(t, current.shared.retireObsolete(ctx, 1))
	raw, err := store.Get(ctx, old.shared.recordKey("obsolete"))
	require.NoError(t, err)
	require.Empty(t, raw)
	raw, err = store.Get(ctx, current.shared.recordKey("current"))
	require.NoError(t, err)
	require.NotEmpty(t, raw)
}
