package sandbox

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
)

// ManagerConfig configures the SandboxManager.
type ManagerConfig struct {
	PoolConfigs    map[Language]PoolConfig
	DefaultTimeout int // seconds
}

// Manager orchestrates sandbox lifecycle: creation, execution, destruction.
type Manager struct {
	runtime  runtime.Runtime
	config   ManagerConfig
	sessions *SessionStore // optional, for persistent sandboxes

	pools     map[Language]*Pool
	sandboxes map[string]*Sandbox
	mu        sync.RWMutex
	counter   int
}

// NewManager creates a new SandboxManager.
func NewManager(rt runtime.Runtime, cfg ManagerConfig) *Manager {
	pools := make(map[Language]*Pool)
	for lang, pcfg := range cfg.PoolConfigs {
		pools[lang] = NewPool(rt, pcfg)
	}

	return &Manager{
		runtime:   rt,
		config:    cfg,
		pools:     pools,
		sandboxes: make(map[string]*Sandbox),
	}
}

// SetSessionStore sets an optional SessionStore for persistent sandbox state.
func (m *Manager) SetSessionStore(ss *SessionStore) {
	m.sessions = ss
}

// Start initializes the manager and warms up pools.
func (m *Manager) Start(ctx context.Context) {
	for _, pool := range m.pools {
		pool.WarmUp(ctx)
	}
}

// Stop drains all pools and cleans up.
func (m *Manager) Stop(ctx context.Context) {
	for _, pool := range m.pools {
		pool.Drain(ctx)
	}
}

// Create creates a new sandbox.
func (m *Manager) Create(ctx context.Context, cfg SandboxConfig) (*Sandbox, error) {
	pool, ok := m.pools[cfg.Language]
	if !ok {
		return nil, fmt.Errorf("unsupported language: %s", cfg.Language)
	}

	// Acquire a warm container from the pool
	info, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire container: %w", err)
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = m.config.DefaultTimeout
	}

	m.mu.Lock()
	m.counter++
	id := fmt.Sprintf("sb-%d-%d", time.Now().Unix(), m.counter)
	m.mu.Unlock()

	sb := &Sandbox{
		ID:        id,
		Config:    cfg,
		State:     StateReady,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		RuntimeID: info.RuntimeID,
	}

	// Install dependencies if requested
	if len(cfg.Dependencies) > 0 {
		installCmd := buildInstallCommand(cfg.Language, cfg.Dependencies)
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

	return sb, nil
}

// Get retrieves a sandbox by ID.
func (m *Manager) Get(ctx context.Context, id string) (*Sandbox, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	m.mu.RUnlock()
	if ok {
		return sb, nil
	}

	// Fall back to session store for persistent sandboxes
	if m.sessions != nil {
		return m.sessions.Load(ctx, id)
	}

	return nil, fmt.Errorf("sandbox not found: %s", id)
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

	// Remove the container
	if err := m.runtime.RemoveSandbox(ctx, sb.RuntimeID); err != nil {
		return fmt.Errorf("remove sandbox: %w", err)
	}

	return nil
}

// Exec executes a command in a sandbox synchronously.
func (m *Manager) Exec(ctx context.Context, id string, req runtime.ExecRequest) (*runtime.ExecResult, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
	m.mu.RUnlock()

	m.mu.Lock()
	sb.State = StateRunning
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	result, err := m.runtime.Exec(ctx, sb.RuntimeID, req)

	m.mu.Lock()
	sb.State = StateIdle
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	return result, err
}

// ExecStream executes a command in a sandbox with streaming output.
func (m *Manager) ExecStream(ctx context.Context, id string, req runtime.ExecRequest) (<-chan runtime.StreamEvent, error) {
	m.mu.RLock()
	sb, ok := m.sandboxes[id]
	if !ok {
		m.mu.RUnlock()
		return nil, fmt.Errorf("sandbox not found: %s", id)
	}
	m.mu.RUnlock()

	m.mu.Lock()
	sb.State = StateRunning
	sb.UpdatedAt = time.Now()
	m.mu.Unlock()

	ch, err := m.runtime.ExecStream(ctx, sb.RuntimeID, req)
	if err != nil {
		m.mu.Lock()
		sb.State = StateIdle
		m.mu.Unlock()
		return nil, err
	}

	// Wrap channel to update state on completion
	outCh := make(chan runtime.StreamEvent, 64)
	go func() {
		defer close(outCh)
		for event := range ch {
			outCh <- event
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

// buildInstallCommand generates the shell command to install dependencies
// based on the sandbox language.
func buildInstallCommand(lang Language, deps []Dependency) string {
	if len(deps) == 0 {
		return ""
	}
	var pkgs []string
	for _, d := range deps {
		if d.Version != "" {
			switch lang {
			case LangPython:
				pkgs = append(pkgs, d.Name+"=="+d.Version)
			case LangNodeJS:
				pkgs = append(pkgs, d.Name+"@"+d.Version)
			default:
				pkgs = append(pkgs, d.Name)
			}
		} else {
			pkgs = append(pkgs, d.Name)
		}
	}
	switch lang {
	case LangPython:
		return "pip install --no-cache-dir " + strings.Join(pkgs, " ")
	case LangNodeJS:
		return "npm install --no-save " + strings.Join(pkgs, " ")
	default:
		return ""
	}
}
