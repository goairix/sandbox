package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ReservePrepared claims one prepared record without traversing resident
// sandboxes. The reservation deadline is a leased key: ReservedUntil is only
// a scheduling hint and cannot authorize use after the server lease expires.
func (r *FUSEPoolRepository) ReservePrepared(ctx context.Context, poolKey, token string, ttl time.Duration) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("fuse pool repository: nil store")
	}
	if !validPoolID(poolKey) || !validPoolID(token) || ttl <= 0 {
		return nil, state.ErrFUSEPoolInvalidRecord
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()

	metaKey := r.metadataKey(poolKey)
	preparedPrefix := r.poolBase(poolKey) + "state/prepared/"
	for attempt := 0; attempt < 4; attempt++ {
		// Read the count and first candidate at one MVCC revision. A failed
		// CAS retries this bounded selection, never a whole-pool scan.
		selection, err := r.store.client.Txn(requestCtx).
			Then(clientv3.OpGet(metaKey), clientv3.OpGet(preparedPrefix,
				clientv3.WithPrefix(), clientv3.WithLimit(1))).Commit()
		if err != nil {
			return nil, err
		}
		if len(selection.Responses) != 2 || selection.Responses[0].GetResponseRange() == nil ||
			selection.Responses[1].GetResponseRange() == nil {
			return nil, state.ErrFUSEPoolCorrupt
		}
		metaKVs := selection.Responses[0].GetResponseRange().Kvs
		candidates := selection.Responses[1].GetResponseRange().Kvs
		if len(metaKVs) == 0 {
			if len(candidates) != 0 {
				return nil, state.ErrFUSEPoolCorrupt
			}
			return nil, nil
		}
		metadata, err := decodePoolMetadata(metaKVs[0].Value, poolKey)
		if err != nil {
			return nil, err
		}
		if metadata.Prepared == 0 {
			if len(candidates) != 0 {
				return nil, state.ErrFUSEPoolCorrupt
			}
			return nil, nil
		}
		if len(candidates) != 1 || !validPoolID(string(candidates[0].Value)) {
			return nil, state.ErrFUSEPoolCorrupt
		}

		id := string(candidates[0].Value)
		recordKey := r.recordKey(id)
		read, err := r.store.client.Get(requestCtx, recordKey, clientv3.WithRev(selection.Header.Revision))
		if err != nil {
			return nil, err
		}
		if len(read.Kvs) != 1 {
			return nil, state.ErrFUSEPoolCorrupt
		}
		var record state.FUSEPoolRecord
		if json.Unmarshal(read.Kvs[0].Value, &record) != nil || record.PreparationID != id ||
			record.PoolKey != poolKey || record.State != state.FUSEPoolPrepared ||
			record.Revision == 0 || record.Revision == ^uint64(0) ||
			!validPoolID(record.RuntimeID) || !validPoolID(record.RuntimeUID) ||
			!validPoolID(record.MaintainerToken) || record.PrepareUntil.IsZero() ||
			record.ReservationToken != "" || !record.ReservedUntil.IsZero() ||
			string(candidates[0].Key) != r.preparedKey(poolKey, id) {
			return nil, state.ErrFUSEPoolCorrupt
		}
		ownerKey := r.uidOwnerKey(record.RuntimeUID)
		owner, err := r.store.client.Get(requestCtx, ownerKey, clientv3.WithRev(selection.Header.Revision))
		if err != nil {
			return nil, err
		}
		if len(owner.Kvs) != 1 || string(owner.Kvs[0].Value) != id {
			return nil, state.ErrFUSEPoolCorrupt
		}
		metadata.Prepared--
		encodedMetadata, err := json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
		record.State = state.FUSEPoolReserved
		record.ReservationToken = token
		record.ReservedUntil = time.Now().UTC().Add(ttl)
		record.UpdatedAt = time.Now().UTC()
		record.Revision++
		encodedRecord, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		lease, err := r.store.leaseFor(requestCtx, ttl)
		if err != nil {
			return nil, err
		}
		deadlineKey := r.reservationDeadlineKey(id)
		reservedKey := r.reservedKey(poolKey, id)
		written, err := r.store.client.Txn(requestCtx).
			If(clientv3.Compare(clientv3.ModRevision(metaKey), "=", metaKVs[0].ModRevision),
				clientv3.Compare(clientv3.ModRevision(recordKey), "=", read.Kvs[0].ModRevision),
				clientv3.Compare(clientv3.ModRevision(string(candidates[0].Key)), "=", candidates[0].ModRevision),
				clientv3.Compare(clientv3.Version(reservedKey), "=", 0),
				clientv3.Compare(clientv3.Version(deadlineKey), "=", 0),
				clientv3.Compare(clientv3.ModRevision(ownerKey), "=", owner.Kvs[0].ModRevision)).
			Then(clientv3.OpPut(metaKey, string(encodedMetadata)),
				clientv3.OpPut(recordKey, string(encodedRecord)),
				clientv3.OpDelete(string(candidates[0].Key)),
				clientv3.OpPut(reservedKey, id),
				clientv3.OpPut(deadlineKey, id, clientv3.WithLease(lease))).Commit()
		if err != nil {
			// The commit may have reached etcd. Preserve its lease and let
			// the caller resolve this reservation by exact record lookup.
			return nil, err
		}
		if written.Succeeded {
			return &record, nil
		}
		_, _ = r.store.client.Revoke(requestCtx, lease)
	}
	return nil, state.ErrFUSEPoolConflict
}
