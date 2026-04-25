package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
)

const sandboxKeyPrefix = "sandbox:"

// SessionStore manages persistent sandbox state using a state.Store backend.
type SessionStore struct {
	store state.Store
	ttl   time.Duration
}

// NewSessionStore creates a new SessionStore.
func NewSessionStore(store state.Store, ttl time.Duration) *SessionStore {
	return &SessionStore{store: store, ttl: ttl}
}

// Save persists a sandbox to the store.
// The Redis key TTL is derived from the sandbox's own timeout:
//   - Timeout <= 0 (never expire): uses TTL=0 so the key never expires
//   - Timeout > 0: uses the remaining lifetime as TTL
func (s *SessionStore) Save(ctx context.Context, sb *Sandbox) error {
	data, err := json.Marshal(sb)
	if err != nil {
		return fmt.Errorf("marshal sandbox: %w", err)
	}

	ttl := s.ttl // fallback to global default
	if sb.Timeout <= 0 {
		ttl = 0 // never expire
	} else {
		remaining := sb.Timeout - time.Since(sb.CreatedAt)
		if remaining > 0 {
			ttl = remaining
		}
	}

	return s.store.Set(ctx, sandboxKeyPrefix+sb.ID, data, ttl)
}

// Load retrieves a sandbox from the store.
func (s *SessionStore) Load(ctx context.Context, id string) (*Sandbox, error) {
	data, err := s.store.Get(ctx, sandboxKeyPrefix+id)
	if err != nil {
		return nil, fmt.Errorf("get sandbox: %w", err)
	}
	if data == nil {
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
	var sb Sandbox
	if err := json.Unmarshal(data, &sb); err != nil {
		return nil, fmt.Errorf("unmarshal sandbox: %w", err)
	}
	return &sb, nil
}

// Remove deletes a sandbox from the store.
func (s *SessionStore) Remove(ctx context.Context, id string) error {
	return s.store.Delete(ctx, sandboxKeyPrefix+id)
}

// List returns all sandbox IDs in the store.
func (s *SessionStore) List(ctx context.Context) ([]string, error) {
	keys, err := s.store.Keys(ctx, sandboxKeyPrefix+"*")
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(keys))
	for i, key := range keys {
		ids[i] = key[len(sandboxKeyPrefix):]
	}
	return ids, nil
}

// Exists checks if a sandbox exists in the store.
func (s *SessionStore) Exists(ctx context.Context, id string) (bool, error) {
	return s.store.Exists(ctx, sandboxKeyPrefix+id)
}
