package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSentinelAuthEnvironment(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_USERNAME", "data-user")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_PASSWORD", "data-secret")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_SENTINEL_USERNAME", "sentinel-user")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_SENTINEL_PASSWORD", "sentinel-secret")
	cfg, err := config.Load("")
	require.NoError(t, err)
	require.Equal(t, "data-user", cfg.Storage.State.Redis.Username)
	require.Equal(t, "data-secret", cfg.Storage.State.Redis.Password)
	require.Equal(t, "sentinel-user", cfg.Storage.State.Redis.SentinelUsername)
	require.Equal(t, "sentinel-secret", cfg.Storage.State.Redis.SentinelPassword)
}

func TestSentinelAuthYAMLAndDefaults(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")
	cfg, err := config.Load("")
	require.NoError(t, err)
	require.Empty(t, cfg.Storage.State.Redis.SentinelUsername)
	require.Empty(t, cfg.Storage.State.Redis.SentinelPassword)
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte("storage:\n  state:\n    redis:\n      username: data-user\n      password: data-secret\n      sentinel_username: sentinel-user\n      sentinel_password: sentinel-secret\n"), 0600))
	cfg, err = config.Load(path)
	require.NoError(t, err)
	require.Equal(t, "data-user", cfg.Storage.State.Redis.Username)
	require.Equal(t, "data-secret", cfg.Storage.State.Redis.Password)
	require.Equal(t, "sentinel-user", cfg.Storage.State.Redis.SentinelUsername)
	require.Equal(t, "sentinel-secret", cfg.Storage.State.Redis.SentinelPassword)
}

func TestRedisRejectsConflictingMasterName(t *testing.T) {
	for _, mode := range []string{"standalone", "cluster"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")
			t.Setenv("SANDBOX_STORAGE_STATE_REDIS_MODE", mode)
			t.Setenv("SANDBOX_STORAGE_STATE_REDIS_MASTER_NAME", "leftover")
			_, err := config.Load("")
			require.ErrorContains(t, err, "master_name is only valid in sentinel mode")
		})
	}
}
