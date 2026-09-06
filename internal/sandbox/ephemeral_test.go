package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/storage"
)

func validEphemeralRecord() EphemeralLifecycleRecord {
	return EphemeralLifecycleRecord{
		Version: 1, SandboxID: "sandbox-ephemeral", RuntimeID: "runtime-a", RuntimeUID: "uid-a",
		WorkspacePath: "jobs/a", MountType: WorkspaceMountSync,
		Owner: WorkspaceOwner{
			MountType: WorkspaceMountSync, Provider: "minio", StorageIdentityHash: storageIdentityHash("storage-primary"),
			Bucket: "sandbox", Prefix: "workspaces/jobs/a/", WorkspaceHash: hashWorkspaceIdentity("minio", storageIdentityHash("storage-primary"), "sandbox", "workspaces/jobs/a/"),
			SandboxID: "sandbox-ephemeral", Runtime: "docker", RuntimeID: "runtime-a", RuntimeUID: "uid-a",
			Generation: 1, UpdatedAt: time.Now().UTC(),
		},
		State: EphemeralActive, Revision: 1, UpdatedAt: time.Now().UTC(),
	}
}

func TestEphemeralLifecycleStoreUsesIsolatedNamespace(t *testing.T) {
	store := newAtomicMemoryStore()
	lifecycle := NewEphemeralLifecycleStore(store)
	record := validEphemeralRecord()
	require.NoError(t, lifecycle.Create(context.Background(), record))
	assert.True(t, store.hasKey("sandbox:ephemeral:v1:"+record.SandboxID))
	assert.False(t, store.hasKey("sandbox:session:v2:"+record.SandboxID))
}

func TestEphemeralLifecycleStoreRejectsMalformedAndModeInconsistentRecords(t *testing.T) {
	store := newAtomicMemoryStore()
	lifecycle := NewEphemeralLifecycleStore(store)

	cases := map[string]func(*EphemeralLifecycleRecord){
		"empty runtime uid":   func(r *EphemeralLifecycleRecord) { r.RuntimeUID = "" },
		"unknown mount mode":  func(r *EphemeralLifecycleRecord) { r.MountType = WorkspaceMountLocal },
		"sync has fuse field": func(r *EphemeralLifecycleRecord) { r.PreparationID = "prep-a" },
		"fuse missing fields": func(r *EphemeralLifecycleRecord) {
			r.MountType = WorkspaceMountFUSE
			r.Owner.MountType = WorkspaceMountFUSE
		},
		"invalid state": func(r *EphemeralLifecycleRecord) { r.State = "unknown" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			record := validEphemeralRecord()
			edit(&record)
			require.Error(t, lifecycle.Create(context.Background(), record))
		})
	}

	record := validEphemeralRecord()
	raw, err := json.Marshal(record)
	require.NoError(t, err)
	raw[len(raw)-1] = ','
	raw = append(raw, []byte(`"unknown":true}`)...)
	require.NoError(t, store.Set(context.Background(), ephemeralLifecycleKeyPrefix+record.SandboxID, raw, 0))
	_, err = lifecycle.Load(context.Background(), record.SandboxID)
	require.Error(t, err)
}

func TestEphemeralLifecycleStoreTransitionsAndRemovesExactRevision(t *testing.T) {
	store := newAtomicMemoryStore()
	lifecycle := NewEphemeralLifecycleStore(store)
	record := validEphemeralRecord()
	require.NoError(t, lifecycle.Create(context.Background(), record))

	next, err := lifecycle.Transition(context.Background(), record.SandboxID, record.Revision, EphemeralFinalizing)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), next.Revision)
	_, err = lifecycle.Transition(context.Background(), record.SandboxID, record.Revision, EphemeralRemovingRuntime)
	require.Error(t, err)
	require.Error(t, lifecycle.RemoveExact(context.Background(), record))
	require.NoError(t, lifecycle.RemoveExact(context.Background(), *next))
}

func TestEphemeralFUSECreateDoesNotPublishUserSession(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, _, _, store := newFUSETestManager(t, rt)
	ephemeral := NewEphemeralLifecycleStore(store)
	mgr.SetEphemeralLifecycleStore(ephemeral)

	sb, err := mgr.Create(context.Background(), SandboxConfig{
		Mode: ModeEphemeral, WorkspacePath: "jobs/a", WorkspaceMountMode: WorkspaceMountFUSE,
	})
	require.NoError(t, err)
	exists, err := mgr.sessions.Exists(context.Background(), sb.ID)
	require.NoError(t, err)
	assert.False(t, exists)
	assert.True(t, ephemeral.Exists(context.Background(), sb.ID))
}

func TestEphemeralSyncCreateUsesOnlyCleanupNamespace(t *testing.T) {
	mgr, store, _ := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	ephemeral := NewEphemeralLifecycleStore(store)
	mgr.SetEphemeralLifecycleStore(ephemeral)

	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModeEphemeral, WorkspacePath: "team/a"})
	require.NoError(t, err)
	exists, err := mgr.sessions.Exists(context.Background(), sb.ID)
	require.NoError(t, err)
	assert.False(t, exists)
	record, err := ephemeral.Load(context.Background(), sb.ID)
	require.NoError(t, err)
	assert.Equal(t, WorkspaceMountSync, record.MountType)
	assert.Empty(t, record.PreparationID)
}

func TestEphemeralSandboxWithoutWorkspaceUsesNoLifecycleNamespace(t *testing.T) {
	mgr, store, _ := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	ephemeral := NewEphemeralLifecycleStore(store)
	mgr.SetEphemeralLifecycleStore(ephemeral)

	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModeEphemeral})
	require.NoError(t, err)
	exists, err := mgr.sessions.Exists(context.Background(), sb.ID)
	require.NoError(t, err)
	assert.False(t, exists)
	assert.False(t, ephemeral.Exists(context.Background(), sb.ID))
}

func TestDestroyEphemeralFUSERetainsRuntimeAndLeaseWhenFlushFails(t *testing.T) {
	rt := newFUSEManagerRuntime()
	mgr, _, _, store := newFUSETestManager(t, rt)
	ephemeral := NewEphemeralLifecycleStore(store)
	mgr.SetEphemeralLifecycleStore(ephemeral)
	sb, err := mgr.Create(context.Background(), SandboxConfig{
		Mode: ModeEphemeral, WorkspacePath: "jobs/a", WorkspaceMountMode: WorkspaceMountFUSE,
	})
	require.NoError(t, err)
	rt.mockRuntime.mu.Lock()
	rt.flushErr = errors.New("durable flush failed")
	rt.mockRuntime.mu.Unlock()

	err = mgr.Destroy(context.Background(), sb.ID)
	require.ErrorIs(t, err, ErrSandboxCleanupPending)
	assert.False(t, rt.wasRemoved(sb.RuntimeID))
	assert.True(t, ephemeral.Exists(context.Background(), sb.ID))
	assert.True(t, store.hasKey(mgr.fuseLifecycles[sb.ID].lease.Key))
	rt.mockRuntime.mu.Lock()
	rt.flushErr = nil
	rt.mockRuntime.mu.Unlock()
	mgr.Stop(context.Background())
}

func TestStartupFinalizesEphemeralFUSEWithoutRestoringUserSession(t *testing.T) {
	rt := newFUSEManagerRuntime()
	first, _, repo, store := newFUSETestManager(t, rt)
	first.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	sb, err := first.Create(context.Background(), SandboxConfig{
		Mode: ModeEphemeral, WorkspacePath: "jobs/a", WorkspaceMountMode: WorkspaceMountFUSE,
	})
	require.NoError(t, err)
	first.mu.RLock()
	crashed := first.fuseLifecycles[sb.ID]
	first.mu.RUnlock()
	crashed.cancel()
	crashed.renewal.Stop()
	store.expireKey(crashed.lease.Key)

	pool := NewFUSEPool(rt, repo, fusePoolConfig(), fixedFUSESpec("pool-key"))
	profile, err := storage.RootMarkerProfileByID(storage.RootMarkerProfileMinIO)
	require.NoError(t, err)
	restored := NewManager(rt, nil, first.fsMeta, ManagerConfig{
		RuntimeType: "docker", DefaultMountMode: WorkspaceMountFUSE,
		EnabledMountModes: map[WorkspaceMountType]bool{WorkspaceMountFUSE: true},
		FUSEPool:          pool, WorkspaceCoordinator: NewWorkspaceCoordinator(store, time.Minute, 10*time.Second),
		WorkspaceObjectClient: &fuseMarkerClient{exists: true}, WorkspaceMarkerProfile: profile,
		FUSEHealthInterval: time.Hour,
	})
	restored.SetSessionStore(NewSessionStore(store, time.Hour))
	restored.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	require.NoError(t, restored.Start(context.Background()))
	t.Cleanup(func() { restored.Stop(context.Background()) })

	_, err = restored.Get(context.Background(), sb.ID)
	require.ErrorIs(t, err, ErrSandboxNotFound)
	assert.False(t, restored.ephemeral.Exists(context.Background(), sb.ID))
	assert.True(t, rt.wasRemoved(sb.RuntimeID))
}
