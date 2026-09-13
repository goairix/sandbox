package sandbox

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/stretchr/testify/require"
)

func blockedDistributedSyncCleanup(t *testing.T, mode Mode, configure ...func([]*Manager)) ([]*Manager, *mockRuntime, *Sandbox, *syncSandboxLifecycle, func(), <-chan error) {
	t.Helper()
	managers, store, rt := distributedSyncManagers(t)
	for _, manager := range managers {
		manager.runtime = &sharedPoolRuntime{rt}
		manager.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	}
	for _, setup := range configure {
		setup(managers)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: mode, WorkspacePath: "team/a"})
	require.NoError(t, err)
	lifecycle := managers[0].syncLifecycles[sb.ID]
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
	go func() { done <- managers[0].Destroy(ctx, sb.ID) }()
	select {
	case id := <-entered:
		require.Equal(t, sb.RuntimeID, id)
	case <-ctx.Done():
		t.Fatal("finalizer did not reach runtime removal")
	}
	record, err := managers[0].activeSandboxes.Load(ctx, sb.ID)
	require.NoError(t, err)
	require.Equal(t, "sync_final_output_done", record.CleanupCheckpoint)
	return managers, rt, sb, lifecycle, unblock, done
}

func assertSyncCleanupComplete(t *testing.T, manager *Manager, rt *mockRuntime, sb *Sandbox) {
	t.Helper()
	record, err := manager.activeSandboxes.Load(context.Background(), sb.ID)
	require.NoError(t, err)
	require.Nil(t, record)
	keys, err := manager.config.WorkspaceCoordinator.store.Keys(context.Background(), workspaceOwnerKeyPrefix+"*")
	require.NoError(t, err)
	require.Empty(t, keys)
	keys, err = manager.config.WorkspaceCoordinator.store.Keys(context.Background(), workspaceLeaseKeyPrefix+"*")
	require.NoError(t, err)
	require.Empty(t, keys)
	if sb.Config.Mode == ModePersistent {
		_, err = manager.sessions.Load(context.Background(), sb.ID)
	} else {
		_, err = manager.ephemeral.Load(context.Background(), sb.ID)
	}
	require.ErrorIs(t, err, ErrSandboxNotFound)
	rt.mu.Lock()
	count := rt.removedIDs[sb.RuntimeID]
	_, exists := rt.sandboxes[sb.RuntimeID]
	rt.mu.Unlock()
	require.Equal(t, 1, count, "one finalizer must perform exact runtime deletion")
	require.False(t, exists)
}

func TestDistributedSyncCleanupScanPreservesFinalizer(t *testing.T) {
	for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
		t.Run(string(mode), func(t *testing.T) {
			managers, rt, sb, lifecycle, unblock, done := blockedDistributedSyncCleanup(t, mode)
			record, err := managers[0].activeSandboxes.Load(context.Background(), sb.ID)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err = managers[0].reconcileActiveLifecycle(ctx, record)
			unblock()
			first := <-done
			require.NoError(t, err, "local scan must schedule rather than steal cleanup")
			require.NoError(t, first, "original DELETE must retain its controller")
			require.NoError(t, managers[0].destroySyncSandbox(context.Background(), lifecycle), "scheduled duplicate completion must be idempotent")
			assertSyncCleanupComplete(t, managers[0], rt, sb)
		})
	}
}

func TestDistributedSyncCleanupPeerScanDoesNotWaitForLiveOwner(t *testing.T) {
	managers, rt, sb, lifecycle, unblock, done := blockedDistributedSyncCleanup(t, ModePersistent)
	record, err := managers[1].activeSandboxes.Load(context.Background(), sb.ID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	scanErr := managers[1].reconcileActiveLifecycle(ctx, record)
	fenceErr := lifecycle.controller.Fence(context.Background())
	unblock()
	first := <-done
	require.NoError(t, scanErr, "a live remote finalizer must not delay scans of other sandboxes")
	require.NoError(t, fenceErr)
	require.NoError(t, first)
	assertSyncCleanupComplete(t, managers[0], rt, sb)
}

func TestDistributedSyncCleanupConcurrentDeleteKeepsToken(t *testing.T) {
	for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
		for _, replica := range []int{0, 1} {
			t.Run(string(mode)+"/"+[]string{"owner", "peer"}[replica], func(t *testing.T) {
				managers, rt, sb, lifecycle, unblock, done := blockedDistributedSyncCleanup(t, mode)
				ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
				defer cancel()
				second := make(chan error, 1)
				go func() { second <- managers[replica].Destroy(ctx, sb.ID) }()
				var secondErr error
				select {
				case secondErr = <-second:
				case <-time.After(time.Second):
					unblock()
					t.Fatal("duplicate DELETE ignored its deadline")
				}
				fenceErr := lifecycle.controller.Fence(context.Background())
				unblock()
				first := <-done
				require.ErrorIs(t, secondErr, context.DeadlineExceeded, "duplicate must wait for the owner, not immediately fail")
				require.NoError(t, fenceErr, "duplicate DELETE must not relinquish the live token")
				require.NoError(t, first)
				assertSyncCleanupComplete(t, managers[0], rt, sb)
			})
		}
	}
}

func TestSyncFinalizerDuplicateCompletionDoesNotFenceDeletedRecord(t *testing.T) {
	managers, rt, sb, lifecycle, unblock, done := blockedDistributedSyncCleanup(t, ModePersistent)
	unblock()
	require.NoError(t, <-done)
	require.ErrorIs(t, lifecycle.controller.Fence(context.Background()), state.ErrActiveSandboxStaleToken)
	require.NoError(t, managers[0].destroySyncSandbox(context.Background(), lifecycle))
	assertSyncCleanupComplete(t, managers[0], rt, sb)
}

type observedCleanupRepository struct {
	state.ActiveSandboxRepository
	owned chan struct{}
}

func (r observedCleanupRepository) AcquireController(ctx context.Context, lease state.ActiveSandboxControllerLease, ttl time.Duration) (*state.ActiveSandboxControllerLease, bool, error) {
	controller, acquired, err := r.ActiveSandboxRepository.AcquireController(ctx, lease, ttl)
	if err == nil && !acquired {
		select {
		case r.owned <- struct{}{}:
		default:
		}
	}
	return controller, acquired, err
}

func (r observedCleanupRepository) ConfirmRecordAbsence(ctx context.Context, id string) error {
	if repo, ok := r.ActiveSandboxRepository.(interface {
		ConfirmRecordAbsence(context.Context, string) error
	}); ok {
		return repo.ConfirmRecordAbsence(ctx, id)
	}
	return nil
}

func TestDistributedSyncCleanupPeerWaitsForSuccessfulDelete(t *testing.T) {
	for _, realRedis := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "redis"}[realRedis], func(t *testing.T) {
			var repository state.ActiveSandboxRepository
			if realRedis {
				addr := os.Getenv("TEST_REDIS_ADDR")
				if addr == "" {
					t.Skip("requires TEST_REDIS_ADDR")
				}
				store, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr, DB: 14})
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, store.Close()) })
				repository, err = redisstate.NewActiveSandboxRepository(store, "sync-cleanup-"+randSuffix(24))
				require.NoError(t, err)
			}
			for _, mode := range []Mode{ModeEphemeral, ModePersistent} {
				t.Run(string(mode), func(t *testing.T) {
					managers, rt, sb, lifecycle, unblock, done := blockedDistributedSyncCleanup(t, mode, func(managers []*Manager) {
						if repository != nil {
							for _, manager := range managers {
								manager.activeSandboxes = repository
								manager.config.ActiveSandboxes = repository
							}
						}
					})
					owned := make(chan struct{}, 1)
					managers[1].activeSandboxes = observedCleanupRepository{managers[1].activeSandboxes, owned}
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					peerDone := make(chan error, 1)
					go func() { peerDone <- managers[1].Destroy(ctx, sb.ID) }()
					select {
					case <-owned:
					case <-ctx.Done():
						t.Fatal("peer did not reach controller ownership check")
					}
					require.NoError(t, lifecycle.controller.Fence(ctx))
					unblock()
					require.NoError(t, <-done)
					require.NoError(t, <-peerDone)
					assertSyncCleanupComplete(t, managers[0], rt, sb)
				})
			}
		})
	}
}

func TestSyncFinalizerStaleCapabilityRemainsAnError(t *testing.T) {
	managers, rt, sb, lifecycle, unblock, done := blockedDistributedSyncCleanup(t, ModePersistent)
	lifecycle.controller.mu.Lock()
	lifecycle.controller.lost = state.ErrActiveSandboxStaleToken
	lifecycle.controller.mu.Unlock()
	unblock()
	require.ErrorIs(t, <-done, state.ErrActiveSandboxStaleToken)
	require.ErrorIs(t, managers[0].destroySyncSandbox(context.Background(), lifecycle), state.ErrActiveSandboxStaleToken)
	record, err := managers[0].activeSandboxes.Load(context.Background(), sb.ID)
	require.NoError(t, err)
	require.NotNil(t, record, "lost fencing must retain recovery proof")
	require.False(t, lifecycle.finalizeDone)
	managers[0].retireLocalSyncController(sb)
	require.NoError(t, lifecycle.controller.Stop(context.Background()))
	managers[1].runtime = missingAwareRuntime{rt}
	require.NoError(t, managers[1].Destroy(context.Background(), sb.ID), "a peer must recover the durable final-output checkpoint after release")
	assertSyncCleanupComplete(t, managers[0], rt, sb)
}

func TestDistributedSyncCleanupRealUnmountCheckpointDoesNotReplayOutput(t *testing.T) {
	managers, _, rt := distributedSyncManagers(t)
	ctx := context.Background()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	snapshot, opCtx, release, err := managers[0].beginDistributedWorkspaceOperation(ctx, sb.ID)
	require.NoError(t, err)
	snapshot.WorkspaceTransition = workspaceUnmountSynced
	require.NoError(t, managers[0].persistActiveSandboxUpdate(opCtx, snapshot))
	release()
	require.Equal(t, "", managers[0].sandboxes[sb.ID].WorkspaceTransition, "distributed transitions do not mutate the old cache")
	require.NoError(t, rt.RemoveSandbox(ctx, sb.RuntimeID))
	rt.mu.Lock()
	rt.downloadDirErr = errors.New("completed output must not be replayed")
	rt.execFunc = func(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
		return nil, runtime.ErrNotFound
	}
	rt.mu.Unlock()
	managers[0].runtime = missingAwareRuntime{rt}
	require.NoError(t, managers[0].Destroy(ctx, sb.ID))
	assertSyncCleanupComplete(t, managers[0], rt, sb)
}

type failedCleanupAbsenceRepository struct{ state.ActiveSandboxRepository }

func (r failedCleanupAbsenceRepository) ConfirmRecordAbsence(context.Context, string) error {
	return state.ErrDurabilityUnconfirmed
}

func TestDistributedSyncCleanupWaitRequiresDurableAbsence(t *testing.T) {
	managers, _, _ := distributedSyncManagers(t)
	managers[1].activeSandboxes = failedCleanupAbsenceRepository{managers[1].activeSandboxes}
	require.ErrorIs(t, managers[1].waitForActiveCleanup(context.Background(), "completed-sandbox"), state.ErrDurabilityUnconfirmed)
}

type delayedCleanupRepository struct {
	observedCleanupRepository
	entered chan struct{}
	release chan struct{}
	failACK bool
}

func (r delayedCleanupRepository) BeginDestroy(ctx context.Context, id string) (*state.ActiveSandboxRecord, int64, bool, error) {
	record, revision, generation, err := r.ActiveSandboxRepository.BeginDestroy(ctx, id)
	select {
	case r.entered <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, false, ctx.Err()
	}
	select {
	case <-r.release:
	case <-ctx.Done():
		return nil, 0, false, ctx.Err()
	}
	return record, revision, generation, err
}

func (r delayedCleanupRepository) ConfirmRecordAbsence(ctx context.Context, id string) error {
	if r.failACK {
		return state.ErrDurabilityUnconfirmed
	}
	return r.observedCleanupRepository.ConfirmRecordAbsence(ctx, id)
}

func TestDistributedSyncCleanupRecordGoneBeforeAcquire(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("requires TEST_REDIS_ADDR")
	}
	store, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr, DB: 14})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	for _, failACK := range []bool{false, true} {
		t.Run(map[bool]string{false: "durable", true: "unconfirmed"}[failACK], func(t *testing.T) {
			repo, err := redisstate.NewActiveSandboxRepository(store, "sync-gone-"+randSuffix(24))
			require.NoError(t, err)
			managers, rt, sb, _, unblock, done := blockedDistributedSyncCleanup(t, ModePersistent, func(managers []*Manager) {
				for _, manager := range managers {
					manager.activeSandboxes = repo
					manager.config.ActiveSandboxes = repo
				}
			})
			entered, release := make(chan struct{}, 1), make(chan struct{})
			managers[1].activeSandboxes = delayedCleanupRepository{observedCleanupRepository{repo, nil}, entered, release, failACK}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			peerDone := make(chan error, 1)
			go func() { peerDone <- managers[1].Destroy(ctx, sb.ID) }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("peer did not obtain its cleanup snapshot")
			}
			unblock()
			require.NoError(t, <-done)
			close(release)
			peerErr := <-peerDone
			if failACK {
				require.ErrorIs(t, peerErr, state.ErrDurabilityUnconfirmed)
			} else {
				require.NoError(t, peerErr, "confirmed completion between snapshot and acquisition is idempotent")
			}
			assertSyncCleanupComplete(t, managers[0], rt, sb)
		})
	}
}

type staleCleanupAcquireRepository struct{ state.ActiveSandboxRepository }

func (r staleCleanupAcquireRepository) AcquireController(context.Context, state.ActiveSandboxControllerLease, time.Duration) (*state.ActiveSandboxControllerLease, bool, error) {
	return nil, false, state.ErrActiveSandboxStaleToken
}

func TestDistributedSyncCleanupStaleAcquisitionWithRecordStillFails(t *testing.T) {
	managers, rt, sb, lifecycle, unblock, done := blockedDistributedSyncCleanup(t, ModePersistent)
	managers[1].activeSandboxes = staleCleanupAcquireRepository{managers[1].activeSandboxes}
	require.ErrorIs(t, managers[1].Destroy(context.Background(), sb.ID), state.ErrActiveSandboxStaleToken)
	require.NoError(t, lifecycle.controller.Fence(context.Background()))
	unblock()
	require.NoError(t, <-done)
	assertSyncCleanupComplete(t, managers[0], rt, sb)
}
