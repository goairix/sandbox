// Package sandbox provides a Go SDK for the Sandbox execution service.
package sandbox

import "time"

// Mode represents the sandbox lifecycle mode.
type Mode string

const (
	ModeEphemeral  Mode = "ephemeral"
	ModePersistent Mode = "persistent"
)

// SyncDirection specifies the direction of a workspace sync operation.
type SyncDirection string

const (
	SyncDirectionFromContainer SyncDirection = "from_container"
	SyncDirectionToContainer   SyncDirection = "to_container"
)

// ResourceLimits specifies resource constraints for a sandbox.
type ResourceLimits struct {
	Memory string `json:"memory,omitempty"`
	CPU    string `json:"cpu,omitempty"`
	Disk   string `json:"disk,omitempty"`
}

// NetworkConfig controls network access for a sandbox.
type NetworkConfig struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist,omitempty"`
}

// DependencySpec describes a single package dependency.
// Manager must be "pip" or "npm".
type DependencySpec struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Manager string `json:"manager"` // "pip" | "npm"
}

// CreateSandboxRequest is the request body for POST /api/v1/sandboxes.
// Mode is required; use ModeEphemeral or ModePersistent.
type CreateSandboxRequest struct {
	Mode          Mode             `json:"mode"`
	Timeout       int              `json:"timeout,omitempty"`
	Resources     *ResourceLimits  `json:"resources,omitempty"`
	Network       *NetworkConfig   `json:"network,omitempty"`
	Dependencies         []DependencySpec `json:"dependencies,omitempty"`
	WorkspacePath        string           `json:"workspace_path,omitempty"`
	WorkspaceSyncExclude []string         `json:"workspace_sync_exclude,omitempty"`
}

// SandboxResponse is returned by sandbox lifecycle endpoints.
type SandboxResponse struct {
	ID        string    `json:"id"`
	Mode      Mode      `json:"mode"`
	State     string    `json:"state"`
	RuntimeID string    `json:"runtime_id"`
	CreatedAt time.Time `json:"created_at"`
}

// UpdateNetworkRequest is the request body for PUT /api/v1/sandboxes/:id/network.
type UpdateNetworkRequest struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist,omitempty"`
}

// UpdateNetworkResponse is returned by the update network endpoint.
type UpdateNetworkResponse struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist"`
}

// ExecRequest is the request body for POST /api/v1/sandboxes/:id/exec.
type ExecRequest struct {
	Language        string            `json:"language"`
	Code            string            `json:"code"`
	Stdin           string            `json:"stdin,omitempty"`
	Timeout         int               `json:"timeout,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	LineBuffered    bool              `json:"line_buffered,omitempty"`
	RequiresNetwork bool             `json:"requires_network,omitempty"`
}

// ExecResponse is returned by the exec endpoint.
// Duration is in seconds (float64).
type ExecResponse struct {
	ExitCode int     `json:"exit_code"`
	Stdout   string  `json:"stdout"`
	Stderr   string  `json:"stderr"`
	Duration float64 `json:"duration"`
}

// ExecuteRequest is the request body for POST /api/v1/execute (one-shot).
type ExecuteRequest struct {
	Language        string            `json:"language"`
	Code            string            `json:"code"`
	Stdin           string            `json:"stdin,omitempty"`
	Timeout         int               `json:"timeout,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	Resources       *ResourceLimits   `json:"resources,omitempty"`
	Network         *NetworkConfig    `json:"network,omitempty"`
	Dependencies    []DependencySpec  `json:"dependencies,omitempty"`
	LineBuffered    bool              `json:"line_buffered,omitempty"`
	RequiresNetwork bool             `json:"requires_network,omitempty"`
}

// FileInfo describes a file or directory entry.
type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	IsDir   bool      `json:"is_dir"`
	ModTime time.Time `json:"mod_time"`
}

// FileListResponse is returned by the list files endpoint.
type FileListResponse struct {
	Files []FileInfo `json:"files"`
	Path  string     `json:"path"`
}

// FileUploadResponse is returned by the upload file endpoint.
type FileUploadResponse struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// MountWorkspaceRequest is the request body for POST /api/v1/sandboxes/:id/workspace/mount.
type MountWorkspaceRequest struct {
	RootPath string   `json:"root_path"`
	Exclude  []string `json:"exclude,omitempty"`
}

// MountWorkspaceResponse is returned by the mount workspace endpoint.
type MountWorkspaceResponse struct {
	RootPath  string    `json:"root_path"`
	MountedAt time.Time `json:"mounted_at"`
}

// SyncWorkspaceRequest is the request body for POST /api/v1/sandboxes/:id/workspace/sync.
type SyncWorkspaceRequest struct {
	Direction SyncDirection `json:"direction"` // SyncDirectionToContainer | SyncDirectionFromContainer
	Exclude   []string      `json:"exclude,omitempty"`
}

// SyncWorkspaceResponse is returned by the sync workspace endpoint.
type SyncWorkspaceResponse struct {
	Direction string `json:"direction"`
	Message   string `json:"message"`
}

// WorkspaceInfoResponse is returned by the workspace info endpoint.
type WorkspaceInfoResponse struct {
	Mounted      bool      `json:"mounted"`
	RootPath     string    `json:"root_path,omitempty"`
	MountedAt    time.Time `json:"mounted_at,omitempty"`
	LastSyncedAt time.Time `json:"last_synced_at,omitempty"`
}

// SkillMeta holds the parsed frontmatter fields of a SKILL.md.
type SkillMeta struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Compatibility string            `json:"compatibility,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// SkillFile describes a non-SKILL.md file inside a skill directory.
type SkillFile struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// SkillListResponse is returned by GET /api/v1/sandboxes/:id/skills.
type SkillListResponse struct {
	Skills []SkillMeta `json:"skills"`
}

// SkillResponse is returned by GET /api/v1/sandboxes/:id/skills/:name.
type SkillResponse struct {
	SkillMeta
	Path    string      `json:"path"`
	Content string      `json:"content"`
	Files   []SkillFile `json:"files"`
}

// SSEEventType identifies the kind of a streaming execution event.
type SSEEventType string

const (
	SSEEventStdout SSEEventType = "stdout"
	SSEEventStderr SSEEventType = "stderr"
	SSEEventDone   SSEEventType = "done"
	SSEEventError  SSEEventType = "error"
	SSEEventPing   SSEEventType = "ping" // keepalive heartbeat
)

// SSEEvent is a single event received from a streaming execution endpoint.
type SSEEvent struct {
	Type      SSEEventType
	Content   string  // stdout/stderr content, or error message
	ExitCode  int     // set when Type == SSEEventDone
	Elapsed   float64 // seconds elapsed, set when Type == SSEEventDone
	Timestamp int64   // Unix timestamp, set when Type == SSEEventPing
}

// ListFilesRecursiveRequest is the request body for POST /api/v1/sandboxes/:id/files/list-recursive.
type ListFilesRecursiveRequest struct {
	Path     string `json:"path"`
	MaxDepth int    `json:"max_depth,omitempty"`
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
}

// ListFilesRecursiveResponse is returned by the list-recursive endpoint.
type ListFilesRecursiveResponse struct {
	Files      []FileInfo `json:"files"`
	Path       string     `json:"path"`
	TotalCount int        `json:"total_count"`
	Page       int        `json:"page"`
	PageSize   int        `json:"page_size"`
}

// ReadFileLinesRequest is the request body for POST /api/v1/sandboxes/:id/files/read-lines.
type ReadFileLinesRequest struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

// ReadFileLinesResponse is returned by the read-lines endpoint.
type ReadFileLinesResponse struct {
	Lines      []string `json:"lines"`
	StartLine  int      `json:"start_line"`
	EndLine    int      `json:"end_line"`
	TotalLines int      `json:"total_lines"`
}

// EditFileRequest is the request body for POST /api/v1/sandboxes/:id/files/edit.
type EditFileRequest struct {
	Path       string `json:"path"`
	OldStr     string `json:"old_str"`
	NewStr     string `json:"new_str"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// EditFileLinesRequest is the request body for POST /api/v1/sandboxes/:id/files/edit-lines.
type EditFileLinesRequest struct {
	Path       string `json:"path"`
	StartLine  int    `json:"start_line"`
	EndLine    int    `json:"end_line,omitempty"`
	NewContent string `json:"new_content"`
}
