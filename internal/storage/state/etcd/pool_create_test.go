package etcd

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func preparingPoolRecord(id string) state.FUSEPoolRecord {
	return state.FUSEPoolRecord{
		PreparationID: id, PoolKey: "shared-pool", State: state.FUSEPoolPreparing,
		MaintainerToken: "maintainer", Revision: 1,
	}
}

func TestPoolCreateAdmissionNeverOverbooks(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	otherStore, err := New(context.Background(), Options{
		Endpoints: []string{os.Getenv("TEST_ETCD_ENDPOINT")}, Prefix: s.prefix, Timeout: 3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherStore.Close()) })
	otherRepository := NewFUSEPoolRepository(otherStore)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "refill", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for id, candidate := range map[string]*FUSEPoolRepository{
		"preparation-a": repository, "preparation-b": otherRepository,
	} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			results <- candidate.CreatePreparingWithAdmission(ctx, preparingPoolRecord(id), "refill", 1, 10*time.Second)
		}(id)
	}
	wg.Wait()
	close(results)
	var created, rejected int
	for err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, state.ErrFUSEPoolConflict):
			rejected++
		default:
			t.Fatalf("unexpected admission result: %v", err)
		}
	}
	require.Equal(t, 1, created)
	require.Equal(t, 1, rejected)
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Equal(t, 1, count)
	err = otherRepository.CreatePreparingWithAdmission(ctx, preparingPoolRecord("preparation-c"), "wrong", 1, 10*time.Second)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)

	first, err := repository.ReadByPreparationID(ctx, "preparation-a")
	require.NoError(t, err)
	second, err := repository.ReadByPreparationID(ctx, "preparation-b")
	require.NoError(t, err)
	require.NotEqual(t, first == nil, second == nil)
}

func TestPoolCreateRequiresLiveRefillLock(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	err := repository.CreatePreparingWithAdmission(ctx, preparingPoolRecord("preparation-a"), "missing", 1, 5*time.Second)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", time.Second)
	require.NoError(t, err)
	require.True(t, won)
	err = repository.CreatePreparingWithAdmission(ctx, preparingPoolRecord("preparation-a"), "other", 1, 5*time.Second)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	require.NoError(t, repository.UnlockRefill(ctx, "shared-pool", "holder"))
	err = repository.CreatePreparingWithAdmission(ctx, preparingPoolRecord("preparation-a"), "holder", 1, 5*time.Second)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
}

func TestPoolDuplicateCreateDoesNotIncreaseCapacityCount(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	record := preparingPoolRecord("same-preparation")
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx, record, "holder", 2, 10*time.Second))
	require.ErrorIs(t,
		repository.CreatePreparingWithAdmission(ctx, record, "holder", 2, 10*time.Second),
		state.ErrFUSEPoolConflict)
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Equal(t, 1, count)
}

func TestPoolRefillRenewKeepsOneLiveHolder(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", time.Second)
	require.NoError(t, err)
	require.True(t, won)
	won, err = repository.RenewRefillLock(ctx, "shared-pool", "wrong", 5*time.Second)
	require.NoError(t, err)
	require.False(t, won)
	won, err = repository.RenewRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	time.Sleep(2200 * time.Millisecond)
	won, err = repository.TryRefillLock(ctx, "shared-pool", "other", 5*time.Second)
	require.NoError(t, err)
	require.False(t, won)
	require.ErrorIs(t, repository.UnlockRefill(ctx, "shared-pool", "other"), state.ErrFUSEPoolTokenMismatch)
	require.NoError(t, repository.UnlockRefill(ctx, "shared-pool", "holder"))
}
