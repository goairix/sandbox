package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ReturnPreparedWithAdmission puts a live reservation back into the pool only
// when the aggregate preparing+prepared count still fits maxSize. The
// reservation lease, record and both state indexes change in one transaction;
// an ambiguous commit is left for the caller's exact-record readback.
func (r *FUSEPoolRepository) ReturnPreparedWithAdmission(ctx context.Context, id, token string, expectedRevision uint64, maxSize int) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("fuse pool repository: nil store")
	}
	if !validPoolID(id) || !validPoolID(token) || expectedRevision == 0 || maxSize <= 0 {
		return nil, state.ErrFUSEPoolInvalidRecord
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()

	recordKey := r.recordKey(id)
	recordRead, err := r.store.client.Get(requestCtx, recordKey)
	if err != nil {
		return nil, err
	}
	if len(recordRead.Kvs) == 0 {
		return nil, state.ErrFUSEPoolNotFound
	}
	var record state.FUSEPoolRecord
	if json.Unmarshal(recordRead.Kvs[0].Value, &record) != nil || record.PreparationID != id ||
		!validPoolID(record.PoolKey) || !validPoolID(record.RuntimeID) ||
		!validPoolID(record.RuntimeUID) || !validPoolID(record.MaintainerToken) ||
		record.Revision == ^uint64(0) {
		return nil, state.ErrFUSEPoolCorrupt
	}
	if record.Revision != expectedRevision {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.State != state.FUSEPoolReserved || record.ReservationToken == "" ||
		record.ReservedUntil.IsZero() {
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	if record.ReservationToken != token {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	deadlineKey := r.reservationDeadlineKey(id)
	deadlineRead, err := r.store.client.Get(requestCtx, deadlineKey)
	if err != nil {
		return nil, err
	}
	if len(deadlineRead.Kvs) == 0 {
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	if string(deadlineRead.Kvs[0].Value) != id {
		return nil, state.ErrFUSEPoolCorrupt
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
	if metadata.Preparing+metadata.Prepared >= uint64(maxSize) {
		return nil, state.ErrFUSEPoolConflict
	}
	if err := metadata.moveState(state.FUSEPoolReserved, state.FUSEPoolPrepared); err != nil {
		return nil, err
	}
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	record.State = state.FUSEPoolPrepared
	record.ReservationToken = ""
	record.ReservedUntil = time.Time{}
	record.UpdatedAt = time.Now().UTC()
	record.Revision++
	encodedRecord, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}

	reservedKey := r.reservedKey(record.PoolKey, id)
	preparedKey := r.preparedKey(record.PoolKey, id)
	written, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", recordRead.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.ModRevision(metaKey), "=", metaRead.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.ModRevision(deadlineKey), "=", deadlineRead.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.Value(reservedKey), "=", id),
			clientv3.Compare(clientv3.Version(preparedKey), "=", 0),
			clientv3.Compare(clientv3.Value(r.uidOwnerKey(record.RuntimeUID)), "=", id)).
		Then(clientv3.OpPut(metaKey, string(encodedMetadata)),
			clientv3.OpPut(recordKey, string(encodedRecord)),
			clientv3.OpDelete(deadlineKey), clientv3.OpDelete(reservedKey),
			clientv3.OpPut(preparedKey, id)).Commit()
	if err != nil {
		return nil, err
	}
	if !written.Succeeded {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	return &record, nil
}
