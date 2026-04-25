package types

import "time"

type CreateSandboxRequest struct {
	Mode          string           `json:"mode" binding:"required,oneof=ephemeral persistent"`
	Timeout       int              `json:"timeout,omitempty" binding:"min=-1"` // seconds; 0 = use default, -1 = never expire
	Resources     *ResourceLimits  `json:"resources,omitempty"`
	Network       *NetworkConfig   `json:"network,omitempty"`
	Dependencies  []DependencySpec `json:"dependencies,omitempty"`
	WorkspacePath        string           `json:"workspace_path,omitempty"`
	WorkspaceSyncExclude []string         `json:"workspace_sync_exclude,omitempty"`
}

type ResourceLimits struct {
	Memory string `json:"memory,omitempty"`
	CPU    string `json:"cpu,omitempty"`
	Disk   string `json:"disk,omitempty"`
}

type NetworkConfig struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist,omitempty"`
}

type DependencySpec struct {
	Name    string `json:"name" binding:"required"`
	Version string `json:"version,omitempty"`
	Manager string `json:"manager" binding:"required,oneof=pip npm"`
}

type SandboxResponse struct {
	ID        string     `json:"id"`
	Mode      string     `json:"mode"`
	State     string     `json:"state"`
	RuntimeID string     `json:"runtime_id"`
	CreatedAt time.Time  `json:"created_at"`
	Timeout   int        `json:"timeout"`              // seconds; -1 = never expire
	ExpiresAt *time.Time `json:"expires_at,omitempty"` // nil when timeout = -1
}

type ErrorResponse struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

type UpdateNetworkRequest struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist,omitempty"`
}

type UpdateNetworkResponse struct {
	Enabled   bool     `json:"enabled"`
	Whitelist []string `json:"whitelist"`
}

type UpdateTTLRequest struct {
	Timeout int `json:"timeout" binding:"required,min=1"` // seconds; must be > 0 (cannot set to never-expire after creation)
}

type UpdateTTLResponse struct {
	Timeout   int       `json:"timeout"`
	ExpiresAt time.Time `json:"expires_at"`
}
