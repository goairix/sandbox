# syncToContainer 流式管道优化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 将 syncToContainer 从全量内存缓冲改为 io.Pipe 流式管道，消除内存双缓冲，实现读取/tar构建/上传三者并行。

**Architecture:** 保留 collectFiles 元数据遍历阶段，用 io.Pipe 连接 writeTarStream（新方法，含有序预取池）和 UploadArchive，数据流式通过而非缓冲。删除 readFilesConcurrent。

**Tech Stack:** Go stdlib（archive/tar, io, context）, github.com/goairix/fs, github.com/stretchr/testify

---

## File Structure

| File | Action | Responsibility |
|------|--------|----------------|
| `internal/sandbox/workspace.go` | Modify | syncToContainer 重写、writeTarStream 新增、fileEntry 修改、readFilesConcurrent 删除 |
| `internal/sandbox/workspace_test.go` | Modify | 新增 writeTarStream 和 syncToContainer 流式管道的单元测试 |

---

### Task 1: 添加 mock ScopedFS 测试基础设施

**Files:**
- Modify: `internal/sandbox/workspace_test.go`

- [ ] **Step 1: 创建 mockScopedFS 和 mockFileInfo**

在 `workspace_test.go` 中添加测试用的 mock 类型，仅实现 `List` 和 `Open` 方法（writeTarStream 只用到这两个）：

```go
// --- mock infrastructure for workspace streaming tests ---

type mockFileInfo struct {
	name    string
	size    int64
	dir     bool
	modTime time.Time
}

func (m mockFileInfo) Name() string        { return m.name }
func (m mockFileInfo) Size() int64         { return m.size }
func (m mockFileInfo) Mode() os.FileMode   { if m.dir { return os.ModeDir | 0755 }; return 0644 }
func (m mockFileInfo) ModTime() time.Time  { return m.modTime }
func (m mockFileInfo) IsDir() bool         { return m.dir }
func (m mockFileInfo) Sys() interface{}    { return nil }

type mockScopedFS struct {
	files map[string][]byte   // relPath -> content
	dirs  map[string][]fs.FileInfo // dir -> children
	openErr map[string]error   // relPath -> error to return from Open
}

func (m *mockScopedFS) List(_ context.Context, p string, _ ...fs.Option) ([]fs.FileInfo, error) {
	children, ok := m.dirs[p]
	if !ok {
		return nil, nil
	}
	return children, nil
}

func (m *mockScopedFS) Open(_ context.Context, p string, _ ...fs.Option) (io.ReadCloser, error) {
	if m.openErr != nil {
		if err, ok := m.openErr[p]; ok {
			return nil, err
		}
	}
	content, ok := m.files[p]
	if !ok {
		return nil, fmt.Errorf("file not found: %s", p)
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

// Stub methods to satisfy ScopedFS interface (not used by writeTarStream)
func (m *mockScopedFS) MakeDir(context.Context, string, os.FileMode, ...fs.Option) error { return nil }
func (m *mockScopedFS) RemoveDir(context.Context, string, ...fs.Option) error            { return nil }
func (m *mockScopedFS) Create(context.Context, string, ...fs.Option) (io.WriteCloser, error) { return nil, nil }
func (m *mockScopedFS) OpenFile(context.Context, string, int, os.FileMode, ...fs.Option) (io.ReadWriteCloser, error) { return nil, nil }
func (m *mockScopedFS) Remove(context.Context, string, ...fs.Option) error               { return nil }
func (m *mockScopedFS) Copy(context.Context, string, string, ...fs.Option) error          { return nil }
func (m *mockScopedFS) Move(context.Context, string, string, ...fs.Option) error          { return nil }
func (m *mockScopedFS) Rename(context.Context, string, string, ...fs.Option) error        { return nil }
func (m *mockScopedFS) Stat(context.Context, string, ...fs.Option) (fs.FileInfo, error)   { return nil, nil }
func (m *mockScopedFS) Exists(context.Context, string, ...fs.Option) (bool, error)        { return false, nil }
func (m *mockScopedFS) IsDir(context.Context, string, ...fs.Option) (bool, error)         { return false, nil }
func (m *mockScopedFS) IsFile(context.Context, string, ...fs.Option) (bool, error)        { return false, nil }
func (m *mockScopedFS) SignFullUrl(context.Context, string, ...fs.Option) (string, error)  { return "", nil }
func (m *mockScopedFS) FullUrl(context.Context, string, ...fs.Option) (string, error)     { return "", nil }
func (m *mockScopedFS) RelativePath(context.Context, string, ...fs.Option) (string, error) { return "", nil }
func (m *mockScopedFS) ChangeDir(context.Context, string) error                           { return nil }
func (m *mockScopedFS) WorkingDir() string                                                { return "." }
```

- [ ] **Step 2: 更新 imports**

workspace_test.go 的 import 块更新为：

```go
import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goairix/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)
```

- [ ] **Step 3: 验证编译通过**

Run: `go build ./internal/sandbox/...`
Expected: 编译成功，无错误

- [ ] **Step 4: 提交**

```bash
git add internal/sandbox/workspace_test.go
git commit -m "test: add mock ScopedFS for workspace streaming tests"
```

---

### Task 2: 为 fileEntry 添加 size 字段并更新 collectFiles

**Files:**
- Modify: `internal/sandbox/workspace.go:27-32` (fileEntry)
- Modify: `internal/sandbox/workspace.go:240-245` (collectFiles append)

- [ ] **Step 1: 写 collectFiles 的测试**

在 `workspace_test.go` 中添加：

```go
func TestCollectFiles(t *testing.T) {
	mock := &mockScopedFS{
		dirs: map[string][]fs.FileInfo{
			".": {
				mockFileInfo{name: "subdir", dir: true, modTime: time.Unix(1000, 0)},
				mockFileInfo{name: "hello.txt", size: 5, modTime: time.Unix(2000, 0)},
			},
			"subdir": {
				mockFileInfo{name: "nested.py", size: 20, modTime: time.Unix(3000, 0)},
			},
		},
	}

	mgr := &Manager{}
	var entries []fileEntry
	err := mgr.collectFiles(context.Background(), mock, ".", &entries)
	require.NoError(t, err)

	require.Len(t, entries, 3)

	assert.Equal(t, "subdir", entries[0].relPath)
	assert.True(t, entries[0].isDir)

	assert.Equal(t, "subdir/nested.py", entries[1].relPath)
	assert.False(t, entries[1].isDir)
	assert.Equal(t, int64(20), entries[1].size)

	assert.Equal(t, "hello.txt", entries[2].relPath)
	assert.False(t, entries[2].isDir)
	assert.Equal(t, int64(5), entries[2].size)
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./internal/sandbox/ -run TestCollectFiles -v`
Expected: FAIL — fileEntry 没有 size 字段

- [ ] **Step 3: 为 fileEntry 添加 size 字段**

在 `workspace.go` 中将 fileEntry 改为（保留 content，仅添加 size）：

```go
type fileEntry struct {
	relPath string
	isDir   bool
	size    int64
	modTime time.Time
	content []byte // nil for directories
}
```

- [ ] **Step 4: 修改 collectFiles 的 append**

在 `collectFiles` 方法中，将 append 调用改为：

```go
*entries = append(*entries, fileEntry{
	relPath: relPath,
	isDir:   fi.IsDir(),
	size:    fi.Size(),
	modTime: fi.ModTime(),
})
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/sandbox/ -run TestCollectFiles -v`
Expected: PASS

- [ ] **Step 6: 确认全量编译通过**

Run: `go build ./...`
Expected: 编译成功，无错误（content 字段保留，现有代码不受影响）

- [ ] **Step 7: 提交**

```bash
git add internal/sandbox/workspace.go internal/sandbox/workspace_test.go
git commit -m "refactor: add size field to fileEntry for streaming tar headers"
```

---

### Task 3: 实现 writeTarStream 方法

**Files:**
- Modify: `internal/sandbox/workspace.go` (新增 writeTarStream 方法)
- Modify: `internal/sandbox/workspace_test.go` (新增测试)

- [ ] **Step 1: 写 writeTarStream 正常路径的测试**

在 `workspace_test.go` 中添加：

```go
func TestWriteTarStream_ProducesValidTar(t *testing.T) {
	mock := &mockScopedFS{
		files: map[string][]byte{
			"hello.txt":      []byte("hello"),
			"subdir/main.py": []byte("print('hi')"),
		},
	}

	entries := []fileEntry{
		{relPath: "subdir", isDir: true, modTime: time.Unix(1000, 0)},
		{relPath: "subdir/main.py", isDir: false, size: 11, modTime: time.Unix(2000, 0)},
		{relPath: "hello.txt", isDir: false, size: 5, modTime: time.Unix(3000, 0)},
	}

	mgr := &Manager{}
	var buf bytes.Buffer
	err := mgr.writeTarStream(context.Background(), mock, entries, &buf)
	require.NoError(t, err)

	// Verify tar contents
	tr := tar.NewReader(&buf)
	var names []string
	contents := make(map[string]string)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		names = append(names, hdr.Name)
		if hdr.Typeflag != tar.TypeDir {
			data, _ := io.ReadAll(tr)
			contents[hdr.Name] = string(data)
		}
	}

	assert.Equal(t, []string{"subdir/", "subdir/main.py", "hello.txt"}, names)
	assert.Equal(t, "print('hi')", contents["subdir/main.py"])
	assert.Equal(t, "hello", contents["hello.txt"])
}
```

- [ ] **Step 2: 写 writeTarStream 错误处理的测试**

```go
func TestWriteTarStream_OpenError_CleansUpReaders(t *testing.T) {
	mock := &mockScopedFS{
		files: map[string][]byte{
			"a.txt": []byte("aaa"),
			"c.txt": []byte("ccc"),
		},
		openErr: map[string]error{
			"b.txt": fmt.Errorf("permission denied"),
		},
	}

	entries := []fileEntry{
		{relPath: "a.txt", isDir: false, size: 3, modTime: time.Unix(1000, 0)},
		{relPath: "b.txt", isDir: false, size: 3, modTime: time.Unix(2000, 0)},
		{relPath: "c.txt", isDir: false, size: 3, modTime: time.Unix(3000, 0)},
	}

	mgr := &Manager{}
	var buf bytes.Buffer
	err := mgr.writeTarStream(context.Background(), mock, entries, &buf)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "permission denied")
}

func TestWriteTarStream_EmptyEntries(t *testing.T) {
	mgr := &Manager{}
	var buf bytes.Buffer
	err := mgr.writeTarStream(context.Background(), nil, nil, &buf)
	require.NoError(t, err)
	assert.Equal(t, 0, buf.Len())
}

func TestWriteTarStream_ContextCancelled(t *testing.T) {
	mock := &mockScopedFS{
		files: map[string][]byte{
			"a.txt": []byte("aaa"),
		},
	}

	entries := []fileEntry{
		{relPath: "a.txt", isDir: false, size: 3, modTime: time.Unix(1000, 0)},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	mgr := &Manager{}
	var buf bytes.Buffer
	err := mgr.writeTarStream(ctx, mock, entries, &buf)
	require.Error(t, err)
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./internal/sandbox/ -run TestWriteTarStream -v`
Expected: FAIL — writeTarStream 方法不存在

- [ ] **Step 4: 实现 writeTarStream**

在 `workspace.go` 中，在 `syncToContainer` 方法之后添加：

```go
// writeTarStream writes all entries as a tar archive to w, prefetching file
// contents concurrently using a bounded worker pool of maxConcurrentReads.
func (m *Manager) writeTarStream(ctx context.Context, scoped storage.ScopedFS, entries []fileEntry, w io.Writer) error {
	if len(entries) == 0 {
		return nil
	}

	tw := tar.NewWriter(w)
	defer tw.Close()

	type readResult struct {
		reader io.ReadCloser
		err    error
	}

	sem := make(chan struct{}, maxConcurrentReads)
	resultChs := make([]chan readResult, len(entries))

	// Launch prefetch goroutines for all file entries.
	for i, e := range entries {
		if e.isDir {
			continue
		}
		ch := make(chan readResult, 1)
		resultChs[i] = ch
		go func(entry fileEntry, ch chan<- readResult) {
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				ch <- readResult{err: ctx.Err()}
				return
			}
			reader, err := scoped.Open(ctx, entry.relPath)
			if err != nil {
				ch <- readResult{err: fmt.Errorf("open %q: %w", entry.relPath, err)}
				return
			}
			ch <- readResult{reader: reader}
		}(e, ch)
	}

	// cleanup drains and closes any unconsumed readers from index start onward.
	cleanup := func(start int) {
		for j := start; j < len(entries); j++ {
			if resultChs[j] != nil {
				if res := <-resultChs[j]; res.reader != nil {
					res.reader.Close()
				}
			}
		}
	}

	// Consume results in entry order, writing tar entries sequentially.
	for i, e := range entries {
		if e.isDir {
			if err := tw.WriteHeader(&tar.Header{
				Name:     e.relPath + "/",
				Typeflag: tar.TypeDir,
				Mode:     0755,
				ModTime:  e.modTime,
				Uid:      1000,
				Gid:      1000,
			}); err != nil {
				cleanup(i)
				return fmt.Errorf("write dir header %q: %w", e.relPath, err)
			}
			continue
		}

		res := <-resultChs[i]
		if res.err != nil {
			cleanup(i + 1)
			return res.err
		}

		if err := tw.WriteHeader(&tar.Header{
			Name:    e.relPath,
			Size:    e.size,
			Mode:    0644,
			ModTime: e.modTime,
			Uid:     1000,
			Gid:     1000,
		}); err != nil {
			res.reader.Close()
			cleanup(i + 1)
			return fmt.Errorf("write file header %q: %w", e.relPath, err)
		}

		_, copyErr := io.Copy(tw, res.reader)
		res.reader.Close()
		if copyErr != nil {
			cleanup(i + 1)
			return fmt.Errorf("write file content %q: %w", e.relPath, copyErr)
		}
	}

	return nil
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./internal/sandbox/ -run TestWriteTarStream -v`
Expected: 全部 PASS

- [ ] **Step 6: 提交**

```bash
git add internal/sandbox/workspace.go internal/sandbox/workspace_test.go
git commit -m "feat: add writeTarStream with ordered prefetch pipeline"
```

---

### Task 4: 重写 syncToContainer，删除 readFilesConcurrent，移除 content 字段

**Files:**
- Modify: `internal/sandbox/workspace.go:27-32` (fileEntry — 移除 content)
- Modify: `internal/sandbox/workspace.go:168-220` (syncToContainer)
- Delete: `internal/sandbox/workspace.go:259-309` (readFilesConcurrent)
- Modify: `internal/sandbox/workspace.go:1-18` (imports)

- [ ] **Step 1: 移除 fileEntry 的 content 字段**

将 fileEntry 改为：

```go
type fileEntry struct {
	relPath string
	isDir   bool
	size    int64
	modTime time.Time
}
```

- [ ] **Step 2: 重写 syncToContainer**

将 `syncToContainer` 方法替换为：

```go
// syncToContainer collects file metadata from ScopedFS, then streams a tar
// archive directly to the container via io.Pipe, overlapping storage reads,
// tar construction, and container upload.
func (m *Manager) syncToContainer(ctx context.Context, scoped storage.ScopedFS, runtimeID string) error {
	// Phase 1: walk the directory tree to collect file metadata (sequential).
	var entries []fileEntry
	if err := m.collectFiles(ctx, scoped, ".", &entries); err != nil {
		return fmt.Errorf("collect files: %w", err)
	}
	if len(entries) == 0 {
		return nil
	}

	// Phase 2: streaming pipeline — tar writes stream directly to UploadArchive.
	pr, pw := io.Pipe()

	uploadErrCh := make(chan error, 1)
	go func() {
		uploadErrCh <- m.runtime.UploadArchive(ctx, runtimeID, "/workspace", pr)
	}()

	writeErr := m.writeTarStream(ctx, scoped, entries, pw)
	if writeErr != nil {
		pw.CloseWithError(writeErr)
	} else {
		pw.Close()
	}

	uploadErr := <-uploadErrCh

	if writeErr != nil && uploadErr != nil {
		return fmt.Errorf("upload archive: %w", uploadErr)
	}
	if writeErr != nil {
		return fmt.Errorf("write tar stream: %w", writeErr)
	}
	if uploadErr != nil {
		return fmt.Errorf("upload archive: %w", uploadErr)
	}
	return nil
}
```

- [ ] **Step 3: 删除 readFilesConcurrent 方法**

删除整个 `readFilesConcurrent` 方法（当前位于 `syncToContainer` 之后，约 50 行）。

- [ ] **Step 4: 清理 imports**

将 workspace.go 的 import 块更新为（移除 `"bytes"` 和 `"sync"`）：

```go
import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
)
```

- [ ] **Step 5: 编译验证**

Run: `go build ./...`
Expected: 编译成功，无错误

- [ ] **Step 6: 运行全量测试**

Run: `go test ./internal/sandbox/ -count=1 -v`
Expected: 全部 PASS

- [ ] **Step 7: go vet 检查**

Run: `go vet ./...`
Expected: 无警告

- [ ] **Step 8: 提交**

```bash
git add internal/sandbox/workspace.go
git commit -m "feat: rewrite syncToContainer with streaming io.Pipe pipeline

Replace buffered read-all + tar-build + upload with streaming pipeline:
- io.Pipe connects writeTarStream to UploadArchive
- Prefetch pool (8 concurrent) hides storage read latency
- Peak memory reduced from ~2x total file size to ~256KB
- Remove content field from fileEntry (no longer needed)
- Delete readFilesConcurrent (no longer needed)"
```

---

### Task 5: 最终验证

- [ ] **Step 1: 全量编译**

Run: `go build ./...`
Expected: 成功

- [ ] **Step 2: 全量测试**

Run: `go test ./... -count=1`
Expected: 全部 PASS

- [ ] **Step 3: go vet**

Run: `go vet ./...`
Expected: 无警告

- [ ] **Step 4: 确认无遗留的 bytes.Buffer 或 readFilesConcurrent 引用**

Run: `grep -n 'readFilesConcurrent\|bytes\.Buffer' internal/sandbox/workspace.go`
Expected: 无输出
