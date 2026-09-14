package sandbox

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

// Pause only the first atomic deletion, after activeController already fenced.
// A peer must remain able to finish the same checkpoint during the handoff.
type workspaceDeleteHandoffRepository struct {
	observedCleanupRepository
	state.ActiveSandboxCheckpointRepository
	first   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (r *workspaceDeleteHandoffRepository) DeleteController(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64) error {
	_, err := r.DeleteControllerWithResult(ctx, lease, revision)
	return err
}

func (r *workspaceDeleteHandoffRepository) DeleteControllerWithResult(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64) (bool, error) {
	if r.first.CompareAndSwap(false, true) {
		select {
		case r.entered <- struct{}{}:
		case <-ctx.Done():
			return false, ctx.Err()
		}
		select {
		case <-r.release:
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	if repository, ok := r.ActiveSandboxRepository.(state.ActiveSandboxCleanupCompletionRepository); ok {
		return repository.DeleteControllerWithResult(ctx, lease, revision)
	}
	return false, r.ActiveSandboxCheckpointRepository.DeleteController(ctx, lease, revision)
}

func TestWorkspaceDestroyMetricsControllerHandoffBeforeAtomicDelete(t *testing.T) {
	workspaceDestroyMetricStores(t, func(t *testing.T, store state.AtomicStore, repository state.ActiveSandboxRepository) {
		for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
			t.Run(string(mode), func(t *testing.T) {
				reader := captureOrdinaryDestroyMetrics(t)
				managers, _, rt := distributedSyncManagers(t)
				checkpointRepository, ok := repository.(state.ActiveSandboxCheckpointRepository)
				require.True(t, ok)
				handoff := &workspaceDeleteHandoffRepository{
					observedCleanupRepository:         observedCleanupRepository{ActiveSandboxRepository: repository},
					ActiveSandboxCheckpointRepository: checkpointRepository,
					entered:                           make(chan struct{}, 1), release: make(chan struct{}),
				}
				configureWorkspaceDestroyMetrics(managers, store, handoff)
				for _, manager := range managers {
					manager.runtime = &sharedPoolRuntime{rt}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				unblock := func() {
					select {
					case <-handoff.release:
					default:
						close(handoff.release)
					}
				}
				t.Cleanup(unblock)
				root := "team/handoff-" + randSuffix(24)
				require.NoError(t, managers[0].filesystem.MakeDir(ctx, root, 0o755))
				sb, err := managers[0].Create(ctx, SandboxConfig{Mode: mode, WorkspacePath: root})
				require.NoError(t, err)
				lifecycle := managers[0].syncLifecycles[sb.ID]
				ownerDone := make(chan error, 1)
				go func() { ownerDone <- managers[0].Destroy(ctx, sb.ID) }()
				select {
				case <-handoff.entered:
				case <-ctx.Done():
					t.Fatal("owner did not reach the atomic deletion handoff")
				}
				record, err := repository.Load(ctx, sb.ID)
				require.NoError(t, err)
				require.Equal(t, "sync_runtime_removed", record.CleanupCheckpoint)
				require.NoError(t, lifecycle.controller.Stop(ctx), "release the exact worker capability after its final Fence")
				require.NoError(t, managers[1].Destroy(ctx, sb.ID), "uncached peer finishes the checkpoint while old atomic deletion is paused")
				assertOrdinaryDestroyMetrics(t, reader)
				unblock()
				require.NoError(t, <-ownerDone, "the old worker may observe idempotent absence but must not count a second completion")
				assertSyncCleanupComplete(t, managers[1], rt, sb)
				assertOrdinaryDestroyMetrics(t, reader)
			})
		}
	})
}

// Deliberately hide the optional atomic result capability to represent an
// existing custom implementation of the error-only cleanup contract.
type legacyWorkspaceCleanupRepository struct {
	state.ActiveSandboxRepository
	state.ActiveSandboxCheckpointRepository
}

func TestWorkspaceDestroyLegacyRepositoryDoesNotGuessCompletionMetrics(t *testing.T) {
	workspaceDestroyMetricStores(t, func(t *testing.T, store state.AtomicStore, repository state.ActiveSandboxRepository) {
		reader := captureOrdinaryDestroyMetrics(t)
		managers, _, rt := distributedSyncManagers(t)
		checkpointRepository, ok := repository.(state.ActiveSandboxCheckpointRepository)
		require.True(t, ok)
		legacy := legacyWorkspaceCleanupRepository{repository, checkpointRepository}
		configureWorkspaceDestroyMetrics(managers, store, legacy)
		for _, manager := range managers {
			manager.runtime = &sharedPoolRuntime{rt}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		root := "team/legacy-completion-" + randSuffix(24)
		require.NoError(t, managers[0].filesystem.MakeDir(ctx, root, 0o755))
		sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: root})
		require.NoError(t, err)
		require.NoError(t, managers[0].Destroy(ctx, sb.ID), "legacy cleanup remains available without the optional result capability")
		assertSyncCleanupComplete(t, managers[0], rt, sb)
		totals := workspaceDestroyMetricTotals(t, reader)
		require.EqualValues(t, 1, totals["sandbox.active"], "unknown completion is not guessed as an activity decrement")
		require.Zero(t, totals["sandbox.destroy.total"])
	})
}
