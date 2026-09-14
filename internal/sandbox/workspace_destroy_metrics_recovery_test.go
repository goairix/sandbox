package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type workspaceDeleteBarrierRepository struct {
	observedCleanupRepository
	state.ActiveSandboxCheckpointRepository
	entered chan struct{}
	release chan struct{}
}

func (r workspaceDeleteBarrierRepository) DeleteController(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64) error {
	_, err := r.DeleteControllerWithResult(ctx, lease, revision)
	return err
}

func (r workspaceDeleteBarrierRepository) DeleteControllerWithResult(ctx context.Context, lease state.ActiveSandboxControllerLease, revision uint64) (bool, error) {
	select {
	case r.entered <- struct{}{}:
	case <-ctx.Done():
		return false, ctx.Err()
	}
	select {
	case <-r.release:
		if repository, ok := r.ActiveSandboxRepository.(state.ActiveSandboxCleanupCompletionRepository); ok {
			return repository.DeleteControllerWithResult(ctx, lease, revision)
		}
		return false, r.ActiveSandboxCheckpointRepository.DeleteController(ctx, lease, revision)
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func TestDistributedSyncDestroyMetricsAcrossCleanupStages(t *testing.T) {
	workspaceDestroyMetricStores(t, func(t *testing.T, store state.AtomicStore, repository state.ActiveSandboxRepository) {
		for _, stage := range []string{"normal", "sync_runtime_removed", workspaceUnmountSynced, workspaceSyncFromPending} {
			t.Run(stage, func(t *testing.T) {
				for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
					t.Run(string(mode), func(t *testing.T) {
						for _, sameReplica := range []bool{true, false} {
							t.Run(map[bool]string{true: "same-replica", false: "cross-replica"}[sameReplica], func(t *testing.T) {
								reader := captureOrdinaryDestroyMetrics(t)
								managers, _, rt := distributedSyncManagers(t)
								configureWorkspaceDestroyMetrics(managers, store, repository)
								for _, manager := range managers {
									manager.runtime = &sharedPoolRuntime{rt}
								}
								entered, release := make(chan struct{}, 1), make(chan struct{})
								unblock := func() {
									select {
									case <-release:
									default:
										close(release)
									}
								}
								t.Cleanup(unblock)
								if stage == "sync_runtime_removed" {
									checkpointRepository, ok := repository.(state.ActiveSandboxCheckpointRepository)
									require.True(t, ok)
									barrier := workspaceDeleteBarrierRepository{observedCleanupRepository{repository, nil}, checkpointRepository, entered, release}
									configureWorkspaceDestroyMetrics(managers, store, barrier)
								}
								ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
								defer cancel()
								root := "team/stage-" + randSuffix(24)
								require.NoError(t, managers[0].filesystem.MakeDir(ctx, root, 0o755))
								sb, err := managers[0].Create(ctx, SandboxConfig{Mode: mode, WorkspacePath: root})
								require.NoError(t, err)
								owner := 0
								var finishOperation func()
								if stage == "normal" {
									finishOperation, err = managers[0].syncLifecycles[sb.ID].gate.Acquire()
									require.NoError(t, err)
									t.Cleanup(finishOperation)
								} else if stage != "sync_runtime_removed" {
									snapshot, opCtx, endOperation, err := managers[0].beginDistributedWorkspaceOperation(ctx, sb.ID)
									require.NoError(t, err)
									snapshot.WorkspaceTransition = stage
									require.NoError(t, managers[0].persistActiveSandboxUpdate(opCtx, snapshot))
									endOperation()
									lifecycle := managers[0].syncLifecycles[sb.ID]
									managers[0].retireLocalSyncController(sb)
									require.NoError(t, lifecycle.controller.Stop(ctx))
									owner = 1 // uncached peer owns interrupted recovery
								}
								var runtimeEntered <-chan string
								if owner == 1 {
									var runtimeRelease chan struct{}
									runtimeEntered, runtimeRelease = rt.blockPreparedRemovals(8)
									unblock = func() {
										select {
										case <-runtimeRelease:
										default:
											close(runtimeRelease)
										}
									}
									t.Cleanup(unblock)
								}
								ownerDone := make(chan error, 1)
								go func() { ownerDone <- managers[owner].Destroy(ctx, sb.ID) }()
								switch stage {
								case "normal":
									gate := managers[0].syncLifecycles[sb.ID].gate
									require.Eventually(t, func() bool { return !gate.isOpen() }, time.Second, time.Millisecond)
								case "sync_runtime_removed":
									select {
									case <-entered:
									case <-ctx.Done():
										t.Fatal("owner did not reach final record deletion")
									}
								default:
									select {
									case <-runtimeEntered:
									case <-ctx.Done():
										t.Fatal("interrupted owner did not reach runtime removal")
									}
								}
								record, err := managers[owner].activeSandboxes.Load(ctx, sb.ID)
								require.NoError(t, err)
								switch stage {
								case "sync_runtime_removed":
									require.Equal(t, stage, record.CleanupCheckpoint)
								case "normal":
									require.Empty(t, record.CleanupCheckpoint)
								}
								replicas := []int{owner, owner}
								if !sameReplica {
									if owner == 0 {
										replicas = []int{1, 2}
									} else {
										replicas = []int{0, 2}
									}
								}
								waiters, _ := workspaceDestroyMetricWaiters(t, managers, sb, replicas)
								if finishOperation != nil {
									finishOperation()
								} else {
									unblock()
								}
								require.NoError(t, <-ownerDone)
								for range replicas {
									require.NoError(t, <-waiters)
								}
								assertSyncCleanupComplete(t, managers[owner], rt, sb)
								assertOrdinaryDestroyMetrics(t, reader)
							})
						}
					})
				}
			})
		}
	})
}

func TestDistributedSyncDestroyMetricsUncachedCheckpointRecovery(t *testing.T) {
	workspaceDestroyMetricStores(t, func(t *testing.T, store state.AtomicStore, repository state.ActiveSandboxRepository) {
		for _, checkpoint := range []string{"sync_final_output_done", "sync_runtime_removed"} {
			t.Run(checkpoint, func(t *testing.T) {
				for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
					t.Run(string(mode), func(t *testing.T) {
						for _, replicas := range []struct {
							name string
							ids  []int
						}{{"same-replica", []int{1, 1}}, {"cross-replica", []int{0, 2}}} {
							t.Run(replicas.name, func(t *testing.T) {
								reader := captureOrdinaryDestroyMetrics(t)
								managers, _, rt := distributedSyncManagers(t)
								checkpointRepository, ok := repository.(state.ActiveSandboxCheckpointRepository)
								require.True(t, ok)
								entered, release := make(chan struct{}, 1), make(chan struct{})
								barrier := workspaceDeleteBarrierRepository{observedCleanupRepository{repository, nil}, checkpointRepository, entered, release}
								configureWorkspaceDestroyMetrics(managers, store, barrier)
								for _, manager := range managers {
									manager.runtime = &sharedPoolRuntime{rt}
								}
								ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
								defer cancel()
								unblock := func() {
									select {
									case <-release:
									default:
										close(release)
									}
								}
								t.Cleanup(unblock)
								root := "team/uncached-" + randSuffix(24)
								require.NoError(t, managers[0].filesystem.MakeDir(ctx, root, 0o755))
								sb, err := managers[0].Create(ctx, SandboxConfig{Mode: mode, WorkspacePath: root})
								require.NoError(t, err)
								lifecycle := managers[0].syncLifecycles[sb.ID]
								managers[0].retireLocalSyncController(sb)
								require.NoError(t, lifecycle.controller.Stop(ctx))
								record, _, _, err := repository.BeginDestroy(ctx, sb.ID)
								require.NoError(t, err)
								_, err = repository.Checkpoint(ctx, sb.ID, record.Revision, checkpoint)
								require.NoError(t, err)
								require.NoError(t, rt.RemoveSandbox(ctx, sb.RuntimeID), "simulate a crash after exact runtime removal")
								rt.mu.Lock()
								rt.downloadDirErr = errors.New("completed output must not be replayed")
								rt.mu.Unlock()
								ownerDone := make(chan error, 1)
								go func() { ownerDone <- managers[1].Destroy(ctx, sb.ID) }()
								select {
								case <-entered:
								case <-ctx.Done():
									t.Fatal("uncached peer did not reach final record cleanup")
								}
								require.Nil(t, managers[1].syncLifecycles[sb.ID], "checkpoint recovery must not require a restored live runtime")
								waiters, _ := workspaceDestroyMetricWaiters(t, managers, sb, replicas.ids)
								unblock()
								require.NoError(t, <-ownerDone)
								for range replicas.ids {
									require.NoError(t, <-waiters)
								}
								assertSyncCleanupComplete(t, managers[1], rt, sb)
								assertOrdinaryDestroyMetrics(t, reader)
							})
						}
					})
				}
			})
		}
	})
}
