package etcd

import (
	"context"
	"encoding/json"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// RenewOperation replaces the operation and every associated lock with one
// fresh lease in a single transaction. No local retry follows an unknown
// result; an expired old operation cannot be revived by a later grant.
func (r *ActiveSandboxRepository) RenewOperation(ctx context.Context, operation state.ActiveSandboxOperation, ttl time.Duration) (*state.ActiveSandboxOperation, error) {
	if !validActiveID(operation.SandboxID) || operation.Token == "" || operation.Generation <= 0 || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, state.ErrActiveSandboxCorrupt
	}
	if operation.Kind != state.ActiveOperationData && operation.Kind != state.ActiveOperationMutation && operation.Kind != state.ActiveOperationExclusive {
		return nil, state.ErrActiveSandboxCorrupt
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()

	recordKey := r.recordKey(operation.SandboxID)
	recordRead, err := r.store.client.Get(requestCtx, recordKey)
	if err != nil {
		return nil, err
	}
	if len(recordRead.Kvs) == 0 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	record, err := decodeActiveEnvelope(recordRead.Kvs[0].Value, operation.SandboxID)
	if err != nil {
		return nil, err
	}
	if record.Record.Generation != operation.Generation {
		return nil, state.ErrActiveSandboxStaleToken
	}
	opKey := r.operationKey(operation.SandboxID, operation.Token)
	opRead, err := r.store.client.Get(requestCtx, opKey)
	if err != nil {
		return nil, err
	}
	if len(opRead.Kvs) == 0 {
		return nil, state.ErrActiveSandboxStaleToken
	}
	var stored state.ActiveSandboxOperation
	if json.Unmarshal(opRead.Kvs[0].Value, &stored) != nil || stored.SandboxID != operation.SandboxID || stored.Token != operation.Token || stored.Generation != operation.Generation || stored.Kind != operation.Kind {
		return nil, state.ErrActiveSandboxStaleToken
	}

	grantedAt := time.Now()
	lease, err := r.store.leaseFor(requestCtx, ttl)
	if err != nil {
		return nil, err
	}
	operation.ExpiresAt = grantedAt.Add(ttl)
	next, err := json.Marshal(operation)
	if err != nil {
		return nil, err
	}
	compares := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", recordRead.Kvs[0].ModRevision),
		clientv3.Compare(clientv3.ModRevision(opKey), "=", opRead.Kvs[0].ModRevision),
	}
	writes := []clientv3.Op{clientv3.OpPut(opKey, string(next), clientv3.WithLease(lease))}
	if operation.Kind == state.ActiveOperationMutation || operation.Kind == state.ActiveOperationExclusive {
		lockKey := r.mutationKey(operation.SandboxID)
		compares = append(compares, clientv3.Compare(clientv3.Value(lockKey), "=", operation.Token))
		writes = append(writes, clientv3.OpPut(lockKey, operation.Token, clientv3.WithLease(lease)))
	}
	if operation.Kind == state.ActiveOperationExclusive {
		lockKey := r.exclusiveKey(operation.SandboxID)
		compares = append(compares, clientv3.Compare(clientv3.Value(lockKey), "=", operation.Token))
		writes = append(writes, clientv3.OpPut(lockKey, operation.Token, clientv3.WithLease(lease)))
	}

	response, err := r.store.client.Txn(requestCtx).If(compares...).Then(writes...).Commit()
	if err != nil {
		// A committed but unacknowledged renewal may already hold this
		// lease. Revoking it here would turn an unknown result into loss.
		return nil, err
	}
	if !response.Succeeded {
		_, _ = r.store.client.Revoke(requestCtx, lease)
		return nil, state.ErrActiveSandboxStaleToken
	}
	// The old lease is now detached from all keys of this operation. A
	// failed best-effort revoke leaks only until its original expiration.
	if oldLease := clientv3.LeaseID(opRead.Kvs[0].Lease); oldLease != 0 {
		_, _ = r.store.client.Revoke(requestCtx, oldLease)
	}
	return &operation, nil
}
