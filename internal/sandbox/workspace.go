package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
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

// syncToContainer builds a single tar archive from all files in ScopedFS
// and uploads it to the container in one API call.
func (m *Manager) syncToContainer(ctx context.Context, scoped storage.ScopedFS, runtimeID string) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	if err := m.addDirToTar(ctx, tw, scoped, "."); err != nil {
		tw.Close()
		return fmt.Errorf("build tar archive: %w", err)
	}
	tw.Close()

	if buf.Len() == 0 {
		return nil // nothing to sync
	}

	return m.runtime.UploadArchive(ctx, runtimeID, "/workspace", &buf)
}

// addDirToTar recursively adds all files and directories from ScopedFS into the tar writer.
func (m *Manager) addDirToTar(ctx context.Context, tw *tar.Writer, scoped storage.ScopedFS, dir string) error {
	files, err := scoped.List(ctx, dir)
	if err != nil {
		return err
	}

	for _, fi := range files {
		// Extract just the base name — MinIO returns full object keys (e.g.
		// "workspaces/user123/project-a/hello.txt") while local FS returns
		// just "hello.txt". Use filepath.Base to normalize.
		baseName := filepath.Base(strings.TrimRight(fi.Name(), "/"))
		if baseName == "." || baseName == "" {
			continue
		}

		relPath := baseName
		if dir != "." {
			relPath = filepath.Join(dir, baseName)
		}

		if fi.IsDir() {
			// Add directory entry to tar
			if err := tw.WriteHeader(&tar.Header{
				Name:     relPath + "/",
				Typeflag: tar.TypeDir,
				Mode:     0755,
				ModTime:  fi.ModTime(),
				Uid:      1000,
				Gid:      1000,
			}); err != nil {
				return err
			}
			if err := m.addDirToTar(ctx, tw, scoped, relPath); err != nil {
				return err
			}
			continue
		}

		reader, err := scoped.Open(ctx, relPath)
		if err != nil {
			return err
		}

		content, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			return err
		}

		if err := tw.WriteHeader(&tar.Header{
			Name:    relPath,
			Size:    int64(len(content)),
			Mode:    0644,
			ModTime: fi.ModTime(),
			Uid:     1000,
			Gid:     1000,
		}); err != nil {
			return err
		}
		if _, err := tw.Write(content); err != nil {
			return err
		}
	}

	return nil
}

// syncFromContainer downloads the entire /workspace directory from the container
// as a single tar archive and extracts it into ScopedFS.
func (m *Manager) syncFromContainer(ctx context.Context, sandboxID, runtimeID string) error {
	m.mu.RLock()
	scoped, ok := m.workspaces[sandboxID]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("no workspace for sandbox %s", sandboxID)
	}

	tarReader, err := m.runtime.DownloadDir(ctx, runtimeID, "/workspace")
	if err != nil {
		return fmt.Errorf("download workspace: %w", err)
	}
	defer tarReader.Close()

	tr := tar.NewReader(tarReader)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar entry: %w", err)
		}

		// Strip the root "workspace/" prefix added by Docker/tar
		name := strings.TrimPrefix(hdr.Name, "workspace/")
		if name == "" {
			continue
		}

		if hdr.Typeflag == tar.TypeDir {
			_ = scoped.MakeDir(ctx, strings.TrimRight(name, "/"), 0755)
			continue
		}

		writer, err := scoped.Create(ctx, name)
		if err != nil {
			return fmt.Errorf("create %q: %w", name, err)
		}

		_, copyErr := io.Copy(writer, tr)
		closeErr := writer.Close()
		if copyErr != nil {
			return fmt.Errorf("write %q: %w", name, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("flush %q to storage: %w", name, closeErr)
		}
	}

	return nil
}
