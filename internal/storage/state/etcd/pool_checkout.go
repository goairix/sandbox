package etcd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// transitionCheckout separates the two checkout edges from preparation's
// count-changing transaction. Starting a bind requires a live reservation
// lease; finishing an already-bound checkout remains possible after expiry so
// a slow runtime handoff does not become an unresolvable partial effect.
func (r *FUSEPoolRepository) transitionCheckout(ctx context.Context, id string, from, to state.FUSEPoolState, token string, expectedRevision uint64) (*state.FUSEPoolRecord, error) {
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
	if json.Unmarshal(read.Kvs[0].Value, &record) != nil || record.PreparationID != id ||
		!validPoolID(record.PoolKey) || !validPoolID(record.RuntimeID) ||
		!validPoolID(record.RuntimeUID) || record.Revision == ^uint64(0) ||
		record.ReservationToken == "" || record.ReservedUntil.IsZero() {
		return nil, state.ErrFUSEPoolCorrupt
	}
	if record.Revision != expectedRevision {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.State != from {
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	if record.ReservationToken != token {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	fromKey := r.reservedKey(record.PoolKey, id)
	toKey := r.bindingKey(record.PoolKey, id)
	if from == state.FUSEPoolBinding {
		fromKey = toKey
		toKey = r.consumedKey(record.PoolKey, id)
	}

	conditions := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", read.Kvs[0].ModRevision),
		clientv3.Compare(clientv3.Value(fromKey), "=", id),
		clientv3.Compare(clientv3.Version(toKey), "=", 0),
		clientv3.Compare(clientv3.Value(r.uidOwnerKey(record.RuntimeUID)), "=", id),
	}
	if from == state.FUSEPoolReserved {
		deadlineKey := r.reservationDeadlineKey(id)
		deadline, err := r.store.client.Get(requestCtx, deadlineKey)
		if err != nil {
			return nil, err
		}
		if len(deadline.Kvs) == 0 {
			return nil, state.ErrFUSEPoolInvalidTransition
		}
		if string(deadline.Kvs[0].Value) != id {
			return nil, state.ErrFUSEPoolCorrupt
		}
		conditions = append(conditions, clientv3.Compare(clientv3.ModRevision(deadlineKey), "=", deadline.Kvs[0].ModRevision))
	}

	record.State = to
	record.Revision++
	record.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	written, err := r.store.client.Txn(requestCtx).
		If(conditions...).
		Then(clientv3.OpPut(recordKey, string(raw)),
			clientv3.OpDelete(fromKey), clientv3.OpPut(toKey, id)).Commit()
	if err != nil {
		return nil, err
	}
	if !written.Succeeded {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	return &record, nil
}
