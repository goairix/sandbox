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

func TestPoolBindPreparingRuntimeFencesUIDOwner(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	for _, id := range []string{"preparation-a", "preparation-b"} {
		require.NoError(t, repository.CreatePreparingWithAdmission(ctx, preparingPoolRecord(id), "holder", 2, 10*time.Second))
	}
	_, err = repository.BindPreparingRuntime(ctx, "preparation-a", "runtime-a", "uid-a", "wrong", 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolTokenMismatch)
	bound, err := repository.BindPreparingRuntime(ctx, "preparation-a", "runtime-a", "uid-a", "holder", 1)
	require.NoError(t, err)
	require.Equal(t, uint64(2), bound.Revision)
	require.Equal(t, "uid-a", bound.RuntimeUID)
	_, err = repository.BindPreparingRuntime(ctx, "preparation-a", "runtime-a", "uid-a", "holder", 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolCASMismatch)
	_, err = repository.BindPreparingRuntime(ctx, "preparation-b", "runtime-b", "uid-a", "holder", 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolConflict)
	stillUnbound, err := repository.ReadByPreparationID(ctx, "preparation-b")
	require.NoError(t, err)
	require.Empty(t, stillUnbound.RuntimeUID)
}

func TestPoolBindRejectsExpiredPreparationLease(t *testing.T) {
	s := testStore(t)
	repository := NewFUSEPoolRepository(s)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 5*time.Second)
	require.NoError(t, err)
	require.True(t, won)
	require.NoError(t, repository.CreatePreparingWithAdmission(ctx,
		preparingPoolRecord("expiring-preparation"), "holder", 1, time.Second))
	require.Eventually(t, func() bool {
		read, err := s.client.Get(ctx, repository.prepareDeadlineKey("expiring-preparation"))
		return err == nil && len(read.Kvs) == 0
	}, 4*time.Second, 50*time.Millisecond)
	_, err = repository.BindPreparingRuntime(ctx, "expiring-preparation", "runtime", "uid", "holder", 1)
	require.ErrorIs(t, err, state.ErrFUSEPoolCASMismatch)
}
