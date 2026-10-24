package sandbox

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
	etcdstate "github.com/goairix/sandbox/internal/storage/state/etcd"
)

func newEtcdFUSEManager(t *testing.T) (*Manager, *etcdstate.ActiveSandboxRepository) {
	t.Helper()
	endpoint := os.Getenv("TEST_ETCD_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_ETCD_ENDPOINT is not set")
	}
	ctx := context.Background()
	store, err := etcdstate.New(ctx, etcdstate.Options{
		Endpoints: []string{endpoint}, Prefix: "/fuse-manager-test/" + uuid.NewString() + "/",
		Timeout: 3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	active, err := etcdstate.NewActiveSandboxRepository(store, "fuse-manager-test")
	require.NoError(t, err)
	rt := newFUSEManagerRuntime()
	poolRepo := etcdstate.NewFUSEPoolRepository(store)
	pool := NewFUSEPool(rt, poolRepo, fusePoolConfig(), fixedFUSESpec("pool-key"))
	profile, err := storage.RootMarkerProfileByID(storage.RootMarkerProfileMinIO)
	require.NoError(t, err)
	manager := NewManager(rt, nil, &storage.FileSystemMeta{
		Provider: storage.ProviderMinIO, Bucket: "sandbox", SubPath: "workspaces", StorageIdentity: "storage-primary",
	}, ManagerConfig{
		RuntimeType: "kubernetes", InstanceID: "api-a",
		DefaultMountMode:  WorkspaceMountFUSE,
		EnabledMountModes: map[WorkspaceMountType]bool{WorkspaceMountFUSE: true},
		ActiveSandboxes:   active, FUSEPool: pool,
		WorkspaceCoordinator:  NewWorkspaceCoordinator(store, time.Minute, 10*time.Second),
		WorkspaceObjectClient: &fuseMarkerClient{exists: true}, WorkspaceMarkerProfile: profile,
		FUSEHealthInterval: time.Hour,
	})
	manager.SetSessionStore(NewSessionStore(store, time.Hour))
	manager.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	require.NoError(t, pool.WarmUp(ctx))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	return manager, active
}

func TestManagerFUSECreateDestroyWithEtcdState(t *testing.T) {
	manager, active := newEtcdFUSEManager(t)
	ctx := context.Background()
	sb, err := manager.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	require.NotEmpty(t, sb.ID)
	require.NoError(t, manager.Destroy(ctx, sb.ID))
	record, err := active.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
}

func TestManagerFUSETimeoutCleanupWithEtcdState(t *testing.T) {
	manager, active := newEtcdFUSEManager(t)
	ctx := context.Background()
	sb, err := manager.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/ttl", Timeout: 1})
	require.NoError(t, err)
	poolRepo := manager.fusePool.repo.(*etcdstate.FUSEPoolRepository)
	inventory, err := poolRepo.ReadInventory(ctx, "pool-key")
	require.NoError(t, err)
	require.EqualValues(t, 1, inventory.Consumed)
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, manager.reconcileActiveLifecycleControllers(ctx))
	require.Eventually(t, func() bool {
		record, loadErr := active.Load(ctx, sb.ID)
		return loadErr == nil && record == nil
	}, 10*time.Second, 50*time.Millisecond)
	require.True(t, manager.runtime.(*fuseManagerRuntime).wasRemoved(sb.RuntimeID))
	inventory, err = poolRepo.ReadInventory(ctx, "pool-key")
	require.NoError(t, err)
	require.Zero(t, inventory.Consumed)
	require.Zero(t, inventory.Cleanup)
}

func TestManagerFUSEDurableTimeoutClosesAdmissionOnEtcd(t *testing.T) {
	manager, active := newEtcdFUSEManager(t)
	ctx := context.Background()
	sb, err := manager.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/durable-ttl", Timeout: 1})
	require.NoError(t, err)

	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, manager.reconcileActiveLifecycleControllers(ctx))
	record, err := active.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.True(t, record == nil || record.Phase != state.ActiveSandboxActive,
		"durable TTL must close admission without relying on the process-local reaper")
}

func TestManagerFUSEOldTTLScanCannotDestroyRenewedSandboxOnEtcd(t *testing.T) {
	manager, active := newEtcdFUSEManager(t)
	ctx := context.Background()
	sb, err := manager.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/renewed-ttl", Timeout: 1})
	require.NoError(t, err)
	old, err := active.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.NotNil(t, old)

	_, err = manager.UpdateTTL(ctx, sb.ID, 60)
	require.NoError(t, err)
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, manager.reconcileActiveLifecycle(ctx, old))
	current, err := active.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Equal(t, state.ActiveSandboxActive, current.Phase)
}

func TestManagerFUSETimeoutKeepsOccupationUntilRemovalOnEtcd(t *testing.T) {
	manager, active := newEtcdFUSEManager(t)
	ctx := context.Background()
	sb, err := manager.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/blocked-ttl", Timeout: 1})
	require.NoError(t, err)
	rt := manager.runtime.(*fuseManagerRuntime)
	rt.mockRuntime.mu.Lock()
	rt.removeFailures[sb.RuntimeID] = errors.New("runtime removal unavailable")
	rt.mockRuntime.mu.Unlock()
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, manager.reconcileActiveLifecycleControllers(ctx))
	record, err := active.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.True(t, record != nil, "failed physical removal must retain durable cleanup state")
	inventory, err := manager.fusePool.repo.(*etcdstate.FUSEPoolRepository).ReadInventory(ctx, "pool-key")
	require.NoError(t, err)
	require.EqualValues(t, 1, inventory.Consumed+inventory.Cleanup,
		"failure must not refund the sandbox's occupied pool capacity")

	rt.mockRuntime.mu.Lock()
	delete(rt.removeFailures, sb.RuntimeID)
	rt.mockRuntime.mu.Unlock()
	require.NoError(t, manager.Destroy(ctx, sb.ID))
	record, err = active.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
}
