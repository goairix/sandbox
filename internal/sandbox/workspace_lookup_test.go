package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceLookupAcrossReplicasAfterMappingLoss(t *testing.T) {
	managers, store, rt := distributedSyncManagers(t)
	ctx := context.Background()
	sb, err := managers[0].Create(ctx, SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	peer := managers[1]
	require.Empty(t, peer.sandboxes)
	keys, err := workspaceStateKeysFromOwner(sb.Workspace.Owner)
	require.NoError(t, err)
	before, err := store.Get(ctx, keys.owner)
	require.NoError(t, err)
	got, err := peer.GetByWorkspace(ctx, "team/a")
	require.NoError(t, err)
	require.Equal(t, sb.ID, got.ID)
	require.Equal(t, "team/a", got.Workspace.RootPath)
	require.Equal(t, sb.Workspace.Owner, got.Workspace.Owner)
	after, err := store.Get(ctx, keys.owner)
	require.NoError(t, err)
	require.Equal(t, before, after)
	rt.mu.Lock()
	require.Zero(t, rt.removed)
	rt.mu.Unlock()
	// A create race is a reason to look up again, never to destroy the owner.
	_, err = managers[2].Create(ctx, sb.Config)
	require.ErrorIs(t, err, ErrWorkspaceLeased)
	got, err = peer.GetByWorkspace(ctx, "team/a")
	require.NoError(t, err)
	require.Equal(t, sb.ID, got.ID)
	rt.mu.Lock()
	require.Zero(t, rt.removedIDs[sb.RuntimeID])
	rt.mu.Unlock()
}

func TestWorkspaceLookupRejectsUnsafeOwnerSnapshots(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Sandbox)
	}{
		{"wrong_root", func(sb *Sandbox) { sb.Workspace.RootPath = "team/b" }},
		{"wrong_generation", func(sb *Sandbox) { sb.Workspace.LeaseGeneration++ }},
		{"wrong_owner", func(sb *Sandbox) { sb.Workspace.Owner.SandboxID = "other" }},
		{"wrong_runtime_uid", func(sb *Sandbox) { sb.RuntimeUID = "replacement" }},
		{"expired", func(sb *Sandbox) { sb.Timeout = time.Nanosecond }},
		{"destroying", func(sb *Sandbox) { sb.State = StateDestroying }},
		{"transition", func(sb *Sandbox) { sb.WorkspaceTransition = workspaceUnmountReleasing }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			managers, store, rt := distributedSyncManagers(t)
			sb, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
			require.NoError(t, err)
			repo := managers[0].activeSandboxes.(*memoryActiveRepository)
			bad := cloneSandbox(sb)
			tc.change(&bad)
			raw, err := json.Marshal(bad)
			require.NoError(t, err)
			repo.mu.Lock()
			record := repo.records[sb.ID]
			record.Snapshot = raw
			repo.records[sb.ID] = record
			repo.mu.Unlock()
			_, err = managers[1].GetByWorkspace(context.Background(), "team/a")
			require.ErrorIs(t, err, ErrWorkspaceLookupConflict)
			keys, err := workspaceStateKeysFromOwner(sb.Workspace.Owner)
			require.NoError(t, err)
			owner, err := store.Get(context.Background(), keys.owner)
			require.NoError(t, err)
			require.NotEmpty(t, owner)
			rt.mu.Lock()
			require.Zero(t, rt.removed)
			rt.mu.Unlock()
		})
	}
}

func TestWorkspaceLookupLifecycleAndBackendConflicts(t *testing.T) {
	for _, phase := range []state.ActiveSandboxPhase{state.ActiveSandboxPublishing, state.ActiveSandboxExclusive, state.ActiveSandboxDestroying, state.ActiveSandboxCleanupPending} {
		t.Run(string(phase), func(t *testing.T) {
			managers, _, _ := distributedSyncManagers(t)
			sb, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
			require.NoError(t, err)
			repo := managers[0].activeSandboxes.(*memoryActiveRepository)
			repo.mu.Lock()
			r := repo.records[sb.ID]
			r.Phase = phase
			repo.records[sb.ID] = r
			repo.mu.Unlock()
			_, err = managers[1].GetByWorkspace(context.Background(), "team/a")
			require.ErrorIs(t, err, ErrWorkspaceLookupConflict)
		})
	}
	for _, scope := range []string{"identity", "bucket", "provider", "subpath"} {
		t.Run(scope, func(t *testing.T) {
			managers, _, _ := distributedSyncManagers(t)
			_, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
			require.NoError(t, err)
			meta := *managers[1].fsMeta
			switch scope {
			case "identity":
				meta.StorageIdentity = "another-storage"
			case "bucket":
				meta.Bucket = "another-bucket"
			case "provider":
				meta.Provider = storage.ProviderS3
			case "subpath":
				meta.SubPath = "other"
			}
			managers[1].fsMeta = &meta
			got, err := managers[1].GetByWorkspace(context.Background(), "team/a")
			require.Error(t, err)
			require.Empty(t, got.ID)
		})
	}
}

func TestWorkspaceLookupUnrelatedCoordinatedSessionDoesNotBlockAbsence(t *testing.T) {
	m, store, rt := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	_, err := m.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	peer := NewManager(rt, m.filesystem, m.fsMeta, m.config)
	peer.SetSessionStore(NewSessionStore(store, time.Hour))
	t.Cleanup(func() { require.NoError(t, peer.Stop(context.Background())) })
	_, err = peer.GetByWorkspace(context.Background(), "team/b")
	require.ErrorIs(t, err, ErrSandboxNotFound)
	_, err = peer.GetByWorkspace(context.Background(), "team/a")
	require.ErrorIs(t, err, ErrWorkspaceLookupConflict)
}

func TestWorkspaceLookupUnrelatedClosedGateDoesNotBlockAbsence(t *testing.T) {
	m, _, _, sb := legacyLookupManager(t, sandboxSessionKeyPrefix)
	m.sandboxes[sb.ID] = &sb
	m.operationGates[sb.ID] = newOperationGate(false)
	_, err := m.GetByWorkspace(context.Background(), "team/b")
	require.ErrorIs(t, err, ErrSandboxNotFound)
	_, err = m.GetByWorkspace(context.Background(), "team/a")
	require.ErrorIs(t, err, ErrWorkspaceLookupConflict)
}

func TestWorkspaceLookupLegacyDockerWithoutUID(t *testing.T) {
	m, store, _, sb := legacyLookupManager(t, legacySandboxSessionKeyPrefix)
	sb.RuntimeUID = ""
	raw, err := json.Marshal(sb)
	require.NoError(t, err)
	require.NoError(t, store.Set(context.Background(), legacySandboxSessionKeyPrefix+sb.ID, raw, 0))
	got, err := m.GetByWorkspace(context.Background(), "team/a")
	require.NoError(t, err, "full Docker container IDs are immutable identities")
	require.Equal(t, sb.ID, got.ID)
	after, err := store.Get(context.Background(), legacySandboxSessionKeyPrefix+sb.ID)
	require.NoError(t, err)
	require.Equal(t, raw, after)
}

type lookupInspectRuntime struct {
	runtime.Runtime
	inspect func(context.Context, string) (*runtime.SandboxInfo, error)
}

func (r lookupInspectRuntime) GetSandbox(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	return r.inspect(ctx, id)
}

func TestWorkspaceLookupRuntimeDependencyFailure(t *testing.T) {
	m, _, rt, _ := legacyLookupManager(t, sandboxSessionKeyPrefix)
	m.runtime = lookupInspectRuntime{Runtime: rt, inspect: func(context.Context, string) (*runtime.SandboxInfo, error) {
		return nil, errors.New("runtime API unavailable")
	}}
	_, err := m.GetByWorkspace(context.Background(), "team/a")
	require.ErrorIs(t, err, ErrWorkspaceLookupUnavailable)
	require.Zero(t, rt.removed)
}

func TestWorkspaceLookupOwnerChangesDuringInspection(t *testing.T) {
	managers, store, rt := distributedSyncManagers(t)
	sb, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	keys, err := workspaceStateKeysFromOwner(sb.Workspace.Owner)
	require.NoError(t, err)
	changed := sb.Workspace.Owner
	changed.Generation++
	raw, err := json.Marshal(changed)
	require.NoError(t, err)
	managers[1].runtime = lookupInspectRuntime{Runtime: rt, inspect: func(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
		require.NoError(t, store.Set(ctx, keys.owner, raw, 0))
		return rt.GetSandbox(ctx, id)
	}}
	_, err = managers[1].GetByWorkspace(context.Background(), "team/a")
	require.ErrorIs(t, err, ErrWorkspaceLookupConflict)
	after, err := store.Get(context.Background(), keys.owner)
	require.NoError(t, err)
	require.Equal(t, raw, after)
	rt.mu.Lock()
	require.Zero(t, rt.removed)
	rt.mu.Unlock()
}

type lookupInventoryStore struct {
	*atomicMemoryStore
	once       sync.Once
	duringScan func()
}

func (s *lookupInventoryStore) Keys(ctx context.Context, pattern string) ([]string, error) {
	s.once.Do(s.duringScan)
	return s.atomicMemoryStore.Keys(ctx, pattern)
}

func TestWorkspaceLookupAbsenceRechecksConcurrentAcquisition(t *testing.T) {
	m, store, _ := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	var lease *WorkspaceLease
	wrapper := &lookupInventoryStore{atomicMemoryStore: store, duringScan: func() {
		var err error
		lease, err = m.config.WorkspaceCoordinator.Acquire(context.Background(), WorkspaceLeaseRequest{
			MountType: WorkspaceMountSync, Provider: string(m.fsMeta.Provider), StorageIdentity: m.fsMeta.StorageIdentity,
			Bucket: m.fsMeta.Bucket, Prefix: "workspaces/team/b/", SandboxID: "sandbox-racer", Runtime: "docker", RuntimeID: "racing-container",
		})
		require.NoError(t, err)
	}}
	m.SetSessionStore(NewSessionStore(wrapper, time.Hour))
	_, err := m.GetByWorkspace(context.Background(), "team/b")
	require.ErrorIs(t, err, ErrWorkspaceLookupConflict)
	require.NotNil(t, lease)
	raw, err := store.Get(context.Background(), lease.ownerKey)
	require.NoError(t, err)
	require.NotEmpty(t, raw, "racing owner must be retained")
}

func TestWorkspaceLookupFUSEHealth(t *testing.T) {
	rt := newFUSEManagerRuntime()
	m, _, _, _ := newFUSETestManager(t, rt)
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	sb, err := m.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	got, err := m.GetByWorkspace(context.Background(), "team/a")
	require.NoError(t, err)
	require.Equal(t, sb.ID, got.ID)
	rt.mu.Lock()
	rt.health.Ready = false
	rt.mu.Unlock()
	_, err = m.GetByWorkspace(context.Background(), "team/a")
	require.ErrorIs(t, err, ErrWorkspaceLookupConflict)
	rt.mockRuntime.mu.Lock()
	require.Zero(t, rt.removed)
	rt.mockRuntime.mu.Unlock()
}

func TestWorkspaceLookupOwnerWithoutReusableRecord(t *testing.T) {
	for _, scenario := range []string{"missing_record", "expired_lease", "malformed_owner", "store_error"} {
		t.Run(scenario, func(t *testing.T) {
			managers, store, _ := distributedSyncManagers(t)
			sb, err := managers[0].Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
			require.NoError(t, err)
			keys, err := workspaceStateKeysFromOwner(sb.Workspace.Owner)
			require.NoError(t, err)
			want := ErrWorkspaceLookupConflict
			switch scenario {
			case "missing_record":
				repo := managers[0].activeSandboxes.(*memoryActiveRepository)
				repo.mu.Lock()
				delete(repo.records, sb.ID)
				repo.mu.Unlock()
			case "expired_lease":
				store.expireKey(keys.lease)
			case "malformed_owner":
				require.NoError(t, store.Set(context.Background(), keys.owner, []byte(`{}`), 0))
			case "store_error":
				store.mu.Lock()
				store.failMethods["Get"] = errors.New("state unavailable")
				store.mu.Unlock()
				want = ErrWorkspaceLookupUnavailable
			}
			_, err = managers[1].GetByWorkspace(context.Background(), "team/a")
			require.ErrorIs(t, err, want)
		})
	}
}

func legacyLookupManager(t *testing.T, prefix string) (*Manager, *atomicMemoryStore, *mockRuntime, Sandbox) {
	t.Helper()
	store := newAtomicMemoryStore()
	rt := newMockRuntime()
	m := NewManager(rt, nil, &storage.FileSystemMeta{Provider: storage.ProviderLocal, LocalPath: t.TempDir()}, ManagerConfig{RuntimeType: "docker"})
	m.SetSessionStore(NewSessionStore(store, time.Hour))
	sb := Sandbox{ID: "sandbox-old", RuntimeID: "old-container", RuntimeUID: "old-container", State: StateReady,
		CreatedAt: time.Now(), Timeout: -1, Config: SandboxConfig{Mode: ModePersistent}, Workspace: &WorkspaceInfo{RootPath: "team/a", BindMounted: true}}
	raw, err := json.Marshal(sb)
	require.NoError(t, err)
	require.NoError(t, store.Set(context.Background(), prefix+sb.ID, raw, 0))
	rt.sandboxes[sb.RuntimeID] = &runtime.SandboxInfo{ID: sb.ID, RuntimeID: sb.RuntimeID, RuntimeUID: sb.RuntimeUID, State: "running", WorkspaceHostPath: filepath.Join(m.fsMeta.LocalPath, "team/a")}
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	return m, store, rt, sb
}

func TestWorkspaceLookupLegacySessionsReadOnly(t *testing.T) {
	for _, prefix := range []string{sandboxSessionKeyPrefix, legacySandboxSessionKeyPrefix} {
		t.Run(prefix, func(t *testing.T) {
			m, store, _, sb := legacyLookupManager(t, prefix)
			before, err := store.Get(context.Background(), prefix+sb.ID)
			require.NoError(t, err)
			got, err := m.GetByWorkspace(context.Background(), "team/a")
			require.NoError(t, err)
			require.Equal(t, sb.ID, got.ID)
			keys, err := store.Keys(context.Background(), "sandbox:*")
			require.NoError(t, err)
			require.Equal(t, []string{prefix + sb.ID}, keys)
			after, err := store.Get(context.Background(), prefix+sb.ID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Empty(t, m.sandboxes)
		})
	}
}

func TestWorkspaceLookupLegacyStorageScope(t *testing.T) {
	for _, scenario := range []string{"different_local_root", "missing_bind_mount", "legacy_sync", "missing_pod_uid"} {
		t.Run(scenario, func(t *testing.T) {
			m, store, rt, sb := legacyLookupManager(t, legacySandboxSessionKeyPrefix)
			switch scenario {
			case "different_local_root":
				m.fsMeta.LocalPath = t.TempDir()
			case "missing_bind_mount":
				rt.sandboxes[sb.RuntimeID].WorkspaceHostPath = ""
			case "legacy_sync":
				m.fsMeta.Provider = storage.ProviderMinIO
			case "missing_pod_uid":
				m.config.RuntimeType = "kubernetes"
				sb.RuntimeUID = ""
				raw, err := json.Marshal(sb)
				require.NoError(t, err)
				require.NoError(t, store.Set(context.Background(), legacySandboxSessionKeyPrefix+sb.ID, raw, 0))
			}
			_, err := m.GetByWorkspace(context.Background(), "team/a")
			require.ErrorIs(t, err, ErrWorkspaceLookupConflict)
		})
	}
}

func TestWorkspaceLookupAmbiguityAndFailures(t *testing.T) {
	t.Run("ambiguous", func(t *testing.T) {
		m, store, _, sb := legacyLookupManager(t, sandboxSessionKeyPrefix)
		sb.ID = "sandbox-second"
		raw, err := json.Marshal(sb)
		require.NoError(t, err)
		require.NoError(t, store.Set(context.Background(), sandboxSessionKeyPrefix+sb.ID, raw, 0))
		_, err = m.GetByWorkspace(context.Background(), "team/a")
		require.ErrorIs(t, err, ErrWorkspaceLookupAmbiguous)
	})
	t.Run("store_unavailable", func(t *testing.T) {
		m, store, rt, _ := legacyLookupManager(t, sandboxSessionKeyPrefix)
		store.failMethods["Keys"] = errors.New("store unavailable")
		_, err := m.GetByWorkspace(context.Background(), "team/a")
		require.ErrorIs(t, err, ErrWorkspaceLookupUnavailable)
		require.Zero(t, rt.removed)
	})
	t.Run("missing_runtime", func(t *testing.T) {
		m, _, rt, sb := legacyLookupManager(t, sandboxSessionKeyPrefix)
		delete(rt.sandboxes, sb.RuntimeID)
		_, err := m.GetByWorkspace(context.Background(), "team/a")
		require.ErrorIs(t, err, ErrWorkspaceLookupConflict)
		require.Zero(t, rt.removed)
	})
	t.Run("invalid_and_absent", func(t *testing.T) {
		m, _, _, _ := legacyLookupManager(t, sandboxSessionKeyPrefix)
		for _, root := range []string{"", "../team/a", "/team/a", "team//a", "team/a/"} {
			_, err := m.GetByWorkspace(context.Background(), root)
			require.ErrorIs(t, err, storage.ErrInvalidWorkspacePrefix)
		}
		_, err := m.GetByWorkspace(context.Background(), "team/b")
		require.ErrorIs(t, err, ErrSandboxNotFound)
	})
}
