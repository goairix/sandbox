# AI Agent Sandbox Platform Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a production-grade, container-isolated sandbox platform that provides AI Agents with secure execution environments for running Python, JS/TS, and Bash commands, supporting file I/O, persistent sessions, and distributed deployment on Docker and Kubernetes.

**Architecture:** Three-layer design: API layer (HTTP + SSE) -> Sandbox Manager (lifecycle, pool, session) -> Runtime abstraction (Docker/K8s). Object storage abstracted to support S3/MinIO/COS/OBS/OSS. Container pool with warm containers for low-latency startup. Two sandbox modes: ephemeral (single-use) and persistent (multi-request sessions).

**Tech Stack:** Go 1.25, Gin (HTTP+SSE), Docker SDK (github.com/docker/docker), client-go (K8s), go-redis/redis, aws-sdk-go-v2 (S3), tencentcloud COS SDK, huaweicloud OBS SDK, aliyun OSS SDK, viper (config), testify (testing).

---

## File Structure

```
github.com/goairix/sandbox
├── cmd/
│   └── sandbox/
│       └── main.go                          # Entry point
│
├── internal/
│   ├── config/
│   │   └── config.go                        # Config struct + loader (viper)
│   │
│   ├── sandbox/
│   │   ├── types.go                         # Core domain types (SandboxConfig, SandboxState, ExecRequest, etc.)
│   │   ├── manager.go                       # SandboxManager - lifecycle orchestration
│   │   ├── pool.go                          # ContainerPool - warm container management
│   │   └── session.go                       # SessionStore - persistent sandbox state
│   │
│   ├── runtime/
│   │   ├── runtime.go                       # Runtime interface definition
│   │   ├── types.go                         # Runtime-specific types (ExecResult, SandboxInfo, etc.)
│   │   ├── docker/
│   │   │   ├── runtime.go                   # DockerRuntime implementation
│   │   │   ├── container.go                 # Container create/start/stop/remove operations
│   │   │   ├── exec.go                      # Container exec (sync + stream)
│   │   │   ├── file.go                      # File upload/download via container API
│   │   │   └── network.go                   # Docker network isolation + whitelist
│   │   └── kubernetes/
│   │       ├── runtime.go                   # K8sRuntime implementation
│   │       ├── pod.go                       # Pod create/delete/status operations
│   │       ├── exec.go                      # Pod exec (sync + stream)
│   │       ├── file.go                      # File upload/download via pod cp
│   │       └── network.go                   # NetworkPolicy management
│   │
│   ├── storage/
│   │   ├── state/
│   │   │   ├── state.go                     # StateStore interface
│   │   │   └── redis/
│   │   │       └── store.go                 # Redis StateStore implementation
│   │   └── object/
│   │       ├── object.go                    # ObjectStore interface
│   │       ├── s3/
│   │       │   └── store.go                 # S3/MinIO implementation
│   │       ├── cos/
│   │       │   └── store.go                 # Tencent COS implementation
│   │       ├── obs/
│   │       │   └── store.go                 # Huawei OBS implementation
│   │       ├── oss/
│   │       │   └── store.go                 # Aliyun OSS implementation
│   │       └── local/
│   │           └── store.go                 # Local filesystem (dev)
│   │
│   └── api/
│       ├── server.go                        # HTTP server setup + graceful shutdown
│       ├── router.go                        # Route registration
│       ├── handler/
│       │   ├── sandbox.go                   # POST/GET/DELETE /sandboxes
│       │   ├── exec.go                      # POST /sandboxes/:id/exec, /exec/stream
│       │   ├── file.go                      # POST upload, GET download, GET list
│       │   └── execute.go                   # POST /execute, /execute/stream (one-shot)
│       └── middleware/
│           ├── auth.go                      # API key auth middleware
│           └── ratelimit.go                 # Rate limiting middleware
│
├── pkg/
│   └── types/
│       ├── sandbox.go                       # Public API types: CreateSandboxRequest/Response
│       ├── exec.go                          # Public API types: ExecRequest/Response, SSE events
│       └── file.go                          # Public API types: FileUpload/Download/List
│
├── docker/
│   ├── images/
│   │   ├── python/
│   │   │   └── Dockerfile                   # Python 3.12 + common libs
│   │   ├── nodejs/
│   │   │   └── Dockerfile                   # Node 20 + common packages
│   │   └── bash/
│   │       └── Dockerfile                   # Alpine + common CLI tools
│   ├── Dockerfile                           # Sandbox service image
│   └── docker-compose.yml                   # Local dev: service + redis + minio
│
├── configs/
│   ├── config.yaml                          # Default config template
│   └── seccomp-profile.json                 # Seccomp security profile
│
├── deploy/
│   └── helm/
│       └── sandbox/
│           ├── Chart.yaml
│           ├── values.yaml
│           └── templates/
│               ├── deployment.yaml
│               ├── service.yaml
│               ├── hpa.yaml
│               └── networkpolicy.yaml
│
└── docs/
    └── superpowers/
        └── plans/
            └── 2026-03-30-sandbox-platform.md
```

---

## Phase 1: Core Types & Config (Foundation)

### Task 1: Project Configuration

**Files:**
- Create: `internal/config/config.go`
- Create: `configs/config.yaml`

- [ ] **Step 1: Write config test**

Create `internal/config/config_test.go`:

```go
package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, 8080, cfg.Server.Port)
	assert.Equal(t, "docker", cfg.Runtime.Type)
	assert.Equal(t, 3, cfg.Pool.MinSize)
	assert.Equal(t, 20, cfg.Pool.MaxSize)
	assert.Equal(t, "local", cfg.Storage.Object.Provider)
}

func TestLoadConfig_FromFile(t *testing.T) {
	content := `
server:
  port: 9090
runtime:
  type: kubernetes
pool:
  min_size: 5
  max_size: 50
storage:
  object:
    provider: s3
    bucket: test-bucket
    region: us-east-1
`
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	err := os.WriteFile(path, []byte(content), 0644)
	require.NoError(t, err)

	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, "kubernetes", cfg.Runtime.Type)
	assert.Equal(t, 5, cfg.Pool.MinSize)
	assert.Equal(t, 50, cfg.Pool.MaxSize)
	assert.Equal(t, "s3", cfg.Storage.Object.Provider)
	assert.Equal(t, "test-bucket", cfg.Storage.Object.Bucket)
}

func TestLoadConfig_EnvOverride(t *testing.T) {
	t.Setenv("SANDBOX_SERVER_PORT", "7070")
	t.Setenv("SANDBOX_RUNTIME_TYPE", "kubernetes")

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, 7070, cfg.Server.Port)
	assert.Equal(t, "kubernetes", cfg.Runtime.Type)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/config/ -v`
Expected: FAIL — package does not exist

- [ ] **Step 3: Install dependencies and write config implementation**

Run:
```bash
cd /Users/dysodeng/project/go/cloud/sandbox
go get github.com/spf13/viper
go get github.com/stretchr/testify
```

Create `internal/config/config.go`:

```go
package config

import (
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Server  ServerConfig  `mapstructure:"server"`
	Runtime RuntimeConfig `mapstructure:"runtime"`
	Pool    PoolConfig    `mapstructure:"pool"`
	Storage StorageConfig `mapstructure:"storage"`
	Security SecurityConfig `mapstructure:"security"`
}

type ServerConfig struct {
	Port int    `mapstructure:"port"`
	Host string `mapstructure:"host"`
}

type RuntimeConfig struct {
	Type       string           `mapstructure:"type"` // "docker" or "kubernetes"
	Docker     DockerConfig     `mapstructure:"docker"`
	Kubernetes KubernetesConfig `mapstructure:"kubernetes"`
}

type DockerConfig struct {
	Host string `mapstructure:"host"` // e.g. "unix:///var/run/docker.sock"
}

type KubernetesConfig struct {
	Kubeconfig string `mapstructure:"kubeconfig"`
	Namespace  string `mapstructure:"namespace"`
}

type PoolConfig struct {
	MinSize        int `mapstructure:"min_size"`
	MaxSize        int `mapstructure:"max_size"`
	RefillInterval int `mapstructure:"refill_interval_seconds"`
}

type StorageConfig struct {
	State  StateStorageConfig  `mapstructure:"state"`
	Object ObjectStorageConfig `mapstructure:"object"`
}

type StateStorageConfig struct {
	Redis RedisConfig `mapstructure:"redis"`
}

type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type ObjectStorageConfig struct {
	Provider  string `mapstructure:"provider"` // "s3", "cos", "obs", "oss", "local"
	Bucket    string `mapstructure:"bucket"`
	Region    string `mapstructure:"region"`
	Endpoint  string `mapstructure:"endpoint"`
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
	LocalPath string `mapstructure:"local_path"` // for "local" provider
}

type SecurityConfig struct {
	ExecTimeout    int      `mapstructure:"exec_timeout_seconds"`
	MaxMemory      string   `mapstructure:"max_memory"`
	MaxDisk        string   `mapstructure:"max_disk"`
	MaxPids        int      `mapstructure:"max_pids"`
	NetworkEnabled bool     `mapstructure:"network_enabled"`
	NetworkWhitelist []string `mapstructure:"network_whitelist"`
	SeccompProfile string   `mapstructure:"seccomp_profile"`
}

func Load(path string) (*Config, error) {
	v := viper.New()

	// Defaults
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("runtime.type", "docker")
	v.SetDefault("runtime.docker.host", "unix:///var/run/docker.sock")
	v.SetDefault("runtime.kubernetes.namespace", "sandbox")
	v.SetDefault("pool.min_size", 3)
	v.SetDefault("pool.max_size", 20)
	v.SetDefault("pool.refill_interval_seconds", 10)
	v.SetDefault("storage.state.redis.addr", "localhost:6379")
	v.SetDefault("storage.state.redis.db", 0)
	v.SetDefault("storage.object.provider", "local")
	v.SetDefault("storage.object.local_path", "/tmp/sandbox-storage")
	v.SetDefault("security.exec_timeout_seconds", 30)
	v.SetDefault("security.max_memory", "256Mi")
	v.SetDefault("security.max_disk", "100Mi")
	v.SetDefault("security.max_pids", 100)
	v.SetDefault("security.network_enabled", false)

	// Env overrides: SANDBOX_SERVER_PORT, SANDBOX_RUNTIME_TYPE, etc.
	v.SetEnvPrefix("SANDBOX")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// File
	if path != "" {
		v.SetConfigFile(path)
		if err := v.ReadInConfig(); err != nil {
			return nil, err
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/config/ -v`
Expected: All 3 tests PASS

- [ ] **Step 5: Create default config file**

Create `configs/config.yaml`:

```yaml
server:
  port: 8080
  host: "0.0.0.0"

runtime:
  type: "docker"  # "docker" or "kubernetes"
  docker:
    host: "unix:///var/run/docker.sock"
  kubernetes:
    kubeconfig: ""
    namespace: "sandbox"

pool:
  min_size: 3
  max_size: 20
  refill_interval_seconds: 10

storage:
  state:
    redis:
      addr: "localhost:6379"
      password: ""
      db: 0
  object:
    provider: "local"  # "s3", "cos", "obs", "oss", "local"
    bucket: ""
    region: ""
    endpoint: ""
    access_key: ""
    secret_key: ""
    local_path: "/tmp/sandbox-storage"

security:
  exec_timeout_seconds: 30
  max_memory: "256Mi"
  max_disk: "100Mi"
  max_pids: 100
  network_enabled: false
  network_whitelist: []
  seccomp_profile: ""
```

- [ ] **Step 6: Commit**

```bash
git add internal/config/ configs/
git commit -m "feat: add config module with viper, defaults, file, and env support"
```

---

### Task 2: Core Domain Types

**Files:**
- Create: `internal/sandbox/types.go`
- Create: `internal/runtime/types.go`
- Create: `pkg/types/sandbox.go`
- Create: `pkg/types/exec.go`
- Create: `pkg/types/file.go`

- [ ] **Step 1: Create internal sandbox domain types**

Create `internal/sandbox/types.go`:

```go
package sandbox

import "time"

// Language represents a supported programming language/runtime.
type Language string

const (
	LangPython Language = "python"
	LangNodeJS Language = "nodejs"
	LangBash   Language = "bash"
)

// Mode represents the sandbox lifecycle mode.
type Mode string

const (
	ModeEphemeral  Mode = "ephemeral"
	ModePersistent Mode = "persistent"
)

// State represents the current state of a sandbox.
type State string

const (
	StateCreating   State = "creating"
	StateReady      State = "ready"
	StateRunning    State = "running"
	StateIdle       State = "idle"
	StateDestroying State = "destroying"
	StateDestroyed  State = "destroyed"
	StateError      State = "error"
)

// Dependency represents an extra package to install at sandbox startup.
type Dependency struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ResourceLimits defines resource constraints for a sandbox.
type ResourceLimits struct {
	Memory string `json:"memory"` // e.g. "256Mi"
	CPU    string `json:"cpu"`    // e.g. "0.5"
	Disk   string `json:"disk"`   // e.g. "100Mi"
}

// NetworkConfig defines network settings for a sandbox.
type NetworkConfig struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist"` // allowed domains
}

// SandboxConfig holds all configuration for creating a sandbox.
type SandboxConfig struct {
	Language     Language       `json:"language"`
	Mode         Mode           `json:"mode"`
	Timeout      int            `json:"timeout"` // seconds, max sandbox lifetime
	Resources    ResourceLimits `json:"resources"`
	Network      NetworkConfig  `json:"network"`
	Dependencies []Dependency   `json:"dependencies"`
}

// Sandbox represents a running sandbox instance.
type Sandbox struct {
	ID        string        `json:"id"`
	Config    SandboxConfig `json:"config"`
	State     State         `json:"state"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	// RuntimeID is the container/pod ID in the underlying runtime
	RuntimeID string `json:"runtime_id"`
}
```

- [ ] **Step 2: Create runtime types**

Create `internal/runtime/types.go`:

```go
package runtime

import "time"

// SandboxInfo holds runtime-level sandbox information.
type SandboxInfo struct {
	ID        string
	RuntimeID string // container ID or pod name
	State     string
	CreatedAt time.Time
}

// ExecRequest holds parameters for executing a command in a sandbox.
type ExecRequest struct {
	Command string            // the command to run
	Stdin   string            // optional stdin input
	Timeout int               // seconds
	Env     map[string]string // additional environment variables
	WorkDir string            // working directory, defaults to /workspace
}

// ExecResult holds the result of a synchronous command execution.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// StreamEvent represents a single event in a streamed execution.
type StreamEvent struct {
	Type    StreamEventType
	Content string
}

// StreamEventType is the type of stream event.
type StreamEventType string

const (
	StreamStdout StreamEventType = "stdout"
	StreamStderr StreamEventType = "stderr"
	StreamDone   StreamEventType = "done"
	StreamError  StreamEventType = "error"
)

// SandboxSpec defines what the runtime needs to create a sandbox.
type SandboxSpec struct {
	ID       string
	Image    string
	Memory   string // e.g. "256Mi"
	CPU      string // e.g. "0.5"
	Disk     string // e.g. "100Mi"
	PidLimit int
	// Network
	NetworkEnabled   bool
	NetworkWhitelist []string
	// Security
	ReadOnlyRootFS bool
	RunAsUser      int64
	SeccompProfile string
	// Labels for identification
	Labels map[string]string
}
```

- [ ] **Step 3: Create public API types**

Create `pkg/types/sandbox.go`:

```go
package types

import "time"

type CreateSandboxRequest struct {
	Language     string            `json:"language" binding:"required,oneof=python nodejs bash"`
	Mode         string            `json:"mode" binding:"required,oneof=ephemeral persistent"`
	Timeout      int               `json:"timeout,omitempty"`
	Resources    *ResourceLimits   `json:"resources,omitempty"`
	Network      *NetworkConfig    `json:"network,omitempty"`
	Dependencies []DependencySpec  `json:"dependencies,omitempty"`
}

type ResourceLimits struct {
	Memory string `json:"memory,omitempty"`
	CPU    string `json:"cpu,omitempty"`
	Disk   string `json:"disk,omitempty"`
}

type NetworkConfig struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist,omitempty"`
}

type DependencySpec struct {
	Name    string `json:"name" binding:"required"`
	Version string `json:"version" binding:"required"`
}

type SandboxResponse struct {
	ID        string    `json:"id"`
	Language  string    `json:"language"`
	Mode      string    `json:"mode"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}
```

Create `pkg/types/exec.go`:

```go
package types

type ExecRequest struct {
	Command string            `json:"command" binding:"required"`
	Stdin   string            `json:"stdin,omitempty"`
	Timeout int               `json:"timeout,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type ExecResponse struct {
	ExitCode int     `json:"exit_code"`
	Stdout   string  `json:"stdout"`
	Stderr   string  `json:"stderr"`
	Duration float64 `json:"duration"` // seconds
}

// ExecuteRequest is for one-shot execution (no pre-created sandbox).
type ExecuteRequest struct {
	Language     string            `json:"language" binding:"required,oneof=python nodejs bash"`
	Command      string            `json:"command" binding:"required"`
	Stdin        string            `json:"stdin,omitempty"`
	Timeout      int               `json:"timeout,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Resources    *ResourceLimits   `json:"resources,omitempty"`
	Network      *NetworkConfig    `json:"network,omitempty"`
	Dependencies []DependencySpec  `json:"dependencies,omitempty"`
}

// SSEEvent represents a Server-Sent Event for streamed execution.
type SSEEvent struct {
	Event string `json:"event"` // "stdout", "stderr", "status", "done", "error"
	Data  any    `json:"data"`
}

type SSEStdoutData struct {
	Content string `json:"content"`
}

type SSEStderrData struct {
	Content string `json:"content"`
}

type SSEStatusData struct {
	State   string  `json:"state"`
	Elapsed float64 `json:"elapsed"`
}

type SSEDoneData struct {
	ExitCode int     `json:"exit_code"`
	Elapsed  float64 `json:"elapsed"`
}

type SSEErrorData struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}
```

Create `pkg/types/file.go`:

```go
package types

import "time"

type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

type FileListResponse struct {
	Files []FileInfo `json:"files"`
	Path  string     `json:"path"`
}

type FileDownloadRequest struct {
	Path string `form:"path" binding:"required"`
}

type FileUploadResponse struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}
```

- [ ] **Step 4: Verify types compile**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build ./internal/sandbox/ && go build ./internal/runtime/ && go build ./pkg/types/`
Expected: No errors

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/types.go internal/runtime/types.go pkg/types/
git commit -m "feat: add core domain types, runtime types, and public API types"
```

---

## Phase 2: Storage Abstraction Layer

### Task 3: Object Storage Interface + Local Implementation

**Files:**
- Create: `internal/storage/object/object.go`
- Create: `internal/storage/object/local/store.go`
- Create: `internal/storage/object/local/store_test.go`

- [ ] **Step 1: Write ObjectStore interface**

Create `internal/storage/object/object.go`:

```go
package object

import (
	"context"
	"io"
	"time"
)

// ObjectInfo holds metadata about a stored object.
type ObjectInfo struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Store is the abstraction over object storage backends.
type Store interface {
	// Put uploads an object.
	Put(ctx context.Context, key string, reader io.Reader, size int64) error
	// Get downloads an object.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Delete removes an object.
	Delete(ctx context.Context, key string) error
	// List returns objects with the given prefix.
	List(ctx context.Context, prefix string) ([]ObjectInfo, error)
	// Exists checks if an object exists.
	Exists(ctx context.Context, key string) (bool, error)
	// PresignedPutURL returns a pre-signed URL for uploading.
	PresignedPutURL(ctx context.Context, key string, expires time.Duration) (string, error)
	// PresignedGetURL returns a pre-signed URL for downloading.
	PresignedGetURL(ctx context.Context, key string, expires time.Duration) (string, error)
}
```

- [ ] **Step 2: Write failing tests for local store**

Create `internal/storage/object/local/store_test.go`:

```go
package local

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalStore_PutAndGet(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	ctx := context.Background()

	data := []byte("hello sandbox")
	err := store.Put(ctx, "test/file.txt", bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	reader, err := store.Get(ctx, "test/file.txt")
	require.NoError(t, err)
	defer reader.Close()

	got, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestLocalStore_Delete(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	ctx := context.Background()

	data := []byte("to delete")
	err := store.Put(ctx, "del.txt", bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	err = store.Delete(ctx, "del.txt")
	require.NoError(t, err)

	exists, err := store.Exists(ctx, "del.txt")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestLocalStore_List(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	ctx := context.Background()

	files := []string{"prefix/a.txt", "prefix/b.txt", "other/c.txt"}
	for _, f := range files {
		data := []byte("content")
		err := store.Put(ctx, f, bytes.NewReader(data), int64(len(data)))
		require.NoError(t, err)
	}

	objs, err := store.List(ctx, "prefix/")
	require.NoError(t, err)
	assert.Len(t, objs, 2)
}

func TestLocalStore_Exists(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	ctx := context.Background()

	exists, err := store.Exists(ctx, "nope.txt")
	require.NoError(t, err)
	assert.False(t, exists)

	data := []byte("yes")
	err = store.Put(ctx, "yes.txt", bytes.NewReader(data), int64(len(data)))
	require.NoError(t, err)

	exists, err = store.Exists(ctx, "yes.txt")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestLocalStore_GetNotFound(t *testing.T) {
	dir := t.TempDir()
	store := New(dir)
	ctx := context.Background()

	_, err := store.Get(ctx, "nonexistent.txt")
	assert.Error(t, err)
}

func TestLocalStore_PresignedURLsNotSupported(t *testing.T) {
	dir := t.TempDir()
	store := New(filepath.Join(dir, "store"))
	ctx := context.Background()

	_, err := store.PresignedPutURL(ctx, "key", 0)
	assert.Error(t, err)

	_, err = store.PresignedGetURL(ctx, "key", 0)
	assert.Error(t, err)
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/storage/object/local/ -v`
Expected: FAIL — package does not exist

- [ ] **Step 4: Implement local store**

Create `internal/storage/object/local/store.go`:

```go
package local

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/storage/object"
)

// Store implements object.Store using the local filesystem.
type Store struct {
	basePath string
}

// New creates a new local filesystem object store.
func New(basePath string) *Store {
	return &Store{basePath: basePath}
}

func (s *Store) fullPath(key string) string {
	return filepath.Join(s.basePath, filepath.FromSlash(key))
}

func (s *Store) Put(_ context.Context, key string, reader io.Reader, _ int64) error {
	path := s.fullPath(key)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, reader)
	return err
}

func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, error) {
	path := s.fullPath(key)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *Store) Delete(_ context.Context, key string) error {
	return os.Remove(s.fullPath(key))
}

func (s *Store) List(_ context.Context, prefix string) ([]object.ObjectInfo, error) {
	dir := s.fullPath(prefix)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var result []object.ObjectInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		key := prefix + entry.Name()
		// Normalize to forward slashes
		key = strings.ReplaceAll(key, string(filepath.Separator), "/")
		result = append(result, object.ObjectInfo{
			Key:          key,
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
	}
	return result, nil
}

func (s *Store) Exists(_ context.Context, key string) (bool, error) {
	_, err := os.Stat(s.fullPath(key))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (s *Store) PresignedPutURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", errors.New("presigned URLs not supported by local store")
}

func (s *Store) PresignedGetURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", errors.New("presigned URLs not supported by local store")
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/storage/object/local/ -v`
Expected: All 6 tests PASS

- [ ] **Step 6: Commit**

```bash
git add internal/storage/object/
git commit -m "feat: add ObjectStore interface and local filesystem implementation"
```

---

### Task 4: S3 Object Storage Implementation

**Files:**
- Create: `internal/storage/object/s3/store.go`

- [ ] **Step 1: Install S3 SDK**

Run:
```bash
cd /Users/dysodeng/project/go/cloud/sandbox
go get github.com/aws/aws-sdk-go-v2
go get github.com/aws/aws-sdk-go-v2/config
go get github.com/aws/aws-sdk-go-v2/credentials
go get github.com/aws/aws-sdk-go-v2/service/s3
go get github.com/aws/aws-sdk-go-v2/feature/s3/manager
```

- [ ] **Step 2: Implement S3 store**

Create `internal/storage/object/s3/store.go`:

```go
package s3

import (
	"context"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/goairix/sandbox/internal/storage/object"
)

// Options holds configuration for the S3 store.
type Options struct {
	Bucket    string
	Region    string
	Endpoint  string
	AccessKey string
	SecretKey string
}

// Store implements object.Store using AWS S3 or S3-compatible services (MinIO).
type Store struct {
	client *s3.Client
	bucket string
}

// New creates a new S3 object store.
func New(ctx context.Context, opts Options) (*Store, error) {
	var cfgOpts []func(*awsconfig.LoadOptions) error

	cfgOpts = append(cfgOpts, awsconfig.WithRegion(opts.Region))

	if opts.AccessKey != "" && opts.SecretKey != "" {
		cfgOpts = append(cfgOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, ""),
		))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return nil, err
	}

	var s3Opts []func(*s3.Options)
	if opts.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(opts.Endpoint)
			o.UsePathStyle = true // required for MinIO
		})
	}

	client := s3.NewFromConfig(cfg, s3Opts...)

	return &Store{
		client: client,
		bucket: opts.Bucket,
	}, nil
}

func (s *Store) Put(ctx context.Context, key string, reader io.Reader, size int64) error {
	input := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
		Body:   reader,
	}
	if size > 0 {
		input.ContentLength = aws.Int64(size)
	}
	_, err := s.client.PutObject(ctx, input)
	return err
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	output, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, err
	}
	return output.Body, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	return err
}

func (s *Store) List(ctx context.Context, prefix string) ([]object.ObjectInfo, error) {
	output, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil, err
	}

	var result []object.ObjectInfo
	for _, obj := range output.Contents {
		result = append(result, object.ObjectInfo{
			Key:          aws.ToString(obj.Key),
			Size:         aws.ToInt64(obj.Size),
			LastModified: aws.ToTime(obj.LastModified),
		})
	}
	return result, nil
}

func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		// Check if it's a not-found error
		return false, nil
	}
	return true, nil
}

func (s *Store) PresignedPutURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	presigner := s3.NewPresignClient(s.client)
	output, err := presigner.PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expires))
	if err != nil {
		return "", err
	}
	return output.URL, nil
}

func (s *Store) PresignedGetURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	presigner := s3.NewPresignClient(s.client)
	output, err := presigner.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expires))
	if err != nil {
		return "", err
	}
	return output.URL, nil
}
```

- [ ] **Step 3: Verify it compiles**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build ./internal/storage/object/s3/`
Expected: No errors

- [ ] **Step 4: Commit**

```bash
git add internal/storage/object/s3/
git commit -m "feat: add S3/MinIO object storage implementation"
```

---

### Task 5: COS / OBS / OSS Object Storage Implementations

**Files:**
- Create: `internal/storage/object/cos/store.go`
- Create: `internal/storage/object/obs/store.go`
- Create: `internal/storage/object/oss/store.go`

- [ ] **Step 1: Install cloud SDKs**

Run:
```bash
cd /Users/dysodeng/project/go/cloud/sandbox
go get github.com/tencentyun/cos-go-sdk-v5
go get github.com/huaweicloud/huaweicloud-sdk-go-obs/obs
go get github.com/aliyun/aliyun-oss-go-sdk/oss
```

- [ ] **Step 2: Implement COS store**

Create `internal/storage/object/cos/store.go`:

```go
package cos

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tencentyun/cos-go-sdk-v5"

	"github.com/goairix/sandbox/internal/storage/object"
)

type Options struct {
	Bucket    string
	Region    string
	SecretID  string
	SecretKey string
}

type Store struct {
	client *cos.Client
	bucket string
}

func New(opts Options) (*Store, error) {
	bucketURL, err := url.Parse(fmt.Sprintf("https://%s.cos.%s.myqcloud.com", opts.Bucket, opts.Region))
	if err != nil {
		return nil, err
	}
	serviceURL, err := url.Parse(fmt.Sprintf("https://cos.%s.myqcloud.com", opts.Region))
	if err != nil {
		return nil, err
	}

	client := cos.NewClient(&cos.BaseURL{
		BucketURL:  bucketURL,
		ServiceURL: serviceURL,
	}, &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:  opts.SecretID,
			SecretKey: opts.SecretKey,
		},
	})

	return &Store{client: client, bucket: opts.Bucket}, nil
}

func (s *Store) Put(ctx context.Context, key string, reader io.Reader, _ int64) error {
	_, err := s.client.Object.Put(ctx, key, reader, nil)
	return err
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.client.Object.Get(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.Object.Delete(ctx, key)
	return err
}

func (s *Store) List(ctx context.Context, prefix string) ([]object.ObjectInfo, error) {
	opt := &cos.BucketGetOptions{
		Prefix: prefix,
	}
	result, _, err := s.client.Bucket.Get(ctx, opt)
	if err != nil {
		return nil, err
	}

	var objs []object.ObjectInfo
	for _, item := range result.Contents {
		modTime, _ := time.Parse(time.RFC3339, item.LastModified)
		objs = append(objs, object.ObjectInfo{
			Key:          item.Key,
			Size:         int64(item.Size),
			LastModified: modTime,
		})
	}
	return objs, nil
}

func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	ok, err := s.client.Object.IsExist(ctx, key)
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (s *Store) PresignedPutURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	presignedURL, err := s.client.Object.GetPresignedURL(ctx, http.MethodPut, key, opts.SecretID, opts.SecretKey, expires, nil)
	if err != nil {
		return "", err
	}
	return presignedURL.String(), nil
}

func (s *Store) PresignedGetURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	presignedURL, err := s.client.Object.GetPresignedURL(ctx, http.MethodGet, key, opts.SecretID, opts.SecretKey, expires, nil)
	if err != nil {
		return "", err
	}
	return presignedURL.String(), nil
}
```

Note: COS presigned URL requires secretID/secretKey at sign time. The Store needs to store these — adjust:

Actually, let me fix the COS store to store the credentials for presigned URL generation:

Create `internal/storage/object/cos/store.go` (corrected):

```go
package cos

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/tencentyun/cos-go-sdk-v5"

	"github.com/goairix/sandbox/internal/storage/object"
)

type Options struct {
	Bucket    string
	Region    string
	SecretID  string
	SecretKey string
}

type Store struct {
	client    *cos.Client
	secretID  string
	secretKey string
}

func New(opts Options) (*Store, error) {
	bucketURL, err := url.Parse(fmt.Sprintf("https://%s.cos.%s.myqcloud.com", opts.Bucket, opts.Region))
	if err != nil {
		return nil, err
	}
	serviceURL, err := url.Parse(fmt.Sprintf("https://cos.%s.myqcloud.com", opts.Region))
	if err != nil {
		return nil, err
	}

	client := cos.NewClient(&cos.BaseURL{
		BucketURL:  bucketURL,
		ServiceURL: serviceURL,
	}, &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:  opts.SecretID,
			SecretKey: opts.SecretKey,
		},
	})

	return &Store{
		client:    client,
		secretID:  opts.SecretID,
		secretKey: opts.SecretKey,
	}, nil
}

func (s *Store) Put(ctx context.Context, key string, reader io.Reader, _ int64) error {
	_, err := s.client.Object.Put(ctx, key, reader, nil)
	return err
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := s.client.Object.Get(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.Object.Delete(ctx, key)
	return err
}

func (s *Store) List(ctx context.Context, prefix string) ([]object.ObjectInfo, error) {
	opt := &cos.BucketGetOptions{
		Prefix: prefix,
	}
	result, _, err := s.client.Bucket.Get(ctx, opt)
	if err != nil {
		return nil, err
	}

	var objs []object.ObjectInfo
	for _, item := range result.Contents {
		modTime, _ := time.Parse(time.RFC3339, item.LastModified)
		objs = append(objs, object.ObjectInfo{
			Key:          item.Key,
			Size:         int64(item.Size),
			LastModified: modTime,
		})
	}
	return objs, nil
}

func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	ok, err := s.client.Object.IsExist(ctx, key)
	if err != nil {
		return false, err
	}
	return ok, nil
}

func (s *Store) PresignedPutURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	presignedURL, err := s.client.Object.GetPresignedURL(ctx, http.MethodPut, key, s.secretID, s.secretKey, expires, nil)
	if err != nil {
		return "", err
	}
	return presignedURL.String(), nil
}

func (s *Store) PresignedGetURL(ctx context.Context, key string, expires time.Duration) (string, error) {
	presignedURL, err := s.client.Object.GetPresignedURL(ctx, http.MethodGet, key, s.secretID, s.secretKey, expires, nil)
	if err != nil {
		return "", err
	}
	return presignedURL.String(), nil
}
```

- [ ] **Step 3: Implement OBS store**

Create `internal/storage/object/obs/store.go`:

```go
package obs

import (
	"context"
	"io"
	"time"

	huaweiobs "github.com/huaweicloud/huaweicloud-sdk-go-obs/obs"

	"github.com/goairix/sandbox/internal/storage/object"
)

type Options struct {
	Bucket    string
	Endpoint  string // e.g. "obs.cn-north-4.myhuaweicloud.com"
	AccessKey string
	SecretKey string
}

type Store struct {
	client *huaweiobs.ObsClient
	bucket string
}

func New(opts Options) (*Store, error) {
	client, err := huaweiobs.New(opts.AccessKey, opts.SecretKey, opts.Endpoint)
	if err != nil {
		return nil, err
	}
	return &Store{client: client, bucket: opts.Bucket}, nil
}

func (s *Store) Put(_ context.Context, key string, reader io.Reader, _ int64) error {
	input := &huaweiobs.PutObjectInput{
		PutObjectBasicInput: huaweiobs.PutObjectBasicInput{
			ObjectOperationInput: huaweiobs.ObjectOperationInput{
				Bucket: s.bucket,
				Key:    key,
			},
		},
		Body: reader,
	}
	_, err := s.client.PutObject(input)
	return err
}

func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, error) {
	input := &huaweiobs.GetObjectInput{
		GetObjectMetadataInput: huaweiobs.GetObjectMetadataInput{
			Bucket: s.bucket,
			Key:    key,
		},
	}
	output, err := s.client.GetObject(input)
	if err != nil {
		return nil, err
	}
	return output.Body, nil
}

func (s *Store) Delete(_ context.Context, key string) error {
	input := &huaweiobs.DeleteObjectInput{
		Bucket: s.bucket,
		Key:    key,
	}
	_, err := s.client.DeleteObject(input)
	return err
}

func (s *Store) List(_ context.Context, prefix string) ([]object.ObjectInfo, error) {
	input := &huaweiobs.ListObjectsInput{
		Bucket: s.bucket,
		ListObjsInput: huaweiobs.ListObjsInput{
			Prefix: prefix,
		},
	}
	output, err := s.client.ListObjects(input)
	if err != nil {
		return nil, err
	}

	var result []object.ObjectInfo
	for _, obj := range output.Contents {
		result = append(result, object.ObjectInfo{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		})
	}
	return result, nil
}

func (s *Store) Exists(_ context.Context, key string) (bool, error) {
	input := &huaweiobs.GetObjectMetadataInput{
		Bucket: s.bucket,
		Key:    key,
	}
	_, err := s.client.GetObjectMetadata(input)
	if err != nil {
		return false, nil
	}
	return true, nil
}

func (s *Store) PresignedPutURL(_ context.Context, key string, expires time.Duration) (string, error) {
	input := &huaweiobs.CreateSignedUrlInput{
		Method:  huaweiobs.HttpMethodPut,
		Bucket:  s.bucket,
		Key:     key,
		Expires: int(expires.Seconds()),
	}
	output, err := s.client.CreateSignedUrl(input)
	if err != nil {
		return "", err
	}
	return output.SignedUrl, nil
}

func (s *Store) PresignedGetURL(_ context.Context, key string, expires time.Duration) (string, error) {
	input := &huaweiobs.CreateSignedUrlInput{
		Method:  huaweiobs.HttpMethodGet,
		Bucket:  s.bucket,
		Key:     key,
		Expires: int(expires.Seconds()),
	}
	output, err := s.client.CreateSignedUrl(input)
	if err != nil {
		return "", err
	}
	return output.SignedUrl, nil
}
```

- [ ] **Step 4: Implement OSS store**

Create `internal/storage/object/oss/store.go`:

```go
package oss

import (
	"bytes"
	"context"
	"io"
	"time"

	alioss "github.com/aliyun/aliyun-oss-go-sdk/oss"

	"github.com/goairix/sandbox/internal/storage/object"
)

type Options struct {
	Bucket    string
	Endpoint  string // e.g. "oss-cn-hangzhou.aliyuncs.com"
	AccessKey string
	SecretKey string
}

type Store struct {
	bucket *alioss.Bucket
}

func New(opts Options) (*Store, error) {
	client, err := alioss.New(opts.Endpoint, opts.AccessKey, opts.SecretKey)
	if err != nil {
		return nil, err
	}
	bucket, err := client.Bucket(opts.Bucket)
	if err != nil {
		return nil, err
	}
	return &Store{bucket: bucket}, nil
}

func (s *Store) Put(_ context.Context, key string, reader io.Reader, _ int64) error {
	return s.bucket.PutObject(key, reader)
}

func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return s.bucket.GetObject(key)
}

func (s *Store) Delete(_ context.Context, key string) error {
	return s.bucket.DeleteObject(key)
}

func (s *Store) List(_ context.Context, prefix string) ([]object.ObjectInfo, error) {
	result, err := s.bucket.ListObjects(alioss.Prefix(prefix))
	if err != nil {
		return nil, err
	}

	var objs []object.ObjectInfo
	for _, obj := range result.Objects {
		objs = append(objs, object.ObjectInfo{
			Key:          obj.Key,
			Size:         obj.Size,
			LastModified: obj.LastModified,
		})
	}
	return objs, nil
}

func (s *Store) Exists(_ context.Context, key string) (bool, error) {
	return s.bucket.IsObjectExist(key)
}

func (s *Store) PresignedPutURL(_ context.Context, key string, expires time.Duration) (string, error) {
	return s.bucket.SignURL(key, alioss.HTTPPut, int64(expires.Seconds()))
}

func (s *Store) PresignedGetURL(_ context.Context, key string, expires time.Duration) (string, error) {
	return s.bucket.SignURL(key, alioss.HTTPGet, int64(expires.Seconds()))
}

// ensure unused import is used
var _ = bytes.NewReader
```

- [ ] **Step 5: Verify all compile**

Run:
```bash
cd /Users/dysodeng/project/go/cloud/sandbox
go build ./internal/storage/object/cos/
go build ./internal/storage/object/obs/
go build ./internal/storage/object/oss/
```
Expected: No errors (if SDK API mismatches, fix accordingly)

- [ ] **Step 6: Commit**

```bash
git add internal/storage/object/cos/ internal/storage/object/obs/ internal/storage/object/oss/
git commit -m "feat: add COS, OBS, OSS object storage implementations"
```

---

### Task 6: State Store (Redis)

**Files:**
- Create: `internal/storage/state/state.go`
- Create: `internal/storage/state/redis/store.go`
- Create: `internal/storage/state/redis/store_test.go`

- [ ] **Step 1: Write StateStore interface**

Create `internal/storage/state/state.go`:

```go
package state

import (
	"context"
	"time"
)

// Store is the abstraction over state storage backends (sandbox sessions, pool state).
type Store interface {
	// Set stores a value with optional TTL. Pass 0 for no expiration.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Get retrieves a value. Returns nil, nil if key does not exist.
	Get(ctx context.Context, key string) ([]byte, error)
	// Delete removes a key.
	Delete(ctx context.Context, key string) error
	// Exists checks if a key exists.
	Exists(ctx context.Context, key string) (bool, error)
	// SetNX sets a value only if the key does not exist (for distributed locks).
	// Returns true if the key was set.
	SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error)
	// Keys returns all keys matching a pattern (e.g. "sandbox:*").
	Keys(ctx context.Context, pattern string) ([]string, error)
}
```

- [ ] **Step 2: Write failing Redis store tests**

Create `internal/storage/state/redis/store_test.go`:

```go
package redis

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func skipIfNoRedis(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR not set, skipping Redis integration test")
	}
}

func testStore(t *testing.T) *Store {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	s, err := New(Options{Addr: addr})
	require.NoError(t, err)
	return s
}

func TestRedisStore_SetAndGet(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()

	err := s.Set(ctx, "test:key1", []byte("value1"), time.Minute)
	require.NoError(t, err)

	val, err := s.Get(ctx, "test:key1")
	require.NoError(t, err)
	assert.Equal(t, []byte("value1"), val)

	// cleanup
	_ = s.Delete(ctx, "test:key1")
}

func TestRedisStore_GetNotFound(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()

	val, err := s.Get(ctx, "test:nonexistent")
	require.NoError(t, err)
	assert.Nil(t, val)
}

func TestRedisStore_Delete(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()

	_ = s.Set(ctx, "test:del", []byte("x"), time.Minute)
	err := s.Delete(ctx, "test:del")
	require.NoError(t, err)

	exists, err := s.Exists(ctx, "test:del")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestRedisStore_SetNX(t *testing.T) {
	skipIfNoRedis(t)
	s := testStore(t)
	ctx := context.Background()

	// cleanup first
	_ = s.Delete(ctx, "test:nx")

	ok, err := s.SetNX(ctx, "test:nx", []byte("first"), time.Minute)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = s.SetNX(ctx, "test:nx", []byte("second"), time.Minute)
	require.NoError(t, err)
	assert.False(t, ok)

	val, err := s.Get(ctx, "test:nx")
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), val)

	_ = s.Delete(ctx, "test:nx")
}
```

- [ ] **Step 3: Run tests to verify they fail (or skip)**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/storage/state/redis/ -v`
Expected: FAIL — package does not exist

- [ ] **Step 4: Install Redis dependency and implement**

Run:
```bash
cd /Users/dysodeng/project/go/cloud/sandbox
go get github.com/redis/go-redis/v9
```

Create `internal/storage/state/redis/store.go`:

```go
package redis

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

type Options struct {
	Addr     string
	Password string
	DB       int
}

type Store struct {
	client *redis.Client
}

func New(opts Options) (*Store, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     opts.Addr,
		Password: opts.Password,
		DB:       opts.DB,
	})
	return &Store{client: client}, nil
}

func (s *Store) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	return s.client.Set(ctx, key, value, ttl).Err()
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, error) {
	val, err := s.client.Get(ctx, key).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return nil, err
	}
	return val, nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	return s.client.Del(ctx, key).Err()
}

func (s *Store) Exists(ctx context.Context, key string) (bool, error) {
	n, err := s.client.Exists(ctx, key).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func (s *Store) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	return s.client.SetNX(ctx, key, value, ttl).Result()
}

func (s *Store) Keys(ctx context.Context, pattern string) ([]string, error) {
	return s.client.Keys(ctx, pattern).Result()
}

// Close closes the Redis connection.
func (s *Store) Close() error {
	return s.client.Close()
}
```

- [ ] **Step 5: Verify it compiles**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build ./internal/storage/state/redis/`
Expected: No errors

- [ ] **Step 6: Commit**

```bash
git add internal/storage/state/
git commit -m "feat: add StateStore interface and Redis implementation"
```

---

## Phase 3: Runtime Abstraction Layer

### Task 7: Runtime Interface

**Files:**
- Create: `internal/runtime/runtime.go`

- [ ] **Step 1: Define Runtime interface**

Create `internal/runtime/runtime.go`:

```go
package runtime

import (
	"context"
	"io"
)

// Runtime is the abstraction over container orchestration backends (Docker, Kubernetes).
type Runtime interface {
	// CreateSandbox creates a new sandbox container/pod from the given spec.
	CreateSandbox(ctx context.Context, spec SandboxSpec) (*SandboxInfo, error)

	// StartSandbox starts a previously created sandbox (for pool warm-up scenarios).
	StartSandbox(ctx context.Context, id string) error

	// StopSandbox stops a running sandbox.
	StopSandbox(ctx context.Context, id string) error

	// RemoveSandbox removes a sandbox completely.
	RemoveSandbox(ctx context.Context, id string) error

	// GetSandbox returns the current info of a sandbox.
	GetSandbox(ctx context.Context, id string) (*SandboxInfo, error)

	// Exec executes a command synchronously and returns the result.
	Exec(ctx context.Context, id string, req ExecRequest) (*ExecResult, error)

	// ExecStream executes a command and streams output via a channel.
	ExecStream(ctx context.Context, id string, req ExecRequest) (<-chan StreamEvent, error)

	// UploadFile uploads a file into the sandbox.
	UploadFile(ctx context.Context, id string, destPath string, reader io.Reader) error

	// DownloadFile downloads a file from the sandbox.
	DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error)

	// ListFiles lists files in a directory inside the sandbox.
	ListFiles(ctx context.Context, id string, dirPath string) ([]FileInfo, error)
}

// FileInfo holds file metadata from inside a sandbox.
type FileInfo struct {
	Name    string
	Path    string
	Size    int64
	IsDir   bool
	ModTime int64 // unix timestamp
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build ./internal/runtime/`
Expected: No errors

- [ ] **Step 3: Commit**

```bash
git add internal/runtime/runtime.go
git commit -m "feat: add Runtime interface for container abstraction"
```

---

### Task 8: Docker Runtime - Container Management

**Files:**
- Create: `internal/runtime/docker/runtime.go`
- Create: `internal/runtime/docker/container.go`
- Create: `internal/runtime/docker/network.go`

- [ ] **Step 1: Install Docker SDK**

Run:
```bash
cd /Users/dysodeng/project/go/cloud/sandbox
go get github.com/docker/docker/client
go get github.com/docker/docker/api/types
go get github.com/docker/docker/api/types/container
go get github.com/docker/docker/api/types/network
go get github.com/docker/go-connections/nat
```

- [ ] **Step 2: Implement Docker network management**

Create `internal/runtime/docker/network.go`:

```go
package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/network"
	dockerclient "github.com/docker/docker/client"
)

const (
	sandboxNetworkName = "sandbox-network"
)

// ensureNetwork creates the sandbox network if it doesn't exist.
func ensureNetwork(ctx context.Context, cli *dockerclient.Client) (string, error) {
	networks, err := cli.NetworkList(ctx, network.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list networks: %w", err)
	}

	for _, n := range networks {
		if n.Name == sandboxNetworkName {
			return n.ID, nil
		}
	}

	resp, err := cli.NetworkCreate(ctx, sandboxNetworkName, network.CreateOptions{
		Driver:     "bridge",
		Internal:   true, // no external access by default
		Attachable: true,
	})
	if err != nil {
		return "", fmt.Errorf("create network: %w", err)
	}
	return resp.ID, nil
}
```

- [ ] **Step 3: Implement container operations**

Create `internal/runtime/docker/container.go`:

```go
package docker

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"

	"github.com/goairix/sandbox/internal/runtime"
)

// imageForSpec returns the Docker image to use for a sandbox spec.
func imageForSpec(spec runtime.SandboxSpec) string {
	if spec.Image != "" {
		return spec.Image
	}
	// Default images based on labels
	lang, ok := spec.Labels["language"]
	if !ok {
		return "sandbox-bash:latest"
	}
	switch lang {
	case "python":
		return "sandbox-python:latest"
	case "nodejs":
		return "sandbox-nodejs:latest"
	default:
		return "sandbox-bash:latest"
	}
}

// createContainerConfig builds Docker container configuration from a SandboxSpec.
func createContainerConfig(spec runtime.SandboxSpec) (*container.Config, *container.HostConfig) {
	config := &container.Config{
		Image: imageForSpec(spec),
		Labels: spec.Labels,
		WorkingDir: "/workspace",
		Tty:        false,
		// Keep container running with a sleep process
		Cmd: []string{"sleep", "infinity"},
	}

	// Parse memory limit
	var memoryBytes int64
	if spec.Memory != "" {
		memoryBytes = parseMemory(spec.Memory)
	}

	hostConfig := &container.HostConfig{
		Resources: container.Resources{
			Memory:    memoryBytes,
			PidsLimit: int64Ptr(int64(spec.PidLimit)),
		},
		ReadonlyRootfs: spec.ReadOnlyRootFS,
		SecurityOpt:    []string{},
		// Tmpfs for writable directories on read-only root
		Tmpfs: map[string]string{
			"/tmp": "size=50m",
		},
	}

	if spec.SeccompProfile != "" {
		hostConfig.SecurityOpt = append(hostConfig.SecurityOpt,
			fmt.Sprintf("seccomp=%s", spec.SeccompProfile))
	}

	// Drop all capabilities, add only needed ones
	hostConfig.CapDrop = []string{"ALL"}
	hostConfig.CapAdd = []string{"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE"}

	// Run as non-root user
	if spec.RunAsUser > 0 {
		config.User = fmt.Sprintf("%d", spec.RunAsUser)
	}

	return config, hostConfig
}

// parseMemory converts "256Mi" to bytes.
func parseMemory(s string) int64 {
	var value int64
	var unit string
	_, _ = fmt.Sscanf(s, "%d%s", &value, &unit)
	switch unit {
	case "Ki":
		return value * 1024
	case "Mi":
		return value * 1024 * 1024
	case "Gi":
		return value * 1024 * 1024 * 1024
	default:
		return value
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}

// createContainer creates a Docker container from spec.
func createContainer(ctx context.Context, cli *dockerclient.Client, spec runtime.SandboxSpec, networkID string) (string, error) {
	config, hostConfig := createContainerConfig(spec)

	resp, err := cli.ContainerCreate(ctx, config, hostConfig, nil, nil, spec.ID)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	// Connect to sandbox network
	if networkID != "" {
		if err := cli.NetworkConnect(ctx, networkID, resp.ID, nil); err != nil {
			// cleanup on failure
			_ = cli.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
			return "", fmt.Errorf("connect network: %w", err)
		}
	}

	return resp.ID, nil
}
```

- [ ] **Step 4: Implement Docker Runtime**

Create `internal/runtime/docker/runtime.go`:

```go
package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	dockerclient "github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/goairix/sandbox/internal/runtime"
)

// Runtime implements runtime.Runtime using Docker.
type Runtime struct {
	cli       *dockerclient.Client
	networkID string
}

// New creates a new Docker runtime.
func New(ctx context.Context, host string) (*Runtime, error) {
	opts := []dockerclient.Opt{
		dockerclient.WithAPIVersionNegotiation(),
	}
	if host != "" {
		opts = append(opts, dockerclient.WithHost(host))
	}

	cli, err := dockerclient.NewClientWithOpts(opts...)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	netID, err := ensureNetwork(ctx, cli)
	if err != nil {
		return nil, err
	}

	return &Runtime{cli: cli, networkID: netID}, nil
}

func (r *Runtime) CreateSandbox(ctx context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	containerID, err := createContainer(ctx, r.cli, spec, r.networkID)
	if err != nil {
		return nil, err
	}

	// Start the container
	if err := r.cli.ContainerStart(ctx, containerID, container.StartOptions{}); err != nil {
		_ = r.cli.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("start container: %w", err)
	}

	return &runtime.SandboxInfo{
		ID:        spec.ID,
		RuntimeID: containerID,
		State:     "running",
		CreatedAt: time.Now(),
	}, nil
}

func (r *Runtime) StartSandbox(ctx context.Context, id string) error {
	return r.cli.ContainerStart(ctx, id, container.StartOptions{})
}

func (r *Runtime) StopSandbox(ctx context.Context, id string) error {
	timeout := 10
	return r.cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout})
}

func (r *Runtime) RemoveSandbox(ctx context.Context, id string) error {
	return r.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true})
}

func (r *Runtime) GetSandbox(ctx context.Context, id string) (*runtime.SandboxInfo, error) {
	info, err := r.cli.ContainerInspect(ctx, id)
	if err != nil {
		return nil, err
	}

	state := "unknown"
	if info.State.Running {
		state = "running"
	} else if info.State.Paused {
		state = "paused"
	} else {
		state = "stopped"
	}

	created, _ := time.Parse(time.RFC3339Nano, info.Created)

	return &runtime.SandboxInfo{
		ID:        id,
		RuntimeID: info.ID,
		State:     state,
		CreatedAt: created,
	}, nil
}

func (r *Runtime) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	start := time.Now()

	workDir := req.WorkDir
	if workDir == "" {
		workDir = "/workspace"
	}

	// Build command
	cmd := []string{"sh", "-c", req.Command}

	// Build env
	var env []string
	for k, v := range req.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		AttachStdin:  req.Stdin != "",
		WorkingDir:   workDir,
		Env:          env,
	}

	execResp, err := r.cli.ContainerExecCreate(ctx, id, execConfig)
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	attachResp, err := r.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}
	defer attachResp.Close()

	// Send stdin if provided
	if req.Stdin != "" {
		_, _ = io.Copy(attachResp.Conn, strings.NewReader(req.Stdin))
		_ = attachResp.CloseWrite()
	}

	// Read stdout/stderr
	var stdout, stderr bytes.Buffer
	_, err = stdcopy.StdCopy(&stdout, &stderr, attachResp.Reader)
	if err != nil {
		return nil, fmt.Errorf("read output: %w", err)
	}

	// Get exit code
	inspectResp, err := r.cli.ContainerExecInspect(ctx, execResp.ID)
	if err != nil {
		return nil, fmt.Errorf("exec inspect: %w", err)
	}

	return &runtime.ExecResult{
		ExitCode: inspectResp.ExitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}, nil
}

func (r *Runtime) ExecStream(ctx context.Context, id string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	workDir := req.WorkDir
	if workDir == "" {
		workDir = "/workspace"
	}

	cmd := []string{"sh", "-c", req.Command}

	var env []string
	for k, v := range req.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	execConfig := container.ExecOptions{
		Cmd:          cmd,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   workDir,
		Env:          env,
	}

	execResp, err := r.cli.ContainerExecCreate(ctx, id, execConfig)
	if err != nil {
		return nil, fmt.Errorf("exec create: %w", err)
	}

	attachResp, err := r.cli.ContainerExecAttach(ctx, execResp.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("exec attach: %w", err)
	}

	ch := make(chan runtime.StreamEvent, 64)

	go func() {
		defer close(ch)
		defer attachResp.Close()

		// Use a pipe to demux stdout/stderr
		stdoutPR, stdoutPW := io.Pipe()
		stderrPR, stderrPW := io.Pipe()

		go func() {
			_, _ = stdcopy.StdCopy(stdoutPW, stderrPW, attachResp.Reader)
			stdoutPW.Close()
			stderrPW.Close()
		}()

		// Stream stdout in a goroutine
		done := make(chan struct{})
		go func() {
			defer close(done)
			buf := make([]byte, 4096)
			for {
				n, err := stderrPR.Read(buf)
				if n > 0 {
					ch <- runtime.StreamEvent{
						Type:    runtime.StreamStderr,
						Content: string(buf[:n]),
					}
				}
				if err != nil {
					break
				}
			}
		}()

		// Stream stdout in this goroutine
		buf := make([]byte, 4096)
		for {
			n, err := stdoutPR.Read(buf)
			if n > 0 {
				ch <- runtime.StreamEvent{
					Type:    runtime.StreamStdout,
					Content: string(buf[:n]),
				}
			}
			if err != nil {
				break
			}
		}

		<-done

		// Get exit code
		inspectResp, err := r.cli.ContainerExecInspect(context.Background(), execResp.ID)
		if err != nil {
			ch <- runtime.StreamEvent{
				Type:    runtime.StreamError,
				Content: err.Error(),
			}
			return
		}

		ch <- runtime.StreamEvent{
			Type:    runtime.StreamDone,
			Content: fmt.Sprintf("%d", inspectResp.ExitCode),
		}
	}()

	return ch, nil
}

func (r *Runtime) UploadFile(ctx context.Context, id string, destPath string, reader io.Reader) error {
	return r.cli.CopyToContainer(ctx, id, destPath, reader, types.CopyToContainerOptions{})
}

func (r *Runtime) DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	reader, _, err := r.cli.CopyFromContainer(ctx, id, srcPath)
	if err != nil {
		return nil, err
	}
	return reader, nil
}

func (r *Runtime) ListFiles(ctx context.Context, id string, dirPath string) ([]runtime.FileInfo, error) {
	// Execute ls -la in the container to list files
	result, err := r.Exec(ctx, id, runtime.ExecRequest{
		Command: fmt.Sprintf("find %s -maxdepth 1 -printf '%%f\\t%%s\\t%%Y\\t%%T@\\n'", dirPath),
		WorkDir: "/workspace",
	})
	if err != nil {
		return nil, err
	}

	var files []runtime.FileInfo
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 || parts[0] == "." {
			continue
		}

		var size int64
		fmt.Sscanf(parts[1], "%d", &size)
		isDir := parts[2] == "d"

		var modTime int64
		fmt.Sscanf(parts[3], "%d", &modTime)

		files = append(files, runtime.FileInfo{
			Name:    parts[0],
			Path:    dirPath + "/" + parts[0],
			Size:    size,
			IsDir:   isDir,
			ModTime: modTime,
		})
	}

	return files, nil
}
```

- [ ] **Step 5: Verify it compiles**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build ./internal/runtime/docker/`
Expected: No errors

- [ ] **Step 6: Commit**

```bash
git add internal/runtime/docker/ internal/runtime/docker/
git commit -m "feat: add Docker runtime implementation (container, exec, file, network)"
```

---

## Phase 4: Sandbox Manager (Core Business Logic)

### Task 9: Container Pool

**Files:**
- Create: `internal/sandbox/pool.go`
- Create: `internal/sandbox/pool_test.go`

- [ ] **Step 1: Write pool tests**

Create `internal/sandbox/pool_test.go`:

```go
package sandbox

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/runtime"
)

// mockRuntime is a simple mock for testing pool logic.
type mockRuntime struct {
	mu         sync.Mutex
	created    int
	removed    int
	sandboxes  map[string]*runtime.SandboxInfo
}

func newMockRuntime() *mockRuntime {
	return &mockRuntime{sandboxes: make(map[string]*runtime.SandboxInfo)}
}

func (m *mockRuntime) CreateSandbox(_ context.Context, spec runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created++
	info := &runtime.SandboxInfo{
		ID:        spec.ID,
		RuntimeID: "container-" + spec.ID,
		State:     "running",
		CreatedAt: time.Now(),
	}
	m.sandboxes[spec.ID] = info
	return info, nil
}

func (m *mockRuntime) StartSandbox(_ context.Context, _ string) error { return nil }
func (m *mockRuntime) StopSandbox(_ context.Context, _ string) error  { return nil }

func (m *mockRuntime) RemoveSandbox(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removed++
	delete(m.sandboxes, id)
	return nil
}

func (m *mockRuntime) GetSandbox(_ context.Context, id string) (*runtime.SandboxInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	info, ok := m.sandboxes[id]
	if !ok {
		return nil, nil
	}
	return info, nil
}

func (m *mockRuntime) Exec(context.Context, string, runtime.ExecRequest) (*runtime.ExecResult, error) {
	return &runtime.ExecResult{}, nil
}

func (m *mockRuntime) ExecStream(context.Context, string, runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	return nil, nil
}

func (m *mockRuntime) UploadFile(context.Context, string, string, interface{}) error { return nil }
func (m *mockRuntime) DownloadFile(context.Context, string, string) (interface{}, error) {
	return nil, nil
}
func (m *mockRuntime) ListFiles(context.Context, string, string) ([]runtime.FileInfo, error) {
	return nil, nil
}

func TestPool_Acquire(t *testing.T) {
	rt := newMockRuntime()
	pool := NewPool(rt, PoolConfig{
		Language: LangPython,
		MinSize:  2,
		MaxSize:  10,
		Image:    "sandbox-python:latest",
	})

	ctx := context.Background()

	// Warm up pool
	pool.WarmUp(ctx)
	time.Sleep(100 * time.Millisecond) // let async creation finish

	assert.Equal(t, 2, pool.Size())

	// Acquire one
	info, err := pool.Acquire(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, info.ID)
	assert.Equal(t, 1, pool.Size())
}

func TestPool_Release(t *testing.T) {
	rt := newMockRuntime()
	pool := NewPool(rt, PoolConfig{
		Language: LangPython,
		MinSize:  2,
		MaxSize:  10,
		Image:    "sandbox-python:latest",
	})

	ctx := context.Background()
	pool.WarmUp(ctx)
	time.Sleep(100 * time.Millisecond)

	info, err := pool.Acquire(ctx)
	require.NoError(t, err)

	// Release should destroy (not return to pool — used containers are dirty)
	pool.Release(ctx, info.ID)

	rt.mu.Lock()
	assert.GreaterOrEqual(t, rt.removed, 1)
	rt.mu.Unlock()
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/sandbox/ -v -run TestPool`
Expected: FAIL — NewPool not defined

- [ ] **Step 3: Implement Pool**

Create `internal/sandbox/pool.go`:

```go
package sandbox

import (
	"context"
	"fmt"
	"sync"

	"github.com/goairix/sandbox/internal/runtime"
)

// PoolConfig configures a container pool for a specific language.
type PoolConfig struct {
	Language Language
	MinSize  int
	MaxSize  int
	Image    string
}

// Pool manages a pool of warm containers for a specific language.
type Pool struct {
	runtime runtime.Runtime
	config  PoolConfig

	mu        sync.Mutex
	available []*runtime.SandboxInfo
	counter   int
}

// NewPool creates a new container pool.
func NewPool(rt runtime.Runtime, cfg PoolConfig) *Pool {
	return &Pool{
		runtime: rt,
		config:  cfg,
	}
}

// WarmUp fills the pool to MinSize.
func (p *Pool) WarmUp(ctx context.Context) {
	p.mu.Lock()
	need := p.config.MinSize - len(p.available)
	p.mu.Unlock()

	for i := 0; i < need; i++ {
		info, err := p.createWarm(ctx)
		if err != nil {
			continue
		}
		p.mu.Lock()
		p.available = append(p.available, info)
		p.mu.Unlock()
	}
}

// Acquire takes a warm container from the pool. If none available, creates one on-demand.
func (p *Pool) Acquire(ctx context.Context) (*runtime.SandboxInfo, error) {
	p.mu.Lock()
	if len(p.available) > 0 {
		info := p.available[0]
		p.available = p.available[1:]
		p.mu.Unlock()

		// Trigger async refill if below min
		go p.refillIfNeeded(context.Background())

		return info, nil
	}
	p.mu.Unlock()

	// No warm containers, create on-demand
	return p.createWarm(ctx)
}

// Release destroys a used container (containers are single-use for security).
func (p *Pool) Release(ctx context.Context, id string) {
	_ = p.runtime.RemoveSandbox(ctx, id)

	// Trigger async refill
	go p.refillIfNeeded(context.Background())
}

// Size returns the number of available warm containers.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.available)
}

// Drain destroys all warm containers in the pool.
func (p *Pool) Drain(ctx context.Context) {
	p.mu.Lock()
	items := make([]*runtime.SandboxInfo, len(p.available))
	copy(items, p.available)
	p.available = nil
	p.mu.Unlock()

	for _, info := range items {
		_ = p.runtime.RemoveSandbox(ctx, info.ID)
	}
}

func (p *Pool) createWarm(ctx context.Context) (*runtime.SandboxInfo, error) {
	p.mu.Lock()
	p.counter++
	id := fmt.Sprintf("pool-%s-%d", p.config.Language, p.counter)
	p.mu.Unlock()

	spec := runtime.SandboxSpec{
		ID:    id,
		Image: p.config.Image,
		Labels: map[string]string{
			"sandbox.pool":     "true",
			"sandbox.language": string(p.config.Language),
		},
		ReadOnlyRootFS: false, // warm containers need writable FS for dependency install
		RunAsUser:      1000,
		PidLimit:       100,
	}

	return p.runtime.CreateSandbox(ctx, spec)
}

func (p *Pool) refillIfNeeded(ctx context.Context) {
	p.mu.Lock()
	need := p.config.MinSize - len(p.available)
	if need <= 0 {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	for i := 0; i < need; i++ {
		info, err := p.createWarm(ctx)
		if err != nil {
			continue
		}
		p.mu.Lock()
		if len(p.available) < p.config.MaxSize {
			p.available = append(p.available, info)
		} else {
			// Pool is full, discard
			go func() { _ = p.runtime.RemoveSandbox(context.Background(), info.ID) }()
		}
		p.mu.Unlock()
	}
}
```

- [ ] **Step 4: Fix mock to match interface and run tests**

The mock needs to match the actual `runtime.Runtime` interface. Update the test mock imports and signatures to use `io.Reader` and `io.ReadCloser`. Replace the mock's `UploadFile` and `DownloadFile` signatures:

```go
func (m *mockRuntime) UploadFile(_ context.Context, _ string, _ string, _ io.Reader) error {
	return nil
}
func (m *mockRuntime) DownloadFile(_ context.Context, _ string, _ string) (io.ReadCloser, error) {
	return nil, nil
}
```

Add `import "io"` to the test file.

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/sandbox/ -v -run TestPool`
Expected: Both tests PASS

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/pool.go internal/sandbox/pool_test.go
git commit -m "feat: add container pool with warm-up, acquire, release, and auto-refill"
```

---

### Task 10: Sandbox Manager

**Files:**
- Create: `internal/sandbox/manager.go`
- Create: `internal/sandbox/manager_test.go`

- [ ] **Step 1: Write Manager tests**

Create `internal/sandbox/manager_test.go`:

```go
package sandbox

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager_CreateEphemeralSandbox(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, ManagerConfig{
		PoolConfigs: map[Language]PoolConfig{
			LangPython: {Language: LangPython, MinSize: 2, MaxSize: 10, Image: "sandbox-python:latest"},
		},
		DefaultTimeout: 30,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Language: LangPython,
		Mode:     ModeEphemeral,
		Timeout:  30,
	})
	require.NoError(t, err)
	assert.Equal(t, StateReady, sb.State)
	assert.Equal(t, LangPython, sb.Config.Language)
	assert.Equal(t, ModeEphemeral, sb.Config.Mode)
}

func TestManager_CreatePersistentSandbox(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, ManagerConfig{
		PoolConfigs: map[Language]PoolConfig{
			LangNodeJS: {Language: LangNodeJS, MinSize: 1, MaxSize: 5, Image: "sandbox-nodejs:latest"},
		},
		DefaultTimeout: 60,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Language: LangNodeJS,
		Mode:     ModePersistent,
		Timeout:  60,
	})
	require.NoError(t, err)
	assert.Equal(t, StateReady, sb.State)
	assert.Equal(t, ModePersistent, sb.Config.Mode)

	// Should be retrievable by ID
	got, err := mgr.Get(ctx, sb.ID)
	require.NoError(t, err)
	assert.Equal(t, sb.ID, got.ID)
}

func TestManager_Destroy(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, ManagerConfig{
		PoolConfigs: map[Language]PoolConfig{
			LangBash: {Language: LangBash, MinSize: 1, MaxSize: 5, Image: "sandbox-bash:latest"},
		},
		DefaultTimeout: 30,
	})

	ctx := context.Background()
	mgr.Start(ctx)
	defer mgr.Stop(ctx)

	time.Sleep(100 * time.Millisecond)

	sb, err := mgr.Create(ctx, SandboxConfig{
		Language: LangBash,
		Mode:     ModeEphemeral,
	})
	require.NoError(t, err)

	err = mgr.Destroy(ctx, sb.ID)
	require.NoError(t, err)

	_, err = mgr.Get(ctx, sb.ID)
	assert.Error(t, err)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/sandbox/ -v -run TestManager`
Expected: FAIL — NewManager not defined

- [ ] **Step 3: Implement Manager**

Create `internal/sandbox/manager.go`:

```go
package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
)

// ManagerConfig configures the SandboxManager.
type ManagerConfig struct {
	PoolConfigs    map[Language]PoolConfig
	DefaultTimeout int // seconds
}

// Manager orchestrates sandbox lifecycle: creation, execution, destruction.
type Manager struct {
	runtime runtime.Runtime
	config  ManagerConfig

	pools     map[Language]*Pool
	sandboxes map[string]*Sandbox
	mu        sync.RWMutex
	counter   int
}

// NewManager creates a new SandboxManager.
func NewManager(rt runtime.Runtime, cfg ManagerConfig) *Manager {
	pools := make(map[Language]*Pool)
	for lang, pcfg := range cfg.PoolConfigs {
		pools[lang] = NewPool(rt, pcfg)
	}

	return &Manager{
		runtime:   rt,
		config:    cfg,
		pools:     pools,
		sandboxes: make(map[string]*Sandbox),
	}
}

// Start initializes the manager and warms up pools.
func (m *Manager) Start(ctx context.Context) {
	for _, pool := range m.pools {
		pool.WarmUp(ctx)
	}
}

// Stop drains all pools and cleans up.
func (m *Manager) Stop(ctx context.Context) {
	for _, pool := range m.pools {
		pool.Drain(ctx)
	}
}

// Create creates a new sandbox.
func (m *Manager) Create(ctx context.Context, cfg SandboxConfig) (*Sandbox, error) {
	pool, ok := m.pools[cfg.Language]
	if !ok {
		return nil, fmt.Errorf("unsupported language: %s", cfg.Language)
	}

	// Acquire a warm container from the pool
	info, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire container: %w", err)
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = m.config.DefaultTimeout
	}

	m.mu.Lock()
	m.counter++
	id := fmt.Sprintf("sb-%d-%d", time.Now().Unix(), m.counter)
	m.mu.Unlock()

	sb := &Sandbox{
		ID:        id,
		Config:    cfg,
		State:     StateReady,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		RuntimeID: info.RuntimeID,
	}

	m.mu.Lock()
	m.sandboxes[id] = sb
	m.mu.Unlock()

	return sb, nil
}

// Get retrieves a sandbox by ID.
func (m *Manager) Get(_ context.Context, id string) (*Sandbox, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	sb, ok := m.sandboxes[id]
	if !ok {
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
	return sb, nil
}

// Destroy removes a sandbox.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox not found: %s", id)
	}
	sb.State = StateDestroying
	delete(m.sandboxes, id)
	m.mu.Unlock()

	// Remove the container
	if err := m.runtime.RemoveSandbox(ctx, sb.RuntimeID); err != nil {
		return fmt.Errorf("remove sandbox: %w", err)
	}

	return nil
}

// Exec executes a command in a sandbox synchronously.
func (m *Manager) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
	m.mu.RUnlock()

	m.mu.Lock()
	sb.State = StateRunning
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	result, err := m.runtime.Exec(ctx, sb.RuntimeID, req)

	m.mu.Lock()
	sb.State = StateIdle
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	return result, err
}

// ExecStream executes a command in a sandbox with streaming output.
func (m *Manager) ExecStream(ctx context.Context, id string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
	m.mu.RUnlock()

	m.mu.Lock()
	sb.State = StateRunning
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	ch, err := m.runtime.ExecStream(ctx, sb.RuntimeID, req)
	if err != nil {
		m.mu.Lock()
		sb.State = StateIdle
		m.mu.Unlock()
		return nil, err
	}

	// Wrap channel to update state on completion
	outCh := make(chan runtime.StreamEvent, 64)
	go func() {
		defer close(outCh)
		for event := range ch {
			outCh <- event
		}
		m.mu.Lock()
		sb.State = StateIdle
		sb.UpdatedAt = time.Now()
		m.mu.Unlock()
	}()

	return outCh, nil
}
```

- [ ] **Step 4: Run tests**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/sandbox/ -v -run TestManager`
Expected: All 3 tests PASS

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/manager.go internal/sandbox/manager_test.go
git commit -m "feat: add SandboxManager with lifecycle, exec, pool integration"
```

---

## Phase 5: API Layer

### Task 11: HTTP Server & Router

**Files:**
- Create: `internal/api/server.go`
- Create: `internal/api/router.go`
- Create: `internal/api/middleware/auth.go`
- Create: `internal/api/middleware/ratelimit.go`

- [ ] **Step 1: Install Gin**

Run:
```bash
cd /Users/dysodeng/project/go/cloud/sandbox
go get github.com/gin-gonic/gin
```

- [ ] **Step 2: Create middleware**

Create `internal/api/middleware/auth.go`:

```go
package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Auth returns a middleware that validates API key from Authorization header.
// If apiKey is empty, authentication is disabled.
func Auth(apiKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if apiKey == "" {
			c.Next()
			return
		}

		auth := c.GetHeader("Authorization")
		if auth == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":   "unauthorized",
				"message": "missing Authorization header",
			})
			return
		}

		token := strings.TrimPrefix(auth, "Bearer ")
		if token != apiKey {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error":   "unauthorized",
				"message": "invalid API key",
			})
			return
		}

		c.Next()
	}
}
```

Create `internal/api/middleware/ratelimit.go`:

```go
package middleware

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// RateLimit returns a simple token-bucket rate limiter middleware.
func RateLimit(requestsPerSecond int) gin.HandlerFunc {
	type client struct {
		tokens   float64
		lastSeen time.Time
	}

	var mu sync.Mutex
	clients := make(map[string]*client)
	maxTokens := float64(requestsPerSecond)

	return func(c *gin.Context) {
		ip := c.ClientIP()

		mu.Lock()
		cl, ok := clients[ip]
		if !ok {
			cl = &client{tokens: maxTokens, lastSeen: time.Now()}
			clients[ip] = cl
		}

		elapsed := time.Since(cl.lastSeen).Seconds()
		cl.tokens += elapsed * float64(requestsPerSecond)
		if cl.tokens > maxTokens {
			cl.tokens = maxTokens
		}
		cl.lastSeen = time.Now()

		if cl.tokens < 1 {
			mu.Unlock()
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"error":   "rate_limit_exceeded",
				"message": "too many requests",
			})
			return
		}

		cl.tokens--
		mu.Unlock()

		c.Next()
	}
}
```

- [ ] **Step 3: Create router**

Create `internal/api/router.go`:

```go
package api

import (
	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/api/middleware"
)

// SetupRouter configures all routes.
func SetupRouter(h *handler.Handler, apiKey string, rateLimit int) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery())

	// Health check (no auth)
	r.GET("/health", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	v1 := r.Group("/api/v1")
	v1.Use(middleware.Auth(apiKey))
	if rateLimit > 0 {
		v1.Use(middleware.RateLimit(rateLimit))
	}

	// Sandbox management
	v1.POST("/sandboxes", h.CreateSandbox)
	v1.GET("/sandboxes/:id", h.GetSandbox)
	v1.DELETE("/sandboxes/:id", h.DestroySandbox)

	// Execution within a sandbox
	v1.POST("/sandboxes/:id/exec", h.ExecSync)
	v1.POST("/sandboxes/:id/exec/stream", h.ExecStream)

	// File operations
	v1.POST("/sandboxes/:id/files/upload", h.UploadFile)
	v1.GET("/sandboxes/:id/files/download", h.DownloadFile)
	v1.GET("/sandboxes/:id/files/list", h.ListFiles)

	// One-shot execution
	v1.POST("/execute", h.ExecuteOneShot)
	v1.POST("/execute/stream", h.ExecuteOneShotStream)

	return r
}
```

- [ ] **Step 4: Create server**

Create `internal/api/server.go`:

```go
package api

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// Server wraps the HTTP server with graceful shutdown.
type Server struct {
	engine     *gin.Engine
	httpServer *http.Server
}

// NewServer creates a new API server.
func NewServer(engine *gin.Engine, host string, port int) *Server {
	return &Server{
		engine: engine,
		httpServer: &http.Server{
			Addr:    fmt.Sprintf("%s:%d", host, port),
			Handler: engine,
		},
	}
}

// Start starts the HTTP server.
func (s *Server) Start() error {
	return s.httpServer.ListenAndServe()
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(shutdownCtx)
}
```

- [ ] **Step 5: Verify compilation (will fail on handler import — that's expected, handlers next task)**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build ./internal/api/middleware/ && go build ./internal/api/`
Expected: middleware compiles; api/ may fail due to missing handler package (OK for now)

- [ ] **Step 6: Commit middleware and server**

```bash
git add internal/api/server.go internal/api/router.go internal/api/middleware/
git commit -m "feat: add HTTP server, router, auth and rate-limit middleware"
```

---

### Task 12: API Handlers

**Files:**
- Create: `internal/api/handler/handler.go`
- Create: `internal/api/handler/sandbox.go`
- Create: `internal/api/handler/exec.go`
- Create: `internal/api/handler/file.go`
- Create: `internal/api/handler/execute.go`

- [ ] **Step 1: Create handler base**

Create `internal/api/handler/handler.go`:

```go
package handler

import (
	"github.com/goairix/sandbox/internal/sandbox"
)

// Handler holds shared dependencies for all HTTP handlers.
type Handler struct {
	manager *sandbox.Manager
}

// NewHandler creates a new Handler.
func NewHandler(mgr *sandbox.Manager) *Handler {
	return &Handler{manager: mgr}
}
```

- [ ] **Step 2: Create sandbox handler**

Create `internal/api/handler/sandbox.go`:

```go
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/pkg/types"
)

func (h *Handler) CreateSandbox(c *gin.Context) {
	var req types.CreateSandboxRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Error:   "invalid_request",
			Message: err.Error(),
		})
		return
	}

	cfg := sandbox.SandboxConfig{
		Language: sandbox.Language(req.Language),
		Mode:     sandbox.Mode(req.Mode),
		Timeout:  req.Timeout,
	}

	if req.Resources != nil {
		cfg.Resources = sandbox.ResourceLimits{
			Memory: req.Resources.Memory,
			CPU:    req.Resources.CPU,
			Disk:   req.Resources.Disk,
		}
	}

	if req.Network != nil {
		cfg.Network = sandbox.NetworkConfig{
			Enabled:   req.Network.Enabled,
			Whitelist: req.Network.Whitelist,
		}
	}

	for _, dep := range req.Dependencies {
		cfg.Dependencies = append(cfg.Dependencies, sandbox.Dependency{
			Name:    dep.Name,
			Version: dep.Version,
		})
	}

	sb, err := h.manager.Create(c.Request.Context(), cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "create_failed",
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusCreated, types.SandboxResponse{
		ID:        sb.ID,
		Language:  string(sb.Config.Language),
		Mode:      string(sb.Config.Mode),
		State:     string(sb.State),
		CreatedAt: sb.CreatedAt,
	})
}

func (h *Handler) GetSandbox(c *gin.Context) {
	id := c.Param("id")

	sb, err := h.manager.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{
			Error:   "not_found",
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, types.SandboxResponse{
		ID:        sb.ID,
		Language:  string(sb.Config.Language),
		Mode:      string(sb.Config.Mode),
		State:     string(sb.State),
		CreatedAt: sb.CreatedAt,
	})
}

func (h *Handler) DestroySandbox(c *gin.Context) {
	id := c.Param("id")

	if err := h.manager.Destroy(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{
			Error:   "not_found",
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "sandbox destroyed"})
}
```

- [ ] **Step 3: Create exec handler**

Create `internal/api/handler/exec.go`:

```go
package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/pkg/types"
)

func (h *Handler) ExecSync(c *gin.Context) {
	id := c.Param("id")

	var req types.ExecRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Error:   "invalid_request",
			Message: err.Error(),
		})
		return
	}

	execReq := runtime.ExecRequest{
		Command: req.Command,
		Stdin:   req.Stdin,
		Timeout: req.Timeout,
		Env:     req.Env,
	}

	result, err := h.manager.Exec(c.Request.Context(), id, execReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "exec_failed",
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, types.ExecResponse{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		Duration: result.Duration.Seconds(),
	})
}

func (h *Handler) ExecStream(c *gin.Context) {
	id := c.Param("id")

	var req types.ExecRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Error:   "invalid_request",
			Message: err.Error(),
		})
		return
	}

	execReq := runtime.ExecRequest{
		Command: req.Command,
		Stdin:   req.Stdin,
		Timeout: req.Timeout,
		Env:     req.Env,
	}

	ch, err := h.manager.ExecStream(c.Request.Context(), id, execReq)
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "exec_failed",
			Message: err.Error(),
		})
		return
	}

	// SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	start := time.Now()
	c.Stream(func(w io.Writer) bool {
		event, ok := <-ch
		if !ok {
			return false
		}

		var eventType string
		var data any

		switch event.Type {
		case runtime.StreamStdout:
			eventType = "stdout"
			data = types.SSEStdoutData{Content: event.Content}
		case runtime.StreamStderr:
			eventType = "stderr"
			data = types.SSEStderrData{Content: event.Content}
		case runtime.StreamDone:
			eventType = "done"
			exitCode, _ := strconv.Atoi(event.Content)
			data = types.SSEDoneData{
				ExitCode: exitCode,
				Elapsed:  time.Since(start).Seconds(),
			}
		case runtime.StreamError:
			eventType = "error"
			data = types.SSEErrorData{
				Error:   "exec_error",
				Message: event.Content,
			}
		}

		jsonData, _ := json.Marshal(data)
		c.SSEvent(eventType, string(jsonData))
		return true
	})
}
```

Note: add `"io"` to the import block in exec.go.

Actually, `c.Stream` uses `func(w io.Writer) bool`. Let me fix: Gin's `c.Stream` signature is `func(step func(w io.Writer) bool)`. But we also need `c.SSEvent`. Let me use the proper Gin SSE approach:

Replace the ExecStream's streaming section:

```go
	// SSE headers
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	start := time.Now()
	flusher, _ := c.Writer.(http.Flusher)

	for event := range ch {
		var eventType string
		var data any

		switch event.Type {
		case runtime.StreamStdout:
			eventType = "stdout"
			data = types.SSEStdoutData{Content: event.Content}
		case runtime.StreamStderr:
			eventType = "stderr"
			data = types.SSEStderrData{Content: event.Content}
		case runtime.StreamDone:
			eventType = "done"
			exitCode, _ := strconv.Atoi(event.Content)
			data = types.SSEDoneData{
				ExitCode: exitCode,
				Elapsed:  time.Since(start).Seconds(),
			}
		case runtime.StreamError:
			eventType = "error"
			data = types.SSEErrorData{
				Error:   "exec_error",
				Message: event.Content,
			}
		}

		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, jsonData)
		if flusher != nil {
			flusher.Flush()
		}
	}
```

- [ ] **Step 4: Create file handler**

Create `internal/api/handler/file.go`:

```go
package handler

import (
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/pkg/types"
)

func (h *Handler) UploadFile(c *gin.Context) {
	id := c.Param("id")
	destPath := c.DefaultPostForm("path", "/workspace/")

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Error:   "invalid_request",
			Message: "file is required",
		})
		return
	}
	defer file.Close()

	sb, err := h.manager.Get(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{
			Error:   "not_found",
			Message: err.Error(),
		})
		return
	}
	_ = sb // used to confirm sandbox exists

	fullPath := destPath
	if fullPath[len(fullPath)-1] == '/' {
		fullPath += header.Filename
	}

	if err := h.manager.UploadFile(c.Request.Context(), id, fullPath, file); err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "upload_failed",
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, types.FileUploadResponse{
		Path: fullPath,
		Size: header.Size,
	})
}

func (h *Handler) DownloadFile(c *gin.Context) {
	id := c.Param("id")
	path := c.Query("path")
	if path == "" {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Error:   "invalid_request",
			Message: "path is required",
		})
		return
	}

	reader, err := h.manager.DownloadFile(c.Request.Context(), id, path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "download_failed",
			Message: err.Error(),
		})
		return
	}
	defer reader.Close()

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", path))
	c.Header("Content-Type", "application/octet-stream")
	io.Copy(c.Writer, reader)
}

func (h *Handler) ListFiles(c *gin.Context) {
	id := c.Param("id")
	dir := c.DefaultQuery("path", "/workspace")

	files, err := h.manager.ListFiles(c.Request.Context(), id, dir)
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "list_failed",
			Message: err.Error(),
		})
		return
	}

	var fileInfos []types.FileInfo
	for _, f := range files {
		fileInfos = append(fileInfos, types.FileInfo{
			Name:  f.Name,
			Path:  f.Path,
			Size:  f.Size,
			IsDir: f.IsDir,
		})
	}

	c.JSON(http.StatusOK, types.FileListResponse{
		Files: fileInfos,
		Path:  dir,
	})
}
```

- [ ] **Step 5: Create one-shot execute handler**

Create `internal/api/handler/execute.go`:

```go
package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/sandbox"
	"github.com/goairix/sandbox/pkg/types"
)

func (h *Handler) ExecuteOneShot(c *gin.Context) {
	var req types.ExecuteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Error:   "invalid_request",
			Message: err.Error(),
		})
		return
	}

	ctx := c.Request.Context()

	// Create ephemeral sandbox
	cfg := sandbox.SandboxConfig{
		Language: sandbox.Language(req.Language),
		Mode:     sandbox.ModeEphemeral,
		Timeout:  req.Timeout,
	}
	if req.Resources != nil {
		cfg.Resources = sandbox.ResourceLimits{
			Memory: req.Resources.Memory,
			CPU:    req.Resources.CPU,
			Disk:   req.Resources.Disk,
		}
	}

	sb, err := h.manager.Create(ctx, cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "create_failed",
			Message: err.Error(),
		})
		return
	}
	defer h.manager.Destroy(ctx, sb.ID)

	// Execute
	result, err := h.manager.Exec(ctx, sb.ID, runtime.ExecRequest{
		Command: req.Command,
		Stdin:   req.Stdin,
		Timeout: req.Timeout,
		Env:     req.Env,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "exec_failed",
			Message: err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, types.ExecResponse{
		ExitCode: result.ExitCode,
		Stdout:   result.Stdout,
		Stderr:   result.Stderr,
		Duration: result.Duration.Seconds(),
	})
}

func (h *Handler) ExecuteOneShotStream(c *gin.Context) {
	var req types.ExecuteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{
			Error:   "invalid_request",
			Message: err.Error(),
		})
		return
	}

	ctx := c.Request.Context()

	cfg := sandbox.SandboxConfig{
		Language: sandbox.Language(req.Language),
		Mode:     sandbox.ModeEphemeral,
		Timeout:  req.Timeout,
	}

	sb, err := h.manager.Create(ctx, cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "create_failed",
			Message: err.Error(),
		})
		return
	}
	defer h.manager.Destroy(ctx, sb.ID)

	ch, err := h.manager.ExecStream(ctx, sb.ID, runtime.ExecRequest{
		Command: req.Command,
		Stdin:   req.Stdin,
		Timeout: req.Timeout,
		Env:     req.Env,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{
			Error:   "exec_failed",
			Message: err.Error(),
		})
		return
	}

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	start := time.Now()
	flusher, _ := c.Writer.(http.Flusher)

	for event := range ch {
		var eventType string
		var data any

		switch event.Type {
		case runtime.StreamStdout:
			eventType = "stdout"
			data = types.SSEStdoutData{Content: event.Content}
		case runtime.StreamStderr:
			eventType = "stderr"
			data = types.SSEStderrData{Content: event.Content}
		case runtime.StreamDone:
			eventType = "done"
			exitCode, _ := strconv.Atoi(event.Content)
			data = types.SSEDoneData{
				ExitCode: exitCode,
				Elapsed:  time.Since(start).Seconds(),
			}
		case runtime.StreamError:
			eventType = "error"
			data = types.SSEErrorData{
				Error:   "exec_error",
				Message: event.Content,
			}
		}

		jsonData, _ := json.Marshal(data)
		fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", eventType, jsonData)
		if flusher != nil {
			flusher.Flush()
		}
	}
}
```

- [ ] **Step 6: Add file methods to Manager**

Add to `internal/sandbox/manager.go` (append these methods):

```go
// UploadFile uploads a file into a sandbox.
func (m *Manager) UploadFile(ctx context.Context, id string, destPath string, reader io.Reader) error {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("sandbox not found: %s", id)
	}
	m.mu.RUnlock()

	return m.runtime.UploadFile(ctx, sb.RuntimeID, destPath, reader)
}

// DownloadFile downloads a file from a sandbox.
func (m *Manager) DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
	m.mu.RUnlock()

	return m.runtime.DownloadFile(ctx, sb.RuntimeID, srcPath)
}

// ListFiles lists files in a sandbox directory.
func (m *Manager) ListFiles(ctx context.Context, id string, dirPath string) ([]runtime.FileInfo, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
	m.mu.RUnlock()

	return m.runtime.ListFiles(ctx, sb.RuntimeID, dirPath)
}
```

Add `"io"` to the imports in manager.go.

- [ ] **Step 7: Verify full API compiles**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build ./internal/api/...`
Expected: No errors

- [ ] **Step 8: Commit**

```bash
git add internal/api/handler/ internal/sandbox/manager.go
git commit -m "feat: add API handlers for sandbox, exec, file, and one-shot execution"
```

---

### Task 13: Application Entry Point

**Files:**
- Create: `cmd/sandbox/main.go`

- [ ] **Step 1: Create main.go**

Create `cmd/sandbox/main.go`:

```go
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/goairix/sandbox/internal/api"
	"github.com/goairix/sandbox/internal/api/handler"
	"github.com/goairix/sandbox/internal/config"
	"github.com/goairix/sandbox/internal/runtime/docker"
	"github.com/goairix/sandbox/internal/sandbox"
)

func main() {
	configPath := flag.String("config", "", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Initialize runtime
	var rt sandbox.RuntimeForManager
	switch cfg.Runtime.Type {
	case "docker":
		rt, err = docker.New(ctx, cfg.Runtime.Docker.Host)
		if err != nil {
			log.Fatalf("failed to create docker runtime: %v", err)
		}
	case "kubernetes":
		log.Fatal("kubernetes runtime not yet implemented")
	default:
		log.Fatalf("unknown runtime type: %s", cfg.Runtime.Type)
	}

	// Build pool configs
	poolConfigs := map[sandbox.Language]sandbox.PoolConfig{
		sandbox.LangPython: {
			Language: sandbox.LangPython,
			MinSize:  cfg.Pool.MinSize,
			MaxSize:  cfg.Pool.MaxSize,
			Image:    "sandbox-python:latest",
		},
		sandbox.LangNodeJS: {
			Language: sandbox.LangNodeJS,
			MinSize:  cfg.Pool.MinSize,
			MaxSize:  cfg.Pool.MaxSize,
			Image:    "sandbox-nodejs:latest",
		},
		sandbox.LangBash: {
			Language: sandbox.LangBash,
			MinSize:  cfg.Pool.MinSize,
			MaxSize:  cfg.Pool.MaxSize,
			Image:    "sandbox-bash:latest",
		},
	}

	mgr := sandbox.NewManager(rt, sandbox.ManagerConfig{
		PoolConfigs:    poolConfigs,
		DefaultTimeout: cfg.Security.ExecTimeout,
	})
	mgr.Start(ctx)

	h := handler.NewHandler(mgr)
	router := api.SetupRouter(h, "", 0)
	server := api.NewServer(router, cfg.Server.Host, cfg.Server.Port)

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh
		log.Println("shutting down...")
		cancel()
		mgr.Stop(context.Background())
		if err := server.Stop(context.Background()); err != nil {
			log.Printf("server shutdown error: %v", err)
		}
	}()

	log.Printf("starting sandbox API server on %s:%d", cfg.Server.Host, cfg.Server.Port)
	if err := server.Start(); err != nil {
		log.Printf("server stopped: %v", err)
	}
}
```

Note: The `sandbox.RuntimeForManager` reference above is wrong — Manager takes `runtime.Runtime` directly. The import and type need adjustment. The actual main.go should cast docker.Runtime (which implements runtime.Runtime) directly:

```go
	var rt runtime.Runtime
```

And import `"github.com/goairix/sandbox/internal/runtime"`.

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build ./cmd/sandbox/`
Expected: Binary produced (or compile errors to fix)

- [ ] **Step 3: Commit**

```bash
git add cmd/sandbox/main.go
git commit -m "feat: add application entry point with config, runtime, and server setup"
```

---

## Phase 6: Docker Images & Dev Environment

### Task 14: Sandbox Base Docker Images

**Files:**
- Create: `docker/images/python/Dockerfile`
- Create: `docker/images/nodejs/Dockerfile`
- Create: `docker/images/bash/Dockerfile`

- [ ] **Step 1: Create Python sandbox image**

Create `docker/images/python/Dockerfile`:

```dockerfile
FROM python:3.12-slim

# Create non-root user
RUN groupadd -r sandbox && useradd -r -g sandbox -u 1000 -m sandbox

# Install common Python libraries
RUN pip install --no-cache-dir \
    numpy==2.2.0 \
    pandas==2.2.3 \
    matplotlib==3.9.3 \
    requests==2.32.3 \
    Pillow==11.0.0 \
    python-pptx==1.0.2 \
    openpyxl==3.1.5 \
    scipy==1.14.1 \
    scikit-learn==1.5.2 \
    sympy==1.13.3 \
    beautifulsoup4==4.12.3

# Create workspace directory
RUN mkdir -p /workspace && chown sandbox:sandbox /workspace

WORKDIR /workspace
USER sandbox

CMD ["sleep", "infinity"]
```

- [ ] **Step 2: Create Node.js sandbox image**

Create `docker/images/nodejs/Dockerfile`:

```dockerfile
FROM node:20-slim

# Create non-root user
RUN groupadd -r sandbox && useradd -r -g sandbox -u 1000 -m sandbox

# Install TypeScript and common packages globally
RUN npm install -g \
    typescript@5.6.3 \
    ts-node@10.9.2 \
    tsx@4.19.2

# Pre-install common packages in a global node_modules
WORKDIR /opt/sandbox-libs
COPY <<'EOF' package.json
{
  "name": "sandbox-libs",
  "private": true,
  "dependencies": {
    "axios": "^1.7.0",
    "lodash": "^4.17.21",
    "sharp": "^0.33.0",
    "cheerio": "^1.0.0",
    "csv-parse": "^5.6.0",
    "exceljs": "^4.4.0",
    "zod": "^3.23.0"
  }
}
EOF
RUN npm install --production && chown -R sandbox:sandbox /opt/sandbox-libs
ENV NODE_PATH=/opt/sandbox-libs/node_modules

# Create workspace
RUN mkdir -p /workspace && chown sandbox:sandbox /workspace

WORKDIR /workspace
USER sandbox

CMD ["sleep", "infinity"]
```

- [ ] **Step 3: Create Bash sandbox image**

Create `docker/images/bash/Dockerfile`:

```dockerfile
FROM alpine:3.20

# Install common CLI tools
RUN apk add --no-cache \
    bash \
    curl \
    wget \
    jq \
    yq \
    git \
    openssh-client \
    zip \
    unzip \
    tar \
    gzip \
    findutils \
    coreutils \
    grep \
    sed \
    awk \
    imagemagick \
    ffmpeg \
    python3 \
    py3-pip

# Create non-root user
RUN addgroup -S sandbox && adduser -S -G sandbox -u 1000 sandbox

# Create workspace
RUN mkdir -p /workspace && chown sandbox:sandbox /workspace

WORKDIR /workspace
USER sandbox

CMD ["sleep", "infinity"]
```

- [ ] **Step 4: Commit**

```bash
git add docker/images/
git commit -m "feat: add sandbox base Docker images for Python, Node.js, and Bash"
```

---

### Task 15: Service Dockerfile & Docker Compose

**Files:**
- Create: `docker/Dockerfile`
- Create: `docker/docker-compose.yml`

- [ ] **Step 1: Create service Dockerfile**

Create `docker/Dockerfile`:

```dockerfile
FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /sandbox ./cmd/sandbox/

FROM alpine:3.20
RUN apk add --no-cache ca-certificates docker-cli
COPY --from=builder /sandbox /usr/local/bin/sandbox
COPY configs/config.yaml /etc/sandbox/config.yaml

EXPOSE 8080

ENTRYPOINT ["sandbox"]
CMD ["--config", "/etc/sandbox/config.yaml"]
```

- [ ] **Step 2: Create docker-compose.yml**

Create `docker/docker-compose.yml`:

```yaml
version: "3.8"

services:
  sandbox-api:
    build:
      context: ..
      dockerfile: docker/Dockerfile
    ports:
      - "8080:8080"
    environment:
      - SANDBOX_RUNTIME_TYPE=docker
      - SANDBOX_RUNTIME_DOCKER_HOST=unix:///var/run/docker.sock
      - SANDBOX_STORAGE_STATE_REDIS_ADDR=redis:6379
      - SANDBOX_STORAGE_OBJECT_PROVIDER=local
      - SANDBOX_STORAGE_OBJECT_LOCAL_PATH=/data/storage
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - sandbox-storage:/data/storage
    depends_on:
      - redis

  redis:
    image: redis:7-alpine
    ports:
      - "6379:6379"
    volumes:
      - redis-data:/data

volumes:
  sandbox-storage:
  redis-data:
```

- [ ] **Step 3: Commit**

```bash
git add docker/Dockerfile docker/docker-compose.yml
git commit -m "feat: add service Dockerfile and docker-compose for local development"
```

---

### Task 16: Seccomp Security Profile

**Files:**
- Create: `configs/seccomp-profile.json`

- [ ] **Step 1: Create seccomp profile**

Create `configs/seccomp-profile.json`:

```json
{
  "defaultAction": "SCMP_ACT_ERRNO",
  "architectures": ["SCMP_ARCH_X86_64", "SCMP_ARCH_AARCH64"],
  "syscalls": [
    {
      "names": [
        "accept", "accept4", "access", "arch_prctl", "bind", "brk",
        "capget", "capset", "chdir", "chmod", "chown", "clock_getres",
        "clock_gettime", "clock_nanosleep", "clone", "close", "connect",
        "copy_file_range", "creat", "dup", "dup2", "dup3", "epoll_create",
        "epoll_create1", "epoll_ctl", "epoll_pwait", "epoll_wait",
        "eventfd", "eventfd2", "execve", "execveat", "exit", "exit_group",
        "faccessat", "faccessat2", "fadvise64", "fallocate", "fchmod",
        "fchmodat", "fchown", "fchownat", "fcntl", "fdatasync",
        "flock", "fork", "fstat", "fstatfs", "fsync", "ftruncate",
        "futex", "getcwd", "getdents", "getdents64", "getegid",
        "geteuid", "getgid", "getgroups", "getpeername", "getpgid",
        "getpgrp", "getpid", "getppid", "getpriority", "getrandom",
        "getresgid", "getresuid", "getrlimit", "getrusage",
        "getsockname", "getsockopt", "gettid", "gettimeofday",
        "getuid", "ioctl", "kill", "lchown", "link", "linkat",
        "listen", "lseek", "lstat", "madvise", "memfd_create",
        "mincore", "mkdir", "mkdirat", "mmap", "mprotect", "mremap",
        "msgctl", "msgget", "msgrcv", "msgsnd", "msync", "munmap",
        "nanosleep", "newfstatat", "open", "openat", "pause", "pipe",
        "pipe2", "poll", "ppoll", "prctl", "pread64", "preadv",
        "prlimit64", "pwrite64", "pwritev", "read", "readahead",
        "readlink", "readlinkat", "readv", "recv", "recvfrom",
        "recvmmsg", "recvmsg", "rename", "renameat", "renameat2",
        "restart_syscall", "rmdir", "rt_sigaction", "rt_sigpending",
        "rt_sigprocmask", "rt_sigqueueinfo", "rt_sigreturn",
        "rt_sigsuspend", "rt_sigtimedwait", "sched_getaffinity",
        "sched_getattr", "sched_getparam", "sched_get_priority_max",
        "sched_get_priority_min", "sched_getscheduler",
        "sched_setaffinity", "sched_setattr", "sched_setparam",
        "sched_setscheduler", "sched_yield", "seccomp", "select",
        "semctl", "semget", "semop", "semtimedop", "send", "sendfile",
        "sendmmsg", "sendmsg", "sendto", "setgid", "setgroups",
        "setitimer", "setpgid", "setpriority", "setregid",
        "setresgid", "setresuid", "setreuid", "setsid", "setsockopt",
        "set_robust_list", "set_tid_address", "setuid", "shmat",
        "shmctl", "shmdt", "shmget", "shutdown", "sigaltstack",
        "signalfd", "signalfd4", "socket", "socketpair", "splice",
        "stat", "statfs", "statx", "symlink", "symlinkat", "sync",
        "sync_file_range", "sysinfo", "tee", "tgkill", "time",
        "timer_create", "timer_delete", "timer_getoverrun",
        "timer_gettime", "timer_settime", "timerfd_create",
        "timerfd_gettime", "timerfd_settime", "tkill", "truncate",
        "umask", "uname", "unlink", "unlinkat", "utime", "utimensat",
        "utimes", "vfork", "vmsplice", "wait4", "waitid", "waitpid",
        "write", "writev"
      ],
      "action": "SCMP_ACT_ALLOW"
    }
  ]
}
```

- [ ] **Step 2: Commit**

```bash
git add configs/seccomp-profile.json
git commit -m "feat: add seccomp security profile for sandbox containers"
```

---

## Phase 7: Kubernetes Runtime (Stub for future)

### Task 17: Kubernetes Runtime Stub

**Files:**
- Create: `internal/runtime/kubernetes/runtime.go`

- [ ] **Step 1: Create K8s runtime stub**

Create `internal/runtime/kubernetes/runtime.go`:

```go
package kubernetes

import (
	"context"
	"errors"
	"io"

	"github.com/goairix/sandbox/internal/runtime"
)

var errNotImplemented = errors.New("kubernetes runtime not yet implemented")

// Runtime implements runtime.Runtime using Kubernetes.
type Runtime struct {
	namespace string
}

// New creates a new Kubernetes runtime.
func New(kubeconfig string, namespace string) (*Runtime, error) {
	return &Runtime{namespace: namespace}, nil
}

func (r *Runtime) CreateSandbox(_ context.Context, _ runtime.SandboxSpec) (*runtime.SandboxInfo, error) {
	return nil, errNotImplemented
}

func (r *Runtime) StartSandbox(_ context.Context, _ string) error {
	return errNotImplemented
}

func (r *Runtime) StopSandbox(_ context.Context, _ string) error {
	return errNotImplemented
}

func (r *Runtime) RemoveSandbox(_ context.Context, _ string) error {
	return errNotImplemented
}

func (r *Runtime) GetSandbox(_ context.Context, _ string) (*runtime.SandboxInfo, error) {
	return nil, errNotImplemented
}

func (r *Runtime) Exec(_ context.Context, _ string, _ runtime.ExecRequest) (*runtime.ExecResult, error) {
	return nil, errNotImplemented
}

func (r *Runtime) ExecStream(_ context.Context, _ string, _ runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	return nil, errNotImplemented
}

func (r *Runtime) UploadFile(_ context.Context, _ string, _ string, _ io.Reader) error {
	return errNotImplemented
}

func (r *Runtime) DownloadFile(_ context.Context, _ string, _ string) (io.ReadCloser, error) {
	return nil, errNotImplemented
}

func (r *Runtime) ListFiles(_ context.Context, _ string, _ string) ([]runtime.FileInfo, error) {
	return nil, errNotImplemented
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build ./internal/runtime/kubernetes/`
Expected: No errors

- [ ] **Step 3: Commit**

```bash
git add internal/runtime/kubernetes/
git commit -m "feat: add Kubernetes runtime stub (interface compliance, not yet implemented)"
```

---

## Phase 8: Integration Verification

### Task 18: Full Build Verification

- [ ] **Step 1: Run go vet on entire project**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go vet ./...`
Expected: No errors

- [ ] **Step 2: Run all tests**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go test ./... -v -count=1`
Expected: All tests pass (Redis tests may skip if no Redis running)

- [ ] **Step 3: Build binary**

Run: `cd /Users/dysodeng/project/go/cloud/sandbox && go build -o sandbox ./cmd/sandbox/`
Expected: Binary produced

- [ ] **Step 4: Build sandbox Docker images**

Run:
```bash
cd /Users/dysodeng/project/go/cloud/sandbox
docker build -t sandbox-python:latest -f docker/images/python/Dockerfile docker/images/python/
docker build -t sandbox-nodejs:latest -f docker/images/nodejs/Dockerfile docker/images/nodejs/
docker build -t sandbox-bash:latest -f docker/images/bash/Dockerfile docker/images/bash/
```
Expected: All 3 images built

- [ ] **Step 5: Commit any fixes**

If any fixes were needed during verification, commit them:
```bash
git add -A
git commit -m "fix: resolve build and test issues from integration verification"
```
