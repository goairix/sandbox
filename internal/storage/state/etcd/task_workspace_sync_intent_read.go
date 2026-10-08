package etcd

import (
	"context"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func workspaceSyncPointResponse(b *Backend, r *clientv3.TxnResponse, keys []string) ([]*mvccpb.KeyValue, error) {
	if err := b.operationResponseHeader(r); err != nil {
		return nil, err
	}
	if len(r.Responses) != len(keys) {
		return nil, ErrOutcomeUnknown
	}
	values := make([]*mvccpb.KeyValue, len(keys))
	for i, response := range r.Responses {
		if response == nil {
			return nil, ErrOutcomeUnknown
		}
		point := response.GetResponseRange()
		if point == nil || !operationNestedHeader(point.Header, r.Header) || point.More || point.Count != int64(len(point.Kvs)) || len(point.Kvs) > 1 {
			return nil, ErrOutcomeUnknown
		}
		if len(point.Kvs) == 1 {
			kv := point.Kvs[0]
			if kv == nil || string(kv.Key) != keys[i] || len(kv.Value) > maxRecordBytes || kv.ModRevision > r.Header.Revision {
				return nil, ErrCorruptRecord
			}
			values[i] = kv
		}
	}
	return values, nil
}

func (b *Backend) workspaceSyncBudgetMatches(kv *mvccpb.KeyValue) bool {
	p := b.workspaceSyncBudget
	return p != nil && immutableWorkspaceSyncKV(kv) && string(kv.Key) == p.key && string(kv.Value) == p.value && kv.CreateRevision == p.create && kv.ModRevision == p.mod
}

func (b *Backend) readWorkspaceSyncIntent(ctx context.Context, ref TaskWorkspaceSyncIntentReference) (*TaskWorkspaceSyncIntentEntry, error) {
	intent, slot, err := b.workspaceSyncIntentKeys(ref)
	if err != nil {
		return nil, err
	}
	cfg, err := b.namespace.Key("meta", "workspace_sync_budget")
	if err != nil {
		return nil, err
	}
	keys := []string{b.identityKey, b.restoreKey, cfg, intent, slot}
	cmps := b.baseComparisons()
	values := []string{b.identityValue, b.restoreEpoch, string(make([]byte, 1024)), string(make([]byte, 16384)), string(make([]byte, 512))}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints(keys), Failure: taskProtoPoints(keys[:2])}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoResponse(keys, values), taskProtoResponse(keys[:2], values[:2])}); err != nil {
		return nil, err
	}
	rpcCtx, cancel := b.requestContext(ctx)
	defer cancel()
	r, err := b.client.Txn(rpcCtx).If(cmps...).Then(workspaceSyncPointOps(keys)...).Else(workspaceSyncPointOps(keys[:2])...).Commit()
	if err != nil || rpcCtx.Err() != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err, rpcCtx.Err())
	}
	if err := b.operationResponseHeader(r); err != nil {
		return nil, err
	}
	if !r.Succeeded {
		return nil, ErrIdentityMismatch
	}
	kvs, err := workspaceSyncPointResponse(b, r, keys)
	if err != nil {
		return nil, err
	}
	var budget taskWorkspaceSyncBudgetRecord
	if !immutableWorkspaceSyncKV(kvs[2]) || decodePreparationDomain(kvs[2], &budget, 1024) != nil {
		return nil, ErrIdentityMismatch
	}
	canonical, err := encodeOperationMetadata(budget, 1024)
	if err != nil || canonical != string(kvs[2].Value) || budget.BudgetID != ref.BudgetID || ref.Slot >= budget.SlotsPerPartition || b.workspaceSyncBudget != nil && !b.workspaceSyncBudgetMatches(kvs[2]) {
		return nil, ErrIdentityMismatch
	}
	if kvs[3] == nil && kvs[4] == nil {
		return nil, ErrOutcomeUnknown
	}
	record, err := decodeWorkspaceSyncIntent(kvs[3], int(budget.MaxIntentBytes))
	if err != nil {
		return nil, err
	}
	if record.BudgetRevision != kvs[2].CreateRevision {
		return nil, ErrIdentityMismatch
	}
	slotRecord, err := decodeWorkspaceSyncSlot(kvs[4])
	if err != nil {
		return nil, err
	}
	wireRef := ref
	wireRef.Revision = 0
	if record.Reference != wireRef || slotRecord.BudgetID != ref.BudgetID || slotRecord.Partition != ref.Task.Partition || slotRecord.Slot != ref.Slot || slotRecord.IntentID != ref.IntentID || slotRecord.IntentDigest != workspaceSyncIntentDigest(string(kvs[3].Value)) || kvs[3].CreateRevision != kvs[4].CreateRevision || ref.Revision != 0 && ref.Revision != kvs[3].CreateRevision {
		return nil, ErrCorruptRecord
	}
	ref.Revision = kvs[3].CreateRevision
	return &TaskWorkspaceSyncIntentEntry{ref, record, kvs[3].CreateRevision}, nil
}

// LoadTaskWorkspaceSyncIntent reads copied historical metadata. It cannot recover
// an original body or authorize dispatch, settlement, refunds or slot reuse.
func (b *Backend) LoadTaskWorkspaceSyncIntent(ctx context.Context, ref TaskWorkspaceSyncIntentReference) (*TaskWorkspaceSyncIntentEntry, error) {
	if b == nil || b.client == nil || ctx == nil || ref.Validate() != nil || ref.Revision <= 0 {
		return nil, ErrInvalidRecord
	}
	return b.readWorkspaceSyncIntent(ctx, ref)
}

func (s *PreparedTaskWorkspaceSyncSource) arbitrateWorkspaceSyncIntent(ctx context.Context, body *taskWorkspaceSyncBody, draft *taskWorkspaceSyncIntentDraft) (taskWorkspaceSyncIntentResult, error) {
	result := taskWorkspaceSyncIntentResult{Reference: draft.record.Reference, Outcome: OutcomeUnknown}
	entry, err := s.archive.owner.backend.readWorkspaceSyncIntent(ctx, result.Reference)
	if err != nil {
		return result, err
	}
	value, err := encodeWorkspaceSyncIntent(entry.Record, int(s.archive.owner.backend.workspaceSyncBudget.options.MaxIntentBytes))
	if err != nil || value != draft.value || draft.revision != 0 && draft.revision != entry.Revision {
		return result, ErrCorruptRecord
	}
	draft.revision = entry.Revision
	result.Reference.Revision = entry.Revision
	// With a lost first ACK and no previously observed birth, this outcome
	// proves matching permanent metadata exists now. An operator's paired
	// exact-byte delete/recreate before this first read cannot be distinguished
	// from the original birth. It proves no dispatch, settlement or refund.
	result.Outcome = OutcomeCommitted
	return result, s.workspaceSyncIntentLocal(ctx, body)
}
