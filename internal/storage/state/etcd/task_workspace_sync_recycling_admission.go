package etcd

import (
	"context"
	"encoding/hex"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// reservePutIntent has already registered the original source and unread body.
// This helper must not begin again: begin's busy gate is intentionally one-shot.
func (s *PreparedTaskWorkspaceSyncSource) reserveRecyclingPutIntent(ctx context.Context, body *taskWorkspaceSyncBody, d *taskWorkspaceSyncIntentDraft) (taskWorkspaceSyncIntentResult, error) {
	result := taskWorkspaceSyncIntentResult{Reference: d.record.Reference, Outcome: OutcomeUnknown}
	o, b := s.archive.owner, s.archive.owner.backend
	budget := b.workspaceSyncBudget
	if d.record.Version != 2 || d.record.Local == nil || body.localTarget == nil {
		return result, ErrInvalidRecord
	}
	o.nativeMu.Lock()
	nativeLocked := true
	defer func() {
		if nativeLocked {
			o.nativeMu.Unlock()
		}
	}()
	if d.writeAttempted {
		return s.arbitrateRecyclingIntent(ctx, body, d)
	}
	seed, _ := hex.DecodeString(workspaceSyncIntentDigest(d.record.Reference.IntentID))
	start := uint32(seed[0]) % budget.options.SlotsPerPartition
	for candidate := uint32(0); candidate < budget.options.SlotsPerPartition; candidate++ {
		if err := s.workspaceSyncIntentLocal(ctx, body); err != nil {
			return result, err
		}
		d.record.Reference.Slot = (start + candidate) % budget.options.SlotsPerPartition
		if err := b.prepareRecyclingIntent(d); err != nil {
			return result, err
		}
		result.Reference = d.record.Reference
		free, state, err := s.readRecyclingCandidate(ctx, d)
		if err != nil {
			return result, err
		}
		if state != "free" {
			continue // fresh full history/prefix/budget guard checked this occupant
		}
		d.recycling = free
		cmps := s.recyclingReserveComparisons(d)
		keys := []string{b.identityKey, b.restoreKey, budget.key, d.intentKey, d.slotKey}
		if err := b.preflightWorkspaceSyncIntent(cmps, d, keys); err != nil {
			return result, err
		}
		if err := s.workspaceSyncIntentLocal(ctx, body); err != nil {
			return result, err
		}
		d.writeAttempted = true // candidate and native generation fixed before RPC
		rpcCtx, cancel := b.requestContext(ctx)
		r, err := b.client.Txn(rpcCtx).If(cmps...).Then(clientv3.OpPut(d.intentKey, d.value), clientv3.OpPut(d.slotKey, d.slotValue)).Else(workspaceSyncPointOps(keys)...).Commit()
		rpcErr := rpcCtx.Err()
		cancel()
		if err != nil || rpcErr != nil {
			return result, errors.Join(ErrOutcomeUnknown, err, rpcErr)
		}
		if err := b.operationResponseHeader(r); err != nil {
			return result, errors.Join(ErrOutcomeUnknown, err)
		}
		if err := taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(r)}); err != nil {
			return result, errors.Join(ErrOutcomeUnknown, err)
		}
		if r.Succeeded {
			if len(r.Responses) != 2 || r.Header.Revision <= free.mod {
				return result, ErrOutcomeUnknown
			}
			for _, response := range r.Responses {
				if response == nil || response.GetResponsePut() == nil || !operationNestedHeader(response.GetResponsePut().Header, r.Header) || response.GetResponsePut().PrevKv != nil {
					return result, ErrOutcomeUnknown
				}
			}
			d.revision = r.Header.Revision
			result.Reference.Revision, result.Outcome = d.revision, OutcomeCommitted
			o.nativeMu.Unlock() // native ACK complete; no mutex spans physical postguard
			nativeLocked = false
			if err := s.workspaceSyncBoundLocal(ctx, body); err != nil {
				return result, err
			}
			if err := s.workspaceSyncIntentLocal(ctx, body); err != nil {
				return result, err
			}
			d.acknowledged = true
			return result, nil
		}
		points, err := workspaceSyncPointResponse(b, r, keys)
		if err != nil {
			return result, errors.Join(ErrOutcomeUnknown, err)
		}
		if !b.workspaceSyncBudgetMatches(points[2]) {
			return result, ErrIdentityMismatch
		}
		// A failed ACK has no writes. Only a new full guard permits considering
		// another candidate, including a same-value free generation changed by CAS.
		if _, _, err := s.readRecyclingCandidate(ctx, d); err != nil {
			return result, err
		}
		if err := s.workspaceSyncIntentLocal(ctx, body); err != nil {
			return result, err
		}
		d.writeAttempted = false
	}
	return result, errors.Join(ErrConflict, errTaskWorkspaceSyncIntentQuota)
}

func (b *Backend) prepareRecyclingIntent(d *taskWorkspaceSyncIntentDraft) error {
	var err error
	d.intentKey, d.slotKey, err = b.workspaceSyncRecyclingKeys(d.record.Reference)
	if err != nil {
		return err
	}
	d.value, err = encodeWorkspaceSyncIntent(d.record, int(b.workspaceSyncBudget.options.MaxIntentBytes))
	if err != nil {
		return err
	}
	r := d.record.Reference
	d.slotValue, err = encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: r.BudgetID, Partition: r.Task.Partition, Slot: r.Slot, State: "occupied", IntentID: r.IntentID, IntentDigest: workspaceSyncIntentDigest(d.value)})
	return err
}

func (s *PreparedTaskWorkspaceSyncSource) recyclingReserveComparisons(d *taskWorkspaceSyncIntentDraft) []clientv3.Cmp {
	o := s.archive.owner
	cmps := append([]clientv3.Cmp(nil), o.comparisons...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(o.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(o.prefix)))
	cmps = append(cmps, o.backend.workspaceSyncBudget.comparisons()...)
	return append(cmps, clientv3.Compare(clientv3.Version(d.intentKey), "=", 0), clientv3.Compare(clientv3.ModRevision(d.slotKey), "=", d.recycling.mod), clientv3.Compare(clientv3.Value(d.slotKey), "=", d.recycling.value))
}
