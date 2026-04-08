package sandbox

import "time"

// Language represents a supported programming language/runtime.
type Language string

const (
	LangPython Language = "python"
	LangNodeJS Language = "nodejs"
	LangBash   Language = "bash"
)

// Mode represents the sandbox lifecycle mode.
type Mode string

const (
	ModeEphemeral  Mode = "ephemeral"
	ModePersistent Mode = "persistent"
)

// State represents the current state of a sandbox.
type State string

const (
	StateCreating   State = "creating"
	StateReady      State = "ready"
	StateRunning    State = "running"
	StateIdle       State = "idle"
	StateDestroying State = "destroying"
	StateDestroyed  State = "destroyed"
	StateError      State = "error"
)

// Dependency represents an extra package to install at sandbox startup.
type Dependency struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ResourceLimits defines resource constraints for a sandbox.
type ResourceLimits struct {
	Memory string `json:"memory"` // e.g. "256Mi"
	CPU    string `json:"cpu"`    // e.g. "0.5"
	Disk   string `json:"disk"`   // e.g. "100Mi"
}

// NetworkConfig defines network settings for a sandbox.
type NetworkConfig struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist"` // allowed domains
}

// WorkspaceInfo holds metadata about a mounted workspace.
type WorkspaceInfo struct {
	RootPath     string    `json:"root_path"`
	MountedAt    time.Time `json:"mounted_at"`
	LastSyncedAt time.Time `json:"last_synced_at,omitempty"`
}

// SandboxConfig holds all configuration for creating a sandbox.
type SandboxConfig struct {
	Language      Language       `json:"language"`
	Mode          Mode           `json:"mode"`
	Timeout       int            `json:"timeout"` // seconds, max sandbox lifetime
	Resources     ResourceLimits `json:"resources"`
	Network       NetworkConfig  `json:"network"`
	Dependencies  []Dependency   `json:"dependencies"`
	WorkspacePath string         `json:"workspace_path,omitempty"`
}

// Sandbox represents a running sandbox instance.
type Sandbox struct {
	ID        string        `json:"id"`
	Config    SandboxConfig `json:"config"`
	State     State         `json:"state"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	// RuntimeID is the container/pod ID in the underlying runtime
	RuntimeID string         `json:"runtime_id"`
	Timeout   time.Duration  `json:"timeout"` // max sandbox lifetime
	Workspace *WorkspaceInfo `json:"workspace,omitempty"`
}
