package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func drainPageFixture(t *testing.T, count int) (*Backend, TaskRecord, int64, string, *clientv3.TxnResponse) {
	t.Helper()
	r := quiesceRecordFixture(t)
	n := namespaceFromTaskCloseTest(t, r.Task.Reference)
	b := &Backend{namespace: n, clusterID: 7, restoreEpoch: r.Task.Reference.RestoreEpoch}
	prefix, err := b.taskOperationsPrefix(r.Task.Reference)
	require.NoError(t, err)
	birth := r.Context.Current.ControlRevision + 10
	out := &pb.RangeResponse{Count: int64(count)}
	for i := 0; i < count && i < 16; i++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1)
		op := OperationRecord{Version: 1, Reference: OperationReference{Namespace: n.Root(), RestoreEpoch: r.Task.Reference.RestoreEpoch, RequestID: uuid.NewString(), SandboxID: r.Task.Reference.SandboxID, OperationID: id, Digest: r.TicketDigest, Partition: r.Task.Reference.Partition, Kind: OperationData, LeaseID: 8}, WorkspaceHash: r.Task.WorkspaceHash, IntentID: r.Task.CreationIntentID, Generation: r.Task.Generation, DataGateEpoch: r.Task.DataGateEpoch, ControlRevision: r.Task.ControlRevision, Runtime: r.Task.Runtime, Snapshot: r.Task.Snapshot, ExpiresAt: r.Task.ExpiresAt}
		wire, err := encodeOperationRecord(op)
		require.NoError(t, err)
		out.Kvs = append(out.Kvs, &mvccpb.KeyValue{Key: []byte(prefix + id), Value: []byte(wire), Lease: 8, CreateRevision: birth - 1, ModRevision: birth - 1})
	}
	out.More = count > 16
	return b, r.Task, birth, prefix, &clientv3.TxnResponse{Header: &pb.ResponseHeader{ClusterId: 7, Revision: birth + 20}, Succeeded: true, Responses: []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: out}}}}
}
func TestTaskOperationDrainPagesShape(t *testing.T) {
	b, task, birth, prefix, res := drainPageFixture(t, 19)
	page, err := b.decodeTaskOperationsPage(res, task, birth, prefix, prefix, 16, 0)
	require.NoError(t, err)
	require.Len(t, page.Entries, 16)
	require.EqualValues(t, 19, page.Count)
	require.True(t, page.More)
	original := append([]byte{}, page.Entries[0].Value...)
	clear(res.Responses[0].GetResponseRange().Kvs[0].Value)
	require.Equal(t, original, page.Entries[0].Value)
	for _, name := range []string{"negative_count", "count_below_rows", "more_false", "more_true_short", "too_many", "nil_kv", "foreign_cluster", "regression", "nested_header", "outside", "reverse", "lease", "mutable", "after_birth", "future_control", "same_revision_snapshot", "oversize"} {
		t.Run(name, func(t *testing.T) {
			b, task, birth, prefix, res := drainPageFixture(t, 19)
			r := res.Responses[0].GetResponseRange()
			min := int64(0)
			switch name {
			case "negative_count":
				r.Count = -1
			case "count_below_rows":
				r.Count = 15
			case "more_false":
				r.More = false
			case "more_true_short":
				r.Kvs = r.Kvs[:15]
			case "too_many":
				r.Kvs = append(r.Kvs, r.Kvs[0])
			case "nil_kv":
				r.Kvs[0] = nil
			case "foreign_cluster":
				res.Header.ClusterId++
			case "regression":
				min = res.Header.Revision + 1
			case "nested_header":
				r.Header = &pb.ResponseHeader{ClusterId: 8, Revision: res.Header.Revision}
			case "outside":
				r.Kvs[0].Key = []byte("foreign")
			case "reverse":
				r.Kvs[0], r.Kvs[1] = r.Kvs[1], r.Kvs[0]
			case "lease":
				r.Kvs[0].Lease++
			case "mutable":
				r.Kvs[0].ModRevision++
			case "after_birth":
				r.Kvs[0].CreateRevision = birth
				r.Kvs[0].ModRevision = birth
			case "future_control", "same_revision_snapshot":
				var o OperationRecord
				require.NoError(t, json.Unmarshal(r.Kvs[0].Value, &o))
				if name == "future_control" {
					o.ControlRevision = task.ControlRevision + 1
				} else {
					o.ExpiresAt = o.ExpiresAt.Add(1)
				}
				w, e := encodeOperationRecord(o)
				require.NoError(t, e)
				r.Kvs[0].Value = []byte(w)
			case "oversize":
				r.Kvs[0].Value = make([]byte, 80*1024)
			}
			page, err := b.decodeTaskOperationsPage(res, task, birth, prefix, prefix, 16, min)
			require.Error(t, err)
			require.Empty(t, page)
		})
	}
}
func TestTaskOperationDrainPagesHistorical(t *testing.T) {
	b, task, birth, prefix, res := drainPageFixture(t, 1)
	var op OperationRecord
	kv := res.Responses[0].GetResponseRange().Kvs[0]
	require.NoError(t, json.Unmarshal(kv.Value, &op))
	op.ControlRevision--
	op.ExpiresAt = op.ExpiresAt.Add(-1)
	w, err := encodeOperationRecord(op)
	require.NoError(t, err)
	kv.Value = []byte(w)
	page, err := b.decodeTaskOperationsPage(res, task, birth, prefix, prefix, 16, 0)
	require.NoError(t, err)
	require.Equal(t, op, page.Entries[0].Record)
	for _, c := range []*TaskClaim{nil, {}} {
		p, e := b.ObserveTaskOperationsPage(context.Background(), c, nil)
		require.ErrorIs(t, e, ErrInvalidRecord)
		require.Empty(t, p)
	}
}

func TestTaskOperationDrainPagesFinalBudget(t *testing.T) {
	b, c, d := quiescenceBuilderFixture(t)
	b.identityKey = "identity"
	b.restoreKey = "restore"
	intent := taskFence{key: "quiescence", value: "original", create: 31, mod: 31}
	receipt := taskFence{key: "receipt", value: "committed", create: 31, mod: 31}
	cmps := taskEmptyComparisons(b, c, intent, receipt, taskQuiescenceReservationPin{key: "reservation", revision: 30})
	require.Len(t, cmps, 53)
	require.Equal(t, 55, len(cmps)+2)
	require.Equal(t, b.baseComparisons(), cmps[:4])
	require.Equal(t, c.comparisons(), cmps[4:44])
	require.Equal(t, intent.comparisons(), cmps[44:48])
	require.Equal(t, receipt.comparisons(), cmps[48:52])
	require.Equal(t, clientv3.Compare(clientv3.ModRevision("reservation"), "=", 30), cmps[52])
	require.NotNil(t, d)
}
