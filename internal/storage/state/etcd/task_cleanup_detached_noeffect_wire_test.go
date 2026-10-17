package etcd

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Dimensional fences intentionally have no client, Kernel, Claim lease or
// registered job. This exercises the exact production native wire builder,
// never commit authority or a fabricated positive physical consumer.
func TestCleanupDetachedNoEffectWriteFitsLiteral64Nodes(t *testing.T) {
	b, input := cleanupLocalWireFixture(t, true)
	c, _, err := b.prepareTaskClaim(context.Background(), input.bundle, input.workerID, 24*time.Hour, "ffffffff-ffff-4fff-bfff-ffffffffffff")
	require.NoError(t, err)
	ref := input.bundle.task.Reference
	qkey, err := b.namespace.taskQuiescenceKey(ref)
	require.NoError(t, err)
	receiptKey := ref.Namespace + "p/00/stages/request/task_quiesce_users/ffffffff-ffff-4fff-bfff-ffffffffffff/receipt"
	reservationKey, err := b.namespace.taskQuiescenceAttemptKey(ref)
	require.NoError(t, err)
	cmps := taskEmptyComparisons(b, c, cleanupWireFence(qkey, 16384), cleanupWireFence(receiptKey, 4096), taskQuiescenceReservationPin{key: reservationKey, revision: math.MaxInt64})
	require.Len(t, cmps, 53)
	key := ref.Namespace + "p/00/tasks/" + ref.TaskID + "/detached-noeffect"
	for _, suffix := range []string{"execution", "occupation", "none", "snapshot", "creation-intent", "publication"} {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(ref.Namespace+suffix), "=", math.MaxInt64))
	}
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))

	// Account the full new record cap rather than a convenient short fixture.
	value := strings.Repeat("x", 32768)
	projection, err := b.detachedNoEffectWriteProjection(cmps, key, value)
	require.NoError(t, err)
	require.Len(t, projection.request.Compare, 60)
	require.Len(t, projection.request.Success, 1)
	require.Len(t, projection.request.Failure, 3)
	require.Equal(t, 64, len(projection.request.Compare)+len(projection.request.Success)+len(projection.request.Failure))
	put := projection.request.Success[0].GetRequestPut()
	require.NotNil(t, put)
	require.Equal(t, []byte(key), put.Key)
	require.Equal(t, []byte(value), put.Value)
	require.Zero(t, put.Lease)
	for i, want := range []string{b.identityKey, b.restoreKey, key} {
		require.Equal(t, []byte(want), projection.request.Failure[i].GetRequestRange().Key)
	}
	require.LessOrEqual(t, projection.request.Size(), 128<<10)
	require.LessOrEqual(t, projection.success.Size(), 256<<10)
	require.LessOrEqual(t, projection.failure.Size(), 256<<10)
	require.NoError(t, validateCleanupWire(projection))
	t.Logf("detached-noeffect native nodes=64 request=%d success=%d failure=%d", projection.request.Size(), projection.success.Size(), projection.failure.Size())

	// The old reservation pre-read duplicates three points in each branch:
	// 59 comparisons + Then3 + Else3 = 65. A no-effect writer cannot use it.
	oldRead := projection
	request := *projection.request
	oldRead.request = &request
	oldRead.request.Compare = oldRead.request.Compare[:59]
	oldRead.request.Success = taskProtoPoints([]string{b.identityKey, b.restoreKey, key})
	oldRead.success = projection.failure
	require.Error(t, validateCleanupWire(oldRead))
	_, err = b.detachedNoEffectWriteProjection(cmps, key, value+"x")
	require.Error(t, err, "a larger settlement must be rejected before native dispatch")

	// Doubling the comparison bytes is a real protobuf-size violation even
	// though the literal node shape itself remains unchanged.
	huge := append([]clientv3.Cmp(nil), cmps...)
	huge[0] = clientv3.Compare(clientv3.Value(b.identityKey), "=", strings.Repeat("x", 128<<10))
	_, err = b.detachedNoEffectWriteProjection(huge, key, value)
	require.Error(t, err)
}

func TestCleanupDetachedNoEffectSourceAndArbitrationPointBounds(t *testing.T) {
	b, input := cleanupLocalWireFixture(t, true)
	ref := input.bundle.task.Reference
	key := ref.Namespace + "p/00/tasks/" + ref.TaskID + "/detached-noeffect"
	keys := []string{b.identityKey, b.restoreKey, ref.Namespace + "execution", ref.Namespace + "occupation", ref.Namespace + "none", ref.Namespace + "snapshot", ref.Namespace + "creation-intent", ref.Namespace + "publication", key}
	values := []string{b.identityValue, b.restoreEpoch, strings.Repeat("e", 4096), strings.Repeat("s", 4096), strings.Repeat("f", 8192), strings.Repeat("s", 65536), strings.Repeat("i", 4096), strings.Repeat("p", 8192), strings.Repeat("n", 32768)}
	for _, scenario := range []struct {
		name         string
		keys, values []string
		nodes        int
	}{{"source", keys, values, 22}, {"arbitration", []string{b.identityKey, b.restoreKey, key}, []string{b.identityValue, b.restoreEpoch, values[8]}, 10}} {
		t.Run(scenario.name, func(t *testing.T) {
			projection, err := b.detachedNoEffectPointProjection(scenario.name, scenario.keys, scenario.values)
			require.NoError(t, err)
			require.Equal(t, scenario.nodes, len(projection.request.Compare)+len(projection.request.Success)+len(projection.request.Failure))
			require.LessOrEqual(t, projection.request.Size(), 128<<10)
			require.LessOrEqual(t, projection.success.Size(), 256<<10)
			require.LessOrEqual(t, projection.failure.Size(), 256<<10)
			require.NoError(t, validateCleanupWire(projection))
			for _, op := range append(append([]*pb.RequestOp(nil), projection.request.Success...), projection.request.Failure...) {
				require.NotNil(t, op.GetRequestRange())
			}
			t.Logf("detached-noeffect %s nodes=%d request=%d response=%d", scenario.name, scenario.nodes, projection.request.Size(), projection.success.Size())
		})
	}
}

func TestCleanupDetachedNoEffectForeignSettlementReplyUsesRecordCap(t *testing.T) {
	b, input := cleanupLocalWireFixture(t)
	ref := input.bundle.task.Reference
	key := ref.Namespace + "p/00/tasks/" + ref.TaskID + "/detached-noeffect"
	cmps := append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	projection, err := b.detachedNoEffectWriteProjection(cmps, key, "small submitted draft")
	require.NoError(t, err)
	// A failed CAS may observe an unrelated maximal record. This cap is only
	// conservative native budget coverage; that foreign value grants no effect
	// permission and must still be rejected by exact original-draft arbitration.
	point := projection.failure.Responses[2].GetResponseRange()
	require.Len(t, point.Kvs, 1)
	require.Len(t, point.Kvs[0].Value, 32768, "a short submitted value cannot bound an existing foreign settlement")
	require.Equal(t, []byte("small submitted draft"), projection.request.Success[0].GetRequestPut().Value)
	require.LessOrEqual(t, projection.failure.Size(), 256<<10)
}
