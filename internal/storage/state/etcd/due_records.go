package etcd

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

const (
	dueRecordBytes = 1024
	dueTimeLayout  = "20060102T150405.000000000Z"
)

// DueCandidateRecord is copied discovery data. It never grants permission to
// change control, claim a task, release ownership or affect a runtime.
type DueCandidateRecord struct {
	Version       uint32    `json:"version"`
	Partition     uint8     `json:"partition"`
	SandboxID     string    `json:"sandbox_id"`
	WorkspaceHash string    `json:"workspace_hash"`
	RestoreEpoch  string    `json:"restore_epoch"`
	Generation    int64     `json:"generation"`
	IntentID      string    `json:"intent_id"`
	ExpiresAt     time.Time `json:"expires_at"`
}

func (r DueCandidateRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || !validDomainExpiry(r.ExpiresAt) || r.ExpiresAt.Location() != time.UTC {
		return ErrInvalidRecord
	}
	hash, err := hex.DecodeString(r.WorkspaceHash)
	if err != nil || len(hash) != 32 || hash[0] != r.Partition {
		return ErrInvalidRecord
	}
	return nil
}

func (n Namespace) dueCandidateKey(r DueCandidateRecord) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	return n.Key("p", fmt.Sprintf("%02x", r.Partition), "due", r.ExpiresAt.Format(dueTimeLayout), r.SandboxID, fmt.Sprintf("%019d", r.Generation), r.IntentID)
}

func encodeDueCandidate(r DueCandidateRecord) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	wire, err := json.Marshal(r)
	if err != nil || len(wire) > dueRecordBytes {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}

func decodeDueCandidate(n Namespace, kv *mvccpb.KeyValue, revision int64) (DueCandidateRecord, error) {
	var r DueCandidateRecord
	if !operationPermanentKV(kv) || kv.Version <= 0 || kv.ModRevision > revision || len(kv.Value) == 0 || len(kv.Value) > dueRecordBytes || strictPreparationMetadata(kv.Value, reflect.TypeOf(r)) != nil || json.Unmarshal(kv.Value, &r) != nil {
		return r, ErrCorruptRecord
	}
	wire, err := encodeDueCandidate(r)
	if err != nil || !bytes.Equal(kv.Value, []byte(wire)) {
		return DueCandidateRecord{}, ErrCorruptRecord
	}
	key, err := n.dueCandidateKey(r)
	if err != nil || string(kv.Key) != key {
		return DueCandidateRecord{}, ErrCorruptRecord
	}
	return r, nil
}

func dueCandidateForControl(partition uint8, control SandboxControlRecord) DueCandidateRecord {
	return DueCandidateRecord{Version: 1, Partition: partition, SandboxID: control.SandboxID, WorkspaceHash: control.WorkspaceHash, RestoreEpoch: control.RestoreEpoch, Generation: control.Generation, IntentID: control.IntentID, ExpiresAt: control.ExpiresAt.UTC()}
}
