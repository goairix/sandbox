package sandbox

import (
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

// Ensure unused imports are referenced (will be used by later tasks)
var _ = require.New
var _ = strings.Contains
