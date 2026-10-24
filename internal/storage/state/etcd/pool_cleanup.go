package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ClaimCleanup currently fences a bound consumed runtime into the cleanup
// phase. It retains the UID owner and inventory record until independent
// termination evidence is confirmed. Unbound and earlier-state claims stay
// explicitly unsupported until their recovery and lease rules are covered.
func (r *FUSEPoolRepository) ClaimCleanup(ctx context.Context, id string, from state.FUSEPoolState,
	maintainerToken, reservationToken string, expectedRevision uint64,
	runtimeID, runtimeUID, cleanupToken string, ttl time.Duration,
) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("fuse pool repository: nil store")
	}
	if !validPoolID(id) || !validPoolID(maintainerToken) || !validPoolID(reservationToken) ||
		!validPoolID(runtimeID) || !validPoolID(runtimeUID) || !validPoolID(cleanupToken) ||
		expectedRevision == 0 || ttl <= 0 ||
		(from != state.FUSEPoolConsumed && from != state.FUSEPoolCleanup) {
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
	if record.RuntimeID != runtimeID || record.RuntimeUID != runtimeUID {
		return nil, state.ErrFUSEPoolConflict
	}
	if record.State == state.FUSEPoolCleanup {
		// Idempotent readback must validate the same ownership facts as a
		// new claim. Otherwise a damaged index could look like success.
		if err := r.validateCleanupOwnership(requestCtx, record, read.Header.Revision); err != nil {
			return nil, err
		}
		if record.CleanupToken == cleanupToken {
			// A retry with the same token observes the original claim. It
			// never lengthens the lease or advances the revision.
			return &record, nil
		}
		return r.reclaimExpiredCleanup(requestCtx, recordKey, read.Kvs[0].ModRevision,
			record, from, expectedRevision, cleanupToken, ttl)
	}
	if record.Revision != expectedRevision {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.State != state.FUSEPoolConsumed || from != state.FUSEPoolConsumed {
		return nil, state.ErrFUSEPoolInvalidTransition
	}
	if record.MaintainerToken != maintainerToken || record.ReservationToken != reservationToken {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	if record.Revision == ^uint64(0) {
		return nil, state.ErrFUSEPoolCorrupt
	}

	lease, err := r.store.leaseFor(requestCtx, ttl)
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
		_, _ = r.store.client.Revoke(requestCtx, lease)
		return nil, err
	}
	fromKey := r.consumedKey(record.PoolKey, id)
	toKey := r.cleanupKey(record.PoolKey, id)
	deadlineKey := r.cleanupDeadlineKey(id)
	written, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", read.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.Value(fromKey), "=", id),
			clientv3.Compare(clientv3.Version(toKey), "=", 0),
			clientv3.Compare(clientv3.Version(deadlineKey), "=", 0),
			clientv3.Compare(clientv3.Value(r.uidOwnerKey(runtimeUID)), "=", id)).
		Then(clientv3.OpPut(recordKey, string(raw)),
			clientv3.OpDelete(fromKey), clientv3.OpPut(toKey, id),
			clientv3.OpDelete(r.reservationDeadlineKey(id)),
			clientv3.OpPut(deadlineKey, id, clientv3.WithLease(lease))).Commit()
	if err != nil {
		// The claim may already be durable. Keep the lease intact so a
		// same-token exact read can determine whether it committed.
		return nil, err
	}
	if !written.Succeeded {
		_, _ = r.store.client.Revoke(requestCtx, lease)
		return nil, state.ErrFUSEPoolCASMismatch
	}
	return &record, nil
}

func (r *FUSEPoolRepository) validateCleanupOwnership(ctx context.Context, record state.FUSEPoolRecord, revision int64) error {
	index, err := r.store.client.Get(ctx, r.cleanupKey(record.PoolKey, record.PreparationID), clientv3.WithRev(revision))
	if err != nil {
		return err
	}
	owner, err := r.store.client.Get(ctx, r.uidOwnerKey(record.RuntimeUID), clientv3.WithRev(revision))
	if err != nil {
		return err
	}
	if len(index.Kvs) != 1 || string(index.Kvs[0].Value) != record.PreparationID ||
		len(owner.Kvs) != 1 || string(owner.Kvs[0].Value) != record.PreparationID {
		return state.ErrFUSEPoolCorrupt
	}
	return nil
}

// reclaimExpiredCleanup replaces only a server-expired cleanup holder. The
// runtime owner, record and termination phase remain durable during takeover.
func (r *FUSEPoolRepository) reclaimExpiredCleanup(ctx context.Context, recordKey string,
	modRevision int64, record state.FUSEPoolRecord, from state.FUSEPoolState,
	expectedRevision uint64, token string, ttl time.Duration,
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
	written, err := r.store.client.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", modRevision),
			clientv3.Compare(clientv3.Value(r.cleanupKey(record.PoolKey, record.PreparationID)), "=", record.PreparationID),
			clientv3.Compare(clientv3.Value(r.uidOwnerKey(record.RuntimeUID)), "=", record.PreparationID),
			clientv3.Compare(clientv3.Version(deadlineKey), "=", 0)).
		Then(clientv3.OpPut(recordKey, string(raw)),
			clientv3.OpPut(deadlineKey, record.PreparationID, clientv3.WithLease(lease))).Commit()
	if err != nil {
		return nil, err
	}
	if !written.Succeeded {
		_, _ = r.store.client.Revoke(ctx, lease)
		return nil, state.ErrFUSEPoolCASMismatch
	}
	return &record, nil
}
