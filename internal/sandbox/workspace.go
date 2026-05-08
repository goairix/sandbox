package sandbox

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
)

// maxConcurrentReads limits the number of parallel file reads from storage
// during syncToContainer. This bounds memory usage and avoids overwhelming
// cloud storage backends with too many concurrent HTTP requests.
const maxConcurrentReads = 8

// fileEntry holds metadata and content for a single file or directory,
// collected during the parallel-read phase of syncToContainer.
type fileEntry struct {
	relPath string
	isDir   bool
	size    int64
	modTime time.Time
}

// isExcluded reports whether path should be skipped during workspace sync.
// A path is excluded if it equals any exclude entry or starts with an exclude
// entry followed by "/". For example, exclude entry ".agent" matches ".agent",
// ".agent/", ".agent/skills/code.yaml", etc.
func isExcluded(path string, exclude []string) bool {
	for _, e := range exclude {
		if path == e || strings.HasPrefix(path, e+"/") {
			return true
		}
	}
	return false
}

// MountWorkspace creates a ScopedFS for the given rootPath, syncs files into the container.
// exclude is an optional list of path prefixes to skip during all subsequent syncs.
func (m *Manager) MountWorkspace(ctx context.Context, sandboxID, rootPath string, exclude []string) error {
	logger.Info(ctx, "MountWorkspace: starting",
		logger.AddField("sandbox_id", sandboxID),
		logger.AddField("root_path", rootPath),
	)

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
		logger.Error(ctx, "MountWorkspace: create scoped filesystem failed",
			logger.AddField("sandbox_id", sandboxID),
			logger.AddField("root_path", rootPath),
			logger.ErrorField(err),
		)
		return fmt.Errorf("create scoped filesystem: %w", err)
	}

	// Sync files from storage to container
	if err := m.syncToContainer(ctx, scoped, runtimeID); err != nil {
		logger.Error(ctx, "MountWorkspace: sync to container failed",
			logger.AddField("sandbox_id", sandboxID),
			logger.AddField("runtime_id", runtimeID),
			logger.ErrorField(err),
		)
		return fmt.Errorf("sync to container: %w", err)
	}

	now := time.Now()
	m.mu.Lock()
	m.workspaces[sandboxID] = scoped
	sb.Workspace = &WorkspaceInfo{
		RootPath:     rootPath,
		MountedAt:    now,
		LastSyncedAt: now,
		SyncExclude:  exclude,
	}
	sb.UpdatedAt = now
	m.mu.Unlock()

	// Persist workspace info to session store
	if m.sessions != nil {
		_ = m.sessions.Save(ctx, sb)
	}

	logger.Info(ctx, "MountWorkspace: completed",
		logger.AddField("sandbox_id", sandboxID),
		logger.AddField("runtime_id", runtimeID),
	)

	return nil
}

// UnmountWorkspace syncs files back from container to storage, then detaches.
func (m *Manager) UnmountWorkspace(ctx context.Context, sandboxID string) error {
	logger.Info(ctx, "UnmountWorkspace: starting",
		logger.AddField("sandbox_id", sandboxID),
	)

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

	if err := m.syncFromContainer(ctx, sandboxID, runtimeID, sb.Workspace.SyncExclude); err != nil {
		logger.Error(ctx, "UnmountWorkspace: sync from container failed",
			logger.AddField("sandbox_id", sandboxID),
			logger.AddField("runtime_id", runtimeID),
			logger.ErrorField(err),
		)
		return fmt.Errorf("sync from container: %w", err)
	}

	m.mu.Lock()
	delete(m.workspaces, sandboxID)
	sb.Workspace = nil
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	// Persist workspace removal to session store
	if m.sessions != nil {
		_ = m.sessions.Save(ctx, sb)
	}

	logger.Info(ctx, "UnmountWorkspace: completed",
		logger.AddField("sandbox_id", sandboxID),
	)

	return nil
}

// SyncWorkspace manually syncs files in the given direction.
// exclude is an optional list of path prefixes to skip during from_container sync.
func (m *Manager) SyncWorkspace(ctx context.Context, sandboxID, direction string, exclude []string) error {
	logger.Info(ctx, "SyncWorkspace: request received",
		logger.AddField("sandbox_id", sandboxID),
		logger.AddField("direction", direction),
	)

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

	var err error
	switch direction {
	case "to_container":
		err = m.syncToContainer(ctx, scoped, runtimeID)
	case "from_container":
		err = m.syncFromContainer(ctx, sandboxID, runtimeID, exclude)
	default:
		return fmt.Errorf("invalid sync direction: %s", direction)
	}

	if err != nil {
		logger.Error(ctx, "SyncWorkspace: failed",
			logger.AddField("sandbox_id", sandboxID),
			logger.AddField("direction", direction),
			logger.ErrorField(err),
		)
	} else {
		logger.Info(ctx, "SyncWorkspace: completed",
			logger.AddField("sandbox_id", sandboxID),
			logger.AddField("direction", direction),
		)
	}
	return err
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

// syncToContainer collects file metadata from ScopedFS, concurrently reads
// file contents, and streams a tar archive into the container via exec pipe.
// The container-side "tar xf -" process consumes data in real-time, keeping
// memory usage proportional to the concurrency window rather than total size.
func (m *Manager) syncToContainer(ctx context.Context, scoped storage.ScopedFS, runtimeID string) error {
	// Phase 1: walk the directory tree to collect file metadata.
	var entries []fileEntry
	if err := m.collectFiles(ctx, scoped, ".", &entries); err != nil {
		logger.Error(ctx, "syncToContainer: collectFiles failed",
			logger.AddField("runtime_id", runtimeID),
			logger.ErrorField(err),
		)
		return fmt.Errorf("collect files: %w", err)
	}
	if len(entries) == 0 {
		logger.Debug(ctx, "syncToContainer: no files to sync",
			logger.AddField("runtime_id", runtimeID),
		)
		return nil
	}

	logger.Debug(ctx, "syncToContainer: starting tar upload",
		logger.AddField("runtime_id", runtimeID),
		logger.AddField("entries", len(entries)),
	)

	// Phase 2: stream tar into container via exec pipe.
	pr, pw := io.Pipe()

	execErrCh := make(chan error, 1)
	go func() {
		execErrCh <- m.runtime.ExecPipe(ctx, runtimeID,
			[]string{"tar", "xf", "-", "-C", "/workspace"}, pr)
	}()

	writeErr := m.writeTarStream(ctx, scoped, entries, pw)
	if writeErr != nil {
		pw.CloseWithError(writeErr)
	} else {
		pw.Close()
	}

	execErr := <-execErrCh

	if writeErr != nil {
		logger.Error(ctx, "syncToContainer: write tar stream failed",
			logger.AddField("runtime_id", runtimeID),
			logger.ErrorField(writeErr),
		)
		return fmt.Errorf("write tar stream: %w", writeErr)
	}
	if execErr != nil {
		logger.Error(ctx, "syncToContainer: exec tar extract failed",
			logger.AddField("runtime_id", runtimeID),
			logger.ErrorField(execErr),
		)
		return fmt.Errorf("exec tar extract: %w", execErr)
	}

	logger.Debug(ctx, "syncToContainer: completed",
		logger.AddField("runtime_id", runtimeID),
		logger.AddField("entries", len(entries)),
	)
	return nil
}

// writeTarStream writes all entries as a tar archive to w, reading files
// concurrently (up to maxConcurrentReads) to reduce I/O latency.
func (m *Manager) writeTarStream(ctx context.Context, scoped storage.ScopedFS, entries []fileEntry, w io.Writer) error {
	if len(entries) == 0 {
		return nil
	}

	tw := tar.NewWriter(w)

	type readResult struct {
		content []byte
		err     error
	}

	sem := make(chan struct{}, maxConcurrentReads)
	resultChs := make([]chan readResult, len(entries))

	for i, e := range entries {
		if e.isDir {
			continue
		}
		ch := make(chan readResult, 1)
		resultChs[i] = ch
		go func(entry fileEntry, ch chan<- readResult) {
			sem <- struct{}{}
			defer func() { <-sem }()
			if ctx.Err() != nil {
				ch <- readResult{err: ctx.Err()}
				return
			}
			reader, err := scoped.Open(ctx, entry.relPath)
			if err != nil {
				ch <- readResult{err: fmt.Errorf("open %q: %w", entry.relPath, err)}
				return
			}
			data, err := io.ReadAll(reader)
			reader.Close()
			if err != nil {
				ch <- readResult{err: fmt.Errorf("read %q: %w", entry.relPath, err)}
				return
			}
			ch <- readResult{content: data}
		}(e, ch)
	}

	cleanup := func(start int) {
		for j := start; j < len(entries); j++ {
			if resultChs[j] != nil {
				<-resultChs[j]
			}
		}
	}

	for i, e := range entries {
		if e.isDir {
			if err := tw.WriteHeader(&tar.Header{
				Name:     e.relPath + "/",
				Typeflag: tar.TypeDir,
				Mode:     0755,
				ModTime:  e.modTime,
				Uid:      1000,
				Gid:      1000,
				Format:   tar.FormatPAX,
			}); err != nil {
				cleanup(i)
				return fmt.Errorf("write dir header %q: %w", e.relPath, err)
			}
			continue
		}

		res := <-resultChs[i]
		if res.err != nil {
			cleanup(i + 1)
			return res.err
		}

		if err := tw.WriteHeader(&tar.Header{
			Name:    e.relPath,
			Size:    int64(len(res.content)),
			Mode:    0644,
			ModTime: e.modTime,
			Uid:     1000,
			Gid:     1000,
			Format:  tar.FormatPAX,
		}); err != nil {
			cleanup(i + 1)
			return fmt.Errorf("write file header %q: %w", e.relPath, err)
		}

		if _, err := tw.Write(res.content); err != nil {
			cleanup(i + 1)
			return fmt.Errorf("write file content %q: %w", e.relPath, err)
		}
	}

	return tw.Close()
}

// collectFiles recursively walks the ScopedFS directory tree and appends
// file/directory metadata to entries. File content is NOT read here.
func (m *Manager) collectFiles(ctx context.Context, scoped storage.ScopedFS, dir string, entries *[]fileEntry) error {
	files, err := scoped.List(ctx, dir)
	if err != nil {
		return err
	}

	for _, fi := range files {
		// Normalize: MinIO returns full object keys, local FS returns base names.
		baseName := filepath.Base(strings.TrimRight(fi.Name(), "/"))
		if baseName == "." || baseName == "" {
			continue
		}

		relPath := baseName
		if dir != "." {
			relPath = filepath.Join(dir, baseName)
		}

		*entries = append(*entries, fileEntry{
			relPath: relPath,
			isDir:   fi.IsDir(),
			size:    fi.Size(),
			modTime: fi.ModTime(),
		})

		if fi.IsDir() {
			if err := m.collectFiles(ctx, scoped, relPath, entries); err != nil {
				return err
			}
		}
	}
	return nil
}

// syncFromContainer incrementally syncs changed files from the container back to storage.
// It compares container file modification times against the last sync timestamp
// and only writes files that were modified since then. Files deleted in the container
// are also removed from storage. Mount sets LastSyncedAt, so every syncFromContainer
// is incremental.
func (m *Manager) syncFromContainer(ctx context.Context, sandboxID, runtimeID string, exclude []string) error {
	m.mu.RLock()
	scoped, ok := m.workspaces[sandboxID]
	sb := m.sandboxes[sandboxID]
	m.mu.RUnlock()
	if !ok {
		logger.Debug(ctx, "syncFromContainer: no workspace found",
			logger.AddField("sandbox_id", sandboxID),
		)
		return fmt.Errorf("no workspace for sandbox %s", sandboxID)
	}

	// Skip sync if the sandbox is gone from the map — it has already been
	// fully destroyed and the pod is likely deleted.
	if sb == nil {
		logger.Debug(ctx, "syncFromContainer: sandbox nil, skipping",
			logger.AddField("sandbox_id", sandboxID),
		)
		return nil
	}

	// Bind-mounted workspaces share the host filesystem directly — no sync needed.
	if sb.Workspace != nil && sb.Workspace.BindMounted {
		logger.Debug(ctx, "syncFromContainer: bind-mounted, skipping",
			logger.AddField("sandbox_id", sandboxID),
		)
		return nil
	}

	// LastSyncedAt is set at mount time; use it as the change detection baseline.
	var cutoff int64
	if sb.Workspace != nil && !sb.Workspace.LastSyncedAt.IsZero() {
		cutoff = sb.Workspace.LastSyncedAt.Unix()
	}

	// Record the sync start time BEFORE collecting the manifest. Any file
	// modified after this point will be picked up by the next sync cycle.
	syncStartedAt := time.Now()

	logger.Debug(ctx, "syncFromContainer: collecting manifest",
		logger.AddField("sandbox_id", sandboxID),
		logger.AddField("runtime_id", runtimeID),
		logger.AddField("cutoff", cutoff),
	)

	// Get container file manifest via exec
	manifest, err := m.containerFileManifest(ctx, runtimeID)
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			logger.Debug(ctx, "syncFromContainer: container not found",
				logger.AddField("sandbox_id", sandboxID),
				logger.AddField("runtime_id", runtimeID),
			)
			return err
		}
		// Fall back to full sync if manifest collection fails
		logger.Warn(ctx, "container manifest failed, falling back to full sync",
			logger.AddField("runtime_id", runtimeID),
			logger.ErrorField(err),
		)
		if err := m.fullSyncFromContainer(ctx, scoped, runtimeID, exclude); err != nil {
			return err
		}
		m.setLastSyncedAt(sb, syncStartedAt)
		m.saveSessionIfAlive(ctx, sandboxID, sb)
		return nil
	}

	logger.Debug(ctx, "syncFromContainer: manifest collected",
		logger.AddField("sandbox_id", sandboxID),
		logger.AddField("manifest_files", len(manifest)),
	)

	// Get storage file set
	storageFiles, err := m.storageFileSet(ctx, scoped, ".")
	if err != nil {
		logger.Warn(ctx, "storage file listing failed, falling back to full sync",
			logger.AddField("runtime_id", runtimeID),
			logger.ErrorField(err),
		)
		if err := m.fullSyncFromContainer(ctx, scoped, runtimeID, exclude); err != nil {
			return err
		}
		m.setLastSyncedAt(sb, syncStartedAt)
		m.saveSessionIfAlive(ctx, sandboxID, sb)
		return nil
	}

	logger.Debug(ctx, "syncFromContainer: storage files listed",
		logger.AddField("sandbox_id", sandboxID),
		logger.AddField("storage_files", len(storageFiles)),
	)

	// Compute changed files: container files that are new or modified since cutoff
	changedSet := make(map[string]struct{})
	for path, modtime := range manifest {
		if strings.HasSuffix(path, "/") {
			continue // skip directories
		}
		if isExcluded(path, exclude) {
			continue
		}
		_, inStorage := storageFiles[path]
		if !inStorage || cutoff == 0 || modtime >= cutoff {
			changedSet[path] = struct{}{}
		}
	}

	// Compute deleted files: in storage but not in container
	var deletedFiles []string
	for path := range storageFiles {
		if isExcluded(path, exclude) {
			continue
		}
		if _, exists := manifest[path]; !exists {
			deletedFiles = append(deletedFiles, path)
		}
	}

	logger.Debug(ctx, "syncFromContainer: diff computed",
		logger.AddField("sandbox_id", sandboxID),
		logger.AddField("changed", len(changedSet)),
		logger.AddField("deleted", len(deletedFiles)),
		logger.AddField("cutoff", cutoff),
	)

	// Nothing to do
	if len(changedSet) == 0 && len(deletedFiles) == 0 {
		m.setLastSyncedAt(sb, syncStartedAt)
		m.saveSessionIfAlive(ctx, sandboxID, sb)
		return nil
	}

	// Download tar and selectively extract only changed files
	if len(changedSet) > 0 {
		logger.Debug(ctx, "syncFromContainer: downloading changed files",
			logger.AddField("sandbox_id", sandboxID),
			logger.AddField("count", len(changedSet)),
		)
		if err := m.downloadChangedFiles(ctx, scoped, runtimeID, changedSet, exclude); err != nil {
			logger.Error(ctx, "syncFromContainer: download changed files failed",
				logger.AddField("sandbox_id", sandboxID),
				logger.AddField("runtime_id", runtimeID),
				logger.ErrorField(err),
			)
			return fmt.Errorf("download changed files: %w", err)
		}
	}

	// Remove deleted files from storage
	for _, path := range deletedFiles {
		_ = scoped.Remove(ctx, path)
	}

	logger.Info(ctx, "syncFromContainer: completed",
		logger.AddField("sandbox_id", sandboxID),
		logger.AddField("synced", len(changedSet)),
		logger.AddField("removed", len(deletedFiles)),
	)

	m.setLastSyncedAt(sb, syncStartedAt)
	m.saveSessionIfAlive(ctx, sandboxID, sb)
	return nil
}

// setLastSyncedAt updates the LastSyncedAt timestamp under lock.
func (m *Manager) setLastSyncedAt(sb *Sandbox, t time.Time) {
	if sb == nil || sb.Workspace == nil {
		return
	}
	m.mu.Lock()
	sb.Workspace.LastSyncedAt = t
	sb.UpdatedAt = t
	m.mu.Unlock()
}

// saveSessionIfAlive persists sb to the session store only if the sandbox is
// still registered in the in-memory map. This prevents a concurrent Destroy
// (which removes the sandbox from the map and deletes the Redis key) from
// having its key re-created by an in-flight autoSync goroutine that captured
// the sb pointer before Destroy ran.
func (m *Manager) saveSessionIfAlive(ctx context.Context, sandboxID string, sb *Sandbox) {
	if m.sessions == nil || sb == nil {
		return
	}
	m.mu.RLock()
	_, alive := m.sandboxes[sandboxID]
	m.mu.RUnlock()
	if alive {
		_ = m.sessions.Save(ctx, sb)
	}
}

// fullSyncFromContainer downloads the entire /workspace as a tar and extracts all files.
func (m *Manager) fullSyncFromContainer(ctx context.Context, scoped storage.ScopedFS, runtimeID string, exclude []string) error {
	tarReader, err := m.runtime.DownloadDir(ctx, runtimeID, "/workspace")
	if err != nil {
		logger.Error(ctx, "fullSyncFromContainer: download failed",
			logger.AddField("runtime_id", runtimeID),
			logger.ErrorField(err),
		)
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
			logger.Error(ctx, "fullSyncFromContainer: read tar entry failed",
				logger.AddField("runtime_id", runtimeID),
				logger.ErrorField(err),
			)
			return fmt.Errorf("read tar entry: %w", err)
		}

		name := strings.TrimPrefix(hdr.Name, "workspace/")
		if name == "" {
			continue
		}

		if isExcluded(name, exclude) {
			continue
		}

		if hdr.Typeflag == tar.TypeDir {
			_ = scoped.MakeDir(ctx, strings.TrimRight(name, "/"), 0755)
			continue
		}

		writer, err := scoped.Create(ctx, name)
		if err != nil {
			logger.Error(ctx, "fullSyncFromContainer: create file failed",
				logger.AddField("runtime_id", runtimeID),
				logger.AddField("file", name),
				logger.ErrorField(err),
			)
			return fmt.Errorf("create %q: %w", name, err)
		}

		_, copyErr := io.Copy(writer, tr)
		closeErr := writer.Close()
		if copyErr != nil {
			logger.Error(ctx, "fullSyncFromContainer: write file failed",
				logger.AddField("runtime_id", runtimeID),
				logger.AddField("file", name),
				logger.ErrorField(copyErr),
			)
			return fmt.Errorf("write %q: %w", name, copyErr)
		}
		if closeErr != nil {
			logger.Error(ctx, "fullSyncFromContainer: flush file failed",
				logger.AddField("runtime_id", runtimeID),
				logger.AddField("file", name),
				logger.ErrorField(closeErr),
			)
			return fmt.Errorf("flush %q to storage: %w", name, closeErr)
		}
	}

	return nil
}

// downloadChangedFiles downloads the workspace tar and only extracts files in changedSet.
func (m *Manager) downloadChangedFiles(ctx context.Context, scoped storage.ScopedFS, runtimeID string, changedSet map[string]struct{}, exclude []string) error {
	tarReader, err := m.runtime.DownloadDir(ctx, runtimeID, "/workspace")
	if err != nil {
		logger.Error(ctx, "downloadChangedFiles: download failed",
			logger.AddField("runtime_id", runtimeID),
			logger.ErrorField(err),
		)
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
			logger.Error(ctx, "downloadChangedFiles: read tar entry failed",
				logger.AddField("runtime_id", runtimeID),
				logger.ErrorField(err),
			)
			return fmt.Errorf("read tar entry: %w", err)
		}

		name := strings.TrimPrefix(hdr.Name, "workspace/")
		if name == "" {
			continue
		}

		if isExcluded(name, exclude) {
			continue
		}

		if hdr.Typeflag == tar.TypeDir {
			_ = scoped.MakeDir(ctx, strings.TrimRight(name, "/"), 0755)
			continue
		}

		// Only write files that are in the changed set
		if _, changed := changedSet[name]; !changed {
			continue
		}

		writer, err := scoped.Create(ctx, name)
		if err != nil {
			logger.Error(ctx, "downloadChangedFiles: create file failed",
				logger.AddField("runtime_id", runtimeID),
				logger.AddField("file", name),
				logger.ErrorField(err),
			)
			return fmt.Errorf("create %q: %w", name, err)
		}

		_, copyErr := io.Copy(writer, tr)
		closeErr := writer.Close()
		if copyErr != nil {
			logger.Error(ctx, "downloadChangedFiles: write file failed",
				logger.AddField("runtime_id", runtimeID),
				logger.AddField("file", name),
				logger.ErrorField(copyErr),
			)
			return fmt.Errorf("write %q: %w", name, copyErr)
		}
		if closeErr != nil {
			logger.Error(ctx, "downloadChangedFiles: flush file failed",
				logger.AddField("runtime_id", runtimeID),
				logger.AddField("file", name),
				logger.ErrorField(closeErr),
			)
			return fmt.Errorf("flush %q to storage: %w", name, closeErr)
		}
	}

	return nil
}

// containerFileManifest runs `find` inside the container and returns a map of
// relative path → modification time (unix seconds).
// Directory entries have a trailing "/".
func (m *Manager) containerFileManifest(ctx context.Context, runtimeID string) (map[string]int64, error) {
	result, err := m.runtime.Exec(ctx, runtimeID, runtime.ExecRequest{
		Command: "find /workspace -not -path /workspace -printf '%P\\t%Y\\t%T@\\n'",
		WorkDir: "/workspace",
		Timeout: 30,
	})
	if err != nil {
		logger.Error(ctx, "containerFileManifest: exec find failed",
			logger.AddField("runtime_id", runtimeID),
			logger.ErrorField(err),
		)
		return nil, fmt.Errorf("exec find: %w", err)
	}

	logger.Debug(ctx, "containerFileManifest: find output",
		logger.AddField("runtime_id", runtimeID),
		logger.AddField("stdout_len", len(result.Stdout)),
		logger.AddField("exit_code", result.ExitCode),
	)

	manifest := make(map[string]int64)
	for _, line := range strings.Split(strings.TrimSpace(result.Stdout), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}

		path := parts[0]
		fileType := parts[1]

		// Parse modtime (float like "1712345678.123456")
		dotIdx := strings.Index(parts[2], ".")
		tsStr := parts[2]
		if dotIdx > 0 {
			tsStr = parts[2][:dotIdx]
		}
		modtime, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil {
			continue
		}

		if fileType == "d" {
			manifest[path+"/"] = modtime
		} else {
			manifest[path] = modtime
		}
	}

	return manifest, nil
}

// storageFileSet recursively lists all files in ScopedFS and returns their relative paths.
func (m *Manager) storageFileSet(ctx context.Context, scoped storage.ScopedFS, dir string) (map[string]struct{}, error) {
	result := make(map[string]struct{})
	if err := m.walkStorageFiles(ctx, scoped, dir, result); err != nil {
		return nil, err
	}
	return result, nil
}

// walkStorageFiles recursively walks ScopedFS directories, collecting file paths.
func (m *Manager) walkStorageFiles(ctx context.Context, scoped storage.ScopedFS, dir string, result map[string]struct{}) error {
	files, err := scoped.List(ctx, dir)
	if err != nil {
		return err
	}

	for _, fi := range files {
		baseName := filepath.Base(strings.TrimRight(fi.Name(), "/"))
		if baseName == "." || baseName == "" {
			continue
		}

		relPath := baseName
		if dir != "." {
			relPath = filepath.Join(dir, baseName)
		}

		if fi.IsDir() {
			if err := m.walkStorageFiles(ctx, scoped, relPath, result); err != nil {
				return err
			}
			continue
		}

		result[relPath] = struct{}{}
	}

	return nil
}
