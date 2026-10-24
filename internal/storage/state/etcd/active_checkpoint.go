package etcd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Checkpoint advances durable cleanup progress at the exact lifecycle
// revision. Physical termination is a separate prerequisite enforced by the
// cleanup caller; this method never infers it from lease expiry.
func (r *ActiveSandboxRepository) Checkpoint(ctx context.Context, id string, expectedRevision uint64, checkpoint string) (*state.ActiveSandboxRecord, error) {
	if !validActiveID(id) || expectedRevision == 0 || checkpoint == "" {
		return nil, state.ErrActiveSandboxCorrupt
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	key := r.recordKey(id)
	read, err := r.store.client.Get(requestCtx, key)
	if err != nil || len(read.Kvs) == 0 {
		return nil, err
	}
	envelope, err := decodeActiveEnvelope(read.Kvs[0].Value, id)
	if err != nil {
		return nil, err
	}
	if envelope.Record.Revision != expectedRevision ||
		(envelope.Record.Phase != state.ActiveSandboxDestroying && envelope.Record.Phase != state.ActiveSandboxCleanupPending) {
		return nil, state.ErrActiveSandboxConflict
	}
	previous := envelope
	envelope.Record.Phase = state.ActiveSandboxCleanupPending
	envelope.Record.Revision++
	envelope.Record.CleanupCheckpoint = checkpoint
	envelope.Record.UpdatedAt = time.Now().UTC()
	indexCompares, indexWrites, err := r.lifecycleIndexMutation(&previous, &envelope)
	if err != nil {
		return nil, err
	}
	next, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	compares := append([]clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", read.Kvs[0].ModRevision)}, indexCompares...)
	writes := append([]clientv3.Op{clientv3.OpPut(key, string(next))}, indexWrites...)
	written, err := r.store.client.Txn(requestCtx).If(compares...).Then(writes...).Commit()
	if err != nil {
		return nil, err
	}
	if !written.Succeeded {
		return nil, state.ErrActiveSandboxConflict
	}
	return &envelope.Record, nil
}
