package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type claimBeforeRetirementStore struct {
	*atomicMemoryStore
	target string
	once   sync.Once
}

func (s *claimBeforeRetirementStore) CompareAndSwap(ctx context.Context, key string, old, next []byte, ttl time.Duration) (bool, error) {
	var before, after ordinaryPoolRecord
	_ = json.Unmarshal(old, &before)
	_ = json.Unmarshal(next, &after)
	if key == s.target && before.State == ordinaryPoolPrepared && after.State == ordinaryPoolCleanup {
		s.once.Do(func() {
			claimed := before
			claimed.State, claimed.ClaimToken, claimed.ClaimUntil = ordinaryPoolClaimed, "business-claim", time.Now().Add(time.Minute)
			claimed.Revision++
			raw, _ := json.Marshal(claimed)
			_, _ = s.atomicMemoryStore.CompareAndSwap(ctx, key, old, raw, 0)
		})
	}
	return s.atomicMemoryStore.CompareAndSwap(ctx, key, old, next, ttl)
}

func TestSharedOrdinaryPoolRetirementCASCannotDeleteConcurrentClaim(t *testing.T) {
	ctx := context.Background()
	rt, store := newSharedPoolRuntime(), &claimBeforeRetirementStore{atomicMemoryStore: newAtomicMemoryStore()}
	old := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"})
	old.EnableShared(store, "rollout-test")
	require.NoError(t, old.Start(ctx))
	require.NoError(t, old.WarmUp(ctx))
	entries, err := old.shared.listCurrentRecords(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	store.target = entries[0].key
	old.Drain(ctx)
	next := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v2"})
	next.EnableShared(store, "rollout-test")
	require.NoError(t, next.Start(ctx))
	t.Cleanup(func() { next.Drain(ctx); _ = next.DrainRelease(ctx) })
	require.NoError(t, next.WarmUp(ctx))
	_, exists := rt.snapshotIDs()[entries[0].record.RuntimeID]
	assert.True(t, exists, "claim winning CAS must fence physical retirement")
	raw, err := store.Get(ctx, store.target)
	require.NoError(t, err)
	var claimed ordinaryPoolRecord
	require.NoError(t, json.Unmarshal(raw, &claimed))
	assert.Equal(t, ordinaryPoolClaimed, claimed.State)
}

type reserveBeforeRetirementRepository struct {
	*memoryFUSEPoolRepository
	once sync.Once
}

func (r *reserveBeforeRetirementRepository) ClaimCleanup(ctx context.Context, preparationID string, from state.FUSEPoolState, maintainer, reservation string, revision uint64, runtimeID, runtimeUID, cleanupToken string, ttl time.Duration) (*state.FUSEPoolRecord, error) {
	if from == state.FUSEPoolPrepared {
		r.once.Do(func() { _, _ = r.ReservePrepared(ctx, "pool-v1", "business-claim", time.Minute) })
	}
	return r.memoryFUSEPoolRepository.ClaimCleanup(ctx, preparationID, from, maintainer, reservation, revision, runtimeID, runtimeUID, cleanupToken, ttl)
}

func TestFUSEPoolSharedRetirementCASCannotDeleteConcurrentReservation(t *testing.T) {
	ctx := context.Background()
	rt, repo, store := newFUSEMockRuntime(), &reserveBeforeRetirementRepository{memoryFUSEPoolRepository: newMemoryFUSEPoolRepository()}, newAtomicMemoryStore()
	cfg := fusePoolConfig()
	cfg.InventoryStore, cfg.InventoryScope, cfg.RefillInterval = store, "rollout-test", time.Hour
	old := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-v1"))
	require.NoError(t, old.Start(ctx))
	require.NoError(t, old.Stop(ctx))
	cfg.MaintainerToken = "api-b"
	next := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-v2"))
	require.NoError(t, next.Start(ctx))
	t.Cleanup(func() { _ = next.DrainRelease(ctx) })
	records, err := repo.ListByPoolKey(ctx, "pool-v1")
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, state.FUSEPoolReserved, records[0].State)
	assert.False(t, rt.wasRemoved(records[0].RuntimeID))
}

type resolvingFUSETestRuntime struct{ *mockRuntime }

func (r *resolvingFUSETestRuntime) ResolvePreparedSandbox(_ context.Context, preparationID, _ string) (runtime.RuntimeRef, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, info := range r.sandboxes {
		if info.ID == preparationID {
			return runtime.RuntimeRef{ID: info.RuntimeID, UID: info.RuntimeUID}, nil
		}
	}
	return runtime.RuntimeRef{}, runtime.ErrNotFound
}

func TestFUSEPoolSharedRecoversUnboundPreparingRuntime(t *testing.T) {
	ctx := context.Background()
	rt, repo, store := &resolvingFUSETestRuntime{newFUSEMockRuntime()}, newMemoryFUSEPoolRepository(), newAtomicMemoryStore()
	r := state.FUSEPoolRecord{PreparationID: "interrupted-preparation", PoolKey: "pool-v1", State: state.FUSEPoolPreparing,
		MaintainerToken: "crashed-api", Revision: 1, UpdatedAt: repo.now.Add(-time.Minute), PrepareUntil: repo.now.Add(-time.Second)}
	repo.seed(r)
	rt.sandboxes["exact-pod"] = &runtime.SandboxInfo{ID: r.PreparationID, RuntimeID: "exact-pod", RuntimeUID: "exact-uid", State: "running"}
	cfg := fusePoolConfig()
	cfg.InventoryStore, cfg.InventoryScope, cfg.RefillInterval, cfg.MinSize = store, "rollout-test", time.Hour, 0
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-v1"))
	t.Cleanup(func() { _ = pool.DrainRelease(ctx) })
	require.NoError(t, pool.Start(ctx))
	assert.True(t, rt.wasRemoved("exact-pod"), "must bind exact UID before retiring interrupted preparation")
}

func TestSharedOrdinaryPoolContractSeparatesReleases(t *testing.T) {
	ctx := context.Background()
	rt, store := newSharedPoolRuntime(), newAtomicMemoryStore()
	cfg := PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"}
	a, b := NewPool(rt, cfg), NewPool(rt, cfg)
	a.EnableShared(store, "release-a")
	b.EnableShared(store, "release-b")
	require.NotEqual(t, a.shared.poolKey, b.shared.poolKey)
	require.NoError(t, a.Start(ctx))
	require.NoError(t, a.WarmUp(ctx))
	require.NoError(t, b.Start(ctx))
	require.NoError(t, b.WarmUp(ctx))
	t.Cleanup(func() { a.Drain(ctx); b.Drain(ctx); _ = a.DrainRelease(ctx); _ = b.DrainRelease(ctx) })
	assert.Len(t, rt.snapshotIDs(), 2, "independent release CAS records must never represent the same Pod")
}

func TestSharedOrdinaryPoolContractCorruptOldGenerationDoesNotBlockReplacement(t *testing.T) {
	ctx := context.Background()
	rt, store := newSharedPoolRuntime(), newAtomicMemoryStore()
	pool := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v2"})
	pool.EnableShared(store, "rollout-test")
	key := "ordinarypool:v1:" + pool.shared.scopeDigest + ":old-corrupt-key:record:old"
	require.NoError(t, store.Set(ctx, key, []byte("invalid"), 0))
	require.NoError(t, pool.Start(ctx))
	t.Cleanup(func() { pool.Drain(ctx); _ = pool.DrainRelease(ctx) })
	require.NoError(t, pool.WarmUp(ctx))
	assert.Equal(t, 1, pool.Size())
	raw, err := store.Get(ctx, key)
	require.NoError(t, err)
	assert.Equal(t, []byte("invalid"), raw, "unknown old state must be retained, not guessed away")
}

func TestFUSEPoolSharedStartupDoesNotRunNamespaceOrphanSweep(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, _, _, store := newFUSETestManager(t, rt)
	mgr.fusePool.config.InventoryStore, mgr.fusePool.config.InventoryScope = store, "rollout-test"
	mgr.fusePool.shared = newSharedFUSEPool(mgr.fusePool)
	require.NoError(t, mgr.reconcileFUSEOrphans(context.Background()))
	rt.mu.Lock()
	calls := rt.orphanCalls
	rt.mu.Unlock()
	assert.Zero(t, calls, "namespace snapshot cleanup must not race other generation preparations")
	require.NoError(t, mgr.reconcileFUSEOrphansWithExclusivity(context.Background(), true))
	rt.mu.Lock()
	calls = rt.orphanCalls
	rt.mu.Unlock()
	assert.Equal(t, 1, calls, "explicit release drain retains orphan cleanup")
}

func TestFUSEPoolContractRolloutRestoresAndDestroysOldActiveSandbox(t *testing.T) {
	ctx := context.Background()
	rt := newFUSEManagerRuntime()
	first, _, repo, store := newFUSETestManager(t, rt)
	sb, err := first.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/rollout"})
	require.NoError(t, err)
	first.mu.RLock()
	oldLifecycle := first.fuseLifecycles[sb.ID]
	first.mu.RUnlock()
	oldLifecycle.cancel()
	oldLifecycle.renewal.Stop()
	require.NoError(t, first.fusePool.Stop(ctx))
	newSpec := fixedFUSESpec("new-pool-key")
	newSpec.Image = "sandbox:v2"
	cfg := first.config
	cfg.FUSEPool = NewFUSEPool(rt, repo, fusePoolConfig(), newSpec)
	restored := NewManager(rt, nil, first.fsMeta, cfg)
	restored.SetSessionStore(NewSessionStore(store, time.Hour))
	t.Cleanup(func() { require.NoError(t, restored.Stop(ctx)) })
	require.NoError(t, restored.restorePersistentSandboxes(ctx), "a new warm contract must not reject an existing exact consumed runtime")
	got, err := restored.Get(ctx, sb.ID)
	require.NoError(t, err)
	assert.Equal(t, sb.RuntimeUID, got.RuntimeUID)
	require.NoError(t, restored.Destroy(ctx, sb.ID), "business cleanup must support prior-generation records")
	assert.True(t, rt.wasRemoved(sb.RuntimeID))
}

func TestSharedOrdinaryPoolRetireInterruptedPreparationWithUnboundUID(t *testing.T) {
	ctx := context.Background()
	rt, store := newSharedPoolRuntime(), newAtomicMemoryStore()
	old := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"})
	old.EnableShared(store, "rollout-test")
	require.NoError(t, old.Start(ctx))
	require.NoError(t, old.WarmUp(ctx))
	entries, err := old.shared.listCurrentRecords(ctx)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	r := entries[0].record
	r.State, r.RuntimeID, r.RuntimeUID = ordinaryPoolPreparing, "", ""
	r.UpdatedAt = time.Now().Add(-3 * time.Minute)
	raw, err := json.Marshal(r)
	require.NoError(t, err)
	require.NoError(t, store.Set(ctx, entries[0].key, raw, 0))
	original := rt.snapshotIDs()
	old.Drain(ctx)
	next := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v2"})
	next.EnableShared(store, "rollout-test")
	require.NoError(t, next.Start(ctx))
	t.Cleanup(func() { next.Drain(ctx); _ = next.DrainRelease(ctx) })
	require.NoError(t, next.WarmUp(ctx))
	for id := range original {
		_, exists := rt.snapshotIDs()[id]
		assert.False(t, exists, "instance-bound orphan must be removed before its record")
	}
}

func TestSharedOrdinaryPoolLostOwnerLeaseRejectsAcquire(t *testing.T) {
	ctx := context.Background()
	pool := NewPool(newSharedPoolRuntime(), PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"})
	pool.EnableShared(newAtomicMemoryStore(), "rollout-test")
	require.NoError(t, pool.Start(ctx))
	require.NoError(t, pool.WarmUp(ctx))
	t.Cleanup(func() { pool.Drain(ctx); _ = pool.DrainRelease(ctx) })
	pool.shared.cancel()
	_, err := pool.Acquire(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSharedOrdinaryPoolReconcilePreservesSlowClaimOfLiveReplica(t *testing.T) {
	ctx := context.Background()
	rt, store := newSharedPoolRuntime(), newAtomicMemoryStore()
	pool := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"})
	pool.EnableShared(store, "rollout-test")
	require.NoError(t, pool.Start(ctx))
	require.NoError(t, pool.WarmUp(ctx))
	t.Cleanup(func() { pool.Drain(ctx); _ = pool.DrainRelease(ctx) })
	info, err := pool.Acquire(ctx)
	require.NoError(t, err)
	require.Eventually(t, func() bool { pool.shared.mu.Lock(); defer pool.shared.mu.Unlock(); return !pool.shared.refilling }, time.Second, time.Millisecond)
	entries, err := pool.shared.listCurrentRecords(ctx)
	require.NoError(t, err)
	for _, entry := range entries {
		if entry.record.RuntimeID != info.RuntimeID {
			continue
		}
		r := entry.record
		r.ClaimUntil = time.Now().Add(-time.Second)
		raw, err := json.Marshal(r)
		require.NoError(t, err)
		require.NoError(t, store.Set(ctx, entry.key, raw, 0))
	}
	require.NoError(t, pool.Reconcile(ctx, nil))
	_, exists := rt.snapshotIDs()[info.RuntimeID]
	assert.True(t, exists, "TTL alone cannot prove a live replica's slow identity migration is abandoned")
	store.expireKey(pool.shared.ownerKey)
	require.NoError(t, pool.Reconcile(ctx, nil))
	_, exists = rt.snapshotIDs()[info.RuntimeID]
	assert.True(t, exists, "missing Redis membership cannot fence a request still migrating its Kubernetes identity")
}

func TestSharedOrdinaryPoolOwnerLeaseRecoversAfterRedisFailure(t *testing.T) {
	ctx := context.Background()
	rt, store := newSharedPoolRuntime(), newAtomicMemoryStore()
	pool := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"})
	pool.EnableShared(store, "rollout-test")
	require.NoError(t, pool.Start(ctx))
	require.NoError(t, pool.WarmUp(ctx))
	t.Cleanup(func() { pool.Drain(ctx); _ = pool.DrainRelease(ctx) })
	store.failNext("CompareAndSwap", errors.New("Redis unavailable"))
	require.Error(t, pool.shared.refreshOwner(ctx))
	_, err := pool.Acquire(ctx)
	require.Error(t, err, "lost membership must reject warm claims")
	store.expireKey(pool.shared.ownerKey)
	require.NoError(t, pool.shared.refreshOwner(ctx))
	_, err = pool.Acquire(ctx)
	require.NoError(t, err, "recovery must not require restarting every API")
}

func TestFUSEPoolSharedOwnerLeaseRecoversAfterRedisFailure(t *testing.T) {
	ctx := context.Background()
	rt, repo, store := newFUSEMockRuntime(), newMemoryFUSEPoolRepository(), newAtomicMemoryStore()
	cfg := fusePoolConfig()
	cfg.InventoryStore, cfg.InventoryScope, cfg.RefillInterval = store, "rollout-test", time.Hour
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-v1"))
	require.NoError(t, pool.Start(ctx))
	t.Cleanup(func() { _ = pool.DrainRelease(ctx) })
	store.failNext("CompareAndSwap", errors.New("Redis unavailable"))
	require.Error(t, pool.shared.refreshOwner(ctx))
	_, err := pool.Acquire(ctx, "pool-v1")
	require.Error(t, err)
	store.expireKey(pool.shared.ownerKey())
	require.NoError(t, pool.shared.refreshOwner(ctx))
	_, err = pool.Acquire(ctx, "pool-v1")
	require.NoError(t, err)
}

func TestFUSEPoolSharedReleaseDrainDeletesRolloutRegistry(t *testing.T) {
	ctx := context.Background()
	rt, repo, store := newFUSEMockRuntime(), newMemoryFUSEPoolRepository(), newAtomicMemoryStore()
	cfg := fusePoolConfig()
	cfg.InventoryStore, cfg.InventoryScope, cfg.RefillInterval = store, "rollout-test", time.Hour
	pool := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-v1"))
	require.NoError(t, pool.Start(ctx))
	require.NoError(t, pool.DrainRelease(ctx))
	keys, err := store.Keys(ctx, pool.shared.base+"*")
	require.NoError(t, err)
	assert.Empty(t, keys, "explicit release drain must clear generation membership state")
}

type failingReplacementRuntime struct {
	*sharedPoolRuntime
}

func (r *failingReplacementRuntime) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	if spec.Image == "sandbox:v2" {
		return nil, errors.New("replacement image cannot start")
	}
	return r.sharedPoolRuntime.CreateSandbox(ctx, spec)
}

func TestSharedOrdinaryPoolReplacementFailurePreservesOldInventory(t *testing.T) {
	ctx := context.Background()
	rt, store := &failingReplacementRuntime{newSharedPoolRuntime()}, newAtomicMemoryStore()
	old := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"})
	old.EnableShared(store, "rollout-test")
	require.NoError(t, old.Start(ctx))
	require.NoError(t, old.WarmUp(ctx))
	original := rt.snapshotIDs()
	old.Drain(ctx)
	next := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v2"})
	next.EnableShared(store, "rollout-test")
	require.NoError(t, next.Start(ctx))
	t.Cleanup(func() { next.Drain(ctx); _ = next.DrainRelease(ctx) })
	require.Error(t, next.WarmUp(ctx))
	assert.Equal(t, original, rt.snapshotIDs(), "cannot retire before healthy replacement capacity exists")
}

func TestFUSEPoolSharedReplacementFailurePreservesOldInventory(t *testing.T) {
	ctx := context.Background()
	rt, repo, store := newFUSEMockRuntime(), newMemoryFUSEPoolRepository(), newAtomicMemoryStore()
	cfg := fusePoolConfig()
	cfg.InventoryStore, cfg.InventoryScope, cfg.RefillInterval = store, "rollout-test", time.Hour
	old := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-v1"))
	require.NoError(t, old.Start(ctx))
	original, err := repo.ListByPoolKey(ctx, "pool-v1")
	require.NoError(t, err)
	require.NoError(t, old.Stop(ctx))
	rt.mu.Lock()
	rt.prepareErr = errors.New("replacement image cannot start")
	rt.mu.Unlock()
	cfg.MaintainerToken = "api-b"
	next := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-v2"))
	t.Cleanup(func() { _ = next.DrainRelease(ctx) })
	require.Error(t, next.Start(ctx))
	remaining, err := repo.ListByPoolKey(ctx, "pool-v1")
	require.NoError(t, err)
	assert.Equal(t, original, remaining)
}

func TestFUSEPoolSharedRolloutDoesNotRetireAnotherRelease(t *testing.T) {
	ctx := context.Background()
	rt, repo, store := newFUSEMockRuntime(), newMemoryFUSEPoolRepository(), newAtomicMemoryStore()
	cfg := fusePoolConfig()
	cfg.InventoryStore, cfg.InventoryScope, cfg.RefillInterval = store, "other-release", time.Hour
	other := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("other-pool"))
	require.NoError(t, other.Start(ctx))
	require.NoError(t, other.Stop(ctx))
	cfg.MaintainerToken, cfg.InventoryScope = "api-b", "this-release"
	next := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("this-pool"))
	require.NoError(t, next.Start(ctx))
	t.Cleanup(func() { _ = next.DrainRelease(ctx) })
	records, err := repo.ListByPoolKey(ctx, "other-pool")
	require.NoError(t, err)
	assert.Len(t, records, 1, "an owner-free pool of another release must not retire")
}

func TestFUSEPoolContractIncludesRuntimeTemplateAndScope(t *testing.T) {
	spec := fixedFUSESpec("")
	spec.PoolContract = "pod/v1:scope:release-a"
	key, err := ComputeFUSEPoolKey(spec)
	require.NoError(t, err)
	for _, contract := range []string{"pod/v2:scope:release-a", "pod/v1:scope:release-b"} {
		spec.PoolContract = contract
		changed, err := ComputeFUSEPoolKey(spec)
		require.NoError(t, err)
		assert.NotEqual(t, key, changed)
	}
}

func TestOrdinaryPoolContractCoversTemplateAndSecurity(t *testing.T) {
	base := PoolConfig{Image: "sandbox:v1", PidLimit: 100, SeccompProfile: "default", RuntimeContract: "pod/v1"}
	key := ordinaryPoolFingerprint(base)
	for _, mutate := range []func(*PoolConfig){
		func(c *PoolConfig) { c.Image = "sandbox:v2" },
		func(c *PoolConfig) { c.PidLimit++ },
		func(c *PoolConfig) { c.SeccompProfile = "strict" },
		func(c *PoolConfig) { c.RuntimeContract = "pod/v2" },
	} {
		changed := base
		mutate(&changed)
		assert.NotEqual(t, key, ordinaryPoolFingerprint(changed))
	}
	base.MinSize, base.MaxSize = 10, 20
	assert.Equal(t, key, ordinaryPoolFingerprint(base), "capacity does not change the runtime contract")
}

func TestFUSEPoolSharedRestartReusesCompatibleInventory(t *testing.T) {
	ctx := context.Background()
	rt, repo, store := newFUSEMockRuntime(), newMemoryFUSEPoolRepository(), newAtomicMemoryStore()
	cfg := fusePoolConfig()
	cfg.InventoryStore, cfg.InventoryScope = store, "rollout-test"
	cfg.RefillInterval = time.Hour
	first := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-v1"))
	require.NoError(t, first.Start(ctx))
	original, err := repo.ListByPoolKey(ctx, "pool-v1")
	require.NoError(t, err)
	require.Len(t, original, 1)
	require.NoError(t, first.Stop(ctx))
	remaining, err := repo.ListByPoolKey(ctx, "pool-v1")
	require.NoError(t, err)
	require.Len(t, remaining, 1, "shared prepared Pod must outlive its API maintainer")
	cfg.MaintainerToken = "api-b"
	second := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-v1"))
	require.NoError(t, second.Start(ctx))
	t.Cleanup(func() { _ = second.DrainRelease(ctx) })
	claimed, err := second.Acquire(ctx, "pool-v1")
	require.NoError(t, err)
	assert.Equal(t, original[0].RuntimeUID, claimed.RuntimeUID)
}

func TestFUSEPoolSharedRetireOnlyOwnerFreePreparedInventory(t *testing.T) {
	ctx := context.Background()
	rt, repo, store := newFUSEMockRuntime(), newMemoryFUSEPoolRepository(), newAtomicMemoryStore()
	cfg := fusePoolConfig()
	cfg.InventoryStore, cfg.InventoryScope = store, "rollout-test"
	cfg.RefillInterval = time.Hour
	cfg.MinSize = 2
	old := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-v1"))
	require.NoError(t, old.Start(ctx))
	cfg.MaintainerToken, cfg.MinSize = "api-b", 1
	next := NewFUSEPool(rt, repo, cfg, fixedFUSESpec("pool-v2"))
	require.NoError(t, next.Start(ctx))
	t.Cleanup(func() { _ = old.Stop(ctx); _ = next.DrainRelease(ctx) })
	records, err := repo.ListByPoolKey(ctx, "pool-v1")
	require.NoError(t, err)
	require.Len(t, records, 2, "old live owner must retain its pool")
	reserved, err := repo.ReservePrepared(ctx, "pool-v1", "business-claim", time.Minute)
	require.NoError(t, err)
	require.NotNil(t, reserved)
	require.NoError(t, old.Stop(ctx))
	require.NoError(t, next.Reconcile(ctx))
	records, err = repo.ListByPoolKey(ctx, "pool-v1")
	require.NoError(t, err)
	require.Len(t, records, 1, "obsolete prepared inventory must retire")
	assert.Equal(t, reserved.RuntimeUID, records[0].RuntimeUID, "reserved runtime must survive")
}

func TestSharedOrdinaryPoolRestartReusesCompatibleInventory(t *testing.T) {
	ctx := context.Background()
	rt, store := newSharedPoolRuntime(), newAtomicMemoryStore()
	cfg := PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"}
	first := NewPool(rt, cfg)
	first.EnableShared(store, "rollout-test")
	require.NoError(t, first.Start(ctx))
	require.NoError(t, first.WarmUp(ctx))
	original := rt.snapshotIDs()
	first.Drain(ctx)
	require.Equal(t, original, rt.snapshotIDs(), "API shutdown must not destroy compatible inventory")

	second := NewPool(rt, cfg)
	second.EnableShared(store, "rollout-test")
	require.NoError(t, second.Start(ctx))
	t.Cleanup(func() { second.Drain(ctx); _ = second.DrainRelease(ctx) })
	require.NoError(t, second.WarmUp(ctx))
	info, err := second.Acquire(ctx)
	require.NoError(t, err)
	_, reused := original[info.RuntimeID]
	assert.True(t, reused)
}

func TestSharedOrdinaryPoolRetireOnlyUnclaimedOldInventory(t *testing.T) {
	ctx := context.Background()
	rt, store := newSharedPoolRuntime(), newAtomicMemoryStore()
	old := NewPool(rt, PoolConfig{MinSize: 2, MaxSize: 2, Image: "sandbox:v1"})
	next := NewPool(rt, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v2"})
	old.EnableShared(store, "rollout-test")
	next.EnableShared(store, "rollout-test")
	require.NoError(t, old.Start(ctx))
	require.NoError(t, old.WarmUp(ctx))
	original := rt.snapshotIDs()
	require.NoError(t, next.Start(ctx))
	t.Cleanup(func() { old.Drain(ctx); next.Drain(ctx); _ = next.DrainRelease(ctx) })
	require.NoError(t, next.WarmUp(ctx))
	for id := range original {
		_, exists := rt.snapshotIDs()[id]
		require.True(t, exists, "live old owner must retain inventory")
	}
	claimed, err := old.Acquire(ctx)
	require.NoError(t, err)
	old.Drain(ctx)
	require.NoError(t, next.WarmUp(ctx))
	remaining := rt.snapshotIDs()
	_, exists := remaining[claimed.RuntimeID]
	require.True(t, exists, "claimed sandbox must survive upgrade retirement")
	for id := range original {
		if id != claimed.RuntimeID {
			_, exists := remaining[id]
			assert.False(t, exists, "owner-free old prepared sandbox must retire")
		}
	}
}
