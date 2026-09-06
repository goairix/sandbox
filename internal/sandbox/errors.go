package sandbox

import "errors"

// Sentinel errors for the sandbox package. Callers should use errors.Is to
// check for these rather than inspecting error strings.
var (
	// ErrSandboxNotFound is returned when a sandbox ID does not exist.
	ErrSandboxNotFound = errors.New("sandbox not found")

	// ErrSandboxNotReady is returned when the sandbox operation gate is closed.
	ErrSandboxNotReady = errors.New("sandbox not ready")

	// ErrSandboxCleanupPending means final sync/flush or an exact teardown
	// boundary failed and the runtime plus ownership state were retained.
	ErrSandboxCleanupPending = errors.New("sandbox cleanup pending")

	// ErrWorkspaceAlreadyMounted is returned when a workspace is already
	// mounted for a sandbox.
	ErrWorkspaceAlreadyMounted = errors.New("workspace already mounted")

	// ErrNoWorkspaceMounted is returned when an operation requires a mounted
	// workspace but none exists.
	ErrNoWorkspaceMounted = errors.New("no workspace mounted")

	// ErrInvalidWorkspaceMountMode is returned when request-level workspace
	// mode selection is invalid for the requested workspace or release.
	ErrInvalidWorkspaceMountMode = errors.New("invalid workspace mount mode")

	// ErrFUSEWorkspaceImmutable prevents public mount APIs from mutating the
	// lifecycle of a runtime-bound FUSE workspace.
	ErrFUSEWorkspaceImmutable = errors.New("FUSE workspace mount is immutable")

	// ErrFUSEWorkspaceOperationUnsupported is kept as a compatibility alias.
	// New callers should use ErrFUSEWorkspaceImmutable.
	ErrFUSEWorkspaceOperationUnsupported = ErrFUSEWorkspaceImmutable

	// ErrReservedFUSEWorkspacePath prevents public file APIs from exposing the
	// exact internal probe object used by the FUSE propagation protocol.
	ErrReservedFUSEWorkspacePath = errors.New("path is reserved by the FUSE workspace protocol")

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
