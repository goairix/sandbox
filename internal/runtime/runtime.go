package runtime

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned when the target sandbox/pod does not exist.
// Use errors.Is to check for this error across wrapped error chains.
var ErrNotFound = errors.New("sandbox not found")

// ErrFileNotFound is returned when the target file does not exist inside the sandbox.
var ErrFileNotFound = errors.New("file not found")

// ErrWorkspaceFUSEUnsupported is returned by runtimes that do not yet
// implement the trusted FUSE control contract.
var ErrWorkspaceFUSEUnsupported = errors.New("workspace FUSE is unsupported")

// ErrTerminationUnconfirmed means an exact runtime may still be able to reach
// its workspace. Callers must retain the owner and lease.
var ErrTerminationUnconfirmed = errors.New("runtime termination is unconfirmed")

// ErrInvalidRuntimeRef means a FUSE control operation did not identify both
// the runtime name and its immutable provider UID.
var ErrInvalidRuntimeRef = errors.New("invalid exact runtime reference")

// ErrFUSENetworkStateUncertain means a request-scoped policy mutation could
// not be proven fail-closed. The owning Manager must close the public gate and
// tear down the single-use runtime.
var ErrFUSENetworkStateUncertain = errors.New("FUSE network policy state is uncertain")

// RuntimeRef identifies one immutable runtime instance. ID alone is never
// sufficient for a FUSE control operation because names may be reused.
type RuntimeRef struct {
	ID  string
	UID string
}

func NewRuntimeRef(id, uid string) (RuntimeRef, error) {
	if id == "" || uid == "" {
		return RuntimeRef{}, ErrInvalidRuntimeRef
	}
	return RuntimeRef{ID: id, UID: uid}, nil
}

func (r RuntimeRef) Validate() error {
	if r.ID == "" || r.UID == "" {
		return ErrInvalidRuntimeRef
	}
	return nil
}

// RuntimeFencer confirms that an exact immutable runtime instance has
// terminated or has been isolated from its workspace infrastructure.
type RuntimeFencer interface {
	ConfirmTerminated(ctx context.Context, runtimeID, runtimeUID string) (TerminationEvidence, error)
}

// PreparedSandboxRemover removes only the exact immutable runtime instance.
// FUSE pool cleanup must use it after RuntimeUID has been bound.
type PreparedSandboxRemover interface {
	RemovePreparedSandbox(ctx context.Context, runtimeID, runtimeUID string) error
}

// OrphanReconciler removes managed runtime resources only after the manager has
// restored state and supplied the complete set of protected runtime UIDs.
type OrphanReconciler interface {
	ReconcileOrphanedResources(ctx context.Context, protectedRuntimeUIDs map[string]struct{}) error
}

// Runtime is the abstraction over container orchestration backends (Docker, Kubernetes).
type Runtime interface {
	// CreateSandbox creates a new sandbox container/pod from the given spec.
	CreateSandbox(ctx context.Context, spec SandboxSpec) (*SandboxInfo, error)

	// PrepareSandbox creates a prefix-free FUSE sandbox without exposing user
	// execution or starting the mounter.
	PrepareSandbox(ctx context.Context, spec SandboxSpec) (*SandboxInfo, error)

	// AuthorizeWorkspaceMount delivers the one-shot workspace authorization over
	// the runtime's trusted control channel.
	AuthorizeWorkspaceMount(ctx context.Context, ref RuntimeRef, auth WorkspaceMountAuthorization) error

	// WaitSandboxReady waits for both trusted FUSE health and sandbox-side probes.
	WaitSandboxReady(ctx context.Context, ref RuntimeRef, expectedGeneration int64) (*SandboxInfo, error)

	// PreparedSandboxHealth verifies a prepared instance is pristine and belongs
	// to the expected pool key.
	PreparedSandboxHealth(ctx context.Context, ref RuntimeRef, poolKey string) error

	// WorkspaceHealth returns trusted health for the mounted workspace.
	WorkspaceHealth(ctx context.Context, ref RuntimeRef) (*WorkspaceHealth, error)

	// QuiesceWorkspace closes workspace activity and returns a single-use token.
	QuiesceWorkspace(ctx context.Context, ref RuntimeRef, expectedGeneration int64) (WorkspaceQuiesceToken, error)

	// ResumeWorkspace consumes the exact token returned by QuiesceWorkspace.
	ResumeWorkspace(ctx context.Context, ref RuntimeRef, token WorkspaceQuiesceToken) error

	// FlushWorkspace durably flushes the mounted workspace through the trusted
	// mounter control channel.
	FlushWorkspace(ctx context.Context, ref RuntimeRef, expectedGeneration int64) error

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

	// UploadFile uploads a file into the sandbox. Size is the exact, non-negative
	// byte count of reader; unknown sizes are not supported.
	UploadFile(ctx context.Context, id, destPath string, size int64, reader io.Reader) error

	// DownloadFile downloads a file from the sandbox.
	DownloadFile(ctx context.Context, id string, srcPath string) (io.ReadCloser, error)

	// FileExists reports whether a regular file exists at the given path inside the sandbox.
	// Returns ErrFileNotFound if the file does not exist.
	FileExists(ctx context.Context, id string, filePath string) error

	// ReadFileContent streams the raw content of a file from the sandbox without
	// any tar wrapping. The caller must close the returned ReadCloser.
	ReadFileContent(ctx context.Context, id string, srcPath string) (io.ReadCloser, error)

	// GlobInfo returns files matching the glob pattern with their content.
	// Pattern syntax: "*/*.md" matches all .md files in immediate subdirectories.
	// Returns FileContent slice where each Content is a tar stream (same format as DownloadFile).
	GlobInfo(ctx context.Context, id string, pattern string) ([]FileContent, error)

	// DownloadFiles downloads multiple files in parallel.
	// Returns partial results even if some files fail (check FileContent.Error).
	// Each Content is a tar stream (same format as DownloadFile).
	DownloadFiles(ctx context.Context, id string, paths []string) ([]FileContent, error)

	// ListFiles lists files in a directory inside the sandbox.
	ListFiles(ctx context.Context, id string, dirPath string) ([]FileInfo, error)

	// ListFilesRecursive lists files recursively in a directory inside the sandbox.
	// maxDepth controls recursion depth (0 = unlimited). Supports pagination via page/pageSize.
	ListFilesRecursive(ctx context.Context, id string, dirPath string, maxDepth int, page int, pageSize int) (*FileListResult, error)

	// GlobFiles finds files matching a glob pattern inside the sandbox.
	// Supported patterns: **/*.txt, **/*.{txt,md}, *.txt, **/*keyword*.txt
	// The baseDir is the root directory to search from.
	// Supports pagination via page/pageSize.
	GlobFiles(ctx context.Context, id string, baseDir string, pattern string, page int, pageSize int) (*FileListResult, error)

	// ReadFileLines reads a range of lines from a file inside the sandbox.
	// startLine is 1-based. endLine of 0 means read to end of file.
	ReadFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int) (*FileLineResult, error)

	// EditFile performs a string replacement in a file inside the sandbox.
	// If replaceAll is true, all occurrences are replaced; otherwise only the first.
	EditFile(ctx context.Context, id string, filePath string, oldStr string, newStr string, replaceAll bool) error

	// EditFileLines replaces a range of lines in a file inside the sandbox.
	// startLine and endLine are 1-based. endLine of 0 means replace to end of file.
	EditFileLines(ctx context.Context, id string, filePath string, startLine int, endLine int, newContent string) error

	// UploadArchive uploads a tar archive to the sandbox, extracting it at destDir.
	UploadArchive(ctx context.Context, id string, destDir string, archive io.Reader) error

	// DownloadDir downloads an entire directory from the sandbox as a tar archive.
	DownloadDir(ctx context.Context, id string, dirPath string) (io.ReadCloser, error)

	// ExecPipe executes a command in the sandbox with an io.Reader connected to
	// its stdin. This enables streaming data into the container (e.g. piping a
	// tar archive to "tar xf -") without buffering the entire payload in memory.
	ExecPipe(ctx context.Context, id string, cmd []string, stdin io.Reader) error

	// UpdateNetwork dynamically enables, disables, or updates network access for a running sandbox.
	UpdateNetwork(ctx context.Context, id string, enabled bool, whitelist []string, blockPrivate bool) error

	// UpdateFUSENetwork applies only the request-scoped user policy for an exact
	// FUSE runtime; it never mutates that runtime's system egress policy.
	UpdateFUSENetwork(ctx context.Context, ref RuntimeRef, enabled bool, whitelist []string, blockPrivate bool) error

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
	ModTime time.Time
}

// FileListResult holds the result of a recursive file listing.
type FileListResult struct {
	Files      []FileInfo
	TotalCount int
	Page       int
	PageSize   int
}

// FileLineResult holds the result of reading file lines.
type FileLineResult struct {
	Lines      []string
	StartLine  int
	EndLine    int
	TotalLines int
}

// FileContent represents a file with its content stream.
type FileContent struct {
	Path    string
	Content io.ReadCloser // tar stream for single file
	Error   error         // per-file error (for batch operations)
}
