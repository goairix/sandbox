package etcd

import (
	"archive/tar"
	"context"
	"errors"
	"io"

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
	s, err := b.prepareTaskWorkspaceSyncRecovery(ctx, c, slot, destination, worker)
	if err != nil {
		return TaskWorkspaceSyncLocalRecoveryResult{}, err
	}
	complete, runErr := s.run(s.ctx)
	closeErr := s.Close()
	result := TaskWorkspaceSyncLocalRecoveryResult{RegularFilesComplete: complete && runErr == nil && closeErr == nil, CompletedFiles: s.completedFiles, CompletedBytes: s.completedBytes}
	if s.checkpoint != nil {
		copy := *s.checkpoint
		result.Checkpoint = &copy
	}
	return result, errors.Join(runErr, closeErr)
}

func (s *taskWorkspaceSyncRecoverySource) run(ctx context.Context) (bool, error) {
	for {
		e, err := s.next(ctx)
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if e.kind == tar.TypeDir {
			continue
		}
		body, err := s.openBody(ctx, e)
		if err != nil {
			return false, err
		}
		charge, err := s.reserveNext(ctx, body)
		if err != nil {
			return false, err
		}
		q, err := s.reconcileLocal(ctx, body, charge)
		if err != nil {
			return false, err
		}
		if err := s.settleLocal(ctx, charge, q); err != nil {
			return false, err
		}
		outcome, err := s.recycleLocal(ctx, charge, q)
		if err != nil || outcome != OutcomeCommitted {
			return false, errors.Join(ErrOutcomeUnknown, err)
		}
		if err := body.Close(); err != nil {
			return false, err
		}
	}
}
