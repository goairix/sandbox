package etcd

import (
	"context"

	"github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/goairix/sandbox/internal/runtime/launcher"
)

// TaskWorkspaceSyncLocalRecoveryResult is owned progress, never an execution
// capability or permission to release a task or its retained archive.
type TaskWorkspaceSyncLocalRecoveryResult struct {
	Checkpoint                     *TaskWorkspaceSyncCheckpoint
	RegularFilesComplete           bool
	CompletedFiles, CompletedBytes uint64
}

func (b *Backend) RecoverTaskWorkspaceSyncLocal(ctx context.Context, c *TaskClaim, slot uint32, destination *controltransport.Destination, worker *launcher.KernelBoundary) (TaskWorkspaceSyncLocalRecoveryResult, error) {
	return TaskWorkspaceSyncLocalRecoveryResult{}, ErrInvalidRecord
}
