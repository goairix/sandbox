package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"

	"github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type taskWorkspaceSyncLocalFinalizer struct {
	source                            *taskWorkspaceSyncRecoverySource
	progress                          taskWorkspaceSyncLocalProgress
	progressKV                        *mvccpb.KeyValue
	progressKey, completeKey, binding string
	transcript                        hash.Hash
	result                            TaskWorkspaceSyncLocalFinalizationResult
	charge                            *taskWorkspaceSyncLocalMetadataCharge
}

func (b *Backend) refuseWorkspaceSyncLocalFinalization(ctx context.Context, c *TaskClaim) error {
	if b.archiveStore == nil || b.archiveStore.recovery == nil {
		return nil
	}
	progress, complete, err := b.namespace.workspaceSyncLocalKeys(c.reference.Task)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if err := c.live(ctx); err != nil {
		c.mu.Unlock()
		return err
	}
	cmps := append(b.baseComparisons(), c.comparisons()...)
	c.mu.Unlock()
	prefix, err := b.taskOperationsPrefix(c.reference.Task)
	if err != nil {
		return err
	}
	cmps = append(cmps, clientv3.Compare(clientv3.Version(prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(prefix)))
	if b.workspaceSyncBudget != nil {
		cmps = append(cmps, b.workspaceSyncBudget.comparisons()...)
	}
	keys := []string{progress, complete}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints(keys)}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoResponse(keys, []string{string(make([]byte, 16384)), string(make([]byte, 8192))}), taskProtoResponse(nil, nil)}); err != nil {
		return err
	}
	rpcCtx, cancel := b.requestContext(ctx)
	r, err := b.client.Txn(rpcCtx).If(cmps...).Then(workspaceSyncPointOps(keys)...).Commit()
	rpcErr := rpcCtx.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err := b.operationResponseHeader(r); err != nil {
		return err
	}
	if !r.Succeeded {
		return ErrConflict
	}
	if err := taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(r)}); err != nil {
		return err
	}
	points, err := workspaceSyncPointResponse(b, r, keys)
	if err != nil {
		return err
	}
	if points[0] != nil || points[1] != nil {
		return ErrConflict
	}
	return nil
}

func (b *Backend) FinalizeTaskWorkspaceSyncLocal(ctx context.Context, c *TaskClaim, slot uint32, destination *controltransport.Destination, worker *launcher.KernelBoundary) (TaskWorkspaceSyncLocalFinalizationResult, error) {
	s, err := b.prepareTaskWorkspaceSyncRecoveryMode(ctx, c, slot, destination, worker, true)
	if err != nil {
		return TaskWorkspaceSyncLocalFinalizationResult{}, err
	}
	// The full source remains registered until actual target/archive Close.
	result, runErr := s.finalizeLocal(s.ctx)
	closeErr := s.Close()
	result.LocalComplete = result.LocalComplete && runErr == nil && closeErr == nil
	return result, errors.Join(runErr, closeErr)
}

func (s *taskWorkspaceSyncRecoverySource) finalizeLocal(ctx context.Context) (TaskWorkspaceSyncLocalFinalizationResult, error) {
	f := &taskWorkspaceSyncLocalFinalizer{source: s, transcript: sha256.New()}
	s.finalization = f
	var err error
	f.progressKey, f.completeKey, err = s.authority.backend.namespace.workspaceSyncLocalKeys(s.authority.claim.reference.Task)
	if err == nil {
		f.binding, err = s.localFinalizationBinding()
	}
	if err != nil {
		return f.result, err
	}
	if err := s.begin(ctx); err != nil {
		return f.result, err
	}
	err = s.validateLocalArchiveOrder(ctx)
	var completeKV *mvccpb.KeyValue
	if err == nil {
		completeKV, err = f.loadProgress(ctx)
	}
	s.end()
	if err != nil {
		return f.result, err
	}
	if f.progress.Phase == "regular" {
		if err := s.restoreCheckpoint(ctx); err != nil {
			return f.result, err
		}
		_, err = s.run(ctx)
		if err != nil {
			return f.result, err
		}
		if err := s.begin(ctx); err != nil {
			return f.result, err
		}
		err = f.freezeCheckpoint(ctx)
		s.end()
		if err != nil {
			return f.result, err
		}
	}
	if err := s.begin(ctx); err != nil {
		return f.result, err
	}
	defer s.end()
	if err := f.frozenCheckpoint(ctx); err != nil {
		return f.result, err
	}
	if completeKV != nil {
		f.result.Completion, err = f.readCompletion(completeKV)
		if err == nil {
			err = f.sealEffects(ctx)
		}
	} else if f.progress.Phase == "sealed" {
		err = ErrCorruptRecord
	} else {
		if f.progress.Phase == "metadata" {
			c := s.authority.claim.reference
			if f.progress.Claim.ClaimID != c.ClaimID {
				next := f.progress
				next.Claim = taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}
				err = f.saveProgress(ctx, next)
			}
			if err == nil {
				err = f.reserveMetadata(ctx)
			}
			if err == nil {
				err = f.executeMetadata(ctx)
			}
			if err == nil {
				c := s.authority.claim.reference
				evidence := taskWorkspaceSyncLocalMetadataEvidence{1, "sandbox.workspace-sync-local-metadata-evidence.v1", workspaceSyncIntentDigest(f.charge.value), f.charge.birth, taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}, s.manifest.Binding.TargetDigest, f.progress.CheckpointRevision, f.progress.CheckpointDigest, hex.EncodeToString(f.transcript.Sum(nil)), f.result.CreatedDirectories, f.result.RemovedEntries, f.result.UpdatedDirectoryModes}
				var value string
				value, err = encodeOperationMetadata(evidence, 4096)
				if err == nil {
					err = f.settleMetadata(ctx, workspaceSyncIntentDigest(value))
				}
			}
		}
		if err == nil {
			err = f.sealEffects(ctx)
		}
		if err == nil {
			err = f.publishCompletion(ctx)
		}
	}
	f.result.CompletedFiles, f.result.CompletedBytes = s.completedFiles, s.completedBytes
	if s.checkpoint != nil {
		copy := *s.checkpoint
		f.result.Checkpoint = &copy
	}
	f.result.LocalComplete = err == nil && f.result.Completion != nil
	return f.result, err
}
