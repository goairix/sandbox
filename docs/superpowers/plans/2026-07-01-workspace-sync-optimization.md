# Workspace Sync Optimization Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement the three workspace sync optimizations from `docs/superpowers/specs/2026-07-01-workspace-sync-optimization.md`: stream storage files into tar without full-file buffering, avoid full tar downloads for small container changes, and add the K8s init-container sync path for remote object storage.

**Architecture:** Keep optimization 1 and 2 inside `internal/sandbox/workspace.go`, preserving the public workspace API. Add `WorkspaceSyncSpec` to runtime creation only for K8s + remote storage + configured sync image/secret, then have K8s inject a sync init container and bootstrap egress policy before Pod creation. Persist each workspace's actual `SyncMode` so restore and manual mount behavior do not depend on current global config.

**Tech Stack:** Go 1.25, stdlib `archive/tar` / `io` / `context`, `golang.org/x/sync/errgroup`, `github.com/goairix/fs`, Kubernetes client-go fake clients, `github.com/stretchr/testify`.

---

## File Structure

| File | Action | Responsibility |
|------|--------|----------------|
| `internal/sandbox/workspace.go` | Modify | Stream `writeTarStream`; split changed-file download into direct and tar paths |
| `internal/sandbox/workspace_test.go` | Modify | Add streaming regression tests, changed-file download path tests, and shared filesystem test-double support |
| `internal/sandbox/pool_test.go` | Modify | Extend `mockRuntime` so workspace tests can inspect `ReadFileContent` / `DownloadDir` calls |
| `go.mod`, `go.sum` | Modify | Add `golang.org/x/sync/errgroup` if not already present |
| `internal/config/config.go` | Modify | Add workspace sync config fields, defaults, and validation |
| `internal/config/config_test.go` | Modify | Cover new defaults, YAML/env loading, and invalid half-enabled sync config |
| `configs/config.yaml` | Modify | Document workspace sync threshold and init-container options |
| `internal/storage/filesystem.go` | Modify | Preserve remote object storage metadata for init-container sync |
| `internal/storage/filesystem_test.go` | Modify | Verify `FileSystemMeta` carries provider endpoint, bucket, region, sub-path, and SSL |
| `internal/runtime/types.go` | Modify | Add `WorkspaceSyncSpec` to `SandboxSpec` |
| `internal/sandbox/types.go` | Modify | Add workspace sync mode constants and `WorkspaceInfo.SyncMode` |
| `cmd/sandbox/main.go` | Modify | Wire config fields into `sandbox.ManagerConfig` |
| `internal/sandbox/manager.go` | Modify | Bypass pool for init-container workspaces, build sync spec, register sync mode, restore by sync mode |
| `internal/sandbox/manager_test.go` | Modify | Cover pool bypass, manual mount API sync, and init-mode registration |
| `internal/runtime/kubernetes/pod.go` | Modify | Inject `workspace-sync` init container when `WorkspaceSync` is set |
| `internal/runtime/kubernetes/pod_test.go` | Create | Assert init container command, secret, security context, and workspace mount |
| `internal/runtime/kubernetes/network.go` | Modify | Add workspace bootstrap NetworkPolicy/CiliumNetworkPolicy helpers and cleanup |
| `internal/runtime/kubernetes/network_test.go` | Create | Cover CIDR bootstrap, Cilium FQDN bootstrap, cleanup, and selector logical ID |
| `internal/runtime/kubernetes/runtime.go` | Modify | Apply bootstrap policy before Pod creation, update normal policy after Ready, clean stale Cilium policies |
| `internal/runtime/kubernetes/runtime_test.go` | Create | Cover `policySandboxID`, `RemoveSandbox`, and `UpdateNetwork` logical ID behavior |

---

### Task 1: Stream `writeTarStream` Without Full-File Buffering

**Files:**
- Modify: `internal/sandbox/workspace.go`
- Modify: `internal/sandbox/workspace_test.go`

- [ ] **Step 1: Add a streaming regression test**

In `internal/sandbox/workspace_test.go`, add these helpers near the existing mock infrastructure:

```go
type signalWriter struct {
	wrote chan struct{}
	once  sync.Once
	buf   bytes.Buffer
}

func newSignalWriter() *signalWriter {
	return &signalWriter{wrote: make(chan struct{})}
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.wrote) })
	return w.buf.Write(p)
}

type waitForWriterReader struct {
	firstReadDone bool
	wrote         <-chan struct{}
}

func (r *waitForWriterReader) Read(p []byte) (int, error) {
	if !r.firstReadDone {
		r.firstReadDone = true
		copy(p, "hello")
		return len("hello"), nil
	}
	select {
	case <-r.wrote:
		return 0, io.EOF
	case <-time.After(100 * time.Millisecond):
		return 0, fmt.Errorf("reader reached EOF before tar writer wrote anything")
	}
}

func (r *waitForWriterReader) Close() error {
	return nil
}
```

Then add the failing test:

```go
func TestWriteTarStream_DoesNotReadWholeFileBeforeWritingTar(t *testing.T) {
	writer := newSignalWriter()
	mock := &mockScopedFS{
		openReaders: map[string]io.ReadCloser{
			"big.txt": &waitForWriterReader{wrote: writer.wrote},
		},
	}
	entries := []fileEntry{
		{relPath: "big.txt", isDir: false, size: int64(len("hello")), modTime: time.Unix(1000, 0)},
	}

	mgr := &Manager{}
	err := mgr.writeTarStream(context.Background(), mock, entries, writer)
	require.NoError(t, err)
}
```

Also extend `mockScopedFS` so `Open` can return a custom reader:

```go
type mockScopedFS struct {
	files       map[string][]byte
	dirs        map[string][]fs.FileInfo
	openErr     map[string]error
	openReaders map[string]io.ReadCloser
}

func (m *mockScopedFS) Open(_ context.Context, p string, _ ...fs.Option) (io.ReadCloser, error) {
	if m.openErr != nil {
		if err, ok := m.openErr[p]; ok {
			return nil, err
		}
	}
	if m.openReaders != nil {
		if r, ok := m.openReaders[p]; ok {
			return r, nil
		}
	}
	content, ok := m.files[p]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", p)
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}
```

Add `sync` to the test import block.

- [ ] **Step 2: Run the regression test and confirm it fails**

Run: `go test ./internal/sandbox -run TestWriteTarStream_DoesNotReadWholeFileBeforeWritingTar -v`

Expected: FAIL with `reader reached EOF before tar writer wrote anything`.

- [ ] **Step 3: Replace the buffered `readResult` implementation**

In `internal/sandbox/workspace.go`, replace the `readResult` type inside `writeTarStream` with:

```go
type readResult struct {
	reader io.ReadCloser
	size   int64
	done   chan struct{}
	err    error
}
```

Replace the "start every file goroutine up front" logic with a bounded prefetch window. Do not use a semaphore here: the window itself is the limit, and it guarantees the main loop never waits on an index whose goroutine is still queued behind later entries.

```go
resultChs := make([]chan readResult, len(entries))
fileIndexes := make([]int, 0, len(entries))
for i, e := range entries {
	if !e.isDir {
		fileIndexes = append(fileIndexes, i)
	}
}

startRead := func(i int) {
	ch := make(chan readResult, 1)
	resultChs[i] = ch
	entry := entries[i]
	go func() {
		if ctx.Err() != nil {
			ch <- readResult{err: ctx.Err()}
			return
		}
		reader, err := scoped.Open(ctx, entry.relPath)
		if err != nil {
			ch <- readResult{err: fmt.Errorf("open %q: %w", entry.relPath, err)}
			return
		}
		done := make(chan struct{})
		ch <- readResult{reader: reader, size: entry.size, done: done}
		<-done
	}()
}

nextFile := 0
for nextFile < len(fileIndexes) && nextFile < maxConcurrentReads {
	startRead(fileIndexes[nextFile])
	nextFile++
}
```

Add these helpers before the main loop:

```go
releaseResult := func(res readResult) {
	if res.reader != nil {
		_ = res.reader.Close()
	}
	if res.done != nil {
		close(res.done)
	}
}

cleanup := func(start int) {
	for _, j := range fileIndexes {
		if j < start || resultChs[j] == nil {
			continue
		}
		res := <-resultChs[j]
		releaseResult(res)
	}
}
```

Replace the file-entry write block with:

```go
res := <-resultChs[i]
if res.err != nil {
	cleanup(i + 1)
	return res.err
}

if err := tw.WriteHeader(&tar.Header{
	Name:    e.relPath,
	Size:    res.size,
	Mode:    0644,
	ModTime: e.modTime,
	Uid:     1000,
	Gid:     1000,
	Format:  tar.FormatPAX,
}); err != nil {
	releaseResult(res)
	cleanup(i + 1)
	return fmt.Errorf("write file header %q: %w", e.relPath, err)
}

if _, err := io.Copy(tw, res.reader); err != nil {
	releaseResult(res)
	cleanup(i + 1)
	return fmt.Errorf("write file content %q: %w", e.relPath, err)
}
releaseResult(res)
if nextFile < len(fileIndexes) {
	startRead(fileIndexes[nextFile])
	nextFile++
}
```

- [ ] **Step 4: Run focused workspace tests**

Run: `go test ./internal/sandbox -run 'TestWriteTarStream|TestCollectFiles' -v`

Expected: PASS for all `TestWriteTarStream_*` and `TestCollectFiles`.

- [ ] **Step 5: Run the sandbox package tests**

Run: `go test ./internal/sandbox`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/sandbox/workspace.go internal/sandbox/workspace_test.go
git commit -m "perf: stream workspace tar uploads"
```

---

### Task 2: Split Changed-File Download Into Direct and Tar Paths

**Files:**
- Modify: `internal/sandbox/workspace.go`
- Modify: `internal/sandbox/workspace_test.go`
- Modify: `internal/sandbox/pool_test.go`
- Modify: `go.mod`
- Modify: `go.sum`

- [ ] **Step 1: Add test support for changed-file downloads**

Extend `mockRuntime` in `internal/sandbox/pool_test.go`:

```go
type mockRuntime struct {
	mu        sync.Mutex
	created   int
	removed   int
	sandboxes map[string]*runtime.SandboxInfo

	readFileContent map[string][]byte
	readFilePaths   []string
	downloadDirData []byte
	downloadDirCalls int
}
```

Update `newMockRuntime`:

```go
func newMockRuntime() *mockRuntime {
	return &mockRuntime{
		sandboxes:       make(map[string]*runtime.SandboxInfo),
		readFileContent: make(map[string][]byte),
	}
}
```

Replace `DownloadDir` and `ReadFileContent` with:

```go
func (m *mockRuntime) DownloadDir(context.Context, string, string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.downloadDirCalls++
	return io.NopCloser(bytes.NewReader(m.downloadDirData)), nil
}

func (m *mockRuntime) ReadFileContent(_ context.Context, _ string, path string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readFilePaths = append(m.readFilePaths, path)
	data, ok := m.readFileContent[path]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", path)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
```

Add `bytes` and `fmt` to the `pool_test.go` import block.

Extend `mockScopedFS.Create` in `internal/sandbox/workspace_test.go` to record writes:

```go
type mockScopedFS struct {
	files       map[string][]byte
	dirs        map[string][]fs.FileInfo
	openErr     map[string]error
	openReaders map[string]io.ReadCloser
	writes      map[string]*bytes.Buffer
}

type bufferWriteCloser struct {
	*bytes.Buffer
}

func (w bufferWriteCloser) Close() error {
	return nil
}

func (m *mockScopedFS) Create(_ context.Context, p string, _ ...fs.Option) (io.WriteCloser, error) {
	if m.writes == nil {
		m.writes = make(map[string]*bytes.Buffer)
	}
	buf := &bytes.Buffer{}
	m.writes[p] = buf
	return bufferWriteCloser{Buffer: buf}, nil
}
```

- [ ] **Step 2: Add failing dispatch tests**

In `internal/sandbox/workspace_test.go`, add:

```go
func TestDownloadChangedFiles_UsesDirectPathForSmallChangeSet(t *testing.T) {
	rt := newMockRuntime()
	rt.readFileContent["/workspace/a.txt"] = []byte("aaa")
	rt.readFileContent["/workspace/b.txt"] = []byte("bbb")
	scoped := &mockScopedFS{}
	mgr := NewManager(rt, nil, nil, ManagerConfig{})

	err := mgr.downloadChangedFiles(context.Background(), scoped, "runtime-1", map[string]struct{}{
		"a.txt": {},
		"b.txt": {},
	}, nil)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"/workspace/a.txt", "/workspace/b.txt"}, rt.readFilePaths)
	assert.Equal(t, 0, rt.downloadDirCalls)
	assert.Equal(t, "aaa", scoped.writes["a.txt"].String())
	assert.Equal(t, "bbb", scoped.writes["b.txt"].String())
}

func TestDownloadChangedFiles_UsesTarPathForLargeChangeSet(t *testing.T) {
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "workspace/a.txt", Size: 3, Mode: 0644}))
	_, _ = tw.Write([]byte("aaa"))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "workspace/b.txt", Size: 3, Mode: 0644}))
	_, _ = tw.Write([]byte("bbb"))
	require.NoError(t, tw.Close())

	rt := newMockRuntime()
	rt.downloadDirData = tarBuf.Bytes()
	scoped := &mockScopedFS{}
	mgr := NewManager(rt, nil, nil, ManagerConfig{SingleFileDownloadThreshold: 1})

	err := mgr.downloadChangedFiles(context.Background(), scoped, "runtime-1", map[string]struct{}{
		"a.txt": {},
		"b.txt": {},
	}, nil)
	require.NoError(t, err)

	assert.Empty(t, rt.readFilePaths)
	assert.Equal(t, 1, rt.downloadDirCalls)
	assert.Equal(t, "aaa", scoped.writes["a.txt"].String())
	assert.Equal(t, "bbb", scoped.writes["b.txt"].String())
}
```

- [ ] **Step 3: Run the tests and confirm they fail**

Run: `go test ./internal/sandbox -run TestDownloadChangedFiles_UsesDirectPathForSmallChangeSet -v`

Expected: FAIL because current code calls `DownloadDir` instead of `ReadFileContent`, so `rt.downloadDirCalls` is `1` and `rt.readFilePaths` is empty.

- [ ] **Step 4: Add threshold configuration and constants**

In `internal/sandbox/manager.go`, extend `ManagerConfig`:

```go
type ManagerConfig struct {
	PoolConfig                  PoolConfig
	DefaultTimeout              int
	ExecTimeoutSeconds          int
	AutoSyncIntervalSeconds     int
	SingleFileDownloadThreshold int
}
```

In `internal/sandbox/workspace.go`, add:

```go
const defaultSingleFileDownloadThreshold = 5
const maxConcurrentStorageWrites = 4

func (m *Manager) singleFileDownloadThreshold() int {
	if m.config.SingleFileDownloadThreshold > 0 {
		return m.config.SingleFileDownloadThreshold
	}
	return defaultSingleFileDownloadThreshold
}
```

- [ ] **Step 5: Add `errgroup` dependency**

Run: `go get golang.org/x/sync/errgroup`

Expected: `go.mod` gains a `golang.org/x/sync` requirement and `go.sum` is updated.

- [ ] **Step 6: Implement direct and tar download helpers**

In `internal/sandbox/workspace.go`, replace `downloadChangedFiles` with:

```go
func (m *Manager) downloadChangedFiles(ctx context.Context, scoped storage.ScopedFS, runtimeID string, changedSet map[string]struct{}, exclude []string) error {
	if len(changedSet) <= m.singleFileDownloadThreshold() {
		return m.downloadFilesDirect(ctx, scoped, runtimeID, changedSet)
	}
	return m.downloadFilesViaTar(ctx, scoped, runtimeID, changedSet, exclude)
}
```

Add:

```go
func (m *Manager) downloadFilesDirect(ctx context.Context, scoped storage.ScopedFS, runtimeID string, changedSet map[string]struct{}) error {
	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(maxConcurrentStorageWrites)

	for p := range changedSet {
		p := p
		eg.Go(func() error {
			fileCtx, cancelFile := context.WithCancel(egCtx)
			defer cancelFile()

			rc, err := m.runtime.ReadFileContent(fileCtx, runtimeID, "/workspace/"+p)
			if err != nil {
				return fmt.Errorf("read %q: %w", p, err)
			}
			defer func() {
				cancelFile()
				_ = rc.Close()
			}()

			w, err := scoped.Create(egCtx, p, contentTypeOpt(p))
			if err != nil {
				cancelFile()
				return fmt.Errorf("create %q: %w", p, err)
			}
			closed := false
			defer func() {
				if !closed {
					_ = w.Close()
				}
			}()

			if _, err := io.Copy(w, rc); err != nil {
				cancelFile()
				return fmt.Errorf("write %q: %w", p, err)
			}
			if err := w.Close(); err != nil {
				closed = true
				return fmt.Errorf("flush %q: %w", p, err)
			}
			closed = true
			return nil
		})
	}

	return eg.Wait()
}
```

Rename the old tar implementation to `downloadFilesViaTar`, and add `io.Copy(io.Discard, tr)` for unchanged regular files:

```go
if _, changed := changedSet[name]; !changed {
	if _, err := io.Copy(io.Discard, tr); err != nil {
		return fmt.Errorf("discard tar entry %q: %w", name, err)
	}
	continue
}
```

- [ ] **Step 7: Run focused tests**

Run: `go test ./internal/sandbox -run 'TestDownloadChangedFiles_|TestWriteTarStream' -v`

Expected: PASS.

- [ ] **Step 8: Run sandbox package tests**

Run: `go test ./internal/sandbox`

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add go.mod go.sum internal/sandbox/manager.go internal/sandbox/workspace.go internal/sandbox/workspace_test.go internal/sandbox/pool_test.go
git commit -m "perf: optimize changed workspace downloads"
```

---

### Task 3: Add Config, Storage Metadata, Runtime Spec, and Sync Mode Types

**Files:**
- Modify: `internal/config/config.go`
- Modify: `internal/config/config_test.go`
- Modify: `configs/config.yaml`
- Modify: `internal/storage/filesystem.go`
- Modify: `internal/storage/filesystem_test.go`
- Modify: `internal/runtime/types.go`
- Modify: `internal/sandbox/types.go`
- Modify: `internal/sandbox/manager.go`
- Modify: `cmd/sandbox/main.go`

- [ ] **Step 1: Add failing config tests**

In `internal/config/config_test.go`, extend `TestLoadDefaults`:

```go
assert.Equal(t, 0, cfg.Workspace.SingleFileDownloadThreshold)
assert.Equal(t, "", cfg.Workspace.SyncSecretRef)
assert.Equal(t, "", cfg.Workspace.SyncImage)
assert.Empty(t, cfg.Workspace.SyncEgressFQDNs)
assert.Empty(t, cfg.Workspace.SyncEgressCIDRs)
```

In `TestLoadFromYAML`, add a `workspace` section before `security`:

```yaml
workspace:
  auto_sync_interval_seconds: 30
  single_file_download_threshold: 7
  sync_secret_ref: "sandbox-storage-secret"
  sync_image: "registry.example.com/workspace-sync:1.0"
  sync_egress_fqdns:
    - "s3.amazonaws.com"
  sync_egress_cidrs:
    - "203.0.113.0/24"
```

Add assertions:

```go
assert.Equal(t, 30, cfg.Workspace.AutoSyncIntervalSeconds)
assert.Equal(t, 7, cfg.Workspace.SingleFileDownloadThreshold)
assert.Equal(t, "sandbox-storage-secret", cfg.Workspace.SyncSecretRef)
assert.Equal(t, "registry.example.com/workspace-sync:1.0", cfg.Workspace.SyncImage)
assert.Equal(t, []string{"s3.amazonaws.com"}, cfg.Workspace.SyncEgressFQDNs)
assert.Equal(t, []string{"203.0.113.0/24"}, cfg.Workspace.SyncEgressCIDRs)
```

Add validation tests:

```go
func TestValidateWorkspaceSyncRequiresSecretAndImageTogether(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")
	t.Setenv("SANDBOX_RUNTIME_TYPE", "kubernetes")
	t.Setenv("SANDBOX_RUNTIME_KUBERNETES_NAMESPACE", "sandbox")
	t.Setenv("SANDBOX_WORKSPACE_SYNC_SECRET_REF", "sandbox-storage-secret")

	_, err := config.Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sync_secret_ref")
	assert.Contains(t, err.Error(), "sync_image")
}

func TestValidateWorkspaceSyncRejectedForDockerRuntime(t *testing.T) {
	t.Setenv("SANDBOX_SECURITY_API_KEY", "test-key")
	t.Setenv("SANDBOX_RUNTIME_TYPE", "docker")
	t.Setenv("SANDBOX_WORKSPACE_SYNC_SECRET_REF", "sandbox-storage-secret")
	t.Setenv("SANDBOX_WORKSPACE_SYNC_IMAGE", "registry.example.com/workspace-sync:1.0")

	_, err := config.Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace sync init container requires runtime.type")
}
```

- [ ] **Step 2: Run config tests and confirm failure**

Run: `go test ./internal/config -run 'TestLoadDefaults|TestLoadFromYAML|TestValidateWorkspaceSync' -v`

Expected: FAIL because config fields do not exist yet.

- [ ] **Step 3: Add config fields, defaults, and validation**

In `internal/config/config.go`, extend `WorkspaceConfig`:

```go
type WorkspaceConfig struct {
	AutoSyncIntervalSeconds     int      `mapstructure:"auto_sync_interval_seconds"`
	SingleFileDownloadThreshold int      `mapstructure:"single_file_download_threshold"`
	SyncSecretRef               string   `mapstructure:"sync_secret_ref"`
	SyncImage                   string   `mapstructure:"sync_image"`
	SyncEgressFQDNs             []string `mapstructure:"sync_egress_fqdns"`
	SyncEgressCIDRs             []string `mapstructure:"sync_egress_cidrs"`
}
```

In `setDefaults`, add:

```go
v.SetDefault("workspace.single_file_download_threshold", 0)
v.SetDefault("workspace.sync_secret_ref", "")
v.SetDefault("workspace.sync_image", "")
v.SetDefault("workspace.sync_egress_fqdns", []string{})
v.SetDefault("workspace.sync_egress_cidrs", []string{})
```

In `Validate`, add after the auto-sync validation:

```go
if c.Workspace.SingleFileDownloadThreshold < 0 {
	return fmt.Errorf("config: workspace.single_file_download_threshold must be >= 0, got %d", c.Workspace.SingleFileDownloadThreshold)
}
syncSecretSet := c.Workspace.SyncSecretRef != ""
syncImageSet := c.Workspace.SyncImage != ""
if syncSecretSet != syncImageSet {
	return fmt.Errorf("config: workspace.sync_secret_ref and workspace.sync_image must be configured together")
}
if syncSecretSet && c.Runtime.Type != "kubernetes" {
	return fmt.Errorf("config: workspace sync init container requires runtime.type \"kubernetes\", got %q", c.Runtime.Type)
}
```

- [ ] **Step 4: Add runtime and sandbox data types**

In `internal/runtime/types.go`, add before `SandboxSpec`:

```go
type WorkspaceSyncSpec struct {
	Image       string
	RootPath    string
	Provider    string
	Endpoint    string
	Region      string
	Bucket      string
	UseSSL      bool
	SecretRef   string
	EgressFQDNs []string
	EgressCIDRs []string
	SyncExclude []string
}
```

Add to `SandboxSpec`:

```go
WorkspaceSync *WorkspaceSyncSpec
```

In `internal/sandbox/types.go`, add:

```go
const (
	WorkspaceSyncModeAPI           = "api"
	WorkspaceSyncModeInitContainer = "init_container"
	WorkspaceSyncModeBindMount     = "bind_mount"
)
```

Add to `WorkspaceInfo`:

```go
SyncMode string `json:"sync_mode,omitempty"`
```

- [ ] **Step 5: Preserve storage metadata**

In `internal/storage/filesystem.go`, extend `FileSystemMeta`:

```go
type FileSystemMeta struct {
	Provider  StorageProvider
	LocalPath string
	Endpoint  string
	Region    string
	Bucket    string
	SubPath   string
	UseSSL    bool
}
```

Initialize metadata in `NewFileSystem`:

```go
meta := &FileSystemMeta{
	Provider: StorageProvider(cfg.Provider),
	Endpoint: cfg.Endpoint,
	Region:   cfg.Region,
	Bucket:   cfg.Bucket,
	SubPath:  cfg.SubPath,
	UseSSL:   cfg.UseSSL,
}
```

Keep `meta.LocalPath = cfg.LocalPath` in the local provider branch.

- [ ] **Step 6: Add storage metadata tests**

In `internal/storage/filesystem_test.go`, change the local filesystem config in `TestNewFileSystem_Local` to include metadata fields:

```go
cfg := config.FileSystemConfig{
	Provider:  "local",
	LocalPath: dir,
	SubPath:   "workspaces",
	UseSSL:    true,
}
```

Then add these assertions after the existing provider/local path assertions:

```go
assert.Equal(t, "workspaces", meta.SubPath)
assert.True(t, meta.UseSSL)
assert.Equal(t, "", meta.Endpoint)
assert.Equal(t, "", meta.Region)
assert.Equal(t, "", meta.Bucket)
```

Add a remote-provider metadata test so object-storage init-container fields are covered without relying on the local-provider branch:

```go
func TestNewFileSystem_S3Metadata(t *testing.T) {
	cfg := config.FileSystemConfig{
		Provider:  "s3",
		Bucket:    "bucket",
		Region:    "us-east-1",
		Endpoint:  "https://s3.amazonaws.com",
		AccessKey: "access-key",
		SecretKey: "secret-key",
		SubPath:   "root",
		UseSSL:    true,
	}

	fsys, meta, err := NewFileSystem(cfg)
	require.NoError(t, err)
	assert.NotNil(t, fsys)
	assert.Equal(t, ProviderS3, meta.Provider)
	assert.Equal(t, "", meta.LocalPath)
	assert.Equal(t, "https://s3.amazonaws.com", meta.Endpoint)
	assert.Equal(t, "us-east-1", meta.Region)
	assert.Equal(t, "bucket", meta.Bucket)
	assert.Equal(t, "root", meta.SubPath)
	assert.True(t, meta.UseSSL)
}
```

- [ ] **Step 7: Wire config into manager**

In `internal/sandbox/manager.go`, extend `ManagerConfig`:

```go
WorkspaceSyncEnabled     bool
WorkspaceSyncSecretRef   string
WorkspaceSyncImage       string
WorkspaceSyncEgressFQDNs []string
WorkspaceSyncEgressCIDRs []string
```

In `cmd/sandbox/main.go`, set:

```go
SingleFileDownloadThreshold: cfg.Workspace.SingleFileDownloadThreshold,
WorkspaceSyncEnabled:        cfg.Workspace.SyncSecretRef != "" && cfg.Workspace.SyncImage != "",
WorkspaceSyncSecretRef:      cfg.Workspace.SyncSecretRef,
WorkspaceSyncImage:          cfg.Workspace.SyncImage,
WorkspaceSyncEgressFQDNs:    cfg.Workspace.SyncEgressFQDNs,
WorkspaceSyncEgressCIDRs:    cfg.Workspace.SyncEgressCIDRs,
```

- [ ] **Step 8: Document config**

In `configs/config.yaml`, under `workspace`, add:

```yaml
  # Changed file count <= this threshold uses per-file download from the container.
  # Set to 0 to use the default threshold (5).
  single_file_download_threshold: 0

  # Kubernetes-only workspace sync init container. Both secret and image must be set.
  # Docker runtime rejects these settings.
  sync_secret_ref: ""
  sync_image: ""
  # Cilium clusters can use FQDN bootstrap egress.
  sync_egress_fqdns: []
  # Standard NetworkPolicy clusters must use stable CIDRs for object storage egress.
  sync_egress_cidrs: []
```

- [ ] **Step 9: Run focused tests**

Run: `go test ./internal/config ./internal/storage ./internal/runtime ./internal/sandbox`

Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add cmd/sandbox/main.go configs/config.yaml internal/config/config.go internal/config/config_test.go internal/runtime/types.go internal/sandbox/manager.go internal/sandbox/types.go internal/storage/filesystem.go internal/storage/filesystem_test.go
git commit -m "feat: add workspace sync configuration model"
```

---

### Task 4: Use Init-Container Sync Mode in Manager Creation and Workspace Registration

**Files:**
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/workspace.go`
- Modify: `internal/sandbox/manager_test.go`
- Modify: `internal/sandbox/pool_test.go`
- Modify: `internal/sandbox/workspace_test.go`

- [ ] **Step 1: Add failing manager tests for pool bypass and sync mode**

In `internal/sandbox/pool_test.go`, extend `mockRuntime`:

```go
type mockRuntime struct {
	mu        sync.Mutex
	created   int
	removed   int
	sandboxes map[string]*runtime.SandboxInfo

	readFileContent map[string][]byte
	readFilePaths   []string
	downloadDirData []byte
	downloadDirCalls int

	lastSpec      runtime.SandboxSpec
	execPipeCalls int
}
```

Set `lastSpec` in `CreateSandbox` while the mutex is held:

```go
m.lastSpec = spec
```

Replace `ExecPipe` so `MountWorkspace` tests do not deadlock on the pipe writer:

```go
func (m *mockRuntime) ExecPipe(_ context.Context, _ string, _ []string, stdin io.Reader) error {
	m.mu.Lock()
	m.execPipeCalls++
	m.mu.Unlock()
	_, err := io.Copy(io.Discard, stdin)
	return err
}
```

In `internal/sandbox/workspace_test.go`, add the `fs.FileSystem`-only stubs to `mockScopedFS` so the same fake can be passed to `NewManager` and then wrapped by `storage.NewScopedFS`:

```go
func (m *mockScopedFS) GetMimeType(context.Context, string, ...fs.Option) (string, error) {
	return "", nil
}

func (m *mockScopedFS) SetMetadata(context.Context, string, map[string]interface{}, ...fs.Option) error {
	return nil
}

func (m *mockScopedFS) GetMetadata(context.Context, string, ...fs.Option) (map[string]interface{}, error) {
	return nil, nil
}

func (m *mockScopedFS) Uploader() fs.Uploader {
	return nil
}
```

In `internal/sandbox/manager_test.go`, add imports:

```go
import (
	"context"
	"testing"
	"time"

	"github.com/goairix/fs"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)
```

Then add:

```go
func TestManager_CreateRemoteWorkspaceSyncBypassesPool(t *testing.T) {
	rt := newMockRuntime()
	fsys := &mockScopedFS{}
	mgr := NewManager(rt, fsys, &storage.FileSystemMeta{
		Provider: storage.ProviderS3,
		Endpoint: "https://s3.amazonaws.com",
		Region:   "us-east-1",
		Bucket:   "bucket",
		SubPath:  "root",
	}, ManagerConfig{
		PoolConfig:              PoolConfig{MinSize: 1, MaxSize: 5, Image: "sandbox:latest"},
		DefaultTimeout:          30,
		WorkspaceSyncEnabled:    true,
		WorkspaceSyncSecretRef:  "sandbox-storage-secret",
		WorkspaceSyncImage:      "registry.example.com/workspace-sync:1.0",
		WorkspaceSyncEgressFQDNs: []string{"s3.amazonaws.com"},
	})

	sb, err := mgr.Create(context.Background(), SandboxConfig{
		Mode:          ModeEphemeral,
		WorkspacePath: "project-a",
	})
	require.NoError(t, err)

	require.NotNil(t, sb.Workspace)
	assert.Equal(t, WorkspaceSyncModeInitContainer, sb.Workspace.SyncMode)
	assert.Equal(t, 1, rt.created)
	assert.Equal(t, "container-"+sb.ID, sb.RuntimeID)

	require.NotNil(t, rt.lastSpec.WorkspaceSync)
	assert.Equal(t, "root/project-a", rt.lastSpec.WorkspaceSync.RootPath)
	assert.Equal(t, "sandbox-storage-secret", rt.lastSpec.WorkspaceSync.SecretRef)
}

func TestManager_MountWorkspaceAlwaysUsesAPISync(t *testing.T) {
	rt := newMockRuntime()
	fsys := &mockScopedFS{
		dirs: map[string][]fs.FileInfo{
			"project-a": {
				mockFileInfo{name: "hello.txt", size: 5, modTime: time.Unix(1000, 0)},
			},
		},
		files: map[string][]byte{
			"project-a/hello.txt": []byte("hello"),
		},
	}
	mgr := NewManager(rt, fsys, &storage.FileSystemMeta{
		Provider: storage.ProviderS3,
		Endpoint: "https://s3.amazonaws.com",
		Region:   "us-east-1",
		Bucket:   "bucket",
		SubPath:  "root",
	}, ManagerConfig{
		PoolConfig:              PoolConfig{MinSize: 1, MaxSize: 5, Image: "sandbox:latest"},
		DefaultTimeout:          30,
		WorkspaceSyncEnabled:    true,
		WorkspaceSyncSecretRef:  "sandbox-storage-secret",
		WorkspaceSyncImage:      "registry.example.com/workspace-sync:1.0",
		WorkspaceSyncEgressFQDNs: []string{"s3.amazonaws.com"},
	})
	sb := &Sandbox{ID: "sandbox-1", RuntimeID: "runtime-1", State: StateReady}
	mgr.mu.Lock()
	mgr.sandboxes[sb.ID] = sb
	mgr.mu.Unlock()

	err := mgr.MountWorkspace(context.Background(), sb.ID, "project-a", nil)
	require.NoError(t, err)
	require.NotNil(t, sb.Workspace)
	assert.Equal(t, WorkspaceSyncModeAPI, sb.Workspace.SyncMode)

	rt.mu.Lock()
	execPipeCalls := rt.execPipeCalls
	rt.mu.Unlock()
	assert.Equal(t, 1, execPipeCalls)
}
```

- [ ] **Step 2: Run test and confirm failure**

Run: `go test ./internal/sandbox -run 'TestManager_CreateRemoteWorkspaceSyncBypassesPool|TestManager_MountWorkspaceAlwaysUsesAPISync' -v`

Expected: FAIL because manager still acquires from pool and `WorkspaceSync` is not populated; `MountWorkspace` also records an empty sync mode instead of `WorkspaceSyncModeAPI`.

- [ ] **Step 3: Add helper functions**

In `internal/sandbox/manager.go`, add:

```go
func (m *Manager) shouldUseWorkspaceInitContainer(workspacePath string) bool {
	return workspacePath != "" &&
		m.config.WorkspaceSyncEnabled &&
		m.fsMeta != nil &&
		m.fsMeta.Provider != storage.ProviderLocal
}

func objectStorageRootPath(meta *storage.FileSystemMeta, workspacePath string) string {
	if meta == nil {
		return path.Clean(workspacePath)
	}
	return path.Join(meta.SubPath, workspacePath)
}

func (m *Manager) workspaceSyncEgressFQDNs() []string {
	if len(m.config.WorkspaceSyncEgressFQDNs) > 0 {
		return append([]string(nil), m.config.WorkspaceSyncEgressFQDNs...)
	}
	if m.fsMeta == nil || m.fsMeta.Endpoint == "" {
		return nil
	}
	host := strings.TrimSpace(m.fsMeta.Endpoint)
	if u, err := url.Parse(host); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimPrefix(host, "https://")
	host = strings.Trim(host, "/")
	if host == "" || net.ParseIP(host) != nil {
		return nil
	}
	return []string{host}
}

func (m *Manager) workspaceSyncEgressCIDRs() []string {
	return append([]string(nil), m.config.WorkspaceSyncEgressCIDRs...)
}
```

Add imports `net`, `net/url`, and `path`; keep existing `path/filepath` and `strings` imports for local-path handling.

- [ ] **Step 4: Populate `WorkspaceSync` in `buildSpec`**

At the end of `buildSpec`, before returning, build into a local variable:

```go
spec := runtime.SandboxSpec{
	ID:                  id,
	Image:               m.config.PoolConfig.Image,
	Memory:              cfg.Resources.Memory,
	MemoryRequest:       m.config.PoolConfig.MemoryRequest,
	CPU:                 cfg.Resources.CPU,
	CPURequest:          m.config.PoolConfig.CPURequest,
	Disk:                cfg.Resources.Disk,
	NetworkEnabled:      cfg.Network.Enabled,
	NetworkWhitelist:    cfg.Network.Whitelist,
	NetworkBlockPrivate: cfg.Network.BlockPrivate,
	ReadOnlyRootFS:      false,
	RunAsUser:           1000,
	PidLimit:            100,
	Labels:              map[string]string{"sandbox.id": id},
}
if m.shouldUseWorkspaceInitContainer(cfg.WorkspacePath) {
	spec.WorkspaceSync = &runtime.WorkspaceSyncSpec{
		Image:       m.config.WorkspaceSyncImage,
		RootPath:    objectStorageRootPath(m.fsMeta, cfg.WorkspacePath),
		Provider:    string(m.fsMeta.Provider),
		Endpoint:    m.fsMeta.Endpoint,
		Region:      m.fsMeta.Region,
		Bucket:      m.fsMeta.Bucket,
		UseSSL:      m.fsMeta.UseSSL,
		SecretRef:   m.config.WorkspaceSyncSecretRef,
		EgressFQDNs: m.workspaceSyncEgressFQDNs(),
		EgressCIDRs: m.workspaceSyncEgressCIDRs(),
		SyncExclude: cfg.WorkspaceSyncExclude,
	}
}
return spec
```

Preserve current CPU/memory behavior if the existing `PoolConfig` fields differ; run `go test` after edits to catch mismatches.

- [ ] **Step 5: Bypass pool and register init-mode workspaces**

In `Create`, compute:

```go
useBindMount := cfg.WorkspacePath != "" && m.fsMeta != nil && m.fsMeta.Provider == storage.ProviderLocal
useSyncInitCtr := m.shouldUseWorkspaceInitContainer(cfg.WorkspacePath)
```

Change direct creation to:

```go
if cfg.Network.Enabled || useBindMount || useSyncInitCtr {
```

Replace the auto-mount workspace block with a switch:

```go
if cfg.WorkspacePath != "" {
	switch {
	case bindMounted:
		scoped, fsErr := storage.NewScopedFS(m.filesystem, cfg.WorkspacePath)
		if fsErr == nil {
			err = m.registerWorkspaceWithMode(spanCtx, id, scoped, cfg.WorkspacePath, cfg.WorkspaceSyncExclude, true, WorkspaceSyncModeBindMount)
		} else {
			err = fmt.Errorf("create scoped filesystem: %w", fsErr)
		}
	case useSyncInitCtr:
		scoped, fsErr := storage.NewScopedFS(m.filesystem, cfg.WorkspacePath)
		if fsErr == nil {
			err = m.registerWorkspaceWithMode(spanCtx, id, scoped, cfg.WorkspacePath, cfg.WorkspaceSyncExclude, false, WorkspaceSyncModeInitContainer)
		} else {
			err = fmt.Errorf("create scoped filesystem: %w", fsErr)
		}
	default:
		err = m.MountWorkspace(spanCtx, id, cfg.WorkspacePath, cfg.WorkspaceSyncExclude)
	}
	if err != nil {
		_ = m.runtime.RemoveSandbox(spanCtx, info.RuntimeID)
		m.mu.Lock()
		delete(m.sandboxes, id)
		m.mu.Unlock()
		metrics.RecordSandboxCreate(spanCtx, source, "error", 0)
		metrics.RecordError(spanCtx, "mount_workspace_failed")
		return nil, fmt.Errorf("mount workspace: %w", err)
	}
}
```

- [ ] **Step 6: Replace workspace registration helper**

In `internal/sandbox/workspace.go`, after `MountWorkspace` creates and syncs `scoped`, replace the inline registration with:

```go
return m.registerWorkspaceWithMode(ctx, sandboxID, scoped, rootPath, exclude, false, WorkspaceSyncModeAPI)
```

In `internal/sandbox/manager.go`, replace `registerWorkspace` with:

```go
func (m *Manager) registerWorkspaceWithMode(ctx context.Context, sandboxID string, scoped storage.ScopedFS, rootPath string, exclude []string, bindMounted bool, syncMode string) error {
	if scoped == nil {
		return fmt.Errorf("scoped filesystem is nil")
	}
	now := time.Now()
	m.mu.Lock()
	sb := m.sandboxes[sandboxID]
	m.workspaces[sandboxID] = scoped
	sb.Workspace = &WorkspaceInfo{
		RootPath:     rootPath,
		MountedAt:    now,
		LastSyncedAt: now,
		BindMounted:  bindMounted,
		SyncExclude:  exclude,
		SyncMode:     syncMode,
	}
	sb.UpdatedAt = now
	m.mu.Unlock()
	if m.sessions != nil {
		_ = m.sessions.Save(ctx, sb)
	}
	return nil
}
```

- [ ] **Step 7: Run manager tests**

Run: `go test ./internal/sandbox -run 'TestManager_CreateRemoteWorkspaceSyncBypassesPool|TestManager_MountWorkspaceAlwaysUsesAPISync|TestManager_Create|TestManager_Destroy' -v`

Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/sandbox/manager.go internal/sandbox/workspace.go internal/sandbox/manager_test.go internal/sandbox/pool_test.go internal/sandbox/workspace_test.go
git commit -m "feat: route remote workspace creation through init sync"
```

---

### Task 5: Inject the Workspace Sync Init Container

**Files:**
- Modify: `internal/runtime/kubernetes/pod.go`
- Create: `internal/runtime/kubernetes/pod_test.go`

- [ ] **Step 1: Add failing pod injection test**

Create `internal/runtime/kubernetes/pod_test.go`:

```go
package kubernetes

import (
	"context"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCreatePodInjectsWorkspaceSyncInitContainer(t *testing.T) {
	client := fake.NewSimpleClientset()
	spec := runtime.SandboxSpec{
		ID:        "sandbox-1",
		Image:     "sandbox:latest",
		RunAsUser: 1000,
		WorkspaceSync: &runtime.WorkspaceSyncSpec{
			Image:       "registry.example.com/workspace-sync:1.0",
			Provider:    "s3",
			Endpoint:    "https://s3.amazonaws.com",
			Region:      "us-east-1",
			Bucket:      "bucket",
			UseSSL:      true,
			RootPath:    "root/project-a",
			SecretRef:   "sandbox-storage-secret",
			SyncExclude: []string{".cache", ".agent"},
		},
	}

	pod, err := createPod(context.Background(), client, "sandbox", spec)
	require.NoError(t, err)
	require.Len(t, pod.Spec.InitContainers, 1)

	init := pod.Spec.InitContainers[0]
	assert.Equal(t, "workspace-sync", init.Name)
	assert.Equal(t, "registry.example.com/workspace-sync:1.0", init.Image)
	assert.Contains(t, init.Command, "--provider=s3")
	assert.Contains(t, init.Command, "--endpoint=https://s3.amazonaws.com")
	assert.Contains(t, init.Command, "--region=us-east-1")
	assert.Contains(t, init.Command, "--bucket=bucket")
	assert.Contains(t, init.Command, "--use-ssl=true")
	assert.Contains(t, init.Command, "--src=root/project-a")
	assert.Contains(t, init.Command, "--dst=/workspace")
	assert.Contains(t, init.Command, "--exclude=.cache,.agent")
	require.Len(t, init.EnvFrom, 1)
	assert.Equal(t, "sandbox-storage-secret", init.EnvFrom[0].SecretRef.Name)
	require.Len(t, init.VolumeMounts, 1)
	assert.Equal(t, "workspace", init.VolumeMounts[0].Name)
	assert.Equal(t, "/workspace", init.VolumeMounts[0].MountPath)
	require.NotNil(t, init.SecurityContext)
	require.NotNil(t, init.SecurityContext.AllowPrivilegeEscalation)
	assert.False(t, *init.SecurityContext.AllowPrivilegeEscalation)
}
```

- [ ] **Step 2: Run test and confirm failure**

Run: `go test ./internal/runtime/kubernetes -run TestCreatePodInjectsWorkspaceSyncInitContainer -v`

Expected: FAIL because no init container is injected.

- [ ] **Step 3: Implement init container injection**

In `internal/runtime/kubernetes/pod.go`, before constructing the Pod, build:

```go
var initContainers []corev1.Container
if ws := spec.WorkspaceSync; ws != nil {
	initContainers = append(initContainers, corev1.Container{
		Name:  "workspace-sync",
		Image: ws.Image,
		Command: []string{
			"/bin/workspace-sync",
			"--provider=" + ws.Provider,
			"--endpoint=" + ws.Endpoint,
			"--region=" + ws.Region,
			"--bucket=" + ws.Bucket,
			"--use-ssl=" + strconv.FormatBool(ws.UseSSL),
			"--src=" + ws.RootPath,
			"--dst=/workspace",
			"--exclude=" + strings.Join(ws.SyncExclude, ","),
		},
		EnvFrom: []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: ws.SecretRef},
			},
		}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "workspace", MountPath: "/workspace"},
		},
		SecurityContext: &corev1.SecurityContext{
			AllowPrivilegeEscalation: &falseVal,
			RunAsUser:                &spec.RunAsUser,
		},
	})
}
```

Add `strconv` and `strings` to imports.

Set `InitContainers: initContainers,` in `corev1.PodSpec`.

- [ ] **Step 4: Run pod tests**

Run: `go test ./internal/runtime/kubernetes -run TestCreatePodInjectsWorkspaceSyncInitContainer -v`

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/runtime/kubernetes/pod.go internal/runtime/kubernetes/pod_test.go
git commit -m "feat: inject workspace sync init container"
```

---

### Task 6: Add K8s Bootstrap Egress Policy and Logical ID Cleanup

**Files:**
- Modify: `internal/runtime/kubernetes/network.go`
- Modify: `internal/runtime/kubernetes/runtime.go`
- Create: `internal/runtime/kubernetes/network_test.go`
- Create: `internal/runtime/kubernetes/runtime_test.go`

- [ ] **Step 1: Add policy unit tests**

Create `internal/runtime/kubernetes/network_test.go` with tests for standard CIDR and Cilium FQDN bootstrap:

```go
package kubernetes

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestApplyWorkspaceSyncBootstrapPolicyStandardCIDRs(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	ws := &runtime.WorkspaceSyncSpec{EgressCIDRs: []string{"203.0.113.0/24"}}

	cleanup, err := applyWorkspaceSyncBootstrapPolicy(context.Background(), client, nil, "sandbox", "logical-1", false, ws)
	require.NoError(t, err)
	require.NotNil(t, cleanup.OnFailure)
	require.NotNil(t, cleanup.AfterReady)

	policy, err := client.NetworkingV1().NetworkPolicies("sandbox").Get(context.Background(), "sandbox-logical-1", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"sandbox.id": "logical-1"}, policy.Spec.PodSelector.MatchLabels)
	assert.Contains(t, cidrsFromPolicy(policy), "203.0.113.0/24")
	assertPolicyAllowsDNS(t, policy)
	assertPolicyAllowsCIDRPort(t, policy, "203.0.113.0/24", 80)
	assertPolicyAllowsCIDRPort(t, policy, "203.0.113.0/24", 443)
}

func TestApplyWorkspaceSyncBootstrapPolicyCiliumFQDNs(t *testing.T) {
	client := k8sfake.NewSimpleClientset()
	dyn := fake.NewSimpleDynamicClient(k8sruntime.NewScheme())
	ws := &runtime.WorkspaceSyncSpec{EgressFQDNs: []string{"oss-cn-hangzhou.aliyuncs.com"}}

	cleanup, err := applyWorkspaceSyncBootstrapPolicy(context.Background(), client, dyn, "sandbox", "logical-1", true, ws)
	require.NoError(t, err)
	require.NotNil(t, cleanup.AfterReady)

	obj, err := dyn.Resource(ciliumNetworkPolicyGVR).Namespace("sandbox").Get(context.Background(), "sandbox-workspace-sync-logical-1", metav1.GetOptions{})
	require.NoError(t, err)
	selector, found, err := unstructured.NestedStringMap(obj.Object, "spec", "endpointSelector", "matchLabels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "logical-1", selector["sandbox.id"])
	egress, found, err := unstructured.NestedSlice(obj.Object, "spec", "egress")
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, ciliumEgressHasFQDN(egress, "oss-cn-hangzhou.aliyuncs.com"))
	assert.True(t, ciliumEgressHasPortProtocol(egress, "53", "UDP"), "missing UDP DNS allow")
	assert.True(t, ciliumEgressHasPortProtocol(egress, "53", "TCP"), "missing TCP DNS allow")
	assert.True(t, ciliumEgressHasPortProtocol(egress, "80", "TCP"))
	assert.True(t, ciliumEgressHasPortProtocol(egress, "443", "TCP"))

	cleanup.AfterReady(context.Background())
	_, err = dyn.Resource(ciliumNetworkPolicyGVR).Namespace("sandbox").Get(context.Background(), "sandbox-workspace-sync-logical-1", metav1.GetOptions{})
	require.Error(t, err)
}

func cidrsFromPolicy(policy *networkingv1.NetworkPolicy) []string {
	var cidrs []string
	for _, rule := range policy.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				cidrs = append(cidrs, peer.IPBlock.CIDR)
			}
		}
	}
	return cidrs
}

func assertPolicyAllowsDNS(t *testing.T, policy *networkingv1.NetworkPolicy) {
	t.Helper()
	assert.True(t, policyHasPort(policy, 53, corev1.ProtocolUDP), "missing UDP DNS allow")
	assert.True(t, policyHasPort(policy, 53, corev1.ProtocolTCP), "missing TCP DNS allow")
}

func assertPolicyAllowsCIDRPort(t *testing.T, policy *networkingv1.NetworkPolicy, cidr string, port int32) {
	t.Helper()
	assert.True(t, policyHasCIDRPort(policy, cidr, port), "missing TCP/%d allow for %s", port, cidr)
}

func policyHasPort(policy *networkingv1.NetworkPolicy, port int32, protocol corev1.Protocol) bool {
	for _, rule := range policy.Spec.Egress {
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == port && p.Protocol != nil && *p.Protocol == protocol {
				return true
			}
		}
	}
	return false
}

func policyHasCIDRPort(policy *networkingv1.NetworkPolicy, cidr string, port int32) bool {
	for _, rule := range policy.Spec.Egress {
		hasCIDR := false
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == cidr {
				hasCIDR = true
			}
		}
		if !hasCIDR {
			continue
		}
		for _, p := range rule.Ports {
			if p.Port != nil && p.Port.IntVal == port && p.Protocol != nil && *p.Protocol == corev1.ProtocolTCP {
				return true
			}
		}
	}
	return false
}

func ciliumEgressHasFQDN(egress []any, fqdn string) bool {
	for _, item := range egress {
		rule, ok := item.(map[string]any)
		if !ok {
			continue
		}
		toFQDNs, ok := rule["toFQDNs"].([]any)
		if !ok {
			continue
		}
		for _, entry := range toFQDNs {
			entryMap, ok := entry.(map[string]any)
			if ok && entryMap["matchName"] == fqdn {
				return true
			}
		}
	}
	return false
}

func ciliumEgressHasPortProtocol(egress []any, port, protocol string) bool {
	for _, item := range egress {
		rule, ok := item.(map[string]any)
		if !ok {
			continue
		}
		toPorts, ok := rule["toPorts"].([]any)
		if !ok {
			continue
		}
		for _, entry := range toPorts {
			entryMap, ok := entry.(map[string]any)
			if !ok {
				continue
			}
			ports, ok := entryMap["ports"].([]any)
			if !ok {
				continue
			}
			for _, p := range ports {
				portMap, ok := p.(map[string]any)
				if ok &&
					fmt.Sprint(portMap["port"]) == port &&
					strings.EqualFold(fmt.Sprint(portMap["protocol"]), protocol) {
					return true
				}
			}
		}
	}
	return false
}
```

Create `internal/runtime/kubernetes/runtime_test.go`:

```go
package kubernetes

import (
	"context"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

func TestPolicySandboxIDPrefersLogicalLabel(t *testing.T) {
	got := policySandboxID(runtime.SandboxSpec{
		ID:     "pod-name",
		Labels: map[string]string{"sandbox.id": "logical-id"},
	})
	assert.Equal(t, "logical-id", got)
}

func TestPolicySandboxIDFallsBackToSpecID(t *testing.T) {
	got := policySandboxID(runtime.SandboxSpec{ID: "pod-name"})
	assert.Equal(t, "pod-name", got)
}

func TestRemoveSandboxCleansLogicalAndRuntimePolicies(t *testing.T) {
	ctx := context.Background()
	client := k8sfake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-name",
			Namespace: "sandbox",
			Labels:    map[string]string{"sandbox.id": "logical-id"},
		}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-logical-id", Namespace: "sandbox"}},
		&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-pod-name", Namespace: "sandbox"}},
	)
	r := &Runtime{client: client, namespace: "sandbox"}

	err := r.RemoveSandbox(ctx, "pod-name")
	require.NoError(t, err)

	assertNetworkPolicyNotFound(t, client, "sandbox", "sandbox-logical-id")
	assertNetworkPolicyNotFound(t, client, "sandbox", "sandbox-pod-name")
}

func TestUpdateNetworkUsesLogicalSandboxIDFromPodLabel(t *testing.T) {
	ctx := context.Background()
	client := k8sfake.NewSimpleClientset(
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-name",
			Namespace: "sandbox",
			Labels:    map[string]string{"sandbox.id": "logical-id"},
		}},
	)
	r := &Runtime{client: client, namespace: "sandbox"}

	err := r.UpdateNetwork(ctx, "pod-name", false, nil, false)
	require.NoError(t, err)

	_, err = client.NetworkingV1().NetworkPolicies("sandbox").Get(ctx, "sandbox-logical-id", metav1.GetOptions{})
	require.NoError(t, err)
	_, err = client.NetworkingV1().NetworkPolicies("sandbox").Get(ctx, "sandbox-pod-name", metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err))
}

func assertNetworkPolicyNotFound(t *testing.T, client *k8sfake.Clientset, namespace, name string) {
	t.Helper()
	_, err := client.NetworkingV1().NetworkPolicies(namespace).Get(context.Background(), name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "expected %s to be deleted, got %v", name, err)
}
```

- [ ] **Step 2: Run policy tests and confirm failure**

Run: `go test ./internal/runtime/kubernetes -run 'TestApplyWorkspaceSyncBootstrapPolicy|TestPolicySandboxID|TestRemoveSandboxCleansLogicalAndRuntimePolicies|TestUpdateNetworkUsesLogicalSandboxIDFromPodLabel' -v`

Expected: FAIL because helpers do not exist.

- [ ] **Step 3: Implement bootstrap helpers**

In `internal/runtime/kubernetes/runtime.go`, add:

```go
func policySandboxID(spec runtime.SandboxSpec) string {
	if spec.Labels != nil && spec.Labels["sandbox.id"] != "" {
		return spec.Labels["sandbox.id"]
	}
	return spec.ID
}
```

In `internal/runtime/kubernetes/network.go`, add:

```go
type workspaceSyncBootstrapCleanup struct {
	OnFailure  func(context.Context)
	AfterReady func(context.Context)
}

func applyWorkspaceSyncBootstrapPolicy(ctx context.Context, client kubernetes.Interface, dynClient dynamic.Interface, namespace, sandboxID string, hasCilium bool, ws *runtime.WorkspaceSyncSpec) (workspaceSyncBootstrapCleanup, error) {
	if hasCilium {
		if len(ws.EgressFQDNs) == 0 {
			return workspaceSyncBootstrapCleanup{}, fmt.Errorf("workspace sync bootstrap requires at least one egress FQDN on Cilium")
		}
		if dynClient == nil {
			return workspaceSyncBootstrapCleanup{}, fmt.Errorf("workspace sync bootstrap requires dynamic client on Cilium")
		}
		if err := applyWorkspaceSyncBootstrapCiliumPolicy(ctx, dynClient, namespace, sandboxID, ws.EgressFQDNs); err != nil {
			return workspaceSyncBootstrapCleanup{}, err
		}
		cleanup := func(cleanupCtx context.Context) {
			_ = deleteWorkspaceSyncBootstrapCiliumPolicy(cleanupCtx, dynClient, namespace, sandboxID)
		}
		return workspaceSyncBootstrapCleanup{OnFailure: cleanup, AfterReady: cleanup}, nil
	}

	if len(ws.EgressCIDRs) == 0 {
		return workspaceSyncBootstrapCleanup{}, fmt.Errorf("workspace sync bootstrap requires at least one egress CIDR on standard NetworkPolicy")
	}
	if err := applyWorkspaceSyncBootstrapNetworkPolicy(ctx, client, namespace, sandboxID, ws.EgressCIDRs); err != nil {
		return workspaceSyncBootstrapCleanup{}, err
	}
	return workspaceSyncBootstrapCleanup{
		OnFailure:  func(cleanupCtx context.Context) { _ = deleteNetworkPolicy(cleanupCtx, client, namespace, sandboxID) },
		AfterReady: func(context.Context) {},
	}, nil
}
```

Implement `applyWorkspaceSyncBootstrapNetworkPolicy` by creating/updating `sandbox-<sandboxID>` with DNS plus `ws.EgressCIDRs` on TCP 80/443. Implement `applyWorkspaceSyncBootstrapCiliumPolicy` with name `sandbox-workspace-sync-<sandboxID>`, `endpointSelector.matchLabels.sandbox.id`, one DNS egress rule for TCP/UDP 53, and `egress.toFQDNs.matchName` entries for each FQDN plus ports 80/443. Implement `deleteWorkspaceSyncBootstrapCiliumPolicy` as a not-found-tolerant delete, matching `deleteCiliumPrivateDeny`.

- [ ] **Step 4: Wire bootstrap into `CreateSandbox`**

In `internal/runtime/kubernetes/runtime.go`, replace the beginning of `CreateSandbox` with the design flow:

```go
policyID := policySandboxID(spec)
var bootstrapCleanup workspaceSyncBootstrapCleanup
if ws := spec.WorkspaceSync; ws != nil {
	if r.hasCilium {
		_ = deleteCiliumPrivateDeny(ctx, r.dynClient, r.namespace, policyID)
	}
	cleanup, err := applyWorkspaceSyncBootstrapPolicy(ctx, r.client, r.dynClient, r.namespace, policyID, r.hasCilium, ws)
	if err != nil {
		return nil, fmt.Errorf("apply workspace sync bootstrap policy: %w", err)
	}
	bootstrapCleanup = cleanup
}
cleanupCreateFailure := func() {
	if bootstrapCleanup.OnFailure != nil {
		bootstrapCleanup.OnFailure(ctx)
	}
}
```

Use `policyID` instead of `spec.ID` for `updateNetworkPolicy`, `applyCiliumPrivateDeny`, and `deleteCiliumPrivateDeny`. On any Pod creation, wait, normal policy, or Cilium deny error, call `cleanupCreateFailure()` and delete the Pod when it exists. After normal `updateNetworkPolicy` succeeds, call `bootstrapCleanup.AfterReady(ctx)` when non-nil.

- [ ] **Step 5: Add logical cleanup helpers**

In `internal/runtime/kubernetes/runtime.go`, add:

```go
func (r *Runtime) policyIDForRuntimeID(ctx context.Context, runtimeID string) string {
	policyID := runtimeID
	if pod, err := getPod(ctx, r.client, r.namespace, runtimeID); err == nil {
		if v := pod.Labels["sandbox.id"]; v != "" {
			policyID = v
		}
	}
	return policyID
}

func (r *Runtime) cleanupSandboxPolicies(ctx context.Context, runtimeID string) {
	policyID := r.policyIDForRuntimeID(ctx, runtimeID)
	if r.hasCilium {
		_ = deleteCiliumPrivateDeny(ctx, r.dynClient, r.namespace, policyID)
		_ = deleteWorkspaceSyncBootstrapCiliumPolicy(ctx, r.dynClient, r.namespace, policyID)
		if policyID != runtimeID {
			_ = deleteCiliumPrivateDeny(ctx, r.dynClient, r.namespace, runtimeID)
			_ = deleteWorkspaceSyncBootstrapCiliumPolicy(ctx, r.dynClient, r.namespace, runtimeID)
		}
	}
	_ = deleteNetworkPolicy(ctx, r.client, r.namespace, policyID)
	if policyID != runtimeID {
		_ = deleteNetworkPolicy(ctx, r.client, r.namespace, runtimeID)
	}
}
```

Change `RemoveSandbox`:

```go
func (r *Runtime) RemoveSandbox(ctx context.Context, id string) error {
	r.cleanupSandboxPolicies(ctx, id)
	return deletePod(ctx, r.client, r.namespace, id)
}
```

Change `UpdateNetwork` to resolve policy ID first:

```go
policyID := r.policyIDForRuntimeID(ctx, id)
if err := updateNetworkPolicy(ctx, r.client, r.namespace, policyID, enabled, whitelist, blockPrivate); err != nil {
	return err
}
```

Use `policyID` for the Cilium deny branch too.

- [ ] **Step 6: Run Kubernetes runtime tests**

Run: `go test ./internal/runtime/kubernetes`

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/runtime/kubernetes/network.go internal/runtime/kubernetes/network_test.go internal/runtime/kubernetes/runtime.go internal/runtime/kubernetes/runtime_test.go
git commit -m "feat: apply workspace sync bootstrap network policy"
```

---

### Task 7: Restore Persistent Workspaces by Recorded Sync Mode

**Files:**
- Modify: `internal/sandbox/manager.go`
- Modify: `internal/sandbox/manager_test.go`

- [ ] **Step 1: Add focused restore tests**

In `internal/sandbox/manager_test.go`, add tests for the pure decision logic by extracting it in the next step:

```go
func TestShouldRestoreWorkspaceWithAPISync(t *testing.T) {
	mgr := &Manager{}

	assert.False(t, mgr.shouldRestoreWorkspaceWithAPISync(&Sandbox{
		Workspace: &WorkspaceInfo{SyncMode: WorkspaceSyncModeBindMount},
	}, false))

	assert.False(t, mgr.shouldRestoreWorkspaceWithAPISync(&Sandbox{
		Config:    SandboxConfig{WorkspacePath: "project-a"},
		Workspace: &WorkspaceInfo{SyncMode: WorkspaceSyncModeInitContainer},
	}, false))

	mgr = &Manager{
		fsMeta: &storage.FileSystemMeta{Provider: storage.ProviderS3},
		config: ManagerConfig{WorkspaceSyncEnabled: true},
	}
	assert.False(t, mgr.shouldRestoreWorkspaceWithAPISync(&Sandbox{
		Config:    SandboxConfig{WorkspacePath: "project-a"},
		Workspace: &WorkspaceInfo{SyncMode: WorkspaceSyncModeInitContainer},
	}, true))

	mgr.config.WorkspaceSyncEnabled = false
	assert.True(t, mgr.shouldRestoreWorkspaceWithAPISync(&Sandbox{
		Config:    SandboxConfig{WorkspacePath: "project-a"},
		Workspace: &WorkspaceInfo{SyncMode: WorkspaceSyncModeInitContainer},
	}, true))

	assert.True(t, mgr.shouldRestoreWorkspaceWithAPISync(&Sandbox{
		Workspace: &WorkspaceInfo{SyncMode: WorkspaceSyncModeAPI},
	}, false))
}
```

- [ ] **Step 2: Run the test and confirm failure**

Run: `go test ./internal/sandbox -run TestShouldRestoreWorkspaceWithAPISync -v`

Expected: FAIL because the helper does not exist.

- [ ] **Step 3: Add restore decision helper**

In `internal/sandbox/manager.go`, add:

```go
func (m *Manager) shouldRestoreWorkspaceWithAPISync(sb *Sandbox, recreatedThisRun bool) bool {
	if sb == nil || sb.Workspace == nil {
		return false
	}
	switch sb.Workspace.SyncMode {
	case WorkspaceSyncModeBindMount:
		return false
	case WorkspaceSyncModeInitContainer:
		return recreatedThisRun && !m.shouldUseWorkspaceInitContainer(sb.Config.WorkspacePath)
	case WorkspaceSyncModeAPI:
		return true
	default:
		return true
	}
}
```

- [ ] **Step 4: Track `recreatedThisRun` in restore**

Inside the `for _, id := range ids` loop in `restorePersistentSandboxes`, initialize:

```go
recreatedThisRun := false
```

Set it to true after successful `recreateSandbox`:

```go
recreatedThisRun = true
```

Replace the existing bind-mount-only restore sync branch with:

```go
if m.shouldRestoreWorkspaceWithAPISync(sbPtr, recreatedThisRun) {
	if syncErr := m.syncToContainer(ctx, scoped, sbPtr.RuntimeID); syncErr != nil {
		logger.Error(ctx, "workspace re-sync failed",
			logger.AddField("sandbox_id", id),
			logger.AddField("runtime_id", sbPtr.RuntimeID),
			logger.ErrorField(syncErr),
		)
	} else if sbPtr.Workspace.SyncMode == WorkspaceSyncModeInitContainer {
		sbPtr.Workspace.SyncMode = WorkspaceSyncModeAPI
		_ = m.sessions.Save(ctx, sbPtr)
	}
}
```

- [ ] **Step 5: Run restore decision tests**

Run: `go test ./internal/sandbox -run 'TestShouldRestoreWorkspaceWithAPISync|TestManager_CreateRemoteWorkspaceSyncBypassesPool' -v`

Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/sandbox/manager.go internal/sandbox/manager_test.go
git commit -m "fix: restore workspaces according to sync mode"
```

---

### Task 8: Full Verification

**Files:**
- Read: all files changed by Tasks 1-7

- [ ] **Step 1: Format Go code**

Run: `gofmt -w cmd/sandbox/main.go internal/config/config.go internal/config/config_test.go internal/runtime/types.go internal/runtime/kubernetes/*.go internal/sandbox/*.go internal/storage/filesystem.go internal/storage/filesystem_test.go`

Expected: command exits 0.

- [ ] **Step 2: Run focused package tests**

Run: `go test ./internal/sandbox ./internal/runtime/kubernetes ./internal/config ./internal/storage`

Expected: PASS.

- [ ] **Step 3: Run full test suite**

Run: `go test ./...`

Expected: PASS.

- [ ] **Step 4: Check git diff for whitespace errors**

Run: `git diff --check`

Expected: no output, exit code 0.

- [ ] **Step 5: Final commit if formatting or verification edits were needed**

If Step 1 changed files after prior task commits:

```bash
git add .
git commit -m "chore: format workspace sync optimization"
```

If Step 1 did not change files, do not create an empty commit.

---

## Self-Review

**Spec coverage:** Optimization 1 is covered by Task 1. Optimization 2 is covered by Task 2. Init-container data model/config is covered by Task 3. Manager direct creation, registration, and manual mount semantics are covered by Task 4. Pod injection is covered by Task 5. Bootstrap NetworkPolicy/Cilium lifecycle and logical sandbox ID handling are covered by Task 6. Restore by `SyncMode` is covered by Task 7. Verification is covered by Task 8.

**Placeholder scan:** This plan intentionally avoids deferred work items in implementation steps. Every listed code change includes the concrete function, field, or test to add.

**Type consistency:** `WorkspaceSyncSpec`, `WorkspaceInfo.SyncMode`, `WorkspaceSyncMode*`, `WorkspaceSyncEgressFQDNs`, and `WorkspaceSyncEgressCIDRs` use the same names across config, manager, runtime, tests, and YAML.

---

Plan complete and saved to `docs/superpowers/plans/2026-07-01-workspace-sync-optimization.md`. Two execution options:

**1. Subagent-Driven (recommended)** - Dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints.

Which approach?
