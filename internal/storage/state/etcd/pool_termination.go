package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/storage/state"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func validPoolTerminationEvidence(evidence state.FUSEPoolTerminationEvidence, runtimeUID string) bool {
	return evidence.RuntimeUID == runtimeUID && validPoolID(evidence.RuntimeUID) &&
		len(evidence.NodeName) <= 1024 && utf8.ValidString(evidence.NodeName) &&
		(evidence.ProcessExited || (evidence.InfrastructureFenced && evidence.NodeName != ""))
}

// ConfirmCleanupTermination persists the runtime's termination proof before
// any inventory deletion may be attempted. The cleanup holder and exact UID
// owner remain fenced; an ambiguous commit must be resolved by exact readback.
func (r *FUSEPoolRepository) ConfirmCleanupTermination(ctx context.Context, id, cleanupToken string,
	expectedRevision uint64, runtimeID, runtimeUID string,
	evidence state.FUSEPoolTerminationEvidence,
) (*state.FUSEPoolRecord, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("fuse pool repository: nil store")
	}
	if !validPoolID(id) || !validPoolID(cleanupToken) || !validPoolID(runtimeID) ||
		!validPoolID(runtimeUID) || expectedRevision == 0 ||
		!validPoolTerminationEvidence(evidence, runtimeUID) {
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
	if record.State != state.FUSEPoolCleanup || record.RuntimeID != runtimeID || record.RuntimeUID != runtimeUID {
		return nil, state.ErrFUSEPoolConflict
	}
	if record.CleanupToken != cleanupToken {
		return nil, state.ErrFUSEPoolTokenMismatch
	}
	if err := r.validateCleanupOwnership(requestCtx, record, read.Header.Revision); err != nil {
		return nil, err
	}
	if record.CleanupPhase == state.FUSEPoolCleanupTerminated {
		if *record.TerminationEvidence != evidence {
			return nil, state.ErrFUSEPoolConflict
		}
		if record.Revision == expectedRevision ||
			(expectedRevision != ^uint64(0) && record.Revision == expectedRevision+1) {
			return &record, nil
		}
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.Revision != expectedRevision {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	if record.Revision == ^uint64(0) {
		return nil, state.ErrFUSEPoolCorrupt
	}
	record.CleanupPhase = state.FUSEPoolCleanupTerminated
	record.TerminationEvidence = &evidence
	record.Revision++
	record.UpdatedAt = time.Now().UTC()
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	written, err := r.store.client.Txn(requestCtx).
		If(clientv3.Compare(clientv3.ModRevision(recordKey), "=", read.Kvs[0].ModRevision),
			clientv3.Compare(clientv3.Value(r.cleanupKey(record.PoolKey, id)), "=", id),
			clientv3.Compare(clientv3.Value(r.uidOwnerKey(runtimeUID)), "=", id)).
		Then(clientv3.OpPut(recordKey, string(raw))).Commit()
	if err != nil {
		return nil, err
	}
	if !written.Succeeded {
		return nil, state.ErrFUSEPoolCASMismatch
	}
	return &record, nil
}
