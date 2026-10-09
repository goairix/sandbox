package etcd

import (
	"errors"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

var errTaskWorkspaceSyncIntentQuota = errors.New("workspace sync native slots occupied")

// TaskWorkspaceSyncBudgetOptions binds existing operator-owned permanent slots.
type TaskWorkspaceSyncBudgetOptions struct {
	BudgetID          string `json:"budget_id"`
	SlotsPerPartition uint32 `json:"slots_per_partition"`
	MaxIntentBytes    uint32 `json:"max_intent_bytes"`
}

type taskWorkspaceSyncBudgetRecord struct {
	Version uint32 `json:"version"`
	TaskWorkspaceSyncBudgetOptions
}

type taskWorkspaceSyncIntentResult struct {
	Reference TaskWorkspaceSyncIntentReference
	Outcome   Outcome
}

// TaskWorkspaceSyncIntentReference locates metadata; it grants no dispatch authority.
type TaskWorkspaceSyncIntentReference struct {
	Task     TaskReference `json:"task"`
	BudgetID string        `json:"budget_id"`
	IntentID string        `json:"intent_id"`
	Slot     uint32        `json:"slot"`
	Revision int64         `json:"revision"`
}

type taskWorkspaceSyncIntentAttribution struct {
	Version             uint32                             `json:"version"`
	Reference           TaskWorkspaceSyncIntentReference   `json:"reference"`
	BudgetRevision      int64                              `json:"budget_revision"`
	Kind                string                             `json:"kind"`
	FinalSyncAttemptID  string                             `json:"final_sync_attempt_id"`
	CommandID           string                             `json:"command_id"`
	TicketDigest        string                             `json:"ticket_digest"`
	ArchiveSHA256       string                             `json:"archive_sha256"`
	PlanDigest          string                             `json:"plan_digest"`
	WorkspaceHash       string                             `json:"workspace_hash"`
	Provider            string                             `json:"provider"`
	StorageIdentityHash string                             `json:"storage_identity_hash"`
	Bucket              string                             `json:"bucket"`
	Prefix              string                             `json:"prefix"`
	RelativePath        string                             `json:"relative_path"`
	Mode                int64                              `json:"mode"`
	Size                int64                              `json:"size"`
	Offset              int64                              `json:"offset"`
	BodySHA256          string                             `json:"body_sha256"`
	Local               *taskWorkspaceSyncLocalAttribution `json:"local,omitempty"`
}

// TaskWorkspaceSyncIntentRecord records structural attribution only. Claim.WorkerID
// is the claim label, never a physical worker identity or settlement evidence.
type TaskWorkspaceSyncIntentRecord struct {
	taskWorkspaceSyncIntentAttribution
	Claim TaskClaimReference
}

type taskWorkspaceSyncIntentWire struct {
	taskWorkspaceSyncIntentAttribution
	Claim taskCloseClaimWire `json:"claim"`
}

// TaskWorkspaceSyncIntentEntry is copied historical metadata, never a source.
type TaskWorkspaceSyncIntentEntry struct {
	Reference TaskWorkspaceSyncIntentReference
	Record    TaskWorkspaceSyncIntentRecord
	Revision  int64
}

type taskWorkspaceSyncSlotRecord struct {
	Version      uint32 `json:"version"`
	BudgetID     string `json:"budget_id"`
	Partition    uint8  `json:"partition"`
	Slot         uint32 `json:"slot"`
	IntentID     string `json:"intent_id"`
	IntentDigest string `json:"intent_digest"`
}

type taskWorkspaceSyncBudgetPin struct {
	profile     uint8 // zero is legacy-compatible only for internal fixtures
	options     TaskWorkspaceSyncBudgetOptions
	key, value  string
	create, mod int64
}

func (p *taskWorkspaceSyncBudgetPin) legacyProfile() bool {
	return p == nil || p.profile == 0 || p.profile == 1
}

type taskWorkspaceSyncIntentDraft struct {
	record                               TaskWorkspaceSyncIntentRecord
	intentKey, slotKey, value, slotValue string
	writeAttempted                       bool
	revision                             int64
	acknowledged                         bool // only original full successful ACK + local postguard
}

func immutableWorkspaceSyncKV(kv *mvccpb.KeyValue) bool {
	return kv != nil && kv.Lease == 0 && kv.Version == 1 && kv.CreateRevision > 0 && kv.ModRevision == kv.CreateRevision
}
