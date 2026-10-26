package etcd

import (
	"context"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var _ state.FUSEPoolBoundStore = (*Store)(nil)

// SetFUSEBound publishes a lifecycle reference only while its exact prepared
// runtime remains consumed. The pool record and publication share one etcd
// transaction, so an earlier cleanup claim rejects the write, while an
// earlier publication is visible to the cleanup owner's later inventory scan.
func (s *Store) SetFUSEBound(ctx context.Context, key string, value []byte, ttl time.Duration, binding state.FUSEPoolPublication) error {
	_, err := s.writeFUSEBound(ctx, key, value, ttl, binding, false)
	return err
}

func (s *Store) SetNXFUSEBound(ctx context.Context, key string, value []byte, ttl time.Duration, binding state.FUSEPoolPublication) (bool, error) {
	return s.writeFUSEBound(ctx, key, value, ttl, binding, true)
}

func (s *Store) writeFUSEBound(ctx context.Context, key string, value []byte, ttl time.Duration, binding state.FUSEPoolPublication, create bool) (bool, error) {
	if !validPoolID(binding.PoolKey) || !validPoolID(binding.PreparationID) ||
		!validPoolID(binding.RuntimeID) || !validPoolID(binding.RuntimeUID) ||
		!validPoolID(binding.ReservationToken) {
		return false, state.ErrFUSEPoolInvalidRecord
	}
	fullKey, err := s.key(key)
	if err != nil {
		return false, err
	}
	requestCtx, cancel := s.requestContext(ctx)
	defer cancel()
	repository := NewFUSEPoolRepository(s)
	recordKey := repository.recordKey(binding.PreparationID)
	read, err := s.client.Get(requestCtx, recordKey)
	if err != nil {
		return false, err
	}
	if len(read.Kvs) != 1 {
		return false, state.ErrFUSEPoolConflict
	}
	record, err := decodePoolRecord(read.Kvs[0].Value, binding.PreparationID)
	if err != nil {
		return false, err
	}
	if record.State != state.FUSEPoolConsumed || record.PoolKey != binding.PoolKey ||
		record.RuntimeID != binding.RuntimeID || record.RuntimeUID != binding.RuntimeUID ||
		record.ReservationToken != binding.ReservationToken {
		return false, state.ErrFUSEPoolConflict
	}
	lease, err := s.leaseFor(requestCtx, ttl)
	if err != nil {
		return false, err
	}
	conditions := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", read.Kvs[0].ModRevision),
		clientv3.Compare(clientv3.Value(repository.uidOwnerKey(binding.RuntimeUID)), "=", binding.PreparationID),
		clientv3.Compare(clientv3.Value(repository.consumedKey(binding.PoolKey, binding.PreparationID)), "=", binding.PreparationID),
	}
	if create {
		conditions = append(conditions, clientv3.Compare(clientv3.Version(fullKey), "=", 0))
	}
	response, err := s.client.Txn(requestCtx).If(conditions...).
		Then(clientv3.OpPut(fullKey, string(value), putOptions(lease)...)).
		Else(clientv3.OpGet(recordKey), clientv3.OpGet(fullKey)).Commit()
	if err != nil {
		// A lost reply may follow a committed write; revoking its lease here
		// would remove the publication that cleanup must observe.
		return false, err
	}
	if response.Succeeded {
		return true, nil
	}
	if lease != 0 {
		_, _ = s.client.Revoke(requestCtx, lease)
	}
	if len(response.Responses) != 2 || response.Responses[0].GetResponseRange() == nil || response.Responses[1].GetResponseRange() == nil {
		return false, state.ErrFUSEPoolConflict
	}
	current := response.Responses[0].GetResponseRange().Kvs
	if len(current) != 1 || current[0].ModRevision != read.Kvs[0].ModRevision {
		return false, state.ErrFUSEPoolConflict
	}
	if create && len(response.Responses[1].GetResponseRange().Kvs) != 0 {
		return false, nil
	}
	return false, state.ErrFUSEPoolConflict
}
