package runtime

import (
	"context"
	"io"
)

// Runtime is the abstraction over container orchestration backends (Docker, Kubernetes).
type Runtime interface {
	// CreateSandbox creates a new sandbox container/pod from the given spec.
	CreateSandbox(ctx context.Context, spec SandboxSpec) (*SandboxInfo, error)

	// StartSandbox starts a previously created sandbox (for pool warm-up scenarios).
	StartSandbox(ctx context.Context, id string) error

	// StopSandbox stops a running sandbox.
	StopSandbox(ctx context.Context, id string) error

	// RemoveSandbox removes a sandbox completely.
	RemoveSandbox(ctx context.Context, id string) error

	// GetSandbox returns the current info of a sandbox.
	GetSandbox(ctx context.Context, id string) (*SandboxInfo, error)

	// Exec executes a command synchronously and returns the result.
	Exec(ctx context.Context, id string, req ExecRequest) (*ExecResult, error)

	// ExecStream executes a command and streams output via a channel.
	ExecStream(ctx context.Context, id string, req ExecRequest) (<-chan StreamEvent, error)

	// UploadFile uploads a file into the sandbox.
	UploadFile(ctx context.Context, id string, destPath string, reader io.Reader) error

	// DownloadFile downloads a file from the sandbox.
	DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error)

	// ListFiles lists files in a directory inside the sandbox.
	ListFiles(ctx context.Context, id string, dirPath string) ([]FileInfo, error)

	// UploadArchive uploads a tar archive to the sandbox, extracting it at destDir.
	UploadArchive(ctx context.Context, id string, destDir string, archive io.Reader) error

	// DownloadDir downloads an entire directory from the sandbox as a tar archive.
	DownloadDir(ctx context.Context, id string, dirPath string) (io.ReadCloser, error)

	// ExecPipe executes a command in the sandbox with an io.Reader connected to
	// its stdin. This enables streaming data into the container (e.g. piping a
	// tar archive to "tar xf -") without buffering the entire payload in memory.
	ExecPipe(ctx context.Context, id string, cmd []string, stdin io.Reader) error

	// UpdateNetwork dynamically enables, disables, or updates network access for a running sandbox.
	UpdateNetwork(ctx context.Context, id string, enabled bool, whitelist []string) error

	// RenameSandbox renames a sandbox container/pod for easier identification.
	RenameSandbox(ctx context.Context, id string, newName string) error

	// UpdateLabels patches labels on a sandbox. Use a nil value to remove a label.
	UpdateLabels(ctx context.Context, id string, labels map[string]*string) error

	// ListSandboxes returns sandboxes matching the given labels.
	ListSandboxes(ctx context.Context, labels map[string]string) ([]SandboxInfo, error)

	// IsStateful reports whether sandbox pods/containers survive a process
	// restart independently (true for Kubernetes, false for Docker).
	// When true, Start restores persistent sandboxes synchronously before
	// cleaning up orphaned pool containers, so live pods are not mistakenly
	// deleted.
	IsStateful() bool
}

// FileInfo holds file metadata from inside a sandbox.
type FileInfo struct {
	Name    string
	Path    string
	Size    int64
	IsDir   bool
	ModTime int64 // unix timestamp
}
