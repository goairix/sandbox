package etcd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Transition currently publishes only the preparing→prepared production edge.
// Later edges must add their reservation lease and counter changes before they
// are admitted; unsupported transitions fail explicitly without a partial
// record update.
func (r *FUSEPoolRepository) Transition(ctx context.Context, id string, from, to state.FUSEPoolState, token string, expectedRevision uint64) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil || !validPoolID(id) || !validPoolID(token) || expectedRevision == 0 ||
		from != state.FUSEPoolPreparing || to != state.FUSEPoolPrepared {
		return nil, state.ErrFUSEPoolInvalidTransition
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
	if json.Unmarshal(recordRead.Kvs[0].Value, &record) != nil || record.PreparationID != id || !validPoolID(record.PoolKey) {
		return nil, state.ErrFUSEPoolCorrupt
	}
	if record.Revision != expectedRevision {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.State != from || !validPoolID(record.RuntimeID) || !validPoolID(record.RuntimeUID) ||
		record.ReservationToken != "" {
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	if record.MaintainerToken != token {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	if record.Revision == ^uint64(0) {
		return nil, state.ErrFUSEPoolCorrupt
	}
	metaKey := r.metadataKey(record.PoolKey)
	metaRead, err := r.store.client.Get(requestCtx, metaKey)
	if err != nil {
		return nil, err
	}
	if len(metaRead.Kvs) == 0 {
		return nil, state.ErrFUSEPoolCorrupt
	}
	metadata, err := decodePoolMetadata(metaRead.Kvs[0].Value, record.PoolKey)
	if err != nil || metadata.Preparing == 0 {
		return nil, state.ErrFUSEPoolCorrupt
	}
	metadata.Preparing--
	metadata.Prepared++
	encodedMetadata, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	record.State = to
	record.Revision++
	record.UpdatedAt = time.Now().UTC()
	encodedRecord, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}

	preparingKey := r.preparingKey(record.PoolKey, id)
	preparedKey := r.preparedKey(record.PoolKey, id)
	written, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", recordRead.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.ModRevision(metaKey), "=", metaRead.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.Value(preparingKey), "=", id),
			clientv3.Compare(clientv3.Version(preparedKey), "=", 0),
			clientv3.Compare(clientv3.Value(r.uidOwnerKey(record.RuntimeUID)), "=", id),
			clientv3.Compare(clientv3.Version(r.prepareDeadlineKey(id)), ">", 0)).
		Then(clientv3.OpPut(recordKey, string(encodedRecord)),
			clientv3.OpPut(metaKey, string(encodedMetadata)),
			clientv3.OpDelete(preparingKey), clientv3.OpPut(preparedKey, id)).Commit()
	if err != nil {
		return nil, err
	}
	if !written.Succeeded {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	return &record, nil
}
