package etcd

import (
	"fmt"
	"strings"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// All inputs here are routing/codec models. The unsigned terminal bytes and
// dimensional native stamps must never enter a positive release capability.
func detachedOwnerHistoryModel(t *testing.T) (*Backend, p.TaskRuntimeTerminationIntent, *mvccpb.KeyValue) {
	t.Helper()
	b, input := cleanupLocalWireFixture(t, true)
	task := input.bundle.task
	key, err := b.namespace.taskKey(task.Reference)
	require.NoError(t, err)
	wire, err := encodeTaskRecord(task)
	require.NoError(t, err)
	actual := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), CreateRevision: input.bundle.birth, ModRevision: input.bundle.birth, Version: 1}
	r := detachedOwnerReleaseCodecIntent(t)
	r.Current.Namespace, r.Current.RestoreEpoch, r.Current.SandboxID, r.Current.TaskID = task.Reference.Namespace, task.Reference.RestoreEpoch, task.Reference.SandboxID, task.Reference.TaskID
	r.Current.WorkspaceHash, r.Current.Runtime = task.WorkspaceHash, p.RuntimeReference(task.Runtime)
	r.Current.Generation, r.Current.DataGateEpoch = task.Generation, task.DataGateEpoch
	r.Current.ControlRevision, r.Current.ClaimCreateRevision = input.bundle.birth, input.bundle.birth+1
	r.Current.TaskDigest = workspaceSyncIntentDigest(wire)
	r.Partition, r.TaskBirth, r.CreationIntentID = task.Reference.Partition, input.bundle.birth, task.CreationIntentID
	r.Authority.Task = localRuntimeStamp(actual)
	_, err = p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	return b, r, actual
}

// Independent expected routing uses the already accepted namespace/key codecs,
// not the new profile dispatcher. It contains no current index or execution.
func detachedOwnerHistoryModelKeys(t *testing.T, b *Backend, r p.TaskRuntimeTerminationIntent, actualTask *mvccpb.KeyValue) []string {
	t.Helper()
	var task TaskRecord
	require.NoError(t, decodeTaskRecord(actualTask, &task))
	keys, err := p.DetachedTaskRuntimeTerminationKeysFor(r)
	require.NoError(t, err)
	_, snapshot, err := b.namespace.sandboxKeys(r.Partition, task.Reference.SandboxID, task.Snapshot.Version)
	require.NoError(t, err)
	publication, proof, err := b.namespace.runtimePublicationKeys(r.Partition, r.CreationIntentID)
	require.NoError(t, err)
	enrollment, activation, err := b.namespace.runtimeEnrollmentKeys(r.Partition, r.CreationIntentID)
	require.NoError(t, err)
	return []string{keys.Task, keys.Quiescence, keys.QuiescenceReceipt, keys.QuiescenceReservation, keys.FinalSync, keys.Issuer, keys.Settlement, snapshot, keys.CreationIntent, publication, proof, enrollment, activation}
}

func TestDetachedOwnerReleaseHistoryProfileDispatch(t *testing.T) {
	_, r, _ := detachedOwnerHistoryModel(t)
	claim := taskCloseClaimWire{Task: localRuntimeTerminationTask(r), ClaimID: r.Current.ClaimID, WorkerID: "release", CreateRevision: 110, LeaseID: 99}
	seal, err := newDetachedOwnerReleaseRecord(r, 100, 103, claim)
	require.NoError(t, err)
	wire, err := encodeDetachedOwnerRelease(seal)
	require.NoError(t, err)
	key := claim.Task.Namespace + fmt.Sprintf("p/%02x/tasks/%s/owner-release", claim.Task.Partition, claim.Task.TaskID)
	kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), Version: 1, CreateRevision: 111, ModRevision: 111}
	profile, err := decodeTaskOwnerReleaseProfile(claim.Task, kv)
	require.NoError(t, err)
	require.Equal(t, taskOwnerReleaseDetached, profile)

	local := r
	local.Version, local.DetachedEffects = 1, nil
	local.Authority.Archive, local.Authority.LocalComplete = r.Authority.Task, r.Authority.Task
	legacy, err := newLocalOwnerReleaseRecord(local, 100, 103, claim)
	require.NoError(t, err)
	legacyWire, err := encodeLocalOwnerRelease(legacy)
	require.NoError(t, err)
	legacyKV := *kv
	legacyKV.Value = []byte(legacyWire)
	profile, err = decodeTaskOwnerReleaseProfile(claim.Task, &legacyKV)
	require.NoError(t, err)
	require.Equal(t, taskOwnerReleaseLocal, profile)
	_, err = decodeDetachedOwnerRelease(&legacyKV)
	require.Error(t, err)
	_, err = decodeLocalOwnerRelease(kv)
	require.Error(t, err)
	foreign := claim.Task
	foreign.SandboxID = "foreign"
	_, err = decodeTaskOwnerReleaseProfile(foreign, kv)
	require.Error(t, err)
	bad := *kv
	bad.Value = []byte(strings.Replace(wire, `"version":2`, `"version":3`, 1))
	_, err = decodeTaskOwnerReleaseProfile(claim.Task, &bad)
	require.Error(t, err)
}

func TestDetachedOwnerReleaseHistoryLiteral13Points(t *testing.T) {
	b, r, actual := detachedOwnerHistoryModel(t)
	want := detachedOwnerHistoryModelKeys(t, b, r, actual)
	keys, caps, err := b.taskOwnerReleaseHistoryKeys(r, actual)
	require.NoError(t, err)
	require.Equal(t, want, keys)
	require.Equal(t, []int{4096, 32768, 4096, 8192, 8192, 8192, 32768, 65536, 4096, 8192, 16384, 8192, 8192}, caps)
	require.Len(t, keys, 13)
	for _, key := range keys {
		require.NotContains(t, key, "indexes/runtime/")
		require.NotContains(t, key, "cleanup-execution")
	}
	for _, mutate := range []func(*mvccpb.KeyValue){
		func(v *mvccpb.KeyValue) { v.Lease = 1 },
		func(v *mvccpb.KeyValue) {
			v.CreateRevision++
			v.ModRevision++
		},
		func(v *mvccpb.KeyValue) { v.Key = []byte(string(v.Key) + "-foreign") },
		func(v *mvccpb.KeyValue) { v.Value = append([]byte(" "), v.Value...) },
	} {
		bad := *actual
		mutate(&bad)
		_, _, err = b.taskOwnerReleaseHistoryKeys(r, &bad)
		require.Error(t, err)
	}
	_, _, err = b.taskOwnerReleaseHistoryKeys(r, nil)
	require.Error(t, err, "v2 snapshot routing must have a real Task point")
	local := r
	local.Version, local.DetachedEffects = 1, nil
	local.Authority.Archive, local.Authority.LocalComplete = r.Authority.Task, r.Authority.Task
	localKeys, localCaps, err := b.taskOwnerReleaseHistoryKeys(local, actual)
	require.NoError(t, err)
	wantLocal, err := b.localRuntimeHistoryKeys(local, localRuntimeTerminationTask(local))
	require.NoError(t, err)
	require.Equal(t, wantLocal, localKeys)
	require.Equal(t, localRuntimeHistoryCaps(), localCaps)
	require.Len(t, localKeys, 9)
}

func TestDetachedOwnerReleaseHistoryFullCapWire(t *testing.T) {
	b, r, actual := detachedOwnerHistoryModel(t)
	keys, caps, err := b.taskOwnerReleaseHistoryKeys(r, actual)
	require.NoError(t, err)
	all := append([]string{b.identityKey, b.restoreKey}, keys...)
	values := []string{b.identityValue, b.restoreEpoch}
	for _, cap := range caps {
		values = append(values, strings.Repeat("x", cap))
	}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(b.baseComparisons()), Success: taskProtoPoints(all), Failure: taskProtoPoints(all)}
	reply := taskProtoResponse(all, values)
	require.Equal(t, 34, len(request.Compare)+len(request.Success)+len(request.Failure))
	require.NoError(t, validateCleanupWire(cleanupWireProjection{name: "detached-owner-history", request: request, success: reply, failure: reply}))
	require.LessOrEqual(t, request.Size(), 128<<10)
	require.LessOrEqual(t, reply.Size(), 256<<10)
	t.Logf("13 point transport component only: nodes34 request=%d full-cap-reply=%d", request.Size(), reply.Size())
}

func TestDetachedOwnerReleaseHistoryRejectsUnverifiedGraph(t *testing.T) {
	b, r, task := detachedOwnerHistoryModel(t)
	claim := taskCloseClaimWire{Task: localRuntimeTerminationTask(r), ClaimID: r.Current.ClaimID, WorkerID: "release", CreateRevision: 110, LeaseID: 99}
	model, term := detachedOwnerReleaseCodecModel(t, r, 100, 103, claim)
	wire, err := encodeDetachedOwnerRelease(model)
	require.NoError(t, err)
	seal := &mvccpb.KeyValue{Key: []byte(strings.TrimSuffix(string(term.Key), "runtime-termination") + "owner-release"), Value: []byte(wire), Version: 1, CreateRevision: 111, ModRevision: 111}
	proof, err := b.decodeTaskOwnerReleaseProof(claim.Task, task, seal, term, nil, make([]*mvccpb.KeyValue, 13))
	require.Error(t, err)
	require.Nil(t, proof, "codec-valid seal and unsigned terminal bytes cannot manufacture historical authority")
	_, err = b.decodeDetachedRuntimeHistory(claim.Task, task, &detachedRuntimeHistorySnapshot{termination: term})
	require.Error(t, err)
}
