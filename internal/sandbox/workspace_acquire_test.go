package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/goairix/fs/driver/local"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
	redisstate "github.com/goairix/sandbox/internal/storage/state/redis"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceRequestReturnsExistingAcrossReplicas(t *testing.T) {
	managers, _, rt := distributedSyncManagers(t)
	cfg := SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a", Timeout: 120}
	first, err := managers[0].GetOrCreate(context.Background(), cfg)
	require.NoError(t, err)
	before := first.Timeout
	cfg.Timeout = 300
	second, err := managers[1].GetOrCreate(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	require.Equal(t, before, second.Timeout)
	require.True(t, first.CreatedAt.Equal(second.CreatedAt))
	_, err = managers[1].Exec(context.Background(), second.ID, runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	rt.mu.Lock()
	require.Equal(t, 1, rt.created)
	require.Zero(t, rt.removed)
	rt.mu.Unlock()
}

func TestWorkspaceRequestConcurrentAcrossReplicas(t *testing.T) {
	managers, _, rt := distributedSyncManagers(t)
	const requests = 18
	var wg sync.WaitGroup
	ids := make([]string, requests)
	errs := make([]error, requests)
	start := make(chan struct{})
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sb, err := managers[i%len(managers)].GetOrCreate(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
			errs[i] = err
			if sb != nil {
				ids[i] = sb.ID
			}
		}()
	}
	close(start)
	wg.Wait()
	for i := range requests {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], ids[i])
	}
	require.NotEmpty(t, ids[0])
	rt.mu.Lock()
	require.Equal(t, 1, rt.created)
	require.Zero(t, rt.removed)
	rt.mu.Unlock()
}

func TestWorkspaceRequestFUSEIsReusedAndExecutable(t *testing.T) {
	rt := newFUSEManagerRuntime()
	m, _, _, _ := newFUSETestManager(t, rt)
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	cfg := SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"}
	first, err := m.GetOrCreate(context.Background(), cfg)
	require.NoError(t, err)
	second, err := m.GetOrCreate(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	_, err = m.Exec(context.Background(), second.ID, runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	third, err := m.GetOrCreate(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, first.ID, third.ID)
	rt.mockRuntime.mu.Lock()
	require.Zero(t, rt.removed)
	rt.mockRuntime.mu.Unlock()
}

func TestWorkspaceRequestRestoresExpiredLeaseAfterControllerLoss(t *testing.T) {
	managers, store, rt := distributedSyncManagers(t)
	first, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	managers[0].mu.RLock()
	lifecycle := managers[0].syncLifecycles[first.ID]
	managers[0].mu.RUnlock()
	lifecycle.renewal.Stop()
	require.NoError(t, lifecycle.controller.Stop(context.Background()))
	managers[0].mu.Lock()
	delete(managers[0].syncLifecycles, first.ID)
	delete(managers[0].sandboxes, first.ID)
	delete(managers[0].operationGates, first.ID)
	delete(managers[0].workspaces, first.ID)
	managers[0].mu.Unlock()
	keys, err := workspaceStateKeysFromOwner(first.Workspace.Owner)
	require.NoError(t, err)
	store.expireKey(keys.lease)
	second, err := managers[1].GetOrCreate(context.Background(), first.Config)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID)
	raw, err := store.Get(context.Background(), keys.lease)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	_, err = managers[1].Exec(context.Background(), second.ID, runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	rt.mu.Lock()
	require.Equal(t, 1, rt.created)
	require.Zero(t, rt.removed)
	rt.mu.Unlock()
}

func TestWorkspaceRequestWaitsForPublishingAndExclusiveOperations(t *testing.T) {
	for _, phase := range []state.ActiveSandboxPhase{state.ActiveSandboxPublishing, state.ActiveSandboxExclusive} {
		t.Run(string(phase), func(t *testing.T) {
			managers, _, rt := distributedSyncManagers(t)
			first, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
			require.NoError(t, err)
			repo := managers[0].activeSandboxes.(*memoryActiveRepository)
			repo.mu.Lock()
			record := repo.records[first.ID]
			record.Phase = phase
			repo.records[first.ID] = record
			repo.mu.Unlock()
			done := make(chan struct{})
			var second *Sandbox
			var requestErr error
			go func() { second, requestErr = managers[1].GetOrCreate(context.Background(), first.Config); close(done) }()
			select {
			case <-done:
				t.Fatal("request returned before publication completed")
			case <-time.After(100 * time.Millisecond):
			}
			repo.mu.Lock()
			record = repo.records[first.ID]
			record.Phase = state.ActiveSandboxActive
			repo.records[first.ID] = record
			repo.mu.Unlock()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("request failed to converge")
			}
			require.NoError(t, requestErr)
			require.Equal(t, first.ID, second.ID)
			rt.mu.Lock()
			require.Equal(t, 1, rt.created)
			require.Zero(t, rt.removed)
			rt.mu.Unlock()
		})
	}
}

func TestWorkspaceRequestCancellationDoesNotCreate(t *testing.T) {
	managers, _, rt := distributedSyncManagers(t)
	unlock, err := managers[0].lockWorkspaceRequest(context.Background(), "team/a")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = managers[0].GetOrCreate(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	unlock()
	require.Empty(t, managers[0].workspaceRequests)
	rt.mu.Lock()
	require.Zero(t, rt.created)
	rt.mu.Unlock()
}

func TestWorkspaceRequestDependencyFailureDoesNotCreate(t *testing.T) {
	managers, store, rt := distributedSyncManagers(t)
	store.failMethods["Get"] = errors.New("redis unavailable")
	_, err := managers[0].GetOrCreate(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorIs(t, err, ErrWorkspaceLookupUnavailable)
	store.failMethods["Get"] = nil
	rt.mu.Lock()
	require.Zero(t, rt.created)
	rt.mu.Unlock()
}

func TestWorkspaceRequestLocalBindIsReusedConcurrently(t *testing.T) {
	rt := newMockRuntime()
	root := t.TempDir()
	filesystem, err := local.New(local.Config{RootPath: root})
	require.NoError(t, err)
	m := NewManager(rt, filesystem, &storage.FileSystemMeta{Provider: storage.ProviderLocal, LocalPath: root}, ManagerConfig{RuntimeType: "docker"})
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	const count = 8
	ids := make([]string, count)
	errs := make([]error, count)
	var wg sync.WaitGroup
	// The mock records the same writable bind mount that Docker inspection
	// returns. Serialize its metadata update with the mock runtime lock.
	m.runtime = lookupInspectRuntime{Runtime: rt, inspect: func(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
		info, err := rt.GetSandbox(ctx, id)
		if err != nil {
			return nil, err
		}
		copy := *info
		copy.WorkspaceHostPath = m.resolveLocalWorkspacePath("team/a")
		return &copy, nil
	}}
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sb, err := m.GetOrCreate(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
			errs[i] = err
			if sb != nil {
				ids[i] = sb.ID
			}
		}()
	}
	wg.Wait()
	for i := range count {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], ids[i])
	}
	rt.mu.Lock()
	require.Equal(t, 1, rt.created)
	require.Zero(t, rt.removed)
	rt.mu.Unlock()
}

func TestWorkspaceRequestOldResidualOwnerRequiresServerRecovery(t *testing.T) {
	managers, store, rt := distributedSyncManagers(t)
	req := WorkspaceLeaseRequest{MountType: WorkspaceMountSync, Provider: "minio", StorageIdentity: "storage-primary", Bucket: "sandbox", Prefix: "workspaces/team/a/", SandboxID: "sandbox-old", Runtime: "kubernetes", RuntimeID: "gone-pod", RuntimeUID: "gone-uid"}
	lease, err := managers[0].config.WorkspaceCoordinator.Acquire(context.Background(), req)
	require.NoError(t, err)
	owner := lease.OwnerSnapshot()
	owner.UpdatedAt = time.Now().UTC().Add(-time.Hour)
	raw, err := json.Marshal(owner)
	require.NoError(t, err)
	require.NoError(t, store.Set(context.Background(), lease.ownerKey, raw, 0))
	store.expireKey(lease.Key)
	_, err = managers[1].GetOrCreate(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.ErrorIs(t, err, ErrWorkspaceRecoveryRequired)
	require.Contains(t, err.Error(), "sandbox_record_missing")
	after, err := store.Get(context.Background(), lease.ownerKey)
	require.NoError(t, err)
	require.Equal(t, raw, after)
	rt.mu.Lock()
	require.Zero(t, rt.created)
	require.Zero(t, rt.removed)
	rt.mu.Unlock()
}

func TestWorkspaceRequestRealRedisConcurrency(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("requires TEST_REDIS_ADDR")
	}
	store, err := redisstate.New(context.Background(), redisstate.Options{Addr: addr})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	repo, err := redisstate.NewActiveSandboxRepository(store, "request-test-"+uuid.NewString())
	require.NoError(t, err)
	managers, _, rt := distributedSyncManagers(t)
	for _, m := range managers {
		m.activeSandboxes, m.config.ActiveSandboxes = repo, repo
		m.config.WorkspaceCoordinator = NewWorkspaceCoordinator(store, time.Minute, 10*time.Second)
		m.SetSessionStore(NewSessionStore(store, time.Hour))
	}
	root := "request-" + uuid.NewString()
	require.NoError(t, managers[0].filesystem.MakeDir(context.Background(), root, 0o755))
	var wg sync.WaitGroup
	const count = 12
	ids := make([]string, count)
	errs := make([]error, count)
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			sb, err := managers[i%len(managers)].GetOrCreate(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: root})
			errs[i] = err
			if sb != nil {
				ids[i] = sb.ID
			}
		}()
	}
	wg.Wait()
	for i := range count {
		require.NoError(t, errs[i])
		require.Equal(t, ids[0], ids[i])
	}
	require.NotEmpty(t, ids[0])
	_, err = managers[1].Exec(context.Background(), ids[0], runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	rt.mu.Lock()
	require.Equal(t, 1, rt.created)
	require.Zero(t, rt.removed)
	rt.mu.Unlock()
	t.Cleanup(func() { require.NoError(t, managers[0].Destroy(context.Background(), ids[0])) })
}

func TestWorkspaceRequestCleanupRejectsCorruptOwnership(t *testing.T) {
	for _, phase := range []state.ActiveSandboxPhase{state.ActiveSandboxActive, state.ActiveSandboxDestroying} {
		for _, change := range []string{"owner", "generation", "runtime_uid"} {
			t.Run(string(phase)+"/"+change, func(t *testing.T) {
				managers, _, rt := distributedSyncManagers(t)
				first, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
				require.NoError(t, err)
				repo := managers[0].activeSandboxes.(*memoryActiveRepository)
				repo.mu.Lock()
				original := repo.records[first.ID]
				repo.mu.Unlock()
				t.Cleanup(func() { repo.mu.Lock(); repo.records[first.ID] = original; repo.mu.Unlock() })
				bad := cloneSandbox(first)
				bad.Timeout = time.Nanosecond
				switch change {
				case "owner":
					bad.Workspace.Owner = WorkspaceOwner{}
				case "generation":
					bad.Workspace.LeaseGeneration++
				case "runtime_uid":
					bad.RuntimeUID = "replacement"
					bad.Workspace.Owner.RuntimeUID = "replacement"
				}
				raw, err := json.Marshal(bad)
				require.NoError(t, err)
				record := original
				record.Phase = phase
				record.Snapshot = raw
				if change == "runtime_uid" {
					record.RuntimeUID = "replacement"
				}
				repo.mu.Lock()
				repo.records[first.ID] = record
				repo.mu.Unlock()
				_, err = managers[1].GetOrCreate(context.Background(), first.Config)
				require.ErrorIs(t, err, ErrWorkspaceRecoveryRequired)
				rt.mu.Lock()
				require.Equal(t, 1, rt.created)
				require.Zero(t, rt.removed)
				rt.mu.Unlock()
			})
		}
	}
}

func TestWorkspaceRequestWaitsForLocalExclusiveGate(t *testing.T) {
	m, _, rt := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	first, err := m.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	m.mu.RLock()
	gate := m.operationGates[first.ID]
	m.mu.RUnlock()
	exclusive, err := gate.BeginExclusive(context.Background())
	require.NoError(t, err)
	done := make(chan struct{})
	var second *Sandbox
	var requestErr error
	go func() { second, requestErr = m.GetOrCreate(context.Background(), first.Config); close(done) }()
	select {
	case <-done:
		t.Fatal("returned while workspace gate was exclusive")
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, exclusive.Reopen())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not converge after gate reopened")
	}
	require.NoError(t, requestErr)
	require.Equal(t, first.ID, second.ID)
	rt.mu.Lock()
	require.Equal(t, 1, rt.created)
	require.Zero(t, rt.removed)
	rt.mu.Unlock()
}

func TestWorkspaceRequestExpiredLifecycleIsCleanedBeforeNewCreation(t *testing.T) {
	managers, _, rt := distributedSyncManagers(t)
	first, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	// Simulate expiry after the creator replica has exited; the new replica
	// elects the recovery controller and completes normal finalization.
	managers[0].mu.RLock()
	lifecycle := managers[0].syncLifecycles[first.ID]
	managers[0].mu.RUnlock()
	lifecycle.renewal.Stop()
	require.NoError(t, lifecycle.controller.Stop(context.Background()))
	managers[0].mu.Lock()
	delete(managers[0].syncLifecycles, first.ID)
	delete(managers[0].sandboxes, first.ID)
	delete(managers[0].operationGates, first.ID)
	delete(managers[0].workspaces, first.ID)
	managers[0].mu.Unlock()

	expired := cloneSandbox(first)
	expired.Timeout = time.Nanosecond
	raw, err := json.Marshal(expired)
	require.NoError(t, err)
	repo := managers[0].activeSandboxes.(*memoryActiveRepository)
	repo.mu.Lock()
	record := repo.records[first.ID]
	record.Snapshot = raw
	repo.records[first.ID] = record
	repo.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	second, err := managers[1].GetOrCreate(ctx, first.Config)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
	_, err = managers[1].Exec(context.Background(), second.ID, runtime.ExecRequest{Command: "true"})
	require.NoError(t, err)
	rt.mu.Lock()
	require.Equal(t, 2, rt.created)
	require.Equal(t, 1, rt.removedIDs[first.RuntimeID])
	rt.mu.Unlock()
}

func TestWorkspaceRequestWaitsForProvisionalLease(t *testing.T) {
	managers, store, _ := distributedSyncManagers(t)
	first, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	keys, err := workspaceStateKeysFromOwner(first.Workspace.Owner)
	require.NoError(t, err)
	original, err := store.Get(context.Background(), keys.lease)
	require.NoError(t, err)
	var provisional workspaceLeaseRecord
	require.NoError(t, json.Unmarshal(original, &provisional))
	provisional.Phase = workspaceLeasePhaseProvisional
	provisional.Generation = 0
	raw, err := json.Marshal(provisional)
	require.NoError(t, err)
	require.NoError(t, store.Set(context.Background(), keys.lease, raw, time.Minute))
	done := make(chan struct{})
	var second *Sandbox
	var requestErr error
	go func() { second, requestErr = managers[1].GetOrCreate(context.Background(), first.Config); close(done) }()
	select {
	case <-done:
		t.Fatal("returned while the lease was provisional")
	case <-time.After(100 * time.Millisecond):
	}
	require.NoError(t, store.Set(context.Background(), keys.lease, original, time.Minute))
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not converge after lease publication")
	}
	require.NoError(t, requestErr)
	require.Equal(t, first.ID, second.ID)
}

func TestWorkspaceCleanupDoesNotDeleteAfterProofChanges(t *testing.T) {
	for _, scenario := range []string{"snapshot", "owner"} {
		t.Run(scenario, func(t *testing.T) {
			managers, store, rt := distributedSyncManagers(t)
			first, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
			require.NoError(t, err)
			repo := managers[0].activeSandboxes.(*memoryActiveRepository)
			repo.mu.Lock()
			original := repo.records[first.ID]
			repo.mu.Unlock()
			expired := cloneSandbox(first)
			expired.Timeout = time.Nanosecond
			raw, err := json.Marshal(expired)
			require.NoError(t, err)
			record := original
			record.Snapshot = raw
			repo.mu.Lock()
			repo.records[first.ID] = record
			repo.mu.Unlock()
			keys, err := workspaceStateKeysFromOwner(first.Workspace.Owner)
			require.NoError(t, err)
			ownerBefore, err := store.Get(context.Background(), keys.owner)
			require.NoError(t, err)
			t.Cleanup(func() {
				repo.mu.Lock()
				repo.records[first.ID] = original
				repo.mu.Unlock()
				require.NoError(t, store.Set(context.Background(), keys.owner, ownerBefore, 0))
			})
			var once sync.Once
			managers[1].runtime = lookupInspectRuntime{Runtime: rt, inspect: func(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
				once.Do(func() {
					if scenario == "snapshot" {
						changed := cloneSandbox(first)
						changed.Workspace.RootPath = "team/b"
						changedRaw, err := json.Marshal(changed)
						require.NoError(t, err)
						repo.mu.Lock()
						next := repo.records[first.ID]
						next.Snapshot = changedRaw
						next.Revision++
						repo.records[first.ID] = next
						repo.mu.Unlock()
					} else {
						changed := first.Workspace.Owner
						changed.Generation++
						changedRaw, err := json.Marshal(changed)
						require.NoError(t, err)
						require.NoError(t, store.Set(ctx, keys.owner, changedRaw, 0))
					}
				})
				return rt.GetSandbox(ctx, id)
			}}
			err = managers[1].cleanupRequestedWorkspace(context.Background(), "team/a", first.ID)
			require.Error(t, err)
			rt.mu.Lock()
			require.Zero(t, rt.removed)
			rt.mu.Unlock()
		})
	}
}
