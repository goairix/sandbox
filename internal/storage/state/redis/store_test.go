package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skipIfNoRedis(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set, skipping Redis integration test")
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	s, err := New(context.Background(), Options{Addr: addr})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func atomicTestKey(t *testing.T, s *Store, suffix string) string {
	t.Helper()
	key := fmt.Sprintf("test:atomic:%s:%s", uuid.NewString(), suffix)
	t.Cleanup(func() {
		require.NoError(t, s.Delete(context.Background(), key))
	})
	return key
}

func TestAtomicCompareAndSwapSuccessMismatchMissingAndTTL(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()
	key := atomicTestKey(t, s, "cas")

	require.NoError(t, s.Set(ctx, key, []byte{0x00, 0x2a, 0xff}, 0))
	swapped, err := s.CompareAndSwap(ctx, key, []byte{0x00, 0x2a, 0xff}, []byte("next"), 150*time.Millisecond)
	require.NoError(t, err)
	require.True(t, swapped)
	pttl, err := s.client.PTTL(ctx, key).Result()
	require.NoError(t, err)
	assert.Positive(t, pttl)
	assert.LessOrEqual(t, pttl, 150*time.Millisecond)
	require.Eventually(t, func() bool {
		got, getErr := s.Get(ctx, key)
		return getErr == nil && got == nil
	}, time.Second, 10*time.Millisecond)

	swapped, err = s.CompareAndSwap(ctx, key, []byte("next"), []byte("unused"), time.Minute)
	require.NoError(t, err)
	assert.False(t, swapped)

	require.NoError(t, s.Set(ctx, key, []byte("original"), 0))
	swapped, err = s.CompareAndSwap(ctx, key, []byte("wrong"), []byte("must-not-be-written"), time.Minute)
	require.NoError(t, err)
	assert.False(t, swapped)
	got, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("original"), got)

	swapped, err = s.CompareAndSwap(ctx, key, []byte("original"), []byte("persistent"), 0)
	require.NoError(t, err)
	require.True(t, swapped)
	pttl, err = s.client.PTTL(ctx, key).Result()
	require.NoError(t, err)
	assert.Equal(t, time.Duration(-1), pttl)
}

func TestAtomicCompareAndDeleteUsesOriginalBytes(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()
	key := atomicTestKey(t, s, "compare-delete")
	require.NoError(t, s.Set(ctx, key, []byte{0x00, 0xfe, 0xff}, 0))

	deleted, err := s.CompareAndDelete(ctx, key, []byte{0x00, 0xfe, 0x00})
	require.NoError(t, err)
	assert.False(t, deleted)

	deleted, err = s.CompareAndDelete(ctx, key, []byte{0x00, 0xfe, 0xff})
	require.NoError(t, err)
	assert.True(t, deleted)
	got, err := s.Get(ctx, key)
	require.NoError(t, err)
	assert.Nil(t, got)
	deleted, err = s.CompareAndDelete(ctx, key, []byte{0x00, 0xfe, 0xff})
	require.NoError(t, err)
	assert.False(t, deleted)
}

func TestAtomicIncrementAcrossClients(t *testing.T) {
	skipIfNoRedis(t)
	a, b := testStore(t), testStore(t)
	key := atomicTestKey(t, a, "increment")
	const perClient = 100

	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for _, s := range []*Store{a, b} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			for range perClient {
				_, err := store.Increment(context.Background(), key)
				if err != nil {
					errCh <- err
					return
				}
			}
		}(s)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		require.NoError(t, err)
	}
	got, err := a.Get(context.Background(), key)
	require.NoError(t, err)
	assert.Equal(t, []byte("200"), got)
}

func TestAtomicOperationsPropagateCanceledContext(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := s.CompareAndSwap(ctx, "test:atomic:canceled", []byte("a"), []byte("b"), 0)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.Canceled), err)
}

func TestRedisStore_SetAndGet(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()

	err := s.Set(ctx, "test:key1", []byte("value1"), time.Minute)
	require.NoError(t, err)

	val, err := s.Get(ctx, "test:key1")
	require.NoError(t, err)
	assert.Equal(t, []byte("value1"), val)

	// cleanup
	_ = s.Delete(ctx, "test:key1")
}

func TestRedisStore_GetNotFound(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()

	val, err := s.Get(ctx, "test:nonexistent")
	require.NoError(t, err)
	assert.Nil(t, val)
}

func TestRedisStore_Delete(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()

	_ = s.Set(ctx, "test:del", []byte("x"), time.Minute)
	err := s.Delete(ctx, "test:del")
	require.NoError(t, err)

	exists, err := s.Exists(ctx, "test:del")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestRedisStore_SetNX(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()

	// cleanup first
	_ = s.Delete(ctx, "test:nx")

	ok, err := s.SetNX(ctx, "test:nx", []byte("first"), time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = s.SetNX(ctx, "test:nx", []byte("second"), time.Minute)
	require.NoError(t, err)
	assert.False(t, ok)

	val, err := s.Get(ctx, "test:nx")
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), val)

	_ = s.Delete(ctx, "test:nx")
}
