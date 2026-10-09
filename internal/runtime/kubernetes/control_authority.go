package kubernetes

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/goairix/sandbox/internal/storage/state"
)

// LoadWorkspaceControlKey retains one authority per state scope across API
// replicas/restarts and API credential rotations. It is fencing history, not
// active lifecycle state, and must not be cleared during release drain.
// SetNX's durability acknowledgement is required even for an existing key.
func LoadWorkspaceControlKey(ctx context.Context, store state.Store, scope string) (ed25519.PrivateKey, error) {
	if store == nil || scope == "" {
		return nil, fmt.Errorf("workspace control authority requires scoped state")
	}
	digest := sha256.Sum256([]byte(scope))
	key := "sandbox:control-authority:v1:{" + hex.EncodeToString(digest[:]) + "}"
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, fmt.Errorf("generate workspace control authority: %w", err)
	}
	defer clear(seed)
	if _, err := store.SetNX(ctx, key, seed, 0); err != nil {
		return nil, fmt.Errorf("persist workspace control authority: %w", err)
	}
	stored, err := store.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("load workspace control authority: %w", err)
	}
	defer clear(stored)
	if len(stored) != ed25519.SeedSize {
		return nil, fmt.Errorf("workspace control authority is invalid")
	}
	return ed25519.NewKeyFromSeed(stored), nil
}

// WithWorkspaceControlKey installs a private server-only signing key. It must
// be stable across replicas and is never copied into a Pod or public metadata.
func WithWorkspaceControlKey(key ed25519.PrivateKey) Option {
	return func(r *Runtime) { r.workspaceControlKey = append(ed25519.PrivateKey(nil), key...) }
}
