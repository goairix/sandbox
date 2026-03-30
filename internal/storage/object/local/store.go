package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/storage/object"
)

// Store implements object.Store using the local filesystem.
type Store struct {
	basePath string
}

// New creates a new local filesystem object store.
func New(basePath string) *Store {
	return &Store{basePath: basePath}
}

func (s *Store) fullPath(key string) (string, error) {
	joined := filepath.Join(s.basePath, filepath.FromSlash(key))
	resolved, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("local store: resolve path: %w", err)
	}
	base, err := filepath.Abs(s.basePath)
	if err != nil {
		return "", fmt.Errorf("local store: resolve base path: %w", err)
	}
	// Ensure the resolved path is within the base path
	if !strings.HasPrefix(resolved, base+string(filepath.Separator)) && resolved != base {
		return "", fmt.Errorf("local store: path %q escapes base directory", key)
	}
	return resolved, nil
}

func (s *Store) Put(_ context.Context, key string, reader io.Reader, _ int64) error {
	path, err := s.fullPath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, reader)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func (s *Store) Get(_ context.Context, key string) (io.ReadCloser, error) {
	path, err := s.fullPath(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (s *Store) Delete(_ context.Context, key string) error {
	path, err := s.fullPath(key)
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (s *Store) List(_ context.Context, prefix string) ([]object.ObjectInfo, error) {
	dir, err := s.fullPath(prefix)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var result []object.ObjectInfo
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		key := prefix + entry.Name()
		// Normalize to forward slashes
		key = strings.ReplaceAll(key, string(filepath.Separator), "/")
		result = append(result, object.ObjectInfo{
			Key:          key,
			Size:         info.Size(),
			LastModified: info.ModTime(),
		})
	}
	return result, nil
}

func (s *Store) Exists(_ context.Context, key string) (bool, error) {
	path, err := s.fullPath(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func (s *Store) PresignedPutURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", errors.New("presigned URLs not supported by local store")
}

func (s *Store) PresignedGetURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", errors.New("presigned URLs not supported by local store")
}
