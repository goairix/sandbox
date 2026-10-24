package etcd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestScopedOrdinaryPoolOwnerReadUsesExactScopeAndPool(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	scope := strings.Repeat("a", 16)
	pool := strings.Repeat("b", 32)
	otherPool := strings.Repeat("c", 32)
	key := "ordinarypool:v1:" + scope + ":" + pool + ":owner:token"
	require.NoError(t, store.Set(ctx, key, []byte("token"), time.Minute))

	found, err := store.HasScopedOrdinaryPoolOwner(ctx, scope, pool)
	require.NoError(t, err)
	require.True(t, found)
	found, err = store.HasScopedOrdinaryPoolOwner(ctx, scope, otherPool)
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, store.Delete(ctx, key))
	found, err = store.HasScopedOrdinaryPoolOwner(ctx, scope, pool)
	require.NoError(t, err)
	require.False(t, found)
}
