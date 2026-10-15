package etcd

import (
	"context"
	"errors"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The complete four-record schema has caps 4096+1024+4096+4096 = 13312
// bytes (including JSON escaping); it never reads the 64KiB snapshot payload.
// At most eight comparisons, four success points, and two failure points are
// 14 nodes. Maximal keys and protobuf scalars are included by taskProtoResponse.
// Both request branches and both replies are checked before every dispatch.
func (b *Backend) readViewPreflight(keys []string, limits []int, pins []clientv3.Cmp) error {
	if len(keys) > 4 || len(keys) != len(limits) || len(pins) > 4 {
		return ErrInvalidRecord
	}
	values := make([]string, len(keys))
	for i, key := range keys {
		if !validNamespaceKey(b.namespace, key) || limits[i] <= 0 || limits[i] > 4096 {
			return ErrInvalidRecord
		}
		values[i] = strings.Repeat("x", limits[i])
	}
	for _, pin := range pins {
		if pin.Target != pb.Compare_MOD || pin.Result != pb.Compare_EQUAL || len(pin.RangeEnd) != 0 {
			return ErrInvalidRecord
		}
	}
	metadata := []string{b.identityKey, b.restoreKey}
	cmps := append(b.baseComparisons(), pins...)
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints(keys), Failure: taskProtoPoints(metadata)}
	return taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{
		taskProtoResponse(keys, values), taskProtoResponse(metadata, []string{strings.Repeat("x", maxIdentityJSONBytes), strings.Repeat("x", maxIdentityFieldBytes)}),
	})
}

// readViewPoints is private to the fixed domain view: no range scans, retry,
// writes, leases, or generalized store adaptation are involved.
func (b *Backend) readViewPoints(ctx context.Context, keys []string, limits []int, pins []clientv3.Cmp) ([]*mvccpb.KeyValue, int64, error) {
	if err := b.readViewPreflight(keys, limits, pins); err != nil {
		return nil, 0, err
	}
	ops := make([]clientv3.Op, len(keys))
	for i, key := range keys {
		ops[i] = clientv3.OpGet(key)
	}
	rpcCtx, cancel := b.requestContext(ctx)
	r, err := b.client.Txn(rpcCtx).If(append(b.baseComparisons(), pins...)...).Then(ops...).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	rpcErr := rpcCtx.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return nil, 0, errors.Join(err, rpcErr)
	}
	if r == nil || r.Header == nil || r.Header.ClusterId != b.clusterID {
		return nil, 0, ErrIdentityMismatch
	}
	if r.Header.Revision <= 0 || len(r.Header.XXX_unrecognized) != 0 || len(r.XXX_unrecognized) != 0 {
		return nil, 0, ErrCorruptRecord
	}
	if !r.Succeeded {
		metadata := []string{b.identityKey, b.restoreKey}
		points, err := b.readViewResponse(r, metadata, []int{maxIdentityJSONBytes, maxIdentityFieldBytes})
		if err != nil {
			return nil, 0, errors.Join(ErrIdentityMismatch, err)
		}
		if (*pb.TxnResponse)(r).Size() > maxMutationBytes {
			return nil, 0, ErrCorruptRecord
		}
		if points[0] == nil || points[1] == nil || string(points[0].Value) != b.identityValue || string(points[1].Value) != b.restoreEpoch {
			return nil, 0, ErrIdentityMismatch
		}
		if len(pins) == 0 {
			return nil, 0, ErrIdentityMismatch
		}
		return nil, 0, ErrConflict
	}
	points, err := b.readViewResponse(r, keys, limits)
	// Check the fixed flat shape before protobuf Size: an unexpected recursive
	// Txn response must not trigger an unbounded traversal in the generated code.
	if err == nil && (*pb.TxnResponse)(r).Size() > maxMutationBytes {
		return nil, 0, ErrCorruptRecord
	}
	return points, r.Header.Revision, err
}

func (b *Backend) readViewResponse(r *clientv3.TxnResponse, keys []string, limits []int) ([]*mvccpb.KeyValue, error) {
	if len(r.Responses) != len(keys) {
		return nil, ErrCorruptRecord
	}
	points := make([]*mvccpb.KeyValue, len(keys))
	for i, key := range keys {
		op := r.Responses[i]
		if op == nil || len(op.XXX_unrecognized) != 0 {
			return nil, ErrCorruptRecord
		}
		rangeResponse := op.GetResponseRange()
		if rangeResponse == nil || len(rangeResponse.XXX_unrecognized) != 0 || rangeResponse.More || len(rangeResponse.Kvs) > 1 || rangeResponse.Count != int64(len(rangeResponse.Kvs)) || !operationNestedHeader(rangeResponse.Header, r.Header) {
			return nil, ErrCorruptRecord
		}
		if h := rangeResponse.Header; h != nil && (len(h.XXX_unrecognized) != 0 || h.MemberId != 0 && h.MemberId != r.Header.MemberId || h.RaftTerm != 0 && h.RaftTerm != r.Header.RaftTerm) {
			return nil, ErrCorruptRecord
		}
		if len(rangeResponse.Kvs) == 0 {
			continue
		}
		kv := rangeResponse.Kvs[0]
		if kv == nil || string(kv.Key) != key || !validNamespaceKey(b.namespace, string(kv.Key)) || len(kv.XXX_unrecognized) != 0 || kv.Lease != 0 || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > r.Header.Revision || kv.Version <= 0 || kv.Version > kv.ModRevision-kv.CreateRevision+1 || len(kv.Value) > limits[i] {
			return nil, ErrCorruptRecord
		}
		points[i] = kv
	}
	return points, nil
}
