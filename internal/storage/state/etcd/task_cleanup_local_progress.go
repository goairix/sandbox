package etcd

import (
	"context"
	"errors"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// cleanupLocalProgress holds only one original submission's immutable bytes
// and full E/S fences. It shares the job's actor-to-physical ownership transfer;
// no pointer into mutable caller input or new runtime permission is retained.
type cleanupLocalProgress struct {
	execution taskFence
	slot      taskFence
	value     string
}

// A live registry entry, not a matching Run copied from storage, identifies the
// original post-work owner. Close/cancellation may seal admission while waiting
// for this job, so neither is a reason to prevent its actual joined marker.
func (b *Backend) validateCleanupJoinOwner(j *cleanupLocalJob) error {
	if b == nil || b.archiveLife == nil || j == nil || j.worker == nil || j.worker.backend != b || j.worker.self != j.worker || !j.effectsJoined || j.runID == "" || j.state == nil || j.state.record.Task != j.task {
		return ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	for _, registered := range j.worker.cleanupJobs {
		if registered == j {
			return nil
		}
	}
	return ErrInvalidRecord
}

// reconcileCleanupJoinedState makes at most one bounded E/S read after actual
// work joins. Only the old full fence or this job's sole submitted value may
// advance its observation. A foreign value, slot ABA or uncertain read retains
// the occupation. The following joined CAS arbitrates any late original write;
// neither this function nor its caller resends that progress or physical work.
func (b *Backend) reconcileCleanupJoinedState(ctx context.Context, j *cleanupLocalJob) error {
	if err := b.validateCleanupJoinOwner(j); err != nil {
		return err
	}
	p := j.pendingProgress
	if p == nil {
		return nil
	}
	s := j.state
	if ctx == nil || s.slot == nil || s.record.Run == nil || s.record.Run.Joined || s.record.Run.ID != j.runID || p.execution != s.execution || p.slot != *s.slot || b.validateCleanupState(s) != nil {
		return ErrInvalidRecord
	}
	keys := []string{p.execution.key, p.slot.key}
	values := []string{cleanupWirePadding(cleanupExecutionBytes), cleanupWirePadding(cleanupExecutionBytes)}
	// This is the existing two-point E/S request, with the same actual protobuf
	// limit check and strict response decoder as charged cleanupReadPoints.
	projection := b.cleanupProjection("point", b.baseComparisons(), taskProtoPoints(keys), taskProtoResponse(keys, values))
	if err := validateCleanupWire(projection); err != nil {
		return err
	}
	rpc, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(rpc).If(b.baseComparisons()...).Then(workspaceSyncPointOps(keys)...).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	points, revision, err := b.decodeCleanupPoints(response, keys)
	if err != nil {
		return err
	}
	observed, err := b.cleanupStateFromPoints(j.task, points[0], points[1], revision)
	if err != nil {
		return err
	}
	if observed.execution.key != p.execution.key || observed.execution.create != p.execution.create || observed.execution.lease != p.execution.lease || observed.slot == nil || *observed.slot != p.slot || observed.record.Incarnation != s.record.Incarnation || observed.record.Run == nil || *observed.record.Run != *s.record.Run {
		return ErrConflict
	}
	if observed.execution != p.execution && (observed.execution.value != p.value || observed.execution.mod <= p.execution.mod) {
		return ErrConflict
	}
	if err = b.validateCleanupJoinOwner(j); err != nil {
		return err
	}
	j.state = observed
	return nil
}
