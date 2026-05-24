package sandbox

import "errors"

// Sentinel errors for the sandbox package. Callers should use errors.Is to
// check for these rather than inspecting error strings.
var (
	// ErrSandboxNotFound is returned when a sandbox ID does not exist.
	ErrSandboxNotFound = errors.New("sandbox not found")

	// ErrWorkspaceAlreadyMounted is returned when a workspace is already
	// mounted for a sandbox.
	ErrWorkspaceAlreadyMounted = errors.New("workspace already mounted")

	// ErrNoWorkspaceMounted is returned when an operation requires a mounted
	// workspace but none exists.
	ErrNoWorkspaceMounted = errors.New("no workspace mounted")

	// ErrUploadNotFound is returned when a multipart upload ID does not exist.
	ErrUploadNotFound = errors.New("upload not found")

	// ErrUnexpectedChunkIndex is returned when a chunk arrives out of order.
	ErrUnexpectedChunkIndex = errors.New("unexpected chunk index")

	// ErrIncompleteUpload is returned when a multipart upload is finalised
	// before all chunks have been received.
	ErrIncompleteUpload = errors.New("incomplete upload")
)
