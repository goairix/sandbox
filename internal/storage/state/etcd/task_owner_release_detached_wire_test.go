package etcd

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This is an actual protobuf projection with the original Claim constructor's
// dimensional placeholders. No client/job/Kernel exists and no effect or
// native Claim authority is claimed by these bounds.
func detachedOwnerReleaseWireModel(t *testing.T) (*taskLocalRuntimeAuthority, *mvccpb.KeyValue, [3]string) {
	t.Helper()
	b, input := cleanupLocalWireFixture(t, true)
	c, _, err := b.prepareTaskClaim(context.Background(), input.bundle, input.workerID, 24*time.Hour, "ffffffff-ffff-4fff-bfff-ffffffffffff")
	require.NoError(t, err)
	ref := input.bundle.task.Reference
	termination, provider, release, err := b.namespace.localRuntimeReleaseKeys(ref)
	require.NoError(t, err)
	prefix, err := b.taskOperationsPrefix(ref)
	require.NoError(t, err)
	settlement, err := b.namespace.taskDetachedNoEffectKey(ref)
	require.NoError(t, err)
	q, err := b.namespace.taskQuiescenceKey(ref)
	require.NoError(t, err)
	qr, err := b.namespace.taskQuiescenceAttemptKey(ref)
	require.NoError(t, err)
	fs, err := b.namespace.taskFinalSyncAttemptKey(ref)
	require.NoError(t, err)
	issuer, err := b.namespace.commandIssuerKey("11111111-1111-4111-8111-111111111111")
	require.NoError(t, err)
	_, snapshot, err := b.namespace.sandboxKeys(ref.Partition, ref.SandboxID, input.bundle.task.Snapshot.Version)
	require.NoError(t, err)
	intent, err := b.namespace.intentKey(ref.Partition, input.bundle.task.CreationIntentID)
	require.NoError(t, err)
	publication, _, err := b.namespace.runtimePublicationKeys(ref.Partition, input.bundle.task.CreationIntentID)
	require.NoError(t, err)
	receipt := ref.Namespace + "p/00/stages/request/task_quiesce_users/ffffffff-ffff-4fff-bfff-ffffffffffff/receipt"
	comparisons := append(b.baseComparisons(), c.comparisons()...)
	require.Len(t, comparisons, 44)
	for _, key := range []string{q, receipt, qr, fs, issuer, snapshot, intent, publication} {
		comparisons = append(comparisons, clientv3.Compare(clientv3.ModRevision(key), "=", math.MaxInt64))
	}
	a := &taskLocalRuntimeAuthority{backend: b, task: input.bundle.task, comparisons: comparisons, prefix: prefix, terminationKey: termination, providerKey: provider, releaseKey: release,
		detached: &taskDetachedRuntimeAuthority{settlement: taskFence{key: settlement, mod: math.MaxInt64}},
	}
	deleted := [3]string{c.fences[5].key, c.fences[4].key, c.fences[7].key}
	kv := &mvccpb.KeyValue{Key: []byte(termination), Value: []byte(strings.Repeat("t", 16384)), CreateRevision: math.MaxInt64 - 1, ModRevision: math.MaxInt64, Version: 3}
	return a, kv, deleted
}

func TestDetachedOwnerReleaseLiteral63NodesAndBudget(t *testing.T) {
	a, termination, deleted := detachedOwnerReleaseWireModel(t)
	value := strings.Repeat("r", 8192)
	cmps, request, success, err := a.detachedReleaseProjection(termination, deleted, value)
	require.NoError(t, err)
	require.Len(t, cmps, 59)
	require.Len(t, request.Compare, 59)
	require.Len(t, request.Success, 4)
	require.Empty(t, request.Failure)
	require.Equal(t, 63, len(request.Compare)+len(request.Success)+len(request.Failure))
	require.Equal(t, taskProtoComparisons(a.comparisons), request.Compare[:52], "all base4/Claim40/source8 comparisons must survive")
	wantTail := []clientv3.Cmp{
		clientv3.Compare(clientv3.Version(a.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(a.prefix)),
		clientv3.Compare(clientv3.ModRevision(a.detached.settlement.key), "=", math.MaxInt64),
		clientv3.Compare(clientv3.Value(a.terminationKey), "=", string(termination.Value)),
		clientv3.Compare(clientv3.LeaseValue(a.terminationKey), "=", 0),
		clientv3.Compare(clientv3.CreateRevision(a.terminationKey), "=", math.MaxInt64-1),
		clientv3.Compare(clientv3.ModRevision(a.terminationKey), "=", math.MaxInt64),
		clientv3.Compare(clientv3.Version(a.releaseKey), "=", 0),
	}
	require.Equal(t, taskProtoComparisons(wantTail), request.Compare[52:])
	for i, key := range deleted {
		op := request.Success[i].GetRequestDeleteRange()
		require.NotNil(t, op)
		require.Equal(t, []byte(key), op.Key)
		require.Empty(t, op.RangeEnd)
		require.False(t, op.PrevKv)
		require.EqualValues(t, 1, success.Responses[i].GetResponseDeleteRange().Deleted)
	}
	put := request.Success[3].GetRequestPut()
	require.NotNil(t, put)
	require.Equal(t, []byte(a.releaseKey), put.Key)
	require.Equal(t, []byte(value), put.Value)
	require.Zero(t, put.Lease)
	require.False(t, put.PrevKv)
	require.NotNil(t, success.Responses[3].GetResponsePut())
	require.LessOrEqual(t, request.Size(), 128<<10)
	require.LessOrEqual(t, success.Size(), 256<<10)
	failure := taskProtoResponse(nil, nil)
	require.NoError(t, validateCleanupWire(cleanupWireProjection{name: "detached-owner-release", request: request, success: success, failure: failure}))
	t.Logf("v2 release literal63 nodes req=%d success=%d failure=%d; dimensional only", request.Size(), success.Size(), failure.Size())

	// A v2 projection cannot borrow the old local helper's 60/64 shape.
	_, _, _, err = a.releaseProjection(termination, deleted, value)
	require.Error(t, err)
	local := *a
	local.detached = nil
	_, _, _, err = local.detachedReleaseProjection(termination, deleted, value)
	require.Error(t, err)
	_, _, _, err = a.detachedReleaseProjection(termination, deleted, value+"x")
	require.Error(t, err)
	tooMany := *a
	tooMany.comparisons = append(append([]clientv3.Cmp(nil), a.comparisons...), clientv3.Compare(clientv3.ModRevision(a.providerKey), "=", 1))
	_, _, _, err = tooMany.detachedReleaseProjection(termination, deleted, value)
	require.Error(t, err, "literal shape is frozen; no hidden extra read/compare")
	huge := *a
	huge.comparisons = append([]clientv3.Cmp(nil), a.comparisons...)
	huge.comparisons[0] = clientv3.Compare(clientv3.Value(a.backend.identityKey), "=", strings.Repeat("x", 128<<10))
	_, _, _, err = huge.detachedReleaseProjection(termination, deleted, value)
	require.Error(t, err, "protobuf byte bounds are independent of the node count")
}
