package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ClaimCleanup fences any pool state into cleanup while retaining inventory
// and UID ownership. A preparing intent may still lack runtime identity;
// exact runtime evidence can be attached later by the same cleanup token.
func (r *FUSEPoolRepository) ClaimCleanup(ctx context.Context, id string, from state.FUSEPoolState,
	maintainerToken, reservationToken string, expectedRevision uint64,
	runtimeID, runtimeUID, cleanupToken string, ttl time.Duration,
) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("fuse pool repository: nil store")
	}
	if !validPoolID(id) || !validPoolID(maintainerToken) ||
		(reservationToken != "" && !validPoolID(reservationToken)) ||
		(runtimeID == "") != (runtimeUID == "") ||
		(runtimeID != "" && (!validPoolID(runtimeID) || !validPoolID(runtimeUID))) ||
		!validPoolID(cleanupToken) || expectedRevision == 0 || ttl <= 0 || !validPoolState(from) {
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
	record, err := decodePoolRecord(read.Kvs[0].Value, id)
	if err != nil {
		return nil, err
	}
	if record.RuntimeID != "" && (record.RuntimeID != runtimeID || record.RuntimeUID != runtimeUID) {
		return nil, state.ErrFUSEPoolConflict
	}
	if record.State == state.FUSEPoolCleanup {
		// Idempotent readback must validate the same ownership facts as a
		// new claim. Otherwise a damaged index could look like success.
		if err := r.validateCleanupOwnership(requestCtx, record, read.Header.Revision); err != nil {
			return nil, err
		}
		if record.CleanupToken == cleanupToken {
			if record.RuntimeID == "" && runtimeID != "" {
				return r.attachCleanupRuntime(requestCtx, recordKey, read.Kvs[0].ModRevision,
					record, runtimeID, runtimeUID)
			}
			// A retry with the same token observes the original claim. It
			// never lengthens the lease or advances the revision.
			return &record, nil
		}
		return r.reclaimExpiredCleanup(requestCtx, recordKey, read.Kvs[0].ModRevision,
			record, from, expectedRevision, runtimeID, runtimeUID, cleanupToken, ttl)
	}
	if record.Revision != expectedRevision {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.State != from || from == state.FUSEPoolCleanup {
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	if record.MaintainerToken != maintainerToken || record.ReservationToken != reservationToken {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	return r.claimFromState(requestCtx, recordKey, read.Kvs[0].ModRevision,
		record, runtimeID, runtimeUID, cleanupToken, ttl)
}

func (r *FUSEPoolRepository) claimFromState(ctx context.Context, recordKey string,
	modRevision int64, record state.FUSEPoolRecord,
	runtimeID, runtimeUID, cleanupToken string, ttl time.Duration,
) (*state.FUSEPoolRecord, error) {
	if record.Revision == ^uint64(0) {
		return nil, state.ErrFUSEPoolCorrupt
	}
	fromKey := r.stateKey(record.PoolKey, record.PreparationID, record.State)
	toKey := r.cleanupKey(record.PoolKey, record.PreparationID)
	conditions := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", modRevision),
		clientv3.Compare(clientv3.Value(fromKey), "=", record.PreparationID),
		clientv3.Compare(clientv3.Version(toKey), "=", 0),
		clientv3.Compare(clientv3.Version(r.cleanupDeadlineKey(record.PreparationID)), "=", 0),
	}
	operations := make([]clientv3.Op, 0, 8)
	if record.RuntimeUID != "" {
		conditions = append(conditions,
			clientv3.Compare(clientv3.Value(r.uidOwnerKey(record.RuntimeUID)), "=", record.PreparationID))
	} else if runtimeUID != "" {
		conditions = append(conditions, clientv3.Compare(clientv3.Version(r.uidOwnerKey(runtimeUID)), "=", 0))
		operations = append(operations, clientv3.OpPut(r.uidOwnerKey(runtimeUID), record.PreparationID))
		record.RuntimeID, record.RuntimeUID = runtimeID, runtimeUID
	}
	if record.State == state.FUSEPoolPreparing || record.State == state.FUSEPoolPrepared {
		metaKey := r.metadataKey(record.PoolKey)
		metaRead, err := r.store.client.Get(ctx, metaKey)
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
		if record.State == state.FUSEPoolPreparing {
			if metadata.Preparing == 0 {
				return nil, state.ErrFUSEPoolCorrupt
			}
			metadata.Preparing--
		} else {
			if metadata.Prepared == 0 {
				return nil, state.ErrFUSEPoolCorrupt
			}
			metadata.Prepared--
		}
		encodedMetadata, err := json.Marshal(metadata)
		if err != nil {
			return nil, err
		}
		conditions = append(conditions, clientv3.Compare(clientv3.ModRevision(metaKey), "=", metaRead.Kvs[0].ModRevision))
		operations = append(operations, clientv3.OpPut(metaKey, string(encodedMetadata)))
	}
	lease, err := r.store.leaseFor(ctx, ttl)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	record.State = state.FUSEPoolCleanup
	record.CleanupToken = cleanupToken
	record.CleanupUntil = now.Add(ttl)
	record.CleanupPhase = state.FUSEPoolCleanupTerminating
	record.UpdatedAt = now
	record.Revision++
	raw, err := json.Marshal(record)
	if err != nil {
		_, _ = r.store.client.Revoke(ctx, lease)
		return nil, err
	}
	operations = append(operations,
		clientv3.OpPut(recordKey, string(raw)),
		clientv3.OpDelete(fromKey), clientv3.OpPut(toKey, record.PreparationID),
		clientv3.OpDelete(r.prepareDeadlineKey(record.PreparationID)),
		clientv3.OpDelete(r.reservationDeadlineKey(record.PreparationID)),
		clientv3.OpPut(r.cleanupDeadlineKey(record.PreparationID), record.PreparationID, clientv3.WithLease(lease)))
	written, err := r.store.client.Txn(ctx).If(conditions...).Then(operations...).Commit()
	if err != nil {
		// The claim may already be durable. Keep the lease intact so a
		// same-token exact read can determine whether it committed.
		return nil, err
	}
	if !written.Succeeded {
		_, _ = r.store.client.Revoke(ctx, lease)
		return nil, state.ErrFUSEPoolCASMismatch
	}
	return &record, nil
}

func (r *FUSEPoolRepository) validateCleanupOwnership(ctx context.Context, record state.FUSEPoolRecord, revision int64) error {
	index, err := r.store.client.Get(ctx, r.cleanupKey(record.PoolKey, record.PreparationID), clientv3.WithRev(revision))
	if err != nil {
		return err
	}
	if len(index.Kvs) != 1 || string(index.Kvs[0].Value) != record.PreparationID {
		return state.ErrFUSEPoolCorrupt
	}
	if record.RuntimeUID != "" {
		owner, err := r.store.client.Get(ctx, r.uidOwnerKey(record.RuntimeUID), clientv3.WithRev(revision))
		if err != nil {
			return err
		}
		if len(owner.Kvs) != 1 || string(owner.Kvs[0].Value) != record.PreparationID {
			return state.ErrFUSEPoolCorrupt
		}
	}
	return nil
}

func (r *FUSEPoolRepository) attachCleanupRuntime(ctx context.Context, recordKey string,
	modRevision int64, record state.FUSEPoolRecord, runtimeID, runtimeUID string,
) (*state.FUSEPoolRecord, error) {
	if record.Revision == ^uint64(0) {
		return nil, state.ErrFUSEPoolCorrupt
	}
	record.RuntimeID, record.RuntimeUID = runtimeID, runtimeUID
	record.Revision++
	record.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	written, err := r.store.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", modRevision),
			clientv3.Compare(clientv3.Value(r.cleanupKey(record.PoolKey, record.PreparationID)), "=", record.PreparationID),
			clientv3.Compare(clientv3.Version(r.uidOwnerKey(runtimeUID)), "=", 0)).
		Then(clientv3.OpPut(recordKey, string(raw)),
			clientv3.OpPut(r.uidOwnerKey(runtimeUID), record.PreparationID)).Commit()
	if err != nil {
		return nil, err
	}
	if !written.Succeeded {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	return &record, nil
}

// reclaimExpiredCleanup replaces only a server-expired cleanup holder. The
// runtime owner, record and termination phase remain durable during takeover.
func (r *FUSEPoolRepository) reclaimExpiredCleanup(ctx context.Context, recordKey string,
	modRevision int64, record state.FUSEPoolRecord, from state.FUSEPoolState,
	expectedRevision uint64, runtimeID, runtimeUID, token string, ttl time.Duration,
) (*state.FUSEPoolRecord, error) {
	deadlineKey := r.cleanupDeadlineKey(record.PreparationID)
	deadline, err := r.store.client.Get(ctx, deadlineKey)
	if err != nil {
		return nil, err
	}
	if len(deadline.Kvs) != 0 {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	if from != state.FUSEPoolCleanup || record.Revision != expectedRevision {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.Revision == ^uint64(0) {
		return nil, state.ErrFUSEPoolCorrupt
	}
	conditions := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", modRevision),
		clientv3.Compare(clientv3.Value(r.cleanupKey(record.PoolKey, record.PreparationID)), "=", record.PreparationID),
		clientv3.Compare(clientv3.Version(deadlineKey), "=", 0),
	}
	operations := make([]clientv3.Op, 0, 3)
	if record.RuntimeUID != "" {
		conditions = append(conditions,
			clientv3.Compare(clientv3.Value(r.uidOwnerKey(record.RuntimeUID)), "=", record.PreparationID))
	} else if runtimeUID != "" {
		conditions = append(conditions, clientv3.Compare(clientv3.Version(r.uidOwnerKey(runtimeUID)), "=", 0))
		operations = append(operations, clientv3.OpPut(r.uidOwnerKey(runtimeUID), record.PreparationID))
		record.RuntimeID, record.RuntimeUID = runtimeID, runtimeUID
	}
	lease, err := r.store.leaseFor(ctx, ttl)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	record.CleanupToken = token
	record.CleanupUntil = now.Add(ttl)
	record.UpdatedAt = now
	record.Revision++
	raw, err := json.Marshal(record)
	if err != nil {
		_, _ = r.store.client.Revoke(ctx, lease)
		return nil, err
	}
	operations = append(operations,
		clientv3.OpPut(recordKey, string(raw)),
		clientv3.OpPut(deadlineKey, record.PreparationID, clientv3.WithLease(lease)))
	written, err := r.store.client.Txn(ctx).If(conditions...).Then(operations...).Commit()
	if err != nil {
		return nil, err
	}
	if !written.Succeeded {
		_, _ = r.store.client.Revoke(ctx, lease)
		return nil, state.ErrFUSEPoolCASMismatch
	}
	return &record, nil
}
