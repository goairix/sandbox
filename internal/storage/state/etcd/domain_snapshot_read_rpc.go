package etcd

import (
	"context"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func (b *Backend) snapshotReadKeys(key snapshotCacheKey) ([2]string, error) {
	var keys [2]string
	if err := (SnapshotReference{Version: key.version, Digest: key.digest}).Validate(); err != nil {
		return keys, err
	}
	_, snapshot, err := b.namespace.sandboxKeys(key.partition, key.sandboxID, key.version)
	if err != nil {
		return keys, err
	}
	keys[0] = snapshot
	keys[1], err = b.namespace.placementKey(key.sandboxID)
	return keys, err
}

// This profile permits only the snapshot and its placement, preserving the
// separate metadata view's original 4096-byte point limit and pin contract.
func (b *Backend) snapshotReadPreflight(keys [2]string) error {
	for _, key := range keys {
		if !validNamespaceKey(b.namespace, key) {
			return ErrInvalidRecord
		}
	}
	metadata := []string{b.identityKey, b.restoreKey}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(b.baseComparisons()), Success: taskProtoPoints(keys[:]), Failure: taskProtoPoints(metadata)}
	return taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{
		taskProtoResponse(keys[:], []string{strings.Repeat("x", 65536), strings.Repeat("x", 4096)}),
		taskProtoResponse(metadata, []string{strings.Repeat("x", maxIdentityJSONBytes), strings.Repeat("x", maxIdentityFieldBytes)}),
	})
}

func (b *Backend) readSnapshotPoints(ctx context.Context, keys [2]string) ([]*mvccpb.KeyValue, error) {
	if err := b.snapshotReadPreflight(keys); err != nil {
		return nil, err
	}
	points, _, err := b.readViewDispatch(ctx, keys[:], []int{65536, 4096}, nil)
	return points, err
}
