package etcd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
)

func TestSnapshotReadInput(t *testing.T) {
	for _, b := range []*Backend{nil, {}} {
		result, err := b.ReadWorkspaceSnapshot(context.Background(), WorkspaceIdentity{})
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrInvalidRecord)
		result, err = b.ReadSandboxSnapshot(context.Background(), "sandbox")
		require.Nil(t, result)
		require.ErrorIs(t, err, ErrInvalidRecord)
	}
	b := snapshotLifecycleBackend(t)
	_, err := b.registerSnapshotRead(nil)
	require.ErrorIs(t, err, ErrInvalidRecord)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = b.registerSnapshotRead(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestSnapshotReadWire(t *testing.T) {
	o := validOptions(t)
	b := &Backend{namespace: o.Namespace, restoreEpoch: "epoch", identityValue: "identity"}
	b.identityKey, _ = b.namespace.Key("meta", "identity")
	b.restoreKey, _ = b.namespace.Key("meta", "restore_epoch")
	key, _ := snapshotCacheTestRecord("sandbox", "{}")
	keys, err := b.snapshotReadKeys(key)
	require.NoError(t, err)
	require.NoError(t, b.snapshotReadPreflight(keys))
	// The old view contract must remain four metadata points, each <=4096.
	require.ErrorIs(t, b.readViewPreflight(keys[:], []int{65536, 4096}, nil), ErrInvalidRecord)
	bad := keys
	bad[1] = "/outside/placement"
	require.ErrorIs(t, b.snapshotReadPreflight(bad), ErrInvalidRecord)
	request := &pb.TxnRequest{Compare: taskProtoComparisons(b.baseComparisons()), Success: taskProtoPoints(keys[:]), Failure: taskProtoPoints([]string{b.identityKey, b.restoreKey})}
	require.Less(t, request.Size(), maxMutationBytes)
	for _, op := range request.Success {
		r := op.GetRequestRange()
		require.NotNil(t, r)
		require.Empty(t, r.RangeEnd)
	}
}
