package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKubernetesNetworkProviderRejectsUnsupportedValue(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "unit-test-key")
	t.Setenv("SANDBOX_RUNTIME_KUBERNETES_NETWORK_POLICY_PROVIDER", "unknown")
	_, err := Load("")
	require.Error(t, err)
	require.Contains(t, err.Error(), "network_policy_provider")
}
