package etcd

import (
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// TaskReference locates permanent diagnostic metadata. It is never a claim or
// permission to execute cleanup, and cannot reconstruct a previous owner.
type TaskReference struct {
	Namespace    string `json:"namespace"`
	RestoreEpoch string `json:"restore_epoch"`
	TaskID       string `json:"task_id"`
	SandboxID    string `json:"sandbox_id"`
	Partition    uint8  `json:"partition"`
}
type TaskRecord struct {
	Version          uint32            `json:"version"`
	Reference        TaskReference     `json:"reference"`
	Kind             string            `json:"kind"`
	WorkspaceHash    string            `json:"workspace_hash"`
	CreationIntentID string            `json:"creation_intent_id"`
	Generation       int64             `json:"generation"`
	DataGateEpoch    int64             `json:"data_gate_epoch"`
	ControlRevision  int64             `json:"control_revision"`
	Runtime          RuntimeReference  `json:"runtime"`
	Snapshot         SnapshotReference `json:"snapshot"`
	ExpiresAt        time.Time         `json:"expires_at"`
}
type CleanupIntentRecord struct {
	Version uint32              `json:"version"`
	Task    TaskRecord          `json:"task"`
	Attempt StageAttemptLocator `json:"attempt"`
}
type TaskLinkRecord struct {
	Version   uint32        `json:"version"`
	Reference TaskReference `json:"reference"`
}
type TaskCheckpointState string

const (
	TaskCheckpointPending             TaskCheckpointState = "pending"
	TaskCheckpointNeedsReconciliation TaskCheckpointState = "needs_reconciliation"
)

type TaskCheckpointRecord struct {
	Version      uint32              `json:"version"`
	Reference    TaskReference       `json:"reference"`
	ClaimID      string              `json:"claim_id"`
	DetailDigest string              `json:"detail_digest"`
	State        TaskCheckpointState `json:"state"`
	Attempt      StageAttemptLocator `json:"attempt"`
}

func (r TaskReference) Validate() error {
	if len(r.Namespace) > maxNamespaceRootBytes || !strings.HasSuffix(r.Namespace, "/") {
		return ErrInvalidRecord
	}
	parts := strings.Split(strings.TrimSuffix(r.Namespace, "/"), "/")
	if len(parts) < 4 {
		return ErrInvalidRecord
	}
	n, err := NewNamespace(strings.Join(parts[:len(parts)-2], "/"), parts[len(parts)-2], parts[len(parts)-1])
	if err != nil || n.Root() != r.Namespace || !validDomainSegment(r.RestoreEpoch) || !validDomainSegment(r.SandboxID) || !validPreparationUUID(r.TaskID) {
		return ErrInvalidRecord
	}
	return nil
}
func (r TaskRecord) Validate() error {
	if r.Reference.Validate() != nil || r.Kind != "cleanup" || !validOwnership(r.Version, r.WorkspaceHash, r.Reference.SandboxID, r.CreationIntentID, r.Reference.RestoreEpoch, r.Generation) || hashPartition(r.WorkspaceHash) != r.Reference.Partition || r.DataGateEpoch <= 0 || r.ControlRevision <= 0 || r.Runtime.Validate() != nil || r.Snapshot.Validate() != nil || !validDomainExpiry(r.ExpiresAt) {
		return ErrInvalidRecord
	}
	return nil
}
func taskAttemptMatches(a StageAttemptLocator, r TaskReference) bool {
	return a.Validate() == nil && a.Namespace == r.Namespace && a.RestoreEpoch == r.RestoreEpoch && a.Partition == r.Partition
}
func (r CleanupIntentRecord) Validate() error {
	if r.Version != 1 || r.Task.Validate() != nil || !taskAttemptMatches(r.Attempt, r.Task.Reference) {
		return ErrInvalidRecord
	}
	return nil
}
func (r TaskLinkRecord) Validate() error {
	if r.Version != 1 || r.Reference.Validate() != nil {
		return ErrInvalidRecord
	}
	return nil
}
func (r TaskCheckpointRecord) Validate() error {
	if r.Version != 1 || r.Reference.Validate() != nil || !taskAttemptMatches(r.Attempt, r.Reference) || (r.State != TaskCheckpointPending && r.State != TaskCheckpointNeedsReconciliation) {
		return ErrInvalidRecord
	}
	if r.ClaimID == "" && r.DetailDigest == "" && r.State != TaskCheckpointPending {
		return ErrInvalidRecord
	}
	if r.ClaimID != "" || r.DetailDigest != "" {
		if !validPreparationUUID(r.ClaimID) || !validHexDigest(r.DetailDigest) {
			return ErrInvalidRecord
		}
	}
	return nil
}
func encodeTaskRecord(r TaskRecord) (string, error) { return encodeOperationMetadata(r, 4096) }
func encodeCleanupIntentRecord(r CleanupIntentRecord) (string, error) {
	return encodeOperationMetadata(r, 4096)
}
func encodeTaskLinkRecord(r TaskLinkRecord) (string, error) { return encodeOperationMetadata(r, 2048) }
func encodeTaskCheckpointRecord(r TaskCheckpointRecord) (string, error) {
	return encodeOperationMetadata(r, 4096)
}

// Keep task envelope policy local: unlike the immutable task/intent/link, a
// checkpoint may advance through metadata CAS, but must remain permanent.
func decodeTaskMetadata[T interface{ Validate() error }](kv *mvccpb.KeyValue, dst *T, limit int, mutable bool) error {
	if kv == nil || dst == nil || kv.Lease != 0 || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || (!mutable && kv.ModRevision != kv.CreateRevision) {
		return ErrCorruptRecord
	}
	return decodePreparationDomain(kv, dst, limit)
}
func decodeTaskRecord(kv *mvccpb.KeyValue, dst *TaskRecord) error {
	return decodeTaskMetadata(kv, dst, 4096, false)
}
func decodeCleanupIntentRecord(kv *mvccpb.KeyValue, dst *CleanupIntentRecord) error {
	return decodeTaskMetadata(kv, dst, 4096, false)
}
func decodeTaskLinkRecord(kv *mvccpb.KeyValue, dst *TaskLinkRecord) error {
	return decodeTaskMetadata(kv, dst, 2048, false)
}
func decodeTaskCheckpointRecord(kv *mvccpb.KeyValue, dst *TaskCheckpointRecord) error {
	return decodeTaskMetadata(kv, dst, 4096, true)
}
