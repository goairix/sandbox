package etcd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

type OperationKind string

const (
	OperationData     OperationKind = "data"
	OperationMutation OperationKind = "mutation"
)

// OperationOutcome describes short-lived metadata evidence. Expired means the
// original attempt cannot commit; it does not describe the target runtime.
type OperationOutcome string

const (
	OperationUnknown   OperationOutcome = "unknown"
	OperationCommitted OperationOutcome = "committed"
	OperationAborted   OperationOutcome = "aborted"
	OperationExpired   OperationOutcome = "expired"
)

type OperationReference struct {
	Namespace    string        `json:"namespace"`
	RestoreEpoch string        `json:"restore_epoch"`
	RequestID    string        `json:"request_id"`
	SandboxID    string        `json:"sandbox_id"`
	OperationID  string        `json:"operation_id"`
	Digest       string        `json:"digest"`
	Partition    uint8         `json:"partition"`
	Kind         OperationKind `json:"kind"`
	LeaseID      int64         `json:"lease_id"`
}
type OperationRecord struct {
	Version         uint32             `json:"version"`
	Reference       OperationReference `json:"reference"`
	WorkspaceHash   string             `json:"workspace_hash"`
	IntentID        string             `json:"intent_id"`
	Generation      int64              `json:"generation"`
	DataGateEpoch   int64              `json:"data_gate_epoch"`
	ControlRevision int64              `json:"control_revision"`
	Runtime         RuntimeReference   `json:"runtime"`
	Snapshot        SnapshotReference  `json:"snapshot"`
	ExpiresAt       time.Time          `json:"expires_at"`
}
type operationReceipt struct {
	Version   uint32             `json:"version"`
	Reference OperationReference `json:"reference"`
	Outcome   OperationOutcome   `json:"outcome"`
}

// Validate accepts only references backed by a known original Lease. A Begin
// diagnostic with LeaseID zero is not a recoverable operation reference.
func (r OperationReference) Validate() error {
	if len(r.Namespace) > maxNamespaceRootBytes || !strings.HasSuffix(r.Namespace, "/") {
		return ErrInvalidRecord
	}
	parts := strings.Split(strings.TrimSuffix(r.Namespace, "/"), "/")
	if len(parts) < 4 {
		return ErrInvalidRecord
	}
	n, err := NewNamespace(strings.Join(parts[:len(parts)-2], "/"), parts[len(parts)-2], parts[len(parts)-1])
	if err != nil || n.Root() != r.Namespace || !validDomainSegment(r.RestoreEpoch) || !validDomainSegment(r.RequestID) || !validDomainSegment(r.SandboxID) || !validPreparationUUID(r.OperationID) || !validHexDigest(r.Digest) || r.LeaseID <= 0 || (r.Kind != OperationData && r.Kind != OperationMutation) {
		return ErrInvalidRecord
	}
	return nil
}
func (r OperationRecord) Validate() error {
	if r.Reference.Validate() != nil || !validOwnership(r.Version, r.WorkspaceHash, r.Reference.SandboxID, r.IntentID, r.Reference.RestoreEpoch, r.Generation) || hashPartition(r.WorkspaceHash) != r.Reference.Partition || r.DataGateEpoch <= 0 || r.ControlRevision <= 0 || r.Runtime.Validate() != nil || r.Snapshot.Validate() != nil || !validDomainExpiry(r.ExpiresAt) {
		return ErrInvalidRecord
	}
	return nil
}
func (r operationReceipt) Validate() error {
	if r.Version != 1 || r.Reference.Validate() != nil || (r.Outcome != OperationCommitted && r.Outcome != OperationAborted) {
		return ErrInvalidRecord
	}
	return nil
}
func encodeOperationRecord(r OperationRecord) (string, error) {
	return encodeOperationMetadata(r, 4096)
}
func encodeOperationReceipt(r operationReceipt) (string, error) {
	return encodeOperationMetadata(r, 2048)
}
func encodeOperationMetadata(r interface{ Validate() error }, limit int) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	wire, err := json.Marshal(r)
	if err != nil || len(wire) > limit || !utf8.Valid(wire) {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}
func decodeOperationRecord(kv *mvccpb.KeyValue, dst *OperationRecord) error {
	return decodeOperationMetadata(kv, dst, 4096, ErrCorruptRecord, func(r OperationRecord) OperationReference { return r.Reference })
}
func decodeOperationReceipt(kv *mvccpb.KeyValue, dst *operationReceipt) error {
	return decodeOperationMetadata(kv, dst, 2048, ErrCorruptReceipt, func(r operationReceipt) OperationReference { return r.Reference })
}
func decodeOperationMetadata[T interface{ Validate() error }](kv *mvccpb.KeyValue, dst *T, limit int, corrupt error, reference func(T) OperationReference) error {
	if dst == nil || kv == nil || kv.Lease <= 0 || kv.CreateRevision <= 0 || kv.ModRevision != kv.CreateRevision || len(kv.Value) > limit || !utf8.Valid(kv.Value) {
		return corrupt
	}
	// Decode from owned bytes and replace the destination only after all checks.
	wire := bytes.Clone(kv.Value)
	if strictPreparationMetadata(wire, reflect.TypeOf(dst)) != nil {
		return corrupt
	}
	var fresh T
	if json.Unmarshal(wire, &fresh) != nil || fresh.Validate() != nil || reference(fresh).LeaseID != kv.Lease {
		return corrupt
	}
	*dst = fresh
	return nil
}

// validateOperationCompletion requires the committed token and receipt to share
// their first revision and complete reference. The earlier guard is independent.
func validateOperationCompletion(token, receipt *mvccpb.KeyValue) error {
	var r operationReceipt
	if err := decodeOperationReceipt(receipt, &r); err != nil {
		return err
	}
	var operation OperationRecord
	if err := decodeOperationRecord(token, &operation); err != nil {
		return err
	}
	if r.Outcome != OperationCommitted || r.Reference != operation.Reference || token.CreateRevision != receipt.CreateRevision {
		return ErrCorruptReceipt
	}
	return nil
}
func (n Namespace) operationKeys(r OperationReference) (token, guard, receipt, mutation string, err error) {
	if err = r.Validate(); err != nil {
		return
	}
	if n.Root() != r.Namespace {
		err = ErrIdentityMismatch
		return
	}
	p := fmt.Sprintf("%02x", r.Partition)
	token, err = n.Key("p", p, "sandboxes", r.SandboxID, "operations", r.OperationID)
	if err != nil {
		return
	}
	guard, err = n.Key("p", p, "operation-attempts", r.OperationID, "guard")
	if err != nil {
		return
	}
	receipt, err = n.Key("p", p, "operation-attempts", r.OperationID, "receipt")
	if err != nil {
		return
	}
	mutation, err = n.Key("p", p, "sandboxes", r.SandboxID, "mutation")
	return
}
