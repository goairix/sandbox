package types

import "time"

type MountWorkspaceRequest struct {
	RootPath string `json:"root_path" binding:"required"`
}

type MountWorkspaceResponse struct {
	RootPath  string    `json:"root_path"`
	MountedAt time.Time `json:"mounted_at"`
}

type UnmountWorkspaceResponse struct {
	Message string `json:"message"`
}

type SyncWorkspaceRequest struct {
	Direction string   `json:"direction" binding:"required,oneof=to_container from_container"`
	Exclude   []string `json:"exclude,omitempty"`
}

type SyncWorkspaceResponse struct {
	Direction string `json:"direction"`
	Message   string `json:"message"`
}

type WorkspaceInfoResponse struct {
	Mounted      bool      `json:"mounted"`
	RootPath     string    `json:"root_path,omitempty"`
	MountedAt    time.Time `json:"mounted_at,omitempty"`
	LastSyncedAt time.Time `json:"last_synced_at,omitempty"`
}
