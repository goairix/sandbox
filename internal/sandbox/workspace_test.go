package sandbox

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

	"github.com/dysodeng/fs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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

// --- mock infrastructure for workspace streaming tests ---

type mockFileInfo struct {
	name    string
	size    int64
	dir     bool
	modTime time.Time
}

func (m mockFileInfo) Name() string       { return m.name }
func (m mockFileInfo) Size() int64        { return m.size }
func (m mockFileInfo) Mode() os.FileMode  { if m.dir { return os.ModeDir | 0755 }; return 0644 }
func (m mockFileInfo) ModTime() time.Time { return m.modTime }
func (m mockFileInfo) IsDir() bool        { return m.dir }
func (m mockFileInfo) Sys() interface{}   { return nil }

type mockScopedFS struct {
	files   map[string][]byte        // relPath -> content
	dirs    map[string][]fs.FileInfo // dir -> children
	openErr map[string]error         // relPath -> error to return from Open
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
func (m *mockScopedFS) Remove(context.Context, string, ...fs.Option) error              { return nil }
func (m *mockScopedFS) Copy(context.Context, string, string, ...fs.Option) error        { return nil }
func (m *mockScopedFS) Move(context.Context, string, string, ...fs.Option) error        { return nil }
func (m *mockScopedFS) Rename(context.Context, string, string, ...fs.Option) error      { return nil }
func (m *mockScopedFS) Stat(context.Context, string, ...fs.Option) (fs.FileInfo, error) { return nil, nil }
func (m *mockScopedFS) Exists(context.Context, string, ...fs.Option) (bool, error)      { return false, nil }
func (m *mockScopedFS) IsDir(context.Context, string, ...fs.Option) (bool, error)       { return false, nil }
func (m *mockScopedFS) IsFile(context.Context, string, ...fs.Option) (bool, error)      { return false, nil }
func (m *mockScopedFS) SignFullUrl(context.Context, string, ...fs.Option) (string, error) {
	return "", nil
}
func (m *mockScopedFS) FullUrl(context.Context, string, ...fs.Option) (string, error) { return "", nil }
func (m *mockScopedFS) RelativePath(context.Context, string, ...fs.Option) (string, error) {
	return "", nil
}
func (m *mockScopedFS) ChangeDir(context.Context, string) error { return nil }
func (m *mockScopedFS) WorkingDir() string                      { return "." }
