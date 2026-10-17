package etcd

import (
	"context"
	"errors"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Index-only coverage uses the same continuous-prefix plan as normal birth,
// but can cover only an authenticated legacy row, never an empty or unrelated
// suffix. No optional E/release observation is interpreted as absence here.
func (b *Backend) cleanupBackfillIndexPlan(p *WorkerPartitionClaim, page *cleanupLegacyPage, index *cleanupBirthIndexEvidence) (cleanupBackfillRecord, error) {
	if err := b.validateCleanupBirthIndexEvidence(p, index); err != nil {
		return cleanupBackfillRecord{}, err
	}
	next, legacy, err := b.cleanupBackfillPagePlan(p, page)
	if err != nil {
		return cleanupBackfillRecord{}, err
	}
	if legacy < 0 {
		return cleanupBackfillRecord{}, ErrInvalidRecord
	}
	row := page.rows[legacy]
	if index.record.Reference.TaskID != row.taskID || index.birth != row.birth || index.revision < page.revision {
		return cleanupBackfillRecord{}, ErrInvalidRecord
	}
	return next, nil
}

// Validate original lineage and both existing shares before spending Claim
// credit. This does not extend the session clock or acquire another lease.
func (b *Backend) validateCleanupBackfillIndex(ctx context.Context, p *WorkerPartitionClaim, claims *WorkerBudgetShare, page *cleanupLegacyPage, index *cleanupBirthIndexEvidence) error {
	if ctx == nil || !b.validWorkerBackend() {
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := b.cleanupBackfillIndexPlan(p, page, index); err != nil {
		return err
	}
	if b.requestTimeout <= 0 || b.requestTimeout > 3*time.Second {
		return ErrInvalidConfiguration
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if err := b.cleanupBirthMembership(p, page.ranges, 4*b.requestTimeout+time.Second); err != nil {
		return err
	}
	if !b.validWorkerBudget(claims) || claims.kind != WorkerBudgetClaim || claims.session != p.session {
		return ErrInvalidRecord
	}
	if claims.release != nil || claims.released || claims.lost {
		return ErrGuardExpired
	}
	return ctx.Err()
}

// One native CAS publishes/preserves P and advances B with W20, B4, the exact
// mandatory cohort16 and P1/4. Unknown ACKs lose the session: a replacement
// must independently observe durable coverage, never adopt this invocation.
// The original owner remains held through actual return, including Close.
func (b *Backend) advanceCleanupBackfillIndex(ctx context.Context, p *WorkerPartitionClaim, claims *WorkerBudgetShare, page *cleanupLegacyPage, index *cleanupBirthIndexEvidence) (_ *cleanupBackfillState, resultErr error) {
	if err := b.validateCleanupBackfillIndex(ctx, p, claims, page, index); err != nil {
		return nil, err
	}
	o, stop, err := b.beginExpiredTaskOwner(ctx, p, claims)
	if err != nil {
		return nil, err
	}
	defer stop()
	defer o.finish()
	knownConflict := false
	defer func() {
		if resultErr != nil && o.call.started && !knownConflict {
			o.call.lose()
		}
	}()

	next, mutation, err := b.cleanupBackfillIndexMutation(o, page, index)
	if err != nil {
		return nil, err
	}
	if err = b.preflightCleanupBackfillIndex(mutation); err != nil {
		return nil, err
	}
	ops := make([]clientv3.Op, len(mutation.Writes))
	for i, write := range mutation.Writes {
		ops[i] = clientv3.OpPut(write.Key, string(write.Value))
	}
	if _, err = b.cleanupOwnerComparisons(o); err != nil {
		return nil, err
	}
	rpc, cancel := o.call.requestContext()
	if err = rpc.Err(); err != nil {
		cancel()
		return nil, err
	}
	o.call.started = true
	response, err := b.client.Txn(rpc).If(mutation.Comparisons...).Then(ops...).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err = b.operationResponseHeader(response); err != nil || !enrolledResponseBound(response) {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if !response.Succeeded {
		if _, err = b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey}); err != nil {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
		if err = o.postcheck(); err != nil {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
		knownConflict = true
		return nil, ErrConflict
	}
	if !cleanupBackfillAdvanceACK(response, mutation) {
		return nil, ErrOutcomeUnknown
	}
	if err = o.postcheck(); err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}

	write := mutation.Writes[len(mutation.Writes)-1]
	state := &cleanupBackfillState{origin: b, partition: p, record: next, fence: taskFence{key: write.Key, value: string(write.Value), create: page.fence.create, mod: response.Header.Revision}}
	state.self = state
	return state, nil
}

// This concrete builder only adds discovery coverage. A present P must already
// be canonical and immutable; rewriting malformed P would leave Version > 1
// and cannot repair discovery. All four mandatory fences include the link.
func (b *Backend) cleanupBackfillIndexMutation(o *workerDueOwner, page *cleanupLegacyPage, index *cleanupBirthIndexEvidence) (cleanupBackfillRecord, Mutation, error) {
	cmps, err := b.cleanupOwnerComparisons(o)
	if err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	if err = b.validateCleanupBackfillIndex(o.call.ctx, o.partition, o.share, page, index); err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	next, err := b.cleanupBackfillIndexPlan(o.partition, page, index)
	if err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	cmps = append(cmps, page.fence.comparisons()...)
	for _, fence := range []taskFence{index.task, index.intent, index.link, index.receipt} {
		cmps = append(cmps, fence.comparisons()...)
	}
	var writes []Write
	if index.pending != nil {
		cmps = append(cmps, index.pending.comparisons()...)
	} else {
		key, err := b.namespace.cleanupPendingKey(index.record.Reference)
		if err != nil {
			return cleanupBackfillRecord{}, Mutation{}, err
		}
		value, err := encodeCleanupPending(cleanupPendingRecord{Version: 1, Reference: index.record.Reference, TaskDigest: workerIdentityDigest(index.task.value)})
		if err != nil {
			return cleanupBackfillRecord{}, Mutation{}, err
		}
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
		writes = append(writes, Write{Key: key, Value: []byte(value)})
	}
	value, err := encodeCleanupBackfill(next)
	if err != nil {
		return cleanupBackfillRecord{}, Mutation{}, err
	}
	writes = append(writes, Write{Key: page.fence.key, Value: []byte(value)})
	return next, Mutation{Comparisons: cmps, Writes: writes}, nil
}

// Index-only wire dimensions are 45/47 nodes, at most 91136 request bytes,
// 4096 success bytes and 14592 failure bytes. This validates CRUD and size;
// the original evidence/owner validators alone authorize native dispatch.
func (b *Backend) preflightCleanupBackfillIndex(m Mutation) error {
	return b.preflightCleanupPendingMode(m, false, true)
}
