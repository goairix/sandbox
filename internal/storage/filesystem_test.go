package storage

import (
	"testing"

	"github.com/goairix/sandbox/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewFileSystem_Local(t *testing.T) {
	dir := t.TempDir()
	cfg := config.FileSystemConfig{
		Provider:  "local",
		LocalPath: dir,
	}

	fsys, err := NewFileSystem(cfg)
	require.NoError(t, err)
	assert.NotNil(t, fsys)
}

func TestNewFileSystem_LocalEmptyPath(t *testing.T) {
	cfg := config.FileSystemConfig{
		Provider:  "local",
		LocalPath: "",
	}

	_, err := NewFileSystem(cfg)
	assert.Error(t, err)
}

func TestNewFileSystem_UnknownProvider(t *testing.T) {
	cfg := config.FileSystemConfig{
		Provider: "unknown",
	}

	_, err := NewFileSystem(cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported filesystem provider")
}
