package etcd

import (
	"archive/tar"
	"context"
	"encoding/hex"
	"errors"

	"github.com/google/uuid"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (s *PreparedTaskWorkspaceSyncSource) reservePutIntent(ctx context.Context, body *taskWorkspaceSyncBody) (taskWorkspaceSyncIntentResult, error) {
	result := taskWorkspaceSyncIntentResult{Outcome: OutcomeUnknown}
	if err := s.begin(ctx); err != nil {
		return result, err
	}
	defer s.end()
	if body == nil || body.self != body || body.source != s || s.body != body || body.entry == nil || body.entry.self != body.entry || body.entry.origin != s || s.current != body.entry || body.entry.kind != tar.TypeReg || s.broken {
		return result, ErrInvalidRecord
	}
	body.mu.Lock()
	if body.closed || body.active || body.bytes != 0 || body.complete || body.localConsumed || body.ctx.Err() != nil {
		body.mu.Unlock()
		return result, ErrInvalidRecord
	}
	body.active = true
	body.done = make(chan struct{})
	draft := body.intent
	body.mu.Unlock()
	defer func() { body.mu.Lock(); body.active = false; close(body.done); body.mu.Unlock() }()
	o := s.archive.owner
	b, budget := o.backend, o.backend.workspaceSyncBudget
	if budget == nil || budget.profile > 2 {
		return result, ErrInvalidConfiguration
	}
	if budget.profile == 2 && body.localTarget == nil {
		return result, ErrInvalidRecord
	}
	if budget.profile == 2 {
		if err := body.localTarget.begin(ctx); err != nil {
			return result, err
		}
		defer body.localTarget.ops.Done() // Close joins reserve RPC and its postguard
	}
	if err := s.local(ctx); err != nil {
		return result, err
	}
	if draft == nil {
		entry, w, archive := body.entry, s.plan.workspace, o.prepared.draft
		ref := TaskWorkspaceSyncIntentReference{Task: o.claim.reference.Task, BudgetID: budget.options.BudgetID, IntentID: uuid.NewString()}
		r := TaskWorkspaceSyncIntentRecord{taskWorkspaceSyncIntentAttribution: taskWorkspaceSyncIntentAttribution{Version: 1, Reference: ref, BudgetRevision: budget.create, Kind: "put_object", FinalSyncAttemptID: archive.finalSync.record.AttemptID, CommandID: archive.reference().CommandID, TicketDigest: archive.ticket.Digest(), ArchiveSHA256: s.diagnostics.SHA256, PlanDigest: s.plan.digest, WorkspaceHash: w.hash, Provider: w.provider, StorageIdentityHash: w.storageIdentityHash, Bucket: w.bucket, Prefix: w.prefix, RelativePath: entry.path, Mode: entry.mode, Size: entry.size, Offset: entry.offset, BodySHA256: entry.sha256}, Claim: o.claim.reference}
		if body.localTarget != nil {
			if err := s.workspaceSyncBoundLocal(ctx, body); err != nil {
				return result, err
			}
			pin := body.localTarget.pin
			r.Version, r.Local = 2, &pin
		}
		draft = &taskWorkspaceSyncIntentDraft{record: r}
		body.mu.Lock()
		body.intent = draft
		body.mu.Unlock()
	}
	result.Reference = draft.record.Reference
	bounded, cancel := context.WithCancel(ctx)
	stopSource := context.AfterFunc(s.ctx, cancel)
	stopBody := context.AfterFunc(body.ctx, cancel)
	defer stopSource()
	defer stopBody()
	defer cancel()
	if budget.profile == 2 {
		stopTarget := context.AfterFunc(body.localTarget.ctx, cancel)
		defer stopTarget()
		if err := s.archive.owner.spool.sealOriginalBinding(bounded, s, body); err != nil {
			return result, err
		}
		return s.reserveRecyclingPutIntent(bounded, body, draft)
	}
	o.nativeMu.Lock()
	nativeLocked := true
	defer func() {
		if nativeLocked {
			o.nativeMu.Unlock()
		}
	}()
	if draft.writeAttempted {
		return s.arbitrateWorkspaceSyncIntent(bounded, body, draft)
	}
	seed, _ := hex.DecodeString(workspaceSyncIntentDigest(draft.record.Reference.IntentID))
	start := uint32(seed[0]) % budget.options.SlotsPerPartition
	for candidate := uint32(0); candidate < budget.options.SlotsPerPartition; candidate++ {
		if err := s.local(bounded); err != nil {
			return result, err
		}
		draft.record.Reference.Slot = (start + candidate) % budget.options.SlotsPerPartition
		if err := b.prepareWorkspaceSyncIntent(draft); err != nil {
			return result, err
		}
		result.Reference = draft.record.Reference
		cmps := append([]clientv3.Cmp(nil), o.comparisons...)
		cmps = append(cmps, clientv3.Compare(clientv3.Version(o.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(o.prefix)))
		cmps = append(cmps, budget.comparisons()...)
		cmps = append(cmps, clientv3.Compare(clientv3.Version(draft.intentKey), "=", 0), clientv3.Compare(clientv3.Version(draft.slotKey), "=", 0))
		keys := []string{b.identityKey, b.restoreKey, budget.key, draft.intentKey, draft.slotKey}
		if err := b.preflightWorkspaceSyncIntent(cmps, draft, keys); err != nil {
			return result, err
		}
		if err := s.local(bounded); err != nil {
			return result, err
		}
		draft.writeAttempted = true // fixed candidate before a native request can escape
		rpcCtx, rpcCancel := b.requestContext(bounded)
		r, err := b.client.Txn(rpcCtx).If(cmps...).Then(clientv3.OpPut(draft.intentKey, draft.value), clientv3.OpPut(draft.slotKey, draft.slotValue)).Else(workspaceSyncPointOps(keys)...).Commit()
		rpcErr := rpcCtx.Err()
		rpcCancel()
		if err != nil || rpcErr != nil {
			return result, errors.Join(ErrOutcomeUnknown, err, rpcErr)
		}
		if err := b.operationResponseHeader(r); err != nil {
			return result, errors.Join(ErrOutcomeUnknown, err)
		}
		if r.Succeeded {
			if len(r.Responses) != 2 {
				return result, ErrOutcomeUnknown
			}
			for _, response := range r.Responses {
				if response == nil || response.GetResponsePut() == nil || !operationNestedHeader(response.GetResponsePut().Header, r.Header) || response.GetResponsePut().PrevKv != nil {
					return result, ErrOutcomeUnknown
				}
			}
			draft.revision = r.Header.Revision
			result.Reference.Revision = draft.revision
			result.Outcome = OutcomeCommitted
			o.nativeMu.Unlock()
			nativeLocked = false
			if err := s.workspaceSyncIntentLocal(bounded, body); err != nil {
				return result, err
			}
			if draft.record.Version == 2 {
				if err := s.workspaceSyncBoundLocal(bounded, body); err != nil {
					return result, err
				}
				if err := s.workspaceSyncIntentLocal(bounded, body); err != nil {
					return result, err
				}
				draft.acknowledged = true
			}
			return result, nil
		}
		values, err := workspaceSyncPointResponse(b, r, keys)
		if err != nil {
			return result, errors.Join(ErrOutcomeUnknown, err)
		}
		if !b.workspaceSyncBudgetMatches(values[2]) {
			return result, ErrIdentityMismatch
		}
		// A failed full transaction grants no slot evidence until all original
		// history and the complete current prefix have been freshly checked.
		if err := s.workspaceSyncIntentQuotaGuard(bounded, draft); err != nil {
			return result, err
		}
		if err := s.workspaceSyncIntentLocal(bounded, body); err != nil {
			return result, err
		}
		if values[3] != nil {
			return result, ErrConflict
		}
		if _, err := decodeWorkspaceSyncSlot(values[4]); err != nil {
			return result, errors.Join(ErrConflict, err)
		}
		if slot, _ := decodeWorkspaceSyncSlot(values[4]); slot.BudgetID != budget.options.BudgetID || slot.Partition != draft.record.Reference.Task.Partition || slot.Slot != draft.record.Reference.Slot {
			return result, ErrConflict
		}
		draft.writeAttempted = false // explicit native failed branch: no writes
	}
	return result, errors.Join(ErrConflict, errTaskWorkspaceSyncIntentQuota)
}

func (s *PreparedTaskWorkspaceSyncSource) workspaceSyncIntentQuotaGuard(ctx context.Context, d *taskWorkspaceSyncIntentDraft) error {
	o, b := s.archive.owner, s.archive.owner.backend
	cmps := append([]clientv3.Cmp(nil), o.comparisons...)
	cmps = append(cmps, clientv3.Compare(clientv3.Version(o.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(o.prefix)))
	cmps = append(cmps, b.workspaceSyncBudget.comparisons()...)
	keys := []string{b.identityKey, b.restoreKey, b.workspaceSyncBudget.key, d.intentKey, d.slotKey}
	values := []string{b.identityValue, b.restoreEpoch, b.workspaceSyncBudget.value, string(make([]byte, b.workspaceSyncBudget.options.MaxIntentBytes)), string(make([]byte, 512))}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints(keys[3:]), Failure: taskProtoPoints(keys)}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoResponse(keys[3:], values[3:]), taskProtoResponse(keys, values)}); err != nil {
		return err
	}
	rpcCtx, cancel := b.requestContext(ctx)
	defer cancel()
	r, err := b.client.Txn(rpcCtx).If(cmps...).Then(workspaceSyncPointOps(keys[3:])...).Else(workspaceSyncPointOps(keys)...).Commit()
	if err != nil || rpcCtx.Err() != nil {
		return errors.Join(ErrOutcomeUnknown, err, rpcCtx.Err())
	}
	if err := b.operationResponseHeader(r); err != nil {
		return err
	}
	if !r.Succeeded {
		points, err := workspaceSyncPointResponse(b, r, keys)
		if err != nil {
			return err
		}
		if !b.workspaceSyncBudgetMatches(points[2]) {
			return ErrIdentityMismatch
		}
		return ErrConflict
	}
	points, err := workspaceSyncPointResponse(b, r, keys[3:])
	if err != nil {
		return err
	}
	if points[0] != nil {
		return ErrConflict
	}
	slot, err := decodeWorkspaceSyncSlot(points[1])
	ref := d.record.Reference
	if err != nil || slot.BudgetID != ref.BudgetID || slot.Partition != ref.Task.Partition || slot.Slot != ref.Slot {
		return ErrConflict
	}
	return nil
}

func (s *PreparedTaskWorkspaceSyncSource) workspaceSyncIntentLocal(ctx context.Context, body *taskWorkspaceSyncBody) error {
	if err := s.local(ctx); err != nil {
		return err
	}
	body.mu.Lock()
	defer body.mu.Unlock()
	if body.closed || body.ctx.Err() != nil || body.bytes != 0 || body.localTarget != nil && body.localTarget.ctx.Err() != nil {
		return ErrGuardExpired
	}
	return nil
}

func (b *Backend) prepareWorkspaceSyncIntent(d *taskWorkspaceSyncIntentDraft) error {
	var err error
	d.intentKey, d.slotKey, err = b.workspaceSyncIntentKeys(d.record.Reference)
	if err != nil {
		return err
	}
	d.value, err = encodeWorkspaceSyncIntent(d.record, int(b.workspaceSyncBudget.options.MaxIntentBytes))
	if err != nil {
		return err
	}
	r := d.record.Reference
	d.slotValue, err = encodeOperationMetadata(taskWorkspaceSyncSlotRecord{1, r.BudgetID, r.Task.Partition, r.Slot, r.IntentID, workspaceSyncIntentDigest(d.value)}, 512)
	return err
}

func workspaceSyncPointOps(keys []string) []clientv3.Op {
	ops := make([]clientv3.Op, len(keys))
	for i, key := range keys {
		ops[i] = clientv3.OpGet(key)
	}
	return ops
}

func (b *Backend) preflightWorkspaceSyncIntent(cmps []clientv3.Cmp, d *taskWorkspaceSyncIntentDraft, keys []string) error {
	projection, err := b.workspaceSyncIntentWireProjection(cmps, d.intentKey, d.value, d.slotKey, d.slotValue, keys)
	if err != nil {
		return err
	}
	return checkTaskWireProjections(b.namespace, []cleanupWireProjection{projection})
}

// This accepts inert bytes, not a prepared body or an intent permission. Both
// native puts and all five failure points remain the original admission shape.
func (b *Backend) workspaceSyncIntentWireProjection(cmps []clientv3.Cmp, intentKey, value, slotKey, slotValue string, keys []string) (cleanupWireProjection, error) {
	values := []string{b.identityValue, b.restoreEpoch, b.workspaceSyncBudget.value, string(make([]byte, b.workspaceSyncBudget.options.MaxIntentBytes)), string(make([]byte, 512))}
	if len(keys) != len(values) {
		return cleanupWireProjection{}, ErrInvalidMutation
	}
	for i, key := range keys {
		if len(key) > 1024 || len(values[i]) > maxRecordBytes || !validNamespaceKey(b.namespace, key) {
			return cleanupWireProjection{}, ErrInvalidMutation
		}
	}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: []*pb.RequestOp{taskProtoPut(intentKey, value, 0), taskProtoPut(slotKey, slotValue, 0)}, Failure: taskProtoPoints(keys)}
	return cleanupWireProjection{"intent-write", request, taskProtoPutResponse(2), taskProtoResponse(keys, values)}, nil
}
