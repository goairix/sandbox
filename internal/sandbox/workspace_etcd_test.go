package sandbox

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
	etcdstate "github.com/goairix/sandbox/internal/storage/state/etcd"
)

func TestWorkspaceRequestRealEtcdConcurrencyAcrossReplicas(t *testing.T) {
	endpoint := os.Getenv("TEST_ETCD_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_ETCD_ENDPOINT is not set")
	}
	ctx := context.Background()
	store, err := etcdstate.New(ctx, etcdstate.Options{
		Endpoints: []string{endpoint}, Prefix: "/workspace-concurrency-test/" + uuid.NewString() + "/",
		Timeout: 3 * time.Second,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	repository, err := etcdstate.NewActiveSandboxRepository(store, "workspace-test")
	require.NoError(t, err)
	managers, _, rt := distributedSyncManagers(t)
	for _, manager := range managers {
		manager.activeSandboxes = repository
		manager.config.ActiveSandboxes = repository
		manager.config.WorkspaceCoordinator = NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
		manager.SetSessionStore(NewSessionStore(store, time.Hour))
		manager.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	}
	root := "workspace-" + uuid.NewString()
	require.NoError(t, managers[0].filesystem.MakeDir(ctx, root, 0o755))

	const requests = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	ids := make([]string, requests)
	errs := make([]error, requests)
	reused := make([]bool, requests)
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			sb, didReuse, requestErr := managers[i%len(managers)].GetOrCreate(requestCtx,
				SandboxConfig{Mode: ModePersistent, WorkspacePath: root})
			errs[i], reused[i] = requestErr, didReuse
			if sb != nil {
				ids[i] = sb.ID
			}
		}()
	}
	close(start)
	wg.Wait()
	created := 0
	for i := range requests {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], ids[i])
		if !reused[i] {
			created++
		}
	}
	require.Equal(t, 1, created)
	require.NotEmpty(t, ids[0])
	_, err = managers[1].Exec(ctx, ids[0], runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	rt.mu.Lock()
	require.Equal(t, 1, rt.created)
	require.Zero(t, rt.removed)
	rt.mu.Unlock()
}

func TestWorkspaceOwnerReadAfterEtcdClientRestart(t *testing.T) {
	endpoint := os.Getenv("TEST_ETCD_ENDPOINT")
	if endpoint == "" {
		t.Skip("TEST_ETCD_ENDPOINT is not set")
	}
	ctx := context.Background()
	options := etcdstate.Options{
		Endpoints: []string{endpoint}, Prefix: "/workspace-restart-test/" + uuid.NewString() + "/",
		Timeout: 3 * time.Second,
	}
	store, err := etcdstate.New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	repository, err := etcdstate.NewActiveSandboxRepository(store, "workspace-test")
	require.NoError(t, err)
	managers, _, _ := distributedSyncManagers(t)
	for _, manager := range managers[:2] {
		manager.activeSandboxes = repository
		manager.config.ActiveSandboxes = repository
		manager.config.WorkspaceCoordinator = NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
		manager.SetSessionStore(NewSessionStore(store, time.Hour))
		manager.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	}
	root := "workspace-" + uuid.NewString()
	require.NoError(t, managers[0].filesystem.MakeDir(ctx, root, 0o755))
	created, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: root})
	require.NoError(t, err)

	// A new client and a Manager with no local sandbox cache model a new API
	// replica after restart. Read-only lookup must not mint a fresh owner.
	reopened, err := etcdstate.New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, reopened.Close()) })
	reopenedRepository, err := etcdstate.NewActiveSandboxRepository(reopened, "workspace-test")
	require.NoError(t, err)
	peer := managers[2]
	peer.activeSandboxes = reopenedRepository
	peer.config.ActiveSandboxes = reopenedRepository
	peer.config.WorkspaceCoordinator = NewWorkspaceCoordinator(reopened, time.Minute, 10*time.Second)
	peer.SetSessionStore(NewSessionStore(reopened, time.Hour))
	peer.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(reopened))
	require.Empty(t, peer.sandboxes)
	got, err := peer.GetByWorkspace(ctx, root)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	keys, err := workspaceStateKeysFromOwner(created.Workspace.Owner)
	require.NoError(t, err)
	ownerBefore, err := store.Get(ctx, keys.owner)
	require.NoError(t, err)
	require.NoError(t, store.Delete(ctx, keys.lease))
	got, err = peer.GetByWorkspace(ctx, root)
	require.NoError(t, err)
	require.Equal(t, created.ID, got.ID)
	ownerAfter, err := reopened.Get(ctx, keys.owner)
	require.NoError(t, err)
	require.Equal(t, ownerBefore, ownerAfter)
	lease, err := reopened.Get(ctx, keys.lease)
	require.NoError(t, err)
	require.Nil(t, lease)
}
