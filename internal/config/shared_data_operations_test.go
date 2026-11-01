package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSharedDataConfigurationDefaultsAndOverrides(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "unit-test-key")
	cfg, err := config.Load("")
	require.NoError(t, err)
	require.False(t, cfg.Storage.State.SharedDataOperations)
	t.Setenv("SANDBOX_STORAGE_STATE_SHARED_DATA_OPERATIONS", "true")
	cfg, err = config.Load("")
	require.NoError(t, err)
	require.True(t, cfg.Storage.State.SharedDataOperations)
	t.Setenv("SANDBOX_STORAGE_STATE_SHARED_DATA_OPERATIONS", "false")
	p := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(p, []byte("storage:\n  state:\n    shared_data_operations: true\n"), 0600))
	cfg, err = config.Load(p)
	require.NoError(t, err)
	require.False(t, cfg.Storage.State.SharedDataOperations, "env has precedence")
	require.NoError(t, os.Unsetenv("SANDBOX_STORAGE_STATE_SHARED_DATA_OPERATIONS"))
	cfg, err = config.Load(p)
	require.NoError(t, err)
	require.True(t, cfg.Storage.State.SharedDataOperations)
}
