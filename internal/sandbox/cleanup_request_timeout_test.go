package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

// A healthy remote controller with failed cleanup must not make either request
// wait indefinitely, and timing out must preserve the runtime and ownership.
func TestWorkspaceCleanupRequestsHaveServerDeadline(t *testing.T) {
	for _, operation := range []string{"destroy", "create", "create_after_lock_wait"} {
		t.Run(operation, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				managers, store, rt := distributedSyncManagers(t)
				sb, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
				require.NoError(t, err)
				repo := managers[0].activeSandboxes.(*memoryActiveRepository)
				_, _, _, err = repo.BeginDestroy(context.Background(), sb.ID)
				require.NoError(t, err)
				keys, err := workspaceStateKeysFromOwner(sb.Workspace.Owner)
				require.NoError(t, err)
				owner, err := store.Get(context.Background(), keys.owner)
				require.NoError(t, err)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if operation == "create_after_lock_wait" {
					unlock, lockErr := managers[1].lockWorkspaceRequest(ctx, "team/a")
					require.NoError(t, lockErr)
					go func() {
						time.Sleep(15 * time.Second)
						unlock()
					}()
				}
				done := make(chan error, 1)
				go func() {
					if operation == "destroy" {
						done <- managers[1].Destroy(ctx, sb.ID)
						return
					}
					_, _, callErr := managers[1].GetOrCreate(ctx, sb.Config)
					done <- callErr
				}()
				select {
				case err = <-done:
					require.ErrorIs(t, err, context.DeadlineExceeded)
					if operation == "destroy" {
						require.ErrorIs(t, err, ErrSandboxCleanupPending)
					} else {
						require.True(t, errors.Is(err, ErrWorkspaceRecoveryRequired) || errors.Is(err, ErrWorkspaceLookupUnavailable), err)
					}
				case <-time.After(25 * time.Second):
					cancel()
					<-done
					t.Fatal("request exceeded server wait budget while remote cleanup was stuck")
				}
				after, err := store.Get(context.Background(), keys.owner)
				require.NoError(t, err)
				require.Equal(t, owner, after)
				rt.mu.Lock()
				require.Equal(t, 1, rt.created)
				require.Zero(t, rt.removed)
				rt.mu.Unlock()
			})
		})
	}
}

type slowCleanupRepository struct {
	*memoryActiveRepository
	release        chan struct{}
	readAfterError bool
}

func (r *slowCleanupRepository) BeginDestroy(ctx context.Context, id string) (*state.ActiveSandboxRecord, int64, bool, error) {
	if r.readAfterError {
		<-ctx.Done()
		return nil, 0, false, ctx.Err()
	}
	return r.memoryActiveRepository.BeginDestroy(ctx, id)
}

func (r *slowCleanupRepository) Load(ctx context.Context, id string) (*state.ActiveSandboxRecord, error) {
	if r.readAfterError {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.release:
		}
	}
	return r.memoryActiveRepository.Load(ctx, id)
}

func (r *slowCleanupRepository) ReleaseController(ctx context.Context, lease state.ActiveSandboxControllerLease) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.release:
		return r.memoryActiveRepository.ReleaseController(ctx, lease)
	}
}

func TestDestroyDeadlineIncludesCleanupReadAndControllerRelease(t *testing.T) {
	for _, readAfterError := range []bool{false, true} {
		t.Run(map[bool]string{false: "release", true: "readback"}[readAfterError], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				managers, _, _ := distributedSyncManagers(t)
				sb, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
				require.NoError(t, err)
				r := &slowCleanupRepository{memoryActiveRepository: managers[0].activeSandboxes.(*memoryActiveRepository), release: make(chan struct{}), readAfterError: readAfterError}
				managers[0].activeSandboxes = r
				controller := managers[0].syncLifecycles[sb.ID].controller
				controller.mu.Lock()
				controller.repository = r
				controller.mu.Unlock()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- managers[0].Destroy(ctx, sb.ID) }()
				select {
				case err = <-done:
					close(r.release)
					if readAfterError {
						require.ErrorIs(t, err, ErrSandboxCleanupPending)
					} else {
						require.NoError(t, err)
					}
				case <-time.After(25 * time.Second):
					cancel()
					close(r.release)
					<-done
					t.Fatal("cleanup readback/release escaped the request deadline")
				}
			})
		})
	}
}

type slowControllerRenewalRepository struct {
	*memoryActiveRepository
	release chan struct{}
	entered chan struct{}
}

func (r *slowControllerRenewalRepository) RenewController(ctx context.Context, lease state.ActiveSandboxControllerLease, ttl time.Duration) (*state.ActiveSandboxControllerLease, error) {
	select {
	case r.entered <- struct{}{}:
	default:
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.release:
		return r.memoryActiveRepository.RenewController(ctx, lease, ttl)
	}
}

func TestDestroyDeadlineIncludesControllerMutexWait(t *testing.T) {
	managers, _, rt := distributedSyncManagers(t)
	sb, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	controller := managers[0].syncLifecycles[sb.ID].controller
	slow := &slowControllerRenewalRepository{memoryActiveRepository: managers[0].activeSandboxes.(*memoryActiveRepository), release: make(chan struct{}), entered: make(chan struct{}, 1)}
	controller.mu.Lock()
	controller.repository = slow
	controller.mu.Unlock()
	// An independent renewal owns the mutex while waiting for its RPC.
	// Mutex blocking is not a synctest durable wait, so use a short real deadline.
	blockerDone := make(chan error, 1)
	go func() { blockerDone <- controller.Fence(context.Background()) }()
	<-slow.entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- managers[0].Destroy(ctx, sb.ID) }()
	select {
	case err = <-done:
		close(slow.release)
		<-blockerDone
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.ErrorIs(t, err, ErrSandboxCleanupPending)
	case <-time.After(time.Second):
		cancel()
		close(slow.release)
		<-blockerDone
		<-done
		t.Fatal("controller mutex contention escaped the destroy deadline")
	}
	rt.mu.Lock()
	require.Zero(t, rt.removed)
	rt.mu.Unlock()
}

type lostRestoreAckStore struct {
	*atomicMemoryStore
	leaseKey string
	failed   atomic.Bool
	release  chan struct{}
}

func (s *lostRestoreAckStore) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	created, err := s.atomicMemoryStore.SetNX(ctx, key, value, ttl)
	if key != s.leaseKey || err != nil || !created {
		return created, err
	}
	s.failed.Store(true)
	<-ctx.Done()
	return false, ctx.Err()
}

func (s *lostRestoreAckStore) Get(ctx context.Context, key string) ([]byte, error) {
	if key == s.leaseKey && s.failed.Load() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.release:
		}
	}
	return s.atomicMemoryStore.Get(ctx, key)
}

func TestRestoreLostAckReadbackHonorsRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		managers, store, _ := distributedSyncManagers(t)
		sb, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
		require.NoError(t, err)
		lifecycle := managers[0].syncLifecycles[sb.ID]
		lifecycle.renewal.Stop()
		keys, err := workspaceStateKeysFromOwner(sb.Workspace.Owner)
		require.NoError(t, err)
		store.expireKey(keys.lease)
		slow := &lostRestoreAckStore{atomicMemoryStore: store, leaseKey: keys.lease, release: make(chan struct{})}
		managers[1].config.WorkspaceCoordinator.store = slow
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, restoreErr := managers[1].config.WorkspaceCoordinator.Restore(ctx, sb.Workspace.Owner)
			done <- restoreErr
		}()
		select {
		case err = <-done:
			close(slow.release)
			require.ErrorIs(t, err, context.DeadlineExceeded)
		case <-time.After(3 * time.Second):
			close(slow.release)
			<-done
			t.Fatal("restore acknowledgement readback escaped the request deadline")
		}
		// The uncertain acknowledgement must retain the matching owner/lease so
		// a later bounded restore can finish, rather than allocate a replacement.
		_, err = managers[0].config.WorkspaceCoordinator.Restore(context.Background(), sb.Workspace.Owner)
		require.NoError(t, err)
	})
}

type slowRestoredRenewalStore struct {
	*atomicMemoryStore
	leaseKey string
	calls    atomic.Int32
	release  chan struct{}
}

func (s *slowRestoredRenewalStore) CompareAndSwap(ctx context.Context, key string, oldValue, newValue []byte, ttl time.Duration) (bool, error) {
	if key == s.leaseKey && s.calls.Add(1) == 2 {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-s.release:
		}
	}
	return s.atomicMemoryStore.CompareAndSwap(ctx, key, oldValue, newValue, ttl)
}

func TestWorkspaceReuseDeadlineIncludesInitialRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		managers, store, _ := distributedSyncManagers(t)
		sb, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
		require.NoError(t, err)
		lifecycle := managers[0].syncLifecycles[sb.ID]
		lifecycle.renewal.Stop()
		require.NoError(t, lifecycle.controller.Stop(context.Background()))
		managers[0].retireLocalSyncController(sb)
		keys, err := workspaceStateKeysFromOwner(sb.Workspace.Owner)
		require.NoError(t, err)
		store.expireKey(keys.lease)
		slow := &slowRestoredRenewalStore{atomicMemoryStore: store, leaseKey: keys.lease, release: make(chan struct{})}
		managers[1].config.WorkspaceCoordinator.store = slow
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, _, callErr := managers[1].GetOrCreate(ctx, sb.Config); done <- callErr }()
		select {
		case err = <-done:
			close(slow.release)
			require.ErrorIs(t, err, context.DeadlineExceeded)
		case <-time.After(25 * time.Second):
			cancel()
			close(slow.release)
			<-done
			t.Fatal("initial renewal escaped the workspace reuse deadline")
		}
	})
}

func TestRestoredWorkspaceRenewalOutlivesRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		managers, store, _ := distributedSyncManagers(t)
		sb, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
		require.NoError(t, err)
		lifecycle := managers[0].syncLifecycles[sb.ID]
		lifecycle.renewal.Stop()
		require.NoError(t, lifecycle.controller.Stop(context.Background()))
		managers[0].retireLocalSyncController(sb)
		keys, err := workspaceStateKeysFromOwner(sb.Workspace.Owner)
		require.NoError(t, err)
		store.expireKey(keys.lease)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		restored, reused, err := managers[1].GetOrCreate(ctx, sb.Config)
		require.NoError(t, err)
		require.True(t, reused)
		require.Equal(t, sb.ID, restored.ID)
		time.Sleep(70 * time.Second)
		lease, err := store.Get(context.Background(), keys.lease)
		require.NoError(t, err)
		require.NotNil(t, lease, "lease renewal must survive the request and the original lease TTL")
	})
}

type slowRequestLockStore struct {
	*atomicMemoryStore
	release chan struct{}
}

func (s *slowRequestLockStore) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	if strings.HasPrefix(key, "sandbox:workspace:request:") {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-s.release:
		}
	}
	return s.atomicMemoryStore.SetNX(ctx, key, value, ttl)
}

func TestWorkspaceRequestDeadlineIncludesLockRPC(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		managers, store, rt := distributedSyncManagers(t)
		slow := &slowRequestLockStore{atomicMemoryStore: store, release: make(chan struct{})}
		managers[1].config.WorkspaceCoordinator.store = slow
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, _, err := managers[1].GetOrCreate(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
			done <- err
		}()
		select {
		case err := <-done:
			close(slow.release)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.ErrorIs(t, err, ErrWorkspaceLookupUnavailable)
		case <-time.After(25 * time.Second):
			cancel()
			close(slow.release)
			<-done
			t.Fatal("distributed lock acquisition escaped the workspace deadline")
		}
		rt.mu.Lock()
		require.Zero(t, rt.created)
		rt.mu.Unlock()
	})
}
