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
}

// FileInfo holds file metadata from inside a sandbox.
type FileInfo struct {
	Name    string
	Path    string
	Size    int64
	IsDir   bool
	ModTime int64 // unix timestamp
}
