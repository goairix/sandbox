package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

func blockedWorkspaceMetricsCleanup(t *testing.T, mount WorkspaceMountType, mode Mode, store state.AtomicStore, repository state.ActiveSandboxRepository) ([]*Manager, *mockRuntime, *Sandbox, func(), <-chan error) {
	t.Helper()
	if mount == WorkspaceMountSync {
		managers, rt, sb, _, unblock, done := blockedDistributedSyncCleanup(t, mode, func(managers []*Manager) {
			configureWorkspaceDestroyMetrics(managers, store, repository)
		})
		return managers, rt, sb, unblock, done
	}
	creator, rt, _ := newDistributedFUSERecoveryManager(t)
	cfg := creator.config
	cfg.InstanceID = "metrics-absence-peer"
	peer := NewManager(rt, creator.filesystem, creator.fsMeta, cfg)
	t.Cleanup(func() { require.NoError(t, peer.Stop(context.Background())) })
	managers := []*Manager{creator, peer}
	configureWorkspaceDestroyMetrics(managers, store, repository)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	sb, err := creator.Create(ctx, SandboxConfig{Mode: mode, WorkspacePath: "team/metrics-absence-" + randSuffix(24)})
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
	done := make(chan error, 1)
	go func() { done <- creator.Destroy(ctx, sb.ID) }()
	select {
	case id := <-entered:
		require.Equal(t, sb.RuntimeID, id)
	case <-ctx.Done():
		t.Fatal("FUSE cleanup owner did not reach removal")
	}
	return managers, rt.mockRuntime, sb, unblock, done
}

// The legacy memory fixture lacks Redis's record-fenced acquisition. Align
// only this snapshot fixture atomically; Redis still uses its actual Lua CAS.
type historicalWorkspaceMetricsMemoryRepository struct {
	delayedCleanupRepository
	memory *memoryActiveRepository
}

func (r historicalWorkspaceMetricsMemoryRepository) AcquireController(ctx context.Context, lease state.ActiveSandboxControllerLease, ttl time.Duration) (*state.ActiveSandboxControllerLease, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	r.memory.mu.Lock()
	defer r.memory.mu.Unlock()
	record, exists := r.memory.records[lease.SandboxID]
	if !exists || record.Generation != lease.Generation {
		return nil, false, state.ErrActiveSandboxStaleToken
	}
	if current, ok := r.memory.controllers[lease.SandboxID]; ok && current.ExpiresAt.After(time.Now()) {
		return nil, false, nil
	}
	lease.ExpiresAt = time.Now().Add(ttl)
	r.memory.controllers[lease.SandboxID] = lease
	return &lease, true, nil
}

func TestWorkspaceDestroyMetricsHistoricalAbsence(t *testing.T) {
	workspaceDestroyMetricStores(t, func(t *testing.T, store state.AtomicStore, repository state.ActiveSandboxRepository) {
		for _, mount := range []WorkspaceMountType{WorkspaceMountSync, WorkspaceMountFUSE} {
			for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
				for _, failACK := range []bool{false, true} {
					name := string(mount) + "/" + string(mode) + "/" + map[bool]string{false: "durable", true: "unconfirmed"}[failACK]
					t.Run(name, func(t *testing.T) {
						reader := captureOrdinaryDestroyMetrics(t)
						managers, rt, sb, unblock, ownerDone := blockedWorkspaceMetricsCleanup(t, mount, mode, store, repository)
						entered, release := make(chan struct{}, 1), make(chan struct{})
						t.Cleanup(func() {
							select {
							case <-release:
							default:
								close(release)
							}
						})
						delayed := delayedCleanupRepository{
							observedCleanupRepository: observedCleanupRepository{ActiveSandboxRepository: repository},
							entered:                   entered, release: release, failACK: failACK,
						}
						managers[1].activeSandboxes = delayed
						if memory, ok := repository.(*memoryActiveRepository); ok {
							managers[1].activeSandboxes = historicalWorkspaceMetricsMemoryRepository{delayedCleanupRepository: delayed, memory: memory}
						}
						ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
						defer cancel()
						peerDone := make(chan error, 1)
						go func() { peerDone <- managers[1].Destroy(ctx, sb.ID) }()
						select {
						case <-entered:
						case <-ctx.Done():
							t.Fatal("peer did not capture cleanup snapshot")
						}
						unblock()
						require.NoError(t, <-ownerDone)
						close(release)
						peerErr := <-peerDone
						if failACK {
							require.ErrorIs(t, peerErr, state.ErrDurabilityUnconfirmed)
						} else {
							require.NoError(t, peerErr, "historical cleanup must remain idempotent")
						}
						assertSyncCleanupComplete(t, managers[0], rt, sb)
						assertOrdinaryDestroyMetrics(t, reader)
					})
				}
			}
		}
	})
}

func TestWorkspaceDestroyMetricsCanceledWaiter(t *testing.T) {
	workspaceDestroyMetricStores(t, func(t *testing.T, store state.AtomicStore, repository state.ActiveSandboxRepository) {
		for _, mount := range []WorkspaceMountType{WorkspaceMountSync, WorkspaceMountFUSE} {
			for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
				for _, canceled := range []bool{false, true} {
					name := string(mount) + "/" + string(mode) + "/" + map[bool]string{false: "deadline", true: "cancel"}[canceled]
					t.Run(name, func(t *testing.T) {
						reader := captureOrdinaryDestroyMetrics(t)
						managers, rt, sb, unblock, ownerDone := blockedWorkspaceMetricsCleanup(t, mount, mode, store, repository)
						owned := make(chan struct{}, 1)
						managers[1].activeSandboxes = observedCleanupRepository{ActiveSandboxRepository: repository, owned: owned}
						ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
						defer cancel()
						peerDone := make(chan error, 1)
						go func() { peerDone <- managers[1].Destroy(ctx, sb.ID) }()
						select {
						case <-owned:
						case <-ctx.Done():
							t.Fatal("peer did not observe live owner")
						}
						if canceled {
							cancel()
						}
						peerErr := <-peerDone
						unblock()
						require.NoError(t, <-ownerDone, "waiter cancellation must not cancel cleanup owner")
						if canceled {
							require.ErrorIs(t, peerErr, context.Canceled)
						} else {
							require.ErrorIs(t, peerErr, context.DeadlineExceeded)
						}
						assertSyncCleanupComplete(t, managers[0], rt, sb)
						assertOrdinaryDestroyMetrics(t, reader)
					})
				}
			}
		}
	})
}
