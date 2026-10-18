//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Root/B installs the only optional hook after the original owner-release
// fixture's zero-history-index-read assertions. This helper consumes its real
// observer proof/seven slots/native release; it creates no source or proof.
// The host transcript is never promoted to a detached positive fixture.
func detachedPhysicalReleasedRead(t *testing.T, f *cleanupDetachedNoEffectNativeFixture) {
	t.Helper()
	require.Equal(t, "1", os.Getenv("SANDBOX_DETACHED_TERMINAL_PHYSICAL_NATIVE"))
	require.Equal(t, "1", os.Getenv("SANDBOX_DETACHED_OWNER_RELEASE_PHYSICAL_NATIVE"))
	b, claim := f.b, f.job.claim
	require.NotNil(t, claim)
	claim.mu.Lock()
	lost, attempt := claim.lost, claim.ownerReleaseAttempt
	claim.mu.Unlock()
	require.True(t, lost, "terminal observation must not borrow a live TaskClaim")
	require.NotNil(t, attempt)
	select {
	case <-attempt.done:
	default:
		t.Fatal("actual release owner has not returned and joined")
	}

	// B's accepted standalone loader supplies an independent real precondition
	// outside the public API's eight-RPC count. It also proves this invocation
	// starts after the actual seal and complete signed surviving history exist.
	reader, err := b.beginLocalRuntimeRead(f.ctx, f.ref)
	require.NoError(t, err)
	proof, err := b.readTaskOwnerReleaseProof(reader.ctx, f.ref, nil)
	reader.finish()
	require.NoError(t, err)
	require.NotNil(t, proof)
	require.Nil(t, proof.local)
	require.NotNil(t, proof.detached)
	actual := proof.detached
	require.NotNil(t, actual.history)
	require.Equal(t, f.ref, actual.record.Task)
	require.Equal(t, "cleanup-complete", actual.termination.Phase)
	index, err := b.namespace.runtimeIndexKey(actual.record.Runtime.UID)
	require.NoError(t, err)
	t.Log("actual detached physical proof, seven complete provider slots, native owner release and dead original Claim verified before terminal API")

	original := b.client.KV
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	issuer := b.taskRuntimeTerminationIssuer.(*detachedTerminationFixtureIssuer)
	certificates, signatures := issuer.certificates.Load(), issuer.signatures.Load()
	defer func() {
		b.client.KV = original
		b.client.Lease = lease.Lease
	}()
	var requests []*pb.TxnRequest
	var replyBytes []int
	pointsRead := make(map[string]int)
	b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, comparisons []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		branches := make([][]string, 2)
		for branch, ops := range [][]clientv3.Op{yes, no} {
			for _, op := range ops {
				require.True(t, op.IsGet(), "terminal observation sends no native write or nested transaction")
				key := string(op.KeyBytes())
				require.True(t, reflect.DeepEqual(clientv3.OpGet(key), op), "every point is a current linearizable Get without options")
				branches[branch] = append(branches[branch], key)
				pointsRead[key]++
			}
		}
		request := &pb.TxnRequest{
			Compare: taskProtoComparisons(comparisons),
			Success: taskProtoPoints(branches[0]), Failure: taskProtoPoints(branches[1]),
		}
		require.LessOrEqual(t, len(request.Compare)+len(request.Success)+len(request.Failure), 64)
		require.LessOrEqual(t, request.Size(), 128<<10)
		response, err := original.Txn(ctx).If(comparisons...).Then(yes...).Else(no...).Commit()
		require.NoError(t, err)
		require.NotNil(t, response)
		require.True(t, response.Succeeded)
		require.LessOrEqual(t, (*pb.TxnResponse)(response).Size(), 256<<10)
		requests = append(requests, request)
		replyBytes = append(replyBytes, (*pb.TxnResponse)(response).Size())
		return response, nil
	}}
	result, err := b.ReadSandboxReleased(f.ctx, f.ref.SandboxID)
	require.NoError(t, err, "real detached native release must reach the public terminal consumer")
	require.NotNil(t, result)
	require.Equal(t, f.ref, result.Task)
	require.Equal(t, f.ref.SandboxID, result.SandboxID)
	require.GreaterOrEqual(t, result.Revision, actual.kv.CreateRevision)
	require.Len(t, requests, 8, "the independent precondition reads are outside this count")
	for i, cardinality := range []int{1, 4, 1, 4, 1, 2, 13, 5} {
		require.Len(t, requests[i].Success, cardinality, "RPC %d", i+1)
	}
	total := 0
	for i, size := range replyBytes {
		total += size
		t.Logf("actual detached terminal RPC%d nodes=%d request=%d reply=%d", i+1, len(requests[i].Compare)+len(requests[i].Success)+len(requests[i].Failure), requests[i].Size(), size)
	}
	require.LessOrEqual(t, total, 512<<10)
	require.Equal(t, 1, pointsRead[index], "only final absence confirmation reads the deleted runtime index")
	require.Zero(t, pointsRead[f.job.state.execution.key])
	require.Zero(t, pointsRead[f.job.state.slot.key])
	require.Zero(t, lease.grants.Load())
	require.Equal(t, certificates, issuer.certificates.Load())
	require.Equal(t, signatures, issuer.signatures.Load())

	// The actual final transaction retains every complete native source,
	// including all four companions. Task repeats are deduplicated, giving
	// twenty-one pins without a successor owner; the finite maximum is22.
	mods := make(map[string]int64)
	for _, comparison := range requests[7].Compare {
		if comparison.Target == pb.Compare_MOD {
			mods[string(comparison.Key)] = comparison.GetModRevision()
		}
	}
	require.Len(t, mods, 21)
	retained := append([]*mvccpb.KeyValue{actual.kv, actual.history.termination, actual.history.provider}, actual.history.history[:]...)
	for _, point := range retained {
		require.Equal(t, point.ModRevision, mods[string(point.Key)], "no detached source/companion may escape final pinning")
	}

	// Stop counting before independent postreads, then compare the actual
	// native bytes/envelopes, not only projected receipt or release DTOs.
	b.client.KV = original
	historyKeys := make([]string, len(actual.history.history))
	for i, point := range actual.history.history {
		historyKeys[i] = string(point.Key)
	}
	after, err := b.readEnrolledPoints(f.ctx, historyKeys, detachedRuntimeHistoryCaps())
	require.NoError(t, err)
	require.Equal(t, actual.history.history[:], after)
	headKeys := []string{string(actual.kv.Key), string(actual.history.termination.Key), string(actual.history.provider.Key)}
	head, err := b.readEnrolledPoints(f.ctx, headKeys, []int{8192, 16384, 16384})
	require.NoError(t, err)
	require.Equal(t, retained[:3], head)
	t.Logf("actual detached terminal eight-RPC replies=%d; readonly21 final pins, no live Claim/issuer/provider/quota/admission authority", total)
}
