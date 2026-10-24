package config_test

import (
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/require"
)

func TestEtcdStateConfigurationRequiresIsolatedTLSNamespace(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")
	t.Setenv("SANDBOX_STORAGE_STATE_BACKEND", "etcd")

	_, err := config.Load("")
	require.ErrorContains(t, err, "storage.state.etcd.endpoints")

	t.Setenv("SANDBOX_STORAGE_STATE_ETCD_ENDPOINTS", "etcd-a:2379,etcd-b:2379")
	_, err = config.Load("")
	require.ErrorContains(t, err, "storage.state.etcd.prefix")

	t.Setenv("SANDBOX_STORAGE_STATE_ETCD_PREFIX", "/sandbox/test-cell/")
	_, err = config.Load("")
	require.ErrorContains(t, err, "storage.state.etcd.ca_file")

	t.Setenv("SANDBOX_STORAGE_STATE_ETCD_CA_FILE", "/etc/sandbox/etcd-ca.pem")
	t.Setenv("SANDBOX_STORAGE_STATE_ETCD_CERT_FILE", "/etc/sandbox/etcd-client.pem")
	t.Setenv("SANDBOX_STORAGE_STATE_ETCD_KEY_FILE", "/etc/sandbox/etcd-client-key.pem")
	t.Setenv("SANDBOX_STORAGE_STATE_ETCD_TIMEOUT_MS", "3000")
	cfg, err := config.Load("")
	require.NoError(t, err)
	require.Equal(t, "etcd", cfg.Storage.State.Backend)
	require.Equal(t, []string{"etcd-a:2379", "etcd-b:2379"}, cfg.Storage.State.Etcd.Endpoints)
	require.Empty(t, cfg.Storage.State.Redis.Addr)

	t.Setenv("SANDBOX_STORAGE_STATE_REDIS_ADDR", "redis:6379")
	_, err = config.Load("")
	require.ErrorContains(t, err, "Redis and etcd cannot be configured together")
}
