package etcd

import (
	"bytes"
	"encoding/json"
	"reflect"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// The pending family has its own small bound so discovery never reads Task
// execution descendants or grows its response budget with retained history.
const cleanupPendingBytes = 2048

// cleanupPendingRecord is permanent discovery evidence, not execution
// authority. New producers write it with the immutable Task and initial receipt;
// a future authenticated legacy backfill may have a later creation revision.
type cleanupPendingRecord struct {
	Version    uint32        `json:"version"`
	Reference  TaskReference `json:"reference"`
	TaskDigest string        `json:"task_digest"`
}

// Validate checks routing identity and digest syntax. The consumer must compare
// the digest with authenticated canonical Task bytes and its original receipt.
func (r cleanupPendingRecord) Validate() error {
	if r.Version != 1 || r.Reference.Validate() != nil || !validHexDigest(r.TaskDigest) {
		return ErrInvalidRecord
	}
	return nil
}

func (n Namespace) cleanupPendingKey(ref TaskReference) (string, error) {
	parts, err := n.taskKeyParts(ref)
	if err != nil {
		return "", err
	}
	parts[2] = "cleanup-pending"
	return n.Key(parts...)
}

func encodeCleanupPending(r cleanupPendingRecord) (string, error) {
	return encodeOperationMetadata(r, cleanupPendingBytes)
}

// A fixed-view reader may accept only this exact immutable row in its original
// namespace/restore epoch. Canonical re-encoding rejects alternate JSON bytes;
// neither an old cursor nor a rewritten/recreated value proves Task authority.
func decodeCleanupPending(n Namespace, epoch string, kv *mvccpb.KeyValue, revision int64) (cleanupPendingRecord, error) {
	var r cleanupPendingRecord
	if !validDomainSegment(epoch) || revision <= 0 || kv == nil || kv.Version != 1 || kv.Lease != 0 || kv.CreateRevision <= 0 || kv.ModRevision != kv.CreateRevision || kv.ModRevision > revision || len(kv.Value) == 0 || len(kv.Value) > cleanupPendingBytes {
		return r, ErrCorruptRecord
	}
	if strictPreparationMetadata(kv.Value, reflect.TypeOf(r)) != nil || json.Unmarshal(kv.Value, &r) != nil || r.Validate() != nil || r.Reference.RestoreEpoch != epoch {
		return cleanupPendingRecord{}, ErrCorruptRecord
	}
	key, err := n.cleanupPendingKey(r.Reference)
	if err != nil || string(kv.Key) != key {
		return cleanupPendingRecord{}, ErrCorruptRecord
	}
	wire, err := encodeCleanupPending(r)
	if err != nil || !bytes.Equal(kv.Value, []byte(wire)) {
		return cleanupPendingRecord{}, ErrCorruptRecord
	}
	return r, nil
}
