package sandbox

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage"
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
	manager.reapOnce()
	record, err := active.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
	require.True(t, manager.runtime.(*fuseManagerRuntime).wasRemoved(sb.RuntimeID))
	inventory, err = poolRepo.ReadInventory(ctx, "pool-key")
	require.NoError(t, err)
	require.Zero(t, inventory.Consumed)
	require.Zero(t, inventory.Cleanup)
}
