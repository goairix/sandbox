package sandbox

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func distributedSyncManagers(t *testing.T) ([]*Manager, *atomicMemoryStore, *mockRuntime) {
	creator, store, rt := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	repository := newMemoryActiveRepository()
	creator.config.RuntimeType = "kubernetes"
	creator.config.InstanceID = "creator"
	creator.config.ActiveSandboxes = repository
	creator.activeSandboxes = repository
	peers := []*Manager{creator}
	for _, id := range []string{"peer-b", "peer-c"} {
		cfg := creator.config
		cfg.InstanceID = id
		peer := NewManager(rt, creator.filesystem, creator.fsMeta, cfg)
		peer.SetSessionStore(NewSessionStore(store, time.Hour))
		peers = append(peers, peer)
		t.Cleanup(func() { require.NoError(t, peer.Stop(context.Background())) })
	}
	return peers, store, rt
}

func TestDistributedWorkspaceRealRedisAcrossReplicas(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("requires TEST_REDIS_ADDR")
	}
	store, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	repo, err := redisstate.NewActiveSandboxRepository(store, "workspace-test-"+uuid.NewString())
	require.NoError(t, err)
	managers, _, _ := distributedSyncManagers(t)
	for _, manager := range managers {
		manager.activeSandboxes = repo
		manager.config.ActiveSandboxes = repo
		manager.config.WorkspaceCoordinator = NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
		manager.SetSessionStore(NewSessionStore(store, time.Hour))
		manager.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = managers[1].Destroy(cleanupCtx, sb.ID)
	})
	root := "redis-" + uuid.NewString()
	require.NoError(t, managers[0].filesystem.MakeDir(ctx, root, 0o755))
	require.NoError(t, managers[1].MountWorkspace(ctx, sb.ID, root, nil))
	require.NoError(t, managers[2].SyncWorkspace(ctx, sb.ID, "from_container", nil))
	require.NoError(t, managers[0].SyncWorkspace(ctx, sb.ID, "to_container", nil))
	for _, manager := range managers {
		info, err := manager.GetWorkspaceInfo(ctx, sb.ID)
		require.NoError(t, err)
		require.NotNil(t, info)
		require.False(t, info.LastSyncedAt.IsZero())
	}
	require.NoError(t, managers[2].UnmountWorkspace(ctx, sb.ID))
	for _, manager := range managers {
		info, err := manager.GetWorkspaceInfo(ctx, sb.ID)
		require.NoError(t, err)
		require.Nil(t, info)
	}
	require.NoError(t, managers[1].Destroy(ctx, sb.ID))
}

func TestDistributedWorkspaceDynamicMountAcrossReplicas(t *testing.T) {
	managers, store, _ := distributedSyncManagers(t)
	ctx := context.Background()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent})
	require.NoError(t, err)
	require.NoError(t, managers[1].MountWorkspace(ctx, sb.ID, "team/a", []string{"cache"}))
	for _, manager := range managers {
		got, err := manager.Get(ctx, sb.ID)
		require.NoError(t, err)
		require.NotNil(t, got.Workspace)
		require.Equal(t, "team/a", got.Config.WorkspacePath)
		require.Equal(t, WorkspaceMountSync, got.Workspace.MountType)
		require.NotZero(t, got.Workspace.Owner.Generation)
	}
	require.NoError(t, managers[2].SyncWorkspace(ctx, sb.ID, "from_container", nil))
	require.NoError(t, managers[0].SyncWorkspace(ctx, sb.ID, "to_container", nil))
	require.NoError(t, managers[2].UnmountWorkspace(ctx, sb.ID))
	for _, manager := range managers {
		info, e := manager.GetWorkspaceInfo(ctx, sb.ID)
		require.NoError(t, e)
		require.Nil(t, info)
	}
	require.NoError(t, managers[0].Destroy(ctx, sb.ID))
	owners, err := store.Keys(ctx, workspaceOwnerKeyPrefix+"*")
	require.NoError(t, err)
	require.Empty(t, owners)
	leases, err := store.Keys(ctx, workspaceLeaseKeyPrefix+"*")
	require.NoError(t, err)
	require.Empty(t, leases)
}

func TestDistributedWorkspaceCreatedSyncCanSyncOnUncachedPeer(t *testing.T) {
	managers, _, _ := distributedSyncManagers(t)
	ctx := context.Background()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	require.NoError(t, managers[1].SyncWorkspace(ctx, sb.ID, "from_container", nil))
	require.NoError(t, managers[2].SyncWorkspace(ctx, sb.ID, "to_container", nil))
	require.NoError(t, managers[2].UnmountWorkspace(ctx, sb.ID))
	require.NoError(t, managers[1].Destroy(ctx, sb.ID))
}

func TestDistributedWorkspaceFlushAcrossUncachedPeers(t *testing.T) {
	rt := newFUSEManagerRuntime()
	creator, _, _, _ := newFUSETestManager(t, rt)
	repository := newMemoryActiveRepository()
	creator.config.RuntimeType = "kubernetes"
	creator.config.InstanceID = "creator"
	creator.activeSandboxes = repository
	creator.config.ActiveSandboxes = repository
	cfg := creator.config
	cfg.InstanceID = "peer"
	peer := NewManager(rt, creator.filesystem, creator.fsMeta, cfg)
	peer.sessions = creator.sessions
	peer.ephemeral = creator.ephemeral
	t.Cleanup(func() {
		require.NoError(t, peer.Stop(context.Background()))
		require.NoError(t, creator.Stop(context.Background()))
	})
	sb, err := creator.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	require.ErrorIs(t, peer.UnmountWorkspace(context.Background(), sb.ID), ErrFUSEWorkspaceImmutable)
	require.NoError(t, peer.SyncWorkspace(context.Background(), sb.ID, "from_container", nil))
	for _, manager := range []*Manager{creator, peer} {
		info, e := manager.GetWorkspaceInfo(context.Background(), sb.ID)
		require.NoError(t, e)
		require.NotNil(t, info)
		require.True(t, info.Flushed)
		require.NotNil(t, info.LastFlushedAt)
		require.False(t, info.LastSyncedAt.IsZero())
	}
	require.NoError(t, creator.Destroy(context.Background(), sb.ID))
}

func TestDistributedWorkspaceExclusiveWaitsForPeerStream(t *testing.T) {
	managers, _, _ := distributedSyncManagers(t)
	ctx := context.Background()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent})
	require.NoError(t, err)
	_, _, release, err := managers[0].beginDistributedOperation(ctx, sb.ID, state.ActiveOperationData)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- managers[1].MountWorkspace(ctx, sb.ID, "team/a", nil) }()
	select {
	case e := <-done:
		t.Fatalf("mount must drain another replica's reader: %v", e)
	case <-time.After(25 * time.Millisecond):
	}
	release()
	require.NoError(t, <-done)
	result, err := managers[2].Exec(ctx, sb.ID, runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	require.Zero(t, result.ExitCode)
	require.NoError(t, managers[1].Destroy(ctx, sb.ID))
}

type drainingWorkspaceRepository struct {
	*memoryActiveRepository
	entered chan struct{}
	once    sync.Once
}

func (r *drainingWorkspaceRepository) LiveOperations(ctx context.Context, id string) (int64, error) {
	r.once.Do(func() { close(r.entered) })
	return r.memoryActiveRepository.LiveOperations(ctx, id)
}

func TestDistributedWorkspaceRejectsUIDReplacementWhileDraining(t *testing.T) {
	managers, _, rt := distributedSyncManagers(t)
	ctx := context.Background()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent})
	require.NoError(t, err)
	_, _, release, err := managers[0].beginDistributedOperation(ctx, sb.ID, state.ActiveOperationData)
	require.NoError(t, err)
	repository := &drainingWorkspaceRepository{memoryActiveRepository: managers[1].activeSandboxes.(*memoryActiveRepository), entered: make(chan struct{})}
	managers[1].activeSandboxes = repository
	done := make(chan error, 1)
	go func() { done <- managers[1].MountWorkspace(ctx, sb.ID, "team/replaced", nil) }()
	// This barrier occurs after admission's first runtime GET, unlike merely
	// observing the exclusive phase, which can precede that GET.
	select {
	case <-repository.entered:
	case <-time.After(time.Second):
		t.Fatal("workspace operation never entered drain")
	}
	rt.mu.Lock()
	old := *rt.sandboxes[sb.RuntimeID]
	replacement := old
	replacement.RuntimeUID = "replacement-uid"
	rt.sandboxes[sb.RuntimeID] = &replacement
	before := rt.execPipeCalls
	rt.mu.Unlock()
	release()
	require.Error(t, <-done)
	rt.mu.Lock()
	require.Equal(t, before, rt.execPipeCalls, "must never copy into replacement Pod")
	rt.sandboxes[sb.RuntimeID] = &old
	rt.mu.Unlock()
	require.NoError(t, managers[0].Destroy(ctx, sb.ID))
}
