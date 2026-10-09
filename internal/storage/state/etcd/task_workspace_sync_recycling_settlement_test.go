package etcd

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestTaskWorkspaceSyncSettlementOriginalAuthority(t *testing.T) {
	var source *PreparedTaskWorkspaceSyncSource
	_, err := source.settleLocal(context.Background(), nil)
	require.Error(t, err)
	_, err = source.recycleLocal(context.Background(), nil)
	require.Error(t, err)
	d := &taskWorkspaceSyncIntentDraft{}
	p := &taskWorkspaceSyncLocalPut{draft: d}
	p.self, d.localPut = p, p
	w := &taskWorkspaceSyncLocalWitness{origin: p}
	w.self, p.witness = w, w
	q := &taskWorkspaceSyncLocalSettlement{witness: w, draft: d}
	q.self = q
	require.False(t, q.original(), "self and witness alone cannot grant GC")
	d.localSettlement = q
	require.True(t, q.original())
	copy := *q
	copy.self = &copy
	require.False(t, copy.original(), "reset copied self cannot replace the original settlement registration")
	witnessCopy := *w
	putCopy := &taskWorkspaceSyncLocalPut{draft: p.draft, witness: &witnessCopy}
	putCopy.self = putCopy
	witnessCopy.self, witnessCopy.origin = &witnessCopy, putCopy
	copy.witness = &witnessCopy
	require.False(t, copy.original(), "joint copies cannot reconstruct GC authority")
}

func TestTaskWorkspaceSyncSettlementVersionBoundary(t *testing.T) {
	require.True(t, recyclingCanAdvance(math.MaxInt64-2), "next version Max-1 remains a valid, charged native generation")
	require.False(t, recyclingCanAdvance(math.MaxInt64-1), "next version Max is rejected by strict native slot decoding")
	require.False(t, recyclingCanAdvance(math.MaxInt64), "next version overflows")
	require.False(t, recyclingCanAdvance(0))
}

func TestTaskWorkspaceSyncSettlementWireBounds(t *testing.T) {
	b, d := recyclingLocalDraftFixture(t)
	d.record.RelativePath = strings.Repeat("p", workspaceSyncMaxPathBytes)
	d.record.Local.Root = "/" + strings.Repeat("r", 4095)
	require.NoError(t, b.prepareRecyclingIntent(d))
	d.revision = 7
	d.recycling = &taskWorkspaceSyncRecyclingGeneration{create: 2, mod: 4, version: 1}
	o := &taskArchiveDelivery{backend: b, prefix: fmt.Sprintf("%sp/%02x/sandboxes/%s/operations/", b.namespace.Root(), d.record.Reference.Task.Partition, d.record.Reference.Task.SandboxID)}
	for i := 0; i < 50; i++ {
		key := b.namespace.Root() + fmt.Sprintf("history/%02d/", i)
		key += strings.Repeat("k", 1024-len(key))
		o.comparisons = append(o.comparisons, clientv3.Compare(clientv3.Value(key), "=", strings.Repeat("v", 512)))
	}
	s := &PreparedTaskWorkspaceSyncSource{archive: &ReceivedTaskWorkspaceArchive{owner: o}}
	w := &taskWorkspaceSyncLocalWitness{bytes: d.record.Size, sha256: d.record.BodySHA256, dev: d.record.Local.Dev, ino: 22}
	value, err := recyclingTerminalValue(d, w)
	require.NoError(t, err)
	require.LessOrEqual(t, len(value), 512)
	q := &taskWorkspaceSyncLocalSettlement{draft: d, value: value, terminal: taskWorkspaceSyncRecyclingGeneration{create: 2, mod: 8, version: 3, value: value}}
	ref := d.record.Reference
	free, err := encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: ref.BudgetID, Partition: ref.Task.Partition, Slot: ref.Slot, State: "free"})
	require.NoError(t, err)
	failureKeys := []string{b.identityKey, b.restoreKey, b.workspaceSyncBudget.key}
	replyKeys := []string{strings.Repeat("k", 1024), strings.Repeat("k", 1024), strings.Repeat("k", 1024)}
	failure := taskProtoResponse(replyKeys, []string{strings.Repeat("i", 1024), strings.Repeat("r", 128), strings.Repeat("b", 1024)})
	for _, stage := range []string{"terminal", "gc"} {
		t.Run(stage, func(t *testing.T) {
			cmps := s.recyclingTerminalComparisons(d)
			ops := []*pb.RequestOp{taskProtoPut(d.slotKey, value, 0)}
			success := taskProtoPutResponse(1)
			if stage == "gc" {
				cmps = s.recyclingGCComparisons(q)
				ops = taskProtoWrites([]Write{{Key: d.slotKey, Value: []byte(free)}, {Key: d.intentKey, Delete: true}})
				success = recyclingGCReply()
			}
			request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: ops, Failure: taskProtoPoints(failureKeys)}
			require.Equal(t, 64, len(request.Compare)+len(request.Success)+len(request.Failure))
			require.NoError(t, taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{success, failure}))
			t.Logf("actual protobuf %s originalPins50 nodes64 request%d success%d failure%d intent%d slot%d", stage, request.Size(), success.Size(), failure.Size(), len(d.value), len(value))
			request.Failure = append(request.Failure, taskProtoRange(b.identityKey))
			require.ErrorIs(t, taskCheckWire(b.namespace, []*pb.TxnRequest{request}, nil), ErrInvalidMutation)
		})
	}
	keys := []string{d.slotKey, d.intentKey}
	read := &pb.TxnRequest{Compare: taskProtoComparisons(s.recyclingCompletionComparisons(d, false)), Success: taskProtoPoints(keys), Failure: taskProtoPoints(failureKeys)}
	reply := taskProtoResponse([]string{strings.Repeat("k", 1024), strings.Repeat("k", 1024)}, []string{strings.Repeat("s", 512), strings.Repeat("i", 16384)})
	require.Equal(t, 59, len(read.Compare)+len(read.Success)+len(read.Failure))
	require.NoError(t, taskCheckWire(b.namespace, []*pb.TxnRequest{read}, []*pb.TxnResponse{reply, failure}))
	t.Logf("original readonly unknown protobuf nodes59 request%d worstReplyB16384Key1024=%d", read.Size(), reply.Size())
	read.Success[0] = taskProtoPut(d.slotKey, strings.Repeat("x", maxMutationBytes), 0)
	require.ErrorIs(t, taskCheckWire(b.namespace, []*pb.TxnRequest{read}, nil), ErrInvalidMutation)
	require.ErrorIs(t, taskCheckWire(b.namespace, nil, []*pb.TxnResponse{taskProtoResponse(keys, []string{strings.Repeat("x", maxMutationBytes), d.value})}), ErrInvalidMutation)
}
