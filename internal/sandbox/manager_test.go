package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager_CreateEphemeralSandbox(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, ManagerConfig{
		PoolConfigs: map[Language]PoolConfig{
			LangPython: {Language: LangPython, MinSize: 2, MaxSize: 10, Image: "sandbox-python:latest"},
		},
		DefaultTimeout: 30,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Language: LangPython,
		Mode:     ModeEphemeral,
		Timeout:  30,
	})
	require.NoError(t, err)
	assert.Equal(t, StateReady, sb.State)
	assert.Equal(t, LangPython, sb.Config.Language)
	assert.Equal(t, ModeEphemeral, sb.Config.Mode)
}

func TestManager_CreatePersistentSandbox(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, ManagerConfig{
		PoolConfigs: map[Language]PoolConfig{
			LangNodeJS: {Language: LangNodeJS, MinSize: 1, MaxSize: 5, Image: "sandbox-nodejs:latest"},
		},
		DefaultTimeout: 60,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Language: LangNodeJS,
		Mode:     ModePersistent,
		Timeout:  60,
	})
	require.NoError(t, err)
	assert.Equal(t, StateReady, sb.State)
	assert.Equal(t, ModePersistent, sb.Config.Mode)

	// Should be retrievable by ID
	got, err := mgr.Get(ctx, sb.ID)
	require.NoError(t, err)
	assert.Equal(t, sb.ID, got.ID)
}

func TestManager_Destroy(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, ManagerConfig{
		PoolConfigs: map[Language]PoolConfig{
			LangBash: {Language: LangBash, MinSize: 1, MaxSize: 5, Image: "sandbox-bash:latest"},
		},
		DefaultTimeout: 30,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Language: LangBash,
		Mode:     ModeEphemeral,
	})
	require.NoError(t, err)

	err = mgr.Destroy(ctx, sb.ID)
	require.NoError(t, err)

	_, err = mgr.Get(ctx, sb.ID)
	assert.Error(t, err)
}
