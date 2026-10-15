package etcd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (s *taskWorkspaceSyncRecoverySource) localFinalizationBinding() (string, error) {
	if err := s.manifest.validate(); err != nil {
		return "", err
	}
	wire, err := json.Marshal(s.manifest)
	if err != nil {
		return "", err
	}
	b := s.authority.backend.workspaceSyncBudget
	d := framedDigest("sandbox.workspace-sync-local-binding.v1", workspaceSyncIntentDigest(string(wire)), b.value, strconv.FormatInt(b.create, 10))
	return hex.EncodeToString(d[:]), nil
}

func (f *taskWorkspaceSyncLocalFinalizer) points(ctx context.Context, keys []string, bounds []int) ([]*mvccpb.KeyValue, error) {
	s := f.source
	values := make([]string, len(bounds))
	for i, bound := range bounds {
		values[i] = string(make([]byte, bound))
	}
	r, err := s.native(ctx, s.comparisons(nil), workspaceSyncPointOps(keys), taskProtoPoints(keys), taskProtoResponse(keys, values), false)
	if err != nil {
		return nil, err
	}
	return workspaceSyncPointResponse(s.authority.backend, r, keys)
}

func (f *taskWorkspaceSyncLocalFinalizer) progressComparisons() []clientv3.Cmp {
	return []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(f.progressKey), "=", f.progressKV.ModRevision), clientv3.Compare(clientv3.Value(f.progressKey), "=", string(f.progressKV.Value))}
}

func (f *taskWorkspaceSyncLocalFinalizer) saveProgress(ctx context.Context, next taskWorkspaceSyncLocalProgress) error {
	s := f.source
	c := s.authority.claim.reference
	next.Claim = taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}
	value, err := encodeWorkspaceSyncLocalProgress(next)
	if err != nil {
		return err
	}
	cmps := s.comparisons(nil)
	if f.charge != nil {
		cmps = f.metadataComparisons(false)
	}
	if f.progressKV == nil {
		cmps = append(cmps, clientv3.Compare(clientv3.Version(f.progressKey), "=", 0))
	} else if f.charge == nil {
		if !recyclingCanAdvance(f.progressKV.Version) {
			return ErrConflict
		}
		cmps = append(cmps, f.progressComparisons()...)
	}
	r, err := s.native(ctx, cmps, []clientv3.Op{clientv3.OpPut(f.progressKey, value)}, []*pb.RequestOp{taskProtoPut(f.progressKey, value, 0)}, taskProtoPutResponse(1), f.charge != nil)
	if err != nil {
		return err
	}
	if !operationPutResponses(r, 1) || r.Header.Revision == math.MaxInt64 || f.progressKV != nil && r.Header.Revision <= f.progressKV.ModRevision {
		return ErrOutcomeUnknown
	}
	kv := &mvccpb.KeyValue{Key: []byte(f.progressKey), Value: []byte(value), CreateRevision: r.Header.Revision, ModRevision: r.Header.Revision, Version: 1}
	if f.progressKV != nil {
		kv.CreateRevision, kv.Version = f.progressKV.CreateRevision, f.progressKV.Version+1
	}
	f.progress, f.progressKV = next, kv
	if f.charge != nil {
		return f.metadataGuard(ctx)
	}
	return s.guard(ctx)
}

func (f *taskWorkspaceSyncLocalFinalizer) loadProgress(ctx context.Context) (*mvccpb.KeyValue, error) {
	s := f.source
	points, err := f.points(ctx, []string{f.progressKey, f.completeKey}, []int{16384, 8192})
	if err != nil {
		return nil, err
	}
	if points[0] == nil {
		if points[1] != nil {
			return nil, ErrCorruptRecord
		}
		c := s.authority.claim.reference
		f.progress = taskWorkspaceSyncLocalProgress{Version: 1, Task: c.Task, TaskBirth: s.manifest.Seed.TaskBirth, Binding: f.binding, Claim: taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}, Phase: "regular"}
		return nil, f.saveProgress(ctx, f.progress)
	}
	p, err := decodeWorkspaceSyncLocalProgress(points[0])
	if err != nil || p.Task != s.manifest.Seed.Task || p.TaskBirth != s.manifest.Seed.TaskBirth || p.Binding != f.binding || points[0].CreateRevision <= s.manifest.Seed.ArchiveRevision {
		return nil, errors.Join(ErrIdentityMismatch, err)
	}
	c := s.authority.claim.reference
	if p.Phase == "regular" && p.Claim.ClaimID == c.ClaimID {
		// Legacy regular GC already occupies all 64 nodes and cannot compare
		// progress. A genuine fresh claim is its required late-commit fence.
		return nil, ErrConflict
	}
	if p.Claim.ClaimID != c.ClaimID && (p.Claim.CreateRevision >= c.CreateRevision || points[0].ModRevision >= c.CreateRevision) || p.Claim.ClaimID == c.ClaimID && (p.Claim != (taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}) || points[0].ModRevision <= c.CreateRevision) {
		return nil, ErrConflict
	}
	f.progress, f.progressKV = p, points[0]
	return points[1], nil
}

func (s *taskWorkspaceSyncRecoverySource) validateLocalArchiveOrder(ctx context.Context) error {
	cursor := newWorkspaceSyncArchiveCursor(s.manifest.Received.Bytes, s.manifest.Received.Entries, s.index, s.plan, s.readAt)
	var order taskWorkspaceSyncLocalArchiveOrder
	for {
		e, err := cursor.next(ctx)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := order.accept(e); err != nil {
			return err
		}
	}
}

func (f *taskWorkspaceSyncLocalFinalizer) freezeCheckpoint(ctx context.Context) error {
	s := f.source
	for slot := uint32(0); slot < s.authority.backend.workspaceSyncBudget.options.SlotsPerPartition; slot++ {
		charge, err := s.readCharged(ctx, slot)
		if err != nil {
			return err
		}
		if charge != nil {
			return ErrConflict
		}
	}
	kv, err := s.readCheckpoint(ctx)
	if err != nil {
		return err
	}
	// Version 1 is the durable initial checkpoint, explicitly representing an
	// empty regular prefix. It is frozen without rewriting its legacy wire.
	next := f.progress
	next.Phase, next.CheckpointRevision, next.CheckpointDigest = "metadata", kv.ModRevision, workspaceSyncIntentDigest(string(kv.Value))
	s.checkpointKV = kv
	return f.saveProgress(ctx, next)
}
