package config_test

import (
	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"testing"
)

func TestRedisBootstrapGateEnvironment(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_MODE", "sentinel")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_MASTER_NAME", "sandbox")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_ADDR", "")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_ADDRS", "redis-0:26379,redis-1:26379,redis-2:26379")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_REQUIRE_HA", "true")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_DURABILITY", "replica_ack")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_BOOTSTRAP_STATE_DIRECTORY", "/bootstrap")
	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_BOOTSTRAP_PUBLIC_KEYS_FILE", "/identity-public/public-keys.json")
	cfg, err := config.Load("")
	require.NoError(t, err)
	require.Equal(t, "/bootstrap", cfg.Storage.State.Redis.BootstrapStateDirectory)
	require.Equal(t, "/identity-public/public-keys.json", cfg.Storage.State.Redis.BootstrapPublicKeysFile)
}

func TestRedisBootstrapGateRejectsUnsafeConfiguration(t *testing.T) {
	for name, changes := range map[string]map[string]string{
		"missing public":  {"BOOTSTRAP_PUBLIC_KEYS_FILE": ""},
		"missing state":   {"BOOTSTRAP_STATE_DIRECTORY": ""},
		"relative state":  {"BOOTSTRAP_STATE_DIRECTORY": "bootstrap"},
		"root state":      {"BOOTSTRAP_STATE_DIRECTORY": "/"},
		"relative public": {"BOOTSTRAP_PUBLIC_KEYS_FILE": "public.json"},
		"root public":     {"BOOTSTRAP_PUBLIC_KEYS_FILE": "/"},
		"no HA":           {"REQUIRE_HA": "false"},
		"native":          {"DURABILITY": "native"},
		"ack zero":        {"ACK_REPLICAS": "0"},
		"ack two":         {"ACK_REPLICAS": "2"},
		"missing members": {"ADDRS": "redis-0:26379,redis-1:26379"},
		"not sentinel":    {"MODE": "cluster", "MASTER_NAME": ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")
			for key, value := range map[string]string{"MODE": "sentinel", "MASTER_NAME": "sandbox", "ADDR": "", "ADDRS": "redis-0:26379,redis-1:26379,redis-2:26379", "REQUIRE_HA": "true", "DURABILITY": "replica_ack", "ACK_REPLICAS": "1", "BOOTSTRAP_STATE_DIRECTORY": "/bootstrap", "BOOTSTRAP_PUBLIC_KEYS_FILE": "/identity-public/public-keys.json"} {
				t.Setenv("SANDBOX_STORAGE_STATE_REDIS_"+key, value)
			}
			for key, value := range changes {
				t.Setenv("SANDBOX_STORAGE_STATE_REDIS_"+key, value)
			}
			_, err := config.Load("")
			require.Error(t, err, "unsafe bootstrap configuration was accepted")
		})
	}
}

func TestRedisBootstrapGateYAMLAndDefaults(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")
	cfg, err := config.Load("")
	require.NoError(t, err)
	require.Empty(t, cfg.Storage.State.Redis.BootstrapStateDirectory)
	require.Empty(t, cfg.Storage.State.Redis.BootstrapPublicKeysFile)
	path := filepath.Join(t.TempDir(), "config.yaml")
	yaml := []byte("storage:\n  state:\n    redis:\n      mode: sentinel\n      master_name: sandbox\n      addrs: [redis-0:26379, redis-1:26379, redis-2:26379]\n      require_ha: true\n      durability: replica_ack\n      ack_replicas: 1\n      bootstrap_state_directory: /bootstrap\n      bootstrap_public_keys_file: /identity-public/public-keys.json\n")
	require.NoError(t, os.WriteFile(path, yaml, 0600))
	cfg, err = config.Load(path)
	require.NoError(t, err)
	require.Equal(t, "/bootstrap", cfg.Storage.State.Redis.BootstrapStateDirectory)
	require.Equal(t, "/identity-public/public-keys.json", cfg.Storage.State.Redis.BootstrapPublicKeysFile)
}
