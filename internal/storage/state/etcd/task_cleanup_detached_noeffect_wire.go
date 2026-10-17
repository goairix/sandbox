package etcd

import (
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This projection intentionally has no reservation pre-read: with the original
// 53 drain comparisons, six positive Mod pins and one absence comparison, both
// three-point read branches would require 65 nodes. Only native Else reads the
// original settlement alongside identity/restore, for a literal 64-node write.
func (b *Backend) detachedNoEffectWriteProjection(cmps []clientv3.Cmp, key, value string) (cleanupWireProjection, error) {
	if b == nil || len(value) == 0 || len(value) > p.MaxDetachedNoEffectSettlementBytes || !validNamespaceKey(b.namespace, key) {
		return cleanupWireProjection{}, ErrInvalidMutation
	}
	keys := []string{b.identityKey, b.restoreKey, key}
	projection := cleanupWireProjection{
		name: "detached-noeffect-write",
		request: &pb.TxnRequest{
			Compare: taskProtoComparisons(cmps),
			Success: []*pb.RequestOp{taskProtoPut(key, value, 0)},
			Failure: taskProtoPoints(keys),
		},
		success: taskProtoPutResponse(1),
		// Else may observe a different maximal settlement, even when this
		// original attempt's submitted value is short. This padding is only
		// conservative wire accounting; the real Put retains exact bytes.
		failure: taskProtoResponse(keys, []string{b.identityValue, b.restoreEpoch, cleanupWirePadding(p.MaxDetachedNoEffectSettlementBytes)}),
	}
	if err := validateCleanupWire(projection); err != nil {
		return cleanupWireProjection{}, err
	}
	return projection, nil
}

// All native read branches include the identical bounded source or arbitration
// points. This helper measures actual protobuf objects; it grants no caller a
// key-selection or mutation capability and never dispatches a request itself.
func (b *Backend) detachedNoEffectPointProjection(name string, keys, values []string) (cleanupWireProjection, error) {
	if b == nil || len(keys) != len(values) || len(keys) != 3 && len(keys) != 9 || keys[0] != b.identityKey || keys[1] != b.restoreKey {
		return cleanupWireProjection{}, ErrInvalidRecord
	}
	for i, key := range keys {
		if !validNamespaceKey(b.namespace, key) || len(values[i]) > maxRecordBytes {
			return cleanupWireProjection{}, ErrInvalidRecord
		}
	}
	response := taskProtoResponse(keys, values)
	projection := cleanupWireProjection{
		name: "detached-noeffect-" + name,
		request: &pb.TxnRequest{
			Compare: taskProtoComparisons(b.baseComparisons()),
			Success: taskProtoPoints(keys),
			Failure: taskProtoPoints(keys),
		},
		success: response,
		failure: response,
	}
	if err := validateCleanupWire(projection); err != nil {
		return cleanupWireProjection{}, err
	}
	return projection, nil
}
