package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/fs"
	"github.com/goairix/fs/driver/local"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
)

type failingSessionSetStore struct {
	state.AtomicStore
	mu     sync.Mutex
	set    int
	failAt int
}

func (s *failingSessionSetStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	s.mu.Lock()
	s.set++
	fail := s.set == s.failAt
	s.mu.Unlock()
	if fail {
		return errors.New("session save failed")
	}
	return s.AtomicStore.Set(ctx, key, value, ttl)
}

func replaceSessionStoreWithFailingSet(t *testing.T, mgr *Manager, failAt int) {
	t.Helper()
	atomicStore, ok := mgr.sessions.store.(state.AtomicStore)
	require.True(t, ok)
	mgr.sessions = NewSessionStore(&failingSessionSetStore{AtomicStore: atomicStore, failAt: failAt}, time.Hour)
}

func readyFUSEManager(t *testing.T) (*Manager, *Sandbox) {
	t.Helper()
	mgr, sb, _ := readyFUSEManagerWithRuntime(t)
	return mgr, sb
}

func readyFUSEManagerWithRuntime(t *testing.T) (*Manager, *Sandbox, *mockRuntime) {
	t.Helper()
	rt := newFUSEManagerRuntime()
	mgr, _, _, _ := newFUSETestManager(t, rt)
	sb, err := mgr.Create(context.Background(), SandboxConfig{Mode: ModePersistent, WorkspacePath: "team/a"})
	require.NoError(t, err)
	t.Cleanup(func() { mgr.Stop(context.Background()) })
	return mgr, sb, rt.mockRuntime
}

func TestFUSEWorkspacePublicMountAndUnmountConflict(t *testing.T) {
	mgr, sb := readyFUSEManager(t)

	err := mgr.MountWorkspace(context.Background(), sb.ID, "team/b", nil)
	require.ErrorIs(t, err, ErrFUSEWorkspaceImmutable)
	err = mgr.UnmountWorkspace(context.Background(), sb.ID)
	require.ErrorIs(t, err, ErrFUSEWorkspaceImmutable)
}

func TestSyncModeAfterFUSERollbackUsesLegacyPoolAndFullCopy(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "team", "a"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "team", "a", "remote.txt"), []byte("remote"), 0o644))
	filesystem, err := local.New(local.Config{RootPath: root})
	require.NoError(t, err)
	rt := newMockRuntime()
	mgr := NewManager(rt, filesystem, &storage.FileSystemMeta{Provider: storage.ProviderMinIO}, ManagerConfig{
		DefaultMountMode:  WorkspaceMountSync,
		EnabledMountModes: map[WorkspaceMountType]bool{WorkspaceMountSync: true},
		// A leftover FUSE pool reference must not influence the rollback path;
		// only workspace.mode selects FUSE acquisition.
		FUSEPool:   &FUSEPool{},
		PoolConfig: PoolConfig{Image: "sandbox:sync"},
	})

	sb, err := mgr.Create(context.Background(), SandboxConfig{
		Mode:          ModePersistent,
		WorkspacePath: "team/a",
	})
	require.NoError(t, err)
	require.NotNil(t, sb.Workspace)
	assert.NotEqual(t, WorkspaceMountFUSE, sb.Workspace.MountType)

	rt.mu.Lock()
	defer rt.mu.Unlock()
	assert.Equal(t, 1, rt.created, "sync rollback must use the legacy container pool")
	assert.Zero(t, rt.prepareSeq, "sync rollback must not prepare a privileged FUSE shell")
	assert.Equal(t, 1, rt.execPipeCalls, "the first sync mount must perform a full storage-to-container copy")
}

func TestFUSESyncDirections(t *testing.T) {
	mgr, sb, rt := readyFUSEManagerWithRuntime(t)

	require.NoError(t, mgr.SyncWorkspace(context.Background(), sb.ID, "to_container", nil))
	rt.mu.Lock()
	assert.Zero(t, rt.quiesceCalls)
	assert.Zero(t, rt.flushCalls)
	assert.Zero(t, rt.resumeCalls)
	rt.mu.Unlock()

	require.NoError(t, mgr.SyncWorkspace(context.Background(), sb.ID, "from_container", nil))
	rt.mu.Lock()
	assert.Equal(t, 1, rt.quiesceCalls)
	assert.Equal(t, 1, rt.flushCalls)
	assert.Equal(t, 1, rt.resumeCalls)
	assert.Equal(t, []string{"quiesce", "flush", "resume"}, rt.workspaceOps)
	rt.mu.Unlock()
	info, infoErr := mgr.GetWorkspaceInfo(context.Background(), sb.ID)
	require.NoError(t, infoErr)
	assert.True(t, info.Flushed)
	require.NotNil(t, info.LastFlushedAt)
}

func TestFUSESyncWaitsForExistingFileStreamBeforeQuiesce(t *testing.T) {
	mgr, sb, rt := readyFUSEManagerWithRuntime(t)
	fuseRuntime := mgr.runtime.(*fuseManagerRuntime)
	fuseRuntime.mu.Lock()
	fuseRuntime.downloadReader = io.NopCloser(strings.NewReader("hello"))
	fuseRuntime.mu.Unlock()
	stream, err := mgr.DownloadFile(context.Background(), sb.ID, "/workspace/a.txt")
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- mgr.SyncWorkspace(context.Background(), sb.ID, "from_container", nil) }()
	time.Sleep(20 * time.Millisecond)
	rt.mu.Lock()
	assert.Zero(t, rt.quiesceCalls)
	rt.mu.Unlock()
	require.NoError(t, stream.Close())
	require.NoError(t, <-done)

	rt.mu.Lock()
	assert.Equal(t, 1, rt.quiesceCalls)
	rt.mu.Unlock()
}

func TestFUSESyncQuiesceFailureDoesNotFlushAndClosesAdmission(t *testing.T) {
	mgr, sb, rt := readyFUSEManagerWithRuntime(t)
	rt.mu.Lock()
	rt.quiesceErr = errors.New("open writer enumeration failed")
	rt.mu.Unlock()

	err := mgr.SyncWorkspace(context.Background(), sb.ID, "from_container", nil)
	require.ErrorContains(t, err, "open writer enumeration failed")
	rt.mu.Lock()
	assert.Zero(t, rt.flushCalls)
	assert.Zero(t, rt.resumeCalls)
	rt.mu.Unlock()
	mgr.mu.RLock()
	gate := mgr.operationGates[sb.ID]
	flushed := mgr.sandboxes[sb.ID].Workspace.Flushed
	mgr.mu.RUnlock()
	assert.False(t, gate.isOpen())
	assert.False(t, flushed)
	require.ErrorIs(t, mgr.SyncWorkspace(context.Background(), sb.ID, "to_container", nil), ErrSandboxNotReady)
	rt.mu.Lock()
	rt.quiesceErr = nil
	rt.mu.Unlock()
}

func TestFUSERecursiveListHidesReservedProbeFromStablePaginationCount(t *testing.T) {
	mgr, sb, rt := readyFUSEManagerWithRuntime(t)
	reserved, err := fuseprotocol.DeriveProbeObjectName(sb.RuntimeUID, sb.Workspace.LeaseGeneration)
	require.NoError(t, err)
	all := []runtime.FileInfo{
		{Name: reserved, Path: "/workspace/" + reserved},
		{Name: "user.txt", Path: "/workspace/user.txt"},
	}
	rt.mu.Lock()
	rt.reservedFileCount = 1
	rt.listRecursiveFunc = func(page, pageSize int) *runtime.FileListResult {
		start := (page - 1) * pageSize
		if start >= len(all) {
			return &runtime.FileListResult{TotalCount: len(all), Page: page, PageSize: pageSize}
		}
		end := start + pageSize
		if end > len(all) {
			end = len(all)
		}
		return &runtime.FileListResult{Files: append([]runtime.FileInfo(nil), all[start:end]...), TotalCount: len(all), Page: page, PageSize: pageSize}
	}
	rt.mu.Unlock()

	result, err := mgr.ListFilesRecursive(context.Background(), sb.ID, "/workspace", 0, 1, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	require.Len(t, result.Files, 1)
	assert.Equal(t, "user.txt", result.Files[0].Name)
}

func TestFUSERecursiveListFirstPageUsesBoundedIncrementalScan(t *testing.T) {
	mgr, sb, rt := readyFUSEManagerWithRuntime(t)
	reserved, err := fuseprotocol.DeriveProbeObjectName(sb.RuntimeUID, sb.Workspace.LeaseGeneration)
	require.NoError(t, err)
	const total = 100_000
	var calls, largestPageSize int
	rt.mu.Lock()
	rt.reservedFileCount = 1
	rt.listRecursiveFunc = func(page, pageSize int) *runtime.FileListResult {
		calls++
		if pageSize > largestPageSize {
			largestPageSize = pageSize
		}
		start := (page - 1) * pageSize
		end := start + pageSize
		if end > total {
			end = total
		}
		files := make([]runtime.FileInfo, 0, end-start)
		for i := start; i < end; i++ {
			name := fmt.Sprintf("user-%06d.txt", i)
			if i == 0 {
				name = reserved
			}
			files = append(files, runtime.FileInfo{Name: name, Path: "/workspace/" + name})
		}
		return &runtime.FileListResult{Files: files, TotalCount: total, Page: page, PageSize: pageSize}
	}
	rt.mu.Unlock()

	result, err := mgr.ListFilesRecursive(context.Background(), sb.ID, "/workspace", 0, 1, 1)
	require.NoError(t, err)
	require.Len(t, result.Files, 1)
	assert.Equal(t, "user-000001.txt", result.Files[0].Name)
	assert.Equal(t, total-1, result.TotalCount)
	assert.LessOrEqual(t, calls, 2)
	assert.LessOrEqual(t, largestPageSize, 256)
}

func TestFUSERecursiveListDoesNotSubtractAbsentProbeFromTotal(t *testing.T) {
	mgr, sb, rt := readyFUSEManagerWithRuntime(t)
	rt.mu.Lock()
	rt.reservedFileCount = 0
	rt.listRecursiveFunc = func(page, pageSize int) *runtime.FileListResult {
		return &runtime.FileListResult{
			Files:      []runtime.FileInfo{{Name: "user.txt", Path: "/workspace/user.txt"}},
			TotalCount: 1,
			Page:       page,
			PageSize:   pageSize,
		}
	}
	rt.mu.Unlock()

	result, err := mgr.ListFilesRecursive(context.Background(), sb.ID, "/workspace", 0, 1, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
}

func TestFUSERecursiveListCountsMultipleReservedBasenamesAcrossPages(t *testing.T) {
	mgr, sb, rt := readyFUSEManagerWithRuntime(t)
	current, err := fuseprotocol.DeriveProbeObjectName(sb.RuntimeUID, sb.Workspace.LeaseGeneration)
	require.NoError(t, err)
	oldGeneration, err := fuseprotocol.DeriveProbeObjectName(sb.RuntimeUID, sb.Workspace.LeaseGeneration+1)
	require.NoError(t, err)
	otherRuntime, err := fuseprotocol.DeriveProbeObjectName("other-runtime", sb.Workspace.LeaseGeneration)
	require.NoError(t, err)
	all := []runtime.FileInfo{
		{Name: current, Path: "/workspace/" + current},
		{Name: "user-a.txt", Path: "/workspace/user-a.txt"},
		{Name: oldGeneration, Path: "/workspace/" + oldGeneration},
		{Name: "user-b.txt", Path: "/workspace/user-b.txt"},
		{Name: otherRuntime, Path: "/workspace/" + otherRuntime},
		{Name: "user-c.txt", Path: "/workspace/user-c.txt"},
	}
	rt.mu.Lock()
	rt.reservedFileCount = 3
	rt.listRecursiveFunc = func(page, pageSize int) *runtime.FileListResult {
		start := (page - 1) * pageSize
		if start >= len(all) {
			return &runtime.FileListResult{TotalCount: len(all), Page: page, PageSize: pageSize}
		}
		end := start + pageSize
		if end > len(all) {
			end = len(all)
		}
		return &runtime.FileListResult{Files: append([]runtime.FileInfo(nil), all[start:end]...), TotalCount: len(all), Page: page, PageSize: pageSize}
	}
	rt.mu.Unlock()

	first, err := mgr.ListFilesRecursive(context.Background(), sb.ID, "/workspace", 0, 1, 2)
	require.NoError(t, err)
	require.Len(t, first.Files, 2)
	assert.Equal(t, []string{"user-a.txt", "user-b.txt"}, []string{first.Files[0].Name, first.Files[1].Name})
	assert.Equal(t, 3, first.TotalCount)
	second, err := mgr.ListFilesRecursive(context.Background(), sb.ID, "/workspace", 0, 2, 2)
	require.NoError(t, err)
	require.Len(t, second.Files, 1)
	assert.Equal(t, "user-c.txt", second.Files[0].Name)
	assert.Equal(t, 3, second.TotalCount)
	ghost, err := mgr.ListFilesRecursive(context.Background(), sb.ID, "/workspace", 0, 3, 2)
	require.NoError(t, err)
	assert.Empty(t, ghost.Files)
	assert.Equal(t, 3, ghost.TotalCount)
}

func TestFUSESyncRejectsStaleQuiesceTokenWithoutFlush(t *testing.T) {
	mgr, sb, rt := readyFUSEManagerWithRuntime(t)
	rt.mu.Lock()
	rt.quiesceToken = runtime.WorkspaceQuiesceToken{RuntimeUID: sb.RuntimeUID, Generation: sb.Workspace.LeaseGeneration + 1, Opaque: "stale"}
	rt.mu.Unlock()

	err := mgr.SyncWorkspace(context.Background(), sb.ID, "from_container", nil)
	require.ErrorContains(t, err, "invalid generation-bound token")
	rt.mu.Lock()
	assert.Zero(t, rt.flushCalls)
	assert.Zero(t, rt.resumeCalls)
	rt.quiesceToken = runtime.WorkspaceQuiesceToken{}
	rt.mu.Unlock()
}

func TestFUSESyncPendingSessionFailureClosesAdmissionBeforeQuiesce(t *testing.T) {
	mgr, sb, rt := readyFUSEManagerWithRuntime(t)
	replaceSessionStoreWithFailingSet(t, mgr, 1)

	err := mgr.SyncWorkspace(context.Background(), sb.ID, "from_container", nil)
	require.ErrorContains(t, err, "session save failed")
	rt.mu.Lock()
	assert.Zero(t, rt.quiesceCalls)
	rt.mu.Unlock()
	mgr.mu.RLock()
	gate := mgr.operationGates[sb.ID]
	flushed := mgr.sandboxes[sb.ID].Workspace.Flushed
	mgr.mu.RUnlock()
	assert.False(t, gate.isOpen())
	assert.False(t, flushed)
}

func TestFUSESyncCompletedSessionFailureDoesNotReportFlushed(t *testing.T) {
	mgr, sb, rt := readyFUSEManagerWithRuntime(t)
	replaceSessionStoreWithFailingSet(t, mgr, 2)

	err := mgr.SyncWorkspace(context.Background(), sb.ID, "from_container", nil)
	require.ErrorContains(t, err, "session save failed")
	rt.mu.Lock()
	assert.Equal(t, []string{"quiesce", "flush", "resume"}, rt.workspaceOps)
	rt.mu.Unlock()
	mgr.mu.RLock()
	gate := mgr.operationGates[sb.ID]
	flushed := mgr.sandboxes[sb.ID].Workspace.Flushed
	mgr.mu.RUnlock()
	assert.False(t, gate.isOpen())
	assert.False(t, flushed)
}

func TestFUSEPublicFilesHideAndRejectOnlyExactReservedProbeBasename(t *testing.T) {
	mgr, sb, rt := readyFUSEManagerWithRuntime(t)
	reserved, err := fuseprotocol.DeriveProbeObjectName(sb.RuntimeUID, sb.Workspace.LeaseGeneration)
	require.NoError(t, err)
	lookalike := reserved + "-user"
	rt.mu.Lock()
	rt.listFiles = []runtime.FileInfo{
		{Name: reserved, Path: "/workspace/" + reserved},
		{Name: lookalike, Path: "/workspace/" + lookalike},
	}
	rt.mu.Unlock()

	files, err := mgr.ListFiles(context.Background(), sb.ID, "/workspace")
	require.NoError(t, err)
	require.Len(t, files, 1)
	assert.Equal(t, lookalike, files[0].Name)

	err = mgr.UploadFile(context.Background(), sb.ID, "/workspace/"+reserved, 1, strings.NewReader("x"))
	require.ErrorIs(t, err, ErrReservedFUSEWorkspacePath)
	require.NoError(t, mgr.UploadFile(context.Background(), sb.ID, "/workspace/"+lookalike, 1, strings.NewReader("x")))
	rt.mu.Lock()
	assert.Equal(t, 1, rt.uploadCalls)
	rt.mu.Unlock()
}

func TestIsExcluded(t *testing.T) {
	exclude := []string{".agent", ".cache"}

	// Exact match
	assert.True(t, isExcluded(".agent", exclude))
	assert.True(t, isExcluded(".cache", exclude))

	// Prefix match (subdirectories / files)
	assert.True(t, isExcluded(".agent/IDENTITY.md", exclude))
	assert.True(t, isExcluded(".agent/skills/code.yaml", exclude))
	assert.True(t, isExcluded(".cache/tmp.dat", exclude))

	// Directory entries (trailing slash)
	assert.True(t, isExcluded(".agent/", exclude))
	assert.True(t, isExcluded(".agent/skills/", exclude))

	// Non-excluded paths
	assert.False(t, isExcluded("src/main.py", exclude))
	assert.False(t, isExcluded("data/input.csv", exclude))
	assert.False(t, isExcluded(".agentx/other", exclude))
	assert.False(t, isExcluded("my.agent/file", exclude))

	// Empty exclude list
	assert.False(t, isExcluded(".agent", nil))
	assert.False(t, isExcluded(".agent", []string{}))
}

func TestStorageWriteOptions(t *testing.T) {
	tests := []struct {
		name            string
		path            string
		wantContentType string
		wantDisposition string
	}{
		{name: "html", path: "index.html", wantContentType: "text/html; charset=utf-8", wantDisposition: "inline"},
		{name: "htm", path: "page.htm", wantContentType: "text/html; charset=utf-8", wantDisposition: "inline"},
		{name: "mixed case html", path: "report.HtMl", wantContentType: "text/html; charset=utf-8", wantDisposition: "inline"},
		{name: "plain text", path: "notes.txt", wantContentType: "text/plain; charset=utf-8"},
		{name: "unknown extension", path: "payload.unknownext", wantContentType: "application/octet-stream"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := &fs.Options{}
			for _, opt := range storageWriteOptions(tt.path) {
				opt(got)
			}

			assert.Equal(t, tt.wantContentType, got.ContentType)
			assert.Equal(t, tt.wantDisposition, got.ContentDisposition)
		})
	}
}

func TestSyncFromContainer_ExcludeFiltering(t *testing.T) {
	// Test that excluded paths are filtered from manifest
	manifest := map[string]int64{
		"src/main.py":             100,
		".agent/IDENTITY.md":      200,
		".agent/skills/code.yaml": 200,
		".agent/":                 200,
		"data/input.csv":          100,
	}

	exclude := []string{".agent"}

	// Simulate changed set building with exclude
	changedSet := make(map[string]struct{})
	var cutoff int64 = 0
	for path, modtime := range manifest {
		if strings.HasSuffix(path, "/") {
			continue
		}
		if isExcluded(path, exclude) {
			continue
		}
		if cutoff == 0 || modtime > cutoff {
			changedSet[path] = struct{}{}
		}
	}

	assert.Contains(t, changedSet, "src/main.py")
	assert.Contains(t, changedSet, "data/input.csv")
	assert.NotContains(t, changedSet, ".agent/IDENTITY.md")
	assert.NotContains(t, changedSet, ".agent/skills/code.yaml")

	// Simulate deleted files building with exclude
	storageFiles := map[string]struct{}{
		"src/main.py":    {},
		"old_file.txt":   {},
		".agent/SOUL.md": {},
	}

	var deletedFiles []string
	for path := range storageFiles {
		if isExcluded(path, exclude) {
			continue
		}
		if _, exists := manifest[path]; !exists {
			deletedFiles = append(deletedFiles, path)
		}
	}

	assert.Contains(t, deletedFiles, "old_file.txt")
	assert.NotContains(t, deletedFiles, ".agent/SOUL.md")
}

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

func TestWriteTarStream_DoesNotPrefetchEntireWorkspace(t *testing.T) {
	const fileCount = 16
	files := make(map[string][]byte, fileCount)
	entries := make([]fileEntry, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		name := fmt.Sprintf("file-%02d.bin", i)
		files[name] = bytes.Repeat([]byte{byte(i)}, 1024)
		entries = append(entries, fileEntry{relPath: name, size: 1024})
	}
	fs := &countingScopedFS{mockScopedFS: &mockScopedFS{files: files}}
	w := newBlockingWriter()
	done := make(chan error, 1)
	go func() {
		done <- (&Manager{}).writeTarStream(context.Background(), fs, entries, w)
	}()

	select {
	case <-w.started:
	case <-time.After(time.Second):
		t.Fatal("tar writer 未开始写入")
	}
	deadline := time.Now().Add(100 * time.Millisecond)
	for fs.opened.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	openedWhileBlocked := fs.opened.Load()
	close(w.release)
	require.NoError(t, <-done)

	if openedWhileBlocked > 1 {
		t.Fatalf("输出阻塞时不应预读后续文件，实际已打开 %d 个文件", openedWhileBlocked)
	}
}

func TestStreamTarToContainer_ExecPipeFailureDoesNotBlockProducer(t *testing.T) {
	consumerErr := errors.New("tar process exited")
	mgr := &Manager{runtime: &failingExecPipeRuntime{err: consumerErr}}
	fs := &mockScopedFS{
		files: map[string][]byte{"large.bin": bytes.Repeat([]byte("x"), 1024)},
	}
	entries := []fileEntry{{relPath: "large.bin", size: 1024}}
	type result struct {
		writeErr error
		execErr  error
	}
	done := make(chan result, 1)
	go func() {
		writeErr, execErr := mgr.streamTarToContainer(context.Background(), fs, entries, "runtime-1")
		done <- result{writeErr: writeErr, execErr: execErr}
	}()

	select {
	case got := <-done:
		require.ErrorIs(t, got.execErr, consumerErr)
		require.ErrorIs(t, got.writeErr, consumerErr)
	case <-time.After(time.Second):
		t.Fatal("ExecPipe 提前失败后 tar 生产端仍阻塞")
	}
}

// --- mock infrastructure for workspace streaming tests ---

type mockFileInfo struct {
	name    string
	size    int64
	dir     bool
	modTime time.Time
}

func (m mockFileInfo) Name() string { return m.name }
func (m mockFileInfo) Size() int64  { return m.size }
func (m mockFileInfo) Mode() os.FileMode {
	if m.dir {
		return os.ModeDir | 0755
	}
	return 0644
}
func (m mockFileInfo) ModTime() time.Time { return m.modTime }
func (m mockFileInfo) IsDir() bool        { return m.dir }
func (m mockFileInfo) Sys() interface{}   { return nil }

type mockScopedFS struct {
	files   map[string][]byte        // relPath -> content
	dirs    map[string][]fs.FileInfo // dir -> children
	openErr map[string]error         // relPath -> error to return from Open
}

type countingScopedFS struct {
	*mockScopedFS
	opened atomic.Int64
}

func (m *countingScopedFS) Open(ctx context.Context, path string, opts ...fs.Option) (io.ReadCloser, error) {
	m.opened.Add(1)
	return m.mockScopedFS.Open(ctx, path, opts...)
}

type blockingWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

type failingExecPipeRuntime struct {
	runtime.Runtime
	err error
}

func (r *failingExecPipeRuntime) ExecPipe(context.Context, string, []string, io.Reader) error {
	return r.err
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{started: make(chan struct{}), release: make(chan struct{})}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
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
func (m *mockScopedFS) Create(context.Context, string, ...fs.Option) (io.WriteCloser, error) {
	return nil, nil
}
func (m *mockScopedFS) OpenFile(context.Context, string, int, os.FileMode, ...fs.Option) (io.ReadWriteCloser, error) {
	return nil, nil
}
func (m *mockScopedFS) Remove(context.Context, string, ...fs.Option) error         { return nil }
func (m *mockScopedFS) Copy(context.Context, string, string, ...fs.Option) error   { return nil }
func (m *mockScopedFS) Move(context.Context, string, string, ...fs.Option) error   { return nil }
func (m *mockScopedFS) Rename(context.Context, string, string, ...fs.Option) error { return nil }
func (m *mockScopedFS) Stat(context.Context, string, ...fs.Option) (fs.FileInfo, error) {
	return nil, nil
}

type failingObjectRemoval struct {
	*mockScopedFS
	err error
}

func (s failingObjectRemoval) Remove(context.Context, string, ...fs.Option) error { return s.err }

func TestSyncFromSnapshotRetainsTimestampWhenObjectRemovalFails(t *testing.T) {
	rt := newMockRuntime()
	mgr := NewManager(rt, nil, nil, ManagerConfig{})
	cutoff := time.Unix(100, 0)
	sb := &Sandbox{ID: "sandbox-a", RuntimeID: "runtime-a", Workspace: &WorkspaceInfo{MountType: WorkspaceMountSync, LastSyncedAt: cutoff}}
	failed := errors.New("object delete unavailable")
	scoped := failingObjectRemoval{mockScopedFS: &mockScopedFS{dirs: map[string][]fs.FileInfo{".": {mockFileInfo{name: "deleted.txt", size: 1}}}}, err: failed}
	require.ErrorIs(t, mgr.syncFromContainerSnapshot(context.Background(), sb, scoped, nil), failed)
	require.Equal(t, cutoff, sb.Workspace.LastSyncedAt)
}
func (m *mockScopedFS) Exists(context.Context, string, ...fs.Option) (bool, error) { return false, nil }
func (m *mockScopedFS) IsDir(context.Context, string, ...fs.Option) (bool, error)  { return false, nil }
func (m *mockScopedFS) IsFile(context.Context, string, ...fs.Option) (bool, error) { return false, nil }
func (m *mockScopedFS) SignFullUrl(context.Context, string, ...fs.Option) (string, error) {
	return "", nil
}
func (m *mockScopedFS) FullUrl(context.Context, string, ...fs.Option) (string, error) { return "", nil }
func (m *mockScopedFS) RelativePath(context.Context, string, ...fs.Option) (string, error) {
	return "", nil
}
func (m *mockScopedFS) ChangeDir(context.Context, string) error { return nil }
func (m *mockScopedFS) WorkingDir() string                      { return "." }
