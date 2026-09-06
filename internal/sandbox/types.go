package sandbox

import (
	"errors"
	"time"
)

var (
	// ErrNetworkRequired is returned when a command requires network access
	// but the sandbox has networking disabled.
	ErrNetworkRequired = errors.New("network access is required but not enabled for this sandbox")
	// ErrInvalidExecTimeout is returned when a requested execution timeout is invalid.
	ErrInvalidExecTimeout = errors.New("invalid execution timeout")
	// ErrExecTimeout is returned when a sandbox execution exceeds its effective timeout.
	ErrExecTimeout = errors.New("sandbox execution timed out")
)

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
	Manager string `json:"manager"` // "pip" or "npm"
}

// ResourceLimits defines resource constraints for a sandbox.
type ResourceLimits struct {
	Memory  string `json:"memory"`   // e.g. "256Mi"
	CPU     string `json:"cpu"`      // e.g. "0.5"
	Disk    string `json:"disk"`     // /workspace, e.g. "100Mi"
	TmpDisk string `json:"tmp_disk"` // /tmp, e.g. "50Mi"
}

// NetworkConfig defines network settings for a sandbox.
type NetworkConfig struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist"` // allowed domains/IPs
	// BlockPrivate blocks RFC1918 private ranges by default; Whitelist entries
	// can still reach internal addresses.
	BlockPrivate bool `json:"block_private"`
}

type WorkspaceMountMode string

// WorkspaceMountType is retained as a compatibility name for persisted
// workspace metadata. Request routing uses WorkspaceMountMode.
type WorkspaceMountType = WorkspaceMountMode

const (
	WorkspaceMountSync  WorkspaceMountMode = "sync"
	WorkspaceMountLocal WorkspaceMountMode = "local"
	WorkspaceMountFUSE  WorkspaceMountMode = "fuse"
)

type WorkspaceMountState string

const (
	WorkspaceMountMounting WorkspaceMountState = "mounting"
	WorkspaceMountReady    WorkspaceMountState = "ready"
	WorkspaceMountError    WorkspaceMountState = "error"
)

// WorkspaceInfo holds metadata about a mounted workspace.
type WorkspaceInfo struct {
	RootPath             string              `json:"root_path"`
	MountedAt            time.Time           `json:"mounted_at"`
	LastSyncedAt         time.Time           `json:"last_synced_at,omitempty"`
	LastHealthyAt        time.Time           `json:"last_healthy_at,omitempty"`
	BindMounted          bool                `json:"bind_mounted,omitempty"`
	SyncExclude          []string            `json:"sync_exclude,omitempty"`
	MountType            WorkspaceMountType  `json:"mount_type,omitempty"`
	MountState           WorkspaceMountState `json:"mount_state,omitempty"`
	Driver               string              `json:"driver,omitempty"`
	Owner                WorkspaceOwner      `json:"owner,omitempty"`
	LeaseGeneration      int64               `json:"lease_generation,omitempty"`
	FUSEPreparationID    string              `json:"fuse_preparation_id,omitempty"`
	FUSEPoolKey          string              `json:"fuse_pool_key,omitempty"`
	FUSEReservationToken string              `json:"fuse_reservation_token,omitempty"`
	FUSERecordRevision   uint64              `json:"fuse_record_revision,omitempty"`
	Flushed              bool                `json:"flushed,omitempty"`
	LastFlushedAt        *time.Time          `json:"last_flushed_at,omitempty"`
}

// SandboxConfig holds all configuration for creating a sandbox.
type SandboxConfig struct {
	Mode                 Mode               `json:"mode"`
	Timeout              int                `json:"timeout"` // seconds; 0 = use default, -1 = never expire
	Resources            ResourceLimits     `json:"resources"`
	Network              NetworkConfig      `json:"network"`
	Dependencies         []Dependency       `json:"dependencies"`
	WorkspacePath        string             `json:"workspace_path,omitempty"`
	WorkspaceMountMode   WorkspaceMountMode `json:"workspace_mount_mode,omitempty"`
	WorkspaceSyncExclude []string           `json:"workspace_sync_exclude,omitempty"`
}

// Sandbox represents a running sandbox instance.
type Sandbox struct {
	ID        string        `json:"id"`
	Config    SandboxConfig `json:"config"`
	State     State         `json:"state"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
	// RuntimeID is the container/pod ID in the underlying runtime
	RuntimeID  string         `json:"runtime_id"`
	RuntimeUID string         `json:"runtime_uid,omitempty"`
	Timeout    time.Duration  `json:"timeout"` // max sandbox lifetime
	Workspace  *WorkspaceInfo `json:"workspace,omitempty"`
}
