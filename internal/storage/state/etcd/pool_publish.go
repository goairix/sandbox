package etcd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// transitionPreparing is shared by the public transition and the production
// refill-locked path. The record, aggregate count and state index always move
// together; a cold reservation adds its server-expiring lease in that txn.
func (r *FUSEPoolRepository) transitionPreparing(ctx context.Context, id string,
	to state.FUSEPoolState, token, refillToken string,
	expectedRevision uint64, reservationTTL time.Duration,
) (*state.FUSEPoolRecord, error) {
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
	record, err := decodePoolRecord(read.Kvs[0].Value, id)
	if err != nil {
		return nil, err
	}
	if record.Revision != expectedRevision {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.State != state.FUSEPoolPreparing || !validPoolID(record.RuntimeID) || record.ReservationToken != "" {
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	if to == state.FUSEPoolPrepared && record.MaintainerToken != token {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	if record.Revision == ^uint64(0) {
		return nil, state.ErrFUSEPoolCorrupt
	}
	lockKey := r.refillKey(record.PoolKey)
	if refillToken != "" {
		lock, err := r.store.client.Get(requestCtx, lockKey)
		if err != nil {
			return nil, err
		}
		if len(lock.Kvs) == 0 || string(lock.Kvs[0].Value) != refillToken {
			return nil, state.ErrFUSEPoolTokenMismatch
		}
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
	if err != nil || metadata.Preparing == 0 {
		return nil, state.ErrFUSEPoolCorrupt
	}
	metadata.Preparing--
	if to == state.FUSEPoolPrepared {
		metadata.Prepared++
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	record.State = to
	record.Revision++
	now := time.Now().UTC()
	record.UpdatedAt = now
	if to == state.FUSEPoolReserved {
		record.ReservationToken = token
		record.ReservedUntil = now.Add(reservationTTL)
	}
	encodedRecord, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}

	destinationKey := r.preparedKey(record.PoolKey, id)
	conditions := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", read.Kvs[0].ModRevision),
		clientv3.Compare(clientv3.ModRevision(metaKey), "=", metaRead.Kvs[0].ModRevision),
		clientv3.Compare(clientv3.Value(r.preparingKey(record.PoolKey, id)), "=", id),
		clientv3.Compare(clientv3.Value(r.uidOwnerKey(record.RuntimeUID)), "=", id),
		clientv3.Compare(clientv3.Version(r.prepareDeadlineKey(id)), ">", 0),
	}
	if refillToken != "" {
		conditions = append(conditions, clientv3.Compare(clientv3.Value(lockKey), "=", refillToken))
	}
	operations := []clientv3.Op{
		clientv3.OpPut(recordKey, string(encodedRecord)),
		clientv3.OpPut(metaKey, string(encodedMetadata)),
		clientv3.OpDelete(r.preparingKey(record.PoolKey, id)),
	}
	if to == state.FUSEPoolReserved {
		destinationKey = r.reservedKey(record.PoolKey, id)
	}
	conditions = append(conditions, clientv3.Compare(clientv3.Version(destinationKey), "=", 0))
	operations = append(operations, clientv3.OpPut(destinationKey, id))
	var lease clientv3.LeaseID
	if to == state.FUSEPoolReserved {
		conditions = append(conditions, clientv3.Compare(clientv3.Version(r.reservationDeadlineKey(id)), "=", 0))
		lease, err = r.store.leaseFor(requestCtx, reservationTTL)
		if err != nil {
			return nil, err
		}
		operations = append(operations,
			clientv3.OpPut(r.reservationDeadlineKey(id), id, clientv3.WithLease(lease)))
	}
	written, err := r.store.client.Txn(requestCtx).If(conditions...).Then(operations...).Commit()
	if err != nil {
		return nil, err
	}
	if !written.Succeeded {
		if lease != 0 {
			_, _ = r.store.client.Revoke(requestCtx, lease)
		}
		return nil, state.ErrFUSEPoolCASMismatch
	}
	return &record, nil
}
