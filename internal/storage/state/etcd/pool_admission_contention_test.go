package etcd

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage/state"
)

func TestPoolAdmissionRetriesIndependentAggregateContention(t *testing.T) {
	store := testStore(t)
	repository := NewFUSEPoolRepository(store)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 10*time.Second)
	require.NoError(t, err)
	require.True(t, won)

	realKV := store.client.KV
	barrier := &poolAggregateBarrierKV{KV: realKV, gate: make(chan struct{})}
	store.client.KV = barrier
	t.Cleanup(func() { store.client.KV = realKV })

	results := make(chan error, 2)
	var workers sync.WaitGroup
	for i := 0; i < 2; i++ {
		workers.Add(1)
		go func(id string) {
			defer workers.Done()
			results <- repository.CreatePreparingWithAdmission(ctx,
				preparingPoolRecord(id), "holder", 2, time.Minute)
		}(fmt.Sprintf("preparation-%d", i))
	}
	workers.Wait()
	close(results)
	for admissionErr := range results {
		require.NoError(t, admissionErr)
	}

	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Equal(t, 2, count)
}

func TestPoolAdmissionDoesNotRetryLostCommitReply(t *testing.T) {
	store := testStore(t)
	repository := NewFUSEPoolRepository(store)
	ctx := context.Background()
	won, err := repository.TryRefillLock(ctx, "shared-pool", "holder", 10*time.Second)
	require.NoError(t, err)
	require.True(t, won)

	realKV := store.client.KV
	lostReply := &checkoutLostReplyKV{KV: realKV}
	store.client.KV = lostReply
	t.Cleanup(func() { store.client.KV = realKV })
	err = repository.CreatePreparingWithAdmission(ctx, preparingPoolRecord("preparation"),
		"holder", 2, time.Minute)
	require.ErrorContains(t, err, "reply lost after commit")
	require.EqualValues(t, 1, lostReply.commits.Load())

	record, err := repository.ReadByPreparationID(ctx, "preparation")
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolPreparing, record.State)
	count, err := repository.CountPreparingAndPrepared(ctx, "shared-pool")
	require.NoError(t, err)
	require.Equal(t, 1, count)
}
