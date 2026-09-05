package docker

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

func TestWorkspaceFUSEPrepareRequiresSpec(t *testing.T) {
	rt := &Runtime{}
	ctx := context.Background()

	_, err := rt.PrepareSandbox(ctx, runtime.SandboxSpec{})
	require.ErrorIs(t, err, runtime.ErrWorkspaceFUSEUnsupported)
}

func TestCreateContainerConfigUsesConfiguredTmpDiskLimit(t *testing.T) {
	_, hostConfig, err := createContainerConfig(runtime.SandboxSpec{
		ID:      "sandbox-test",
		Image:   "sandbox:latest",
		TmpDisk: "200Mi",
	})

	require.NoError(t, err)
	assert.Equal(t, "size=209715200", hostConfig.Tmpfs["/tmp"])
}

func TestCreateContainerConfigDefaultsTmpDiskLimitTo50Mi(t *testing.T) {
	_, hostConfig, err := createContainerConfig(runtime.SandboxSpec{
		ID:    "sandbox-test",
		Image: "sandbox:latest",
	})

	require.NoError(t, err)
	assert.Equal(t, "size=52428800", hostConfig.Tmpfs["/tmp"])
}

func TestCreateContainerConfigForFUSE(t *testing.T) {
	spec := fuseDockerSpecForTest()
	cfg, host, err := createContainerConfig(spec)
	require.NoError(t, err)

	assert.Equal(t, spec.WorkspaceFUSE.DockerImage, cfg.Image)
	assert.Empty(t, cfg.Cmd, "the trusted image entrypoint must remain PID 1")
	assert.Equal(t, []string{"ALL"}, []string(host.CapDrop))
	assert.Equal(t, []string{"SYS_ADMIN", "NET_ADMIN"}, []string(host.CapAdd))
	require.Len(t, host.Devices, 1)
	assert.Equal(t, "/dev/fuse", host.Devices[0].PathOnHost)
	assert.Equal(t, "/dev/fuse", host.Devices[0].PathInContainer)
	assert.Equal(t, "rwm", host.Devices[0].CgroupPermissions)
	assert.True(t, host.ReadonlyRootfs)
	assert.Contains(t, host.SecurityOpt, "no-new-privileges=true")
	assert.Contains(t, host.SecurityOpt, "apparmor=sandbox-fuse")
	assert.NotContains(t, host.SecurityOpt, "unconfined")
	assert.NotContains(t, strings.Join(host.Binds, " "), ":/workspace")
	assert.Contains(t, strings.Join(host.Binds, " "), ":/run/secrets/workspace:ro")
	assert.Contains(t, strings.Join(host.Binds, " "), "/var/lib/sandbox/fuse-secrets/"+spec.ID+":")
	require.Len(t, host.Mounts, 1)
	assert.Equal(t, "/var/cache/s3fs", host.Mounts[0].Target)
	assert.Contains(t, host.Mounts[0].Source, spec.ID)
	_, wholeRunTmpfs := host.Tmpfs["/run"]
	assert.False(t, wholeRunTmpfs, "mounting tmpfs on /run hides the trusted image directory /run/s3fs")
	assert.Equal(t, "size=16777216,mode=0700", host.Tmpfs["/run/s3fs"])
	assert.Equal(t, []string{"1.1.1.1"}, []string(host.DNS))
	assert.Equal(t, []string{"objects.example.com:198.51.100.10"}, []string(host.ExtraHosts))
}

func TestCreateContainerConfigRejectsEndpointHostIPOutsideSystemEgress(t *testing.T) {
	spec := fuseDockerSpecForTest()
	spec.WorkspaceFUSE.EndpointHostIPs = []string{"203.0.113.10"}
	_, _, err := createContainerConfig(spec)
	require.Error(t, err)
}

func TestCreateContainerConfigRejectsLiteralEndpointOutsideSystemEgress(t *testing.T) {
	spec := fuseDockerSpecForTest()
	spec.WorkspaceFUSE.Endpoint = "203.0.113.10:9000"
	spec.WorkspaceFUSE.EndpointHostIPs = nil

	_, _, err := createContainerConfig(spec)
	require.Error(t, err)
}

func TestCreateContainerConfigRejectsUnsafeFUSESecurity(t *testing.T) {
	for _, profile := range []string{"", "unconfined", "apparmor=", "apparmor=unconfined", "label=", "label=disable", "label=type:", "label=type:spc_t", "label=user:system_u", " sandbox-fuse", "../sandbox-fuse"} {
		t.Run(profile, func(t *testing.T) {
			spec := fuseDockerSpecForTest()
			spec.WorkspaceFUSE.LSMProfile = profile
			_, _, err := createContainerConfig(spec)
			require.Error(t, err)
		})
	}
}

func TestCanonicalDockerFUSEEndpointDerivesMinIOHTTPSURL(t *testing.T) {
	spec := fuseDockerSpecForTest().WorkspaceFUSE
	endpoint, hostname, port, err := canonicalDockerFUSEEndpoint(spec)
	require.NoError(t, err)
	assert.Equal(t, "https://objects.example.com:9000", endpoint)
	assert.Equal(t, "objects.example.com", hostname)
	assert.Equal(t, int32(9000), port)
}

func TestCanonicalDockerFUSEEndpointRejectsUnsafeOrUnapprovedEndpoints(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		endpoint string
		useSSL   bool
		ports    []int32
	}{
		{name: "minio scheme supplied", provider: "minio", endpoint: "https://objects.example.com:9000", useSSL: true, ports: []int32{9000}},
		{name: "minio without TLS", provider: "minio", endpoint: "objects.example.com:9000", useSSL: false, ports: []int32{9000}},
		{name: "minio endpoint port not approved", provider: "minio", endpoint: "objects.example.com:9000", useSSL: true, ports: []int32{443}},
		{name: "minio non-canonical port", provider: "minio", endpoint: "objects.example.com:09000", useSSL: true, ports: []int32{9000}},
		{name: "OBS without HTTPS", provider: "obs", endpoint: "http://obs.example.com", useSSL: true, ports: []int32{80}},
		{name: "OBS scheme mismatch", provider: "obs", endpoint: "https://obs.example.com", useSSL: false, ports: []int32{443}},
		{name: "OBS path", provider: "obs", endpoint: "https://obs.example.com/path", useSSL: true, ports: []int32{443}},
		{name: "unknown provider", provider: "other", endpoint: "https://objects.example.com", useSSL: true, ports: []int32{443}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := fuseDockerSpecForTest().WorkspaceFUSE
			spec.Provider, spec.Endpoint, spec.UseSSL, spec.SystemEgress.EndpointPorts = test.provider, test.endpoint, test.useSSL, test.ports
			_, _, _, err := canonicalDockerFUSEEndpoint(spec)
			require.Error(t, err)
		})
	}
}

func TestCreateContainerConfigRejectsIPv6FUSEInputs(t *testing.T) {
	mutations := []func(*runtime.WorkspaceFUSESpec){
		func(fuse *runtime.WorkspaceFUSESpec) {
			fuse.SystemEgress.DNSCIDRs = []string{"2001:4860:4860::8888/128"}
		},
		func(fuse *runtime.WorkspaceFUSESpec) { fuse.SystemEgress.EndpointCIDRs = []string{"2001:db8::10/128"} },
		func(fuse *runtime.WorkspaceFUSESpec) { fuse.EndpointHostIPs = []string{"2001:db8::10"} },
		func(fuse *runtime.WorkspaceFUSESpec) {
			fuse.Endpoint = "[2001:db8::10]:9000"
			fuse.SystemEgress.EndpointCIDRs = []string{"2001:db8::10/128"}
		},
	}
	for index, mutate := range mutations {
		t.Run(fmt.Sprintf("case-%d", index), func(t *testing.T) {
			spec := fuseDockerSpecForTest()
			mutate(spec.WorkspaceFUSE)
			_, _, err := createContainerConfig(spec)
			require.Error(t, err)
		})
	}
}

func fuseDockerSpecForTest() runtime.SandboxSpec {
	return runtime.SandboxSpec{
		ID:       "sandbox-fuse-pool-test",
		Image:    "ordinary@sha256:" + strings.Repeat("1", 64),
		Memory:   "512Mi",
		TmpDisk:  "64Mi",
		PidLimit: 128,
		WorkspaceFUSE: &runtime.WorkspaceFUSESpec{
			RuntimeType: "docker", Provider: "minio", Driver: "s3fs",
			Profile: "minio-sigv4-path-style-v1", StorageIdentity: "minio-primary",
			CredentialGeneration: "generation-1",
			DockerImage:          "sandbox-fuse@sha256:" + strings.Repeat("2", 64),
			SecretName:           "workspace-storage", Bucket: "workspace", Endpoint: "objects.example.com:9000",
			EndpointHostIPs: []string{"198.51.100.10"},
			Region:          "us-east-1", UseSSL: true, CacheSize: "2Gi", CacheMedium: "disk",
			MountTimeout: 30 * time.Second, FlushTimeout: 30 * time.Second, UnmountTimeout: 15 * time.Second,
			LSMProfile: "sandbox-fuse", PoolKey: strings.Repeat("a", 64),
			SystemEgress: runtime.SystemEgressSpec{Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"1.1.1.1/32"}, DNSPorts: []int32{53}, EndpointCIDRs: []string{"198.51.100.10/32"}, EndpointPorts: []int32{9000}},
		},
	}
}
