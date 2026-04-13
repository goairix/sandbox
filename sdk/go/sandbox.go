package sandbox

import (
	"context"
	"io"
)

// SandboxOptions configures a new sandbox created via NewSandbox.
type SandboxOptions struct {
	Mode          Mode
	Timeout       int
	Resources     *ResourceLimits
	Network       *NetworkConfig
	Dependencies  []DependencySpec
	WorkspacePath string
}

// Sandbox is a high-level handle to a running sandbox instance.
type Sandbox struct {
	client *Client
	id     string
}

// ID returns the sandbox identifier.
func (s *Sandbox) ID() string { return s.id }

// NewSandbox creates a new sandbox and returns a high-level Sandbox handle.
// If opts.Mode is empty, ModeEphemeral is used.
func (c *Client) NewSandbox(ctx context.Context, opts SandboxOptions) (*Sandbox, error) {
	mode := opts.Mode
	if mode == "" {
		mode = ModeEphemeral
	}
	req := CreateSandboxRequest{
		Mode:          mode,
		Timeout:       opts.Timeout,
		Resources:     opts.Resources,
		Network:       opts.Network,
		Dependencies:  opts.Dependencies,
		WorkspacePath: opts.WorkspacePath,
	}
	resp, err := c.CreateSandbox(ctx, req)
	if err != nil {
		return nil, err
	}
	return &Sandbox{client: c, id: resp.ID}, nil
}

// Close destroys the sandbox. Suitable for use with defer.
func (s *Sandbox) Close(ctx context.Context) error {
	return s.client.DestroySandbox(ctx, s.id)
}

// Run executes code in the sandbox and returns the result.
func (s *Sandbox) Run(ctx context.Context, language, code string) (ExecResponse, error) {
	return s.client.Exec(ctx, s.id, ExecRequest{Language: language, Code: code})
}

// UploadFile uploads a file to the sandbox at remotePath.
func (s *Sandbox) UploadFile(ctx context.Context, remotePath string, r io.Reader) error {
	_, err := s.client.UploadFile(ctx, s.id, remotePath, r)
	return err
}

// DownloadFile downloads a file from the sandbox. Caller must close the returned ReadCloser.
func (s *Sandbox) DownloadFile(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	return s.client.DownloadFile(ctx, s.id, remotePath)
}

// ListFiles lists files in a directory inside the sandbox.
func (s *Sandbox) ListFiles(ctx context.Context, dir string) (FileListResponse, error) {
	return s.client.ListFiles(ctx, s.id, dir)
}

// MountWorkspace mounts a workspace by root path.
func (s *Sandbox) MountWorkspace(ctx context.Context, rootPath string) error {
	_, err := s.client.MountWorkspace(ctx, s.id, MountWorkspaceRequest{RootPath: rootPath})
	return err
}

// UnmountWorkspace unmounts the current workspace.
func (s *Sandbox) UnmountWorkspace(ctx context.Context) error {
	return s.client.UnmountWorkspace(ctx, s.id)
}

// Sync syncs the workspace from container to host (from_container direction).
func (s *Sandbox) Sync(ctx context.Context) (SyncWorkspaceResponse, error) {
	return s.client.SyncWorkspace(ctx, s.id, SyncWorkspaceRequest{Direction: SyncDirectionFromContainer})
}

// SyncTo syncs the workspace from host to container (to_container direction).
func (s *Sandbox) SyncTo(ctx context.Context) (SyncWorkspaceResponse, error) {
	return s.client.SyncWorkspace(ctx, s.id, SyncWorkspaceRequest{Direction: SyncDirectionToContainer})
}

// WorkspaceInfo returns the current workspace status.
func (s *Sandbox) WorkspaceInfo(ctx context.Context) (WorkspaceInfoResponse, error) {
	return s.client.GetWorkspaceInfo(ctx, s.id)
}

// EnableNetwork enables network access with the given whitelist.
func (s *Sandbox) EnableNetwork(ctx context.Context, whitelist []string) error {
	_, err := s.client.UpdateNetwork(ctx, s.id, UpdateNetworkRequest{Enabled: true, Whitelist: whitelist})
	return err
}

// DisableNetwork disables network access.
func (s *Sandbox) DisableNetwork(ctx context.Context) error {
	_, err := s.client.UpdateNetwork(ctx, s.id, UpdateNetworkRequest{Enabled: false})
	return err
}

// Run is a convenience method on Client for one-shot execution without pre-creating a sandbox.
func (c *Client) Run(ctx context.Context, language, code string) (ExecResponse, error) {
	return c.Execute(ctx, ExecuteRequest{Language: language, Code: code})
}
