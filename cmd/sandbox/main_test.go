package main

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/runtime"
)

func TestBuildFUSESpecUsesOnlyFixedProviderConfiguration(t *testing.T) {
	cfg := &config.Config{
		Runtime: config.RuntimeConfig{Type: "docker"},
		Storage: config.StorageConfig{FileSystem: config.FileSystemConfig{
			Provider: "minio", Bucket: "bucket", Endpoint: "minio.example:9000", Region: "us-east-1", SubPath: "workspaces", UseSSL: true,
		}},
		Workspace: config.WorkspaceConfig{
			SecretName: "runtime-secret", CacheSize: "2Gi", CacheMedium: "disk",
			MountTimeoutSeconds: 30, FlushTimeoutSeconds: 60, UnmountTimeoutSeconds: 15,
			MounterResources: config.WorkspaceFUSEResourceConfig{CPURequest: "50m", CPULimit: "1", MemoryRequest: "64Mi", MemoryLimit: "512Mi", EphemeralStorageRequest: "512Mi", EphemeralStorageLimit: "3Gi"},
			Backend: config.WorkspaceBackendConfig{
				Preset: "minio",
				Driver: "s3fs", Profile: "minio-sigv4-path-style-v1", StorageIdentity: "physical-a",
				MounterImage: "mounter@sha256:" + strings.Repeat("a", 64), DockerImage: "sandbox@sha256:" + strings.Repeat("b", 64),
				CredentialGeneration: "rotation-1", LSMProfile: "sandbox-fuse", SystemEgressMode: "cidr",
				DNSCIDRs: []string{"1.1.1.1/32"}, SystemEgressCIDRs: []string{"192.0.2.10/32"}, EndpointPorts: []int32{9000},
			},
		},
		Security: config.SecurityConfig{MaxMemory: "512Mi", MaxMemoryRequest: "128Mi", MaxCPU: "500m", MaxCPURequest: "100m", MaxDisk: "1Gi", MaxTmpDisk: "64Mi", MaxPids: 100, SeccompProfile: "RuntimeDefault"},
	}

	spec, err := buildFUSESpec(cfg, "ordinary@sha256:"+strings.Repeat("c", 64))
	require.NoError(t, err)
	require.NotNil(t, spec.WorkspaceFUSE)
	assert.Equal(t, cfg.Workspace.Backend.DockerImage, spec.Image)
	assert.NotEmpty(t, spec.WorkspaceFUSE.PoolKey)
	assert.Empty(t, spec.WorkspaceFUSE.SystemEgress.DNSCIDRs)
	assert.Empty(t, spec.WorkspaceFUSE.SystemEgress.DNSPorts)
	assert.Empty(t, spec.WorkspaceFUSE.SystemEgress.EndpointCIDRs)
	assert.Equal(t, 30*time.Second, spec.WorkspaceFUSE.MountTimeout)
	assert.NotContains(t, spec.WorkspaceFUSE.PoolKey, "workspaces")

	cfg.Runtime.Type = "kubernetes"
	spec, err = buildFUSESpec(cfg, "ordinary@sha256:"+strings.Repeat("c", 64))
	require.NoError(t, err)
	assert.Equal(t, "ordinary@sha256:"+strings.Repeat("c", 64), spec.Image)
}

func TestLoadRuntimeFUSECredentialsUsesInlineConfigOnlyForFUSE(t *testing.T) {
	cfg := &config.Config{
		Storage:   config.StorageConfig{FileSystem: config.FileSystemConfig{AccessKey: "inline-access", SecretKey: "inline-secret"}},
		Workspace: config.WorkspaceConfig{DefaultMountMode: "fuse", EnabledMountModes: []string{"fuse"}},
	}
	credentials, err := loadRuntimeFUSECredentials(cfg)
	require.NoError(t, err)
	assert.Equal(t, runtime.FUSECredentials{AccessKey: []byte("inline-access"), SecretKey: []byte("inline-secret")}, credentials)
	credentials.Zero()

	cfg.Workspace.DefaultMountMode = "sync"
	cfg.Workspace.EnabledMountModes = []string{"sync"}
	credentials, err = loadRuntimeFUSECredentials(cfg)
	require.NoError(t, err)
	assert.Empty(t, credentials.AccessKey)
	assert.Empty(t, credentials.SecretKey)
}

func TestOwnershipTokensAreOpaqueAndUnique(t *testing.T) {
	a, err := newOwnershipToken()
	require.NoError(t, err)
	b, err := newOwnershipToken()
	require.NoError(t, err)
	assert.Len(t, a, 64)
	assert.NotEqual(t, a, b)
}
