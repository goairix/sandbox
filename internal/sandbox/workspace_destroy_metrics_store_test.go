package sandbox

import (
	"context"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
)

// Keep workspace scans and fixed prepared-runtime fixture identities isolated
// across simultaneously running memory/Redis regression processes.
type workspaceMetricsScopedStore struct {
	store  state.AtomicStore
	prefix string
}

func (s workspaceMetricsScopedStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return s.store.Set(ctx, s.prefix+key, value, ttl)
}

func (s workspaceMetricsScopedStore) Get(ctx context.Context, key string) ([]byte, error) {
	return s.store.Get(ctx, s.prefix+key)
}

func (s workspaceMetricsScopedStore) Delete(ctx context.Context, key string) error {
	return s.store.Delete(ctx, s.prefix+key)
}

func (s workspaceMetricsScopedStore) Exists(ctx context.Context, key string) (bool, error) {
	return s.store.Exists(ctx, s.prefix+key)
}

func (s workspaceMetricsScopedStore) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	return s.store.SetNX(ctx, s.prefix+key, value, ttl)
}

func (s workspaceMetricsScopedStore) Keys(ctx context.Context, pattern string) ([]string, error) {
	keys, err := s.store.Keys(ctx, s.prefix+pattern)
	if err != nil {
		return nil, err
	}
	for index, key := range keys {
		keys[index] = strings.TrimPrefix(key, s.prefix)
	}
	return keys, nil
}

func (s workspaceMetricsScopedStore) CompareAndSwap(ctx context.Context, key string, oldValue, newValue []byte, ttl time.Duration) (bool, error) {
	return s.store.CompareAndSwap(ctx, s.prefix+key, oldValue, newValue, ttl)
}

func (s workspaceMetricsScopedStore) CompareAndDelete(ctx context.Context, key string, expected []byte) (bool, error) {
	return s.store.CompareAndDelete(ctx, s.prefix+key, expected)
}

func (s workspaceMetricsScopedStore) Increment(ctx context.Context, key string) (int64, error) {
	return s.store.Increment(ctx, s.prefix+key)
}
