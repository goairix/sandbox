package etcd

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// DeleteCleanup removes a claimed inventory record after the caller has
// completed physical runtime removal. The repository cannot infer physical
// termination from etcd metadata; it only fences the exact cleanup token,
// revision, state index and UID owner before dropping the recovery anchor.
func (r *FUSEPoolRepository) DeleteCleanup(ctx context.Context, id, token string, expectedRevision uint64) (bool, error) {
	if r == nil || r.store == nil {
		return false, errors.New("fuse pool repository: nil store")
	}
	if !validPoolID(id) || !validPoolID(token) || expectedRevision == 0 {
		return false, state.ErrFUSEPoolInvalidRecord
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	recordKey := r.recordKey(id)
	read, err := r.store.client.Get(requestCtx, recordKey)
	if err != nil {
		return false, err
	}
	if len(read.Kvs) == 0 {
		return true, nil
	}
	record, err := decodePoolRecord(read.Kvs[0].Value, id)
	if err != nil {
		return false, err
	}
	if record.State != state.FUSEPoolCleanup {
		return false, state.ErrFUSEPoolConflict
	}
	if record.Revision != expectedRevision {
		return false, state.ErrFUSEPoolCASMismatch
	}
	if record.CleanupToken != token {
		return false, state.ErrFUSEPoolTokenMismatch
	}

	metaKey := r.metadataKey(record.PoolKey)
	metaRead, err := r.store.client.Get(requestCtx, metaKey)
	if err != nil {
		return false, err
	}
	if len(metaRead.Kvs) != 1 {
		return false, state.ErrFUSEPoolCorrupt
	}
	metadata, err := decodePoolMetadata(metaRead.Kvs[0].Value, record.PoolKey)
	if err != nil || metadata.Total == 0 || metadata.Cleanup == 0 || metadata.Generation == ^uint64(0) {
		return false, state.ErrFUSEPoolCorrupt
	}
	metadata.Total--
	metadata.Cleanup--
	metadata.Generation++
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return false, err
	}

	// Other state indexes must be absent. A malformed duplicate index must
	// not survive deletion and make a later bounded scan appear consistent.
	conditions := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", read.Kvs[0].ModRevision),
		clientv3.Compare(clientv3.ModRevision(metaKey), "=", metaRead.Kvs[0].ModRevision),
		clientv3.Compare(clientv3.Value(r.cleanupKey(record.PoolKey, id)), "=", id),
		clientv3.Compare(clientv3.Version(r.preparingKey(record.PoolKey, id)), "=", 0),
		clientv3.Compare(clientv3.Version(r.preparedKey(record.PoolKey, id)), "=", 0),
		clientv3.Compare(clientv3.Version(r.reservedKey(record.PoolKey, id)), "=", 0),
		clientv3.Compare(clientv3.Version(r.bindingKey(record.PoolKey, id)), "=", 0),
		clientv3.Compare(clientv3.Version(r.consumedKey(record.PoolKey, id)), "=", 0),
	}
	operations := []clientv3.Op{
		clientv3.OpPut(metaKey, string(encodedMetadata)),
		clientv3.OpDelete(recordKey),
		clientv3.OpDelete(r.cleanupKey(record.PoolKey, id)),
		clientv3.OpDelete(r.cleanupDeadlineKey(id)),
		clientv3.OpDelete(r.prepareDeadlineKey(id)),
		clientv3.OpDelete(r.reservationDeadlineKey(id)),
	}
	if record.RuntimeUID != "" {
		conditions = append(conditions, clientv3.Compare(clientv3.Value(r.uidOwnerKey(record.RuntimeUID)), "=", id))
		operations = append(operations, clientv3.OpDelete(r.uidOwnerKey(record.RuntimeUID)))
	}
	written, err := r.store.client.Txn(requestCtx).If(conditions...).Then(operations...).Commit()
	if err != nil {
		return false, err
	}
	if !written.Succeeded {
		return false, state.ErrFUSEPoolCASMismatch
	}
	return true, nil
}
