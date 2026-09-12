package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

func TestDrainAuditAllowsGenerationCountersButRejectsLifecycleState(t *testing.T) {
	store := newAtomicMemoryStore()
	require.NoError(t, store.Set(context.Background(), "sandbox:workspace:generation:stable", []byte("9"), 0))
	require.NoError(t, AuditDrainedState(context.Background(), store))
	require.NoError(t, store.Set(context.Background(), "sandbox:ephemeral:v1:active", []byte("{}"), 0))
	require.ErrorContains(t, AuditDrainedState(context.Background(), store), "ephemeral")
}

func TestDrainAuditAllowsFUSEPoolMembershipGenerationHistory(t *testing.T) {
	store := newAtomicMemoryStore()
	require.NoError(t, store.Set(context.Background(), "fusepool:membership-generations", []byte("historical fencing state"), 0))
	require.NoError(t, AuditDrainedState(context.Background(), store))

	require.NoError(t, store.Set(context.Background(), "fusepool:record:active", []byte("{}"), 0))
	require.ErrorContains(t, AuditDrainedState(context.Background(), store), "FUSE pool")
}

func TestManagerStopPreservesPersistentAndFinalizesEphemeral(t *testing.T) {
	mgr, store, rt := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	mgr.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	persistent, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	ephemeral, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModeEphemeral, WorkspacePath: "team/b"})
	require.NoError(t, err)

	require.NoError(t, mgr.Stop(context.Background()))
	persistentSession, err := mgr.sessions.Exists(context.Background(), persistent.ID)
	require.NoError(t, err)
	assert.True(t, persistentSession)
	persistentRuntime, err := rt.GetSandbox(context.Background(), persistent.RuntimeID)
	require.NoError(t, err)
	assert.NotNil(t, persistentRuntime)
	assert.False(t, mgr.ephemeral.Exists(context.Background(), ephemeral.ID))
	assert.True(t, rt.wasRemoved(ephemeral.RuntimeID))
}

func TestManagerDrainReleaseFinalizesPersistentAndEphemeral(t *testing.T) {
	mgr, store, rt := newCoordinatedSyncManager(t, time.Minute, 10*time.Second)
	mgr.SetEphemeralLifecycleStore(NewEphemeralLifecycleStore(store))
	persistent, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	ephemeral, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModeEphemeral, WorkspacePath: "team/b"})
	require.NoError(t, err)

	require.NoError(t, mgr.DrainRelease(context.Background()))
	assert.True(t, rt.wasRemoved(persistent.RuntimeID))
	assert.True(t, rt.wasRemoved(ephemeral.RuntimeID))
	require.NoError(t, AuditDrainedState(context.Background(), store))
}

func TestManagerDrainReleaseRemovesOrphanedOrdinaryPoolRuntime(t *testing.T) {
	rt := newMockRuntime()
	rt.listedSandboxes = []runtime.SandboxInfo{{
		RuntimeID: "sandbox-pool-orphan",
		Labels:    map[string]string{"sandbox.managed": "true", "sandbox.pool": "true"},
	}}
	mgr := NewManager(rt, nil, nil, ManagerConfig{})

	require.NoError(t, mgr.DrainRelease(context.Background()))
	assert.True(t, rt.wasRemoved("sandbox-pool-orphan"))
	assert.Equal(t, map[string]string{"sandbox.managed": "true", "sandbox.pool": "true"}, rt.listLabels)
}

func TestManagerDrainReleaseDoesNotTreatFUSEPoolRuntimeAsOrdinary(t *testing.T) {
	rt := newMockRuntime()
	rt.listedSandboxes = []runtime.SandboxInfo{{
		RuntimeID: "sandbox-pool-fuse",
		Labels: map[string]string{
			"sandbox.managed":        "true",
			"sandbox.pool":           "true",
			"sandbox.workspace.mode": string(WorkspaceMountFUSE),
		},
	}}
	mgr := NewManager(rt, nil, nil, ManagerConfig{})

	require.NoError(t, mgr.DrainRelease(context.Background()))
	assert.False(t, rt.wasRemoved("sandbox-pool-fuse"))
}

func TestManagerDrainReleaseReportsOrdinaryPoolDiscoveryFailure(t *testing.T) {
	rt := newMockRuntime()
	rt.listSandboxesErr = errors.New("list failed")
	mgr := NewManager(rt, nil, nil, ManagerConfig{})

	err := mgr.DrainRelease(context.Background())
	require.ErrorContains(t, err, "list ordinary pool runtimes during release drain")
}

func TestManagerDrainReleaseReportsOrdinaryPoolRemovalFailure(t *testing.T) {
	rt := newMockRuntime()
	rt.listedSandboxes = []runtime.SandboxInfo{{
		RuntimeID: "sandbox-pool-stuck",
		Labels:    map[string]string{"sandbox.managed": "true", "sandbox.pool": "true"},
	}}
	rt.failRemove("sandbox-pool-stuck", errors.New("delete failed"))
	mgr := NewManager(rt, nil, nil, ManagerConfig{})

	err := mgr.DrainRelease(context.Background())
	require.ErrorContains(t, err, "remove ordinary pool runtime sandbox-pool-stuck during release drain")
}

func TestManagerDrainReleaseRejectsInvalidOrdinaryPoolIdentity(t *testing.T) {
	rt := newMockRuntime()
	rt.listedSandboxes = []runtime.SandboxInfo{{
		RuntimeID: "sandbox-pool-unmanaged",
		Labels:    map[string]string{"sandbox.pool": "true"},
	}}
	mgr := NewManager(rt, nil, nil, ManagerConfig{})

	err := mgr.DrainRelease(context.Background())
	require.ErrorContains(t, err, "ordinary pool runtime sandbox-pool-unmanaged has invalid pool identity")
	assert.False(t, rt.wasRemoved("sandbox-pool-unmanaged"))
}
