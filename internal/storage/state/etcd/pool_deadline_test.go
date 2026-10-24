package etcd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestPoolDeadlineUsesServerLeaseAndFencesSnapshot(t *testing.T) {
	store := testStore(t)
	repo := NewFUSEPoolRepository(store)
	ctx := context.Background()
	locked, err := repo.TryRefillLock(ctx, "shared-pool", "refill", 10*time.Second)
	require.NoError(t, err)
	require.True(t, locked)
	require.NoError(t, repo.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("preparation"), "refill", 1, time.Second))
	record, err := repo.ReadByPreparationID(ctx, "preparation")
	require.NoError(t, err)
	require.NotNil(t, record)
	live, err := repo.DeadlineLive(ctx, *record)
	require.NoError(t, err)
	require.True(t, live)
	require.Eventually(t, func() bool {
		live, err := repo.DeadlineLive(ctx, *record)
		return err == nil && !live
	}, 4*time.Second, 50*time.Millisecond)

	_, err = repo.ClaimCleanup(ctx, "preparation", state.FUSEPoolPreparing,
		"maintainer", "", record.Revision, "", "", "cleanup", 5*time.Second)
	require.NoError(t, err)
	_, err = repo.DeadlineLive(ctx, *record)
	require.ErrorIs(t, err, state.ErrFUSEPoolCASMismatch)
}
