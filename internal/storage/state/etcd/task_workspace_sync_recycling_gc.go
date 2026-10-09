package etcd

import (
	"context"
	"math"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (s *PreparedTaskWorkspaceSyncSource) recyclingGCComparisons(q *taskWorkspaceSyncLocalSettlement) []clientv3.Cmp {
	d := q.draft
	return append(s.recyclingCompletionComparisons(d, true), clientv3.Compare(clientv3.ModRevision(d.slotKey), "=", q.terminal.mod), clientv3.Compare(clientv3.Value(d.slotKey), "=", q.value))
}

func recyclingGCReply() *pb.TxnResponse {
	r := taskProtoPutResponse(1)
	r.Responses = append(r.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: r.Header, Deleted: math.MaxInt64}}})
	return r
}

func (s *PreparedTaskWorkspaceSyncSource) recycleLocal(ctx context.Context, q *taskWorkspaceSyncLocalSettlement) (Outcome, error) {
	if !q.original() {
		return OutcomeUnknown, ErrInvalidRecord
	}
	op, err := s.beginLocalSettlement(ctx, q.witness)
	if err != nil {
		return OutcomeUnknown, err
	}
	defer op.end()
	if !q.known || q.terminal.create != q.draft.recycling.create || q.terminal.mod <= q.draft.revision || q.terminal.mod == math.MaxInt64 || q.terminal.version != q.draft.recycling.version+2 || q.terminal.version == math.MaxInt64 || q.terminal.value != q.value {
		return OutcomeUnknown, ErrInvalidRecord
	}
	if q.completed {
		return OutcomeCommitted, nil
	}
	if q.gcDenied {
		return OutcomeUnknown, ErrConflict
	}
	d, ref := q.draft, q.draft.record.Reference
	free, err := encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: ref.BudgetID, Partition: ref.Task.Partition, Slot: ref.Slot, State: "free"})
	if err != nil {
		return OutcomeUnknown, err
	}
	if q.gcAttempted {
		points, err := op.readCompletion(d)
		if err != nil {
			return OutcomeUnknown, err
		}
		if points[1] != nil || !recyclingGenerationMatches(points[0], q.terminal.create, q.terminal.version+1, q.terminal.mod, free) {
			return OutcomeUnknown, ErrOutcomeUnknown
		}
	} else {
		q.gcAttempted = true
		writes := []Write{{Key: d.slotKey, Value: []byte(free)}, {Key: d.intentKey, Delete: true}}
		r, err := op.native(s.recyclingGCComparisons(q), []clientv3.Op{clientv3.OpPut(d.slotKey, free), clientv3.OpDelete(d.intentKey)}, taskProtoWrites(writes), recyclingGCReply())
		if err != nil {
			q.gcDenied = r != nil && !r.Succeeded
			return OutcomeUnknown, err
		}
		if len(r.Responses) != 2 || r.Header.Revision <= q.terminal.mod || r.Header.Revision == math.MaxInt64 || r.Responses[0] == nil || r.Responses[1] == nil {
			return OutcomeUnknown, ErrOutcomeUnknown
		}
		put, deleted := r.Responses[0].GetResponsePut(), r.Responses[1].GetResponseDeleteRange()
		if put == nil || put.PrevKv != nil || !operationNestedHeader(put.Header, r.Header) || deleted == nil || deleted.Deleted != 1 || len(deleted.PrevKvs) != 0 || !operationNestedHeader(deleted.Header, r.Header) {
			return OutcomeUnknown, ErrOutcomeUnknown
		}
	}
	if err := op.guard(); err != nil {
		return OutcomeUnknown, err
	}
	if q.probeComplete != nil {
		q.probeComplete()
	}
	if err := op.local(); err != nil {
		return OutcomeUnknown, err
	}
	q.completed = true
	return OutcomeCommitted, nil
}
