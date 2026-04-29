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
// rootPath is interpreted relative to the underlying filesystem's root (as the driver sees it).
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
	// Ensure the resolved path is equal to rootPath or a child of it.
	if joined != s.rootPath && !strings.HasPrefix(joined, s.rootPath+"/") {
		return "", ErrPathEscaped
	}
	return joined, nil
}

func cleanRoot(root string) string {
	// For relative paths, Clean then trim trailing slashes.
	cleaned := path.Clean(root)
	if cleaned == "/" {
		return ""
	}
	// Strip leading slash to convert absolute paths to relative.
	// Object storage (MinIO/S3) rejects object names starting with "/".
	cleaned = strings.TrimLeft(cleaned, "/")
	return strings.TrimRight(cleaned, "/")
}

// List lists the contents of directory p.
func (s *scopedFS) List(ctx context.Context, p string, opts ...fs.Option) ([]fs.FileInfo, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	return s.driver.List(ctx, resolved, opts...)
}

// MakeDir creates a directory at path p with the given permissions.
func (s *scopedFS) MakeDir(ctx context.Context, p string, perm os.FileMode, opts ...fs.Option) error {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return err
	}
	return s.driver.MakeDir(ctx, resolved, perm, opts...)
}

// RemoveDir removes the directory at path p.
func (s *scopedFS) RemoveDir(ctx context.Context, p string, opts ...fs.Option) error {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return err
	}
	return s.driver.RemoveDir(ctx, resolved, opts...)
}

// Create creates a file at path p and returns a WriteCloser.
func (s *scopedFS) Create(ctx context.Context, p string, opts ...fs.Option) (io.WriteCloser, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	return s.driver.Create(ctx, resolved, opts...)
}

// Open opens the file at path p and returns a ReadCloser.
func (s *scopedFS) Open(ctx context.Context, p string, opts ...fs.Option) (io.ReadCloser, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	return s.driver.Open(ctx, resolved, opts...)
}

// OpenFile opens the file at path p with the given flags and permissions.
func (s *scopedFS) OpenFile(ctx context.Context, p string, flag int, perm os.FileMode, opts ...fs.Option) (io.ReadWriteCloser, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	return s.driver.OpenFile(ctx, resolved, flag, perm, opts...)
}

// Remove removes the file at path p.
func (s *scopedFS) Remove(ctx context.Context, p string, opts ...fs.Option) error {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return err
	}
	return s.driver.Remove(ctx, resolved, opts...)
}

// Copy copies the file at src to dst.
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

// Move moves the file at src to dst.
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

// Rename renames oldPath to newPath.
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

// Stat returns file info for path p.
func (s *scopedFS) Stat(ctx context.Context, p string, opts ...fs.Option) (fs.FileInfo, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return nil, err
	}
	return s.driver.Stat(ctx, resolved, opts...)
}

// Exists reports whether path p exists.
func (s *scopedFS) Exists(ctx context.Context, p string, opts ...fs.Option) (bool, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return false, err
	}
	return s.driver.Exists(ctx, resolved, opts...)
}

// IsDir reports whether path p is a directory.
func (s *scopedFS) IsDir(ctx context.Context, p string, opts ...fs.Option) (bool, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return false, err
	}
	return s.driver.IsDir(ctx, resolved, opts...)
}

// IsFile reports whether path p is a regular file.
func (s *scopedFS) IsFile(ctx context.Context, p string, opts ...fs.Option) (bool, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return false, err
	}
	return s.driver.IsFile(ctx, resolved, opts...)
}

// SignFullUrl returns a signed URL for path p.
func (s *scopedFS) SignFullUrl(ctx context.Context, p string, opts ...fs.Option) (string, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return "", err
	}
	return s.driver.SignFullUrl(ctx, resolved, opts...)
}

// FullUrl returns the full URL for path p.
func (s *scopedFS) FullUrl(ctx context.Context, p string, opts ...fs.Option) (string, error) {
	resolved, err := s.resolvePath(p)
	if err != nil {
		return "", err
	}
	return s.driver.FullUrl(ctx, resolved, opts...)
}

// RelativePath returns the relative path by stripping the rootPath prefix from the result.
func (s *scopedFS) RelativePath(ctx context.Context, fullUrl string, opts ...fs.Option) (string, error) {
	rel, err := s.driver.RelativePath(ctx, fullUrl, opts...)
	if err != nil {
		return "", err
	}
	// Strip the rootPath prefix to return path relative to the scoped root.
	rel = strings.TrimPrefix(rel, s.rootPath)
	rel = strings.TrimPrefix(rel, "/")
	return rel, nil
}

// ChangeDir changes the working directory to p (relative to rootPath).
// The path p is always interpreted relative to rootPath (not the current cwd).
// The target must be a directory within the scope.
func (s *scopedFS) ChangeDir(ctx context.Context, p string) error {
	if path.IsAbs(p) {
		return ErrPathEscaped
	}
	// Resolve relative to rootPath, ignoring current cwd.
	joined := path.Clean(path.Join(s.rootPath, p))
	if joined != s.rootPath && !strings.HasPrefix(joined, s.rootPath+"/") {
		return ErrPathEscaped
	}
	isDir, err := s.driver.IsDir(ctx, joined)
	if err != nil {
		return fmt.Errorf("storage: ChangeDir stat failed: %w", err)
	}
	if !isDir {
		return fmt.Errorf("storage: ChangeDir target is not a directory: %s", p)
	}
	// Store cwd relative to rootPath.
	rel := strings.TrimPrefix(joined, s.rootPath)
	rel = strings.TrimPrefix(rel, "/")
	s.cwd = rel
	return nil
}

// WorkingDir returns the current working directory relative to rootPath.
// Returns "." if no ChangeDir has been called.
func (s *scopedFS) WorkingDir() string {
	if s.cwd == "" {
		return "."
	}
	return s.cwd
}
