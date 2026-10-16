package etcd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (b *Backend) pinWorkerControl(ctx context.Context, options *WorkerControlOptions) error {
	if options == nil {
		return nil
	}
	key, err := b.namespace.Key("meta", "worker_control_budget")
	if err != nil {
		return err
	}
	points, err := b.readDomain(ctx, key)
	if err != nil {
		return err
	}
	kv := points[0]
	var record workerProfileRecord
	if kv == nil || kv.Lease != 0 || kv.Version != 1 || kv.CreateRevision <= 0 || kv.ModRevision != kv.CreateRevision || string(kv.Key) != key || decodePreparationDomain(kv, &record, workerRecordBytes) != nil {
		return ErrIdentityMismatch
	}
	wire, err := encodeOperationMetadata(record, workerRecordBytes)
	if err != nil || wire != string(kv.Value) || record.Namespace != b.namespace.Root() || record.RestoreEpoch != b.restoreEpoch || record.IdentityDigest != workerIdentityDigest(b.identityValue) {
		return ErrIdentityMismatch
	}
	owned := *options
	if owned.SessionTTL == 0 {
		owned.SessionTTL = 15 * time.Second
	}
	b.workerControl = &workerBackend{origin: b, options: owned, profile: ownTaskFence(kv)}
	return nil
}

// Requires the existing lifecycle lock. These immutable comparisons remain
// private and are composed into the actual original consumer transaction;
// diagnostic references cannot supply, modify or reconstruct them.
func (b *Backend) workerComparisons(s *WorkerSession, p *WorkerPartitionClaim) ([]clientv3.Cmp, error) {
	if !b.validWorkerSession(s) {
		return nil, ErrInvalidRecord
	}
	if err := s.live(); err != nil {
		return nil, err
	}
	cmps := append(b.baseComparisons(), b.workerControl.profile.comparisons()...)
	cmps = append(cmps, s.guard.comparisons()...)
	if p != nil {
		if !b.validWorkerPartition(p) || p.session != s || p.released || p.lost {
			return nil, ErrInvalidRecord
		}
		cmps = append(cmps, p.fence.comparisons()...)
	}
	return cmps, nil
}

func (call *workerCall) requestContext() (context.Context, context.CancelFunc) {
	b, s := call.session.origin, call.session
	b.archiveLife.mu.Lock()
	deadline := s.deadline
	b.archiveLife.mu.Unlock()
	parent := call.ctx
	stop := func() {}
	if !deadline.IsZero() {
		parent, stop = context.WithDeadline(parent, deadline)
	}
	ctx, cancel := b.requestContext(parent)
	return ctx, func() { cancel(); stop() }
}

func (b *Backend) workerFailureKeys(s *WorkerSession) []string {
	return []string{b.identityKey, b.restoreKey, b.workerControl.profile.key, s.guard.key}
}

func (b *Backend) workerFailureValues(s *WorkerSession) []string {
	return []string{b.identityValue, b.restoreEpoch, b.workerControl.profile.value, s.guard.value}
}

func (b *Backend) workerPoints(r *clientv3.TxnResponse, keys []string) ([]*mvccpb.KeyValue, error) {
	if len(r.Responses) != len(keys) {
		return nil, ErrCorruptRecord
	}
	points := make([]*mvccpb.KeyValue, len(keys))
	for i, op := range r.Responses {
		if op == nil {
			return nil, ErrCorruptRecord
		}
		point := op.GetResponseRange()
		if point == nil || point.More || point.Count != int64(len(point.Kvs)) || len(point.Kvs) > 1 || !operationNestedHeader(point.Header, r.Header) {
			return nil, ErrCorruptRecord
		}
		if len(point.Kvs) == 1 {
			kv := point.Kvs[0]
			limit := workerRecordBytes
			if keys[i] == b.identityKey {
				limit = maxIdentityJSONBytes
			}
			if kv == nil || string(kv.Key) != keys[i] || len(kv.Value) > limit || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > r.Header.Revision {
				return nil, ErrCorruptRecord
			}
			points[i] = kv
		}
	}
	return points, nil
}

func (b *Backend) workerFailedFences(s *WorkerSession, r *clientv3.TxnResponse) error {
	points, err := b.workerPoints(r, b.workerFailureKeys(s))
	if err != nil {
		return err
	}
	if points[0] == nil || points[0].Lease != 0 || string(points[0].Value) != b.identityValue || points[1] == nil || points[1].Lease != 0 || string(points[1].Value) != b.restoreEpoch {
		return ErrIdentityMismatch
	}
	if !b.workerControl.profile.matches(points[2]) || points[2].Version != 1 {
		return ErrIdentityMismatch
	}
	if s.guard.create > 0 && (!s.guard.matches(points[3]) || points[3].Version != 1) {
		return ErrGuardExpired
	}
	return ErrConflict
}

func (b *Backend) workerTxn(call *workerCall, cmps []clientv3.Cmp, success []clientv3.Op, protoSuccess []*pb.RequestOp, expected *pb.TxnResponse) (*clientv3.TxnResponse, error) {
	s := call.session
	keys := b.workerFailureKeys(s)
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: protoSuccess, Failure: taskProtoPoints(keys)}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{expected, taskProtoResponse(keys, b.workerFailureValues(s))}); err != nil {
		return nil, err
	}
	if err := call.ctx.Err(); err != nil {
		return nil, err
	}
	rpc, cancel := call.requestContext()
	if err := rpc.Err(); err != nil {
		cancel()
		return nil, err
	}
	call.started = true
	response, err := b.client.Txn(rpc).If(cmps...).Then(success...).Else(workspaceSyncPointOps(keys)...).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err = b.operationResponseHeader(response); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if err = taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(response)}); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if !response.Succeeded {
		return response, b.workerFailedFences(s, response)
	}
	return response, nil
}

func (b *Backend) workerGuardCheck(call *workerCall, p *WorkerPartitionClaim) error {
	s := call.session
	b.archiveLife.mu.Lock()
	cmps, err := b.workerComparisons(s, p)
	b.archiveLife.mu.Unlock()
	if err != nil {
		return err
	}
	keys := []string{b.identityKey}
	r, err := b.workerTxn(call, cmps, []clientv3.Op{clientv3.OpGet(b.identityKey)}, taskProtoPoints(keys), taskProtoResponse(keys, []string{b.identityValue}))
	if err != nil {
		return err
	}
	points, err := b.workerPoints(r, keys)
	if err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if points[0] == nil || points[0].Lease != 0 || string(points[0].Value) != b.identityValue {
		return ErrIdentityMismatch
	}
	return call.postcheck()
}

func (b *Backend) workerRevoke(ctx context.Context, lease int64) error {
	rpc, cancel := b.requestContext(ctx)
	defer cancel()
	r, err := b.client.Revoke(rpc, clientv3.LeaseID(lease))
	if errors.Is(err, rpctypes.ErrLeaseNotFound) {
		return nil
	}
	if err != nil || rpc.Err() != nil {
		return errors.Join(ErrOutcomeUnknown, err, rpc.Err())
	}
	if r == nil || r.Header == nil || r.Header.ClusterId != b.clusterID || r.Header.Revision <= 0 {
		return ErrOutcomeUnknown
	}
	return nil
}

// Exact wire accounting for the largest new control transactions happens
// before this original session's first Grant, including later child carriers.
func (b *Backend) preflightWorker(s *WorkerSession) error {
	record := s.record(math.MaxInt64)
	value, err := encodeWorkerSessionRecord(record)
	if err != nil {
		return err
	}
	guard := taskFence{key: s.guard.key, value: value, lease: math.MaxInt64, create: math.MaxInt64, mod: math.MaxInt64}
	cmps := append(b.baseComparisons(), b.workerControl.profile.comparisons()...)
	cmps = append(cmps, guard.comparisons()...)
	partitionKey, err := b.namespace.workerPartitionKey(255)
	if err != nil {
		return err
	}
	partitionValue, err := encodeWorkerPartitionRecord(workerPartitionRecord{workerSessionRecord: record, Partition: 255})
	if err != nil {
		return err
	}
	partition := taskFence{key: partitionKey, value: partitionValue, lease: math.MaxInt64, create: math.MaxInt64, mod: math.MaxInt64}
	cmps = append(cmps, partition.comparisons()...)
	// The later soft-share schema has these exact additional finite fields;
	// accounting it does not implement allocation or side-effect authority.
	for _, kind := range []string{"relist", "range", "claim"} {
		key, err := b.namespace.Key("workers", "budget", kind, "63")
		if err != nil {
			return err
		}
		share := struct {
			workerSessionRecord
			Kind string `json:"kind"`
			Slot uint16 `json:"slot"`
		}{record, kind, 99}
		shareValue, err := encodeOperationMetadata(share, workerRecordBytes)
		if err != nil {
			return err
		}
		f := taskFence{key: key, value: shareValue, lease: math.MaxInt64, create: math.MaxInt64, mod: math.MaxInt64}
		all := append(append([]clientv3.Cmp(nil), cmps...), f.comparisons()...)
		keys := []string{b.identityKey, b.restoreKey, b.workerControl.profile.key, guard.key}
		values := []string{b.identityValue, b.restoreEpoch, b.workerControl.profile.value, value}
		request := &pb.TxnRequest{Compare: taskProtoComparisons(all), Success: []*pb.RequestOp{taskProtoPut(key, shareValue, math.MaxInt64)}, Failure: taskProtoPoints(keys)}
		if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoPutResponse(1), taskProtoResponse(keys, values)}); err != nil {
			return fmt.Errorf("worker control preflight: %w", err)
		}
	}
	return nil
}
