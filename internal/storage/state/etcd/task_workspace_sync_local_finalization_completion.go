package etcd

import (
	"context"
	"errors"
	"math"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (f *taskWorkspaceSyncLocalFinalizer) frozenCheckpoint(ctx context.Context) error {
	s := f.source
	kv, err := s.readCheckpoint(ctx)
	if err != nil || kv.ModRevision != f.progress.CheckpointRevision || workspaceSyncIntentDigest(string(kv.Value)) != f.progress.CheckpointDigest {
		return errors.Join(ErrConflict, err)
	}
	r, err := s.decodeCheckpoint(kv)
	if err != nil {
		return err
	}
	s.checkpointKV = kv
	if r.WorkspaceSync != nil {
		copy := *r.WorkspaceSync
		s.checkpoint = &copy
	}
	return nil
}

func (f *taskWorkspaceSyncLocalFinalizer) readCompletion(kv *mvccpb.KeyValue) (*TaskWorkspaceSyncLocalCompletionReference, error) {
	p, err := decodeWorkspaceSyncLocalComplete(kv)
	s := f.source
	if err != nil || string(kv.Key) != f.completeKey || p.Task != s.manifest.Seed.Task || p.TaskBirth != s.manifest.Seed.TaskBirth || p.Binding != f.binding || p.RootDigest != s.manifest.Binding.TargetDigest || f.progress.Phase != "sealed" || f.progressKV.ModRevision != kv.CreateRevision || workspaceSyncIntentDigest(string(f.progressKV.Value)) != p.ProgressDigest || p.CheckpointRevision != f.progress.CheckpointRevision || p.CheckpointDigest != f.progress.CheckpointDigest || p.EvidenceDigest != f.progress.EvidenceDigest || p.Claim != f.progress.Claim {
		return nil, errors.Join(ErrIdentityMismatch, err)
	}
	return &TaskWorkspaceSyncLocalCompletionReference{Task: p.Task, CreateRevision: kv.CreateRevision, SHA256: workspaceSyncIntentDigest(string(kv.Value))}, nil
}

// Called with the sole foreground source operation held. It seals effect
// admission and actually joins/closes the target without waiting on itself.
func (f *taskWorkspaceSyncLocalFinalizer) sealEffects(ctx context.Context) error {
	s := f.source
	if err := s.guard(ctx); err != nil {
		return err
	}
	s.effectsSealed = true
	if err := s.target.Close(); err != nil {
		return err
	}
	if s.localIOComplete != nil {
		s.localIOComplete("target-close")
	}
	return s.guard(ctx)
}

func (f *taskWorkspaceSyncLocalFinalizer) publishCompletion(ctx context.Context) error {
	s := f.source
	if !s.effectsSealed || f.progress.Phase != "effects-done" || s.checkpointKV == nil {
		return ErrInvalidRecord
	}
	c := s.authority.claim.reference
	next := f.progress
	next.Phase, next.Claim = "sealed", taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}
	value, err := encodeWorkspaceSyncLocalProgress(next)
	if err != nil {
		return err
	}
	complete := taskWorkspaceSyncLocalComplete{1, "sandbox.workspace-sync-local-complete.v1", c.Task, s.manifest.Seed.TaskBirth, f.binding, next.Claim, workspaceSyncIntentDigest(value), next.EvidenceDigest, next.CheckpointRevision, next.CheckpointDigest, s.manifest.Binding.TargetDigest}
	completion, err := encodeOperationMetadata(complete, 8192)
	if err != nil {
		return err
	}
	cmps := append(s.comparisons(nil), f.progressComparisons()...)
	cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(s.authority.claim.checkpointKey), "=", s.checkpointKV.ModRevision), clientv3.Compare(clientv3.Value(s.authority.claim.checkpointKey), "=", string(s.checkpointKV.Value)), clientv3.Compare(clientv3.Version(f.completeKey), "=", 0))
	r, err := s.native(ctx, cmps, []clientv3.Op{clientv3.OpPut(f.progressKey, value), clientv3.OpPut(f.completeKey, completion)}, []*pb.RequestOp{taskProtoPut(f.progressKey, value, 0), taskProtoPut(f.completeKey, completion, 0)}, taskProtoPutResponse(2), false)
	if err != nil {
		return err
	}
	if !operationPutResponses(r, 2) || r.Header.Revision <= f.progressKV.ModRevision || r.Header.Revision == math.MaxInt64 {
		return ErrOutcomeUnknown
	}
	f.progress, f.progressKV = next, &mvccpb.KeyValue{Key: []byte(f.progressKey), Value: []byte(value), CreateRevision: f.progressKV.CreateRevision, ModRevision: r.Header.Revision, Version: f.progressKV.Version + 1}
	kv := &mvccpb.KeyValue{Key: []byte(f.completeKey), Value: []byte(completion), CreateRevision: r.Header.Revision, ModRevision: r.Header.Revision, Version: 1}
	f.result.Completion, err = f.readCompletion(kv)
	if err != nil {
		return err
	}
	return s.guard(ctx)
}
