package etcd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// BindPreparingRuntime attaches the runtime UID to one preparing intent. The
// UID owner, record, live preparation deadline and refill holder are compared
// in one transaction. A lost write reply is returned as unknown; the caller
// can read the exact preparation record before deciding any recovery step.
func (r *FUSEPoolRepository) BindPreparingRuntime(ctx context.Context, id, runtimeID, runtimeUID, refillToken string, expectedRevision uint64) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil || !validPoolID(id) || !validPoolID(runtimeID) ||
		!validPoolID(runtimeUID) || !validPoolID(refillToken) || expectedRevision == 0 {
		return nil, state.ErrFUSEPoolInvalidRecord
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	recordKey := r.recordKey(id)
	read, err := r.store.client.Get(requestCtx, recordKey)
	if err != nil {
		return nil, err
	}
	if len(read.Kvs) == 0 {
		return nil, state.ErrFUSEPoolNotFound
	}
	var record state.FUSEPoolRecord
	if json.Unmarshal(read.Kvs[0].Value, &record) != nil || record.PreparationID != id || !validPoolID(record.PoolKey) {
		return nil, state.ErrFUSEPoolCorrupt
	}
	if record.Revision != expectedRevision {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.State != state.FUSEPoolPreparing || record.RuntimeID != "" || record.RuntimeUID != "" {
		return nil, state.ErrFUSEPoolConflict
	}
	lockKey := r.refillKey(record.PoolKey)
	lock, err := r.store.client.Get(requestCtx, lockKey)
	if err != nil {
		return nil, err
	}
	if len(lock.Kvs) == 0 || string(lock.Kvs[0].Value) != refillToken {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	if record.Revision == ^uint64(0) {
		return nil, state.ErrFUSEPoolCorrupt
	}
	record.RuntimeID = runtimeID
	record.RuntimeUID = runtimeUID
	record.Revision++
	record.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}

	ownerKey := r.uidOwnerKey(runtimeUID)
	written, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", read.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.Value(lockKey), "=", refillToken),
			clientv3.Compare(clientv3.Value(r.preparingKey(record.PoolKey, id)), "=", id),
			clientv3.Compare(clientv3.Version(r.prepareDeadlineKey(id)), ">", 0),
			clientv3.Compare(clientv3.Version(ownerKey), "=", 0)).
		Then(clientv3.OpPut(recordKey, string(raw)), clientv3.OpPut(ownerKey, id)).
		Else(clientv3.OpGet(lockKey), clientv3.OpGet(ownerKey), clientv3.OpGet(recordKey)).Commit()
	if err != nil {
		return nil, err
	}
	if written.Succeeded {
		return &record, nil
	}
	if len(written.Responses) != 3 || written.Responses[0].GetResponseRange() == nil ||
		written.Responses[1].GetResponseRange() == nil || written.Responses[2].GetResponseRange() == nil {
		return nil, state.ErrFUSEPoolCorrupt
	}
	locks := written.Responses[0].GetResponseRange().Kvs
	if len(locks) == 0 || string(locks[0].Value) != refillToken {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	if len(written.Responses[1].GetResponseRange().Kvs) != 0 {
		return nil, state.ErrFUSEPoolConflict
	}
	return nil, state.ErrFUSEPoolCASMismatch
}
