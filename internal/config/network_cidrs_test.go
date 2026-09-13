package config

import (
	"github.com/stretchr/testify/require"
	"reflect"
	"testing"
)

func TestAuthoritativeNetworkCIDRsLoadFromEnvironment(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")
	t.Setenv("SANDBOX_RUNTIME_KUBERNETES_POD_CIDRS", "10.42.0.0/16,fd00:42::/64")
	t.Setenv("SANDBOX_RUNTIME_KUBERNETES_SERVICE_CIDRS", "10.96.0.0/12,fd00:96::/112")
	cfg, err := Load("")
	require.NoError(t, err)
	value := reflect.ValueOf(cfg.Runtime.Kubernetes)
	require.True(t, value.FieldByName("PodCIDRs").IsValid(), "configured authoritative PodCIDRs must not be discarded")
	require.Equal(t, []string{"10.42.0.0/16", "fd00:42::/64"}, value.FieldByName("PodCIDRs").Interface())
	require.Equal(t, []string{"10.96.0.0/12", "fd00:96::/112"}, value.FieldByName("ServiceCIDRs").Interface())
}

func TestAuthoritativeNetworkCIDRsRejectPartialOrInvalidConfiguration(t *testing.T) {
	for _, services := range []string{"", "not-a-cidr"} {
		t.Run(services, func(t *testing.T) {
			t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")
			t.Setenv("SANDBOX_RUNTIME_KUBERNETES_POD_CIDRS", "10.42.0.0/16")
			t.Setenv("SANDBOX_RUNTIME_KUBERNETES_SERVICE_CIDRS", services)
			_, err := Load("")
			require.Error(t, err)
		})
	}
}

func TestAuthoritativeNetworkCIDRsRejectMismatchedAddressFamilies(t *testing.T) {
	for _, sets := range []struct{ pods, services string }{
		{"10.42.0.0/16", "10.96.0.0/12,fd00:96::/112"},
		{"10.42.0.0/16,fd00:42::/64", "10.96.0.0/12"},
	} {
		t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")
		t.Setenv("SANDBOX_RUNTIME_KUBERNETES_POD_CIDRS", sets.pods)
		t.Setenv("SANDBOX_RUNTIME_KUBERNETES_SERVICE_CIDRS", sets.services)
		_, err := Load("")
		require.Error(t, err)
	}
}
