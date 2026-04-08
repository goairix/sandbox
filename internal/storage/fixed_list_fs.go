package storage

import (
	"context"
	"io"
	"strings"

	"github.com/dysodeng/fs"
)

// fixedListFS wraps an fs.FileSystem to fix broken List behavior in drivers
// (like MinIO) that strip trailing "/" from the prefix, causing S3 non-recursive
// listing to return the directory itself as a common prefix instead of its contents.
//
// Workaround: append "/" to the path before calling the underlying List, then
// trim the prefix from returned keys to normalize the result.
type fixedListFS struct {
	fs.FileSystem
}

// NewFixedListFS wraps a filesystem to fix the List method for object-storage drivers.
func NewFixedListFS(fsys fs.FileSystem) fs.FileSystem {
	return &fixedListFS{FileSystem: fsys}
}

func (f *fixedListFS) List(ctx context.Context, path string, opts ...fs.Option) ([]fs.FileInfo, error) {
	// The underlying driver does: Prefix = strings.TrimRight(path, "/")
	// To get "workspaces/user123/project-a/" as prefix, we need the path to be
	// "workspaces/user123/project-a//" so that TrimRight leaves one "/".
	fixedPath := strings.TrimRight(path, "/") + "//"
	return f.FileSystem.List(ctx, fixedPath, opts...)
}

// Delegate all other methods to the underlying filesystem (fs.FileSystem embedding
// handles this automatically, but Uploader needs explicit delegation).
func (f *fixedListFS) Uploader() fs.Uploader {
	return f.FileSystem.Uploader()
}

// The following methods are delegated via embedding:
// Create, Open, OpenFile, Remove, Copy, Move, Rename,
// MakeDir, RemoveDir, Stat, Exists, IsDir, IsFile,
// GetMimeType, SetMetadata, GetMetadata,
// SignFullUrl, FullUrl, RelativePath

// Override methods that take path arguments to ensure they go through
// the original filesystem (not the fixed one). The embedding already
// delegates correctly, but we explicitly list them for clarity.
func (f *fixedListFS) Create(ctx context.Context, path string, opts ...fs.Option) (io.WriteCloser, error) {
	return f.FileSystem.Create(ctx, path, opts...)
}

func (f *fixedListFS) Open(ctx context.Context, path string, opts ...fs.Option) (io.ReadCloser, error) {
	return f.FileSystem.Open(ctx, path, opts...)
}

func (f *fixedListFS) Stat(ctx context.Context, path string, opts ...fs.Option) (fs.FileInfo, error) {
	return f.FileSystem.Stat(ctx, path, opts...)
}

func (f *fixedListFS) Exists(ctx context.Context, path string, opts ...fs.Option) (bool, error) {
	return f.FileSystem.Exists(ctx, path, opts...)
}

func (f *fixedListFS) IsDir(ctx context.Context, path string, opts ...fs.Option) (bool, error) {
	return f.FileSystem.IsDir(ctx, path, opts...)
}

func (f *fixedListFS) IsFile(ctx context.Context, path string, opts ...fs.Option) (bool, error) {
	return f.FileSystem.IsFile(ctx, path, opts...)
}
