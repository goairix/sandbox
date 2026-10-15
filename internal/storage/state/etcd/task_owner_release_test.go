package etcd

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestLocalRuntimeOwnerReleaseAdmission(t *testing.T) {
	for _, b := range []*Backend{nil, {}} {
		result, err := b.ReleaseTaskWorkspaceOwnerLocal(context.Background(), nil)
		require.Error(t, err)
		require.NotEqual(t, OutcomeCommitted, result.Outcome)
		_, err = b.LoadTaskOwnerRelease(context.Background(), TaskReference{})
		require.Error(t, err)
	}
}

func TestLocalRuntimeOwnerReleaseBudget(t *testing.T) {
	b, d := recyclingLocalDraftFixture(t)
	tk, _, rk, err := b.namespace.localRuntimeReleaseKeys(d.record.Reference.Task)
	require.NoError(t, err)
	_, complete, err := b.namespace.workspaceSyncLocalKeys(d.record.Reference.Task)
	require.NoError(t, err)
	prefix, err := b.taskOperationsPrefix(d.record.Reference.Task)
	require.NoError(t, err)
	a := &taskLocalRuntimeAuthority{backend: b, prefix: prefix, releaseKey: rk, complete: taskFence{key: complete, value: strings.Repeat("c", 8192), create: 1, mod: 1}}
	for i := 0; i < 50; i++ {
		a.comparisons = append(a.comparisons, clientv3.Compare(clientv3.Value(b.namespace.Root()+fmt.Sprintf("authority-%02d", i)), "=", strings.Repeat("x", 512)))
	}
	termination := &mvccpb.KeyValue{Key: []byte(tk), Value: []byte(strings.Repeat("t", 16384)), Version: 3, CreateRevision: 2, ModRevision: 4}
	cmps, request, success, err := a.releaseProjection(termination, [3]string{b.namespace.Root() + "owner", b.namespace.Root() + "control", b.namespace.Root() + "index"}, strings.Repeat("r", 8192))
	require.NoError(t, err)
	require.Len(t, cmps, 60)
	require.Equal(t, 64, len(request.Compare)+len(request.Success)+len(request.Failure))
	require.LessOrEqual(t, request.Size(), 262144)
	require.LessOrEqual(t, success.Size(), 262144)
	t.Logf("release nodes=64 compares=60 request_bytes=%d success_bytes=%d failure_bytes=%d", request.Size(), success.Size(), taskProtoResponse(nil, nil).Size())
}

func TestLocalRuntimeOwnerReleaseRecord(t *testing.T) {
	r := localReleaseTerminationFixture(t)
	r.Phase, r.TerminalProof, r.CleanupDigest = "cleanup-complete", []byte("codec-only"), strings.Repeat("e", 64)
	task := TaskReference{Namespace: r.Current.Namespace, RestoreEpoch: r.Current.RestoreEpoch, TaskID: r.Current.TaskID, SandboxID: r.Current.SandboxID, Partition: r.Partition}
	claim := taskCloseClaimWire{Task: task, ClaimID: task.TaskID, WorkerID: "worker", CreateRevision: 99, LeaseID: 2}
	receipt, err := newLocalOwnerReleaseRecord(r, 100, 103, claim)
	require.NoError(t, err)
	wire, err := encodeLocalOwnerRelease(receipt)
	require.NoError(t, err)
	require.LessOrEqual(t, len(wire), 8192)
	kv := &mvccpb.KeyValue{Value: []byte(wire), Version: 1, CreateRevision: 104, ModRevision: 104}
	got, err := decodeLocalOwnerRelease(kv)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	for _, bad := range []string{" " + wire, strings.Replace(wire, `"version":1`, `"version":1,"version":1`, 1), strings.Replace(wire, `"version":1`, `"version":1,"unknown":true`, 1)} {
		copy := *kv
		copy.Value = []byte(bad)
		_, err := decodeLocalOwnerRelease(&copy)
		require.Error(t, err)
	}
	for _, change := range []func(*mvccpb.KeyValue){func(k *mvccpb.KeyValue) { k.Lease = 1 }, func(k *mvccpb.KeyValue) { k.Version++ }, func(k *mvccpb.KeyValue) { k.ModRevision++ }} {
		copy := *kv
		change(&copy)
		_, err := decodeLocalOwnerRelease(&copy)
		require.Error(t, err)
	}
	r.Phase, r.CleanupDigest = "terminated", ""
	_, err = newLocalOwnerReleaseRecord(r, 100, 103, claim)
	require.Error(t, err)
}
