package sandbox

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/goairix/sandbox/internal/storage"
)

// MountWorkspace creates a ScopedFS for the given rootPath, syncs files into the container.
func (m *Manager) MountWorkspace(ctx context.Context, sandboxID, rootPath string) error {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	runtimeID := sb.RuntimeID
	_, exists := m.workspaces[sandboxID]
	m.mu.RUnlock()

	if exists {
		return fmt.Errorf("workspace already mounted for sandbox %s", sandboxID)
	}

	scoped, err := storage.NewScopedFS(m.filesystem, rootPath)
	if err != nil {
		return fmt.Errorf("create scoped filesystem: %w", err)
	}

	// Sync files from storage to container
	if err := m.syncToContainer(ctx, scoped, runtimeID); err != nil {
		return fmt.Errorf("sync to container: %w", err)
	}

	m.mu.Lock()
	m.workspaces[sandboxID] = scoped
	sb.Workspace = &WorkspaceInfo{
		RootPath:  rootPath,
		MountedAt: time.Now(),
	}
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	return nil
}

// UnmountWorkspace syncs files back from container to storage, then detaches.
func (m *Manager) UnmountWorkspace(ctx context.Context, sandboxID string) error {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	runtimeID := sb.RuntimeID
	_, hasWS := m.workspaces[sandboxID]
	m.mu.RUnlock()

	if !hasWS {
		return fmt.Errorf("no workspace mounted for sandbox %s", sandboxID)
	}

	if err := m.syncFromContainer(ctx, sandboxID, runtimeID); err != nil {
		return fmt.Errorf("sync from container: %w", err)
	}

	m.mu.Lock()
	delete(m.workspaces, sandboxID)
	sb.Workspace = nil
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	return nil
}

// SyncWorkspace manually syncs files in the given direction.
func (m *Manager) SyncWorkspace(ctx context.Context, sandboxID, direction string) error {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("sandbox not found: %s", sandboxID)
	}
	runtimeID := sb.RuntimeID
	scoped, hasWS := m.workspaces[sandboxID]
	m.mu.RUnlock()

	if !hasWS {
		return fmt.Errorf("no workspace mounted for sandbox %s", sandboxID)
	}

	switch direction {
	case "to_container":
		return m.syncToContainer(ctx, scoped, runtimeID)
	case "from_container":
		return m.syncFromContainer(ctx, sandboxID, runtimeID)
	default:
		return fmt.Errorf("invalid sync direction: %s", direction)
	}
}

// GetWorkspaceInfo returns workspace info for a sandbox.
func (m *Manager) GetWorkspaceInfo(_ context.Context, sandboxID string) (*WorkspaceInfo, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[sandboxID]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("sandbox not found: %s", sandboxID)
	}

	return sb.Workspace, nil
}

// syncToContainer uploads all files from ScopedFS into the container /workspace.
func (m *Manager) syncToContainer(ctx context.Context, scoped storage.ScopedFS, runtimeID string) error {
	return m.syncDir(ctx, scoped, runtimeID, ".")
}

// syncDir recursively syncs a directory from ScopedFS into the container.
func (m *Manager) syncDir(ctx context.Context, scoped storage.ScopedFS, runtimeID, dir string) error {
	files, err := scoped.List(ctx, dir)
	if err != nil {
		return fmt.Errorf("list %q: %w", dir, err)
	}

	for _, fi := range files {
		relPath := dir
		if relPath == "." {
			relPath = fi.Name()
		} else {
			relPath = filepath.Join(dir, fi.Name())
		}

		if fi.IsDir() {
			if err := m.syncDir(ctx, scoped, runtimeID, relPath); err != nil {
				return err
			}
			continue
		}

		reader, err := scoped.Open(ctx, relPath)
		if err != nil {
			return fmt.Errorf("open %q: %w", relPath, err)
		}

		destPath := filepath.Join("/workspace", relPath)
		uploadErr := m.runtime.UploadFile(ctx, runtimeID, destPath, reader)
		reader.Close()
		if uploadErr != nil {
			return fmt.Errorf("upload %q: %w", destPath, uploadErr)
		}
	}

	return nil
}

// syncFromContainer downloads all files from container /workspace into ScopedFS.
func (m *Manager) syncFromContainer(ctx context.Context, sandboxID, runtimeID string) error {
	m.mu.RLock()
	scoped, ok := m.workspaces[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no workspace for sandbox %s", sandboxID)
	}

	files, err := m.runtime.ListFiles(ctx, runtimeID, "/workspace")
	if err != nil {
		return fmt.Errorf("list container files: %w", err)
	}

	for _, fi := range files {
		if fi.IsDir {
			continue
		}

		// Compute relative path from /workspace
		relPath := fi.Path
		if len(relPath) > len("/workspace/") {
			relPath = relPath[len("/workspace/"):]
		} else {
			relPath = fi.Name
		}

		reader, err := m.runtime.DownloadFile(ctx, runtimeID, fi.Path)
		if err != nil {
			return fmt.Errorf("download %q: %w", fi.Path, err)
		}

		writer, err := scoped.Create(ctx, relPath)
		if err != nil {
			reader.Close()
			return fmt.Errorf("create %q: %w", relPath, err)
		}

		_, copyErr := io.Copy(writer, reader)
		reader.Close()
		writer.Close()
		if copyErr != nil {
			return fmt.Errorf("copy %q: %w", relPath, copyErr)
		}
	}

	return nil
}
