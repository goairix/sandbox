package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/stretchr/testify/require"
)

// The hook runs after reconciliation loads the record snapshot and before its
// runtime inventory call. Atomic updates still use the actual backing store.
type ordinaryContentionRuntime struct {
	*sharedPoolRuntime
	hookMu          sync.Mutex
	beforeInventory func(context.Context)
	bounded         bool
	publications    atomic.Int64
}

func (r *ordinaryContentionRuntime) inventoryHook(ctx context.Context) {
	r.hookMu.Lock()
	hook := r.beforeInventory
	r.beforeInventory = nil
	r.hookMu.Unlock()
	if hook != nil {
		hook(ctx)
	}
}

func (r *ordinaryContentionRuntime) GetSandbox(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	r.inventoryHook(ctx)
	return r.sharedPoolRuntime.GetSandbox(ctx, id)
}

func (r *ordinaryContentionRuntime) ListSandboxes(ctx context.Context, labels map[string]string) ([]runtime.SandboxInfo, error) {
	if r.bounded && labels["sandbox.pool.instance"] == "" {
		return nil, fmt.Errorf("steady refill performed a broad inventory scan")
	}
	r.inventoryHook(ctx)
	return r.sharedPoolRuntime.ListSandboxes(ctx, labels)
}

func (r *ordinaryContentionRuntime) PublishOrdinaryPoolPrepared(ctx context.Context, ref runtime.RuntimeRef, key, instance string) error {
	r.publications.Add(1)
	return r.sharedPoolRuntime.PublishOrdinaryPoolPrepared(ctx, ref, key, instance)
}

func ordinaryContentionStores(t *testing.T, test func(*testing.T, state.AtomicStore)) {
	t.Helper()
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			var store state.AtomicStore = newAtomicMemoryStore()
			if backend == "redis" {
				addr := os.Getenv("TEST_REDIS_ADDR")
				if addr == "" {
					t.Skip("requires TEST_REDIS_ADDR")
				}
				redis, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr, DB: 14})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, redis.Close()) })
				store = redis
			}
			test(t, store)
		})
	}
}

func seedOrdinaryContention(t *testing.T, store state.AtomicStore, bounded bool) (*sharedOrdinaryPool, *ordinaryContentionRuntime, ordinaryPoolEntry) {
	t.Helper()
	ctx := context.Background()
	r := &ordinaryContentionRuntime{sharedPoolRuntime: newSharedPoolRuntime(), bounded: bounded}
	pool := NewPool(r, PoolConfig{MinSize: 1, MaxSize: 1, Image: "sandbox:v1"})
	pool.EnableShared(store, "ordinary-contention-"+randSuffix(24))
	p := pool.shared
	p.ownerHealthy.Store(true)
	// Tests explicitly drive refill instead of racing an asynchronous refill
	// against the controlled inventory interleaving.
	p.stopping = true
	t.Cleanup(p.cancel)
	instance := randSuffix(24)
	pod, err := r.CreateSandbox(ctx, runtime.SandboxSpec{ID: "contended", Labels: map[string]string{
		"sandbox.pool": "true", "sandbox.pool.key": p.poolKey,
		"sandbox.pool.instance": instance, "sandbox.pool.state": "prepared",
	}})
	require.NoError(t, err)
	record := ordinaryPoolRecord{PreparationID: instance, RuntimeID: pod.RuntimeID, RuntimeUID: pod.RuntimeUID,
		PoolKey: p.poolKey, State: ordinaryPoolPrepared, UpdatedAt: time.Now().UTC(), Revision: 1}
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	entry := ordinaryPoolEntry{key: p.recordKey(instance), raw: raw, record: record}
	require.NoError(t, store.Set(ctx, entry.key, raw, 0))
	t.Cleanup(func() {
		keys, err := store.Keys(ctx, p.scopePattern())
		require.NoError(t, err)
		for _, key := range keys {
			require.NoError(t, store.Delete(ctx, key))
		}
	})
	return p, r, entry
}

func TestOrdinaryPoolCheckoutContentionRefresh(t *testing.T) {
	ordinaryContentionStores(t, func(t *testing.T, store state.AtomicStore) {
		for _, scanOrphans := range []bool{false, true} {
			for _, consumed := range []bool{false, true} {
				t.Run(fmt.Sprintf("startup=%t/consumed=%t", scanOrphans, consumed), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					p, r, entry := seedOrdinaryContention(t, store, !scanOrphans)
					r.beforeInventory = func(hookCtx context.Context) {
						// The actual checkout CAS runs while reconciliation holds
						// its lock: checkout must not take that shared slow-path lock.
						claimed, err := p.acquire(hookCtx)
						require.NoError(t, err)
						require.Equal(t, entry.record.RuntimeID, claimed.RuntimeID)
						require.Equal(t, entry.record.RuntimeUID, claimed.RuntimeUID)
						// Kubernetes returns an annotation-derived business view;
						// the runtime is no longer returned as warm inventory.
						require.NoError(t, r.UpdateLabels(hookCtx, entry.record.RuntimeID, map[string]*string{"sandbox.pool": nil}))
						if consumed {
							require.NoError(t, p.confirmAcquired(hookCtx, entry.record.RuntimeID, entry.record.RuntimeUID))
						}
					}
					require.NoError(t, p.reconcileInventory(ctx, nil, scanOrphans))
					require.Equal(t, int64(0), p.knownSize.Load())
					require.Equal(t, int64(0), r.publications.Load(), "business runtime must not be republished")
					r.mu.Lock()
					require.Empty(t, r.removedIDs)
					r.mu.Unlock()
					entries, err := p.listCurrentRecords(ctx)
					require.NoError(t, err)
					require.Empty(t, entries, "consumed runtime must not be adopted as pool inventory")
					require.NoError(t, p.refill(ctx))
					require.Equal(t, int64(1), p.knownSize.Load())
					entries, err = p.listCurrentRecords(ctx)
					require.NoError(t, err)
					require.Len(t, entries, 1)
					require.NotEqual(t, entry.record.RuntimeID, entries[0].record.RuntimeID)
					pod, err := r.GetSandbox(ctx, entry.record.RuntimeID)
					require.NoError(t, err)
					require.Equal(t, "running", pod.State)
				})
			}
		}
	})
}

func TestOrdinaryPoolClaimRetirementContentionRefresh(t *testing.T) {
	ordinaryContentionStores(t, func(t *testing.T, store state.AtomicStore) {
		for _, scanOrphans := range []bool{false, true} {
			t.Run(fmt.Sprintf("startup=%t", scanOrphans), func(t *testing.T) {
				ctx := context.Background()
				p, r, entry := seedOrdinaryContention(t, store, !scanOrphans)
				claimed, err := p.acquire(ctx)
				require.NoError(t, err)
				require.Equal(t, entry.record.RuntimeID, claimed.RuntimeID)
				r.beforeInventory = func(hookCtx context.Context) {
					require.NoError(t, r.UpdateLabels(hookCtx, claimed.RuntimeID, map[string]*string{"sandbox.pool": nil}))
					require.NoError(t, p.confirmAcquired(hookCtx, claimed.RuntimeID, claimed.RuntimeUID))
				}
				require.NoError(t, p.reconcileInventory(ctx, nil, scanOrphans))
				require.Zero(t, p.knownSize.Load())
				records, err := p.listCurrentRecords(ctx)
				require.NoError(t, err)
				require.Empty(t, records)
				require.Zero(t, r.publications.Load())
				r.mu.Lock()
				require.Empty(t, r.removedIDs)
				r.mu.Unlock()
			})
		}
	})
}

type ordinaryContentionStore struct {
	state.AtomicStore
	beforeCleanupCAS func(context.Context, string, []byte)
	readErr          error
	cleanupAttempts  int
}

func (s *ordinaryContentionStore) CompareAndSwap(ctx context.Context, key string, oldValue, newValue []byte, ttl time.Duration) (bool, error) {
	var record ordinaryPoolRecord
	if json.Unmarshal(newValue, &record) == nil && record.State == ordinaryPoolCleanup {
		s.cleanupAttempts++
		if s.beforeCleanupCAS != nil {
			s.beforeCleanupCAS(ctx, key, oldValue)
		}
	}
	return s.AtomicStore.CompareAndSwap(ctx, key, oldValue, newValue, ttl)
}

func (s *ordinaryContentionStore) Get(ctx context.Context, key string) ([]byte, error) {
	if s.readErr != nil && s.cleanupAttempts > 0 {
		return nil, s.readErr
	}
	return s.AtomicStore.Get(ctx, key)
}

func TestOrdinaryPoolLatestStateContentionRefresh(t *testing.T) {
	ordinaryContentionStores(t, func(t *testing.T, backing state.AtomicStore) {
		for _, latestState := range []ordinaryPoolState{ordinaryPoolPreparing, ordinaryPoolPrepared, ordinaryPoolCleanup, ordinaryPoolClaimed} {
			t.Run(string(latestState), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				store := &ordinaryContentionStore{AtomicStore: backing}
				p, r, entry := seedOrdinaryContention(t, store, true)
				r.mu.Lock()
				r.sandboxes[entry.record.RuntimeID].State = "stopped"
				r.mu.Unlock()
				store.beforeCleanupCAS = func(hookCtx context.Context, key string, oldRaw []byte) {
					store.beforeCleanupCAS = nil
					latest := entry.record
					latest.State, latest.Revision = latestState, latest.Revision+1
					latest.UpdatedAt = time.Now().UTC().Add(-ordinaryPoolStaleTTL)
					if latestState == ordinaryPoolClaimed {
						latest.ClaimToken = randSuffix(24)
						latest.ClaimUntil = time.Now().UTC().Add(ordinaryPoolClaimTTL)
					}
					raw, err := json.Marshal(latest)
					require.NoError(t, err)
					swapped, err := backing.CompareAndSwap(hookCtx, key, oldRaw, raw, 0)
					require.NoError(t, err)
					require.True(t, swapped)
				}
				require.NoError(t, p.reconcileInventory(ctx, nil, false))
				records, err := p.listCurrentRecords(ctx)
				require.NoError(t, err)
				r.mu.Lock()
				removed := r.removedIDs[entry.record.RuntimeID]
				r.mu.Unlock()
				if latestState == ordinaryPoolClaimed {
					require.Zero(t, removed, "in-flight claimed runtime must never be cleaned by reconciliation")
					require.Len(t, records, 1)
					require.Equal(t, ordinaryPoolClaimed, records[0].record.State)
				} else {
					require.Equal(t, 1, removed)
					require.Empty(t, records)
				}
			})
		}
	})
}

func TestOrdinaryPoolContentionRetainsInvalidLatestEvidence(t *testing.T) {
	ordinaryContentionStores(t, func(t *testing.T, backing state.AtomicStore) {
		for _, failure := range []string{"corrupt", "state", "uid", "store"} {
			t.Run(failure, func(t *testing.T) {
				ctx := context.Background()
				store := &ordinaryContentionStore{AtomicStore: backing}
				p, r, entry := seedOrdinaryContention(t, store, true)
				r.mu.Lock()
				r.sandboxes[entry.record.RuntimeID].State = "stopped"
				r.mu.Unlock()
				var evidence []byte
				store.beforeCleanupCAS = func(hookCtx context.Context, key string, oldRaw []byte) {
					store.beforeCleanupCAS = nil
					latest := entry.record
					latest.Revision++
					switch failure {
					case "state":
						latest.State = "invalid"
					case "uid":
						latest.RuntimeUID = "replacement-uid"
					case "store":
						store.readErr = errors.New("latest record read unavailable")
					}
					var err error
					evidence, err = json.Marshal(latest)
					require.NoError(t, err)
					if failure == "corrupt" {
						evidence = []byte("not-json")
					}
					swapped, err := backing.CompareAndSwap(hookCtx, key, oldRaw, evidence, 0)
					require.NoError(t, err)
					require.True(t, swapped)
				}
				err := p.reconcileInventory(ctx, nil, false)
				require.Error(t, err)
				want := map[string]string{"corrupt": "corrupt", "state": "corrupt", "uid": "identity changed", "store": "latest record read unavailable"}[failure]
				require.ErrorContains(t, err, want)
				raw, err := backing.Get(ctx, entry.key)
				require.NoError(t, err)
				require.Equal(t, evidence, raw)
				r.mu.Lock()
				require.Empty(t, r.removedIDs)
				r.mu.Unlock()
			})
		}
	})
}

func TestOrdinaryPoolContentionBoundedAndCancelled(t *testing.T) {
	ordinaryContentionStores(t, func(t *testing.T, backing state.AtomicStore) {
		for _, cancelled := range []bool{false, true} {
			t.Run(fmt.Sprintf("cancelled=%t", cancelled), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				store := &ordinaryContentionStore{AtomicStore: backing}
				p, r, entry := seedOrdinaryContention(t, store, true)
				r.mu.Lock()
				r.sandboxes[entry.record.RuntimeID].State = "stopped"
				r.mu.Unlock()
				store.beforeCleanupCAS = func(hookCtx context.Context, key string, oldRaw []byte) {
					var latest ordinaryPoolRecord
					require.NoError(t, json.Unmarshal(oldRaw, &latest))
					latest.Revision++
					latest.UpdatedAt = time.Now().UTC()
					raw, err := json.Marshal(latest)
					require.NoError(t, err)
					swapped, err := backing.CompareAndSwap(hookCtx, key, oldRaw, raw, 0)
					require.NoError(t, err)
					require.True(t, swapped)
					if cancelled {
						cancel()
					}
				}
				err := p.reconcileInventory(ctx, nil, false)
				if cancelled {
					require.ErrorIs(t, err, context.Canceled)
					require.Equal(t, 1, store.cleanupAttempts)
				} else {
					require.ErrorContains(t, err, "contention")
					require.Equal(t, ordinaryPoolSnapshotAttempts, store.cleanupAttempts)
				}
				r.mu.Lock()
				require.Empty(t, r.removedIDs)
				r.mu.Unlock()
				raw, err := backing.Get(context.Background(), entry.key)
				require.NoError(t, err)
				require.NotEmpty(t, raw)
			})
		}
	})
}
