package etcd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// RuntimeDispatchKind identifies the runtime operation recorded by metadata.
type RuntimeDispatchKind string

const (
	RuntimeDispatchCreate  RuntimeDispatchKind = "create"
	RuntimeDispatchPrepare RuntimeDispatchKind = "prepare"
)

// DispatchClaimReference attributes the declaration to its original claim.
type DispatchClaimReference struct {
	ClaimID        string `json:"claim_id"`
	WorkerID       string `json:"worker_id"`
	CreateRevision int64  `json:"create_revision"`
	LeaseID        int64  `json:"lease_id"`
}

// RuntimeDispatchRecord is a permanent schema-1 operation declaration.
type RuntimeDispatchRecord struct {
	Version             uint32                 `json:"version"`
	IntentID            string                 `json:"intent_id"`
	SandboxID           string                 `json:"sandbox_id"`
	WorkspaceHash       string                 `json:"workspace_hash"`
	RequestHash         string                 `json:"request_hash"`
	ConfigurationDigest string                 `json:"configuration_digest"`
	RestoreEpoch        string                 `json:"restore_epoch"`
	Generation          int64                  `json:"generation"`
	DataGateEpoch       int64                  `json:"data_gate_epoch"`
	Snapshot            SnapshotReference      `json:"snapshot"`
	ExpiresAt           time.Time              `json:"expires_at"`
	OperationID         string                 `json:"operation_id"`
	Kind                RuntimeDispatchKind    `json:"kind"`
	Target              string                 `json:"target"`
	PayloadDigest       string                 `json:"payload_digest"`
	Claim               DispatchClaimReference `json:"claim"`
	Attempt             StageAttemptLocator    `json:"attempt"`
}

// RuntimeDispatchInputRecord preserves the compact payload representation.
type RuntimeDispatchInputRecord struct {
	Version       uint32          `json:"version"`
	IntentID      string          `json:"intent_id"`
	SandboxID     string          `json:"sandbox_id"`
	WorkspaceHash string          `json:"workspace_hash"`
	RestoreEpoch  string          `json:"restore_epoch"`
	OperationID   string          `json:"operation_id"`
	PayloadDigest string          `json:"payload_digest"`
	Generation    int64           `json:"generation"`
	Payload       json.RawMessage `json:"payload"`
}

func canonicalDispatchUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.String() == value
}
func (r RuntimeDispatchRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) ||
		!validHexDigest(r.RequestHash) ||
		!validHexDigest(r.ConfigurationDigest) ||
		r.DataGateEpoch <= 0 ||
		r.Snapshot.Validate() != nil ||
		!validDomainExpiry(r.ExpiresAt) ||
		!canonicalDispatchUUID(r.OperationID) ||
		(r.Kind != RuntimeDispatchCreate && r.Kind != RuntimeDispatchPrepare) ||
		!validOpaque(r.Target, 128) ||
		!validHexDigest(r.PayloadDigest) ||
		!canonicalDispatchUUID(r.Claim.ClaimID) ||
		!validDomainSegment(r.Claim.WorkerID) ||
		r.Claim.CreateRevision <= 0 ||
		r.Claim.LeaseID <= 0 ||
		r.Attempt.Validate() != nil ||
		r.Attempt.Partition != hashPartition(r.WorkspaceHash) ||
		r.Attempt.RestoreEpoch != r.RestoreEpoch ||
		r.Attempt.StageID != "runtime_dispatch" {
		return fmt.Errorf("%w: invalid runtime dispatch declaration", ErrInvalidRecord)
	}
	return nil
}
func (r RuntimeDispatchInputRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) ||
		!canonicalDispatchUUID(r.OperationID) ||
		!validHexDigest(r.PayloadDigest) {
		return fmt.Errorf("%w: invalid runtime dispatch input", ErrInvalidRecord)
	}
	digest, err := snapshotDigest(r.Payload)
	if err != nil {
		return err
	}
	if digest != r.PayloadDigest {
		return fmt.Errorf("%w: runtime dispatch payload digest mismatch", ErrInvalidRecord)
	}
	return nil
}
func (n Namespace) runtimeDispatchKeys(p uint8, intentID string) (declaration, input string, err error) {
	if !validDomainSegment(intentID) {
		return "", "", ErrInvalidRecord
	}
	declaration, err = n.Key("p", fmt.Sprintf("%02x", p), "intents", intentID, "runtime-dispatch")
	if err != nil {
		return "", "", err
	}
	input, err = n.Key("p", fmt.Sprintf("%02x", p), "intents", intentID, "runtime-input")
	if err != nil {
		return "", "", err
	}
	return declaration, input, nil
}
