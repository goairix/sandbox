package config_test

import (
	"os"
	"path/filepath"
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

	// Storage.Object defaults
	assert.Equal(t, "local", cfg.Storage.Object.Provider)
	assert.Equal(t, "", cfg.Storage.Object.Bucket)
	assert.Equal(t, "", cfg.Storage.Object.Region)
	assert.Equal(t, "", cfg.Storage.Object.Endpoint)
	assert.Equal(t, "", cfg.Storage.Object.AccessKey)
	assert.Equal(t, "", cfg.Storage.Object.SecretKey)
	assert.Equal(t, "/tmp/sandbox-storage", cfg.Storage.Object.LocalPath)

	// Security defaults
	assert.Equal(t, 30, cfg.Security.ExecTimeoutSeconds)
	assert.Equal(t, "256Mi", cfg.Security.MaxMemory)
	assert.Equal(t, "100Mi", cfg.Security.MaxDisk)
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
  object:
    provider: "s3"
    bucket: "my-bucket"
    region: "us-east-1"
    endpoint: "https://s3.amazonaws.com"
    access_key: "AKIAIOSFODNN7EXAMPLE"
    secret_key: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
    local_path: "/data/sandbox"

security:
  api_key: "test-yaml-api-key"
  exec_timeout_seconds: 60
  max_memory: "512Mi"
  max_disk: "200Mi"
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

	// Storage.Object
	assert.Equal(t, "s3", cfg.Storage.Object.Provider)
	assert.Equal(t, "my-bucket", cfg.Storage.Object.Bucket)
	assert.Equal(t, "us-east-1", cfg.Storage.Object.Region)
	assert.Equal(t, "https://s3.amazonaws.com", cfg.Storage.Object.Endpoint)
	assert.Equal(t, "AKIAIOSFODNN7EXAMPLE", cfg.Storage.Object.AccessKey)
	assert.Equal(t, "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", cfg.Storage.Object.SecretKey)
	assert.Equal(t, "/data/sandbox", cfg.Storage.Object.LocalPath)

	// Security
	assert.Equal(t, 60, cfg.Security.ExecTimeoutSeconds)
	assert.Equal(t, "512Mi", cfg.Security.MaxMemory)
	assert.Equal(t, "200Mi", cfg.Security.MaxDisk)
	assert.Equal(t, 200, cfg.Security.MaxPids)
	assert.Equal(t, true, cfg.Security.NetworkEnabled)
	assert.Equal(t, []string{"10.0.0.0/8", "192.168.0.0/16"}, cfg.Security.NetworkWhitelist)
	assert.Equal(t, "/etc/seccomp/sandbox.json", cfg.Security.SeccompProfile)
}

func TestEnvOverrides(t *testing.T) {
	// Set env vars before loading; clean up after
	envVars := map[string]string{
		"SANDBOX_SERVER_PORT":                   "7070",
		"SANDBOX_SERVER_HOST":                   "localhost",
		"SANDBOX_RUNTIME_TYPE":                  "kubernetes",
		"SANDBOX_RUNTIME_DOCKER_HOST":           "tcp://docker.example.com:2376",
		"SANDBOX_RUNTIME_KUBERNETES_KUBECONFIG": "/root/.kube/config",
		"SANDBOX_RUNTIME_KUBERNETES_NAMESPACE":  "production",
		"SANDBOX_POOL_MIN_SIZE":                 "10",
		"SANDBOX_POOL_MAX_SIZE":                 "100",
		"SANDBOX_POOL_REFILL_INTERVAL_SECONDS":  "30",
		"SANDBOX_STORAGE_STATE_REDIS_ADDR":      "cache.example.com:6379",
		"SANDBOX_STORAGE_STATE_REDIS_PASSWORD":  "redispass",
		"SANDBOX_STORAGE_STATE_REDIS_DB":        "2",
		"SANDBOX_STORAGE_OBJECT_PROVIDER":       "cos",
		"SANDBOX_STORAGE_OBJECT_BUCKET":         "env-bucket",
		"SANDBOX_STORAGE_OBJECT_REGION":         "ap-guangzhou",
		"SANDBOX_STORAGE_OBJECT_ENDPOINT":       "https://cos.ap-guangzhou.myqcloud.com",
		"SANDBOX_STORAGE_OBJECT_ACCESS_KEY":     "env-access-key",
		"SANDBOX_STORAGE_OBJECT_SECRET_KEY":     "env-secret-key",
		"SANDBOX_STORAGE_OBJECT_LOCAL_PATH":     "/env/sandbox-storage",
		"SANDBOX_SECURITY_EXEC_TIMEOUT_SECONDS": "120",
		"SANDBOX_SECURITY_API_KEY":              "env-api-key",
		"SANDBOX_SECURITY_MAX_MEMORY":           "1Gi",
		"SANDBOX_SECURITY_MAX_DISK":             "500Mi",
		"SANDBOX_SECURITY_MAX_PIDS":             "500",
		"SANDBOX_SECURITY_NETWORK_ENABLED":      "true",
		"SANDBOX_SECURITY_SECCOMP_PROFILE":      "/etc/seccomp/custom.json",
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

	assert.Equal(t, "cos", cfg.Storage.Object.Provider)
	assert.Equal(t, "env-bucket", cfg.Storage.Object.Bucket)
	assert.Equal(t, "ap-guangzhou", cfg.Storage.Object.Region)
	assert.Equal(t, "https://cos.ap-guangzhou.myqcloud.com", cfg.Storage.Object.Endpoint)
	assert.Equal(t, "env-access-key", cfg.Storage.Object.AccessKey)
	assert.Equal(t, "env-secret-key", cfg.Storage.Object.SecretKey)
	assert.Equal(t, "/env/sandbox-storage", cfg.Storage.Object.LocalPath)

	assert.Equal(t, 120, cfg.Security.ExecTimeoutSeconds)
	assert.Equal(t, "1Gi", cfg.Security.MaxMemory)
	assert.Equal(t, "500Mi", cfg.Security.MaxDisk)
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
