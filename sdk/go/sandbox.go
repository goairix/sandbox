package sandbox

import (
	"context"
	"io"
)

// SandboxOptions configures a new sandbox created via NewSandbox.
type SandboxOptions struct {
	Mode                 Mode
	Timeout              int
	Resources            *ResourceLimits
	Network              *NetworkConfig
	Dependencies         []DependencySpec
	WorkspacePath        string
	WorkspaceSyncExclude []string
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
		Mode:                 mode,
		Timeout:              opts.Timeout,
		Resources:            opts.Resources,
		Network:              opts.Network,
		Dependencies:         opts.Dependencies,
		WorkspacePath:        opts.WorkspacePath,
		WorkspaceSyncExclude: opts.WorkspaceSyncExclude,
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

// RunStream executes code in the sandbox and streams output as SSE events.
func (s *Sandbox) RunStream(ctx context.Context, language, code string) (<-chan SSEEvent, error) {
	return s.client.ExecStream(ctx, s.id, ExecRequest{Language: language, Code: code})
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
// exclude is an optional list of path prefixes to skip during all syncs.
func (s *Sandbox) MountWorkspace(ctx context.Context, rootPath string, exclude ...string) error {
	_, err := s.client.MountWorkspace(ctx, s.id, MountWorkspaceRequest{RootPath: rootPath, Exclude: exclude})
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

// EnableNetwork enables network access with an optional destination whitelist.
// Pass nil or an empty slice to allow all external traffic.
func (s *Sandbox) EnableNetwork(ctx context.Context, whitelist []string) error {
	_, err := s.client.UpdateNetwork(ctx, s.id, UpdateNetworkRequest{Enabled: true, Whitelist: whitelist})
	return err
}

// BlockPrivateNetwork enables network access but blocks RFC1918 private IP ranges by default.
// Entries in internalWhitelist are still allowed to reach internal addresses.
// Pass nil or an empty slice if no internal addresses need to be reachable.
func (s *Sandbox) BlockPrivateNetwork(ctx context.Context, internalWhitelist []string) error {
	_, err := s.client.UpdateNetwork(ctx, s.id, UpdateNetworkRequest{
		Enabled:      true,
		Whitelist:    internalWhitelist,
		BlockPrivate: true,
	})
	return err
}

// DisableNetwork disables all network access.
func (s *Sandbox) DisableNetwork(ctx context.Context) error {
	_, err := s.client.UpdateNetwork(ctx, s.id, UpdateNetworkRequest{Enabled: false})
	return err
}

// UpdateTTL dynamically updates the sandbox's TTL.
// timeoutSeconds must be > 0; setting to never-expire (-1) after creation is not allowed.
func (s *Sandbox) UpdateTTL(ctx context.Context, timeoutSeconds int) (UpdateTTLResponse, error) {
	return s.client.UpdateTTL(ctx, s.id, UpdateTTLRequest{Timeout: timeoutSeconds})
}

// ListSkills lists all agent skills stored in the sandbox at /workspace/.agent/skills/.
func (s *Sandbox) ListSkills(ctx context.Context) (SkillListResponse, error) {
	return s.client.ListSkills(ctx, s.id)
}

// GetSkill returns the full content and attached file list for a named skill.
func (s *Sandbox) GetSkill(ctx context.Context, name string) (SkillResponse, error) {
	return s.client.GetSkill(ctx, s.id, name)
}

// GetSkillFile returns the raw content of an attached skill file.
// filePath is relative to the skill directory (e.g. "scripts/run.sh").
// Caller must close the returned ReadCloser.
func (s *Sandbox) GetSkillFile(ctx context.Context, name, filePath string) (io.ReadCloser, error) {
	return s.client.GetSkillFile(ctx, s.id, name, filePath)
}

// ListFilesRecursive lists files recursively in a sandbox directory.
func (s *Sandbox) ListFilesRecursive(ctx context.Context, req ListFilesRecursiveRequest) (ListFilesRecursiveResponse, error) {
	return s.client.ListFilesRecursive(ctx, s.id, req)
}

// GlobFiles finds files matching a glob pattern in a sandbox directory.
func (s *Sandbox) GlobFiles(ctx context.Context, req GlobFilesRequest) (GlobFilesResponse, error) {
	return s.client.GlobFiles(ctx, s.id, req)
}

// ReadFile reads the full content of a file from the sandbox as a stream.
// Caller is responsible for closing the returned ReadCloser.
func (s *Sandbox) ReadFile(ctx context.Context, remotePath string) (io.ReadCloser, error) {
	return s.client.ReadFile(ctx, s.id, remotePath)
}

// ReadFileLines reads a range of lines from a file in the sandbox.
func (s *Sandbox) ReadFileLines(ctx context.Context, req ReadFileLinesRequest) (ReadFileLinesResponse, error) {
	return s.client.ReadFileLines(ctx, s.id, req)
}

// EditFile performs a string replacement in a file in the sandbox.
func (s *Sandbox) EditFile(ctx context.Context, req EditFileRequest) error {
	return s.client.EditFile(ctx, s.id, req)
}

// EditFileLines replaces a range of lines in a file in the sandbox.
func (s *Sandbox) EditFileLines(ctx context.Context, req EditFileLinesRequest) error {
	return s.client.EditFileLines(ctx, s.id, req)
}

// InitMultipartUpload starts a multipart upload session for a large file.
// totalChunks is the total number of chunks that will be uploaded (1–10000).
// Returns the upload ID to use in subsequent UploadChunk / CompleteMultipartUpload calls.
func (s *Sandbox) InitMultipartUpload(ctx context.Context, remotePath string, totalChunks int) (string, error) {
	resp, err := s.client.InitMultipartUpload(ctx, s.id, MultipartInitRequest{
		Path:        remotePath,
		TotalChunks: totalChunks,
	})
	if err != nil {
		return "", err
	}
	return resp.UploadID, nil
}

// UploadChunk uploads a single chunk for an in-progress multipart upload.
// chunkIndex is zero-based and must be uploaded in sequential order.
// Returns the number of chunks received so far and the total expected.
func (s *Sandbox) UploadChunk(ctx context.Context, uploadID string, chunkIndex int, r io.Reader) (received, total int, err error) {
	resp, err := s.client.UploadChunk(ctx, s.id, uploadID, chunkIndex, r)
	if err != nil {
		return 0, 0, err
	}
	return resp.Received, resp.Total, nil
}

// GetMultipartStatus returns the current status of a multipart upload.
func (s *Sandbox) GetMultipartStatus(ctx context.Context, uploadID string) (MultipartStatusResponse, error) {
	return s.client.GetMultipartStatus(ctx, s.id, uploadID)
}

// CompleteMultipartUpload finalises a multipart upload, merges all chunks, and returns the destination path and file size.
func (s *Sandbox) CompleteMultipartUpload(ctx context.Context, uploadID string) (MultipartCompleteResponse, error) {
	return s.client.CompleteMultipartUpload(ctx, s.id, uploadID)
}

// CancelMultipartUpload cancels an in-progress multipart upload and removes all staging files.
func (s *Sandbox) CancelMultipartUpload(ctx context.Context, uploadID string) error {
	return s.client.CancelMultipartUpload(ctx, s.id, uploadID)
}

// Run is a convenience method on Client for one-shot execution without pre-creating a sandbox.
func (c *Client) Run(ctx context.Context, language, code string) (ExecResponse, error) {
	return c.Execute(ctx, ExecuteRequest{Language: language, Code: code})
}
