package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

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
