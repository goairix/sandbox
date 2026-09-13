package config

import (
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestRepositorySampleDocumentsOptionalNetworkRanges(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "config.yaml")
	v := viper.New()
	v.SetConfigFile(path)
	require.NoError(t, v.ReadInConfig())
	for _, key := range []string{"runtime.kubernetes.pod_cidrs", "runtime.kubernetes.service_cidrs"} {
		require.True(t, v.IsSet(key), "repository sample must document %s", key)
		require.Empty(t, v.GetStringSlice(key), "optional ranges should default to automatic discovery")
	}
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Empty(t, cfg.Runtime.Kubernetes.PodCIDRs)
	require.Empty(t, cfg.Runtime.Kubernetes.ServiceCIDRs)
	require.Equal(t, "standalone", cfg.Storage.State.Redis.Mode)
	require.Equal(t, "best_effort", cfg.Storage.State.Redis.Durability)
	require.False(t, cfg.Storage.State.Redis.RequireHA)
}
