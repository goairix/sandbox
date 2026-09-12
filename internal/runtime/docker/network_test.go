package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	dnetwork "github.com/docker/docker/api/types/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

func TestCleanupStaleEmptySandboxNetworksOnlyRemovesOwnedEmptyNetworks(t *testing.T) {
	_, fake := newFakeDockerRuntime(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	fake.networks = map[string]dnetwork.Inspect{
		"stale": {
			ID: "stale", Name: pairNetworkPrefix + "stale", Created: now.Add(-6 * time.Minute),
			Labels: map[string]string{"sandbox.managed": "true"}, Containers: map[string]dnetwork.EndpointResource{},
		},
		"fresh": {
			ID: "fresh", Name: pairNetworkPrefix + "fresh", Created: now.Add(-time.Minute),
			Labels: map[string]string{"sandbox.managed": "true"}, Containers: map[string]dnetwork.EndpointResource{},
		},
		"attached": {
			ID: "attached", Name: pairNetworkPrefix + "attached", Created: now.Add(-time.Hour),
			Labels: map[string]string{"sandbox.managed": "true"}, Containers: map[string]dnetwork.EndpointResource{"container": {}},
		},
		"unowned": {
			ID: "unowned", Name: pairNetworkPrefix + "unowned", Created: now.Add(-time.Hour),
			Containers: map[string]dnetwork.EndpointResource{},
		},
		"other": {
			ID: "other", Name: "application-network", Created: now.Add(-time.Hour),
			Labels: map[string]string{"sandbox.managed": "true"}, Containers: map[string]dnetwork.EndpointResource{},
		},
	}

	removed, err := cleanupStaleEmptySandboxNetworks(context.Background(), fake, now)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	_, staleExists := fake.networks["stale"]
	assert.False(t, staleExists)
	for _, id := range []string{"fresh", "attached", "unowned", "other"} {
		_, exists := fake.networks[id]
		assert.True(t, exists, "%s must be preserved", id)
	}
}

func TestCleanupStaleSandboxNetworkResourcesRemovesOnlyOrphanGateways(t *testing.T) {
	_, fake := newFakeDockerRuntime(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour)
	fresh := now.Add(-time.Minute)
	fake.containers = map[string]*fakeContainer{
		"orphan-gateway": {
			config: &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "orphan", "sandbox.role": "gateway"}},
			name:   gatewayNamePrefix + "orphan", created: old.Unix(), running: true,
		},
		"active-gateway": {
			config: &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "active", "sandbox.role": "gateway"}},
			name:   gatewayNamePrefix + "active", created: old.Unix(), running: true,
		},
		"active-runtime": {
			config: &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "sandbox-pool-old"}},
			name:   "active", created: old.Unix(), running: false,
		},
		"fresh-gateway": {
			config: &container.Config{Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "fresh", "sandbox.role": "gateway"}},
			name:   gatewayNamePrefix + "fresh", created: fresh.Unix(), running: true,
		},
	}
	fake.networks = map[string]dnetwork.Inspect{
		"orphan-network": {
			ID: "orphan-network", Name: pairNetworkPrefix + "orphan", Created: old,
			Labels: map[string]string{"sandbox.managed": "true"}, Containers: map[string]dnetwork.EndpointResource{"orphan-gateway": {}},
		},
		"active-network": {
			ID: "active-network", Name: pairNetworkPrefix + "active", Created: old,
			Labels: map[string]string{"sandbox.managed": "true"}, Containers: map[string]dnetwork.EndpointResource{"active-gateway": {}, "active-runtime": {}},
		},
		"fresh-network": {
			ID: "fresh-network", Name: pairNetworkPrefix + "fresh", Created: fresh,
			Labels: map[string]string{"sandbox.managed": "true"}, Containers: map[string]dnetwork.EndpointResource{"fresh-gateway": {}},
		},
	}

	removed, err := cleanupStaleSandboxNetworkResources(context.Background(), fake, now)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	_, orphanGatewayExists := fake.containers["orphan-gateway"]
	assert.False(t, orphanGatewayExists)
	_, orphanNetworkExists := fake.networks["orphan-network"]
	assert.False(t, orphanNetworkExists)
	for _, id := range []string{"active-gateway", "active-runtime", "fresh-gateway"} {
		_, exists := fake.containers[id]
		assert.True(t, exists, "%s must be preserved", id)
	}
	for _, id := range []string{"active-network", "fresh-network"} {
		_, exists := fake.networks[id]
		assert.True(t, exists, "%s must be preserved", id)
	}
}

func TestCreateManagedPairNetworkRetriesOnceAfterAddressPoolExhaustion(t *testing.T) {
	_, fake := newFakeDockerRuntime(t)
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	fake.networks["stale"] = dnetwork.Inspect{
		ID: "stale", Name: pairNetworkPrefix + "stale", Created: now.Add(-time.Hour),
		Labels: map[string]string{"sandbox.managed": "true"}, Containers: map[string]dnetwork.EndpointResource{},
	}
	fake.networkCreateErrors = []error{errors.New("could not find an available, non-overlapping IPv4 address pool among the defaults to assign to the network"), nil}

	response, err := createManagedPairNetwork(context.Background(), fake, pairNetworkPrefix+"new", dnetwork.CreateOptions{
		Driver: "bridge", Labels: map[string]string{"sandbox.managed": "true"},
	}, now)
	require.NoError(t, err)
	assert.NotEmpty(t, response.ID)
	assert.Equal(t, 2, fake.networkCreateCalls)
	_, staleExists := fake.networks["stale"]
	assert.False(t, staleExists)
}

func TestCreateManagedPairNetworkDoesNotRetryOtherErrors(t *testing.T) {
	_, fake := newFakeDockerRuntime(t)
	fake.networkCreateErrors = []error{errors.New("permission denied")}

	_, err := createManagedPairNetwork(context.Background(), fake, pairNetworkPrefix+"new", dnetwork.CreateOptions{}, time.Now())
	require.ErrorContains(t, err, "permission denied")
	assert.Equal(t, 1, fake.networkCreateCalls)
}

func TestFUSEPairNetworkExplicitlyDisablesIPv6(t *testing.T) {
	rt, fake := newFakeDockerRuntime(t)
	spec := fuseDockerSpecForTest()
	rt.gatewayImage = "registry.example.com/sandbox-gateway:v0.2.12"

	_, _, _, err := createFUSESandboxPair(context.Background(), fake, spec.ID, rt.openNetworkID, rt.gatewayImage, dockerWorkspaceSecretRoot, spec.WorkspaceFUSE.CASecretKey, spec.WorkspaceFUSE.SystemEgress)
	require.NoError(t, err)
	pair := fake.networkCreateOptions[pairNetworkPrefix+spec.ID]
	require.NotNil(t, pair.EnableIPv6)
	assert.False(t, *pair.EnableIPv6)
}

func TestFUSEGatewaySystemPolicyUsesExactProtocolsAndPermanentDeny(t *testing.T) {
	command, err := buildFUSEGatewayIptablesCmd(fuseDockerSpecForTest().WorkspaceFUSE.SystemEgress, false, nil, false)
	require.NoError(t, err)

	assert.Contains(t, command, "-A SBOX_SYSTEM -d 1.1.1.1/32 -p udp --dport 53 -j ACCEPT")
	assert.Contains(t, command, "-A SBOX_SYSTEM -d 1.1.1.1/32 -p tcp --dport 53 -j ACCEPT")
	assert.Contains(t, command, "-A SBOX_SYSTEM -d 198.51.100.10/32 -p tcp --dport 9000 -j ACCEPT")
	assert.NotContains(t, command, "-A SBOX_SYSTEM -d 198.51.100.10/32 -p udp")
	for _, denied := range []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4"} {
		assert.Contains(t, command, "-A SBOX_PERMANENT -d "+denied+" -j DROP")
	}
	assert.Less(t, strings.Index(command, "-A FORWARD -j SBOX_PERMANENT"), strings.Index(command, "-A FORWARD -j SBOX_SYSTEM"))
	assert.Less(t, strings.Index(command, "-A FORWARD -j SBOX_SYSTEM"), strings.Index(command, "-A FORWARD -j SBOX_USER"))
}

func TestFUSEUserUpdateDoesNotFlushOrRewriteSystemChains(t *testing.T) {
	command, err := buildFUSEGatewayUserIptablesCmd(true, []string{"203.0.113.7/32"}, false)
	require.NoError(t, err)

	assert.Contains(t, command, "iptables-restore --noflush")
	assert.Contains(t, command, "-F SBOX_USER")
	assert.Contains(t, command, "-A SBOX_USER -d 203.0.113.7/32 -j ACCEPT")
	assert.NotContains(t, command, "-F FORWARD")
	assert.NotContains(t, command, "SBOX_SYSTEM")
	assert.NotContains(t, command, "SBOX_PERMANENT")
}

func TestFUSEPermanentDenyPrecedesUserWhitelistAndOpenAccess(t *testing.T) {
	initial, err := buildFUSEGatewayIptablesCmd(fuseDockerSpecForTest().WorkspaceFUSE.SystemEgress, false, nil, false)
	require.NoError(t, err)
	user, err := buildFUSEGatewayUserIptablesCmd(true, []string{"169.254.169.254/32"}, false)
	require.NoError(t, err)

	assert.Less(t, strings.Index(initial, "-A FORWARD -j SBOX_PERMANENT"), strings.Index(initial, "-A FORWARD -j SBOX_USER"))
	assert.Contains(t, user, "-A SBOX_USER -d 169.254.169.254/32 -j ACCEPT")
	assert.Contains(t, initial, "-A SBOX_PERMANENT -d 169.254.0.0/16 -j DROP")
}

func TestFUSEGatewayCIDRsAreSortedAndDeduplicated(t *testing.T) {
	system := runtime.SystemEgressSpec{
		Mode:     runtime.SystemEgressCIDR,
		DNSCIDRs: []string{"1.1.1.1/32", "1.0.0.1/32", "1.1.1.1/32"}, DNSPorts: []int32{53, 53},
		EndpointCIDRs: []string{"203.0.113.20/32", "198.51.100.10/32", "203.0.113.20/32"}, EndpointPorts: []int32{9000, 443, 9000},
	}
	command, err := buildFUSEGatewayIptablesCmd(system, false, nil, false)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(command, "-d 1.1.1.1/32"), "one DNS CIDR produces exactly TCP and UDP rules")
	assert.Equal(t, 2, strings.Count(command, "-d 203.0.113.20/32"), "one endpoint CIDR produces one rule per deduplicated endpoint port")
	assert.Less(t, strings.Index(command, "-d 1.0.0.1/32"), strings.Index(command, "-d 1.1.1.1/32"))
	assert.Less(t, strings.Index(command, "-d 198.51.100.10/32"), strings.Index(command, "-d 203.0.113.20/32"))
}

func TestFUSEGatewayRejectsIPv6AndForbiddenSystemCIDRs(t *testing.T) {
	tests := []runtime.SystemEgressSpec{
		{Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"2001:4860:4860::8888/128"}, DNSPorts: []int32{53}, EndpointCIDRs: []string{"198.51.100.10/32"}, EndpointPorts: []int32{443}},
		{Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"10.0.0.53/32"}, DNSPorts: []int32{53}, EndpointCIDRs: []string{"198.51.100.10/32"}, EndpointPorts: []int32{443}},
		{Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"169.254.169.254/32"}, DNSPorts: []int32{53}, EndpointCIDRs: []string{"198.51.100.10/32"}, EndpointPorts: []int32{443}},
		{Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"1.1.1.1/32"}, DNSPorts: []int32{53}, EndpointCIDRs: []string{"2001:db8::10/128"}, EndpointPorts: []int32{443}},
		{Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"1.1.1.1/32"}, DNSPorts: []int32{53}, EndpointCIDRs: []string{"169.254.169.254/32"}, EndpointPorts: []int32{443}},
		{Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"1.1.1.1/32"}, DNSPorts: []int32{53}, EndpointCIDRs: []string{"0.0.0.0/0"}, EndpointPorts: []int32{443}},
	}
	for index, system := range tests {
		t.Run(string(rune('a'+index)), func(t *testing.T) {
			_, err := buildFUSEGatewayIptablesCmd(system, false, nil, false)
			require.Error(t, err)
		})
	}

	_, err := buildFUSEGatewayUserIptablesCmd(true, []string{"2001:db8::10/128"}, false)
	require.Error(t, err)
}

func TestFUSEGatewayRequiresExactDNSPort53(t *testing.T) {
	for _, ports := range [][]int32{{}, {54}, {53, 54}} {
		system := fuseDockerSpecForTest().WorkspaceFUSE.SystemEgress
		system.DNSPorts = ports
		_, err := buildFUSEGatewayIptablesCmd(system, false, nil, false)
		require.Error(t, err)
	}
}

func TestResolveFUSEWhitelistRejectsExplicitIPv6(t *testing.T) {
	for _, value := range []string{"2001:db8::10", "2001:db8::10/128"} {
		_, err := resolveFUSEWhitelist([]string{value})
		require.Error(t, err)
	}
}

func TestResolveFUSEWhitelistCanonicalizesIPv4AddressesToHostCIDRs(t *testing.T) {
	resolved, err := resolveFUSEWhitelist([]string{"203.0.113.7", "198.51.100.0/24", "203.0.113.7"})
	require.NoError(t, err)
	assert.Equal(t, []string{"198.51.100.0/24", "203.0.113.7/32"}, resolved)

	command, err := buildFUSEGatewayUserIptablesCmd(true, resolved, false)
	require.NoError(t, err)
	assert.Contains(t, command, "-A SBOX_USER -d 203.0.113.7/32 -j ACCEPT")
}
