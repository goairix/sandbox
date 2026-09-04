package sandbox

import "errors"

// Sentinel errors for the sandbox package. Callers should use errors.Is to
// check for these rather than inspecting error strings.
var (
	// ErrSandboxNotFound is returned when a sandbox ID does not exist.
	ErrSandboxNotFound = errors.New("sandbox not found")

	// ErrSandboxNotReady is returned when the sandbox operation gate is closed.
	ErrSandboxNotReady = errors.New("sandbox not ready")

	// ErrWorkspaceAlreadyMounted is returned when a workspace is already
	// mounted for a sandbox.
	ErrWorkspaceAlreadyMounted = errors.New("workspace already mounted")

	// ErrNoWorkspaceMounted is returned when an operation requires a mounted
	// workspace but none exists.
	ErrNoWorkspaceMounted = errors.New("no workspace mounted")

	// ErrFUSEWorkspaceOperationUnsupported prevents legacy copy-sync workspace
	// APIs from mutating the lifecycle of a runtime-bound FUSE workspace.
	ErrFUSEWorkspaceOperationUnsupported = errors.New("legacy workspace operation is unsupported for FUSE sandbox")

	// ErrWorkspaceAcquireCleanupUnconfirmed means lease acquisition failed and
	// exact compensation could not prove that every provisional write vanished.
	ErrWorkspaceAcquireCleanupUnconfirmed = errors.New("workspace acquisition cleanup is unconfirmed")

	// ErrUploadNotFound is returned when a multipart upload ID does not exist.
	ErrUploadNotFound = errors.New("upload not found")

	// ErrUnexpectedChunkIndex is returned when a chunk arrives out of order.
	ErrUnexpectedChunkIndex = errors.New("unexpected chunk index")

	// ErrIncompleteUpload is returned when a multipart upload is finalised
	// before all chunks have been received.
	ErrIncompleteUpload = errors.New("incomplete upload")
)
