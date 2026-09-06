package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDrainAuditAllowsGenerationCountersButRejectsLifecycleState(t *testing.T) {
	store := newAtomicMemoryStore()
	require.NoError(t, store.Set(context.Background(), "sandbox:workspace:generation:stable", []byte("9"), 0))
	require.NoError(t, AuditDrainedState(context.Background(), store))
	require.NoError(t, store.Set(context.Background(), "sandbox:ephemeral:v1:active", []byte("{}"), 0))
	require.ErrorContains(t, AuditDrainedState(context.Background(), store), "ephemeral")
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
