package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/goairix/fs/driver/local"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupTestFS creates a local fs with t.TempDir(), creates a "workspace" directory,
// and returns a ScopedFS instance rooted at workspace.
func setupTestFS(t *testing.T) ScopedFS {
	t.Helper()
	dir := t.TempDir()

	lfs, err := local.New(local.Config{RootPath: dir})
	require.NoError(t, err)

	ctx := context.Background()
	err = lfs.MakeDir(ctx, "workspace", 0755)
	require.NoError(t, err)

	sfs, err := NewScopedFS(lfs, "workspace")
	require.NoError(t, err)

	return sfs
}

func TestNewScopedFS_EmptyRoot(t *testing.T) {
	lfs, err := local.New(local.Config{RootPath: t.TempDir()})
	require.NoError(t, err)

	_, err = NewScopedFS(lfs, "")
	assert.Error(t, err)
}

func TestNewScopedFS_NilFS(t *testing.T) {
	_, err := NewScopedFS(nil, "/some/root")
	assert.Error(t, err)
}

func TestNewScopedFS_AbsoluteRootStripped(t *testing.T) {
	dir := t.TempDir()
	lfs, err := local.New(local.Config{RootPath: dir})
	require.NoError(t, err)

	ctx := context.Background()
	require.NoError(t, lfs.MakeDir(ctx, "workspaces/user/project", 0755))

	// Absolute path should be treated as relative by stripping the leading "/".
	sfs, err := NewScopedFS(lfs, "/workspaces/user/project")
	require.NoError(t, err)
	assert.Equal(t, "workspaces/user/project", sfs.(*scopedFS).rootPath)
}

func TestScopedFS_ResolvePath_Normal(t *testing.T) {
	sfs := setupTestFS(t).(*scopedFS)
	resolved, err := sfs.resolvePath("file.txt")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(resolved, "workspace/file.txt"), "got: %s", resolved)
}

func TestScopedFS_ResolvePath_Subdirectory(t *testing.T) {
	sfs := setupTestFS(t).(*scopedFS)
	resolved, err := sfs.resolvePath("sub/dir/file.txt")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(resolved, "workspace/sub/dir/file.txt"), "got: %s", resolved)
}

func TestScopedFS_ResolvePath_EscapeBlocked(t *testing.T) {
	sfs := setupTestFS(t).(*scopedFS)
	_, err := sfs.resolvePath("../etc/passwd")
	assert.ErrorIs(t, err, ErrPathEscaped)
}

func TestScopedFS_ResolvePath_AbsoluteBlocked(t *testing.T) {
	sfs := setupTestFS(t).(*scopedFS)
	_, err := sfs.resolvePath("/etc/passwd")
	assert.ErrorIs(t, err, ErrPathEscaped)
}

func TestScopedFS_ResolvePath_DotDotInMiddleBlocked(t *testing.T) {
	sfs := setupTestFS(t).(*scopedFS)
	_, err := sfs.resolvePath("sub/../../etc/passwd")
	assert.ErrorIs(t, err, ErrPathEscaped)
}

func TestScopedFS_ResolvePath_DotStaysInRoot(t *testing.T) {
	sfs := setupTestFS(t).(*scopedFS)
	resolved, err := sfs.resolvePath(".")
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(resolved, "workspace"), "got: %s", resolved)
}

func TestScopedFS_CreateAndOpen(t *testing.T) {
	ctx := context.Background()
	sfs := setupTestFS(t)

	// Create a file
	w, err := sfs.Create(ctx, "hello.txt")
	require.NoError(t, err)
	_, err = io.WriteString(w, "hello world")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	// Open and read it back
	r, err := sfs.Open(ctx, "hello.txt")
	require.NoError(t, err)
	defer r.Close()

	data, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "hello world", string(data))
}

func TestScopedFS_CreateEscapeBlocked(t *testing.T) {
	ctx := context.Background()
	sfs := setupTestFS(t)

	_, err := sfs.Create(ctx, "../escape.txt")
	assert.True(t, errors.Is(err, ErrPathEscaped), "expected ErrPathEscaped, got: %v", err)
}

func TestScopedFS_ChangeDir(t *testing.T) {
	ctx := context.Background()
	sfs := setupTestFS(t)

	// Create a subdirectory
	err := sfs.MakeDir(ctx, "subdir", 0755)
	require.NoError(t, err)

	// Change into it
	err = sfs.ChangeDir(ctx, "subdir")
	require.NoError(t, err)
	assert.Equal(t, "subdir", sfs.WorkingDir())

	// Create a file inside the subdir
	w, err := sfs.Create(ctx, "inner.txt")
	require.NoError(t, err)
	_, _ = io.WriteString(w, "inner")
	require.NoError(t, w.Close())

	// Change back to root
	err = sfs.ChangeDir(ctx, ".")
	require.NoError(t, err)
	// After cd to ".", cwd resolves to rootPath itself, WorkingDir should be "."
	assert.Equal(t, ".", sfs.WorkingDir())

	// Verify the file exists at subdir/inner.txt from root
	exists, err := sfs.Exists(ctx, "subdir/inner.txt")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestScopedFS_ChangeDirEscapeBlocked(t *testing.T) {
	ctx := context.Background()
	sfs := setupTestFS(t)

	err := sfs.ChangeDir(ctx, "..")
	assert.Error(t, err)
}

func TestScopedFS_WorkingDir_Default(t *testing.T) {
	sfs := setupTestFS(t)
	assert.Equal(t, ".", sfs.WorkingDir())
}

func TestScopedFS_List(t *testing.T) {
	ctx := context.Background()
	sfs := setupTestFS(t)

	// Create some files
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		w, err := sfs.Create(ctx, name)
		require.NoError(t, err)
		require.NoError(t, w.Close())
	}

	entries, err := sfs.List(ctx, ".")
	require.NoError(t, err)

	names := make(map[string]bool)
	for _, e := range entries {
		names[e.Name()] = true
	}

	assert.True(t, names["a.txt"])
	assert.True(t, names["b.txt"])
	assert.True(t, names["c.txt"])
}

func TestScopedFS_Exists(t *testing.T) {
	ctx := context.Background()
	sfs := setupTestFS(t)

	// File should not exist yet
	exists, err := sfs.Exists(ctx, "newfile.txt")
	require.NoError(t, err)
	assert.False(t, exists)

	// Create the file
	w, err := sfs.Create(ctx, "newfile.txt")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	// Now it should exist
	exists, err = sfs.Exists(ctx, "newfile.txt")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestScopedFS_CopyMoveSameScope(t *testing.T) {
	ctx := context.Background()
	sfs := setupTestFS(t)

	// Create source file
	w, err := sfs.Create(ctx, "source.txt")
	require.NoError(t, err)
	_, _ = io.WriteString(w, "content")
	require.NoError(t, w.Close())

	// Copy source.txt → copy.txt
	err = sfs.Copy(ctx, "source.txt", "copy.txt")
	require.NoError(t, err)

	exists, err := sfs.Exists(ctx, "copy.txt")
	require.NoError(t, err)
	assert.True(t, exists)

	// Source still exists after copy
	exists, err = sfs.Exists(ctx, "source.txt")
	require.NoError(t, err)
	assert.True(t, exists)

	// Move copy.txt → moved.txt
	err = sfs.Move(ctx, "copy.txt", "moved.txt")
	require.NoError(t, err)

	exists, err = sfs.Exists(ctx, "moved.txt")
	require.NoError(t, err)
	assert.True(t, exists)

	// copy.txt should be gone
	exists, err = sfs.Exists(ctx, "copy.txt")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestScopedFS_CopyEscapeBlocked(t *testing.T) {
	ctx := context.Background()
	sfs := setupTestFS(t)

	// Create source file
	w, err := sfs.Create(ctx, "source.txt")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	// Try to copy to a path that escapes the scope
	err = sfs.Copy(ctx, "source.txt", "../../escape.txt")
	assert.True(t, errors.Is(err, ErrPathEscaped), "expected ErrPathEscaped, got: %v", err)
}

func TestScopedFS_Remove(t *testing.T) {
	ctx := context.Background()
	sfs := setupTestFS(t)

	// Create a file
	w, err := sfs.Create(ctx, "todelete.txt")
	require.NoError(t, err)
	require.NoError(t, w.Close())

	// Verify it exists
	exists, err := sfs.Exists(ctx, "todelete.txt")
	require.NoError(t, err)
	assert.True(t, exists)

	// Remove it
	err = sfs.Remove(ctx, "todelete.txt")
	require.NoError(t, err)

	// Verify it's gone
	exists, err = sfs.Exists(ctx, "todelete.txt")
	require.NoError(t, err)
	assert.False(t, exists)
}
