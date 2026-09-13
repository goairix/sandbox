package sandbox

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

type disappearingOrdinaryRuntime struct {
	missingAwareRuntime
	getCalls int
	cleaned  runtime.RuntimeRef
}

type failedFinalDeleteRepository struct{ *memoryActiveRepository }

func (r failedFinalDeleteRepository) DeleteController(context.Context, state.ActiveSandboxControllerLease, uint64) error {
	return errors.New("final delete unavailable")
}

func (r *disappearingOrdinaryRuntime) GetSandbox(context.Context, string) (*runtime.SandboxInfo, error) {
	r.getCalls++
	if r.getCalls == 1 {
		return &runtime.SandboxInfo{RuntimeID: "pod-a", RuntimeUID: "uid-a"}, nil
	}
	return nil, runtime.ErrNotFound
}

func (r *disappearingOrdinaryRuntime) RemoveOrdinarySandbox(context.Context, runtime.RuntimeRef) error {
	return runtime.ErrNotFound
}

func (r *disappearingOrdinaryRuntime) CleanupOrdinarySandboxPolicies(_ context.Context, ref runtime.RuntimeRef, _ string) error {
	r.cleaned = ref
	return errors.New("policy cleanup retry required")
}

func TestOrdinaryRuntimeDisappearingDuringRemovalStillCleansExactPolicies(t *testing.T) {
	rt := &disappearingOrdinaryRuntime{missingAwareRuntime: missingAwareRuntime{newMockRuntime()}}
	m := NewManager(rt, nil, nil, ManagerConfig{})
	err := m.removeExactOrdinaryRuntime(context.Background(), &Sandbox{ID: "sandbox-a", RuntimeID: "pod-a", RuntimeUID: "uid-a"})
	require.EqualError(t, err, "policy cleanup retry required")
	require.Equal(t, runtime.RuntimeRef{ID: "pod-a", UID: "uid-a"}, rt.cleaned)
}

type missingAwareRuntime struct{ *mockRuntime }

func (r missingAwareRuntime) GetSandbox(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	info, err := r.mockRuntime.GetSandbox(ctx, id)
	if info == nil && err == nil {
		return nil, runtime.ErrNotFound
	}
	return info, err
}

func TestDistributedOrdinaryCleanupRecoveredWithoutLocalCache(t *testing.T) {
	managers, _, rt := distributedSyncManagers(t)
	for _, manager := range managers {
		manager.runtime = missingAwareRuntime{rt}
	}
	ctx := context.Background()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent})
	require.NoError(t, err)
	record, _, _, err := managers[0].activeSandboxes.BeginDestroy(ctx, sb.ID)
	require.NoError(t, err)
	require.NoError(t, managers[2].reconcileActiveLifecycle(ctx, record))
	record, err = managers[2].activeSandboxes.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Nil(t, record, "a peer must recover ordinary durable cleanup")
	_, err = managers[2].runtime.GetSandbox(ctx, sb.RuntimeID)
	require.ErrorIs(t, err, runtime.ErrNotFound)
}

func TestDistributedOrdinaryCleanupRecoversAlreadyAbsentRuntime(t *testing.T) {
	managers, _, rt := distributedSyncManagers(t)
	for _, manager := range managers {
		manager.runtime = missingAwareRuntime{rt}
	}
	ctx := context.Background()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent})
	require.NoError(t, err)
	require.NoError(t, rt.RemoveSandbox(ctx, sb.RuntimeID))
	require.NoError(t, managers[2].Destroy(ctx, sb.ID))
	record, err := managers[2].activeSandboxes.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
}

func TestDistributedWorkspaceAbandonedMountIntentIsRecoverable(t *testing.T) {
	managers, store, _ := distributedSyncManagers(t)
	ctx := context.Background()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent})
	require.NoError(t, err)
	snapshot, opCtx, release, err := managers[1].beginDistributedWorkspaceOperation(ctx, sb.ID)
	require.NoError(t, err)
	snapshot.Workspace = &WorkspaceInfo{RootPath: "team/crash", MountType: WorkspaceMountSync, MountState: WorkspaceMountMounting}
	snapshot.Config.WorkspacePath = "team/crash"
	snapshot.WorkspaceTransition = workspaceMountPreparing
	require.NoError(t, managers[1].persistActiveSandboxUpdate(opCtx, snapshot))
	req, err := managers[1].workspaceLeaseRequest(snapshot, snapshot.Workspace.RootPath)
	require.NoError(t, err)
	_, err = managers[1].config.WorkspaceCoordinator.Acquire(ctx, req)
	require.NoError(t, err)
	// Simulate a lost response/crash before owner generation was published.
	release()
	record, err := managers[2].activeSandboxes.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Equal(t, state.ActiveSandboxCleanupPending, record.Phase)
	require.NoError(t, managers[2].reconcileActiveLifecycle(ctx, record))
	record, err = managers[2].activeSandboxes.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
	keys, err := store.Keys(ctx, workspaceOwnerKeyPrefix+"*")
	require.NoError(t, err)
	require.Empty(t, keys, "pending mount must not retain a renewable owner")
}

func TestDistributedPersistentSyncRecoversCrashAfterRuntimeRemoval(t *testing.T) {
	managers, store, rt := distributedSyncManagers(t)
	for _, manager := range managers {
		manager.runtime = missingAwareRuntime{rt}
	}
	ctx := context.Background()
	require.NoError(t, managers[0].filesystem.MakeDir(ctx, "team/final-sync-crash", 0o755))
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/final-sync-crash"})
	require.NoError(t, err)
	lifecycle := managers[0].syncLifecycles[sb.ID]
	managers[0].retireLocalSyncController(sb)
	require.NoError(t, lifecycle.controller.Stop(ctx))
	record, _, _, err := managers[0].activeSandboxes.BeginDestroy(ctx, sb.ID)
	require.NoError(t, err)
	_, err = managers[0].activeSandboxes.Checkpoint(ctx, sb.ID, record.Revision, "sync_final_output_done")
	require.NoError(t, err)
	require.NoError(t, rt.RemoveSandbox(ctx, sb.RuntimeID))
	// Final output was durable before Pod deletion; a peer must not require
	// a running container or try to copy its output a second time.
	require.NoError(t, managers[2].Destroy(ctx, sb.ID))
	record, err = managers[2].activeSandboxes.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
	owners, err := store.Keys(ctx, workspaceOwnerKeyPrefix+"*")
	require.NoError(t, err)
	require.Empty(t, owners)
}

func TestDistributedSyncRecoveryPreservesRequestExcludes(t *testing.T) {
	managers, _, rt := distributedSyncManagers(t)
	for _, manager := range managers {
		manager.runtime = missingAwareRuntime{rt}
	}
	ctx := context.Background()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	file, err := managers[0].filesystem.Create(ctx, "team/a/keep.txt")
	require.NoError(t, err)
	_, err = io.WriteString(file, "must survive recovery")
	require.NoError(t, err)
	require.NoError(t, file.Close())
	rt.mu.Lock()
	rt.execFunc = func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return nil, errors.New("manifest unavailable")
	}
	rt.downloadDirErr = errors.New("download unavailable")
	rt.mu.Unlock()
	require.ErrorIs(t, managers[1].SyncWorkspace(ctx, sb.ID, "from_container", []string{"keep.txt"}), ErrSandboxCleanupPending)
	record, err := managers[2].activeSandboxes.Load(ctx, sb.ID)
	require.NoError(t, err)
	snapshot, err := decodeActiveSandboxPhase(record, sb.ID, state.ActiveSandboxDestroying, state.ActiveSandboxCleanupPending)
	require.NoError(t, err)
	require.Contains(t, snapshot.WorkspaceTransitionExclude, "keep.txt")
	rt.mu.Lock()
	rt.execFunc = nil
	rt.downloadDirErr = nil
	rt.mu.Unlock()
	lifecycle := managers[0].syncLifecycles[sb.ID]
	managers[0].retireLocalSyncController(sb)
	require.NoError(t, lifecycle.controller.Stop(ctx))
	require.NoError(t, managers[2].reconcileActiveLifecycle(ctx, record))
	exists, err := managers[0].filesystem.Exists(ctx, "team/a/keep.txt")
	require.NoError(t, err)
	require.True(t, exists, "recovery must not delete a path excluded by the original request")
}

func TestDistributedSyncFinalDeleteFailurePreservesPeerRecoveryProof(t *testing.T) {
	managers, _, rt := distributedSyncManagers(t)
	for _, manager := range managers {
		manager.runtime = missingAwareRuntime{rt}
	}
	base := managers[0].activeSandboxes.(*memoryActiveRepository)
	managers[0].activeSandboxes = failedFinalDeleteRepository{base}
	ctx := context.Background()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	require.ErrorContains(t, managers[0].Destroy(ctx, sb.ID), "final delete unavailable")
	record, err := base.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Equal(t, "sync_runtime_removed", record.CleanupCheckpoint, "failed deletion must not erase recovery proof")
	require.NotNil(t, managers[0].syncLifecycles[sb.ID], "failed final CAS must retain the locally tracked controller")
	require.NoError(t, managers[0].Stop(ctx), "shutdown must relinquish the failed finalizer's capability")
	require.NoError(t, managers[2].Destroy(ctx, sb.ID))
	record, err = base.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
}
