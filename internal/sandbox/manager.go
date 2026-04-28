package sandbox

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/goairix/fs"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
	telemetry "github.com/goairix/sandbox/internal/telemetry/trace"
)

// validDepRegexp validates dependency names and versions to prevent command injection.
var validDepRegexp = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

const randSuffixLen = 10

// randSuffix generates a random lowercase alphanumeric string of length n.
func randSuffix(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	for i := range b {
		b[i] = alphabet[b[i]%byte(len(alphabet))]
	}
	return string(b)
}

// ManagerConfig configures the SandboxManager.
type ManagerConfig struct {
	PoolConfig              PoolConfig
	DefaultTimeout          int // seconds
	AutoSyncIntervalSeconds int // 0 = disabled
}

// Manager orchestrates sandbox lifecycle: creation, execution, destruction.
type Manager struct {
	runtime    runtime.Runtime
	filesystem fs.FileSystem
	fsMeta     *storage.FileSystemMeta
	config     ManagerConfig
	sessions   *SessionStore // optional, for persistent sandboxes

	pool       *Pool
	sandboxes  map[string]*Sandbox
	workspaces map[string]storage.ScopedFS // sandbox ID -> ScopedFS
	mu         sync.RWMutex

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewManager creates a new SandboxManager.
func NewManager(rt runtime.Runtime, fsys fs.FileSystem, fsMeta *storage.FileSystemMeta, cfg ManagerConfig) *Manager {
	return &Manager{
		runtime:    rt,
		filesystem: fsys,
		fsMeta:     fsMeta,
		config:     cfg,
		pool:       NewPool(rt, cfg.PoolConfig),
		sandboxes:  make(map[string]*Sandbox),
		workspaces: make(map[string]storage.ScopedFS),
		stopCh:     make(chan struct{}),
	}
}

// SetSessionStore sets an optional SessionStore for persistent sandbox state.
func (m *Manager) SetSessionStore(ss *SessionStore) {
	m.sessions = ss
}

// Start initializes the manager and warms up the pool.
// It first removes any orphaned pool containers left over from a previous
// process that exited without cleanup (e.g. crash, SIGKILL).
func (m *Manager) Start(ctx context.Context) {
	spanCtx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.Start")
	defer span.End()

	if m.runtime.IsStateful() {
		// Stateful runtimes (e.g. Kubernetes): pods survive process restarts, so
		// we must restore persistent sandboxes synchronously first. This registers
		// their RuntimeIDs in m.sandboxes before cleanupOrphanedPoolContainers
		// runs, preventing live pods from being mistakenly deleted as orphans.
		m.restorePersistentSandboxes(spanCtx)
		m.cleanupOrphanedPoolContainers(spanCtx)
		m.pool.WarmUp(spanCtx)
	} else {
		// Non-stateful runtimes (e.g. Docker): containers are gone after restart,
		// so cleanup first, then warm up, then restore in background to avoid
		// blocking API startup.
		m.cleanupOrphanedPoolContainers(spanCtx)
		m.pool.WarmUp(spanCtx)
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			m.restorePersistentSandboxes(spanCtx)
		}()
	}

	m.wg.Add(1)
	go m.reapExpiredSandboxes()

	if m.config.AutoSyncIntervalSeconds > 0 {
		m.wg.Add(1)
		go m.autoSyncWorkspaces()
	}
}

// Stop drains the pool and cleans up.
func (m *Manager) Stop(ctx context.Context) {
	spanCtx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.Stop")
	defer span.End()

	close(m.stopCh)
	m.wg.Wait()
	m.pool.Drain(spanCtx)
}

// Create creates a new sandbox.
func (m *Manager) Create(ctx context.Context, cfg SandboxConfig) (*Sandbox, error) {
	spanCtx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.Create")
	defer span.End()

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = m.config.DefaultTimeout
	}

	var timeoutDuration time.Duration
	if timeout < 0 {
		// -1 means never expire
		timeoutDuration = -1
	} else {
		timeoutDuration = time.Duration(timeout) * time.Second
	}

	m.mu.Lock()
	id := fmt.Sprintf("sandbox-%s", randSuffix(randSuffixLen))
	m.mu.Unlock()

	var info *runtime.SandboxInfo
	var err error
	var bindMounted bool

	useBindMount := cfg.WorkspacePath != "" && m.fsMeta != nil && m.fsMeta.Provider == storage.ProviderLocal

	if cfg.Network.Enabled || useBindMount {
		spec := m.buildSpec(id, cfg)
		if useBindMount {
			hostPath := m.resolveLocalWorkspacePath(cfg.WorkspacePath)
			spec.Mounts = append(spec.Mounts, runtime.Mount{
				HostPath:      hostPath,
				ContainerPath: "/workspace",
			})
			bindMounted = true
		}
		info, err = m.runtime.CreateSandbox(spanCtx, spec)
		if err != nil {
			telemetry.Error(err, span)
			return nil, fmt.Errorf("create sandbox: %w", err)
		}
	} else {
		info, err = m.pool.Acquire(spanCtx)
		if err != nil {
			telemetry.Error(err, span)
			return nil, fmt.Errorf("acquire container: %w", err)
		}
		// Remove pool label and update sandbox.id so the pod is correctly
		// identified after being taken from the pool (effective on K8s).
		nilVal := (*string)(nil)
		_ = m.runtime.UpdateLabels(spanCtx, info.RuntimeID, map[string]*string{
			"sandbox.pool": nilVal,
			"sandbox.id":   &id,
		})
	}

	// Rename container for easier identification (best-effort)
	_ = m.runtime.RenameSandbox(spanCtx, info.RuntimeID, id)

	sb := &Sandbox{
		ID:        id,
		Config:    cfg,
		State:     StateReady,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		RuntimeID: info.RuntimeID,
		Timeout:   timeoutDuration,
	}

	// Install dependencies if requested
	if len(cfg.Dependencies) > 0 {
		installCmd := buildInstallCommand(cfg.Dependencies)
		if installCmd != "" {
			if _, err := m.runtime.Exec(spanCtx, info.RuntimeID, runtime.ExecRequest{
				Command: installCmd,
				WorkDir: "/workspace",
				Timeout: 120, // 2 min for dependency install
			}); err != nil {
				// Cleanup on failure
				_ = m.runtime.RemoveSandbox(spanCtx, info.RuntimeID)
				return nil, fmt.Errorf("install dependencies: %w", err)
			}
		}
	}

	m.mu.Lock()
	m.sandboxes[id] = sb
	m.mu.Unlock()

	// Persist persistent sandboxes to session store
	if cfg.Mode == ModePersistent && m.sessions != nil {
		if err := m.sessions.Save(spanCtx, sb); err != nil {
			// Log but don't fail — in-memory state is still valid
			_ = err
		}
	}

	// Auto-mount workspace if specified
	if cfg.WorkspacePath != "" {
		if bindMounted {
			// Bind mount: just register the scoped FS, no file copy needed.
			if err := m.registerWorkspace(spanCtx, id, cfg.WorkspacePath); err != nil {
				_ = m.runtime.RemoveSandbox(spanCtx, info.RuntimeID)
				m.mu.Lock()
				delete(m.sandboxes, id)
				m.mu.Unlock()
				return nil, fmt.Errorf("register workspace: %w", err)
			}
		} else {
			if err := m.MountWorkspace(spanCtx, id, cfg.WorkspacePath, cfg.WorkspaceSyncExclude); err != nil {
				_ = m.runtime.RemoveSandbox(spanCtx, info.RuntimeID)
				m.mu.Lock()
				delete(m.sandboxes, id)
				m.mu.Unlock()
				return nil, fmt.Errorf("mount workspace: %w", err)
			}
		}
	}

	span.SetAttributes(attribute.String("sandbox.id", id))
	metrics.SandboxActiveGauge.Add(spanCtx, 1)
	return sb, nil
}

// Get retrieves a sandbox by ID. Returns a copy to prevent external data races.
func (m *Manager) Get(ctx context.Context, id string) (Sandbox, error) {
	spanCtx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.Get")
	defer span.End()

	sb, err := m.resolve(spanCtx, id)
	if err != nil {
		return Sandbox{}, err
	}
	return *sb, nil
}

// resolve looks up a sandbox by ID: first in memory, then in the session store.
// Returns a pointer to the in-memory Sandbox (caller must hold no lock).
func (m *Manager) resolve(ctx context.Context, id string) (*Sandbox, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	m.mu.RUnlock()
	if ok {
		return sb, nil
	}

	// Fall back to session store for persistent sandboxes
	if m.sessions != nil {
		sbPtr, err := m.sessions.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		logger.Info(ctx, "sandbox restored from session store",
			logger.AddField("sandbox_id", id),
			logger.AddField("runtime_id", sbPtr.RuntimeID),
		)
		// Register into in-memory map so subsequent lookups find it directly
		m.mu.Lock()
		m.sandboxes[id] = sbPtr
		m.mu.Unlock()

		// Restore workspace ScopedFS if workspace was mounted
		if sbPtr.Workspace != nil && sbPtr.Workspace.RootPath != "" {
			scoped, fsErr := storage.NewScopedFS(m.filesystem, sbPtr.Workspace.RootPath)
			if fsErr == nil {
				m.mu.Lock()
				m.workspaces[id] = scoped
				m.mu.Unlock()
			}
		}

		return sbPtr, nil
	}

	return nil, fmt.Errorf("sandbox not found: %s", id)
}

// Destroy removes a sandbox.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Destroy",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)
	defer span.End()

	sb, err := m.resolve(ctx, id)
	if err != nil {
		telemetry.Error(err, span)
		return err
	}

	m.mu.Lock()
	sb.State = StateDestroying
	delete(m.sandboxes, id)
	m.mu.Unlock()

	// Clean up session store
	if m.sessions != nil {
		_ = m.sessions.Remove(ctx, id)
	}

	// Best-effort sync workspace back before destroying
	m.mu.RLock()
	_, hasWS := m.workspaces[id]
	m.mu.RUnlock()
	if hasWS {
		_ = m.syncFromContainer(ctx, id, sb.RuntimeID, nil)
		m.mu.Lock()
		delete(m.workspaces, id)
		m.mu.Unlock()
	}

	// Remove the container
	if err := m.runtime.RemoveSandbox(ctx, sb.RuntimeID); err != nil {
		telemetry.Error(err, span)
		return fmt.Errorf("remove sandbox: %w", err)
	}

	// Notify pool so it can refill
	m.pool.NotifyRemoved()
	metrics.SandboxActiveGauge.Add(ctx, -1)

	return nil
}

// Exec executes a command in a sandbox synchronously.
func (m *Manager) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.Exec",
		trace.WithAttributes(
			attribute.String("sandbox.id", id),
			attribute.String("exec.language", req.Command[:min(len(req.Command), 20)]),
		),
	)
	defer span.End()

	sb, err := m.resolve(ctx, id)
	if err != nil {
		telemetry.Error(err, span)
		return nil, err
	}

	if req.RequiresNetwork && !sb.Config.Network.Enabled {
		telemetry.Error(ErrNetworkRequired, span)
		return nil, ErrNetworkRequired
	}

	m.mu.Lock()
	sb.State = StateRunning
	sb.UpdatedAt = time.Now()
	runtimeID := sb.RuntimeID
	m.mu.Unlock()

	result, err := m.runtime.Exec(ctx, runtimeID, req)

	m.mu.Lock()
	if err != nil {
		sb.State = StateError
	} else {
		sb.State = StateIdle
	}
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	if err != nil {
		telemetry.Error(err, span)
		return nil, err
	}

	metrics.SandboxExecTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("sandbox.id", id),
		attribute.Int("exec.exit_code", result.ExitCode),
	))
	span.SetAttributes(
		attribute.Int("exec.exit_code", result.ExitCode),
		attribute.Float64("exec.duration_s", result.Duration.Seconds()),
	)
	return result, nil
}

// ExecStream executes a command in a sandbox with streaming output.
func (m *Manager) ExecStream(ctx context.Context, id string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.ExecStream",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)

	sb, err := m.resolve(ctx, id)
	if err != nil {
		telemetry.Error(err, span)
		span.End()
		return nil, err
	}

	if req.RequiresNetwork && !sb.Config.Network.Enabled {
		telemetry.Error(ErrNetworkRequired, span)
		span.End()
		return nil, ErrNetworkRequired
	}

	m.mu.Lock()
	sb.State = StateRunning
	sb.UpdatedAt = time.Now()
	runtimeID := sb.RuntimeID
	m.mu.Unlock()

	ch, err := m.runtime.ExecStream(ctx, runtimeID, req)
	if err != nil {
		m.mu.Lock()
		sb.State = StateError
		sb.UpdatedAt = time.Now()
		m.mu.Unlock()
		telemetry.Error(err, span)
		span.End()
		return nil, err
	}

	// Wrap channel to update state on completion
	outCh := make(chan runtime.StreamEvent, 64)
	go func() {
		defer span.End()
		defer close(outCh)
		for event := range ch {
			select {
			case outCh <- event:
			case <-ctx.Done():
				// Drain remaining events to avoid blocking the upstream goroutine
				for range ch {
				}
				return
			}
		}
		m.mu.Lock()
		sb.State = StateIdle
		sb.UpdatedAt = time.Now()
		m.mu.Unlock()
		metrics.SandboxExecTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("sandbox.id", id),
			attribute.String("exec.type", "stream"),
		))
	}()

	return outCh, nil
}

// UploadFile uploads a file into a sandbox.
func (m *Manager) UploadFile(ctx context.Context, id string, destPath string, reader io.Reader) error {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.UploadFile",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)
	defer span.End()

	sb, err := m.resolve(ctx, id)
	if err != nil {
		return err
	}

	return m.runtime.UploadFile(ctx, sb.RuntimeID, destPath, reader)
}

// DownloadFile downloads a file from a sandbox.
func (m *Manager) DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.DownloadFile",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
	)
	defer span.End()

	sb, err := m.resolve(ctx, id)
	if err != nil {
		return nil, err
	}

	return m.runtime.DownloadFile(ctx, sb.RuntimeID, srcPath)
}

// ListFiles lists files in a sandbox directory.
func (m *Manager) ListFiles(ctx context.Context, id string, dirPath string) ([]runtime.FileInfo, error) {
	sb, err := m.resolve(ctx, id)
	if err != nil {
		return nil, err
	}

	return m.runtime.ListFiles(ctx, sb.RuntimeID, dirPath)
}

// ListFilesRecursive lists files recursively in a sandbox directory.
func (m *Manager) ListFilesRecursive(ctx context.Context, id string, dirPath string, maxDepth int, page int, pageSize int) (*runtime.FileListResult, error) {
	sb, err := m.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.runtime.ListFilesRecursive(ctx, sb.RuntimeID, dirPath, maxDepth, page, pageSize)
}

// ReadFileLines reads a range of lines from a file in a sandbox.
func (m *Manager) ReadFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int) (*runtime.FileLineResult, error) {
	sb, err := m.resolve(ctx, id)
	if err != nil {
		return nil, err
	}
	return m.runtime.ReadFileLines(ctx, sb.RuntimeID, filePath, startLine, endLine)
}

// EditFile performs a string replacement in a file in a sandbox.
func (m *Manager) EditFile(ctx context.Context, id string, filePath string, oldStr string, newStr string, replaceAll bool) error {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.EditFile",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
		trace.WithAttributes(attribute.String("file_path", filePath)),
	)
	defer span.End()

	sb, err := m.resolve(ctx, id)
	if err != nil {
		return err
	}
	return m.runtime.EditFile(ctx, sb.RuntimeID, filePath, oldStr, newStr, replaceAll)
}

// EditFileLines replaces a range of lines in a file in a sandbox.
func (m *Manager) EditFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int, newContent string) error {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.EditFileLines",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
		trace.WithAttributes(attribute.String("file_path", filePath)),
	)
	defer span.End()

	sb, err := m.resolve(ctx, id)
	if err != nil {
		return err
	}
	return m.runtime.EditFileLines(ctx, sb.RuntimeID, filePath, startLine, endLine, newContent)
}

// UpdateNetwork dynamically updates network access for a running sandbox.
func (m *Manager) UpdateNetwork(ctx context.Context, id string, enabled bool, whitelist []string) error {
	ctx, span := telemetry.Tracer().Start(ctx, "sandbox.Manager.UpdateNetwork",
		trace.WithAttributes(attribute.String("sandbox.id", id)),
		trace.WithAttributes(attribute.String("white_list", strings.Join(whitelist, ","))),
	)
	defer span.End()

	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox not found: %s", id)
	}
	runtimeID := sb.RuntimeID
	m.mu.Unlock()

	if err := m.runtime.UpdateNetwork(ctx, runtimeID, enabled, whitelist); err != nil {
		return err
	}

	m.mu.Lock()
	sb.Config.Network.Enabled = enabled
	sb.Config.Network.Whitelist = whitelist
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	return nil
}

// UpdateTTL dynamically updates the TTL for a running sandbox.
// The new timeout (in seconds) must be > 0; setting to never-expire (-1) after
// creation is not allowed.
func (m *Manager) UpdateTTL(ctx context.Context, id string, timeoutSeconds int) (*Sandbox, error) {
	if timeoutSeconds <= 0 {
		return nil, fmt.Errorf("timeout must be greater than 0")
	}

	sb, err := m.resolve(ctx, id)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	newTimeout := time.Duration(timeoutSeconds) * time.Second

	m.mu.Lock()
	sb.Timeout = newTimeout
	sb.CreatedAt = now // reset so reaper uses new baseline
	sb.UpdatedAt = now
	m.mu.Unlock()

	// Persist to session store if applicable
	if sb.Config.Mode == ModePersistent && m.sessions != nil {
		if err := m.sessions.Save(ctx, sb); err != nil {
			_ = err
		}
	}

	return sb, nil
}

// reapExpiredSandboxes periodically checks for sandboxes that have exceeded
// their timeout and destroys them.
func (m *Manager) reapExpiredSandboxes() {
	defer m.wg.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.reapOnce()
		}
	}
}

func (m *Manager) reapOnce() {
	now := time.Now()
	var expired []string

	m.mu.RLock()
	for id, sb := range m.sandboxes {
		if sb.Timeout > 0 && now.Sub(sb.CreatedAt) > sb.Timeout {
			expired = append(expired, id)
		}
	}
	m.mu.RUnlock()

	for _, id := range expired {
		_ = m.Destroy(context.Background(), id)
	}
}

// autoSyncWorkspaces periodically syncs changed files from all mounted
// workspaces back to storage. This reduces data loss if a container crashes
// between manual syncs.
func (m *Manager) autoSyncWorkspaces() {
	defer m.wg.Done()
	interval := time.Duration(m.config.AutoSyncIntervalSeconds) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.autoSyncOnce()
		}
	}
}

func (m *Manager) autoSyncOnce() {
	m.mu.RLock()
	type syncTarget struct {
		sandboxID   string
		runtimeID   string
		syncExclude []string
	}
	var targets []syncTarget
	for id := range m.workspaces {
		if sb, ok := m.sandboxes[id]; ok {
			var exclude []string
			if sb.Workspace != nil {
				exclude = sb.Workspace.SyncExclude
			}
			targets = append(targets, syncTarget{sandboxID: id, runtimeID: sb.RuntimeID, syncExclude: exclude})
		}
	}
	m.mu.RUnlock()

	for _, t := range targets {
		if err := m.syncFromContainer(context.Background(), t.sandboxID, t.runtimeID, t.syncExclude); err != nil {
			logger.Error(context.Background(), "auto-sync failed",
				logger.AddField("sandbox_id", t.sandboxID),
				logger.ErrorField(err),
			)
		}
	}
}

// restorePersistentSandboxes reloads all persistent sandboxes from the session
// store at startup. If a container is gone, it recreates the container and
// re-syncs the workspace.
func (m *Manager) restorePersistentSandboxes(ctx context.Context) {
	if m.sessions == nil {
		return
	}

	ids, err := m.sessions.List(ctx)
	if err != nil {
		logger.Error(ctx, "failed to list persistent sandboxes", logger.ErrorField(err))
		return
	}

	var restored, recreated, failed int
	for _, id := range ids {
		sbPtr, err := m.sessions.Load(ctx, id)
		if err != nil {
			logger.Error(ctx, "failed to load sandbox from session store",
				logger.AddField("sandbox_id", id),
				logger.ErrorField(err),
			)
			failed++
			continue
		}

		// Check if the container still exists and is running
		existingInfo, rtErr := m.runtime.GetSandbox(ctx, sbPtr.RuntimeID)
		if rtErr != nil || existingInfo == nil || existingInfo.State != "running" {
			// Container gone or not healthy — recreate it
			logger.Warn(ctx, "sandbox container gone, recreating",
				logger.AddField("sandbox_id", id),
				logger.AddField("runtime_id", sbPtr.RuntimeID),
			)
			newInfo, recreateErr := m.recreateSandbox(ctx, sbPtr)
			if recreateErr != nil {
				logger.Error(ctx, "failed to recreate sandbox",
					logger.AddField("sandbox_id", id),
					logger.ErrorField(recreateErr),
				)
				_ = m.sessions.Remove(ctx, id)
				failed++
				continue
			}
			sbPtr.RuntimeID = newInfo.RuntimeID
			sbPtr.State = StateReady
			sbPtr.UpdatedAt = time.Now()
			_ = m.sessions.Save(ctx, sbPtr)
			recreated++
		}

		// Register into in-memory map
		m.sandboxes[id] = sbPtr

		// Restore workspace ScopedFS and sync if needed
		if sbPtr.Workspace != nil && sbPtr.Workspace.RootPath != "" {
			scoped, fsErr := storage.NewScopedFS(m.filesystem, sbPtr.Workspace.RootPath)
			if fsErr == nil {
				m.workspaces[id] = scoped
				// Re-sync files to the new container (skip for bind mount)
				if !sbPtr.Workspace.BindMounted {
					if syncErr := m.syncToContainer(ctx, scoped, sbPtr.RuntimeID); syncErr != nil {
						logger.Error(ctx, "workspace re-sync failed",
							logger.AddField("sandbox_id", id),
							logger.AddField("runtime_id", sbPtr.RuntimeID),
							logger.ErrorField(syncErr),
						)
					}
				}
			} else {
				logger.Error(ctx, "failed to restore workspace scoped fs",
					logger.AddField("sandbox_id", id),
					logger.ErrorField(fsErr),
				)
			}
		}

		restored++
	}

	if restored > 0 || failed > 0 {
		logger.Info(ctx, "persistent sandboxes restore complete",
			logger.AddField("restored", restored),
			logger.AddField("recreated", recreated),
			logger.AddField("failed", failed),
		)
	}
}

// recreateSandbox creates a new container for a persistent sandbox whose
// container was lost (e.g. docker-compose restart).
func (m *Manager) recreateSandbox(ctx context.Context, sb *Sandbox) (*runtime.SandboxInfo, error) {
	// Use the original RuntimeID as the pod/container name so the identity is
	// consistent across restarts. On Docker, RenameSandbox will rename it to
	// sb.ID afterwards; on Kubernetes, pod names are immutable so RuntimeID is
	// the permanent name.
	spec := m.buildSpec(sb.RuntimeID, sb.Config)
	spec.Labels["sandbox.id"] = sb.ID

	// Restore bind mount if applicable
	if sb.Workspace != nil && sb.Workspace.BindMounted && m.fsMeta != nil && m.fsMeta.Provider == storage.ProviderLocal {
		hostPath := m.resolveLocalWorkspacePath(sb.Workspace.RootPath)
		spec.Mounts = append(spec.Mounts, runtime.Mount{
			HostPath:      hostPath,
			ContainerPath: "/workspace",
		})
	}

	info, err := m.runtime.CreateSandbox(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("create container: %w", err)
	}

	// Rename for identification
	_ = m.runtime.RenameSandbox(ctx, info.RuntimeID, sb.ID)

	return info, nil
}

// cleanupOrphanedPoolContainers removes pool containers left over from a
// previous process that exited without graceful shutdown.
// Must be called AFTER restorePersistentSandboxes so that active containers
// are already registered in m.sandboxes.
func (m *Manager) cleanupOrphanedPoolContainers(ctx context.Context) {
	containers, err := m.runtime.ListSandboxes(ctx, map[string]string{
		"sandbox.pool": "true",
	})
	if err != nil {
		logger.Error(ctx, "failed to list orphaned pool containers", logger.ErrorField(err))
		return
	}

	// Build set of runtime IDs belonging to restored persistent sandboxes
	activeRuntimeIDs := make(map[string]struct{})
	m.mu.RLock()
	for _, sb := range m.sandboxes {
		activeRuntimeIDs[sb.RuntimeID] = struct{}{}
	}
	m.mu.RUnlock()

	var removed int
	for _, c := range containers {
		if _, active := activeRuntimeIDs[c.RuntimeID]; active {
			continue // in use by a persistent sandbox
		}
		logger.Info(ctx, "removing orphaned pool container",
			logger.AddField("runtime_id", c.RuntimeID),
		)
		_ = m.runtime.RemoveSandbox(ctx, c.RuntimeID)
		removed++
	}
	if removed > 0 {
		logger.Info(ctx, "cleaned up orphaned pool containers",
			logger.AddField("count", removed),
		)
	}
}

// buildInstallCommand generates the shell command to install dependencies,
// grouping by package manager (pip/npm).
func buildInstallCommand(deps []Dependency) string {
	if len(deps) == 0 {
		return ""
	}
	var pipPkgs, npmPkgs []string
	for _, d := range deps {
		if !validDepRegexp.MatchString(d.Name) {
			logger.Warn(context.Background(), "skipping dependency with invalid name",
				logger.AddField("dep_name", d.Name),
			)
			continue
		}
		if d.Version != "" && !validDepRegexp.MatchString(d.Version) {
			logger.Warn(context.Background(), "skipping dependency with invalid version",
				logger.AddField("dep_name", d.Name),
				logger.AddField("dep_version", d.Version),
			)
			continue
		}
		pkg := d.Name
		switch d.Manager {
		case "pip":
			if d.Version != "" {
				pkg += "==" + d.Version
			}
			pipPkgs = append(pipPkgs, pkg)
		case "npm":
			if d.Version != "" {
				pkg += "@" + d.Version
			}
			npmPkgs = append(npmPkgs, pkg)
		default:
			logger.Warn(context.Background(), "skipping dependency with unknown manager",
				logger.AddField("dep_name", d.Name),
				logger.AddField("manager", d.Manager),
			)
		}
	}
	var cmds []string
	if len(pipPkgs) > 0 {
		cmds = append(cmds, "pip install --no-cache-dir "+strings.Join(pipPkgs, " "))
	}
	if len(npmPkgs) > 0 {
		cmds = append(cmds, "npm install --no-save "+strings.Join(npmPkgs, " "))
	}
	return strings.Join(cmds, " && ")
}

// buildSpec constructs a runtime.SandboxSpec from sandbox config.
// Used for network-enabled sandboxes that bypass the pool.
func (m *Manager) buildSpec(id string, cfg SandboxConfig) runtime.SandboxSpec {
	return runtime.SandboxSpec{
		ID:               id,
		Image:            m.config.PoolConfig.Image,
		Memory:           cfg.Resources.Memory,
		CPU:              cfg.Resources.CPU,
		Disk:             cfg.Resources.Disk,
		NetworkEnabled:   cfg.Network.Enabled,
		NetworkWhitelist: cfg.Network.Whitelist,
		ReadOnlyRootFS:   false,
		RunAsUser:        1000,
		PidLimit:         100,
		Labels: map[string]string{
			"sandbox.id": id,
		},
	}
}

// resolveLocalWorkspacePath returns the absolute host path for a workspace.
func (m *Manager) resolveLocalWorkspacePath(workspacePath string) string {
	if m.fsMeta == nil {
		return workspacePath
	}
	return filepath.Join(m.fsMeta.LocalPath, workspacePath)
}

// registerWorkspace registers a ScopedFS for a bind-mounted workspace without
// copying files (they are already visible via the mount).
func (m *Manager) registerWorkspace(ctx context.Context, sandboxID, rootPath string) error {
	scoped, err := storage.NewScopedFS(m.filesystem, rootPath)
	if err != nil {
		return fmt.Errorf("create scoped filesystem: %w", err)
	}

	now := time.Now()
	m.mu.Lock()
	sb := m.sandboxes[sandboxID]
	m.workspaces[sandboxID] = scoped
	sb.Workspace = &WorkspaceInfo{
		RootPath:     rootPath,
		MountedAt:    now,
		LastSyncedAt: now,
		BindMounted:  true,
	}
	sb.UpdatedAt = now
	m.mu.Unlock()

	if m.sessions != nil {
		_ = m.sessions.Save(ctx, sb)
	}
	return nil
}
