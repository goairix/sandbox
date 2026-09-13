package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
)

const (
	sandboxSessionKeyPrefix       = "sandbox:session:v2:"
	legacySandboxSessionKeyPrefix = "sandbox:"
)

var ErrSessionPublicationConflict = errors.New("sandbox session publication changed")

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

	return s.store.Set(ctx, sandboxSessionKeyPrefix+sb.ID, data, ttl)
}

// Load retrieves a sandbox from the store.
func (s *SessionStore) Load(ctx context.Context, id string) (*Sandbox, error) {
	data, err := s.store.Get(ctx, sandboxSessionKeyPrefix+id)
	if err != nil {
		return nil, fmt.Errorf("get sandbox: %w", err)
	}
	if data == nil {
		return s.loadAndMigrateLegacy(ctx, id)
	}
	var sb Sandbox
	if err := json.Unmarshal(data, &sb); err != nil {
		return nil, fmt.Errorf("unmarshal sandbox: %w", err)
	}
	return &sb, nil
}

// LoadReadOnly checks both current and legacy sessions without migration.
// Audits must conservatively retain an owner if either format is present.
func (s *SessionStore) LoadReadOnly(ctx context.Context, id string) (*Sandbox, error) {
	if s == nil || s.store == nil {
		return nil, fmt.Errorf("session store is not configured")
	}
	if id == "" {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	for _, prefix := range []string{sandboxSessionKeyPrefix, legacySandboxSessionKeyPrefix} {
		data, err := s.store.Get(ctx, prefix+id)
		if err != nil {
			return nil, fmt.Errorf("get sandbox session: %w", err)
		}
		if data == nil {
			continue
		}
		var sb Sandbox
		if err := json.Unmarshal(data, &sb); err != nil || sb.ID != id || sb.RuntimeID == "" || sb.CreatedAt.IsZero() {
			return nil, fmt.Errorf("invalid sandbox session: %s", id)
		}
		return &sb, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
}

func (s *SessionStore) loadAndMigrateLegacy(ctx context.Context, id string) (*Sandbox, error) {
	if id == "" {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	atomicStore, ok := s.store.(state.AtomicStore)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	legacyKey := legacySandboxSessionKeyPrefix + id
	raw, err := s.store.Get(ctx, legacyKey)
	if err != nil {
		return nil, fmt.Errorf("get legacy sandbox: %w", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	var sb Sandbox
	if err := json.Unmarshal(raw, &sb); err != nil || sb.ID != id || sb.RuntimeID == "" || sb.CreatedAt.IsZero() {
		return nil, fmt.Errorf("invalid legacy sandbox session: %s", id)
	}
	if err := s.Save(ctx, &sb); err != nil {
		return nil, fmt.Errorf("migrate legacy sandbox session: %w", err)
	}
	deleted, err := atomicStore.CompareAndDelete(ctx, legacyKey, raw)
	if err != nil || !deleted {
		cleanupErr := s.RemoveExact(ctx, &sb)
		return nil, errors.Join(ErrSessionPublicationConflict, err, cleanupErr)
	}
	return &sb, nil
}

// Remove deletes a sandbox from the store.
func (s *SessionStore) Remove(ctx context.Context, id string) error {
	return s.store.Delete(ctx, sandboxSessionKeyPrefix+id)
}

// RemoveExact compensates a FUSE session publication without deleting a
// newer value written by another actor after an ambiguous Redis reply.
func (s *SessionStore) RemoveExact(ctx context.Context, sb *Sandbox) error {
	if s == nil || sb == nil {
		return ErrSessionPublicationConflict
	}
	atomicStore, ok := s.store.(state.AtomicStore)
	if !ok {
		return ErrSessionPublicationConflict
	}
	expected, err := json.Marshal(sb)
	if err != nil {
		return fmt.Errorf("marshal exact sandbox session: %w", err)
	}
	return s.removeExactValue(ctx, atomicStore, sb.ID, expected)
}

func (s *SessionStore) RemoveExactValue(ctx context.Context, id string, expected []byte) error {
	atomicStore, ok := s.store.(state.AtomicStore)
	if !ok || id == "" || len(expected) == 0 {
		return ErrSessionPublicationConflict
	}
	return s.removeExactValue(ctx, atomicStore, id, append([]byte(nil), expected...))
}

// RemoveMatchingRuntime removes only a session for the immutable runtime being
// finalized. Reading the latest value avoids deleting a reused logical ID.
func (s *SessionStore) RemoveMatchingRuntime(ctx context.Context, sb *Sandbox) error {
	if s == nil || sb == nil || sb.ID == "" || sb.RuntimeUID == "" {
		return ErrSessionPublicationConflict
	}
	raw, err := s.store.Get(ctx, sandboxSessionKeyPrefix+sb.ID)
	if err != nil {
		return err
	}
	if raw == nil {
		return s.confirmSessionAbsence(ctx, sb.ID)
	}
	var current Sandbox
	if json.Unmarshal(raw, &current) != nil || current.ID != sb.ID || current.RuntimeID != sb.RuntimeID || current.RuntimeUID != sb.RuntimeUID {
		return ErrSessionPublicationConflict
	}
	return s.RemoveExactValue(ctx, sb.ID, raw)
}

func (s *SessionStore) removeExactValue(ctx context.Context, atomicStore state.AtomicStore, id string, expected []byte) error {
	deleted, err := atomicStore.CompareAndDelete(ctx, sandboxSessionKeyPrefix+id, expected)
	if err != nil {
		return fmt.Errorf("remove exact sandbox session: %w", err)
	}
	if deleted {
		return nil
	}
	current, err := s.store.Get(ctx, sandboxSessionKeyPrefix+id)
	if err != nil {
		return fmt.Errorf("verify exact sandbox session removal: %w", err)
	}
	if current == nil {
		return s.confirmSessionAbsence(ctx, id)
	}
	if bytes.Equal(current, expected) {
		return ErrSessionPublicationConflict
	}
	return ErrSessionPublicationConflict
}

// RemoveMatchingFUSESession compare-deletes the latest valid revision of one
// lifecycle. A reused sandbox ID or changed immutable runtime/workspace
// identity is retained fail-closed.
func (s *SessionStore) RemoveMatchingFUSESession(ctx context.Context, id, runtimeID, runtimeUID, preparationID string, generation int64) error {
	atomicStore, ok := s.store.(state.AtomicStore)
	if !ok || id == "" || runtimeID == "" || runtimeUID == "" || preparationID == "" || generation <= 0 {
		return ErrSessionPublicationConflict
	}
	var lastErr error
	for range 4 {
		raw, err := s.store.Get(ctx, sandboxSessionKeyPrefix+id)
		if err != nil {
			return fmt.Errorf("load FUSE session for removal: %w", err)
		}
		if raw == nil {
			return s.confirmSessionAbsence(ctx, id)
		}
		var current Sandbox
		if err := json.Unmarshal(raw, &current); err != nil || current.ID != id || current.RuntimeID != runtimeID || current.RuntimeUID != runtimeUID || current.Workspace == nil || current.Workspace.MountType != WorkspaceMountFUSE || current.Workspace.FUSEPreparationID != preparationID || current.Workspace.LeaseGeneration != generation {
			return ErrSessionPublicationConflict
		}
		deleted, err := atomicStore.CompareAndDelete(ctx, sandboxSessionKeyPrefix+id, raw)
		if err != nil {
			lastErr = fmt.Errorf("remove matching FUSE session: %w", err)
			if errors.Is(err, state.ErrDurabilityUnconfirmed) {
				return lastErr
			}
			continue
		}
		if deleted {
			return nil
		}
	}
	return errors.Join(ErrSessionPublicationConflict, lastErr)
}

func (s *SessionStore) confirmSessionAbsence(ctx context.Context, id string) error {
	absent, err := confirmStateAbsence(ctx, s.store, sandboxSessionKeyPrefix+id)
	if err != nil {
		return fmt.Errorf("confirm sandbox session absence: %w", err)
	}
	if !absent {
		return ErrSessionPublicationConflict
	}
	return nil
}

// List returns all sandbox IDs in the store.
func (s *SessionStore) List(ctx context.Context) ([]string, error) {
	keys, err := s.store.Keys(ctx, sandboxSessionKeyPrefix+"*")
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(keys))
	for i, key := range keys {
		ids[i] = key[len(sandboxSessionKeyPrefix):]
	}
	return ids, nil
}

// Exists checks if a sandbox exists in the store.
func (s *SessionStore) Exists(ctx context.Context, id string) (bool, error) {
	return s.store.Exists(ctx, sandboxSessionKeyPrefix+id)
}
