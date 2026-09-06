package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goairix/fs/driver/local"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
)

func newCoordinatedSyncManager(t *testing.T, leaseTTL, renewInterval time.Duration) (*Manager, *atomicMemoryStore, *mockRuntime) {
	t.Helper()
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "team", "a"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "team", "b"), 0o755))
	filesystem, err := local.New(local.Config{RootPath: root})
	require.NoError(t, err)
	store := newAtomicMemoryStore()
	rt := newMockRuntime()
	mgr := NewManager(rt, filesystem, &storage.FileSystemMeta{
		Provider: storage.ProviderMinIO, Bucket: "sandbox", SubPath: "workspaces", StorageIdentity: "storage-primary",
	}, ManagerConfig{
		RuntimeType: "docker", DefaultMountMode: WorkspaceMountSync,
		EnabledMountModes:    map[WorkspaceMountType]bool{WorkspaceMountSync: true},
		WorkspaceCoordinator: NewWorkspaceCoordinator(store, leaseTTL, renewInterval),
		PoolConfig:           PoolConfig{Image: "sandbox:sync"},
	})
	mgr.SetSessionStore(NewSessionStore(store, time.Hour))
	t.Cleanup(func() { mgr.Stop(context.Background()) })
	return mgr, store, rt
}

func TestTwoSyncSandboxesConflictOnSameWorkspace(t *testing.T) {
	mgr, _, _ := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	first, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	require.Equal(t, WorkspaceMountSync, first.Workspace.Owner.MountType)

	_, err = mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorIs(t, err, ErrWorkspaceLeased)
}

func TestSyncSandboxesCanUseDifferentWorkspacePrefixes(t *testing.T) {
	mgr, _, _ := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	_, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	_, err = mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/b"})
	require.NoError(t, err)
}

func TestSyncUnmountReleasesLeaseWithoutStoppingRuntime(t *testing.T) {
	mgr, _, rt := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)

	require.NoError(t, mgr.UnmountWorkspace(context.Background(), sb.ID))
	info, err := rt.GetSandbox(context.Background(), sb.RuntimeID)
	require.NoError(t, err)
	require.NotNil(t, info)
	got, err := mgr.Get(context.Background(), sb.ID)
	require.NoError(t, err)
	assert.Nil(t, got.Workspace)

	prefix, err := storage.BuildWorkspacePrefix("workspaces", "team/a")
	require.NoError(t, err)
	fuseLease, err := mgr.config.WorkspaceCoordinator.Acquire(context.Background(), WorkspaceLeaseRequest{
		MountType: WorkspaceMountFUSE, Provider: "minio", StorageIdentity: "storage-primary", Bucket: "sandbox",
		Prefix: prefix, SandboxID: "fuse-after-sync", Runtime: "docker", RuntimeID: "fuse-runtime",
	})
	require.NoError(t, err)
	require.NoError(t, mgr.config.WorkspaceCoordinator.Release(context.Background(), fuseLease, runtime.TerminationEvidence{}))
}

func TestSyncLeaseLossClosesOperationGate(t *testing.T) {
	mgr, store, _ := newCoordinatedSyncManager(t, 90*time.Millisecond, 20*time.Millisecond)
	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	mgr.mu.RLock()
	gate := mgr.operationGates[sb.ID]
	lifecycle := mgr.syncLifecycles[sb.ID]
	mgr.mu.RUnlock()
	require.NotNil(t, lifecycle)
	store.deleteKey(lifecycle.lease.Key)

	require.Eventually(t, func() bool { return !gate.isOpen() }, time.Second, 5*time.Millisecond)
	_, err = mgr.Exec(context.Background(), sb.ID, runtime.ExecRequest{Command: "true"})
	assert.True(t, errors.Is(err, ErrSandboxNotReady) || errors.Is(err, ErrSandboxNotFound), err)
}

func TestStaleFUSEOwnerBlocksSyncAfterLeaseExpiry(t *testing.T) {
	mgr, store, _ := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	prefix, err := storage.BuildWorkspacePrefix("workspaces", "team/a")
	require.NoError(t, err)
	lease, err := mgr.config.WorkspaceCoordinator.Acquire(context.Background(), WorkspaceLeaseRequest{
		MountType: WorkspaceMountFUSE, Provider: "minio", StorageIdentity: "storage-primary", Bucket: "sandbox",
		Prefix: prefix, SandboxID: "stale-fuse", Runtime: "docker", RuntimeID: "fuse-runtime", RuntimeUID: "fuse-uid",
	})
	require.NoError(t, err)
	_, err = mgr.config.WorkspaceCoordinator.ConsumeMountAttempt(context.Background(), lease, "pool-key")
	require.NoError(t, err)
	store.expireKey(lease.Key)

	_, err = mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorIs(t, err, ErrWorkspaceOwned)
}

func TestSyncRestoreRepublishesExpiredLeaseForExactRuntimeOwner(t *testing.T) {
	first, store, rt := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	sb, err := first.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	first.mu.RLock()
	firstLifecycle := first.syncLifecycles[sb.ID]
	first.mu.RUnlock()
	firstLifecycle.renewal.Stop()
	store.expireKey(firstLifecycle.lease.Key)

	restored := NewManager(rt, first.filesystem, first.fsMeta, ManagerConfig{
		RuntimeType: "docker", DefaultMountMode: WorkspaceMountSync,
		EnabledMountModes:    map[WorkspaceMountType]bool{WorkspaceMountSync: true},
		WorkspaceCoordinator: NewWorkspaceCoordinator(store, time.Minute, 10*time.Second),
		PoolConfig:           PoolConfig{Image: "sandbox:sync"},
	})
	restored.SetSessionStore(NewSessionStore(store, time.Hour))
	t.Cleanup(func() { restored.Stop(context.Background()) })
	require.NoError(t, restored.restorePersistentSandboxes(context.Background()))

	got, err := restored.Get(context.Background(), sb.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Workspace)
	assert.Equal(t, sb.RuntimeUID, got.RuntimeUID)
	assert.Equal(t, sb.Workspace.Owner, got.Workspace.Owner)
	restored.mu.RLock()
	restoredLifecycle := restored.syncLifecycles[sb.ID]
	restored.mu.RUnlock()
	require.NotNil(t, restoredLifecycle)
	assert.Equal(t, firstLifecycle.lease.OwnerSnapshot().Generation, restoredLifecycle.lease.OwnerSnapshot().Generation)
}

func TestDestroyEphemeralSyncRetainsRuntimeAndLeaseWhenFinalSyncFails(t *testing.T) {
	mgr, store, rt := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	ephemeral := NewEphemeralLifecycleStore(store)
	mgr.SetEphemeralLifecycleStore(ephemeral)
	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModeEphemeral, WorkspacePath: "team/a"})
	require.NoError(t, err)
	rt.mu.Lock()
	rt.execFunc = func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return nil, errors.New("manifest unavailable")
	}
	rt.downloadDirErr = errors.New("archive unavailable")
	rt.mu.Unlock()

	err = mgr.Destroy(context.Background(), sb.ID)
	require.ErrorIs(t, err, ErrSandboxCleanupPending)
	assert.False(t, rt.wasRemoved(sb.RuntimeID))
	assert.True(t, ephemeral.Exists(context.Background(), sb.ID))
	keys, keyErr := workspaceStateKeys(WorkspaceLeaseRequest{
		Provider: "minio", StorageIdentity: "storage-primary", Bucket: "sandbox", Prefix: sb.Workspace.Owner.Prefix,
	})
	require.NoError(t, keyErr)
	assert.True(t, store.hasKey(keys.owner))
}

func TestDestroyEphemeralSyncRetriesAfterFinalSyncRecovers(t *testing.T) {
	mgr, store, rt := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	ephemeral := NewEphemeralLifecycleStore(store)
	mgr.SetEphemeralLifecycleStore(ephemeral)
	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModeEphemeral, WorkspacePath: "team/a"})
	require.NoError(t, err)
	rt.mu.Lock()
	rt.execFunc = func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return nil, errors.New("manifest unavailable")
	}
	rt.downloadDirErr = errors.New("archive unavailable")
	rt.mu.Unlock()
	mgr.mu.RLock()
	lifecycle := mgr.syncLifecycles[sb.ID]
	mgr.mu.RUnlock()
	require.ErrorIs(t, mgr.destroySyncSandbox(context.Background(), lifecycle), ErrSandboxCleanupPending)
	rt.mu.Lock()
	rt.execFunc = nil
	rt.downloadDirErr = nil
	rt.mu.Unlock()

	require.NoError(t, mgr.Destroy(context.Background(), sb.ID))
	assert.True(t, rt.wasRemoved(sb.RuntimeID))
	assert.False(t, ephemeral.Exists(context.Background(), sb.ID))
}

func TestStartupFinalizesDestroyingPersistentSyncWithoutPublishing(t *testing.T) {
	first, store, rt := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	sb, err := first.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	rt.mu.Lock()
	rt.execFunc = func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return nil, errors.New("manifest unavailable")
	}
	rt.downloadDirErr = errors.New("archive unavailable")
	rt.mu.Unlock()
	first.mu.RLock()
	lifecycle := first.syncLifecycles[sb.ID]
	first.mu.RUnlock()
	require.ErrorIs(t, first.destroySyncSandbox(context.Background(), lifecycle), ErrSandboxCleanupPending)
	lifecycle.renewal.Stop()
	store.expireKey(lifecycle.lease.Key)
	rt.mu.Lock()
	rt.execFunc = nil
	rt.downloadDirErr = nil
	rt.mu.Unlock()

	restored := NewManager(rt, first.filesystem, first.fsMeta, ManagerConfig{
		RuntimeType: "docker", DefaultMountMode: WorkspaceMountSync,
		EnabledMountModes:    map[WorkspaceMountType]bool{WorkspaceMountSync: true},
		WorkspaceCoordinator: NewWorkspaceCoordinator(store, time.Minute, 10*time.Second),
		PoolConfig:           PoolConfig{Image: "sandbox:sync"},
	})
	restored.SetSessionStore(NewSessionStore(store, time.Hour))
	require.NoError(t, restored.restorePersistentSandboxes(context.Background()))
	_, err = restored.Get(context.Background(), sb.ID)
	require.ErrorIs(t, err, ErrSandboxNotFound)
	assert.True(t, rt.wasRemoved(sb.RuntimeID))
}
