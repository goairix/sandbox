package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var errOperationKindPending = errors.New("etcd state: exclusive operations are not yet implemented")

func (r *ActiveSandboxRepository) operationPrefix(id string) string {
	return r.recordKey(id) + "/operation/"
}

func (r *ActiveSandboxRepository) operationKey(id, token string) string {
	sum := sha256.Sum256([]byte(token))
	return r.operationPrefix(id) + hex.EncodeToString(sum[:])
}

func (r *ActiveSandboxRepository) mutationKey(id string) string {
	return r.recordKey(id) + "/mutation"
}

func (r *ActiveSandboxRepository) exclusiveKey(id string) string {
	return r.recordKey(id) + "/exclusive"
}

// BeginOperation admits data or mutation operations while the durable lifecycle
// record is active. The operation key is attached to an etcd lease, and the
// transaction compares the record revision so BeginDestroy closes admission.
// ExpiresAt is advisory; all future effects must recheck the live lease key.
func (r *ActiveSandboxRepository) BeginOperation(ctx context.Context, id, token string, kind state.ActiveOperationKind, ttl time.Duration) (*state.ActiveSandboxRecord, *state.ActiveSandboxOperation, error) {
	if !validActiveID(id) || token == "" || state.ValidateActiveSandboxTTL(ttl) != nil {
		return nil, nil, state.ErrActiveSandboxCorrupt
	}
	if kind != state.ActiveOperationData && kind != state.ActiveOperationMutation {
		return nil, nil, errOperationKindPending
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	recordKey := r.recordKey(id)
	read, err := r.store.client.Get(requestCtx, recordKey)
	if err != nil || len(read.Kvs) == 0 {
		return nil, nil, err
	}
	envelope, err := decodeActiveEnvelope(read.Kvs[0].Value, id)
	if err != nil {
		return nil, nil, err
	}
	if envelope.Record.Phase != state.ActiveSandboxActive {
		return nil, nil, state.ErrActiveSandboxAdmissionClosed
	}

	// A grant is not a claim. On a known failed compare it is revoked;
	// after an unknown transaction result it must expire naturally.
	grantedAt := time.Now()
	lease, err := r.store.leaseFor(requestCtx, ttl)
	if err != nil {
		return nil, nil, err
	}
	operation := state.ActiveSandboxOperation{
		SandboxID: id, Token: token, Generation: envelope.Record.Generation,
		Kind: kind, ExpiresAt: grantedAt.Add(ttl),
	}
	raw, err := json.Marshal(operation)
	if err != nil {
		return nil, nil, err
	}
	opKey := r.operationKey(id, token)
	compares := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", read.Kvs[0].ModRevision),
		clientv3.Compare(clientv3.Version(opKey), "=", 0),
		clientv3.Compare(clientv3.Version(r.exclusiveKey(id)), "=", 0),
	}
	writes := []clientv3.Op{clientv3.OpPut(opKey, string(raw), clientv3.WithLease(lease))}
	if kind == state.ActiveOperationMutation {
		compares = append(compares, clientv3.Compare(clientv3.Version(r.mutationKey(id)), "=", 0))
		writes = append(writes, clientv3.OpPut(r.mutationKey(id), token, clientv3.WithLease(lease)))
	}
	response, err := r.store.client.Txn(requestCtx).
		If(compares...).Then(writes...).
		Else(clientv3.OpGet(recordKey)).Commit()
	if err != nil {
		return nil, nil, err
	}
	if response.Succeeded {
		return &envelope.Record, &operation, nil
	}
	_, _ = r.store.client.Revoke(requestCtx, lease)
	if len(response.Responses) != 1 || response.Responses[0].GetResponseRange() == nil {
		return nil, nil, state.ErrActiveSandboxCorrupt
	}
	records := response.Responses[0].GetResponseRange().Kvs
	if len(records) == 0 {
		return nil, nil, nil
	}
	current, err := decodeActiveEnvelope(records[0].Value, id)
	if err != nil {
		return nil, nil, err
	}
	if current.Record.Phase != state.ActiveSandboxActive {
		return nil, nil, state.ErrActiveSandboxAdmissionClosed
	}
	return nil, nil, state.ErrActiveSandboxConflict
}

// LiveOperations counts server-live operation keys. The work is bounded to a
// single sandbox prefix, rather than a scan over all resident sandboxes.
func (r *ActiveSandboxRepository) LiveOperations(ctx context.Context, id string) (int64, error) {
	if !validActiveID(id) {
		return 0, state.ErrActiveSandboxCorrupt
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	response, err := r.store.client.Get(requestCtx, r.operationPrefix(id), clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		return 0, err
	}
	return response.Count, nil
}

// EndOperation drops an exact live data or mutation operation. A lease already expired is
// idempotently absent, as in the Redis baseline, provided the generation still
// belongs to the same sandbox record.
func (r *ActiveSandboxRepository) EndOperation(ctx context.Context, operation state.ActiveSandboxOperation) error {
	if !validActiveID(operation.SandboxID) || operation.Token == "" || operation.Generation <= 0 {
		return state.ErrActiveSandboxCorrupt
	}
	if operation.Kind != state.ActiveOperationData && operation.Kind != state.ActiveOperationMutation {
		return errOperationKindPending
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	record, err := r.Load(requestCtx, operation.SandboxID)
	if err != nil {
		return err
	}
	if record == nil || record.Generation != operation.Generation {
		return state.ErrActiveSandboxStaleToken
	}
	opKey := r.operationKey(operation.SandboxID, operation.Token)
	read, err := r.store.client.Get(requestCtx, opKey)
	if err != nil || len(read.Kvs) == 0 {
		return err
	}
	var stored state.ActiveSandboxOperation
	if json.Unmarshal(read.Kvs[0].Value, &stored) != nil || stored.Token != operation.Token || stored.Generation != operation.Generation || stored.Kind != operation.Kind {
		return state.ErrActiveSandboxStaleToken
	}
	compares := []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(opKey), "=", read.Kvs[0].ModRevision)}
	writes := []clientv3.Op{clientv3.OpDelete(opKey)}
	if operation.Kind == state.ActiveOperationMutation {
		// Only the matching holder may remove the shared mutation lock.
		// A prior expired token cannot unlock a successor's lease.
		mutationKey := r.mutationKey(operation.SandboxID)
		compares = append(compares, clientv3.Compare(clientv3.Value(mutationKey), "=", operation.Token))
		writes = append(writes, clientv3.OpDelete(mutationKey))
	}
	response, err := r.store.client.Txn(requestCtx).
		If(compares...).Then(writes...).Commit()
	if err != nil {
		return err
	}
	if !response.Succeeded {
		return state.ErrActiveSandboxStaleToken
	}
	return nil
}

// BeginDestroy durably closes admission before counting live operations. Once
// the phase changes, no new BeginOperation transaction can pass its revision
// comparison, so the count can only fall while cleanup waits for drain.
func (r *ActiveSandboxRepository) BeginDestroy(ctx context.Context, id string) (*state.ActiveSandboxRecord, int64, bool, error) {
	if !validActiveID(id) {
		return nil, 0, false, state.ErrActiveSandboxCorrupt
	}
	requestCtx, cancel := r.store.requestContext(ctx)
	defer cancel()
	key := r.recordKey(id)
	for {
		read, err := r.store.client.Get(requestCtx, key)
		if err != nil || len(read.Kvs) == 0 {
			return nil, 0, false, err
		}
		envelope, err := decodeActiveEnvelope(read.Kvs[0].Value, id)
		if err != nil {
			return nil, 0, false, err
		}
		recordRevision := read.Kvs[0].ModRevision
		won := false
		switch envelope.Record.Phase {
		case state.ActiveSandboxActive, state.ActiveSandboxPublishing, state.ActiveSandboxExclusive:
			envelope.Record.Phase = state.ActiveSandboxDestroying
			envelope.Record.Revision++
			envelope.Record.UpdatedAt = time.Now().UTC()
			next, err := json.Marshal(envelope)
			if err != nil {
				return nil, 0, false, err
			}
			written, err := r.store.client.Txn(requestCtx).
				If(clientv3.Compare(clientv3.ModRevision(key), "=", read.Kvs[0].ModRevision)).
				Then(clientv3.OpPut(key, string(next))).Commit()
			if err != nil {
				return nil, 0, false, err
			}
			if !written.Succeeded {
				continue
			}
			recordRevision = written.Header.Revision
			won = true
		case state.ActiveSandboxDestroying, state.ActiveSandboxCleanupPending:
		default:
			return nil, 0, false, state.ErrActiveSandboxConflict
		}
		// Count against the same record revision. A concurrent Delete or
		// other lifecycle transition restarts the read instead of pairing
		// an old record with a new operation census.
		counted, err := r.store.client.Txn(requestCtx).
			If(clientv3.Compare(clientv3.ModRevision(key), "=", recordRevision)).
			Then(clientv3.OpGet(r.operationPrefix(id), clientv3.WithPrefix(), clientv3.WithCountOnly())).Commit()
		if err != nil {
			return nil, 0, false, err
		}
		if !counted.Succeeded {
			continue
		}
		if len(counted.Responses) != 1 || counted.Responses[0].GetResponseRange() == nil {
			return nil, 0, false, state.ErrActiveSandboxCorrupt
		}
		return &envelope.Record, counted.Responses[0].GetResponseRange().Count, won, nil
	}
}
