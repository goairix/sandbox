# ScopedFS Workspace Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the `object.Store` storage layer with `github.com/goairix/fs` (`fs.FileSystem`) and integrate `ScopedFS` as a per-sandbox workspace with mount/unmount/sync capabilities.

**Architecture:** A shared `fs.FileSystem` instance (created from config at startup) replaces the old `object.Store`. Each sandbox can optionally mount a workspace via `ScopedFS`, which confines all file operations to a root path on the storage backend. Mounting syncs files from storage into the container `/workspace`; unmounting syncs back. Workspace lifecycle is independent of sandbox lifecycle — storage paths persist after sandbox destruction.

**Tech Stack:** Go 1.25, `github.com/goairix/fs` (v0.3.6), Gin HTTP framework, Docker/Kubernetes runtime, `testify` for testing.

---

## File Structure

| File | Action | Responsibility |
|------|--------|----------------|
| `internal/storage/object/` | **Delete** | Old object storage layer — replaced entirely |
| `internal/storage/filesystem.go` | **Create** | Factory: `NewFileSystem(cfg) (fs.FileSystem, error)` |
| `internal/storage/filesystem_test.go` | **Create** | Tests for `NewFileSystem` factory |
| `internal/storage/scoped_fs.go` | **Create** | `ScopedFS` interface + implementation (user-provided code) |
| `internal/storage/scoped_fs_test.go` | **Create** | Tests for `ScopedFS` path resolution and confinement |
| `internal/config/config.go` | **Modify** | `ObjectStorageConfig` → `FileSystemConfig` |
| `internal/sandbox/types.go` | **Modify** | Add `WorkspaceInfo`, `WorkspacePath` to sandbox types |
| `internal/sandbox/workspace.go` | **Create** | Mount/unmount/sync logic on Manager |
| `internal/sandbox/workspace_test.go` | **Create** | Tests for workspace operations |
| `internal/sandbox/manager.go` | **Modify** | Add `filesystem` field, `workspaces` map, update constructor/Create/Destroy |
| `pkg/types/workspace.go` | **Create** | Workspace API request/response types |
| `pkg/types/sandbox.go` | **Modify** | Add `WorkspacePath` to `CreateSandboxRequest` |
| `internal/api/handler/workspace.go` | **Create** | Workspace HTTP handlers |
| `internal/api/router.go` | **Modify** | Register workspace routes |
| `cmd/sandbox/main.go` | **Modify** | Initialize `fs.FileSystem`, pass to Manager |
| `go.mod` / `go.sum` | **Modify** | Add `github.com/goairix/fs` dependency |

---

### Task 1: Add `github.com/goairix/fs` dependency and delete old `object.Store`

**Files:**
- Delete: `internal/storage/object/` (entire directory)
- Modify: `go.mod`

- [ ] **Step 1: Delete the old object storage directory**

```bash
rm -rf internal/storage/object
```

- [ ] **Step 2: Add the `fs` dependency**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go get github.com/goairix/fs@latest
```

- [ ] **Step 3: Tidy modules to remove unused cloud SDK dependencies**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go mod tidy
```

- [ ] **Step 4: Verify the project compiles**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go build ./...
```

Expected: Build succeeds. The deleted `object` package was not imported by any upstream code, so no compile errors.

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "refactor(storage): remove object.Store, add github.com/goairix/fs dependency"
```

---

### Task 2: Update config — `ObjectStorageConfig` → `FileSystemConfig`

**Files:**
- Modify: `internal/config/config.go:59-87` (StorageConfig + ObjectStorageConfig)
- Modify: `internal/config/config.go:226-234` (setDefaults for storage.object)

- [ ] **Step 1: Write the failing test**

Create `internal/config/config_test.go`:

```go
package config

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_FileSystemConfig_Defaults(t *testing.T) {
	// Set required fields that Validate() checks
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")

	cfg, err := Load("")
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

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, "s3", cfg.Storage.FileSystem.Provider)
	assert.Equal(t, "my-bucket", cfg.Storage.FileSystem.Bucket)
	assert.Equal(t, "us-east-1", cfg.Storage.FileSystem.Region)
	assert.Equal(t, "workspaces", cfg.Storage.FileSystem.SubPath)
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/config/ -run TestLoad_FileSystemConfig -v
```

Expected: FAIL — `cfg.Storage.FileSystem` does not exist yet.

- [ ] **Step 3: Update `internal/config/config.go`**

Replace `StorageConfig` and `ObjectStorageConfig` (lines 59-87):

```go
// StorageConfig holds storage backend settings.
type StorageConfig struct {
	State      StateStorageConfig `mapstructure:"state"`
	FileSystem FileSystemConfig   `mapstructure:"filesystem"`
}
```

```go
// FileSystemConfig holds filesystem storage settings.
// Provider can be one of: local, s3, cos, oss, obs, minio.
type FileSystemConfig struct {
	Provider  string `mapstructure:"provider"`
	Bucket    string `mapstructure:"bucket"`
	Region    string `mapstructure:"region"`
	Endpoint  string `mapstructure:"endpoint"`
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
	LocalPath string `mapstructure:"local_path"`
	SubPath   string `mapstructure:"sub_path"`
	UseSSL    bool   `mapstructure:"use_ssl"`
}
```

Replace the `setDefaults` storage object section (lines 228-234) with:

```go
	// Storage — FileSystem
	v.SetDefault("storage.filesystem.provider", "local")
	v.SetDefault("storage.filesystem.bucket", "")
	v.SetDefault("storage.filesystem.region", "")
	v.SetDefault("storage.filesystem.endpoint", "")
	v.SetDefault("storage.filesystem.access_key", "")
	v.SetDefault("storage.filesystem.secret_key", "")
	v.SetDefault("storage.filesystem.local_path", "/tmp/sandbox-storage")
	v.SetDefault("storage.filesystem.sub_path", "")
	v.SetDefault("storage.filesystem.use_ssl", false)
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/config/ -run TestLoad_FileSystemConfig -v
```

Expected: PASS

- [ ] **Step 5: Verify full build**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go build ./...
```

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go && git commit -m "refactor(config): rename ObjectStorageConfig to FileSystemConfig"
```

---

### Task 3: Create `NewFileSystem` factory

**Files:**
- Create: `internal/storage/filesystem.go`
- Create: `internal/storage/filesystem_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/storage/filesystem_test.go`:

```go
package storage

import (
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFileSystem_Local(t *testing.T) {
	dir := t.TempDir()
	cfg := config.FileSystemConfig{
		Provider:  "local",
		LocalPath: dir,
	}

	fsys, err := NewFileSystem(cfg)
	require.NoError(t, err)
	assert.NotNil(t, fsys)
}

func TestNewFileSystem_LocalEmptyPath(t *testing.T) {
	cfg := config.FileSystemConfig{
		Provider:  "local",
		LocalPath: "",
	}

	_, err := NewFileSystem(cfg)
	assert.Error(t, err)
}

func TestNewFileSystem_UnknownProvider(t *testing.T) {
	cfg := config.FileSystemConfig{
		Provider: "unknown",
	}

	_, err := NewFileSystem(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported filesystem provider")
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/storage/ -run TestNewFileSystem -v
```

Expected: FAIL — `NewFileSystem` does not exist.

- [ ] **Step 3: Implement `internal/storage/filesystem.go`**

```go
package storage

import (
	"fmt"

	"github.com/goairix/fs"
	"github.com/goairix/fs/driver/alioss"
	"github.com/goairix/fs/driver/hwobs"
	"github.com/goairix/fs/driver/local"
	"github.com/goairix/fs/driver/minio"
	"github.com/goairix/fs/driver/s3"
	"github.com/goairix/fs/driver/txcos"

	"github.com/goairix/sandbox/internal/config"
)

// NewFileSystem creates a fs.FileSystem from the given configuration.
func NewFileSystem(cfg config.FileSystemConfig) (fs.FileSystem, error) {
	switch cfg.Provider {
	case "local":
		if cfg.LocalPath == "" {
			return nil, fmt.Errorf("storage: local provider requires local_path")
		}
		return local.New(local.Config{
			RootPath: cfg.LocalPath,
			SubPath:  cfg.SubPath,
		})

	case "s3":
		return s3.New(s3.Config{
			Region:          cfg.Region,
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})

	case "cos":
		return txcos.New(txcos.Config{
			BucketURL: cfg.Endpoint,
			SecretID:  cfg.AccessKey,
			SecretKey: cfg.SecretKey,
			SubPath:   cfg.SubPath,
		})

	case "oss":
		return alioss.New(alioss.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})

	case "obs":
		return hwobs.New(hwobs.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})

	case "minio":
		return minio.New(minio.Config{
			Endpoint:        cfg.Endpoint,
			AccessKeyID:     cfg.AccessKey,
			SecretAccessKey: cfg.SecretKey,
			UseSSL:          cfg.UseSSL,
			BucketName:      cfg.Bucket,
			SubPath:         cfg.SubPath,
		})

	default:
		return nil, fmt.Errorf("storage: unsupported filesystem provider: %q", cfg.Provider)
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/storage/ -run TestNewFileSystem -v
```

Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/storage/filesystem.go internal/storage/filesystem_test.go && git commit -m "feat(storage): add NewFileSystem factory for fs.FileSystem"
```

---

### Task 4: Create `ScopedFS` implementation

**Files:**
- Create: `internal/storage/scoped_fs.go`
- Create: `internal/storage/scoped_fs_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/storage/scoped_fs_test.go`:

```go
package storage

import (
	"context"
	"io"
	"os"
	"testing"

	"github.com/goairix/fs/driver/local"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestFS(t *testing.T) (*scopedFS, string) {
	t.Helper()
	dir := t.TempDir()

	fsys, err := local.New(local.Config{RootPath: dir})
	require.NoError(t, err)

	// Create root directory for scoping
	ctx := context.Background()
	err = fsys.MakeDir(ctx, "workspace", os.ModePerm)
	require.NoError(t, err)

	scoped, err := NewScopedFS(fsys, "workspace")
	require.NoError(t, err)

	return scoped.(*scopedFS), dir
}

func TestNewScopedFS_EmptyRoot(t *testing.T) {
	fsys, err := local.New(local.Config{RootPath: t.TempDir()})
	require.NoError(t, err)

	_, err = NewScopedFS(fsys, "")
	assert.Error(t, err)
}

func TestNewScopedFS_NilFS(t *testing.T) {
	_, err := NewScopedFS(nil, "workspace")
	assert.Error(t, err)
}

func TestScopedFS_ResolvePath_Normal(t *testing.T) {
	sfs, _ := setupTestFS(t)

	resolved, err := sfs.resolvePath("file.txt")
	require.NoError(t, err)
	assert.Equal(t, "workspace/file.txt", resolved)
}

func TestScopedFS_ResolvePath_Subdirectory(t *testing.T) {
	sfs, _ := setupTestFS(t)

	resolved, err := sfs.resolvePath("sub/dir/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "workspace/sub/dir/file.txt", resolved)
}

func TestScopedFS_ResolvePath_EscapeBlocked(t *testing.T) {
	sfs, _ := setupTestFS(t)

	_, err := sfs.resolvePath("../etc/passwd")
	assert.ErrorIs(t, err, ErrPathEscaped)
}

func TestScopedFS_ResolvePath_AbsoluteBlocked(t *testing.T) {
	sfs, _ := setupTestFS(t)

	_, err := sfs.resolvePath("/etc/passwd")
	assert.ErrorIs(t, err, ErrPathEscaped)
}

func TestScopedFS_ResolvePath_DotDotInMiddleBlocked(t *testing.T) {
	sfs, _ := setupTestFS(t)

	_, err := sfs.resolvePath("sub/../../etc/passwd")
	assert.ErrorIs(t, err, ErrPathEscaped)
}

func TestScopedFS_ResolvePath_DotStaysInRoot(t *testing.T) {
	sfs, _ := setupTestFS(t)

	resolved, err := sfs.resolvePath(".")
	require.NoError(t, err)
	assert.Equal(t, "workspace", resolved)
}

func TestScopedFS_CreateAndOpen(t *testing.T) {
	sfs, _ := setupTestFS(t)
	ctx := context.Background()

	w, err := sfs.Create(ctx, "hello.txt")
	require.NoError(t, err)
	_, err = w.Write([]byte("hello world"))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	r, err := sfs.Open(ctx, "hello.txt")
	require.NoError(t, err)
	defer r.Close()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(data))
}

func TestScopedFS_CreateEscapeBlocked(t *testing.T) {
	sfs, _ := setupTestFS(t)
	ctx := context.Background()

	_, err := sfs.Create(ctx, "../escape.txt")
	assert.ErrorIs(t, err, ErrPathEscaped)
}

func TestScopedFS_ChangeDir(t *testing.T) {
	sfs, _ := setupTestFS(t)
	ctx := context.Background()

	// Create a subdirectory
	err := sfs.MakeDir(ctx, "subdir", os.ModePerm)
	require.NoError(t, err)

	err = sfs.ChangeDir(ctx, "subdir")
	require.NoError(t, err)
	assert.Equal(t, "subdir", sfs.WorkingDir())

	// Create file in cwd — should land in workspace/subdir/
	w, err := sfs.Create(ctx, "in_sub.txt")
	require.NoError(t, err)
	_, _ = w.Write([]byte("inside subdir"))
	require.NoError(t, w.Close())

	// Verify by opening with explicit path from root
	err = sfs.ChangeDir(ctx, "/")
	require.NoError(t, err)
	assert.Equal(t, ".", sfs.WorkingDir())

	r, err := sfs.Open(ctx, "subdir/in_sub.txt")
	require.NoError(t, err)
	defer r.Close()
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "inside subdir", string(data))
}

func TestScopedFS_ChangeDirEscapeBlocked(t *testing.T) {
	sfs, _ := setupTestFS(t)
	ctx := context.Background()

	err := sfs.ChangeDir(ctx, "..")
	assert.ErrorIs(t, err, ErrPathEscaped)
}

func TestScopedFS_WorkingDir_Default(t *testing.T) {
	sfs, _ := setupTestFS(t)
	assert.Equal(t, ".", sfs.WorkingDir())
}

func TestScopedFS_List(t *testing.T) {
	sfs, _ := setupTestFS(t)
	ctx := context.Background()

	w, err := sfs.Create(ctx, "a.txt")
	require.NoError(t, err)
	_, _ = w.Write([]byte("a"))
	w.Close()

	w, err = sfs.Create(ctx, "b.txt")
	require.NoError(t, err)
	_, _ = w.Write([]byte("b"))
	w.Close()

	files, err := sfs.List(ctx, ".")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(files), 2)
}

func TestScopedFS_Exists(t *testing.T) {
	sfs, _ := setupTestFS(t)
	ctx := context.Background()

	exists, err := sfs.Exists(ctx, "nope.txt")
	require.NoError(t, err)
	assert.False(t, exists)

	w, err := sfs.Create(ctx, "yes.txt")
	require.NoError(t, err)
	w.Close()

	exists, err = sfs.Exists(ctx, "yes.txt")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestScopedFS_CopyMoveSameScope(t *testing.T) {
	sfs, _ := setupTestFS(t)
	ctx := context.Background()

	w, err := sfs.Create(ctx, "src.txt")
	require.NoError(t, err)
	_, _ = w.Write([]byte("source"))
	w.Close()

	err = sfs.Copy(ctx, "src.txt", "dst.txt")
	require.NoError(t, err)

	exists, err := sfs.Exists(ctx, "dst.txt")
	require.NoError(t, err)
	assert.True(t, exists)

	err = sfs.Move(ctx, "dst.txt", "moved.txt")
	require.NoError(t, err)

	exists, err = sfs.Exists(ctx, "moved.txt")
	require.NoError(t, err)
	assert.True(t, exists)

	exists, err = sfs.Exists(ctx, "dst.txt")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestScopedFS_CopyEscapeBlocked(t *testing.T) {
	sfs, _ := setupTestFS(t)
	ctx := context.Background()

	err := sfs.Copy(ctx, "file.txt", "../../escape.txt")
	assert.ErrorIs(t, err, ErrPathEscaped)
}

func TestScopedFS_Remove(t *testing.T) {
	sfs, _ := setupTestFS(t)
	ctx := context.Background()

	w, err := sfs.Create(ctx, "todel.txt")
	require.NoError(t, err)
	w.Close()

	err = sfs.Remove(ctx, "todel.txt")
	require.NoError(t, err)

	exists, err := sfs.Exists(ctx, "todel.txt")
	require.NoError(t, err)
	assert.False(t, exists)
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/storage/ -run TestScopedFS -v
```

Expected: FAIL — `ScopedFS`, `NewScopedFS`, `ErrPathEscaped` not defined.

- [ ] **Step 3: Implement `internal/storage/scoped_fs.go`**

```go
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/goairix/fs"
)

// ErrPathEscaped is returned when a path attempts to escape the scoped root directory.
var ErrPathEscaped = errors.New("path escapes the scoped root directory")

// ScopedFS restricts all file operations to a root directory.
type ScopedFS interface {
	List(ctx context.Context, p string, opts ...fs.Option) ([]fs.FileInfo, error)
	MakeDir(ctx context.Context, p string, perm os.FileMode, opts ...fs.Option) error
	RemoveDir(ctx context.Context, p string, opts ...fs.Option) error
	Create(ctx context.Context, p string, opts ...fs.Option) (io.WriteCloser, error)
	Open(ctx context.Context, p string, opts ...fs.Option) (io.ReadCloser, error)
	OpenFile(ctx context.Context, p string, flag int, perm os.FileMode, opts ...fs.Option) (io.ReadWriteCloser, error)
	Remove(ctx context.Context, p string, opts ...fs.Option) error
	Copy(ctx context.Context, src, dst string, opts ...fs.Option) error
	Move(ctx context.Context, src, dst string, opts ...fs.Option) error
	Rename(ctx context.Context, oldPath, newPath string, opts ...fs.Option) error
	Stat(ctx context.Context, p string, opts ...fs.Option) (fs.FileInfo, error)
	Exists(ctx context.Context, p string, opts ...fs.Option) (bool, error)
	IsDir(ctx context.Context, p string, opts ...fs.Option) (bool, error)
	IsFile(ctx context.Context, p string, opts ...fs.Option) (bool, error)
	SignFullUrl(ctx context.Context, p string, opts ...fs.Option) (string, error)
	FullUrl(ctx context.Context, p string, opts ...fs.Option) (string, error)
	RelativePath(ctx context.Context, fullUrl string, opts ...fs.Option) (string, error)
	ChangeDir(ctx context.Context, p string) error
	WorkingDir() string
}

type scopedFS struct {
	driver   fs.FileSystem
	rootPath string
	cwd      string
}

// NewScopedFS creates a ScopedFS that confines operations to rootPath.
func NewScopedFS(filesystem fs.FileSystem, rootPath string) (ScopedFS, error) {
	root := cleanRoot(rootPath)
	if root == "" || root == "." {
		return nil, fmt.Errorf("storage: rootPath must not be empty")
	}
	if filesystem == nil {
		return nil, fmt.Errorf("storage: filesystem must not be nil")
	}
	return &scopedFS{
		driver:   filesystem,
		rootPath: root,
	}, nil
}

func (s *scopedFS) resolvePath(p string) (string, error) {
	if path.IsAbs(p) {
		return "", ErrPathEscaped
	}

	base := s.rootPath
	if s.cwd != "" {
		base = path.Join(s.rootPath, s.cwd)
	}

	joined := path.Clean(path.Join(base, p))

	if joined != s.rootPath && !strings.HasPrefix(joined, s.rootPath+"/") {
		return "", ErrPathEscaped
	}

	return joined, nil
}

func cleanRoot(root string) string {
	return strings.TrimRight(path.Clean(root), "/")
}

// --- Single-path proxy methods ---

func (s *scopedFS) List(ctx context.Context, p string, opts ...fs.Option) ([]fs.FileInfo, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	return s.driver.List(ctx, resolved, opts...)
}

func (s *scopedFS) MakeDir(ctx context.Context, p string, perm os.FileMode, opts ...fs.Option) error {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return err
	}
	return s.driver.MakeDir(ctx, resolved, perm, opts...)
}

func (s *scopedFS) RemoveDir(ctx context.Context, p string, opts ...fs.Option) error {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return err
	}
	return s.driver.RemoveDir(ctx, resolved, opts...)
}

func (s *scopedFS) Create(ctx context.Context, p string, opts ...fs.Option) (io.WriteCloser, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	return s.driver.Create(ctx, resolved, opts...)
}

func (s *scopedFS) Open(ctx context.Context, p string, opts ...fs.Option) (io.ReadCloser, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	return s.driver.Open(ctx, resolved, opts...)
}

func (s *scopedFS) OpenFile(ctx context.Context, p string, flag int, perm os.FileMode, opts ...fs.Option) (io.ReadWriteCloser, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	return s.driver.OpenFile(ctx, resolved, flag, perm, opts...)
}

func (s *scopedFS) Remove(ctx context.Context, p string, opts ...fs.Option) error {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return err
	}
	return s.driver.Remove(ctx, resolved, opts...)
}

func (s *scopedFS) Stat(ctx context.Context, p string, opts ...fs.Option) (fs.FileInfo, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	return s.driver.Stat(ctx, resolved, opts...)
}

func (s *scopedFS) Exists(ctx context.Context, p string, opts ...fs.Option) (bool, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return false, err
	}
	return s.driver.Exists(ctx, resolved, opts...)
}

func (s *scopedFS) IsDir(ctx context.Context, p string, opts ...fs.Option) (bool, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return false, err
	}
	return s.driver.IsDir(ctx, resolved, opts...)
}

func (s *scopedFS) IsFile(ctx context.Context, p string, opts ...fs.Option) (bool, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return false, err
	}
	return s.driver.IsFile(ctx, resolved, opts...)
}

func (s *scopedFS) SignFullUrl(ctx context.Context, p string, opts ...fs.Option) (string, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return "", err
	}
	return s.driver.SignFullUrl(ctx, resolved, opts...)
}

func (s *scopedFS) FullUrl(ctx context.Context, p string, opts ...fs.Option) (string, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return "", err
	}
	return s.driver.FullUrl(ctx, resolved, opts...)
}

// --- Dual-path proxy methods ---

func (s *scopedFS) Copy(ctx context.Context, src, dst string, opts ...fs.Option) error {
	resolvedSrc, err := s.resolvePath(src)
	if err != nil {
		return err
	}
	resolvedDst, err := s.resolvePath(dst)
	if err != nil {
		return err
	}
	return s.driver.Copy(ctx, resolvedSrc, resolvedDst, opts...)
}

func (s *scopedFS) Move(ctx context.Context, src, dst string, opts ...fs.Option) error {
	resolvedSrc, err := s.resolvePath(src)
	if err != nil {
		return err
	}
	resolvedDst, err := s.resolvePath(dst)
	if err != nil {
		return err
	}
	return s.driver.Move(ctx, resolvedSrc, resolvedDst, opts...)
}

func (s *scopedFS) Rename(ctx context.Context, oldPath, newPath string, opts ...fs.Option) error {
	resolvedOld, err := s.resolvePath(oldPath)
	if err != nil {
		return err
	}
	resolvedNew, err := s.resolvePath(newPath)
	if err != nil {
		return err
	}
	return s.driver.Rename(ctx, resolvedOld, resolvedNew, opts...)
}

func (s *scopedFS) RelativePath(ctx context.Context, fullUrl string, opts ...fs.Option) (string, error) {
	rel, err := s.driver.RelativePath(ctx, fullUrl, opts...)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimPrefix(rel, s.rootPath+"/")
	if trimmed == rel {
		trimmed = strings.TrimPrefix(rel, s.rootPath)
	}
	return trimmed, nil
}

// --- Working directory ---

func (s *scopedFS) ChangeDir(ctx context.Context, p string) error {
	if p == "/" {
		s.cwd = ""
		return nil
	}
	resolved, err := s.resolvePath(p)
	if err != nil {
		return err
	}
	isDir, err := s.driver.IsDir(ctx, resolved)
	if err != nil {
		return err
	}
	if !isDir {
		return fmt.Errorf("storage: %q is not a directory", p)
	}
	if resolved == s.rootPath {
		s.cwd = ""
	} else {
		s.cwd = strings.TrimPrefix(resolved, s.rootPath+"/")
	}
	return nil
}

func (s *scopedFS) WorkingDir() string {
	if s.cwd == "" {
		return "."
	}
	return s.cwd
}
```

- [ ] **Step 4: Run tests to verify they pass**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go test ./internal/storage/ -run "TestScopedFS|TestNewScopedFS" -v
```

Expected: All PASS

- [ ] **Step 5: Commit**

```bash
git add internal/storage/scoped_fs.go internal/storage/scoped_fs_test.go && git commit -m "feat(storage): add ScopedFS with path confinement and working directory"
```

---

### Task 5: Update sandbox types — add `WorkspaceInfo` and `WorkspacePath`

**Files:**
- Modify: `internal/sandbox/types.go:54-74`
- Create: `pkg/types/workspace.go`
- Modify: `pkg/types/sandbox.go:5-12`

- [ ] **Step 1: Add `WorkspacePath` to `SandboxConfig` and `WorkspaceInfo` to `Sandbox` in `internal/sandbox/types.go`**

Add after `NetworkConfig` (line 52), before `SandboxConfig`:

```go
// WorkspaceInfo holds metadata about a mounted workspace.
type WorkspaceInfo struct {
	RootPath  string    `json:"root_path"`
	MountedAt time.Time `json:"mounted_at"`
}
```

Add `WorkspacePath` field to `SandboxConfig` (after `Dependencies`):

```go
type SandboxConfig struct {
	Language      Language       `json:"language"`
	Mode          Mode           `json:"mode"`
	Timeout       int            `json:"timeout"`
	Resources     ResourceLimits `json:"resources"`
	Network       NetworkConfig  `json:"network"`
	Dependencies  []Dependency   `json:"dependencies"`
	WorkspacePath string         `json:"workspace_path,omitempty"`
}
```

Add `Workspace` field to `Sandbox` (after `Timeout`):

```go
type Sandbox struct {
	ID        string        `json:"id"`
	Config    SandboxConfig `json:"config"`
	State     State         `json:"state"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	RuntimeID string        `json:"runtime_id"`
	Timeout   time.Duration `json:"timeout"`
	Workspace *WorkspaceInfo `json:"workspace,omitempty"`
}
```

- [ ] **Step 2: Add `WorkspacePath` to `CreateSandboxRequest` in `pkg/types/sandbox.go`**

```go
type CreateSandboxRequest struct {
	Language      string           `json:"language" binding:"required,oneof=python nodejs bash"`
	Mode          string           `json:"mode" binding:"required,oneof=ephemeral persistent"`
	Timeout       int              `json:"timeout,omitempty" binding:"min=0,max=3600"`
	Resources     *ResourceLimits  `json:"resources,omitempty"`
	Network       *NetworkConfig   `json:"network,omitempty"`
	Dependencies  []DependencySpec `json:"dependencies,omitempty"`
	WorkspacePath string           `json:"workspace_path,omitempty"`
}
```

- [ ] **Step 3: Create `pkg/types/workspace.go`**

```go
package types

import "time"

type MountWorkspaceRequest struct {
	RootPath string `json:"root_path" binding:"required"`
}

type MountWorkspaceResponse struct {
	RootPath  string    `json:"root_path"`
	MountedAt time.Time `json:"mounted_at"`
}

type UnmountWorkspaceResponse struct {
	Message string `json:"message"`
}

type SyncWorkspaceRequest struct {
	Direction string `json:"direction" binding:"required,oneof=to_container from_container"`
}

type SyncWorkspaceResponse struct {
	Direction string `json:"direction"`
	Message   string `json:"message"`
}

type WorkspaceInfoResponse struct {
	Mounted   bool      `json:"mounted"`
	RootPath  string    `json:"root_path,omitempty"`
	MountedAt time.Time `json:"mounted_at,omitempty"`
}
```

- [ ] **Step 4: Verify build**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go build ./...
```

Expected: Build succeeds.

- [ ] **Step 5: Commit**

```bash
git add internal/sandbox/types.go pkg/types/sandbox.go pkg/types/workspace.go && git commit -m "feat(types): add workspace types for mount/unmount/sync API"
```

---

### Task 6: Update Manager — add `filesystem` and `workspaces` fields

**Files:**
- Modify: `internal/sandbox/manager.go:2-15` (imports)
- Modify: `internal/sandbox/manager.go:36-69` (Manager struct + NewManager)
- Modify: `internal/sandbox/manager.go:96-168` (Create method)
- Modify: `internal/sandbox/manager.go:199-226` (Destroy method)

- [ ] **Step 1: Add `filesystem` and `workspaces` fields to Manager struct**

Update the `Manager` struct (lines 42-53):

```go
// Manager orchestrates sandbox lifecycle: creation, execution, destruction.
type Manager struct {
	runtime    runtime.Runtime
	filesystem fs.FileSystem
	config     ManagerConfig
	sessions   *SessionStore // optional, for persistent sandboxes

	pools      map[Language]*Pool
	sandboxes  map[string]*Sandbox
	workspaces map[string]ScopedFS // sandbox ID -> ScopedFS
	mu         sync.RWMutex

	stopCh chan struct{}
	wg     sync.WaitGroup
}
```

Add import for `fs`:

```go
import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goairix/fs"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
)
```

Note: The `storage` import is needed in the next task (workspace.go). For this step, only the `fs` import is needed for the field type.

- [ ] **Step 2: Update `NewManager` to accept `fs.FileSystem`**

```go
// NewManager creates a new SandboxManager.
func NewManager(rt runtime.Runtime, fsys fs.FileSystem, cfg ManagerConfig) *Manager {
	pools := make(map[Language]*Pool)
	for lang, pcfg := range cfg.PoolConfigs {
		pools[lang] = NewPool(rt, pcfg)
	}

	return &Manager{
		runtime:    rt,
		filesystem: fsys,
		config:     cfg,
		pools:      pools,
		sandboxes:  make(map[string]*Sandbox),
		workspaces: make(map[string]ScopedFS),
		stopCh:     make(chan struct{}),
	}
}
```

Note: `ScopedFS` here is `storage.ScopedFS` — but since `workspace.go` (same package) will use it via the `storage` import, we need to use the full type. However, since Manager is in the `sandbox` package and `ScopedFS` is in the `storage` package, the field type must be `storage.ScopedFS`.

Updated Manager struct with correct type:

```go
type Manager struct {
	runtime    runtime.Runtime
	filesystem fs.FileSystem
	config     ManagerConfig
	sessions   *SessionStore

	pools      map[Language]*Pool
	sandboxes  map[string]*Sandbox
	workspaces map[string]storage.ScopedFS
	mu         sync.RWMutex

	stopCh chan struct{}
	wg     sync.WaitGroup
}
```

- [ ] **Step 3: Update `Create` to auto-mount workspace when `WorkspacePath` is set**

After the dependency installation block (line 154) and before the sandbox is stored in the map (line 156), add:

```go
	m.mu.Lock()
	m.sandboxes[id] = sb
	m.mu.Unlock()

	// Auto-mount workspace if specified
	if cfg.WorkspacePath != "" {
		if err := m.MountWorkspace(ctx, id, cfg.WorkspacePath); err != nil {
			// Cleanup on failure
			_ = m.runtime.RemoveSandbox(ctx, info.RuntimeID)
			m.mu.Lock()
			delete(m.sandboxes, id)
			m.mu.Unlock()
			return nil, fmt.Errorf("mount workspace: %w", err)
		}
	}
```

- [ ] **Step 4: Update `Destroy` to auto-sync workspace before destruction**

In the `Destroy` method, after setting state to `StateDestroying` (line 206) and before deleting from sandboxes map:

```go
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

	// Best-effort sync workspace back before destroying
	if _, hasWS := m.workspaces[id]; hasWS {
		_ = m.syncFromContainer(ctx, id, sb.RuntimeID)
		m.mu.Lock()
		delete(m.workspaces, id)
		m.mu.Unlock()
	}

	// Clean up session store
	if m.sessions != nil {
		_ = m.sessions.Remove(ctx, id)
	}

	// Remove the container
	if err := m.runtime.RemoveSandbox(ctx, sb.RuntimeID); err != nil {
		return fmt.Errorf("remove sandbox: %w", err)
	}

	// Notify pool so it can refill
	if pool, ok := m.pools[sb.Config.Language]; ok {
		pool.NotifyRemoved()
	}

	return nil
}
```

- [ ] **Step 5: Update `cmd/sandbox/main.go` to pass filesystem to Manager**

Add imports:

```go
import (
	// ... existing imports ...
	"github.com/goairix/sandbox/internal/storage"
)
```

After the runtime initialization block (line 49) and before pool config (line 51), add:

```go
	// Initialize filesystem
	fsys, err := storage.NewFileSystem(cfg.Storage.FileSystem)
	if err != nil {
		log.Fatalf("failed to create filesystem: %v", err)
	}
```

Update the `NewManager` call (line 86):

```go
	mgr := sandbox.NewManager(rt, fsys, sandbox.ManagerConfig{
		PoolConfigs:    poolConfigs,
		DefaultTimeout: cfg.Security.SandboxTimeoutSeconds,
	})
```

- [ ] **Step 6: Update handler `CreateSandbox` to pass `WorkspacePath`**

In `internal/api/handler/sandbox.go`, in the `CreateSandbox` method, add after line 80 (Dependencies loop):

```go
	cfg.WorkspacePath = req.WorkspacePath
```

So the cfg construction becomes:

```go
	cfg := sandbox.SandboxConfig{
		Language:      sandbox.Language(req.Language),
		Mode:          sandbox.Mode(req.Mode),
		Timeout:       req.Timeout,
		WorkspacePath: req.WorkspacePath,
	}
```

- [ ] **Step 7: Verify build**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go build ./...
```

Expected: Build may fail because `MountWorkspace` and `syncFromContainer` don't exist yet. That's OK — they will be created in Task 7. If using TDD strictly, you can stub them first. For a pragmatic approach, commit the type changes and continue to Task 7.

- [ ] **Step 8: Commit**

```bash
git add internal/sandbox/manager.go internal/api/handler/sandbox.go cmd/sandbox/main.go && git commit -m "feat(sandbox): add filesystem and workspaces to Manager, auto-mount on create"
```

---

### Task 7: Create workspace sync logic — `workspace.go`

**Files:**
- Create: `internal/sandbox/workspace.go`
- Create: `internal/sandbox/workspace_test.go`

- [ ] **Step 1: Implement `internal/sandbox/workspace.go`**

```go
package sandbox

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/goairix/sandbox/internal/storage"
)

// MountWorkspace creates a ScopedFS for the given rootPath, syncs files into the container.
func (m *Manager) MountWorkspace(ctx context.Context, sandboxID, rootPath string) error {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	runtimeID := sb.RuntimeID
	m.mu.RUnlock()

	// Check if already mounted
	m.mu.RLock()
	if _, exists := m.workspaces[sandboxID]; exists {
		m.mu.RUnlock()
		return fmt.Errorf("workspace already mounted for sandbox %s", sandboxID)
	}
	m.mu.RUnlock()

	scoped, err := storage.NewScopedFS(m.filesystem, rootPath)
	if err != nil {
		return fmt.Errorf("create scoped filesystem: %w", err)
	}

	// Sync files from storage to container
	if err := m.syncToContainer(ctx, scoped, runtimeID); err != nil {
		return fmt.Errorf("sync to container: %w", err)
	}

	m.mu.Lock()
	m.workspaces[sandboxID] = scoped
	sb.Workspace = &WorkspaceInfo{
		RootPath:  rootPath,
		MountedAt: time.Now(),
	}
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	return nil
}

// UnmountWorkspace syncs files back from container to storage, then detaches.
func (m *Manager) UnmountWorkspace(ctx context.Context, sandboxID string) error {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	runtimeID := sb.RuntimeID
	_, hasWS := m.workspaces[sandboxID]
	m.mu.RUnlock()

	if !hasWS {
		return fmt.Errorf("no workspace mounted for sandbox %s", sandboxID)
	}

	// Sync files from container back to storage
	if err := m.syncFromContainer(ctx, sandboxID, runtimeID); err != nil {
		return fmt.Errorf("sync from container: %w", err)
	}

	m.mu.Lock()
	delete(m.workspaces, sandboxID)
	sb.Workspace = nil
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	return nil
}

// SyncWorkspace manually syncs files in the given direction.
func (m *Manager) SyncWorkspace(ctx context.Context, sandboxID, direction string) error {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	runtimeID := sb.RuntimeID
	scoped, hasWS := m.workspaces[sandboxID]
	m.mu.RUnlock()

	if !hasWS {
		return fmt.Errorf("no workspace mounted for sandbox %s", sandboxID)
	}

	switch direction {
	case "to_container":
		return m.syncToContainer(ctx, scoped, runtimeID)
	case "from_container":
		return m.syncFromContainer(ctx, sandboxID, runtimeID)
	default:
		return fmt.Errorf("invalid sync direction: %s", direction)
	}
}

// GetWorkspaceInfo returns workspace info for a sandbox.
func (m *Manager) GetWorkspaceInfo(_ context.Context, sandboxID string) (*WorkspaceInfo, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("sandbox not found: %s", sandboxID)
	}

	return sb.Workspace, nil
}

// syncToContainer uploads all files from ScopedFS into the container /workspace.
func (m *Manager) syncToContainer(ctx context.Context, scoped storage.ScopedFS, runtimeID string) error {
	return m.syncDir(ctx, scoped, runtimeID, ".")
}

// syncDir recursively syncs a directory from ScopedFS into the container.
func (m *Manager) syncDir(ctx context.Context, scoped storage.ScopedFS, runtimeID, dir string) error {
	files, err := scoped.List(ctx, dir)
	if err != nil {
		return fmt.Errorf("list %q: %w", dir, err)
	}

	for _, fi := range files {
		relPath := dir
		if relPath == "." {
			relPath = fi.Name()
		} else {
			relPath = filepath.Join(dir, fi.Name())
		}

		if fi.IsDir() {
			if err := m.syncDir(ctx, scoped, runtimeID, relPath); err != nil {
				return err
			}
			continue
		}

		reader, err := scoped.Open(ctx, relPath)
		if err != nil {
			return fmt.Errorf("open %q: %w", relPath, err)
		}

		destPath := filepath.Join("/workspace", relPath)
		uploadErr := m.runtime.UploadFile(ctx, runtimeID, destPath, reader)
		reader.Close()
		if uploadErr != nil {
			return fmt.Errorf("upload %q: %w", destPath, uploadErr)
		}
	}

	return nil
}

// syncFromContainer downloads all files from container /workspace into ScopedFS.
func (m *Manager) syncFromContainer(ctx context.Context, sandboxID, runtimeID string) error {
	m.mu.RLock()
	scoped, ok := m.workspaces[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no workspace for sandbox %s", sandboxID)
	}

	files, err := m.runtime.ListFiles(ctx, runtimeID, "/workspace")
	if err != nil {
		return fmt.Errorf("list container files: %w", err)
	}

	for _, fi := range files {
		if fi.IsDir {
			continue
		}

		// Compute relative path from /workspace
		relPath := fi.Path
		if len(relPath) > len("/workspace/") {
			relPath = relPath[len("/workspace/"):]
		} else {
			relPath = fi.Name
		}

		reader, err := m.runtime.DownloadFile(ctx, runtimeID, fi.Path)
		if err != nil {
			return fmt.Errorf("download %q: %w", fi.Path, err)
		}

		writer, err := scoped.Create(ctx, relPath)
		if err != nil {
			reader.Close()
			return fmt.Errorf("create %q: %w", relPath, err)
		}

		_, copyErr := io.Copy(writer, reader)
		reader.Close()
		writer.Close()
		if copyErr != nil {
			return fmt.Errorf("copy %q: %w", relPath, copyErr)
		}
	}

	return nil
}
```

- [ ] **Step 2: Verify build**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go build ./...
```

Expected: Build succeeds. All references to `MountWorkspace`, `syncFromContainer`, etc. are now resolved.

- [ ] **Step 3: Commit**

```bash
git add internal/sandbox/workspace.go && git commit -m "feat(sandbox): add workspace mount/unmount/sync logic"
```

---

### Task 8: Create workspace API handlers and routes

**Files:**
- Create: `internal/api/handler/workspace.go`
- Modify: `internal/api/router.go:45-58`

- [ ] **Step 1: Create `internal/api/handler/workspace.go`**

```go
package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/goairix/sandbox/pkg/types"
)

func (h *Handler) MountWorkspace(c *gin.Context) {
	id := c.Param("id")

	var req types.MountWorkspaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := h.manager.MountWorkspace(c.Request.Context(), id, req.RootPath); err != nil {
		if containsAny(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		if containsAny(err.Error(), "already mounted") {
			c.JSON(http.StatusConflict, types.ErrorResponse{Message: err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{Message: err.Error()})
		return
	}

	info, _ := h.manager.GetWorkspaceInfo(c.Request.Context(), id)
	c.JSON(http.StatusOK, types.MountWorkspaceResponse{
		RootPath:  info.RootPath,
		MountedAt: info.MountedAt,
	})
}

func (h *Handler) UnmountWorkspace(c *gin.Context) {
	id := c.Param("id")

	if err := h.manager.UnmountWorkspace(c.Request.Context(), id); err != nil {
		if containsAny(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		if containsAny(err.Error(), "no workspace mounted") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{Message: err.Error()})
		return
	}

	c.JSON(http.StatusOK, types.UnmountWorkspaceResponse{Message: "workspace unmounted"})
}

func (h *Handler) SyncWorkspace(c *gin.Context) {
	id := c.Param("id")

	var req types.SyncWorkspaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
		return
	}

	if err := h.manager.SyncWorkspace(c.Request.Context(), id, req.Direction); err != nil {
		if containsAny(err.Error(), "not found") {
			c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
			return
		}
		if containsAny(err.Error(), "no workspace mounted") {
			c.JSON(http.StatusBadRequest, types.ErrorResponse{Message: err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, types.ErrorResponse{Message: err.Error()})
		return
	}

	c.JSON(http.StatusOK, types.SyncWorkspaceResponse{
		Direction: req.Direction,
		Message:   "sync completed",
	})
}

func (h *Handler) GetWorkspaceInfo(c *gin.Context) {
	id := c.Param("id")

	info, err := h.manager.GetWorkspaceInfo(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, types.ErrorResponse{Message: err.Error()})
		return
	}

	if info == nil {
		c.JSON(http.StatusOK, types.WorkspaceInfoResponse{Mounted: false})
		return
	}

	c.JSON(http.StatusOK, types.WorkspaceInfoResponse{
		Mounted:   true,
		RootPath:  info.RootPath,
		MountedAt: info.MountedAt,
	})
}

// containsAny checks if s contains any of the given substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
```

- [ ] **Step 2: Add workspace routes to `internal/api/router.go`**

After the file operations routes (line 58), add:

```go
	// Workspace operations
	v1.POST("/sandboxes/:id/workspace/mount", h.MountWorkspace)
	v1.POST("/sandboxes/:id/workspace/unmount", h.UnmountWorkspace)
	v1.POST("/sandboxes/:id/workspace/sync", h.SyncWorkspace)
	v1.GET("/sandboxes/:id/workspace/info", h.GetWorkspaceInfo)
```

- [ ] **Step 3: Verify build**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go build ./...
```

Expected: Build succeeds.

- [ ] **Step 4: Run all tests**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go test ./... -v
```

Expected: All tests pass.

- [ ] **Step 5: Commit**

```bash
git add internal/api/handler/workspace.go internal/api/router.go && git commit -m "feat(api): add workspace mount/unmount/sync/info endpoints"
```

---

### Task 9: Final integration verification

**Files:**
- None (verification only)

- [ ] **Step 1: Run full test suite**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go test ./... -v -count=1
```

Expected: All tests pass.

- [ ] **Step 2: Verify build**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go build ./cmd/sandbox/
```

Expected: Binary compiles successfully.

- [ ] **Step 3: Run vet and check for issues**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go vet ./...
```

Expected: No issues.

- [ ] **Step 4: Verify unused imports removed**

```bash
cd /Users/dysodeng/project/go/cloud/sandbox && go mod tidy && go build ./...
```

Expected: Clean build.

- [ ] **Step 5: Commit any cleanup**

```bash
git add -A && git diff --cached --stat
```

If there are changes:

```bash
git commit -m "chore: tidy modules and cleanup"
```
