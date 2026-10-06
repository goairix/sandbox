package etcd

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

type creationClaimRecord struct {
	Version       uint32 `json:"version"`
	ClaimID       string `json:"claim_id"`
	WorkerID      string `json:"worker_id"`
	IntentID      string `json:"intent_id"`
	SandboxID     string `json:"sandbox_id"`
	WorkspaceHash string `json:"workspace_hash"`
	RestoreEpoch  string `json:"restore_epoch"`
	Partition     uint8  `json:"partition"`
	Generation    int64  `json:"generation"`
	DataGateEpoch int64  `json:"data_gate_epoch"`
	LeaseID       int64  `json:"lease_id"`
}

func (r creationClaimRecord) validate() error {
	id, err := uuid.Parse(r.ClaimID)
	if err != nil || id.String() != r.ClaimID || r.Version != 1 || !validDomainSegment(r.WorkerID) || !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || hashPartition(r.WorkspaceHash) != r.Partition || r.DataGateEpoch <= 0 || r.LeaseID <= 0 {
		return ErrCorruptRecord
	}
	return nil
}
func encodeCreationClaim(r creationClaimRecord) (string, error) {
	if err := r.validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	if len(encoded) > 4096 {
		return "", ErrInvalidRecord
	}
	return string(encoded), nil
}
func decodeCreationClaim(kv *mvccpb.KeyValue) (*creationClaimRecord, error) {
	if kv == nil || kv.Lease <= 0 || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || len(kv.Value) > 4096 || !utf8.Valid(kv.Value) {
		return nil, ErrCorruptRecord
	}
	var r creationClaimRecord
	decoder := json.NewDecoder(bytes.NewReader(kv.Value))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil {
		return nil, ErrCorruptRecord
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, ErrCorruptRecord
	}
	if r.validate() != nil || r.LeaseID != kv.Lease {
		return nil, ErrCorruptRecord
	}
	return &r, nil
}
func (r creationClaimRecord) reference(revision int64) CreationClaimReference {
	return CreationClaimReference{ClaimID: r.ClaimID, WorkerID: r.WorkerID, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, Partition: r.Partition, Generation: r.Generation, DataGateEpoch: r.DataGateEpoch, CreateRevision: revision, LeaseID: r.LeaseID}
}
