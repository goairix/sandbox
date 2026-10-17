package etcd

import (
	"context"
	"errors"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Cold deletion requires the original signed loader's proof and exact present
// P. Missing P is only a refused stale observation, never an unguarded success.
// No execution occupation or physical release is inferred from metadata alone.
func (b *Backend) validateCleanupPendingDelete(ctx context.Context, p *WorkerPartitionClaim, claims *WorkerBudgetShare, birth *cleanupBirthEvidence, release *cleanupBirthReleaseEvidence) error {
	if ctx == nil || !b.validWorkerBackend() {
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := b.validateCleanupBirthEvidence(p, birth); err != nil {
		return err
	}
	if birth.pending == nil || release == nil || release.self != release || release.origin != b || release.partition != p || release.birth != birth {
		return ErrInvalidRecord
	}
	if birth.execution != nil && birth.execution.Phase != "settled" {
		return ErrInvalidRecord
	}
	if err := b.validateCleanupBirthRelease(p, birth); err != nil {
		return err
	}
	if err := matchCleanupBirthReleaseProof(birth, release.proof); err != nil {
		return err
	}
	if b.requestTimeout <= 0 || b.requestTimeout > 3*time.Second {
		return ErrInvalidConfiguration
	}

	// Check the same real Claim/session before credit and again in the builder.
	// The cleanup margin reserves four request timeouts plus one second; no
	// diagnostic clock or proof can extend the original leased owner lifetime.
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerPartition(p) || !b.validWorkerBudget(claims) || claims.kind != WorkerBudgetClaim || claims.session != p.session {
		return ErrInvalidRecord
	}
	if b.archiveLife.closed {
		return context.Canceled
	}
	if p.release != nil || p.released || p.lost || claims.release != nil || claims.released || claims.lost || p.session.release != nil {
		return ErrGuardExpired
	}
	if err := p.session.live(); err != nil {
		return err
	}
	if p.session.ttlSeconds != 60 {
		return ErrInvalidConfiguration
	}
	if time.Until(p.session.deadline) <= 4*b.requestTimeout+time.Second {
		return ErrGuardExpired
	}
	return ctx.Err()
}

// One finite original Claim CAS deletes only P. It leaves B, execution, slots
// and physical cleanup untouched. Unknown outcomes lose the session and require
// fresh paid observations; finish joins actual native return even during Close.
func (b *Backend) deleteCleanupPending(ctx context.Context, p *WorkerPartitionClaim, claims *WorkerBudgetShare, birth *cleanupBirthEvidence, release *cleanupBirthReleaseEvidence) (resultErr error) {
	if err := b.validateCleanupPendingDelete(ctx, p, claims, birth, release); err != nil {
		return err
	}
	o, stop, err := b.beginExpiredTaskOwner(ctx, p, claims)
	if err != nil {
		return err
	}
	defer stop()
	defer o.finish()
	knownConflict := false
	defer func() {
		if resultErr != nil && o.call.started && !knownConflict {
			o.call.lose()
		}
	}()

	mutation, err := b.cleanupPendingDeleteMutation(o, birth, release)
	if err != nil {
		return err
	}
	if err = b.preflightCleanupPendingDelete(mutation); err != nil {
		return err
	}
	if _, err = b.cleanupOwnerComparisons(o); err != nil {
		return err
	}
	rpc, cancel := o.call.requestContext()
	if err = rpc.Err(); err != nil {
		cancel()
		return err
	}
	o.call.started = true
	response, err := b.client.Txn(rpc).If(mutation.Comparisons...).Then(clientv3.OpDelete(mutation.Writes[0].Key)).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err = b.operationResponseHeader(response); err != nil || !enrolledResponseBound(response) {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if !response.Succeeded {
		if _, err = b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey}); err != nil {
			return errors.Join(ErrOutcomeUnknown, err)
		}
		if err = o.postcheck(); err != nil {
			return errors.Join(ErrOutcomeUnknown, err)
		}
		knownConflict = true
		return ErrConflict
	}
	if !cleanupBackfillAdvanceACK(response, mutation) {
		return ErrOutcomeUnknown
	}
	if err = o.postcheck(); err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	return nil
}

// The concrete mutation retains W20 and common-birth T/I/receipt fences, then
// exact E absence/settled, signed release and P fences. The existing immutable
// link was authenticated by the birth loader. No caller selects native keys.
func (b *Backend) cleanupPendingDeleteMutation(o *workerDueOwner, birth *cleanupBirthEvidence, release *cleanupBirthReleaseEvidence) (Mutation, error) {
	cmps, err := b.cleanupOwnerComparisons(o)
	if err != nil {
		return Mutation{}, err
	}
	if err = b.validateCleanupPendingDelete(o.call.ctx, o.partition, o.share, birth, release); err != nil {
		return Mutation{}, err
	}
	for _, fence := range []taskFence{birth.task, birth.intent, birth.receipt} {
		cmps = append(cmps, fence.comparisons()...)
	}
	if birth.executionFence == nil {
		key, err := b.namespace.cleanupExecutionKey(birth.record.Reference)
		if err != nil {
			return Mutation{}, err
		}
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	} else {
		cmps = append(cmps, birth.executionFence.comparisons()...)
	}
	cmps = append(cmps, birth.release.comparisons()...)
	cmps = append(cmps, birth.pending.comparisons()...)
	return Mutation{Comparisons: cmps, Writes: []Write{{Key: birth.pending.key, Delete: true}}}, nil
}
