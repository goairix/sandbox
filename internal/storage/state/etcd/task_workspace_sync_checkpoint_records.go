package etcd

import (
	"bytes"
	"encoding/json"
	"math"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// TaskWorkspaceSyncCheckpoint is copied diagnostic progress. It grants no
// archive consumption, physical target or execution authority.
type TaskWorkspaceSyncCheckpoint struct {
	Version            uint32 `json:"version"`
	FinalSyncAttemptID string `json:"final_sync_attempt_id"`
	PlanDigest         string `json:"plan_digest"`
	ArchiveSHA256      string `json:"archive_sha256"`
	ArchiveBytes       uint64 `json:"archive_bytes"`
	ArchiveEntries     uint64 `json:"archive_entries"`
	TicketDigest       string `json:"ticket_digest"`
	TargetDigest       string `json:"target_digest"`
	EntryOrdinal       uint64 `json:"entry_ordinal"`
	BodyEnd            uint64 `json:"body_end"`
	PathDigest         string `json:"path_digest"`
	BodySHA256         string `json:"body_sha256"`
	IntentID           string `json:"intent_id"`
	IntentBirth        int64  `json:"intent_birth"`
	EvidenceDigest     string `json:"evidence_digest"`
}

func (r TaskWorkspaceSyncCheckpoint) Validate() error {
	if r.Version != 1 || !validPreparationUUID(r.FinalSyncAttemptID) || !validPreparationUUID(r.IntentID) || r.ArchiveBytes < 1536 || r.ArchiveBytes > math.MaxInt64 || r.ArchiveEntries == 0 || r.EntryOrdinal == 0 || r.EntryOrdinal > r.ArchiveEntries || r.BodyEnd == 0 || r.BodyEnd > r.ArchiveBytes || r.IntentBirth <= 0 || r.IntentBirth == math.MaxInt64 {
		return ErrInvalidRecord
	}
	for _, digest := range []string{r.PlanDigest, r.ArchiveSHA256, r.TicketDigest, r.TargetDigest, r.PathDigest, r.BodySHA256, r.EvidenceDigest} {
		if !validHexDigest(digest) {
			return ErrInvalidRecord
		}
	}
	return nil
}

// Separate wire types preserve the exact v1 six-field JSON and its mandatory
// empty fields. V2 has a mandatory nonnull seventh field, without omitempty.
type taskCheckpointV1Wire struct {
	Version      uint32              `json:"version"`
	Reference    TaskReference       `json:"reference"`
	ClaimID      string              `json:"claim_id"`
	DetailDigest string              `json:"detail_digest"`
	State        TaskCheckpointState `json:"state"`
	Attempt      StageAttemptLocator `json:"attempt"`
}

type taskCheckpointV2Wire struct {
	taskCheckpointV1Wire
	WorkspaceSync *TaskWorkspaceSyncCheckpoint `json:"workspace_sync"`
}

func (w taskCheckpointV1Wire) record() TaskCheckpointRecord {
	return TaskCheckpointRecord{Version: w.Version, Reference: w.Reference, ClaimID: w.ClaimID, DetailDigest: w.DetailDigest, State: w.State, Attempt: w.Attempt}
}
func (w taskCheckpointV1Wire) Validate() error {
	if w.Version != 1 {
		return ErrInvalidRecord
	}
	return w.record().Validate()
}
func (w taskCheckpointV2Wire) record() TaskCheckpointRecord {
	r := w.taskCheckpointV1Wire.record()
	if w.WorkspaceSync != nil {
		owned := *w.WorkspaceSync
		r.WorkspaceSync = &owned
	}
	return r
}
func (w taskCheckpointV2Wire) Validate() error {
	if w.Version != 2 {
		return ErrInvalidRecord
	}
	return w.record().Validate()
}

func encodeTaskCheckpointRecord(r TaskCheckpointRecord) (string, error) {
	if r.Validate() != nil {
		return "", ErrInvalidRecord
	}
	w := taskCheckpointV1Wire{r.Version, r.Reference, r.ClaimID, r.DetailDigest, r.State, r.Attempt}
	if r.Version == 1 {
		return encodeOperationMetadata(w, 4096)
	}
	owned := *r.WorkspaceSync
	return encodeOperationMetadata(taskCheckpointV2Wire{w, &owned}, 4096)
}

func decodeTaskCheckpointRecord(kv *mvccpb.KeyValue, dst *TaskCheckpointRecord) error {
	if kv == nil || dst == nil || len(kv.Value) > 4096 {
		return ErrCorruptRecord
	}
	var version struct {
		Version uint32 `json:"version"`
	}
	if json.Unmarshal(kv.Value, &version) != nil {
		return ErrCorruptRecord
	}
	if version.Version == 1 {
		var w taskCheckpointV1Wire
		if decodeTaskMetadata(kv, &w, 4096, true) != nil {
			return ErrCorruptRecord
		}
		*dst = w.record()
		return nil
	}
	if version.Version != 2 || kv.ModRevision == math.MaxInt64 || kv.Version <= 0 || kv.Version == math.MaxInt64 {
		return ErrCorruptRecord
	}
	var w taskCheckpointV2Wire
	if decodeTaskMetadata(kv, &w, 4096, true) != nil {
		return ErrCorruptRecord
	}
	r := w.record()
	canonical, err := encodeTaskCheckpointRecord(r)
	if err != nil || !bytes.Equal(kv.Value, []byte(canonical)) {
		return ErrCorruptRecord
	}
	*dst = r
	return nil
}
