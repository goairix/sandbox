package sandbox

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func ordinaryCleanupRepositories(t *testing.T, test func(*testing.T, state.ActiveSandboxRepository)) {
	t.Helper()
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			var repository state.ActiveSandboxRepository
			if backend == "redis" {
				addr := os.Getenv("TEST_REDIS_ADDR")
				if addr == "" {
					t.Skip("requires TEST_REDIS_ADDR")
				}
				store, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr, DB: 14})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				repository, err = redisstate.NewActiveSandboxRepository(store, "ordinary-cleanup-"+randSuffix(24))
				require.NoError(t, err)
			}
			test(t, repository)
		})
	}
}

func blockedDistributedOrdinaryCleanup(t *testing.T, mode Mode, repository state.ActiveSandboxRepository) ([]*Manager, *mockRuntime, *Sandbox, func(), <-chan error) {
	t.Helper()
	managers, _, rt := distributedSyncManagers(t)
	for _, manager := range managers {
		manager.runtime = &sharedPoolRuntime{rt}
		if repository != nil {
			manager.activeSandboxes = repository
			manager.config.ActiveSandboxes = repository
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: mode})
	require.NoError(t, err)
	require.Nil(t, sb.Workspace)
	entered, release := rt.blockPreparedRemovals(8)
	unblock := func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}
	t.Cleanup(unblock)
	done := make(chan error, 1)
	go func() { done <- managers[0].Destroy(ctx, sb.ID) }()
	select {
	case id := <-entered:
		require.Equal(t, sb.RuntimeID, id)
	case <-ctx.Done():
		t.Fatal("ordinary DELETE did not reach exact runtime removal")
	}
	return managers, rt, sb, unblock, done
}

func assertOrdinaryCleanupComplete(t *testing.T, manager *Manager, rt *mockRuntime, sb *Sandbox) {
	t.Helper()
	record, err := manager.activeSandboxes.Load(context.Background(), sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
	if sb.Config.Mode == ModePersistent {
		_, err = manager.sessions.Load(context.Background(), sb.ID)
		require.ErrorIs(t, err, ErrSandboxNotFound)
	}
	rt.mu.Lock()
	count := rt.removedIDs[sb.RuntimeID]
	_, exists := rt.sandboxes[sb.RuntimeID]
	rt.mu.Unlock()
	require.Equal(t, 1, count, "only the fenced owner may delete the runtime")
	require.False(t, exists)
}

func captureOrdinaryDestroyMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	meter := provider.Meter(t.Name())
	active, err := meter.Int64UpDownCounter("sandbox.active")
	require.NoError(t, err)
	destroyed, err := meter.Int64Counter("sandbox.destroy.total")
	require.NoError(t, err)
	oldActive, oldDestroyed := metrics.SandboxActiveGauge, metrics.SandboxDestroyTotal
	metrics.SandboxActiveGauge, metrics.SandboxDestroyTotal = active, destroyed
	t.Cleanup(func() {
		metrics.SandboxActiveGauge, metrics.SandboxDestroyTotal = oldActive, oldDestroyed
		require.NoError(t, provider.Shutdown(context.Background()))
	})
	return reader
}

func assertOrdinaryDestroyMetrics(t *testing.T, reader *sdkmetric.ManualReader) {
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
	require.Contains(t, totals, "sandbox.active")
	require.Contains(t, totals, "sandbox.destroy.total")
	require.Zero(t, totals["sandbox.active"], "one created sandbox must have one activity decrement")
	require.EqualValues(t, 1, totals["sandbox.destroy.total"], "waiters must not count additional destructions")
}

func TestDistributedOrdinaryCleanupConcurrentDeletesWaitForOwner(t *testing.T) {
	ordinaryCleanupRepositories(t, func(t *testing.T, repository state.ActiveSandboxRepository) {
		for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
			t.Run(string(mode), func(t *testing.T) {
				for _, replicas := range []struct {
					name string
					ids  []int
				}{{"cross-replica", []int{1, 2}}, {"same-replica", []int{0, 0}}} {
					t.Run(replicas.name, func(t *testing.T) {
						reader := captureOrdinaryDestroyMetrics(t)
						managers, rt, sb, unblock, ownerDone := blockedDistributedOrdinaryCleanup(t, mode, repository)
						ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
						defer cancel()
						owned := make(chan struct{}, 2)
						peers := make(chan error, 2)
						wrapped := make(map[int]bool)
						for _, replica := range replicas.ids {
							if !wrapped[replica] {
								manager := managers[replica]
								manager.activeSandboxes = observedCleanupRepository{manager.activeSandboxes, owned}
								wrapped[replica] = true
							}
						}
						for _, replica := range replicas.ids {
							manager := managers[replica]
							go func() { peers <- manager.Destroy(ctx, sb.ID) }()
						}
						for range 2 {
							select {
							case <-owned:
							case <-ctx.Done():
								t.Fatal("peer did not check the live cleanup owner")
							}
						}
						unblock()
						require.NoError(t, <-ownerDone)
						for range 2 {
							require.NoError(t, <-peers, "concurrent DELETE must share confirmed cleanup success")
						}
						assertOrdinaryCleanupComplete(t, managers[0], rt, sb)
						assertOrdinaryDestroyMetrics(t, reader)
					})
				}
			})
		}
	})
}

func TestDistributedOrdinaryCleanupDuplicateHonorsContext(t *testing.T) {
	for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
		for _, replica := range []int{0, 1, 2} {
			for _, canceled := range []bool{false, true} {
				name := string(mode) + "/" + []string{"owner", "peer-b", "peer-c"}[replica] + "/" + map[bool]string{false: "deadline", true: "cancel"}[canceled]
				t.Run(name, func(t *testing.T) {
					managers, rt, sb, unblock, ownerDone := blockedDistributedOrdinaryCleanup(t, mode, nil)
					ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
					defer cancel()
					owned := make(chan struct{}, 1)
					managers[replica].activeSandboxes = observedCleanupRepository{managers[replica].activeSandboxes, owned}
					peerDone := make(chan error, 1)
					go func() { peerDone <- managers[replica].Destroy(ctx, sb.ID) }()
					select {
					case <-owned:
					case <-ctx.Done():
						t.Fatal("duplicate did not check cleanup ownership")
					}
					if canceled {
						cancel()
					}
					peerErr := <-peerDone
					unblock()
					require.NoError(t, <-ownerDone, "duplicate cancellation must not cancel the owner")
					if canceled {
						require.ErrorIs(t, peerErr, context.Canceled)
					} else {
						require.ErrorIs(t, peerErr, context.DeadlineExceeded)
					}
					require.ErrorIs(t, peerErr, ErrSandboxCleanupPending)
					assertOrdinaryCleanupComplete(t, managers[0], rt, sb)
				})
			}
		}
	}
}

func TestDistributedOrdinaryCleanupScanSkipsLiveOwner(t *testing.T) {
	for _, replica := range []int{0, 1, 2} {
		t.Run([]string{"owner", "peer-b", "peer-c"}[replica], func(t *testing.T) {
			managers, rt, sb, unblock, ownerDone := blockedDistributedOrdinaryCleanup(t, ModePersistent, nil)
			record, err := managers[replica].activeSandboxes.Load(context.Background(), sb.ID)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			scanErr := managers[replica].reconcileActiveLifecycle(ctx, record)
			unblock()
			require.NoError(t, <-ownerDone)
			require.NoError(t, scanErr, "background scan must skip a live owner without waiting or logging false failure")
			assertOrdinaryCleanupComplete(t, managers[0], rt, sb)
		})
	}
}

func TestDistributedOrdinaryCleanupWaitRequiresDurableAbsence(t *testing.T) {
	managers, rt, sb, unblock, ownerDone := blockedDistributedOrdinaryCleanup(t, ModePersistent, nil)
	owned := make(chan struct{}, 1)
	managers[1].activeSandboxes = failedCleanupAbsenceRepository{observedCleanupRepository{managers[1].activeSandboxes, owned}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	peerDone := make(chan error, 1)
	go func() { peerDone <- managers[1].Destroy(ctx, sb.ID) }()
	select {
	case <-owned:
	case <-ctx.Done():
		t.Fatal("peer did not check cleanup ownership")
	}
	unblock()
	require.NoError(t, <-ownerDone)
	require.ErrorIs(t, <-peerDone, state.ErrDurabilityUnconfirmed)
	assertOrdinaryCleanupComplete(t, managers[0], rt, sb)
}

func TestDistributedOrdinaryCleanupFailureRetainsStateForRecovery(t *testing.T) {
	managers, rt, sb, unblock, ownerDone := blockedDistributedOrdinaryCleanup(t, ModePersistent, nil)
	rt.mu.Lock()
	rt.removeFailures[sb.RuntimeID] = runtime.ErrTerminationUnconfirmed
	rt.mu.Unlock()
	owned := make(chan struct{}, 1)
	managers[1].activeSandboxes = observedCleanupRepository{managers[1].activeSandboxes, owned}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	peerDone := make(chan error, 1)
	go func() { peerDone <- managers[1].Destroy(ctx, sb.ID) }()
	select {
	case <-owned:
	case <-ctx.Done():
		t.Fatal("peer did not check cleanup ownership")
	}
	unblock()
	ownerErr := <-ownerDone
	peerErr := <-peerDone
	require.ErrorIs(t, ownerErr, runtime.ErrTerminationUnconfirmed)
	require.ErrorIs(t, peerErr, ErrSandboxCleanupPending)
	require.ErrorIs(t, peerErr, context.DeadlineExceeded, "a failed owner must not produce false cleanup success")
	record, err := managers[2].activeSandboxes.Load(context.Background(), sb.ID)
	require.NoError(t, err)
	require.NotNil(t, record, "keep the recovery proof until runtime termination is confirmed")
	rt.mu.Lock()
	delete(rt.removeFailures, sb.RuntimeID)
	rt.mu.Unlock()
	require.NoError(t, managers[2].reconcileActiveLifecycle(context.Background(), record))
	assertOrdinaryCleanupComplete(t, managers[2], rt, sb)
}

func TestDistributedOrdinaryCleanupRecordGoneBeforeAcquire(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("requires TEST_REDIS_ADDR")
	}
	store, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr, DB: 14})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for _, failACK := range []bool{false, true} {
		t.Run(map[bool]string{false: "durable", true: "unconfirmed"}[failACK], func(t *testing.T) {
			reader := captureOrdinaryDestroyMetrics(t)
			repo, err := redisstate.NewActiveSandboxRepository(store, "ordinary-gone-"+randSuffix(24))
			require.NoError(t, err)
			managers, rt, sb, unblock, ownerDone := blockedDistributedOrdinaryCleanup(t, ModePersistent, repo)
			entered, release := make(chan struct{}, 1), make(chan struct{})
			resumePeer := func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}
			t.Cleanup(resumePeer)
			managers[1].activeSandboxes = delayedCleanupRepository{observedCleanupRepository{repo, nil}, entered, release, failACK}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			peerDone := make(chan error, 1)
			go func() { peerDone <- managers[1].Destroy(ctx, sb.ID) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("peer did not obtain its cleanup snapshot")
			}
			unblock()
			require.NoError(t, <-ownerDone)
			resumePeer()
			peerErr := <-peerDone
			if failACK {
				require.ErrorIs(t, peerErr, state.ErrDurabilityUnconfirmed)
			} else {
				require.NoError(t, peerErr, "confirmed cleanup between snapshot and acquisition is idempotent")
			}
			assertOrdinaryCleanupComplete(t, managers[0], rt, sb)
			assertOrdinaryDestroyMetrics(t, reader)
		})
	}
}

func TestDistributedOrdinaryCleanupStaleAcquisitionWithRecordStillFails(t *testing.T) {
	managers, rt, sb, unblock, ownerDone := blockedDistributedOrdinaryCleanup(t, ModePersistent, nil)
	managers[1].activeSandboxes = staleCleanupAcquireRepository{managers[1].activeSandboxes}
	peerErr := managers[1].Destroy(context.Background(), sb.ID)
	unblock()
	require.NoError(t, <-ownerDone)
	require.ErrorIs(t, peerErr, state.ErrActiveSandboxStaleToken)
	assertOrdinaryCleanupComplete(t, managers[0], rt, sb)
}
