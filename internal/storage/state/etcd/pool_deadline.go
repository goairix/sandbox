package etcd

import (
	"context"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var _ state.FUSEPoolDeadlineReader = (*FUSEPoolRepository)(nil)

// DeadlineLive observes the exact record and its leased deadline in one
// server revision. The record's local timestamp is not used to grant or deny
// cleanup. A changed record is a conflict so the caller must start a new pass.
func (r *FUSEPoolRepository) DeadlineLive(ctx context.Context, record state.FUSEPoolRecord) (bool, error) {
	if r == nil || r.store == nil || !validPoolID(record.PreparationID) ||
		!validPoolID(record.PoolKey) || record.Revision == 0 {
		return false, state.ErrFUSEPoolInvalidRecord
	}
	var deadlineKey string
	switch record.State {
	case state.FUSEPoolPreparing:
		deadlineKey = r.prepareDeadlineKey(record.PreparationID)
	case state.FUSEPoolReserved:
		deadlineKey = r.reservationDeadlineKey(record.PreparationID)
	case state.FUSEPoolCleanup:
		deadlineKey = r.cleanupDeadlineKey(record.PreparationID)
	default:
		return false, state.ErrFUSEPoolInvalidTransition
	}

	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	read, err := r.store.client.Txn(requestCtx).
		Then(clientv3.OpGet(r.recordKey(record.PreparationID)), clientv3.OpGet(deadlineKey)).Commit()
	if err != nil {
		return false, err
	}
	if len(read.Responses) != 2 || read.Responses[0].GetResponseRange() == nil ||
		read.Responses[1].GetResponseRange() == nil {
		return false, state.ErrFUSEPoolCorrupt
	}
	currentKVs := read.Responses[0].GetResponseRange().Kvs
	if len(currentKVs) != 1 {
		return false, state.ErrFUSEPoolCASMismatch
	}
	current, err := decodePoolRecord(currentKVs[0].Value, record.PreparationID)
	if err != nil {
		return false, err
	}
	if current.PoolKey != record.PoolKey || current.State != record.State || current.Revision != record.Revision {
		return false, state.ErrFUSEPoolCASMismatch
	}
	deadlineKVs := read.Responses[1].GetResponseRange().Kvs
	if len(deadlineKVs) == 0 {
		return false, nil
	}
	if len(deadlineKVs) != 1 || string(deadlineKVs[0].Value) != record.PreparationID || deadlineKVs[0].Lease == 0 {
		return false, state.ErrFUSEPoolCorrupt
	}
	return true, nil
}
