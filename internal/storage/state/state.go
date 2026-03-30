package state

import (
	"context"
	"time"
)

// Store is the abstraction over state storage backends (sandbox sessions, pool state).
type Store interface {
	// Set stores a value with optional TTL. Pass 0 for no expiration.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Get retrieves a value. Returns nil, nil if key does not exist.
	Get(ctx context.Context, key string) ([]byte, error)
	// Delete removes a key.
	Delete(ctx context.Context, key string) error
	// Exists checks if a key exists.
	Exists(ctx context.Context, key string) (bool, error)
	// SetNX sets a value only if the key does not exist (for distributed locks).
	// Returns true if the key was set.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	// Keys returns all keys matching a pattern (e.g. "sandbox:*").
	Keys(ctx context.Context, pattern string) ([]string, error)
}
