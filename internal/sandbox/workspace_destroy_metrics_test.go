package sandbox

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Preserve the actual cleanup snapshot while synchronizing duplicate requests;
// in particular, local finalizers need not acquire a second controller.
type workspaceDestroySnapshotRepository struct {
	observedCleanupRepository
	snapshots chan struct{}
}

func (r workspaceDestroySnapshotRepository) BeginDestroy(ctx context.Context, id string) (*state.ActiveSandboxRecord, int64, bool, error) {
	record, revision, changed, err := r.ActiveSandboxRepository.BeginDestroy(ctx, id)
	if err == nil && record != nil {
		select {
		case r.snapshots <- struct{}{}:
		case <-ctx.Done():
			return nil, 0, false, ctx.Err()
		}
	}
	return record, revision, changed, err
}

func (r workspaceDestroySnapshotRepository) CheckpointController(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64, checkpoint string) (*state.ActiveSandboxRecord, error) {
	repository, ok := r.ActiveSandboxRepository.(state.ActiveSandboxCheckpointRepository)
	if !ok {
		return nil, state.ErrActiveSandboxCorrupt
	}
	return repository.CheckpointController(ctx, lease, revision, checkpoint)
}

func (r workspaceDestroySnapshotRepository) DeleteController(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64) error {
	repository, ok := r.ActiveSandboxRepository.(state.ActiveSandboxCheckpointRepository)
	if !ok {
		return state.ErrActiveSandboxCorrupt
	}
	return repository.DeleteController(ctx, lease, revision)
}

func (r workspaceDestroySnapshotRepository) DeleteControllerWithResult(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64) (bool, error) {
	if repository, ok := r.ActiveSandboxRepository.(state.ActiveSandboxCleanupCompletionRepository); ok {
		return repository.DeleteControllerWithResult(ctx, lease, revision)
	}
	return false, r.DeleteController(ctx, lease, revision)
}

func workspaceDestroyMetricStores(t *testing.T, test func(*testing.T, state.AtomicStore, state.ActiveSandboxRepository)) {
	t.Helper()
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			if backend == "memory" {
				test(t, newAtomicMemoryStore(), newMemoryActiveRepository())
				return
			}
			addr := os.Getenv("TEST_REDIS_ADDR")
			if addr == "" {
				t.Skip("requires TEST_REDIS_ADDR")
			}
			store, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr, DB: 14})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			repository, err := redisstate.NewActiveSandboxRepository(store, "workspace-destroy-metrics-"+randSuffix(24))
			require.NoError(t, err)
			test(t, workspaceMetricsScopedStore{store: store, prefix: "workspace-metrics-test-" + randSuffix(24) + ":"}, repository)
		})
	}
}

func configureWorkspaceDestroyMetrics(managers []*Manager, store state.AtomicStore, repository state.ActiveSandboxRepository) {
	for _, manager := range managers {
		manager.activeSandboxes = repository
		manager.config.ActiveSandboxes = repository
		manager.config.WorkspaceCoordinator = NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
		manager.SetSessionStore(NewSessionStore(store, time.Hour))
		manager.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	}
}

func workspaceDestroyMetricWaiters(t *testing.T, managers []*Manager, sb *Sandbox, replicas []int) (<-chan error, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	snapshots := make(chan struct{}, len(replicas))
	wrapped := make(map[int]bool)
	for _, replica := range replicas {
		if !wrapped[replica] {
			manager := managers[replica]
			manager.activeSandboxes = workspaceDestroySnapshotRepository{
				observedCleanupRepository: observedCleanupRepository{ActiveSandboxRepository: manager.activeSandboxes},
				snapshots:                 snapshots,
			}
			wrapped[replica] = true
		}
	}
	done := make(chan error, len(replicas))
	for _, replica := range replicas {
		manager := managers[replica]
		go func() { done <- manager.Destroy(ctx, sb.ID) }()
	}
	for range replicas {
		select {
		case <-snapshots:
		case <-ctx.Done():
			t.Fatal("duplicate request did not capture the pending cleanup")
		}
	}
	return done, ctx
}

func TestDistributedSyncDestroyMetricsCountOnlyCompletingWorker(t *testing.T) {
	workspaceDestroyMetricStores(t, func(t *testing.T, store state.AtomicStore, repository state.ActiveSandboxRepository) {
		for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
			t.Run(string(mode), func(t *testing.T) {
				for _, replicas := range []struct {
					name string
					ids  []int
				}{{"same-replica", []int{0, 0}}, {"cross-replica", []int{1, 2}}} {
					t.Run(replicas.name, func(t *testing.T) {
						reader := captureOrdinaryDestroyMetrics(t)
						managers, rt, sb, lifecycle, unblock, ownerDone := blockedDistributedSyncCleanup(t, mode, func(managers []*Manager) {
							configureWorkspaceDestroyMetrics(managers, store, repository)
						})
						waiters, ctx := workspaceDestroyMetricWaiters(t, managers, sb, replicas.ids)
						require.NoError(t, lifecycle.controller.Fence(ctx))
						unblock()
						require.NoError(t, <-ownerDone)
						for range replicas.ids {
							require.NoError(t, <-waiters)
						}
						assertSyncCleanupComplete(t, managers[0], rt, sb)
						assertOrdinaryDestroyMetrics(t, reader)
					})
				}
			})
		}
	})
}

func TestDistributedFUSEDestroyMetricsCountOnlyCompletingWorker(t *testing.T) {
	workspaceDestroyMetricStores(t, func(t *testing.T, store state.AtomicStore, repository state.ActiveSandboxRepository) {
		for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
			t.Run(string(mode), func(t *testing.T) {
				for _, replicas := range []struct {
					name string
					ids  []int
				}{{"same-replica", []int{0, 0}}, {"cross-replica", []int{1, 2}}} {
					t.Run(replicas.name, func(t *testing.T) {
						reader := captureOrdinaryDestroyMetrics(t)
						creator, rt, _ := newDistributedFUSERecoveryManager(t)
						managers := []*Manager{creator}
						for _, id := range []string{"peer-b", "peer-c"} {
							cfg := creator.config
							cfg.InstanceID = id
							peer := NewManager(rt, creator.filesystem, creator.fsMeta, cfg)
							managers = append(managers, peer)
							t.Cleanup(func() { require.NoError(t, peer.Stop(context.Background())) })
						}
						configureWorkspaceDestroyMetrics(managers, store, repository)
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						sb, err := creator.Create(ctx, SandboxConfig{Mode: mode, WorkspacePath: "team/metrics-" + randSuffix(24)})
						require.NoError(t, err)
						entered, release := rt.blockPreparedRemovals(8)
						unblock := func() {
							select {
							case <-release:
							default:
								close(release)
							}
						}
						t.Cleanup(unblock)
						ownerDone := make(chan error, 1)
						go func() { ownerDone <- creator.Destroy(ctx, sb.ID) }()
						select {
						case <-entered:
						case <-ctx.Done():
							t.Fatal("FUSE owner did not reach runtime removal")
						}
						waiters, _ := workspaceDestroyMetricWaiters(t, managers, sb, replicas.ids)
						unblock()
						require.NoError(t, <-ownerDone)
						for range replicas.ids {
							require.NoError(t, <-waiters)
						}
						assertSyncCleanupComplete(t, creator, rt.mockRuntime, sb)
						assertOrdinaryDestroyMetrics(t, reader)
					})
				}
			})
		}
	})
}

func workspaceDestroyMetricTotals(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var collected metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &collected))
	totals := make(map[string]int64)
	for _, scope := range collected.ScopeMetrics {
		for _, instrument := range scope.Metrics {
			sum, ok := instrument.Data.(metricdata.Sum[int64])
			require.True(t, ok)
			for _, point := range sum.DataPoints {
				totals[instrument.Name] += point.Value
			}
		}
	}
	return totals
}

func TestFUSECreateDestroyMetricsBalance(t *testing.T) {
	for _, distributed := range []bool{false, true} {
		t.Run(map[bool]string{false: "local", true: "distributed"}[distributed], func(t *testing.T) {
			for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
				t.Run(string(mode), func(t *testing.T) {
					reader := captureOrdinaryDestroyMetrics(t)
					manager, rt, store := newDistributedFUSERecoveryManager(t)
					manager.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
					if !distributed {
						manager.activeSandboxes = nil
						manager.config.ActiveSandboxes = nil
					}
					sb, err := manager.Create(context.Background(), SandboxConfig{Mode: mode, WorkspacePath: "team/balance-" + randSuffix(24)})
					require.NoError(t, err)
					totals := workspaceDestroyMetricTotals(t, reader)
					require.EqualValues(t, 1, totals["sandbox.active"], "only successful public creation increments activity")
					require.Zero(t, totals["sandbox.destroy.total"])
					require.NoError(t, manager.Destroy(context.Background(), sb.ID))
					require.True(t, rt.wasRemoved(sb.RuntimeID))
					assertOrdinaryDestroyMetrics(t, reader)
				})
			}
		})
	}
}

func TestFailedFUSECreateDoesNotIncrementActivity(t *testing.T) {
	reader := captureOrdinaryDestroyMetrics(t)
	manager, rt, _ := newDistributedFUSERecoveryManager(t)
	rt.waitReadyErr = errors.New("mount readiness failed")
	_, err := manager.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/failed-metrics"})
	require.Error(t, err)
	totals := workspaceDestroyMetricTotals(t, reader)
	require.Zero(t, totals["sandbox.active"])
	require.Zero(t, totals["sandbox.destroy.total"])
}
