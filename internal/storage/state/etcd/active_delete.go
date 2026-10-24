package etcd

import (
	"context"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Delete removes a closed, drained lifecycle record at its exact generation
// and revision. The caller remains responsible for proving physical cleanup;
// absence of an operation lease alone does not prove that a sandbox exited.
// Historical absence is idempotent, but an unknown commit result is returned
// to the caller without a local retry.
func (r *ActiveSandboxRepository) Delete(ctx context.Context, id string, expectedRevision uint64, generation int64) error {
	if !validActiveID(id) || expectedRevision == 0 || generation <= 0 {
		return state.ErrActiveSandboxCorrupt
	}

	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	recordKey := r.recordKey(id)
	read, err := r.store.client.Get(requestCtx, recordKey)
	if err != nil || len(read.Kvs) == 0 {
		return err
	}
	envelope, err := decodeActiveEnvelope(read.Kvs[0].Value, id)
	if err != nil {
		return err
	}
	if envelope.Record.Generation != generation || envelope.Record.Revision != expectedRevision ||
		(envelope.Record.Phase != state.ActiveSandboxDestroying && envelope.Record.Phase != state.ActiveSandboxCleanupPending) {
		return state.ErrActiveSandboxConflict
	}

	// The ranged comparison and deletes are one server transaction. Closing
	// admission precedes this call, so no new operation may enter after the
	// drain check; expired leases are absent at the server's revision.
	indexKey := r.indexKey(envelope.Cursor)
	operationAbsent := clientv3.Compare(clientv3.Version(r.operationPrefix(id)), "=", 0).WithPrefix()
	written, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", read.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.Value(indexKey), "=", id),
			operationAbsent,
			clientv3.Compare(clientv3.Version(r.mutationKey(id)), "=", 0),
			clientv3.Compare(clientv3.Version(r.exclusiveKey(id)), "=", 0)).
		Then(clientv3.OpDelete(recordKey), clientv3.OpDelete(indexKey),
			clientv3.OpDelete(r.controllerKey(id))).Commit()
	if err != nil {
		return err
	}
	if !written.Succeeded {
		return state.ErrActiveSandboxConflict
	}
	return nil
}
