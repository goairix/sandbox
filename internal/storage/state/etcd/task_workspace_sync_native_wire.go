package etcd

import (
	"context"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Wire and response mechanics only. The caller retains its own registration,
// authority comparisons, native serialization and physical postguard.
func workspaceSyncNativeTxn(ctx context.Context, b *Backend, cmps []clientv3.Cmp, success []clientv3.Op, protoSuccess []*pb.RequestOp, reply *pb.TxnResponse, emptyFailure bool) (*clientv3.TxnResponse, error) {
	projection := workspaceSyncNativeProjection(b, cmps, protoSuccess, reply, emptyFailure)
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{projection.request}, []*pb.TxnResponse{projection.success, projection.failure}); err != nil {
		return nil, err
	}

	keys := []string{b.identityKey, b.restoreKey, b.workspaceSyncBudget.key}
	if emptyFailure {
		keys = nil
	}
	rpcCtx, cancel := b.requestContext(ctx)
	r, err := b.client.Txn(rpcCtx).If(cmps...).Then(success...).Else(workspaceSyncPointOps(keys)...).Commit()
	rpcErr := rpcCtx.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err := b.operationResponseHeader(r); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if err := taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(r)}); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if !r.Succeeded {
		if emptyFailure {
			if len(r.Responses) != 0 {
				return nil, ErrOutcomeUnknown
			}
			return r, ErrConflict
		}
		points, err := workspaceSyncPointResponse(b, r, keys)
		if err != nil {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
		if !b.workspaceSyncBudgetMatches(points[2]) {
			return r, ErrIdentityMismatch
		}
		return r, ErrConflict
	}
	return r, nil
}

// This is the native envelope only: its caller still owns serialization,
// authority and physical postguards. The empty failure branch is essential for
// the existing full64-node settlements and must not acquire diagnostic reads.
func workspaceSyncNativeProjection(b *Backend, cmps []clientv3.Cmp, success []*pb.RequestOp, reply *pb.TxnResponse, emptyFailure bool) cleanupWireProjection {
	keys := []string{b.identityKey, b.restoreKey, b.workspaceSyncBudget.key}
	values := []string{b.identityValue, b.restoreEpoch, b.workspaceSyncBudget.value}
	if emptyFailure {
		keys, values = nil, nil
	}
	return cleanupWireProjection{
		request: &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: success, Failure: taskProtoPoints(keys)},
		success: reply, failure: taskProtoResponse(keys, values),
	}
}
