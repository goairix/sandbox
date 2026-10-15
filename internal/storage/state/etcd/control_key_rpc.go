package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Fixed flat transactions: read has 4 comparisons + 1/2 points (7 nodes),
// provision has 5 comparisons + 1 put/3 points (9 nodes). Both branches and
// maximal protobuf reply scalars are preflighted under 64 nodes / 256 KiB.
// Placeholders account bytes without making a temporary copy of a real seed.
func (b *Backend) controlKeyPreflight(name string, provision bool) error {
	keys := []string{b.identityKey, b.restoreKey, name}
	values := []string{strings.Repeat("x", maxIdentityJSONBytes), strings.Repeat("x", maxIdentityFieldBytes), strings.Repeat("x", ed25519.SeedSize)}
	cmps := b.baseComparisons()
	success := taskProtoPoints(keys[2:])
	failure := taskProtoPoints(keys[:2])
	replies := []*pb.TxnResponse{taskProtoResponse(keys[2:], values[2:]), taskProtoResponse(keys[:2], values[:2])}
	if provision {
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(name), "=", 0))
		success = []*pb.RequestOp{taskProtoPut(name, values[2], 0)}
		failure = taskProtoPoints(keys)
		replies = []*pb.TxnResponse{taskProtoPutResponse(1), taskProtoResponse(keys, values)}
	}
	return taskCheckWire(b.namespace, []*pb.TxnRequest{{Compare: taskProtoComparisons(cmps), Success: success, Failure: failure}}, replies)
}

func (b *Backend) controlKeyHeader(r *clientv3.TxnResponse) error {
	if r == nil || r.Header == nil || r.Header.ClusterId != b.clusterID {
		return ErrIdentityMismatch
	}
	if r.Header.Revision <= 0 || len(r.Header.XXX_unrecognized) != 0 || len(r.XXX_unrecognized) != 0 {
		return ErrCorruptRecord
	}
	return nil
}

func (b *Backend) controlKeyPutResponse(r *clientv3.TxnResponse) error {
	if len(r.Responses) != 1 || r.Responses[0] == nil || len(r.Responses[0].XXX_unrecognized) != 0 {
		return ErrCorruptRecord
	}
	put := r.Responses[0].GetResponsePut()
	if put == nil || put.PrevKv != nil || len(put.XXX_unrecognized) != 0 || !operationNestedHeader(put.Header, r.Header) {
		return ErrCorruptRecord
	}
	if h := put.Header; h != nil && (len(h.XXX_unrecognized) != 0 || h.MemberId != 0 && h.MemberId != r.Header.MemberId || h.RaftTerm != 0 && h.RaftTerm != r.Header.RaftTerm) {
		return ErrCorruptRecord
	}
	if (*pb.TxnResponse)(r).Size() > maxMutationBytes {
		return ErrCorruptRecord
	}
	return nil
}

// controlKeyRead is a key-specific read-only transaction. No retry, watch,
// cache, lease, domain view, or sandbox actor is involved.
func (b *Backend) controlKeyRead(ctx context.Context, name string) ([]byte, error) {
	if err := b.controlKeyInput(ctx); err != nil {
		return nil, err
	}
	if err := b.controlKeyPreflight(name, false); err != nil {
		return nil, err
	}
	rpcCtx, cancel := b.requestContext(ctx)
	r, rpcErr := b.client.Txn(rpcCtx).If(b.baseComparisons()...).Then(clientv3.OpGet(name)).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	defer clearControlKeyResponse(r)
	deadlineErr := rpcCtx.Err()
	cancel()
	if rpcErr != nil || deadlineErr != nil {
		// Never embed transport-provided text, which may contain request data.
		if err := b.controlKeyInput(ctx); err != nil {
			return nil, err
		}
		if deadlineErr != nil {
			return nil, deadlineErr
		}
		return nil, ErrOutcomeUnknown
	}
	if err := b.controlKeyHeader(r); err != nil {
		return nil, err
	}
	keys, limits := []string{name}, []int{ed25519.SeedSize}
	if !r.Succeeded {
		keys, limits = []string{b.identityKey, b.restoreKey}, []int{maxIdentityJSONBytes, maxIdentityFieldBytes}
	}
	points, err := b.readViewResponse(r, keys, limits)
	if err != nil {
		return nil, err
	}
	// Only measure Size after the fixed flat shape has been established.
	if (*pb.TxnResponse)(r).Size() > maxMutationBytes {
		return nil, ErrCorruptRecord
	}
	if !r.Succeeded {
		return nil, ErrIdentityMismatch
	}
	if err := controlKeyRecord(points[0]); err != nil {
		return nil, err
	}
	if err := b.controlKeyInput(ctx); err != nil {
		return nil, err
	}
	return bytes.Clone(points[0].Value), nil
}

// Clear temporary seed-sized point values even when validation rejects the
// envelope. Never walk an untrusted recursive shape or clear an oversized
// transport allocation: at most three flat operations and one KV per point.
func clearControlKeyResponse(r *clientv3.TxnResponse) {
	if r == nil {
		return
	}
	for _, op := range r.Responses[:min(3, len(r.Responses))] {
		if op == nil {
			continue
		}
		point := op.GetResponseRange()
		if point == nil || len(point.Kvs) == 0 {
			continue
		}
		if kv := point.Kvs[0]; kv != nil && len(kv.Value) <= ed25519.SeedSize {
			clear(kv.Value)
		}
	}
}
