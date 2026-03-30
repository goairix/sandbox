package redis

import (
	"context"
	"os"
	"testing"
	"time"

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
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	s, err := New(context.Background(), Options{Addr: addr})
	require.NoError(t, err)
	return s
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
