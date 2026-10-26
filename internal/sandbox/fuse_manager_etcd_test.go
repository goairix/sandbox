package sandbox

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
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

func TestManagerOrdinaryTimeoutViaEtcdDueIndex(t *testing.T) {
	manager, active := newEtcdFUSEManager(t)
	ctx := context.Background()
	sb, err := manager.Create(ctx, SandboxConfig{Mode: ModeEphemeral, Timeout: 1})
	require.NoError(t, err)
	require.Nil(t, sb.Workspace)

	page, err := active.ScanDue(ctx, time.Now().Add(2*time.Second), "", 10)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	require.Equal(t, sb.ID, page.Records[0].SandboxID)
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, manager.reconcileActiveLifecycleControllers(ctx))
	require.Eventually(t, func() bool {
		record, loadErr := active.Load(ctx, sb.ID)
		return loadErr == nil && record == nil
	}, 10*time.Second, 50*time.Millisecond)
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

func TestManagerRecoversTerminatedFUSEWithStrongShutdownProofOnEtcd(t *testing.T) {
	testManagerRecoversTerminatedFUSEWithProof(t, true)
}

func TestManagerRetainsTerminatedFUSEWithoutShutdownProofOnEtcd(t *testing.T) {
	testManagerRecoversTerminatedFUSEWithProof(t, false)
}

func testManagerRecoversTerminatedFUSEWithProof(t *testing.T, confirmed bool) {
	t.Helper()
	manager, active := newEtcdFUSEManager(t)
	ctx := context.Background()
	sb, err := manager.Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/terminal-proof"})
	require.NoError(t, err)
	store := manager.sessions.store.(*etcdstate.Store)
	poolRepo := manager.fusePool.repo.(*etcdstate.FUSEPoolRepository)
	rt := manager.runtime.(*fuseManagerRuntime)
	require.NoError(t, manager.Stop(ctx))

	rt.mockRuntime.mu.Lock()
	rt.mockRuntime.sandboxes[sb.RuntimeID].State = "exited"
	rt.mockRuntime.mu.Unlock()
	poolConfig := fusePoolConfig()
	poolConfig.MinSize = 0
	poolConfig.MaintainerToken = "api-b"
	var recoveryRuntime runtime.Runtime = rt
	if !confirmed {
		recoveryRuntime = unconfirmedFailedCreateRuntime{rt}
	}
	config := manager.config
	config.InstanceID = "api-b"
	config.FUSEPool = NewFUSEPool(recoveryRuntime, poolRepo, poolConfig, fixedFUSESpec("pool-key"))
	restarted := NewManager(recoveryRuntime, manager.filesystem, manager.fsMeta, config)
	restarted.SetSessionStore(NewSessionStore(store, time.Hour))
	restarted.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	t.Cleanup(func() { require.NoError(t, restarted.Stop(context.Background())) })

	err = restarted.Destroy(ctx, sb.ID)
	if !confirmed {
		require.ErrorIs(t, err, ErrSandboxNotReady)
		record, loadErr := active.Load(ctx, sb.ID)
		require.NoError(t, loadErr)
		require.NotNil(t, record)
		require.False(t, rt.wasRemoved(sb.RuntimeID))
		return
	}
	require.NoError(t, err)
	record, err := active.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
	require.True(t, rt.wasRemoved(sb.RuntimeID))
}

func TestManagerRecoversExpiredFailedFUSECreateAfterManagerRestartWithEtcd(t *testing.T) {
	manager, _ := newEtcdFUSEManager(t)
	ctx := context.Background()
	store := manager.sessions.store.(*etcdstate.Store)
	repository := manager.fusePool.repo.(*etcdstate.FUSEPoolRepository)
	rt := manager.runtime.(*fuseManagerRuntime)
	manager.config.WorkspaceCoordinator = NewWorkspaceCoordinator(store, 2*time.Second, 500*time.Millisecond)

	prepared, next, err := repository.ListStatePage(ctx, "pool-key", state.FUSEPoolPrepared, "", 10)
	require.NoError(t, err)
	require.Empty(t, next)
	require.Len(t, prepared, 1)
	record := prepared[0]
	request := validLeaseRequest()
	request.Runtime = "kubernetes"
	request.RuntimeID, request.RuntimeUID = record.RuntimeID, record.RuntimeUID
	request.StorageIdentity = manager.fsMeta.StorageIdentity
	lease, err := manager.config.WorkspaceCoordinator.Acquire(ctx, request)
	require.NoError(t, err)
	_, err = manager.config.WorkspaceCoordinator.ConsumeMountAttempt(ctx, lease, record.PoolKey)
	require.NoError(t, err)
	claimed, err := repository.ClaimCleanup(ctx, record.PreparationID, state.FUSEPoolPrepared,
		record.MaintainerToken, record.ReservationToken, record.Revision,
		record.RuntimeID, record.RuntimeUID, "failed-create-cleanup", time.Second)
	require.NoError(t, err)
	require.Equal(t, state.FUSEPoolCleanup, claimed.State)
	require.NoError(t, manager.Stop(ctx))

	poolConfig := fusePoolConfig()
	poolConfig.MinSize = 0
	poolConfig.MaintainerToken = "api-b"
	restartedConfig := manager.config
	restartedConfig.InstanceID = "api-b"
	restartedConfig.FUSEPool = NewFUSEPool(rt, repository, poolConfig, fixedFUSESpec("pool-key"))
	restarted := NewManager(rt, manager.filesystem, manager.fsMeta, restartedConfig)
	restarted.SetSessionStore(NewSessionStore(store, time.Hour))
	restarted.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	t.Cleanup(func() { require.NoError(t, restarted.Stop(context.Background())) })

	require.NoError(t, restarted.fusePool.Reconcile(ctx))
	require.False(t, rt.wasRemoved(record.RuntimeID), "live workspace lease must retain the runtime")
	ownerKey, err := workspaceStateKeysFromOwner(lease.OwnerSnapshot())
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		leaseRaw, readErr := store.Get(ctx, lease.Key)
		if readErr != nil || leaseRaw != nil {
			return false
		}
		if err := restarted.fusePool.Reconcile(ctx); err != nil {
			return false
		}
		remaining, err := repository.ReadByPreparationID(ctx, record.PreparationID)
		if err != nil || remaining != nil || !rt.wasRemoved(record.RuntimeID) {
			return false
		}
		ownerRaw, err := store.Get(ctx, ownerKey.owner)
		return err == nil && ownerRaw == nil
	}, 12*time.Second, 100*time.Millisecond,
		"a restarted manager must finish the exact cleanup only after the old lease expires")
}

func TestManagerRecoversOrphanedFUSEBindingAfterManagerRestartWithEtcd(t *testing.T) {
	testManagerRecoversOrphanedFUSEPublication(t, state.FUSEPoolBinding)
}

func TestManagerRecoversOrphanedFUSEConsumedAfterManagerRestartWithEtcd(t *testing.T) {
	testManagerRecoversOrphanedFUSEPublication(t, state.FUSEPoolConsumed)
}

func testManagerRecoversOrphanedFUSEPublication(t *testing.T, orphanState state.FUSEPoolState) {
	t.Helper()
	manager, _ := newEtcdFUSEManager(t)
	ctx := context.Background()
	store := manager.sessions.store.(*etcdstate.Store)
	repository := manager.fusePool.repo.(*etcdstate.FUSEPoolRepository)
	rt := manager.runtime.(*fuseManagerRuntime)
	manager.config.WorkspaceCoordinator = NewWorkspaceCoordinator(store, 2*time.Second, 500*time.Millisecond)

	record, err := manager.fusePool.Acquire(ctx, "pool-key")
	require.NoError(t, err)
	request := validLeaseRequest()
	request.Runtime = "kubernetes"
	request.RuntimeID = record.RuntimeID
	request.StorageIdentity = manager.fsMeta.StorageIdentity
	lease, err := manager.config.WorkspaceCoordinator.Acquire(ctx, request)
	require.NoError(t, err)
	require.NoError(t, manager.config.WorkspaceCoordinator.BindRuntime(ctx, lease, record.RuntimeUID))
	_, err = manager.config.WorkspaceCoordinator.ConsumeMountAttempt(ctx, lease, record.PoolKey)
	require.NoError(t, err)
	bound, err := repository.Transition(ctx, record.PreparationID, state.FUSEPoolReserved,
		state.FUSEPoolBinding, record.ReservationToken, record.Revision)
	require.NoError(t, err)
	if orphanState == state.FUSEPoolConsumed {
		bound, err = repository.Transition(ctx, bound.PreparationID, state.FUSEPoolBinding,
			state.FUSEPoolConsumed, bound.ReservationToken, bound.Revision)
		require.NoError(t, err)
	}
	require.NoError(t, manager.Stop(ctx))

	poolConfig := fusePoolConfig()
	poolConfig.MinSize = 0
	poolConfig.MaintainerToken = "api-b"
	restartedConfig := manager.config
	restartedConfig.InstanceID = "api-b"
	restartedConfig.FUSEPool = NewFUSEPool(rt, repository, poolConfig, fixedFUSESpec("pool-key"))
	restarted := NewManager(rt, manager.filesystem, manager.fsMeta, restartedConfig)
	restarted.SetSessionStore(NewSessionStore(store, time.Hour))
	restarted.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	t.Cleanup(func() { require.NoError(t, restarted.Stop(context.Background())) })

	require.NoError(t, restarted.fusePool.Reconcile(ctx))
	require.False(t, rt.wasRemoved(record.RuntimeID), "a live workspace lease must retain the runtime")
	ownerKey, err := workspaceStateKeysFromOwner(lease.OwnerSnapshot())
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		leaseRaw, readErr := store.Get(ctx, lease.Key)
		if readErr != nil || leaseRaw != nil {
			return false
		}
		if err := restarted.fusePool.Reconcile(ctx); err != nil {
			return false
		}
		remaining, err := repository.ReadByPreparationID(ctx, bound.PreparationID)
		if err != nil || remaining != nil || !rt.wasRemoved(record.RuntimeID) {
			return false
		}
		ownerRaw, err := store.Get(ctx, ownerKey.owner)
		return err == nil && ownerRaw == nil
	}, 6*time.Second, 100*time.Millisecond,
		"an expired orphan publication must be claimed and safely released")
}
