package runtime

import (
	"context"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveFUSEEndpointPolicy(t *testing.T) {
	lookup := lookupFixture(map[string][]string{
		"minio.example.com":                             {"36.170.50.44", "36.170.50.43"},
		"obs.cn-southwest-2.myhuaweicloud.com":          {"203.0.113.10"},
		"sandbox.obs.cn-southwest-2.myhuaweicloud.com":  {"203.0.113.11"},
		"obs.cn-southwest-268.shuanghuayun.com":         {"10.20.30.41"},
		"sandbox.obs.cn-southwest-268.shuanghuayun.com": {"10.20.30.42"},
	})
	tests := []struct {
		name      string
		spec      WorkspaceFUSESpec
		wantHosts []EndpointHostMapping
		wantCIDRs []string
		wantPorts []int32
	}{
		{
			name:      "public TLS MinIO",
			spec:      WorkspaceFUSESpec{Provider: "minio", Bucket: "sandbox", Endpoint: "minio.example.com", UseSSL: true},
			wantHosts: []EndpointHostMapping{{Host: "minio.example.com", IPs: []string{"36.170.50.43", "36.170.50.44"}}},
			wantCIDRs: []string{"36.170.50.43/32", "36.170.50.44/32"}, wantPorts: []int32{443},
		},
		{
			name:      "private HTTP MinIO explicit port",
			spec:      WorkspaceFUSESpec{Provider: "minio", Profile: "minio-sigv4-path-style-private-http-v1", Bucket: "sandbox", Endpoint: "10.20.30.40:9000", UseSSL: false},
			wantHosts: []EndpointHostMapping{{Host: "10.20.30.40", IPs: []string{"10.20.30.40"}}},
			wantCIDRs: []string{"10.20.30.40/32"}, wantPorts: []int32{9000},
		},
		{
			name: "Huawei public OBS virtual host",
			spec: WorkspaceFUSESpec{Provider: "obs", Bucket: "sandbox", Endpoint: "https://obs.cn-southwest-2.myhuaweicloud.com", UseSSL: true},
			wantHosts: []EndpointHostMapping{
				{Host: "obs.cn-southwest-2.myhuaweicloud.com", IPs: []string{"203.0.113.10"}},
				{Host: "sandbox.obs.cn-southwest-2.myhuaweicloud.com", IPs: []string{"203.0.113.11"}},
			},
			wantCIDRs: []string{"203.0.113.10/32", "203.0.113.11/32"}, wantPorts: []int32{443},
		},
		{
			name: "Huawei private OBS virtual host",
			spec: WorkspaceFUSESpec{Provider: "obs", Bucket: "sandbox", Endpoint: "https://obs.cn-southwest-268.shuanghuayun.com", UseSSL: true},
			wantHosts: []EndpointHostMapping{
				{Host: "obs.cn-southwest-268.shuanghuayun.com", IPs: []string{"10.20.30.41"}},
				{Host: "sandbox.obs.cn-southwest-268.shuanghuayun.com", IPs: []string{"10.20.30.42"}},
			},
			wantCIDRs: []string{"10.20.30.41/32", "10.20.30.42/32"}, wantPorts: []int32{443},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := ResolveFUSEEndpointPolicy(context.Background(), &tt.spec, lookup, EndpointIPv4Only)
			require.NoError(t, err)
			assert.Equal(t, tt.wantHosts, resolved.SystemEgress.Hosts)
			assert.Equal(t, tt.wantCIDRs, resolved.SystemEgress.EndpointCIDRs)
			assert.Equal(t, tt.wantPorts, resolved.SystemEgress.EndpointPorts)
		})
	}
}

func TestResolveFUSEEndpointPolicyRejectsUnsafeAddresses(t *testing.T) {
	tests := []struct {
		name string
		ip   string
	}{
		{name: "unspecified", ip: "0.0.0.0"},
		{name: "loopback", ip: "127.0.0.1"},
		{name: "link local metadata", ip: "169.254.169.254"},
		{name: "multicast", ip: "224.0.0.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := &WorkspaceFUSESpec{Provider: "minio", Bucket: "sandbox", Endpoint: "minio.example.com", UseSSL: true}
			_, err := ResolveFUSEEndpointPolicy(context.Background(), spec, lookupFixture(map[string][]string{"minio.example.com": {tt.ip}}), EndpointIPv4Only)
			require.ErrorContains(t, err, "forbidden address")
		})
	}
}

func TestResolveFUSEEndpointPolicyRejectsPublicHTTPMinIO(t *testing.T) {
	spec := &WorkspaceFUSESpec{Provider: "minio", Profile: "minio-sigv4-path-style-private-http-v1", Bucket: "sandbox", Endpoint: "minio.example.com", UseSSL: false}
	_, err := ResolveFUSEEndpointPolicy(context.Background(), spec, lookupFixture(map[string][]string{"minio.example.com": {"36.170.50.43"}}), EndpointIPv4Only)
	require.ErrorContains(t, err, "HTTP MinIO endpoint must resolve only to private addresses")
}

func TestResolveFUSEEndpointPolicyRequiresUsableAddressFamily(t *testing.T) {
	spec := &WorkspaceFUSESpec{Provider: "minio", Bucket: "sandbox", Endpoint: "minio.example.com", UseSSL: true}
	_, err := ResolveFUSEEndpointPolicy(context.Background(), spec, lookupFixture(map[string][]string{"minio.example.com": {"2001:4860:4860::8888"}}), EndpointIPv4Only)
	require.ErrorContains(t, err, "no usable IPv4 address")
}

func TestResolveFUSEEndpointPolicyAllowsCanonicalDottedOBSBucket(t *testing.T) {
	spec := &WorkspaceFUSESpec{Provider: "obs", Bucket: "team.workspace", Endpoint: "https://obs.cn-southwest-2.myhuaweicloud.com", UseSSL: true}
	lookup := lookupFixture(map[string][]string{
		"obs.cn-southwest-2.myhuaweicloud.com":                {"203.0.113.10"},
		"team.workspace.obs.cn-southwest-2.myhuaweicloud.com": {"203.0.113.11"},
	})
	resolved, err := ResolveFUSEEndpointPolicy(context.Background(), spec, lookup, EndpointIPv4Only)
	require.NoError(t, err)
	assert.Len(t, resolved.SystemEgress.Hosts, 2)
}

func TestFUSEEndpointMappingsCurrentDetectsDNSRotation(t *testing.T) {
	mappings := []EndpointHostMapping{{Host: "minio.example.com", IPs: []string{"36.170.50.43"}}}
	current, err := FUSEEndpointMappingsCurrent(context.Background(), mappings, lookupFixture(map[string][]string{"minio.example.com": {"36.170.50.43"}}), EndpointIPv4Only)
	require.NoError(t, err)
	assert.True(t, current)

	current, err = FUSEEndpointMappingsCurrent(context.Background(), mappings, lookupFixture(map[string][]string{"minio.example.com": {"36.170.50.44"}}), EndpointIPv4Only)
	require.NoError(t, err)
	assert.False(t, current)
}

func lookupFixture(values map[string][]string) LookupNetIPFunc {
	return func(_ context.Context, network, host string) ([]net.IP, error) {
		if network != "ip" {
			return nil, assert.AnError
		}
		raw := values[host]
		result := make([]net.IP, 0, len(raw))
		for _, value := range raw {
			result = append(result, net.ParseIP(value))
		}
		return result, nil
	}
}
