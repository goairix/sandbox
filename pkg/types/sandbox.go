package types

import "time"

type CreateSandboxRequest struct {
	Language     string           `json:"language" binding:"required,oneof=python nodejs bash"`
	Mode         string           `json:"mode" binding:"required,oneof=ephemeral persistent"`
	Timeout      int              `json:"timeout,omitempty"`
	Resources    *ResourceLimits  `json:"resources,omitempty"`
	Network      *NetworkConfig   `json:"network,omitempty"`
	Dependencies []DependencySpec `json:"dependencies,omitempty"`
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
	Version string `json:"version" binding:"required"`
}

type SandboxResponse struct {
	ID        string    `json:"id"`
	Language  string    `json:"language"`
	Mode      string    `json:"mode"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
}

type ErrorResponse struct {
	Error   string `json:"error"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}
