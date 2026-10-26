package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var errCheckoutMetadataContention = errors.New("fuse pool aggregate changed independently")

// transitionCheckout touches one record and the shared pool aggregate. Another
// sandbox can change only the aggregate between our reads and commit; retry
// that proven no-write conflict within the existing request deadline. A lost
// commit reply or a changed record is never retried as a fresh effect.
func (r *FUSEPoolRepository) transitionCheckout(ctx context.Context, id string, from, to state.FUSEPoolState, token string, expectedRevision uint64) (*state.FUSEPoolRecord, error) {
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()

	const maxAggregateAttempts = 16
	for attempt := 0; attempt < maxAggregateAttempts; attempt++ {
		record, err := r.transitionCheckoutOnce(requestCtx, id, from, to, token, expectedRevision)
		if !errors.Is(err, errCheckoutMetadataContention) {
			return record, err
		}
		if err := requestCtx.Err(); err != nil {
			return nil, err
		}
	}
	return nil, state.ErrFUSEPoolCASMismatch
}

// transitionCheckoutOnce separates the two checkout edges from preparation's
// count-changing transaction. Starting a bind requires a live reservation
// lease; finishing an already-bound checkout remains possible after expiry so
// a slow runtime handoff does not become an unresolvable partial effect.
func (r *FUSEPoolRepository) transitionCheckoutOnce(ctx context.Context, id string, from, to state.FUSEPoolState, token string, expectedRevision uint64) (*state.FUSEPoolRecord, error) {
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
	metaKey := r.metadataKey(record.PoolKey)
	metaRead, err := r.store.client.Get(requestCtx, metaKey)
	if err != nil {
		return nil, err
	}
	if len(metaRead.Kvs) != 1 {
		return nil, state.ErrFUSEPoolCorrupt
	}
	metadata, err := decodePoolMetadata(metaRead.Kvs[0].Value, record.PoolKey)
	if err != nil {
		return nil, err
	}
	if err := metadata.moveState(from, to); err != nil {
		return nil, err
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	conditions := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", read.Kvs[0].ModRevision),
		clientv3.Compare(clientv3.ModRevision(metaKey), "=", metaRead.Kvs[0].ModRevision),
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
	if to == state.FUSEPoolConsumed {
		conditions = append(conditions, clientv3.Compare(clientv3.Version(r.unpublishedKey(record.PoolKey, id)), "=", 0))
	}

	record.State = to
	record.Revision++
	record.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	operations := []clientv3.Op{
		clientv3.OpPut(metaKey, string(encodedMetadata)),
		clientv3.OpPut(recordKey, string(raw)),
		clientv3.OpDelete(fromKey), clientv3.OpPut(toKey, id),
	}
	if to == state.FUSEPoolConsumed {
		operations = append(operations, clientv3.OpPut(r.unpublishedKey(record.PoolKey, id), id))
	}
	written, err := r.store.client.Txn(requestCtx).
		If(conditions...).
		Then(operations...).
		Else(clientv3.OpGet(recordKey), clientv3.OpGet(metaKey)).Commit()
	if err != nil {
		return nil, err
	}
	if !written.Succeeded {
		if len(written.Responses) != 2 ||
			written.Responses[0].GetResponseRange() == nil ||
			written.Responses[1].GetResponseRange() == nil {
			return nil, state.ErrFUSEPoolCorrupt
		}
		records := written.Responses[0].GetResponseRange().Kvs
		metas := written.Responses[1].GetResponseRange().Kvs
		if len(records) == 1 && len(metas) == 1 &&
			records[0].ModRevision == read.Kvs[0].ModRevision &&
			metas[0].ModRevision != metaRead.Kvs[0].ModRevision {
			return nil, errCheckoutMetadataContention
		}
		return nil, state.ErrFUSEPoolCASMismatch
	}
	return &record, nil
}
