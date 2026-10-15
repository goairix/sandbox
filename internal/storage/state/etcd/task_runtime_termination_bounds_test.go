package etcd

import (
	"fmt"
	"math"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These are conservative fixed-codec/protobuf projections. They do not claim
// that every independently maximal input is simultaneously admissible, or
// that these synthetic upper bounds were sent to native etcd.
func TestLocalRuntimeReleaseMaximumWire(t *testing.T) {
	n, err := NewNamespace("/"+strings.Repeat("p", 498), "observer", "v1")
	require.NoError(t, err)
	require.Len(t, n.Root(), 512)
	key, err := n.Key(strings.Repeat("k", 512))
	require.NoError(t, err)
	require.Len(t, key, 1024)
	_, err = n.Key(strings.Repeat("k", 513))
	require.Error(t, err)
	b, d := recyclingLocalDraftFixture(t)
	b.namespace = n
	task := d.record.Reference.Task
	task.Namespace = n.Root()
	termination, provider, release, err := n.localRuntimeReleaseKeys(task)
	require.NoError(t, err)
	_, complete, err := n.workspaceSyncLocalKeys(task)
	require.NoError(t, err)
	prefix, err := b.taskOperationsPrefix(task)
	require.NoError(t, err)
	a := &taskLocalRuntimeAuthority{backend: b, prefix: prefix, terminationKey: termination, providerKey: provider, releaseKey: release, complete: taskFence{key: complete, value: strings.Repeat("c", 8192), create: math.MaxInt64, mod: math.MaxInt64}}
	// Eight domain values, two claim values, identity and restore, plus scalar
	// pins total the same fixed50 baseline. Each comparison key uses1024 bytes.
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("%02d", i) + strings.Repeat("k", 510)
		k, err := n.Key(name)
		require.NoError(t, err)
		if i < 12 {
			a.comparisons = append(a.comparisons, clientv3.Compare(clientv3.Value(k), "=", strings.Repeat("v", 8192)))
		} else {
			a.comparisons = append(a.comparisons, clientv3.Compare(clientv3.ModRevision(k), "=", math.MaxInt64))
		}
	}
	a.enrollment = [2]taskFence{{key: key, mod: math.MaxInt64}, {key: key, mod: math.MaxInt64}}
	pv := &mvccpb.KeyValue{Key: []byte(provider), Value: []byte(strings.Repeat("p", 16384)), CreateRevision: math.MaxInt64 - 1, ModRevision: math.MaxInt64, Version: 2}
	tv := &mvccpb.KeyValue{Key: []byte(termination), Value: []byte(strings.Repeat("t", 16384)), CreateRevision: math.MaxInt64 - 1, ModRevision: math.MaxInt64, Version: 2}
	for _, c := range []struct {
		name  string
		cmps  []clientv3.Cmp
		nodes int
	}{{"reserve", a.terminationReserveComparisons(pv), 60}, {"terminated", a.terminationComparisons(tv, pv), 61}, {"provider", a.providerComparisons(pv, tv), 61}} {
		request := &pb.TxnRequest{Compare: taskProtoComparisons(c.cmps), Success: taskProtoWrites([]Write{{Key: termination, Value: tv.Value}})}
		success, failure := taskProtoResponse([]string{termination}, []string{string(tv.Value)}), taskProtoResponse(nil, nil)
		require.Equal(t, c.nodes, len(request.Compare)+len(request.Success)+len(request.Failure))
		require.NoError(t, taskCheckWire(n, []*pb.TxnRequest{request}, []*pb.TxnResponse{success, failure}))
		t.Logf("projection %s root512/key1024 nodes=%d request=%d success=%d failure=%d", c.name, c.nodes, request.Size(), success.Size(), failure.Size())
	}
	_, request, success, err := a.releaseProjection(tv, [3]string{key, key, key}, strings.Repeat("r", 8192))
	require.NoError(t, err)
	failure := taskProtoResponse(nil, nil)
	require.NoError(t, taskCheckWire(n, []*pb.TxnRequest{request}, []*pb.TxnResponse{success, failure}))
	require.Equal(t, 64, len(request.Compare)+len(request.Success)+len(request.Failure))
	t.Logf("projection release root512/key1024 nodes64 request=%d success=%d failure=%d", request.Size(), success.Size(), failure.Size())
	oversize := taskProtoResponse([]string{key}, []string{strings.Repeat("x", 262144)})
	require.Error(t, taskCheckWire(n, nil, []*pb.TxnResponse{oversize, failure}), "success independently bounded")
	require.Error(t, taskCheckWire(n, nil, []*pb.TxnResponse{success, oversize}), "failure independently bounded")
	tooLarge := &pb.TxnRequest{Success: taskProtoWrites([]Write{{Key: key, Value: []byte(strings.Repeat("x", 262144))}})}
	require.Error(t, taskCheckWire(n, []*pb.TxnRequest{tooLarge}, nil))

	r := localReleaseTerminationFixture(t)
	r.Current.Namespace = n.Root()
	r.Current.AuthorityID, r.Current.Target, r.Current.WorkerID, r.Current.Runtime.ID = strings.Repeat("a", 128), strings.Repeat("t", 128), strings.Repeat("w", 128), strings.Repeat("r", 128)
	r.Current.Generation, r.Current.DataGateEpoch, r.Current.ControlRevision, r.Current.ClaimCreateRevision, r.Current.LeaseID = math.MaxInt64, math.MaxInt64, math.MaxInt64-3, math.MaxInt64-2, math.MaxInt64
	require.NoError(t, a.preflightTerminationPhases(r, pv), "both future4096 proof phases checked before effects")
	r.Phase, r.TerminalProof, r.CleanupDigest = "cleanup-complete", make([]byte, 4096), strings.Repeat("f", 64)
	wire, err := p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 16384)
	claim := taskCloseClaimWire{Task: localRuntimeTerminationTask(r), ClaimID: r.Current.ClaimID, WorkerID: strings.Repeat("w", 128), CreateRevision: math.MaxInt64 - 2, LeaseID: math.MaxInt64}
	receipt, err := newLocalOwnerReleaseRecord(r, math.MaxInt64-1, math.MaxInt64, claim)
	require.NoError(t, err)
	rw, err := encodeLocalOwnerRelease(receipt)
	require.NoError(t, err)
	require.LessOrEqual(t, len(rw), 8192)
	t.Logf("canonical root512 max scalar/opaque fields proof4096 termination=%d receipt=%d", len(wire), len(rw))
	r.TerminalProof = make([]byte, 4097)
	_, err = p.EncodeTaskRuntimeTerminationIntent(r)
	require.Error(t, err)
	providerRecord := localReleaseRecordFixture(t)
	providerRecord.Task.Namespace, providerRecord.Binding.Authority.Namespace = n.Root(), n.Root()
	providerRecord.Descriptor = make([]byte, 16384)
	_, err = encodeTaskRuntimeProviderCleanup(providerRecord)
	require.Error(t, err, "independent opaque descriptor ceiling cannot bypass encoded record16KiB cap")
}
