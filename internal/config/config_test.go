package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	// api_key is required by validation, set via env
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-api-key")

	// No file, no other env vars — should return all defaults
	cfg, err := config.Load("")
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Server defaults
	assert.Equal(t, 8080, cfg.Server.Port)
	assert.Equal(t, "0.0.0.0", cfg.Server.Host)

	// Runtime defaults
	assert.Equal(t, "docker", cfg.Runtime.Type)
	assert.Equal(t, "", cfg.Runtime.Docker.Host)
	assert.Equal(t, "", cfg.Runtime.Kubernetes.Kubeconfig)
	assert.Equal(t, "", cfg.Runtime.Kubernetes.Namespace)

	// Pool defaults
	assert.Equal(t, 3, cfg.Pool.MinSize)
	assert.Equal(t, 20, cfg.Pool.MaxSize)
	assert.Equal(t, 10, cfg.Pool.RefillIntervalSeconds)

	// Storage.State.Redis defaults
	assert.Equal(t, "localhost:6379", cfg.Storage.State.Redis.Addr)
	assert.Equal(t, "", cfg.Storage.State.Redis.Password)
	assert.Equal(t, 0, cfg.Storage.State.Redis.DB)

	// Storage.FileSystem defaults
	assert.Equal(t, "local", cfg.Storage.FileSystem.Provider)
	assert.Equal(t, "", cfg.Storage.FileSystem.Bucket)
	assert.Equal(t, "", cfg.Storage.FileSystem.Region)
	assert.Equal(t, "", cfg.Storage.FileSystem.Endpoint)
	assert.Equal(t, "", cfg.Storage.FileSystem.AccessKey)
	assert.Equal(t, "", cfg.Storage.FileSystem.SecretKey)
	assert.Equal(t, "/tmp/sandbox-storage", cfg.Storage.FileSystem.LocalPath)
	assert.Equal(t, "", cfg.Storage.FileSystem.SubPath)
	assert.False(t, cfg.Storage.FileSystem.UseSSL)

	// Security defaults
	assert.Equal(t, 30, cfg.Security.ExecTimeoutSeconds)
	assert.Equal(t, 600, cfg.Security.MaxExecTimeoutSeconds)
	assert.Equal(t, "256Mi", cfg.Security.MaxMemory)
	assert.Equal(t, "100Mi", cfg.Security.MaxDisk)
	assert.Equal(t, "50Mi", cfg.Security.MaxTmpDisk)
	assert.Equal(t, 100, cfg.Security.MaxPids)
	assert.Equal(t, false, cfg.Security.NetworkEnabled)
	assert.Empty(t, cfg.Security.NetworkWhitelist)
	assert.Equal(t, "", cfg.Security.SeccompProfile)
}

func TestLoadFromYAML(t *testing.T) {
	// Write a temp YAML config file
	content := `
server:
  port: 9090
  host: "127.0.0.1"

runtime:
  type: "kubernetes"
  docker:
    host: "unix:///var/run/docker.sock"
  kubernetes:
    kubeconfig: "/home/user/.kube/config"
    namespace: "sandbox-ns"

pool:
  min_size: 5
  max_size: 50
  refill_interval_seconds: 15

storage:
  state:
    redis:
      addr: "redis.example.com:6379"
      password: "secret"
      db: 1
  filesystem:
    provider: "s3"
    bucket: "my-bucket"
    region: "us-east-1"
    endpoint: "https://s3.amazonaws.com"
    access_key: "AKIAIOSFODNN7EXAMPLE"
    secret_key: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
    local_path: "/data/sandbox"
    sub_path: "workspaces"
    use_ssl: true

security:
  api_key: "test-yaml-api-key"
  exec_timeout_seconds: 60
  max_exec_timeout_seconds: 900
  max_memory: "512Mi"
  max_disk: "200Mi"
  max_tmp_disk: "300Mi"
  max_pids: 200
  network_enabled: true
  network_whitelist:
    - "10.0.0.0/8"
    - "192.168.0.0/16"
  seccomp_profile: "/etc/seccomp/sandbox.json"
`
	tmpDir := t.TempDir()
	cfgFile := filepath.Join(tmpDir, "config.yaml")
	err := os.WriteFile(cfgFile, []byte(content), 0644)
	require.NoError(t, err)

	cfg, err := config.Load(cfgFile)
	require.NoError(t, err)
	require.NotNil(t, cfg)

	// Server
	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, "127.0.0.1", cfg.Server.Host)

	// Runtime
	assert.Equal(t, "kubernetes", cfg.Runtime.Type)
	assert.Equal(t, "unix:///var/run/docker.sock", cfg.Runtime.Docker.Host)
	assert.Equal(t, "/home/user/.kube/config", cfg.Runtime.Kubernetes.Kubeconfig)
	assert.Equal(t, "sandbox-ns", cfg.Runtime.Kubernetes.Namespace)

	// Pool
	assert.Equal(t, 5, cfg.Pool.MinSize)
	assert.Equal(t, 50, cfg.Pool.MaxSize)
	assert.Equal(t, 15, cfg.Pool.RefillIntervalSeconds)

	// Storage.State.Redis
	assert.Equal(t, "redis.example.com:6379", cfg.Storage.State.Redis.Addr)
	assert.Equal(t, "secret", cfg.Storage.State.Redis.Password)
	assert.Equal(t, 1, cfg.Storage.State.Redis.DB)

	// Storage.FileSystem
	assert.Equal(t, "s3", cfg.Storage.FileSystem.Provider)
	assert.Equal(t, "my-bucket", cfg.Storage.FileSystem.Bucket)
	assert.Equal(t, "us-east-1", cfg.Storage.FileSystem.Region)
	assert.Equal(t, "https://s3.amazonaws.com", cfg.Storage.FileSystem.Endpoint)
	assert.Equal(t, "AKIAIOSFODNN7EXAMPLE", cfg.Storage.FileSystem.AccessKey)
	assert.Equal(t, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", cfg.Storage.FileSystem.SecretKey)
	assert.Equal(t, "/data/sandbox", cfg.Storage.FileSystem.LocalPath)
	assert.Equal(t, "workspaces", cfg.Storage.FileSystem.SubPath)
	assert.True(t, cfg.Storage.FileSystem.UseSSL)

	// Security
	assert.Equal(t, 60, cfg.Security.ExecTimeoutSeconds)
	assert.Equal(t, 900, cfg.Security.MaxExecTimeoutSeconds)
	assert.Equal(t, "512Mi", cfg.Security.MaxMemory)
	assert.Equal(t, "200Mi", cfg.Security.MaxDisk)
	assert.Equal(t, "300Mi", cfg.Security.MaxTmpDisk)
	assert.Equal(t, 200, cfg.Security.MaxPids)
	assert.Equal(t, true, cfg.Security.NetworkEnabled)
	assert.Equal(t, []string{"10.0.0.0/8", "192.168.0.0/16"}, cfg.Security.NetworkWhitelist)
	assert.Equal(t, "/etc/seccomp/sandbox.json", cfg.Security.SeccompProfile)
}

func TestEnvOverrides(t *testing.T) {
	// Set env vars before loading; clean up after
	envVars := map[string]string{
		"SANDBOX_SERVER_PORT":                       "7070",
		"SANDBOX_SERVER_HOST":                       "localhost",
		"SANDBOX_RUNTIME_TYPE":                      "kubernetes",
		"SANDBOX_RUNTIME_DOCKER_HOST":               "tcp://docker.example.com:2376",
		"SANDBOX_RUNTIME_KUBERNETES_KUBECONFIG":     "/root/.kube/config",
		"SANDBOX_RUNTIME_KUBERNETES_NAMESPACE":      "production",
		"SANDBOX_POOL_MIN_SIZE":                     "10",
		"SANDBOX_POOL_MAX_SIZE":                     "100",
		"SANDBOX_POOL_REFILL_INTERVAL_SECONDS":      "30",
		"SANDBOX_STORAGE_STATE_REDIS_ADDR":          "cache.example.com:6379",
		"SANDBOX_STORAGE_STATE_REDIS_PASSWORD":      "redispass",
		"SANDBOX_STORAGE_STATE_REDIS_DB":            "2",
		"SANDBOX_STORAGE_FILESYSTEM_PROVIDER":       "cos",
		"SANDBOX_STORAGE_FILESYSTEM_BUCKET":         "env-bucket",
		"SANDBOX_STORAGE_FILESYSTEM_REGION":         "ap-guangzhou",
		"SANDBOX_STORAGE_FILESYSTEM_ENDPOINT":       "https://cos.ap-guangzhou.myqcloud.com",
		"SANDBOX_STORAGE_FILESYSTEM_ACCESS_KEY":     "env-access-key",
		"SANDBOX_STORAGE_FILESYSTEM_SECRET_KEY":     "env-secret-key",
		"SANDBOX_STORAGE_FILESYSTEM_LOCAL_PATH":     "/env/sandbox-storage",
		"SANDBOX_SECURITY_EXEC_TIMEOUT_SECONDS":     "120",
		"SANDBOX_SECURITY_MAX_EXEC_TIMEOUT_SECONDS": "900",
		"SANDBOX_SECURITY_API_KEY":                  "env-api-key",
		"SANDBOX_SECURITY_MAX_MEMORY":               "1Gi",
		"SANDBOX_SECURITY_MAX_DISK":                 "500Mi",
		"SANDBOX_SECURITY_MAX_TMP_DISK":             "600Mi",
		"SANDBOX_SECURITY_MAX_PIDS":                 "500",
		"SANDBOX_SECURITY_NETWORK_ENABLED":          "true",
		"SANDBOX_SECURITY_SECCOMP_PROFILE":          "/etc/seccomp/custom.json",
	}

	for k, v := range envVars {
		t.Setenv(k, v)
	}

	cfg, err := config.Load("")
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, 7070, cfg.Server.Port)
	assert.Equal(t, "localhost", cfg.Server.Host)

	assert.Equal(t, "kubernetes", cfg.Runtime.Type)
	assert.Equal(t, "tcp://docker.example.com:2376", cfg.Runtime.Docker.Host)
	assert.Equal(t, "/root/.kube/config", cfg.Runtime.Kubernetes.Kubeconfig)
	assert.Equal(t, "production", cfg.Runtime.Kubernetes.Namespace)

	assert.Equal(t, 10, cfg.Pool.MinSize)
	assert.Equal(t, 100, cfg.Pool.MaxSize)
	assert.Equal(t, 30, cfg.Pool.RefillIntervalSeconds)

	assert.Equal(t, "cache.example.com:6379", cfg.Storage.State.Redis.Addr)
	assert.Equal(t, "redispass", cfg.Storage.State.Redis.Password)
	assert.Equal(t, 2, cfg.Storage.State.Redis.DB)

	assert.Equal(t, "cos", cfg.Storage.FileSystem.Provider)
	assert.Equal(t, "env-bucket", cfg.Storage.FileSystem.Bucket)
	assert.Equal(t, "ap-guangzhou", cfg.Storage.FileSystem.Region)
	assert.Equal(t, "https://cos.ap-guangzhou.myqcloud.com", cfg.Storage.FileSystem.Endpoint)
	assert.Equal(t, "env-access-key", cfg.Storage.FileSystem.AccessKey)
	assert.Equal(t, "env-secret-key", cfg.Storage.FileSystem.SecretKey)
	assert.Equal(t, "/env/sandbox-storage", cfg.Storage.FileSystem.LocalPath)

	assert.Equal(t, 120, cfg.Security.ExecTimeoutSeconds)
	assert.Equal(t, 900, cfg.Security.MaxExecTimeoutSeconds)
	assert.Equal(t, "1Gi", cfg.Security.MaxMemory)
	assert.Equal(t, "500Mi", cfg.Security.MaxDisk)
	assert.Equal(t, "600Mi", cfg.Security.MaxTmpDisk)
	assert.Equal(t, 500, cfg.Security.MaxPids)
	assert.Equal(t, true, cfg.Security.NetworkEnabled)
	assert.Equal(t, "/etc/seccomp/custom.json", cfg.Security.SeccompProfile)
}

func TestLoadNonExistentFile(t *testing.T) {
	_, err := config.Load("/nonexistent/path/config.yaml")
	assert.Error(t, err)
}

func TestValidateEmptyAPIKey(t *testing.T) {
	// Without api_key env, Load should fail validation
	_, err := config.Load("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "api_key")
}

func TestValidateInvalidPoolSize(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")
	t.Setenv("SANDBOX_POOL_MAX_SIZE", "0")
	_, err := config.Load("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "pool.max_size")
}

func TestValidateInvalidPort(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")
	t.Setenv("SANDBOX_SERVER_PORT", "0")
	_, err := config.Load("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "server.port")
}

func TestValidateKubernetesNamespaceRequired(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")
	t.Setenv("SANDBOX_RUNTIME_TYPE", "kubernetes")
	// namespace is empty by default
	_, err := config.Load("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "namespace")
}

func TestValidateMaxExecTimeoutLessThanDefault(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")
	t.Setenv("SANDBOX_SECURITY_EXEC_TIMEOUT_SECONDS", "60")
	t.Setenv("SANDBOX_SECURITY_MAX_EXEC_TIMEOUT_SECONDS", "30")

	_, err := config.Load("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "max_exec_timeout_seconds")
}

func TestValidateMaxExecTimeoutMustBePositive(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")
	t.Setenv("SANDBOX_SECURITY_MAX_EXEC_TIMEOUT_SECONDS", "0")

	_, err := config.Load("")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "max_exec_timeout_seconds")
}

func TestLoad_FileSystemConfig_Defaults(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")

	cfg, err := config.Load("")
	require.NoError(t, err)

	assert.Equal(t, "local", cfg.Storage.FileSystem.Provider)
	assert.Equal(t, "/tmp/sandbox-storage", cfg.Storage.FileSystem.LocalPath)
	assert.Equal(t, "", cfg.Storage.FileSystem.SubPath)
	assert.False(t, cfg.Storage.FileSystem.UseSSL)
}

func TestLoad_FileSystemConfig_EnvOverride(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")
	t.Setenv("SANDBOX_STORAGE_FILESYSTEM_PROVIDER", "s3")
	t.Setenv("SANDBOX_STORAGE_FILESYSTEM_BUCKET", "my-bucket")
	t.Setenv("SANDBOX_STORAGE_FILESYSTEM_REGION", "us-east-1")
	t.Setenv("SANDBOX_STORAGE_FILESYSTEM_SUB_PATH", "workspaces")

	cfg, err := config.Load("")
	require.NoError(t, err)

	assert.Equal(t, "s3", cfg.Storage.FileSystem.Provider)
	assert.Equal(t, "my-bucket", cfg.Storage.FileSystem.Bucket)
	assert.Equal(t, "us-east-1", cfg.Storage.FileSystem.Region)
	assert.Equal(t, "workspaces", cfg.Storage.FileSystem.SubPath)
}

func minimalValidConfig() *config.Config {
	return &config.Config{
		Server:  config.ServerConfig{Port: 8080},
		Runtime: config.RuntimeConfig{Type: "docker"},
		Pool:    config.PoolConfig{MinSize: 0, MaxSize: 1},
		Security: config.SecurityConfig{
			APIKey: "test", ExecTimeoutSeconds: 30, MaxExecTimeoutSeconds: 60,
			MaxUploadBytes: 2 << 30,
		},
		Workspace: config.WorkspaceConfig{Mode: "sync"},
	}
}

func newValidFUSEConfig() *config.Config {
	valid := minimalValidConfig()
	valid.Storage.State.Redis.Addr = "redis:6379"
	valid.Storage.FileSystem.Provider = "minio"
	valid.Storage.FileSystem.Bucket = "sandbox"
	valid.Storage.FileSystem.Endpoint = "minio.example.com:9000"
	valid.Storage.FileSystem.SubPath = "workspaces/团队"
	valid.Storage.FileSystem.CAFile = "/run/secrets/ca.crt"
	valid.Storage.FileSystem.CredentialFiles = config.FileSystemCredentialFileConfig{
		AccessKeyFile: "/run/secrets/storage_access_key",
		SecretKeyFile: "/run/secrets/storage_secret_key",
	}
	valid.Workspace = config.WorkspaceConfig{
		Mode:                      "fuse",
		SecretName:                "sandbox-workspace-minio",
		CacheSize:                 "2Gi",
		CacheMedium:               "disk",
		MountTimeoutSeconds:       30,
		FlushTimeoutSeconds:       30,
		UnmountTimeoutSeconds:     15,
		RecreateMaxAttempts:       1,
		LeaseTTLSeconds:           120,
		LeaseRenewIntervalSeconds: 30,
		QuotaMode:                 "soft",
		MounterResources: config.WorkspaceFUSEResourceConfig{
			CPURequest: "50m", CPULimit: "1", MemoryRequest: "64Mi", MemoryLimit: "512Mi",
			EphemeralStorageRequest: "512Mi", EphemeralStorageLimit: "3Gi",
		},
		FUSEPool: config.WorkspaceFUSEPoolConfig{
			MinSize: 3, MaxSize: 20, RefillIntervalSeconds: 10, PrepareTimeoutSeconds: 120,
		},
		Providers: map[string]config.WorkspaceFUSEProviderConfig{
			"minio": validFUSEProvider("minio.example.com", "cidr"),
		},
	}
	return valid
}

func validFUSEProvider(endpointFQDN, egressMode string) config.WorkspaceFUSEProviderConfig {
	return config.WorkspaceFUSEProviderConfig{
		Driver:               "s3fs",
		Profile:              "s3fs-compatible-v1",
		StorageIdentity:      "primary-object-store",
		MounterImage:         "registry.example.com/mounter@sha256:" + strings.Repeat("a", 64),
		DockerImage:          "registry.example.com/sandbox-fuse@sha256:" + strings.Repeat("b", 64),
		CASecretKey:          "ca.crt",
		CredentialGeneration: "2026-09-03-01",
		EndpointHostIPs:      []string{"192.0.2.10"},
		LSMProfile:           "sandbox-fuse",
		SystemEgressMode:     egressMode,
		DNSCIDRs:             []string{"8.8.8.8/32", "2001:4860:4860::8888/128"},
		SystemEgressFQDNs:    []string{endpointFQDN},
		SystemEgressCIDRs:    []string{"192.0.2.10/32"},
		EndpointPorts:        []int32{443, 9000},
	}
}

func TestLoadFUSEDefaults(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")

	cfg, err := config.Load("")
	require.NoError(t, err)

	assert.Equal(t, "sync", cfg.Workspace.Mode)
	assert.Equal(t, "", cfg.Workspace.SecretName)
	assert.Equal(t, "2Gi", cfg.Workspace.CacheSize)
	assert.Equal(t, "disk", cfg.Workspace.CacheMedium)
	assert.Equal(t, 30, cfg.Workspace.MountTimeoutSeconds)
	assert.Equal(t, 30, cfg.Workspace.FlushTimeoutSeconds)
	assert.Equal(t, 15, cfg.Workspace.UnmountTimeoutSeconds)
	assert.Equal(t, 1, cfg.Workspace.RecreateMaxAttempts)
	assert.Equal(t, 120, cfg.Workspace.LeaseTTLSeconds)
	assert.Equal(t, 30, cfg.Workspace.LeaseRenewIntervalSeconds)
	assert.Equal(t, "soft", cfg.Workspace.QuotaMode)
	assert.Equal(t, config.WorkspaceFUSEPoolConfig{
		MinSize: 3, MaxSize: 20, RefillIntervalSeconds: 10, PrepareTimeoutSeconds: 120,
	}, cfg.Workspace.FUSEPool)
	assert.Equal(t, config.WorkspaceFUSEResourceConfig{
		CPURequest: "50m", CPULimit: "1", MemoryRequest: "64Mi", MemoryLimit: "512Mi",
		EphemeralStorageRequest: "512Mi", EphemeralStorageLimit: "3Gi",
	}, cfg.Workspace.MounterResources)
	assert.Empty(t, cfg.Workspace.Providers)
	assert.Equal(t, int64(2<<30), cfg.Security.MaxUploadBytes)
	assert.Equal(t, config.FileSystemCredentialFileConfig{}, cfg.Storage.FileSystem.CredentialFiles)
	assert.Empty(t, cfg.Storage.FileSystem.CAFile)
	assert.Empty(t, cfg.Storage.FileSystem.SessionToken)
	assert.Empty(t, cfg.Storage.FileSystem.CredentialExpiry)
}

const validFUSEYAML = `
server:
  port: 8080
runtime:
  type: docker
pool:
  min_size: 0
  max_size: 1
storage:
  state:
    redis:
      addr: redis.example.com:6379
  filesystem:
    provider: obs
    bucket: sandbox
    endpoint: https://obs.example.com
    sub_path: teams/project
    credential_files:
      access_key_file: /run/secrets/access-key
      secret_key_file: /run/secrets/secret-key
      session_token_file: ""
      credential_expiry_file: ""
    ca_file: /run/secrets/ca.crt
    session_token: ""
    credential_expiry: ""
workspace:
  mode: fuse
  secret_name: sandbox-obs
  cache_size: 4Gi
  cache_medium: disk
  mount_timeout_seconds: 31
  flush_timeout_seconds: 32
  unmount_timeout_seconds: 16
  recreate_max_attempts: 1
  lease_ttl_seconds: 150
  lease_renew_interval_seconds: 40
  quota_mode: soft
  mounter_resources:
    cpu_request: 75m
    cpu_limit: "2"
    memory_request: 96Mi
    memory_limit: 768Mi
    ephemeral_storage_request: 1Gi
    ephemeral_storage_limit: 5Gi
  fuse_pool:
    min_size: 2
    max_size: 9
    refill_interval_seconds: 11
    prepare_timeout_seconds: 121
  providers:
    obs:
      driver: s3fs
      profile: obs-s3-compatible-v1
      storage_identity: obs-primary
      mounter_image: registry.example.com/mounter@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      docker_image: registry.example.com/sandbox@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
      ca_secret_key: ca.crt
      credential_generation: gen-2
      endpoint_host_ips: [192.0.2.20]
      lsm_profile: sandbox-fuse
      system_egress_mode: cilium-fqdn
      dns_cidrs: [1.1.1.1/32]
      system_egress_fqdns: [obs.example.com]
      system_egress_cidrs: [192.0.2.0/24]
      endpoint_ports: [443]
      proxy_url: ""
security:
  api_key: yaml-key
  exec_timeout_seconds: 30
  max_exec_timeout_seconds: 60
  max_upload_bytes: 3221225472
`

func TestLoadFUSEConfigFromYAML(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "fuse.yaml")
	require.NoError(t, os.WriteFile(cfgFile, []byte(validFUSEYAML), 0o600))

	cfg, err := config.Load(cfgFile)
	require.NoError(t, err)

	assert.Equal(t, "fuse", cfg.Workspace.Mode)
	assert.Equal(t, "sandbox-obs", cfg.Workspace.SecretName)
	assert.Equal(t, "4Gi", cfg.Workspace.CacheSize)
	assert.Equal(t, 31, cfg.Workspace.MountTimeoutSeconds)
	assert.Equal(t, 32, cfg.Workspace.FlushTimeoutSeconds)
	assert.Equal(t, 16, cfg.Workspace.UnmountTimeoutSeconds)
	assert.Equal(t, 1, cfg.Workspace.RecreateMaxAttempts)
	assert.Equal(t, 150, cfg.Workspace.LeaseTTLSeconds)
	assert.Equal(t, 40, cfg.Workspace.LeaseRenewIntervalSeconds)
	assert.Equal(t, config.WorkspaceFUSEPoolConfig{MinSize: 2, MaxSize: 9, RefillIntervalSeconds: 11, PrepareTimeoutSeconds: 121}, cfg.Workspace.FUSEPool)
	assert.Equal(t, "75m", cfg.Workspace.MounterResources.CPURequest)
	assert.Equal(t, "5Gi", cfg.Workspace.MounterResources.EphemeralStorageLimit)
	provider := cfg.Workspace.Providers["obs"]
	assert.Equal(t, "s3fs", provider.Driver)
	assert.Equal(t, "obs-s3-compatible-v1", provider.Profile)
	assert.Equal(t, "obs-primary", provider.StorageIdentity)
	assert.Equal(t, "ca.crt", provider.CASecretKey)
	assert.Equal(t, []string{"192.0.2.20"}, provider.EndpointHostIPs)
	assert.Equal(t, []string{"1.1.1.1/32"}, provider.DNSCIDRs)
	assert.Equal(t, []string{"obs.example.com"}, provider.SystemEgressFQDNs)
	assert.Equal(t, []int32{443}, provider.EndpointPorts)
	assert.Equal(t, "/run/secrets/access-key", cfg.Storage.FileSystem.CredentialFiles.AccessKeyFile)
	assert.Equal(t, "/run/secrets/secret-key", cfg.Storage.FileSystem.CredentialFiles.SecretKeyFile)
	assert.Equal(t, "/run/secrets/ca.crt", cfg.Storage.FileSystem.CAFile)
	assert.Equal(t, int64(3221225472), cfg.Security.MaxUploadBytes)
}

func TestLoadRejectsTemporaryFUSECredentialsFromYAML(t *testing.T) {
	tests := []struct {
		name string
		old  string
		new  string
	}{
		{name: "session token", old: `    session_token: ""`, new: `    session_token: forbidden`},
		{name: "credential expiry", old: `    credential_expiry: ""`, new: `    credential_expiry: tomorrow`},
		{name: "session token file", old: `      session_token_file: ""`, new: `      session_token_file: /run/secrets/token`},
		{name: "credential expiry file", old: `      credential_expiry_file: ""`, new: `      credential_expiry_file: /run/secrets/expiry`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := strings.Replace(validFUSEYAML, tt.old, tt.new, 1)
			require.NotEqual(t, validFUSEYAML, content)
			cfgFile := filepath.Join(t.TempDir(), "fuse.yaml")
			require.NoError(t, os.WriteFile(cfgFile, []byte(content), 0o600))

			_, err := config.Load(cfgFile)
			require.ErrorContains(t, err, "session token or credential expiry")
		})
	}
}

func TestLoadRejectsInvalidFUSEResourcesFromYAML(t *testing.T) {
	tests := []struct {
		name string
		old  string
		new  string
		want string
	}{
		{name: "invalid cpu request", old: `    cpu_request: 75m`, new: `    cpu_request: invalid`, want: "mounter_resources.cpu_request"},
		{name: "cpu request exceeds limit", old: `    cpu_request: 75m`, new: `    cpu_request: "3"`, want: "cpu_request must be <="},
		{name: "memory request exceeds limit", old: `    memory_request: 96Mi`, new: `    memory_request: 1Gi`, want: "memory_request must be <="},
		{name: "ephemeral request exceeds limit", old: `    ephemeral_storage_request: 1Gi`, new: `    ephemeral_storage_request: 6Gi`, want: "ephemeral_storage_request must be <="},
		{name: "cache exceeds ephemeral limit", old: `    ephemeral_storage_limit: 5Gi`, new: `    ephemeral_storage_limit: 3Gi`, want: "ephemeral_storage_limit must be >= workspace.cache_size"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := strings.Replace(validFUSEYAML, tt.old, tt.new, 1)
			require.NotEqual(t, validFUSEYAML, content)
			cfgFile := filepath.Join(t.TempDir(), "fuse.yaml")
			require.NoError(t, os.WriteFile(cfgFile, []byte(content), 0o600))

			_, err := config.Load(cfgFile)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestLoadRejectsMismatchedFUSECAFromYAML(t *testing.T) {
	tests := []struct {
		name string
		old  string
		new  string
	}{
		{name: "only control plane CA", old: `      ca_secret_key: ca.crt`, new: `      ca_secret_key: ""`},
		{name: "only provider CA", old: `    ca_file: /run/secrets/ca.crt`, new: `    ca_file: ""`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := strings.Replace(validFUSEYAML, tt.old, tt.new, 1)
			require.NotEqual(t, validFUSEYAML, content)
			cfgFile := filepath.Join(t.TempDir(), "fuse.yaml")
			require.NoError(t, os.WriteFile(cfgFile, []byte(content), 0o600))

			_, err := config.Load(cfgFile)
			require.ErrorContains(t, err, "ca_file and ca_secret_key")
		})
	}
}

func TestRepositoryConfigDefaultsToSync(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")

	cfg, err := config.Load(filepath.Join("..", "..", "configs", "config.yaml"))
	require.NoError(t, err)
	assert.Equal(t, "sync", cfg.Workspace.Mode)
}

func TestSyncModePreservesLegacyCompatibility(t *testing.T) {
	cfg := minimalValidConfig()
	cfg.Storage.FileSystem.Provider = "local"
	cfg.Storage.FileSystem.AccessKey = "legacy-inline-access-key"
	cfg.Storage.FileSystem.SecretKey = "legacy-inline-secret-key"
	cfg.Storage.FileSystem.SessionToken = "legacy-session-token"
	cfg.Storage.FileSystem.CredentialExpiry = "soon"
	cfg.Storage.FileSystem.SubPath = "/legacy//noncanonical/"
	cfg.Workspace.FUSEPool = config.WorkspaceFUSEPoolConfig{}
	cfg.Workspace.Providers = nil

	require.NoError(t, cfg.Validate())
}

func TestWorkspaceModeValidation(t *testing.T) {
	tests := []struct {
		name string
		mode string
		want string
	}{
		{name: "empty", mode: "", want: "workspace.mode"},
		{name: "unknown", mode: "sidecar", want: "workspace.mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := minimalValidConfig()
			cfg.Workspace.Mode = tt.mode
			require.ErrorContains(t, cfg.Validate(), tt.want)
		})
	}
}

func TestMaxUploadBytesMustBePositive(t *testing.T) {
	for _, value := range []int64{0, -1} {
		t.Run(fmt.Sprintf("value_%d", value), func(t *testing.T) {
			cfg := minimalValidConfig()
			cfg.Security.MaxUploadBytes = value
			require.ErrorContains(t, cfg.Validate(), "security.max_upload_bytes")
		})
	}
}

func TestFUSEConfigValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*config.Config)
		want string
	}{
		{name: "valid minio", edit: func(*config.Config) {}, want: ""},
		{name: "valid obs cilium fqdn", edit: func(c *config.Config) {
			c.Storage.FileSystem.Provider = "obs"
			c.Storage.FileSystem.Endpoint = "https://obs.example.com"
			c.Workspace.Providers = map[string]config.WorkspaceFUSEProviderConfig{
				"obs": validFUSEProvider("obs.example.com", "cilium-fqdn"),
			}
			p := c.Workspace.Providers["obs"]
			p.SystemEgressCIDRs = nil
			p.EndpointHostIPs = nil
			c.Workspace.Providers["obs"] = p
		}, want: ""},
		{name: "valid with no custom CA", edit: func(c *config.Config) {
			c.Storage.FileSystem.CAFile = ""
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.CASecretKey = "" })
		}, want: ""},
		{name: "unsupported provider", edit: func(c *config.Config) { c.Storage.FileSystem.Provider = "s3" }, want: "fuse supports only minio or obs"},
		{name: "missing selected provider", edit: func(c *config.Config) { c.Workspace.Providers = map[string]config.WorkspaceFUSEProviderConfig{} }, want: "workspace.providers.minio"},
		{name: "missing redis", edit: func(c *config.Config) { c.Storage.State.Redis.Addr = "" }, want: "storage.state.redis.addr"},
		{name: "missing secret name", edit: func(c *config.Config) { c.Workspace.SecretName = "" }, want: "workspace.secret_name"},
		{name: "missing bucket", edit: func(c *config.Config) { c.Storage.FileSystem.Bucket = "" }, want: "storage.filesystem.bucket"},
		{name: "missing endpoint", edit: func(c *config.Config) { c.Storage.FileSystem.Endpoint = "" }, want: "storage.filesystem.endpoint"},
		{name: "missing access key file", edit: func(c *config.Config) { c.Storage.FileSystem.CredentialFiles.AccessKeyFile = "" }, want: "access_key_file"},
		{name: "missing secret key file", edit: func(c *config.Config) { c.Storage.FileSystem.CredentialFiles.SecretKeyFile = "" }, want: "secret_key_file"},
		{name: "inline access key", edit: func(c *config.Config) { c.Storage.FileSystem.AccessKey = "inline" }, want: "inline access_key"},
		{name: "inline secret key", edit: func(c *config.Config) { c.Storage.FileSystem.SecretKey = "inline" }, want: "inline secret_key"},
		{name: "session token", edit: func(c *config.Config) { c.Storage.FileSystem.SessionToken = "token" }, want: "session token or credential expiry"},
		{name: "credential expiry", edit: func(c *config.Config) { c.Storage.FileSystem.CredentialExpiry = "tomorrow" }, want: "session token or credential expiry"},
		{name: "session token file", edit: func(c *config.Config) { c.Storage.FileSystem.CredentialFiles.SessionTokenFile = "/secret/token" }, want: "session token or credential expiry"},
		{name: "credential expiry file", edit: func(c *config.Config) { c.Storage.FileSystem.CredentialFiles.CredentialExpiryFile = "/secret/expiry" }, want: "session token or credential expiry"},
		{name: "negative pool min", edit: func(c *config.Config) { c.Workspace.FUSEPool.MinSize = -1 }, want: "fuse_pool.min_size"},
		{name: "zero pool max", edit: func(c *config.Config) { c.Workspace.FUSEPool.MinSize = 0; c.Workspace.FUSEPool.MaxSize = 0 }, want: "fuse_pool.max_size"},
		{name: "pool min exceeds max", edit: func(c *config.Config) { c.Workspace.FUSEPool.MinSize = 21 }, want: "fuse_pool.max_size"},
		{name: "zero refill interval", edit: func(c *config.Config) { c.Workspace.FUSEPool.RefillIntervalSeconds = 0 }, want: "refill_interval_seconds"},
		{name: "zero prepare timeout", edit: func(c *config.Config) { c.Workspace.FUSEPool.PrepareTimeoutSeconds = 0 }, want: "prepare_timeout_seconds"},
		{name: "zero mount timeout", edit: func(c *config.Config) { c.Workspace.MountTimeoutSeconds = 0 }, want: "mount_timeout_seconds"},
		{name: "zero flush timeout", edit: func(c *config.Config) { c.Workspace.FlushTimeoutSeconds = 0 }, want: "flush_timeout_seconds"},
		{name: "zero unmount timeout", edit: func(c *config.Config) { c.Workspace.UnmountTimeoutSeconds = 0 }, want: "unmount_timeout_seconds"},
		{name: "zero lease ttl", edit: func(c *config.Config) { c.Workspace.LeaseTTLSeconds = 0 }, want: "lease_ttl_seconds"},
		{name: "zero lease renew", edit: func(c *config.Config) { c.Workspace.LeaseRenewIntervalSeconds = 0 }, want: "lease_renew_interval_seconds"},
		{name: "lease renew too slow", edit: func(c *config.Config) { c.Workspace.LeaseTTLSeconds = 60; c.Workspace.LeaseRenewIntervalSeconds = 21 }, want: "lease_renew_interval_seconds"},
		{name: "recreate disabled", edit: func(c *config.Config) { c.Workspace.RecreateMaxAttempts = 0 }, want: "recreate_max_attempts"},
		{name: "multiple recreate attempts", edit: func(c *config.Config) { c.Workspace.RecreateMaxAttempts = 2 }, want: "recreate_max_attempts"},
		{name: "memory cache", edit: func(c *config.Config) { c.Workspace.CacheMedium = "memory" }, want: "cache_medium"},
		{name: "invalid cache quantity", edit: func(c *config.Config) { c.Workspace.CacheSize = "large" }, want: "cache_size"},
		{name: "zero cache quantity", edit: func(c *config.Config) { c.Workspace.CacheSize = "0" }, want: "cache_size"},
		{name: "negative cache quantity", edit: func(c *config.Config) { c.Workspace.CacheSize = "-1Gi" }, want: "cache_size"},
		{name: "hard quota", edit: func(c *config.Config) { c.Workspace.QuotaMode = "hard" }, want: "quota_mode"},
		{name: "wrong driver", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.Driver = "goofys" })
		}, want: "driver"},
		{name: "missing profile", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.Profile = "" })
		}, want: "profile"},
		{name: "missing identity", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.StorageIdentity = "" })
		}, want: "storage_identity"},
		{name: "missing credential generation", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.CredentialGeneration = "" })
		}, want: "credential_generation"},
		{name: "missing mounter image", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.MounterImage = "" })
		}, want: "mounter_image"},
		{name: "mutable mounter image", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.MounterImage = "mounter:latest" })
		}, want: "sha256 digest"},
		{name: "short mounter digest", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.MounterImage = "mounter@sha256:abc" })
		}, want: "sha256 digest"},
		{name: "nonhex mounter digest", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) {
				p.MounterImage = "mounter@sha256:" + strings.Repeat("z", 64)
			})
		}, want: "sha256 digest"},
		{name: "uppercase mounter digest", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) {
				p.MounterImage = "registry.example.com/mounter@sha256:" + strings.Repeat("A", 64)
			})
		}, want: "sha256 digest"},
		{name: "invalid mounter image reference", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) {
				p.MounterImage = "invalid image@sha256:" + strings.Repeat("a", 64)
			})
		}, want: "sha256 digest"},
		{name: "missing docker image", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.DockerImage = "" })
		}, want: "docker_image"},
		{name: "mutable docker image", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.DockerImage = "sandbox:latest" })
		}, want: "sha256 digest"},
		{name: "missing lsm profile", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.LSMProfile = "" })
		}, want: "lsm_profile"},
		{name: "unconfined lsm", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.LSMProfile = "unconfined" })
		}, want: "lsm_profile"},
		{name: "disabled lsm", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.LSMProfile = "label=disable" })
		}, want: "lsm_profile"},
		{name: "missing egress mode", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.SystemEgressMode = "" })
		}, want: "system_egress_mode"},
		{name: "unknown egress mode", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.SystemEgressMode = "allow-all" })
		}, want: "system_egress_mode"},
		{name: "missing dns cidrs", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.DNSCIDRs = nil })
		}, want: "dns_cidrs"},
		{name: "bare dns ip", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.DNSCIDRs = []string{"8.8.8.8"} })
		}, want: "host-only"},
		{name: "dns subnet", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.DNSCIDRs = []string{"8.8.8.0/24"} })
		}, want: "host-only"},
		{name: "ipv6 dns subnet", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.DNSCIDRs = []string{"2001:db8::/64"} })
		}, want: "host-only"},
		{name: "shared address DNS", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.DNSCIDRs = []string{"100.64.0.53/32"} })
		}, want: "public resolver"},
		{name: "benchmark DNS", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.DNSCIDRs = []string{"198.18.0.53/32"} })
		}, want: "public resolver"},
		{name: "documentation DNS", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.DNSCIDRs = []string{"2001:db8::53/128"} })
		}, want: "public resolver"},
		{name: "missing endpoint ports", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.EndpointPorts = nil })
		}, want: "endpoint_ports"},
		{name: "zero endpoint port", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.EndpointPorts = []int32{0} })
		}, want: "endpoint_ports"},
		{name: "high endpoint port", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.EndpointPorts = []int32{65536} })
		}, want: "endpoint_ports"},
		{name: "proxy configured", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.ProxyURL = "http://proxy.example.com" })
		}, want: "proxy_url"},
		{name: "only control plane CA", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.CASecretKey = "" })
		}, want: "ca_file and ca_secret_key"},
		{name: "only provider CA", edit: func(c *config.Config) {
			c.Storage.FileSystem.CAFile = ""
		}, want: "ca_file and ca_secret_key"},
		{name: "endpoint host alias is not an IP", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.EndpointHostIPs = []string{"minio.example.com"} })
		}, want: "endpoint_host_ips"},
		{name: "endpoint host alias is not approved", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.EndpointHostIPs = []string{"198.51.100.20"} })
		}, want: "approved system_egress_cidrs"},
		{name: "missing cidr egress", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.SystemEgressCIDRs = nil })
		}, want: "system_egress_cidrs"},
		{name: "invalid cidr egress", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) { p.SystemEgressCIDRs = []string{"not-a-cidr"} })
		}, want: "system_egress_cidrs"},
		{name: "missing fqdn egress", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) {
				p.SystemEgressMode = "cilium-fqdn"
				p.SystemEgressFQDNs = nil
			})
		}, want: "system_egress_fqdns"},
		{name: "empty fqdn egress", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) {
				p.SystemEgressMode = "cilium-fqdn"
				p.SystemEgressFQDNs = []string{""}
			})
		}, want: "system_egress_fqdns"},
		{name: "wildcard fqdn egress", edit: func(c *config.Config) {
			editSelectedProvider(c, func(p *config.WorkspaceFUSEProviderConfig) {
				p.SystemEgressMode = "cilium-fqdn"
				p.SystemEgressFQDNs = []string{"*.example.com"}
			})
		}, want: "wildcard"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newValidFUSEConfig()
			tt.edit(cfg)
			err := cfg.Validate()
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestFUSEMounterResourceValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*config.WorkspaceFUSEResourceConfig)
		want string
	}{
		{name: "invalid cpu request", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.CPURequest = "invalid" }, want: "mounter_resources.cpu_request"},
		{name: "zero cpu request", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.CPURequest = "0" }, want: "mounter_resources.cpu_request"},
		{name: "negative cpu request", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.CPURequest = "-1" }, want: "mounter_resources.cpu_request"},
		{name: "invalid cpu limit", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.CPULimit = "invalid" }, want: "mounter_resources.cpu_limit"},
		{name: "zero cpu limit", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.CPULimit = "0" }, want: "mounter_resources.cpu_limit"},
		{name: "invalid memory request", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.MemoryRequest = "invalid" }, want: "mounter_resources.memory_request"},
		{name: "zero memory request", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.MemoryRequest = "0" }, want: "mounter_resources.memory_request"},
		{name: "invalid memory limit", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.MemoryLimit = "invalid" }, want: "mounter_resources.memory_limit"},
		{name: "zero memory limit", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.MemoryLimit = "0" }, want: "mounter_resources.memory_limit"},
		{name: "invalid ephemeral request", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.EphemeralStorageRequest = "invalid" }, want: "mounter_resources.ephemeral_storage_request"},
		{name: "zero ephemeral request", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.EphemeralStorageRequest = "0" }, want: "mounter_resources.ephemeral_storage_request"},
		{name: "invalid ephemeral limit", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.EphemeralStorageLimit = "invalid" }, want: "mounter_resources.ephemeral_storage_limit"},
		{name: "zero ephemeral limit", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.EphemeralStorageLimit = "0" }, want: "mounter_resources.ephemeral_storage_limit"},
		{name: "cpu request exceeds limit", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.CPURequest = "2" }, want: "cpu_request must be <="},
		{name: "memory request exceeds limit", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.MemoryRequest = "1Gi" }, want: "memory_request must be <="},
		{name: "ephemeral request exceeds limit", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.EphemeralStorageRequest = "4Gi" }, want: "ephemeral_storage_request must be <="},
		{name: "cache exceeds ephemeral limit", edit: func(r *config.WorkspaceFUSEResourceConfig) { r.EphemeralStorageLimit = "1Gi" }, want: "ephemeral_storage_limit must be >= workspace.cache_size"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newValidFUSEConfig()
			tt.edit(&cfg.Workspace.MounterResources)
			require.ErrorContains(t, cfg.Validate(), tt.want)
		})
	}
}

func editSelectedProvider(c *config.Config, edit func(*config.WorkspaceFUSEProviderConfig)) {
	provider := c.Workspace.Providers[c.Storage.FileSystem.Provider]
	edit(&provider)
	c.Workspace.Providers[c.Storage.FileSystem.Provider] = provider
}

func TestFUSESubPathValidation(t *testing.T) {
	valid := []string{"", "workspace", "workspaces/team", "workspaces/团队"}
	for _, subPath := range valid {
		t.Run("valid_"+subPath, func(t *testing.T) {
			cfg := newValidFUSEConfig()
			cfg.Storage.FileSystem.SubPath = subPath
			require.NoError(t, cfg.Validate())
		})
	}

	invalid := []string{
		"/root", "root/", "a//b", ".", "..", "a/./b", "a/../b",
		".sandbox-system", ".sandbox-system/owner", "a\x00b", "a\nb", string([]byte{0xff}),
	}
	for i, subPath := range invalid {
		t.Run(fmt.Sprintf("invalid_%d", i), func(t *testing.T) {
			cfg := newValidFUSEConfig()
			cfg.Storage.FileSystem.SubPath = subPath
			require.ErrorContains(t, cfg.Validate(), "canonical relative prefix")
		})
	}
}

func TestLoadFUSEProviderFromEnvironmentOnly(t *testing.T) {
	env := map[string]string{
		"SANDBOX_SECURITY_API_KEY":                                           "test-key",
		"SANDBOX_WORKSPACE_MODE":                                             "fuse",
		"SANDBOX_WORKSPACE_SECRET_NAME":                                      "sandbox-minio",
		"SANDBOX_STORAGE_FILESYSTEM_PROVIDER":                                "minio",
		"SANDBOX_STORAGE_FILESYSTEM_BUCKET":                                  "sandbox",
		"SANDBOX_STORAGE_FILESYSTEM_ENDPOINT":                                "minio.example.com:9000",
		"SANDBOX_STORAGE_FILESYSTEM_CA_FILE":                                 "/run/secrets/ca.crt",
		"SANDBOX_STORAGE_FILESYSTEM_CREDENTIAL_FILES_ACCESS_KEY_FILE":        "/run/secrets/access-key",
		"SANDBOX_STORAGE_FILESYSTEM_CREDENTIAL_FILES_SECRET_KEY_FILE":        "/run/secrets/secret-key",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_DRIVER":                           "s3fs",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_PROFILE":                          "minio-profile",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_STORAGE_IDENTITY":                 "minio-primary",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_MOUNTER_IMAGE":                    "registry.example.com/mounter@sha256:" + strings.Repeat("a", 64),
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_DOCKER_IMAGE":                     "registry.example.com/sandbox@sha256:" + strings.Repeat("b", 64),
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_CREDENTIAL_GENERATION":            "gen-1",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_ENDPOINT_HOST_IPS":                "192.0.2.10",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_LSM_PROFILE":                      "sandbox-fuse",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_SYSTEM_EGRESS_MODE":               "cidr",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_DNS_CIDRS":                        "8.8.8.8/32,1.1.1.1/32",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_SYSTEM_EGRESS_CIDRS":              "192.0.2.0/24",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_ENDPOINT_PORTS":                   "9000",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_PROXY_URL":                        "",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_CA_SECRET_KEY":                    "ca.crt",
		"SANDBOX_WORKSPACE_PROVIDERS_MINIO_SYSTEM_EGRESS_FQDNS":              "minio.example.com",
		"SANDBOX_STORAGE_FILESYSTEM_CREDENTIAL_FILES_SESSION_TOKEN_FILE":     "",
		"SANDBOX_STORAGE_FILESYSTEM_CREDENTIAL_FILES_CREDENTIAL_EXPIRY_FILE": "",
	}
	for key, value := range env {
		t.Setenv(key, value)
	}

	cfg, err := config.Load("")
	require.NoError(t, err)

	provider, ok := cfg.Workspace.Providers["minio"]
	require.True(t, ok)
	assert.Equal(t, "s3fs", provider.Driver)
	assert.Equal(t, []string{"8.8.8.8/32", "1.1.1.1/32"}, provider.DNSCIDRs)
	assert.Equal(t, []string{"192.0.2.10"}, provider.EndpointHostIPs)
	assert.Equal(t, []int32{9000}, provider.EndpointPorts)
}
