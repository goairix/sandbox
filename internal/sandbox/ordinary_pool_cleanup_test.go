package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
)

// Model the interruption after Pod deletion but before policy cleanup.
type interruptedOrdinaryCleanupRuntime struct {
	*sharedPoolRuntime
	removeErr     error
	cleanupErr    error
	policyPresent bool
}

func (r *interruptedOrdinaryCleanupRuntime) RemoveOrdinarySandbox(ctx context.Context, ref runtime.RuntimeRef) error {
	return r.removeErr
}

func (r *interruptedOrdinaryCleanupRuntime) CleanupOrdinarySandboxPolicies(ctx context.Context, ref runtime.RuntimeRef, logicalID string) error {
	if _, err := r.GetSandbox(ctx, ref.ID); !errors.Is(err, runtime.ErrNotFound) {
		return errors.Join(runtime.ErrTerminationUnconfirmed, err)
	}
	if r.cleanupErr != nil {
		return r.cleanupErr
	}
	r.policyPresent = false
	return nil
}

func seedOrdinaryCleanupRecord(t *testing.T, rt runtime.Runtime, bound bool) (*sharedOrdinaryPool, ordinaryPoolEntry) {
	t.Helper()
	store := newAtomicMemoryStore()
	pool := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"})
	pool.EnableShared(store, "cleanup-test")
	record := ordinaryPoolRecord{PreparationID: "prep-a", PoolKey: pool.shared.poolKey, State: ordinaryPoolPreparing, UpdatedAt: time.Now().UTC(), Revision: 1}
	if bound {
		record.RuntimeID, record.RuntimeUID, record.State = "pod-a", "old-uid", ordinaryPoolPrepared
	}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	entry := ordinaryPoolEntry{key: pool.shared.recordBase + record.PreparationID, record: record, raw: raw}
	require.NoError(t, store.Set(context.Background(), entry.key, raw, 0))
	return pool.shared, entry
}

func reloadOrdinaryCleanupRecord(t *testing.T, pool *sharedOrdinaryPool, entry ordinaryPoolEntry) ordinaryPoolEntry {
	t.Helper()
	raw, err := pool.store.Get(context.Background(), entry.key)
	require.NoError(t, err)
	require.NotEmpty(t, raw, "failed cleanup must retain the record")
	require.NoError(t, json.Unmarshal(raw, &entry.record))
	entry.raw = raw
	require.Equal(t, ordinaryPoolCleanup, entry.record.State)
	return entry
}

func TestOrdinaryPoolCleanupRetriesMissingPodUntilPoliciesAreGone(t *testing.T) {
	ctx := context.Background()
	rt := &interruptedOrdinaryCleanupRuntime{sharedPoolRuntime: newSharedPoolRuntime(), removeErr: runtime.ErrTerminationUnconfirmed, policyPresent: true}
	pool, entry := seedOrdinaryCleanupRecord(t, rt, true)
	require.ErrorIs(t, pool.cleanupRecord(ctx, entry, nil), runtime.ErrTerminationUnconfirmed)
	entry = reloadOrdinaryCleanupRecord(t, pool, entry)
	require.True(t, rt.policyPresent)
	rt.removeErr, rt.cleanupErr = runtime.ErrNotFound, errors.New("policy delete forbidden")
	require.ErrorIs(t, pool.cleanupRecord(ctx, entry, nil), rt.cleanupErr)
	entry = reloadOrdinaryCleanupRecord(t, pool, entry)
	require.True(t, rt.policyPresent)
	rt.cleanupErr = nil
	require.NoError(t, pool.cleanupRecord(ctx, entry, nil))
	require.False(t, rt.policyPresent)
	raw, _ := pool.store.Get(ctx, entry.key)
	require.Empty(t, raw)
}

func TestOrdinaryPoolCleanupRetainsReplacementAndRecord(t *testing.T) {
	rt := &interruptedOrdinaryCleanupRuntime{sharedPoolRuntime: newSharedPoolRuntime(), removeErr: runtime.ErrNotFound, policyPresent: true}
	rt.sandboxes["pod-a"] = &runtime.SandboxInfo{RuntimeID: "pod-a", RuntimeUID: "new-uid"}
	pool, entry := seedOrdinaryCleanupRecord(t, rt, true)
	require.ErrorIs(t, pool.cleanupRecord(context.Background(), entry, nil), runtime.ErrTerminationUnconfirmed)
	_ = reloadOrdinaryCleanupRecord(t, pool, entry)
	require.True(t, rt.policyPresent)
	require.Equal(t, "new-uid", rt.sandboxes["pod-a"].RuntimeUID)
}

// The embedded interface deliberately exposes no policy cleaner capability.
type ordinaryCleanupWithoutCleaner struct{ runtime.Runtime }

func (r ordinaryCleanupWithoutCleaner) RemoveOrdinarySandbox(context.Context, runtime.RuntimeRef) error {
	return runtime.ErrNotFound
}

func TestOrdinaryPoolCleanupRequiresCleanerAfterNotFound(t *testing.T) {
	pool, entry := seedOrdinaryCleanupRecord(t, ordinaryCleanupWithoutCleaner{newSharedPoolRuntime()}, true)
	require.Error(t, pool.cleanupRecord(context.Background(), entry, nil))
	_ = reloadOrdinaryCleanupRecord(t, pool, entry)
}

func TestOrdinaryPoolCleanupRetiresUnboundPreparationWithoutGuessingRuntime(t *testing.T) {
	pool, entry := seedOrdinaryCleanupRecord(t, ordinaryCleanupWithoutCleaner{newSharedPoolRuntime()}, false)
	require.NoError(t, pool.cleanupRecord(context.Background(), entry, nil))
	raw, _ := pool.store.Get(context.Background(), entry.key)
	require.Empty(t, raw)
}

func TestOrdinaryPoolCleanupRetirementRecoversMissingOldPodPolicies(t *testing.T) {
	ctx := context.Background()
	rt := &interruptedOrdinaryCleanupRuntime{sharedPoolRuntime: newSharedPoolRuntime(), removeErr: runtime.ErrNotFound, policyPresent: true}
	pool, current := seedOrdinaryCleanupRecord(t, rt, true)
	current.record.RuntimeID, current.record.RuntimeUID = "pod-current", "current-uid"
	current.raw, _ = json.Marshal(current.record)
	require.NoError(t, pool.store.Set(ctx, current.key, current.raw, 0))
	old := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:old"})
	old.EnableShared(pool.store, "cleanup-test")
	oldRecord := ordinaryPoolRecord{PreparationID: "prep-old", RuntimeID: "pod-a", RuntimeUID: "old-uid", PoolKey: old.shared.poolKey, State: ordinaryPoolPrepared, UpdatedAt: time.Now().UTC(), Revision: 1}
	oldRaw, err := json.Marshal(oldRecord)
	require.NoError(t, err)
	oldKey := old.shared.recordBase + oldRecord.PreparationID
	require.NoError(t, pool.store.Set(ctx, oldKey, oldRaw, 0))
	require.NoError(t, pool.retireObsolete(ctx, 1))
	require.False(t, rt.policyPresent)
	remaining, err := pool.store.Get(ctx, oldKey)
	require.NoError(t, err)
	require.Empty(t, remaining)
	remaining, err = pool.store.Get(ctx, current.key)
	require.NoError(t, err)
	require.Equal(t, current.raw, remaining, "replacement capacity remains available")
}
