package sandbox

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dysodeng/fs"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
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
	PoolConfig     PoolConfig
	DefaultTimeout int // seconds
}

// Manager orchestrates sandbox lifecycle: creation, execution, destruction.
type Manager struct {
	runtime    runtime.Runtime
	filesystem fs.FileSystem
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
func NewManager(rt runtime.Runtime, fsys fs.FileSystem, cfg ManagerConfig) *Manager {
	return &Manager{
		runtime:    rt,
		filesystem: fsys,
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
	m.cleanupOrphanedPoolContainers(ctx)
	m.pool.WarmUp(ctx)
	m.wg.Add(1)
	go m.reapExpiredSandboxes()
}

// Stop drains the pool and cleans up.
func (m *Manager) Stop(ctx context.Context) {
	close(m.stopCh)
	m.wg.Wait()
	m.pool.Drain(ctx)
}

// Create creates a new sandbox.
func (m *Manager) Create(ctx context.Context, cfg SandboxConfig) (*Sandbox, error) {
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = m.config.DefaultTimeout
	}

	m.mu.Lock()
	id := fmt.Sprintf("sandbox-%s", randSuffix(randSuffixLen))
	m.mu.Unlock()

	var info *runtime.SandboxInfo
	var err error

	if cfg.Network.Enabled {
		spec := m.buildSpec(id, cfg)
		info, err = m.runtime.CreateSandbox(ctx, spec)
		if err != nil {
			return nil, fmt.Errorf("create sandbox: %w", err)
		}
	} else {
		info, err = m.pool.Acquire(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire container: %w", err)
		}
	}

	// Rename container for easier identification (best-effort)
	_ = m.runtime.RenameSandbox(ctx, info.RuntimeID, id)

	sb := &Sandbox{
		ID:        id,
		Config:    cfg,
		State:     StateReady,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		RuntimeID: info.RuntimeID,
		Timeout:   time.Duration(timeout) * time.Second,
	}

	// Install dependencies if requested
	if len(cfg.Dependencies) > 0 {
		installCmd := buildInstallCommand(cfg.Dependencies)
		if installCmd != "" {
			if _, err := m.runtime.Exec(ctx, info.RuntimeID, runtime.ExecRequest{
				Command: installCmd,
				WorkDir: "/workspace",
				Timeout: 120, // 2 min for dependency install
			}); err != nil {
				// Cleanup on failure
				_ = m.runtime.RemoveSandbox(ctx, info.RuntimeID)
				return nil, fmt.Errorf("install dependencies: %w", err)
			}
		}
	}

	m.mu.Lock()
	m.sandboxes[id] = sb
	m.mu.Unlock()

	// Persist persistent sandboxes to session store
	if cfg.Mode == ModePersistent && m.sessions != nil {
		if err := m.sessions.Save(ctx, sb); err != nil {
			// Log but don't fail — in-memory state is still valid
			_ = err
		}
	}

	// Auto-mount workspace if specified
	if cfg.WorkspacePath != "" {
		if err := m.MountWorkspace(ctx, id, cfg.WorkspacePath); err != nil {
			_ = m.runtime.RemoveSandbox(ctx, info.RuntimeID)
			m.mu.Lock()
			delete(m.sandboxes, id)
			m.mu.Unlock()
			return nil, fmt.Errorf("mount workspace: %w", err)
		}
	}

	return sb, nil
}

// Get retrieves a sandbox by ID. Returns a copy to prevent external data races.
func (m *Manager) Get(ctx context.Context, id string) (Sandbox, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	if ok {
		cp := *sb
		m.mu.RUnlock()
		return cp, nil
	}
	m.mu.RUnlock()

	// Fall back to session store for persistent sandboxes
	if m.sessions != nil {
		sbPtr, err := m.sessions.Load(ctx, id)
		if err != nil {
			return Sandbox{}, err
		}
		// Register into in-memory map so subsequent lookups find it directly
		m.mu.Lock()
		m.sandboxes[id] = sbPtr
		m.mu.Unlock()
		return *sbPtr, nil
	}

	return Sandbox{}, fmt.Errorf("sandbox not found: %s", id)
}

// Destroy removes a sandbox.
func (m *Manager) Destroy(ctx context.Context, id string) error {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("sandbox not found: %s", id)
	}
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
		return fmt.Errorf("remove sandbox: %w", err)
	}

	// Notify pool so it can refill
	m.pool.NotifyRemoved()

	return nil
}

// Exec executes a command in a sandbox synchronously.
func (m *Manager) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
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

	return result, err
}

// ExecStream executes a command in a sandbox with streaming output.
func (m *Manager) ExecStream(ctx context.Context, id string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	m.mu.Lock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
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
		return nil, err
	}

	// Wrap channel to update state on completion
	outCh := make(chan runtime.StreamEvent, 64)
	go func() {
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
	}()

	return outCh, nil
}

// UploadFile uploads a file into a sandbox.
func (m *Manager) UploadFile(ctx context.Context, id string, destPath string, reader io.Reader) error {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.RUnlock()
		return fmt.Errorf("sandbox not found: %s", id)
	}
	m.mu.RUnlock()

	return m.runtime.UploadFile(ctx, sb.RuntimeID, destPath, reader)
}

// DownloadFile downloads a file from a sandbox.
func (m *Manager) DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
	m.mu.RUnlock()

	return m.runtime.DownloadFile(ctx, sb.RuntimeID, srcPath)
}

// ListFiles lists files in a sandbox directory.
func (m *Manager) ListFiles(ctx context.Context, id string, dirPath string) ([]runtime.FileInfo, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
	m.mu.RUnlock()

	return m.runtime.ListFiles(ctx, sb.RuntimeID, dirPath)
}

// UpdateNetwork dynamically updates network access for a running sandbox.
func (m *Manager) UpdateNetwork(ctx context.Context, id string, enabled bool, whitelist []string) error {
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

// cleanupOrphanedPoolContainers removes pool containers left over from a
// previous process that exited without graceful shutdown.
func (m *Manager) cleanupOrphanedPoolContainers(ctx context.Context) {
	containers, err := m.runtime.ListSandboxes(ctx, map[string]string{
		"sandbox.pool": "true",
	})
	if err != nil {
		log.Printf("failed to list orphaned pool containers: %v", err)
		return
	}
	for _, c := range containers {
		log.Printf("removing orphaned pool container %s", c.RuntimeID)
		_ = m.runtime.RemoveSandbox(ctx, c.RuntimeID)
	}
	if len(containers) > 0 {
		log.Printf("cleaned up %d orphaned pool container(s)", len(containers))
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
			log.Printf("WARNING: skipping dependency with invalid name: %q", d.Name)
			continue
		}
		if d.Version != "" && !validDepRegexp.MatchString(d.Version) {
			log.Printf("WARNING: skipping dependency %q with invalid version: %q", d.Name, d.Version)
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
			log.Printf("WARNING: skipping dependency %q with unknown manager: %q", d.Name, d.Manager)
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
