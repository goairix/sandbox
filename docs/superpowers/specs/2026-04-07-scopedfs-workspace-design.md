# ScopedFS Workspace Design Spec

## Summary

Replace the existing `internal/storage/object` layer with `github.com/dysodeng/fs` (`fs.FileSystem`), and integrate `ScopedFS` as a per-sandbox workspace that restricts all file operations within a designated root directory. Workspaces can be attached at sandbox creation or mounted/unmounted dynamically via new API endpoints. Mounting syncs files from the storage backend into the container's `/workspace`; unmounting syncs back.

## Motivation

- The current `object.Store` interface is limited (Put/Get/Delete/List/Exists/PresignedURL) and not used by any upstream code.
- `github.com/dysodeng/fs` provides a richer `FileSystem` interface (directories, copy, move, rename, metadata, multipart upload) with drivers for local, S3, AliOSS, HuaweiOBS, TencentCOS, and MinIO.
- `ScopedFS` wraps `fs.FileSystem` with path confinement (prevents directory traversal) and working-directory semantics, making it ideal for per-sandbox workspace isolation.

## Part 1: Replace `object.Store` with `fs.FileSystem`

### Changes

1. **Delete** `internal/storage/object/` (all subdirectories and files).
2. **Add dependency** `github.com/dysodeng/fs` to `go.mod`.
3. **Update `config.go`**: Rename `ObjectStorageConfig` to `FileSystemConfig`. Map provider names to `fs` driver constructors:
   - `local` -> `github.com/dysodeng/fs/driver/local`
   - `s3` -> `github.com/dysodeng/fs/driver/s3`
   - `cos` -> `github.com/dysodeng/fs/driver/txcos`
   - `oss` -> `github.com/dysodeng/fs/driver/alioss`
   - `obs` -> `github.com/dysodeng/fs/driver/hwobs`
   - `minio` -> `github.com/dysodeng/fs/driver/minio`
4. **New file** `internal/storage/filesystem.go`: Factory function `NewFileSystem(cfg config.FileSystemConfig) (fs.FileSystem, error)` that creates the appropriate driver based on `cfg.Provider`.
5. **Update `main.go`**: Call `storage.NewFileSystem(cfg.Storage.FileSystem)` at startup, pass the instance to `Manager`.

### Config Structure Change

```go
// Before
type ObjectStorageConfig struct {
    Provider  string `mapstructure:"provider"`
    Bucket    string `mapstructure:"bucket"`
    Region    string `mapstructure:"region"`
    Endpoint  string `mapstructure:"endpoint"`
    AccessKey string `mapstructure:"access_key"`
    SecretKey string `mapstructure:"secret_key"`
    LocalPath string `mapstructure:"local_path"`
}

// After
type FileSystemConfig struct {
    Provider  string `mapstructure:"provider"`  // local, s3, cos, oss, obs, minio
    Bucket    string `mapstructure:"bucket"`
    Region    string `mapstructure:"region"`
    Endpoint  string `mapstructure:"endpoint"`
    AccessKey string `mapstructure:"access_key"`
    SecretKey string `mapstructure:"secret_key"`
    LocalPath string `mapstructure:"local_path"` // for local provider
    SubPath   string `mapstructure:"sub_path"`   // prefix path in bucket
    UseSSL    bool   `mapstructure:"use_ssl"`    // for minio
}
```

Environment variables remain `SANDBOX_STORAGE_FILESYSTEM_*` (replacing `SANDBOX_STORAGE_OBJECT_*`).

## Part 2: Integrate ScopedFS as Sandbox Workspace

### New Files

1. **`internal/storage/scoped_fs.go`**: The user-provided `ScopedFS` implementation (interface + struct). No modifications needed; it wraps any `fs.FileSystem` with path confinement.

2. **`internal/sandbox/workspace.go`**: Workspace management logic:
   - `MountWorkspace(ctx, sandboxID, rootPath)` - Create ScopedFS, sync files to container
   - `UnmountWorkspace(ctx, sandboxID)` - Sync files back, detach ScopedFS
   - `SyncWorkspace(ctx, sandboxID, direction)` - Manual sync (to-container or from-container)
   - `GetWorkspaceInfo(ctx, sandboxID)` - Return workspace status

### Sandbox Struct Changes

```go
// internal/sandbox/types.go
type Sandbox struct {
    ID        string        `json:"id"`
    Config    SandboxConfig `json:"config"`
    State     State         `json:"state"`
    CreatedAt time.Time     `json:"created_at"`
    UpdatedAt time.Time     `json:"updated_at"`
    RuntimeID string        `json:"runtime_id"`
    Timeout   time.Duration `json:"timeout"`

    // Workspace (nil when no workspace is mounted)
    Workspace *WorkspaceInfo `json:"workspace,omitempty"`
}

type WorkspaceInfo struct {
    RootPath  string    `json:"root_path"`   // Storage path (e.g., "user123/project-a")
    MountedAt time.Time `json:"mounted_at"`
}
```

The `ScopedFS` instance is stored in Manager's in-memory map (not serialized), keyed by sandbox ID.

### SandboxConfig Changes

```go
type SandboxConfig struct {
    Language      Language       `json:"language"`
    Mode          Mode           `json:"mode"`
    Timeout       int            `json:"timeout"`
    Resources     ResourceLimits `json:"resources"`
    Network       NetworkConfig  `json:"network"`
    Dependencies  []Dependency   `json:"dependencies"`
    WorkspacePath string         `json:"workspace_path,omitempty"` // optional, mount at creation
}
```

### Manager Changes

```go
type Manager struct {
    runtime    runtime.Runtime
    filesystem fs.FileSystem  // shared fs.FileSystem instance
    config     ManagerConfig
    sessions   *SessionStore

    pools      map[Language]*Pool
    sandboxes  map[string]*Sandbox
    workspaces map[string]storage.ScopedFS // sandbox ID -> ScopedFS
    mu         sync.RWMutex

    stopCh chan struct{}
    wg     sync.WaitGroup
}
```

### Sync Logic

**Mount (ScopedFS -> Container)**:
1. Create `ScopedFS` with `storage.NewScopedFS(filesystem, rootPath)`
2. List all files in ScopedFS root via `scopedFS.List(ctx, ".")`
3. For each file: `scopedFS.Open()` -> `runtime.UploadFile()` to container `/workspace/<relative-path>`
4. For directories: recursively process
5. Store ScopedFS instance in `workspaces` map

**Unmount (Container -> ScopedFS)**:
1. List files in container `/workspace` via `runtime.ListFiles()`
2. For each file: `runtime.DownloadFile()` -> `scopedFS.Create()` to write back
3. Remove ScopedFS from `workspaces` map

**Manual Sync**:
- `to_container`: Same as mount sync (overwrite container files)
- `from_container`: Same as unmount sync (overwrite storage files)

### New API Endpoints

```
POST   /api/v1/sandboxes/:id/workspace/mount     # Mount workspace
POST   /api/v1/sandboxes/:id/workspace/unmount    # Unmount workspace
POST   /api/v1/sandboxes/:id/workspace/sync       # Manual sync
GET    /api/v1/sandboxes/:id/workspace/info        # Workspace info
```

### Request/Response Types

```go
// pkg/types/workspace.go

type MountWorkspaceRequest struct {
    RootPath string `json:"root_path" binding:"required"` // e.g. "user123/project-a"
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

### CreateSandboxRequest Change

```go
type CreateSandboxRequest struct {
    Language      string           `json:"language" binding:"required,oneof=python nodejs bash"`
    Mode          string           `json:"mode" binding:"required,oneof=ephemeral persistent"`
    Timeout       int              `json:"timeout,omitempty" binding:"min=0,max=3600"`
    Resources     *ResourceLimits  `json:"resources,omitempty"`
    Network       *NetworkConfig   `json:"network,omitempty"`
    Dependencies  []DependencySpec `json:"dependencies,omitempty"`
    WorkspacePath string           `json:"workspace_path,omitempty"` // optional workspace to mount
}
```

When `workspace_path` is provided, the sandbox creation flow automatically mounts the workspace after container creation and dependency installation.

### Handler Changes

New handler methods in `internal/api/handler/workspace.go`:
- `MountWorkspace(c *gin.Context)`
- `UnmountWorkspace(c *gin.Context)`
- `SyncWorkspace(c *gin.Context)`
- `GetWorkspaceInfo(c *gin.Context)`

### Router Changes

Add to `internal/api/router.go`:
```go
sbGroup.POST("/:id/workspace/mount", h.MountWorkspace)
sbGroup.POST("/:id/workspace/unmount", h.UnmountWorkspace)
sbGroup.POST("/:id/workspace/sync", h.SyncWorkspace)
sbGroup.GET("/:id/workspace/info", h.GetWorkspaceInfo)
```

### Destroy Behavior

When a sandbox with a mounted workspace is destroyed:
1. Auto-sync from container back to ScopedFS (best-effort)
2. Remove ScopedFS from workspaces map
3. Proceed with normal container destruction

The storage path itself is NOT deleted - it persists for future use.

## File Change Summary

| File | Action | Description |
|------|--------|-------------|
| `internal/storage/object/` | Delete | Remove entire directory |
| `internal/storage/filesystem.go` | Create | fs.FileSystem factory |
| `internal/storage/scoped_fs.go` | Create | ScopedFS implementation |
| `internal/config/config.go` | Modify | ObjectStorageConfig -> FileSystemConfig |
| `internal/sandbox/types.go` | Modify | Add Workspace, WorkspaceInfo, WorkspacePath |
| `internal/sandbox/manager.go` | Modify | Add filesystem field, workspace methods |
| `internal/sandbox/workspace.go` | Create | Workspace sync logic |
| `internal/api/handler/workspace.go` | Create | Workspace API handlers |
| `internal/api/router.go` | Modify | Add workspace routes |
| `pkg/types/sandbox.go` | Modify | Add WorkspacePath to CreateSandboxRequest |
| `pkg/types/workspace.go` | Create | Workspace request/response types |
| `cmd/sandbox/main.go` | Modify | Initialize fs.FileSystem, pass to Manager |
| `go.mod` | Modify | Add github.com/dysodeng/fs dependency |

## Error Handling

- **Mount when already mounted**: Return 409 Conflict
- **Unmount when not mounted**: Return 404
- **Sync when not mounted**: Return 400
- **Path traversal in rootPath**: ScopedFS prevents this; API validates rootPath format
- **Sync failure**: Return error with details, workspace remains in current state
- **Sandbox not found**: Return 404
- **Sandbox in wrong state** (creating/destroying): Return 409

## Testing

- Unit tests for `ScopedFS` path resolution and confinement
- Unit tests for `NewFileSystem` factory
- Integration tests for workspace mount/unmount/sync flow
- API tests for all new endpoints
