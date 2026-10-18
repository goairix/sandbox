package etcd

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// taskQuiescenceReservationPin is a fixed-purpose private native MOD fence.
// It never enters the business mutation or changes its digest/budget.
type taskQuiescenceReservationPin struct {
	key      string
	revision int64
}

func (p taskQuiescenceReservationPin) comparison() clientv3.Cmp {
	return clientv3.Compare(clientv3.ModRevision(p.key), "=", p.revision)
}
func (b *Backend) reservationComparisons(c *TaskClaim, d *taskQuiescenceDraft) []clientv3.Cmp {
	cmps := append(b.baseComparisons(), c.comparisons()...)
	if d.reservation.revision > 0 {
		cmps = append(cmps, d.reservation.comparison())
	}
	return cmps
}

// LoadTaskQuiescenceAttempt returns owned diagnostic attribution only. In
// particular, no digest/deadline/provider or Stage capability is reconstructed.
func (b *Backend) LoadTaskQuiescenceAttempt(ctx context.Context, task TaskReference) (*TaskQuiescenceAttemptEntry, error) {
	if b == nil || ctx == nil {
		return nil, ErrInvalidRecord
	}
	if err := b.validateTaskReference(task); err != nil {
		return nil, err
	}
	key, err := b.namespace.taskQuiescenceAttemptKey(task)
	if err != nil {
		return nil, err
	}
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	points, err := b.readQuiescencePoints(bounded, nil, []string{b.identityKey, b.restoreKey, key})
	if err != nil {
		return nil, err
	}
	return taskQuiescenceAttemptEntry(points[2], task)
}
func taskQuiescenceAttemptEntry(kv *mvccpb.KeyValue, task TaskReference) (*TaskQuiescenceAttemptEntry, error) {
	if kv == nil {
		return nil, nil
	}
	var record TaskQuiescenceAttemptRecord
	if err := decodeTaskQuiescenceAttempt(kv, &record); err != nil {
		return nil, err
	}
	if record.Task.Reference != task {
		return nil, ErrCorruptRecord
	}
	return &TaskQuiescenceAttemptEntry{Reference: record.reference(), Record: &record, Revision: kv.ModRevision}, nil
}
func (b *Backend) readOriginalTaskQuiescenceAttempt(ctx context.Context, c *TaskClaim, d *taskQuiescenceDraft, key string) (*mvccpb.KeyValue, error) {
	if err := taskQuiescenceLive(ctx, c, d); err != nil {
		return nil, err
	}
	ops := []clientv3.Op{clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey), clientv3.OpGet(key)}
	response, err := b.client.Txn(ctx).If(append(b.baseComparisons(), c.comparisons()...)...).Then(ops...).Else(ops...).Commit()
	if live := taskQuiescenceLive(ctx, c, d); live != nil {
		return nil, live
	}
	if err != nil {
		return nil, fmt.Errorf("%w: read task reservation: %w", ErrOutcomeUnknown, err)
	}
	points, err := b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey, key})
	if err != nil {
		return nil, err
	}
	if !response.Succeeded {
		c.lost = true
		return nil, ErrConflict
	}
	return points[2], nil
}
func (b *Backend) pinOriginalTaskQuiescenceAttempt(c *TaskClaim, d *taskQuiescenceDraft, key string, kv *mvccpb.KeyValue) error {
	entry, err := taskQuiescenceAttemptEntry(kv, c.reference.Task)
	if err != nil {
		d.lost = true
		return err
	}
	if entry == nil {
		return ErrOutcomeUnknown
	}
	if !d.reservationSent || string(kv.Value) != d.reservationValue || entry.Reference != d.attemptReference || entry.Record.Task != d.task || entry.Record.Claim != c.reference || entry.Record.DestroyingRevision != c.birth {
		d.lost = true
		return ErrConflict
	}
	if d.reservation.revision > 0 && d.reservation.revision != entry.Revision {
		d.lost = true
		return ErrConflict
	}
	d.reservation = taskQuiescenceReservationPin{key: key, revision: entry.Revision}
	return nil
}
func (b *Backend) reserveTaskQuiescenceAttempt(ctx context.Context, c *TaskClaim, d *taskQuiescenceDraft) error {
	if err := taskQuiescenceLive(ctx, c, d); err != nil {
		return err
	}
	key, err := b.namespace.taskQuiescenceAttemptKey(c.reference.Task)
	if err != nil {
		return err
	}
	kv, err := b.readOriginalTaskQuiescenceAttempt(ctx, c, d, key)
	if err != nil {
		return err
	}
	if kv != nil {
		return b.pinOriginalTaskQuiescenceAttempt(c, d, key, kv)
	}
	if d.reservationSent {
		// The original candidate is fixed before Stage Begin. Absence is
		// expected until that same Stage atomically commits reservation + Q;
		// it never authorizes another command/locator or business resubmission.
		if d.reservation.revision != 0 {
			return ErrOutcomeUnknown
		}
		return nil
	}
	locator := StageAttemptLocator{Namespace: b.namespace.Root(), Partition: c.reference.Task.Partition, RequestID: c.reference.ClaimID, StageID: "task_quiesce_users", AttemptID: uuid.NewString(), RestoreEpoch: b.restoreEpoch}
	record := TaskQuiescenceAttemptRecord{Version: 1, Task: d.task, Claim: c.reference, DestroyingRevision: c.birth, CommandID: d.commandID, Attempt: locator}
	value, err := encodeTaskQuiescenceAttempt(record)
	if err != nil {
		return err
	}
	d.attemptReference = record.reference()
	d.reservationValue = value
	d.reservation = taskQuiescenceReservationPin{key: key}
	// Despite the legacy field name, this now latches candidate identity only.
	// No permanent reservation exists before the original Stage Commit.
	d.reservationSent = true
	return nil
}
