package etcd

import (
	"context"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// DeleteController removes a drained sandbox under the exact live controller.
// An already absent record is idempotent; callers needing a new-completion
// signal must use DeleteControllerWithResult.
func (r *ActiveSandboxRepository) DeleteController(ctx context.Context, lease state.ActiveSandboxControllerLease, expectedRevision uint64) error {
	_, err := r.DeleteControllerWithResult(ctx, lease, expectedRevision)
	return err
}

// DeleteControllerWithResult reports true only after etcd acknowledges the
// atomic deletion. A lost reply remains unknown; it cannot be counted as a
// new completion even if a subsequent read finds historical absence.
func (r *ActiveSandboxRepository) DeleteControllerWithResult(ctx context.Context, lease state.ActiveSandboxControllerLease, expectedRevision uint64) (bool, error) {
	if !validActiveID(lease.SandboxID) || lease.Token == "" || lease.InstanceID == "" || lease.Generation <= 0 || expectedRevision == 0 {
		return false, state.ErrActiveSandboxCorrupt
	}

	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	recordKey := r.recordKey(lease.SandboxID)
	controllerKey := r.controllerKey(lease.SandboxID)
	read, err := r.store.client.Txn(requestCtx).
		Then(clientv3.OpGet(recordKey), clientv3.OpGet(controllerKey)).Commit()
	if err != nil {
		return false, err
	}
	if len(read.Responses) != 2 || read.Responses[0].GetResponseRange() == nil || read.Responses[1].GetResponseRange() == nil {
		return false, state.ErrActiveSandboxCorrupt
	}
	records := read.Responses[0].GetResponseRange().Kvs
	if len(records) == 0 {
		return false, nil
	}
	controllers := read.Responses[1].GetResponseRange().Kvs
	if len(controllers) == 0 {
		return false, state.ErrActiveSandboxStaleToken
	}
	current, err := decodeController(controllers[0].Value, lease.SandboxID)
	if err != nil {
		return false, err
	}
	if current.Token != lease.Token || current.InstanceID != lease.InstanceID || current.PodUID != lease.PodUID || current.Generation != lease.Generation {
		return false, state.ErrActiveSandboxStaleToken
	}
	envelope, err := decodeActiveEnvelope(records[0].Value, lease.SandboxID)
	if err != nil {
		return false, err
	}
	if envelope.Record.Generation != lease.Generation || envelope.Record.Revision != expectedRevision ||
		(envelope.Record.Phase != state.ActiveSandboxDestroying && envelope.Record.Phase != state.ActiveSandboxCleanupPending) {
		return false, state.ErrActiveSandboxConflict
	}

	// The ranged Version comparison is true only when no leased operation
	// remains at the commit revision. BeginDestroy already closed admission,
	// so no new operation can slip between this drain check and deletion.
	indexKey := r.indexKey(envelope.Cursor)
	indexCompares, indexWrites, err := r.lifecycleIndexMutation(&envelope, nil)
	if err != nil {
		return false, err
	}
	operationAbsent := clientv3.Compare(clientv3.Version(r.operationPrefix(lease.SandboxID)), "=", 0).WithPrefix()
	compares := append([]clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", records[0].ModRevision),
		clientv3.Compare(clientv3.ModRevision(controllerKey), "=", controllers[0].ModRevision),
		clientv3.Compare(clientv3.Value(indexKey), "=", lease.SandboxID),
		operationAbsent,
		clientv3.Compare(clientv3.Version(r.mutationKey(lease.SandboxID)), "=", 0),
		clientv3.Compare(clientv3.Version(r.exclusiveKey(lease.SandboxID)), "=", 0),
	}, indexCompares...)
	writes := append([]clientv3.Op{clientv3.OpDelete(recordKey), clientv3.OpDelete(controllerKey), clientv3.OpDelete(indexKey)}, indexWrites...)
	written, err := r.store.client.Txn(requestCtx).
		If(compares...).Then(writes...).
		Else(clientv3.OpGet(controllerKey)).Commit()
	if err != nil {
		return false, err
	}
	if !written.Succeeded {
		if len(written.Responses) != 1 || written.Responses[0].GetResponseRange() == nil {
			return false, state.ErrActiveSandboxCorrupt
		}
		currentControllers := written.Responses[0].GetResponseRange().Kvs
		if len(currentControllers) == 0 || currentControllers[0].ModRevision != controllers[0].ModRevision {
			return false, state.ErrActiveSandboxStaleToken
		}
		return false, state.ErrActiveSandboxConflict
	}
	return true, nil
}
