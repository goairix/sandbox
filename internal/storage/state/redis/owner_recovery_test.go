package redis

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestOwnerRecoveryAtomicallyRejectsRenewedLease(t *testing.T) {
	skipIfNoRedis(t)
	store := testStore(t)
	ctx := context.Background()
	base := "owner-recovery:{" + uuid.NewString() + "}:"
	ownerKey, leaseKey := base+"owner", base+"lease"
	t.Cleanup(func() {
		require.NoError(t, store.Delete(ctx, ownerKey))
		require.NoError(t, store.Delete(ctx, leaseKey))
	})
	require.NoError(t, store.Set(ctx, ownerKey, []byte("old-owner"), 0))
	require.NoError(t, store.Set(ctx, leaseKey, []byte("renewed"), time.Minute))
	deleted, err := store.CompareAndDeleteIfAbsent(ctx, ownerKey, []byte("old-owner"), leaseKey)
	require.NoError(t, err)
	require.False(t, deleted)
	require.NoError(t, store.Delete(ctx, leaseKey))
	deleted, err = store.CompareAndDeleteIfAbsent(ctx, ownerKey, []byte("wrong-owner"), leaseKey)
	require.NoError(t, err)
	require.False(t, deleted)
	deleted, err = store.CompareAndDeleteIfAbsent(ctx, ownerKey, []byte("old-owner"), leaseKey)
	require.NoError(t, err)
	require.True(t, deleted)
}
