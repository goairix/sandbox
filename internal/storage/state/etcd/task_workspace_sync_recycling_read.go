package etcd

import (
	"context"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The original reserve serializes this bounded point read with nativeMu. Every
// candidate decision retains all original history, full prefix and budget pins.
func (s *PreparedTaskWorkspaceSyncSource) readRecyclingCandidate(ctx context.Context, d *taskWorkspaceSyncIntentDraft) (*taskWorkspaceSyncRecyclingGeneration, string, error) {
	o, b := s.archive.owner, s.archive.owner.backend
	cmps := append([]clientv3.Cmp(nil), o.comparisons...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(o.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(o.prefix)))
	cmps = append(cmps, b.workspaceSyncBudget.comparisons()...)
	keys := []string{b.identityKey, b.restoreKey, b.workspaceSyncBudget.key, d.intentKey, d.slotKey}
	values := []string{b.identityValue, b.restoreEpoch, b.workspaceSyncBudget.value, string(make([]byte, b.workspaceSyncBudget.options.MaxIntentBytes)), string(make([]byte, 512))}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints(keys[3:]), Failure: taskProtoPoints(keys[:3])}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoResponse(keys[3:], values[3:]), taskProtoResponse(keys[:3], values[:3])}); err != nil {
		return nil, "", err
	}
	if err := s.local(ctx); err != nil {
		return nil, "", err
	}
	rpcCtx, cancel := b.requestContext(ctx)
	r, err := b.client.Txn(rpcCtx).If(cmps...).Then(workspaceSyncPointOps(keys[3:])...).Else(workspaceSyncPointOps(keys[:3])...).Commit()
	rpcErr := rpcCtx.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return nil, "", errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err := b.operationResponseHeader(r); err != nil {
		return nil, "", err
	}
	if err := taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(r)}); err != nil {
		return nil, "", err
	}
	if !r.Succeeded {
		points, err := workspaceSyncPointResponse(b, r, keys[:3])
		if err != nil {
			return nil, "", err
		}
		if !b.workspaceSyncBudgetMatches(points[2]) {
			return nil, "", ErrIdentityMismatch
		}
		return nil, "", ErrConflict
	}
	points, err := workspaceSyncPointResponse(b, r, keys[3:])
	if err != nil {
		return nil, "", err
	}
	if points[0] != nil {
		return nil, "", ErrConflict
	}
	slot, err := decodeWorkspaceSyncRecyclingSlot(points[1])
	ref := d.record.Reference
	if err != nil || slot.BudgetID != ref.BudgetID || slot.Partition != ref.Task.Partition || slot.Slot != ref.Slot || ref.Slot >= b.workspaceSyncBudget.options.SlotsPerPartition {
		return nil, "", errors.Join(ErrConflict, err)
	}
	kv := points[1]
	return &taskWorkspaceSyncRecyclingGeneration{create: kv.CreateRevision, mod: kv.ModRevision, version: kv.Version, value: string(kv.Value)}, slot.State, nil
}

func (b *Backend) readRecyclingIntent(ctx context.Context, ref TaskWorkspaceSyncIntentReference) (*TaskWorkspaceSyncIntentEntry, error) {
	budget := b.workspaceSyncBudget
	if budget == nil || budget.profile != 2 || ref.BudgetID != budget.options.BudgetID || ref.Slot >= budget.options.SlotsPerPartition {
		return nil, ErrInvalidConfiguration
	}
	intent, slot, err := b.workspaceSyncRecyclingKeys(ref)
	if err != nil {
		return nil, err
	}
	keys := []string{b.identityKey, b.restoreKey, budget.key, intent, slot}
	cmps := append(b.baseComparisons(), budget.comparisons()...)
	values := []string{b.identityValue, b.restoreEpoch, budget.value, string(make([]byte, budget.options.MaxIntentBytes)), string(make([]byte, 512))}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints(keys[3:]), Failure: taskProtoPoints(keys[:3])}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoResponse(keys[3:], values[3:]), taskProtoResponse(keys[:3], values[:3])}); err != nil {
		return nil, err
	}
	rpcCtx, cancel := b.requestContext(ctx)
	defer cancel()
	r, err := b.client.Txn(rpcCtx).If(cmps...).Then(workspaceSyncPointOps(keys[3:])...).Else(workspaceSyncPointOps(keys[:3])...).Commit()
	if err != nil || rpcCtx.Err() != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err, rpcCtx.Err())
	}
	if err := b.operationResponseHeader(r); err != nil {
		return nil, err
	}
	if err := taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(r)}); err != nil {
		return nil, err
	}
	if !r.Succeeded {
		if _, err := workspaceSyncPointResponse(b, r, keys[:3]); err != nil {
			return nil, err
		}
		return nil, ErrIdentityMismatch
	}
	points, err := workspaceSyncPointResponse(b, r, keys[3:])
	if err != nil {
		return nil, err
	}
	return b.recyclingIntentEntry(ref, points[0], points[1])
}

func (b *Backend) recyclingIntentEntry(ref TaskWorkspaceSyncIntentReference, intentKV, slotKV *mvccpb.KeyValue) (*TaskWorkspaceSyncIntentEntry, error) {
	if intentKV == nil {
		return nil, ErrOutcomeUnknown
	}
	record, err := decodeWorkspaceSyncIntent(intentKV, int(b.workspaceSyncBudget.options.MaxIntentBytes))
	if err != nil {
		return nil, err
	}
	slot, err := decodeWorkspaceSyncRecyclingSlot(slotKV)
	if err != nil {
		return nil, err
	}
	wireRef := ref
	wireRef.Revision = 0
	if record.Version != 2 || record.Local == nil || record.BudgetRevision != b.workspaceSyncBudget.create || record.Reference != wireRef || slot.BudgetID != ref.BudgetID || slot.Partition != ref.Task.Partition || slot.Slot != ref.Slot || slot.IntentID != ref.IntentID || slot.IntentDigest != workspaceSyncIntentDigest(string(intentKV.Value)) || slot.State != "occupied" && slot.State != "terminal" || slotKV.ModRevision < intentKV.CreateRevision || slot.State == "occupied" && slotKV.ModRevision != intentKV.CreateRevision || slot.State == "terminal" && slot.IntentBirth != intentKV.CreateRevision || ref.Revision != 0 && ref.Revision != intentKV.CreateRevision {
		return nil, ErrCorruptRecord
	}
	ref.Revision = intentKV.CreateRevision
	return &TaskWorkspaceSyncIntentEntry{Reference: ref, Record: record, Revision: intentKV.CreateRevision}, nil
}

func (s *PreparedTaskWorkspaceSyncSource) arbitrateRecyclingIntent(ctx context.Context, body *taskWorkspaceSyncBody, d *taskWorkspaceSyncIntentDraft) (taskWorkspaceSyncIntentResult, error) {
	result := taskWorkspaceSyncIntentResult{Reference: d.record.Reference, Outcome: OutcomeUnknown}
	entry, err := s.archive.owner.backend.readRecyclingIntent(ctx, result.Reference)
	if err != nil {
		return result, err
	}
	value, err := encodeWorkspaceSyncIntent(entry.Record, int(s.archive.owner.backend.workspaceSyncBudget.options.MaxIntentBytes))
	if err != nil || value != d.value || d.revision != 0 && d.revision != entry.Revision {
		return result, ErrCorruptRecord
	}
	d.revision = entry.Revision // copied metadata only; acknowledged stays false
	result.Reference.Revision, result.Outcome = entry.Revision, OutcomeCommitted
	return result, s.workspaceSyncIntentLocal(ctx, body)
}
