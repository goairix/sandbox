package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var _ state.ActiveSandboxOperationRepository = (*ActiveSandboxRepository)(nil)

func revokeControllerAfterSnapshot(snapshot json.RawMessage) (bool, error) {
	var payload struct {
		Workspace           json.RawMessage `json:"workspace"`
		WorkspaceTransition string          `json:"workspace_transition"`
	}
	if err := json.Unmarshal(snapshot, &payload); err != nil {
		return false, state.ErrActiveSandboxCorrupt
	}
	return len(payload.Workspace) == 0 || bytes.Equal(payload.Workspace, []byte("null")) ||
		payload.WorkspaceTransition == "unmount_synced" || payload.WorkspaceTransition == "unmount_releasing", nil
}

func (r *ActiveSandboxRepository) controllerKey(id string) string {
	return r.recordKey(id) + "/controller"
}

// UpdateOperation publishes a snapshot only while its exact mutation lease
// and lock remain live. Exclusive updates require the operation to be the
// only server-live operation after exclusive admission has closed.
func (r *ActiveSandboxRepository) UpdateOperation(ctx context.Context, operation state.ActiveSandboxOperation, expectedRevision uint64, snapshot json.RawMessage) (*state.ActiveSandboxRecord, error) {
	if !validActiveID(operation.SandboxID) || operation.Token == "" || operation.Generation <= 0 || expectedRevision == 0 ||
		!json.Valid(snapshot) || len(snapshot) > 1<<20 {
		return nil, state.ErrActiveSandboxCorrupt
	}
	if operation.Kind != state.ActiveOperationMutation && operation.Kind != state.ActiveOperationExclusive {
		return nil, state.ErrActiveSandboxStaleToken
	}
	revokeController, err := revokeControllerAfterSnapshot(snapshot)
	if err != nil {
		return nil, err
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
	envelope, err := decodeActiveEnvelope(recordRead.Kvs[0].Value, operation.SandboxID)
	if err != nil {
		return nil, err
	}
	if envelope.Record.Generation != operation.Generation {
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
	if json.Unmarshal(opRead.Kvs[0].Value, &stored) != nil || stored.SandboxID != operation.SandboxID ||
		stored.Token != operation.Token || stored.Generation != operation.Generation || stored.Kind != operation.Kind {
		return nil, state.ErrActiveSandboxStaleToken
	}
	expectedPhase := state.ActiveSandboxActive
	if operation.Kind == state.ActiveOperationExclusive {
		expectedPhase = state.ActiveSandboxExclusive
	}
	if envelope.Record.Phase != expectedPhase || envelope.Record.Revision != expectedRevision {
		return nil, state.ErrActiveSandboxConflict
	}
	if operation.Kind == state.ActiveOperationExclusive {
		live, err := r.LiveOperations(requestCtx, operation.SandboxID)
		if err != nil {
			return nil, err
		}
		if live != 1 {
			return nil, state.ErrActiveSandboxConflict
		}
	}

	envelope.Record.Revision++
	envelope.Record.Snapshot = append(json.RawMessage(nil), snapshot...)
	envelope.Record.UpdatedAt = time.Now().UTC()
	next, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}
	mutationKey := r.mutationKey(operation.SandboxID)
	compares := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(recordKey), "=", recordRead.Kvs[0].ModRevision),
		clientv3.Compare(clientv3.ModRevision(opKey), "=", opRead.Kvs[0].ModRevision),
		clientv3.Compare(clientv3.Value(mutationKey), "=", operation.Token),
	}
	if operation.Kind == state.ActiveOperationExclusive {
		compares = append(compares, clientv3.Compare(clientv3.Value(r.exclusiveKey(operation.SandboxID)), "=", operation.Token))
	}
	writes := []clientv3.Op{clientv3.OpPut(recordKey, string(next))}
	if revokeController {
		// Detaching a workspace revokes its old background controller in
		// the same atomic write, matching the original Redis Lua boundary.
		writes = append(writes, clientv3.OpDelete(r.controllerKey(operation.SandboxID)))
	}
	response, err := r.store.client.Txn(requestCtx).If(compares...).Then(writes...).
		Else(clientv3.OpGet(opKey), clientv3.OpGet(mutationKey), clientv3.OpGet(r.exclusiveKey(operation.SandboxID))).Commit()
	if err != nil {
		return nil, err
	}
	if response.Succeeded {
		return &envelope.Record, nil
	}
	if len(response.Responses) != 3 || response.Responses[0].GetResponseRange() == nil || response.Responses[1].GetResponseRange() == nil || response.Responses[2].GetResponseRange() == nil {
		return nil, state.ErrActiveSandboxCorrupt
	}
	opKVs := response.Responses[0].GetResponseRange().Kvs
	mutationKVs := response.Responses[1].GetResponseRange().Kvs
	exclusiveKVs := response.Responses[2].GetResponseRange().Kvs
	if len(opKVs) == 0 || len(mutationKVs) == 0 || string(mutationKVs[0].Value) != operation.Token {
		return nil, state.ErrActiveSandboxStaleToken
	}
	if operation.Kind == state.ActiveOperationExclusive && (len(exclusiveKVs) == 0 || string(exclusiveKVs[0].Value) != operation.Token) {
		return nil, state.ErrActiveSandboxStaleToken
	}
	return nil, state.ErrActiveSandboxConflict
}
