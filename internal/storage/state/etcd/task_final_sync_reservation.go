package etcd

import (
	"context"
	"errors"
	"time"

	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ReserveTaskFinalSync reserves the original metadata producer once. It returns
// diagnostics only: there is no sender, Stage, lease or provider settlement.
func (b *Backend) ReserveTaskFinalSync(ctx context.Context, c *TaskClaim, prepared *PreparedTaskQuiescence, destination *t.Destination, in TaskFinalSyncPlan) (TaskFinalSyncReservationResult, error) {
	result := TaskFinalSyncReservationResult{Outcome: OutcomeUnknown}
	if !b.validTaskClaim(c) || ctx == nil || in.Validate() != nil || prepared == nil || prepared.self != prepared || prepared.origin != b || prepared.claim != c || prepared.claimReference != c.reference {
		return result, ErrInvalidRecord
	}
	c.mu.Lock()
	d := c.finalSyncDraft
	if d != nil {
		result.Reference = d.record.reference()
		if d.prepared != prepared || d.record.Plan != in || d.lost {
			c.mu.Unlock()
			return result, ErrConflict
		}
		if d.writeAttempted {
			defer c.mu.Unlock()
			return b.arbitrateTaskFinalSync(ctx, c, d, result)
		}
	}
	c.mu.Unlock()
	if err := b.RevalidateTaskQuiescence(ctx, c, prepared, destination); err != nil {
		return result, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.live(ctx); err != nil {
		return result, err
	}
	if !time.Now().Before(prepared.deadline) {
		return result, ErrGuardExpired
	}
	bounded, cancel := context.WithDeadline(ctx, prepared.deadline)
	defer cancel()
	request, finish := b.taskObservationContext(bounded, c)
	defer finish()
	if _, _, err := b.pinTaskDualDrain(request, c, &prepared.pin); err != nil {
		return result, err
	}
	d = c.finalSyncDraft
	if d != nil {
		result.Reference = d.record.reference()
		if d.prepared != prepared || d.record.Plan != in || d.lost {
			return result, ErrConflict
		}
		if d.writeAttempted {
			return b.arbitrateTaskFinalSync(ctx, c, d, result)
		}
	} else {
		r := TaskFinalSyncAttemptRecord{Version: 1, Task: prepared.pin.task, Claim: c.reference, DestroyingRevision: c.birth, Quiescence: prepared.pin.reference, QuiescenceRevision: prepared.pin.intent.create, ReceiptDigest: digestTaskQuiescenceProof(prepared.receipt.Wire()), Plan: in, AttemptID: uuid.NewString()}
		value, err := encodeTaskFinalSyncAttempt(r)
		if err != nil {
			return result, err
		}
		d = &taskFinalSyncDraft{prepared: prepared, record: r, value: value, deadline: prepared.deadline}
		c.finalSyncDraft = d
		result.Reference = r.reference()
	}
	key, err := b.namespace.taskFinalSyncAttemptKey(c.reference.Task)
	if err != nil {
		return result, err
	}
	cmps := taskEmptyComparisons(b, c, prepared.pin.intent, prepared.pin.receipt, prepared.pin.reservation)
	writeCmps := append(append([]clientv3.Cmp(nil), cmps...), clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	if err = b.preflightTaskReservation(writeCmps, key, d.value); err != nil {
		return result, err
	}
	keys := []string{b.identityKey, b.restoreKey, key}
	ops := []clientv3.Op{clientv3.OpGet(keys[0]), clientv3.OpGet(keys[1]), clientv3.OpGet(keys[2])}
	read, err := b.client.Txn(request).If(cmps...).Then(ops...).Else(ops...).Commit()
	if err != nil || request.Err() != nil {
		return result, errors.Join(ErrOutcomeUnknown, err, request.Err())
	}
	points, err := b.stageEvidencePoints(read, keys)
	if err != nil {
		return result, err
	}
	if !read.Succeeded {
		return result, ErrConflict
	}
	if points[2] != nil {
		d.lost = true
		return result, ErrConflict
	}
	if err = taskFinalSyncLive(request, c, d); err != nil {
		return result, err
	}
	d.writeAttempted = true // irrevocable before the native write can be sent
	response, err := b.client.Txn(request).If(writeCmps...).Then(clientv3.OpPut(key, d.value)).Else(ops...).Commit()
	if err != nil {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	if err = b.operationResponseHeader(response); err != nil {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	if !response.Succeeded {
		if _, err = b.stageEvidencePoints(response, keys); err != nil {
			return result, err
		}
		return result, ErrConflict
	}
	if !operationPutResponses(response, 1) || response.Header.Revision <= c.reference.CreateRevision || response.Header.Revision <= d.record.QuiescenceRevision {
		return result, ErrOutcomeUnknown
	}
	d.revision = response.Header.Revision
	result.Outcome = OutcomeCommitted
	return result, taskFinalSyncLive(request, c, d)
}
func taskFinalSyncLive(ctx context.Context, c *TaskClaim, d *taskFinalSyncDraft) error {
	if err := c.live(ctx); err != nil {
		return err
	}
	if !time.Now().Before(d.deadline) {
		return ErrGuardExpired
	}
	return nil
}

// Caller holds c.mu. A dead original may obtain committed attribution with an
// error, never renewed authority. An absent ambiguous attempt stays unknown.
func (b *Backend) arbitrateTaskFinalSync(ctx context.Context, c *TaskClaim, d *taskFinalSyncDraft, result TaskFinalSyncReservationResult) (TaskFinalSyncReservationResult, error) {
	key, err := b.namespace.taskFinalSyncAttemptKey(c.reference.Task)
	if err != nil {
		return result, err
	}
	cmps := taskEmptyComparisons(b, c, d.prepared.pin.intent, d.prepared.pin.receipt, d.prepared.pin.reservation)
	// The same preflight covers this 59-node read and the original 58-node write.
	writeCmps := append(append([]clientv3.Cmp(nil), cmps...), clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	if err = b.preflightTaskReservation(writeCmps, key, d.value); err != nil {
		return result, err
	}
	request, cancel := b.requestContext(ctx)
	defer cancel()
	keys := []string{b.identityKey, b.restoreKey, key}
	ops := []clientv3.Op{clientv3.OpGet(keys[0]), clientv3.OpGet(keys[1]), clientv3.OpGet(keys[2])}
	response, err := b.client.Txn(request).If(cmps...).Then(ops...).Else(ops...).Commit()
	if err != nil || request.Err() != nil {
		return result, errors.Join(ErrOutcomeUnknown, err, request.Err())
	}
	points, err := b.stageEvidencePoints(response, keys)
	if err != nil {
		return result, err
	}
	entry, err := taskFinalSyncEntry(points[2], c.reference.Task)
	if err != nil {
		d.lost = true
		return result, err
	}
	if entry == nil {
		if d.revision > 0 {
			d.lost = true
			return result, ErrConflict
		}
		return result, ErrOutcomeUnknown
	}
	if string(points[2].Value) != d.value || *entry.Record != d.record || (d.revision > 0 && entry.Revision != d.revision) {
		d.lost = true
		return result, ErrConflict
	}
	if err = b.validateTaskFinalSyncHistory(request, entry.Record, &d.prepared.pin); err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrCorruptRecord) || errors.Is(err, ErrCorruptReceipt) || errors.Is(err, ErrIdentityMismatch) {
			d.lost = true
		}
		return result, err
	}
	d.revision = entry.Revision
	result.Outcome = OutcomeCommitted
	if !response.Succeeded {
		return result, ErrConflict
	}
	return result, taskFinalSyncLive(ctx, c, d)
}
