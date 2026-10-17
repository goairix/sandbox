package etcd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This is native point-transport/budget coverage only. Arbitrary bounded values
// deliberately have no signed schema or runtime authority; successful transport
// must never be reported as authenticated quiescence or settlement history.
func TestCleanupDetachedNoEffectQuiescencePointCodecCap(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	keys := []string{b.identityKey, b.restoreKey}
	for _, suffix := range []string{"intent", "receipt", "reservation"} {
		key := b.namespace.Root() + suffix
		keys = append(keys, key+strings.Repeat("x", maxNamespaceKeyBytes-len(key)))
	}
	values := []string{b.identityValue, b.restoreEpoch, strings.Repeat("q", 32768), strings.Repeat("r", 4096), strings.Repeat("a", 8192)}
	for i := 2; i < len(keys); i++ {
		_, err := raw.Put(ctx, keys[i], values[i])
		require.NoError(t, err)
	}
	original := b.client.KV
	var actualRequest *pb.TxnRequest
	var actualResponseBytes int
	b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		require.Len(t, yes, 5)
		require.Len(t, no, 5)
		for i := range keys {
			require.True(t, yes[i].IsGet())
			require.True(t, no[i].IsGet())
			require.Equal(t, []byte(keys[i]), yes[i].KeyBytes())
			require.Equal(t, []byte(keys[i]), no[i].KeyBytes())
		}
		actualRequest = &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints(keys), Failure: taskProtoPoints(keys)}
		response, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		if response != nil {
			actualResponseBytes = (*pb.TxnResponse)(response).Size()
		}
		return response, err
	}}
	t.Cleanup(func() { b.client.KV = original })

	points, _, err := b.readCleanupDetachedNoEffectQuiescencePoints(ctx, keys)
	require.NoError(t, err, "the original quiescence codec permits a bounded 32768-byte intent")
	require.Len(t, points, 5)
	for i := 2; i < len(keys); i++ {
		require.Equal(t, values[i], string(points[i].Value))
	}
	require.NotNil(t, actualRequest)
	require.Equal(t, 14, len(actualRequest.Compare)+len(actualRequest.Success)+len(actualRequest.Failure))
	reply := taskProtoResponse(keys, values)
	projection := cleanupWireProjection{name: "detached-noeffect-quiescence-component", request: actualRequest, success: reply, failure: reply}
	require.NoError(t, validateCleanupWire(projection))
	require.LessOrEqual(t, actualRequest.Size(), 128<<10)
	require.LessOrEqual(t, actualResponseBytes, reply.Size())
	require.LessOrEqual(t, reply.Size(), 256<<10)
	t.Logf("quiescence point component nodes=14 request=%d actual-response=%d projected-response=%d", actualRequest.Size(), actualResponseBytes, reply.Size())

	// Each independent native value cap stays finite. The generic point
	// envelope accepts these updated versions; rejection must come from the
	// fixed component's value limit, not a fabricated signature or record.
	for i := 2; i < len(keys); i++ {
		_, err := raw.Put(ctx, keys[i], values[i]+"x")
		require.NoError(t, err)
		_, _, err = b.readCleanupDetachedNoEffectQuiescencePoints(ctx, keys)
		require.ErrorIs(t, err, ErrCorruptRecord)
		_, err = raw.Put(ctx, keys[i], values[i])
		require.NoError(t, err)
	}
}
