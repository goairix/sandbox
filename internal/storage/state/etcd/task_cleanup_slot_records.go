package etcd

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

const (
	// These permanent records bound physical cleanup admission, independently
	// of leased Task claims and the original scheduling token buckets.
	cleanupExecutionSlots      = 32
	cleanupExecutionBytes      = 4096
	cleanupExecutionDomain     = "sandbox.cleanup-execution.v1"
	cleanupExecutionSlotDomain = "sandbox.cleanup-execution-slot.v1"
)

// Attribution is repeated deliberately in E and S: one immutable occupation
// binds the Task's original committed birth, rather than a reusable numeric key.
type cleanupExecutionRecord struct {
	Version               uint32              `json:"version"`
	Domain                string              `json:"domain"`
	Task                  TaskReference       `json:"task"`
	TaskBirth             int64               `json:"task_birth"`
	InitialAttempt        StageAttemptLocator `json:"initial_attempt"`
	TaskDigest            string              `json:"task_digest"`
	InitialMutationDigest string              `json:"initial_mutation_digest"`
	InitialReceiptDigest  string              `json:"initial_receipt_digest"`

	Incarnation string                   `json:"incarnation"`
	Selection   uint64                   `json:"selection"`
	Slot        uint32                   `json:"slot"`
	Phase       string                   `json:"phase"`
	Run         *cleanupExecutionRun     `json:"run,omitempty"`
	Confirmed   string                   `json:"confirmed"`
	Pending     *cleanupExecutionPending `json:"pending,omitempty"`
	Archive     *cleanupExecutionArchive `json:"archive,omitempty"`
	Release     *cleanupExecutionRelease `json:"release,omitempty"`
}

type cleanupExecutionSlotRecord struct {
	Version               uint32              `json:"version"`
	Domain                string              `json:"domain"`
	Task                  TaskReference       `json:"task"`
	TaskBirth             int64               `json:"task_birth"`
	InitialAttempt        StageAttemptLocator `json:"initial_attempt"`
	TaskDigest            string              `json:"task_digest"`
	InitialMutationDigest string              `json:"initial_mutation_digest"`
	InitialReceiptDigest  string              `json:"initial_receipt_digest"`
	Incarnation           string              `json:"incarnation"`
	Selection             uint64              `json:"selection"`
	Slot                  uint32              `json:"slot"`
}

// Run diagnostics cannot reconstruct a KernelBoundary. Joined is written only
// by the original job after actual effect-bearing work and cleanup have joined.
type cleanupExecutionRun struct {
	ID     string                                `json:"id"`
	Worker launcher.WorkspaceSyncWorkerReference `json:"worker"`
	Joined bool                                  `json:"joined"`
}

// Pending retains the same uncertain attempt; absence is never inferred from
// a cancelled request or an empty read. Original history still authorizes work.
type cleanupExecutionPending struct {
	Purpose   string `json:"purpose"`
	ClaimID   string `json:"claim_id,omitempty"`
	StageID   string `json:"stage_id,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	AttemptID string `json:"attempt_id,omitempty"`
	Digest    string `json:"digest,omitempty"`
	LeaseID   int64  `json:"lease_id,omitempty"`
}

type cleanupExecutionArchive struct {
	StoreID       string `json:"store_id"`
	Slot          uint32 `json:"slot"`
	BindingDigest string `json:"binding_digest"`
}

type cleanupExecutionRelease struct {
	CreateRevision int64  `json:"create_revision"`
	Digest         string `json:"digest"`
}

func cleanupSlotRecordFromExecution(r cleanupExecutionRecord) cleanupExecutionSlotRecord {
	return cleanupExecutionSlotRecord{r.Version, cleanupExecutionSlotDomain, r.Task, r.TaskBirth, r.InitialAttempt, r.TaskDigest, r.InitialMutationDigest, r.InitialReceiptDigest, r.Incarnation, r.Selection, r.Slot}
}

func cleanupIncarnation(r cleanupExecutionSlotRecord) string {
	task, _ := json.Marshal(r.Task)
	attempt, _ := json.Marshal(r.InitialAttempt)
	digest := framedDigest("sandbox.cleanup-execution-incarnation.v1", string(task), strconv.FormatInt(r.TaskBirth, 10), string(attempt), r.TaskDigest, r.InitialMutationDigest, r.InitialReceiptDigest)
	return uuid.NewSHA1(uuid.NameSpaceOID, digest[:]).String()
}

func cleanupCandidate(digest string, selection uint64) uint32 {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return cleanupExecutionSlots
	}
	return uint32((uint64(decoded[0]) + selection%cleanupExecutionSlots) % cleanupExecutionSlots)
}

// Validate checks the permanent occupation schema and its deterministic Task
// incarnation; diagnostic attribution alone grants no execution permission.
func (r cleanupExecutionSlotRecord) Validate() error {
	if r.Version != 1 || r.Domain != cleanupExecutionSlotDomain || r.Task.Validate() != nil || r.TaskBirth <= 0 || !taskAttemptMatches(r.InitialAttempt, r.Task) || !validHexDigest(r.TaskDigest) || !validHexDigest(r.InitialMutationDigest) || !validHexDigest(r.InitialReceiptDigest) || r.Slot >= cleanupExecutionSlots || r.Slot != cleanupCandidate(r.TaskDigest, r.Selection) {
		return ErrInvalidRecord
	}
	id, err := uuid.Parse(r.Incarnation)
	if err != nil || id.Version() != 5 || id.Variant() != uuid.RFC4122 || id.String() != r.Incarnation || cleanupIncarnation(r) != r.Incarnation {
		return ErrInvalidRecord
	}
	return nil
}

func (r cleanupExecutionPending) valid() bool {
	switch r.Purpose {
	case "claim", "close", "quiescence", "final-sync", "archive", "finalize", "termination", "owner-release":
	default:
		return false
	}
	if r.ClaimID != "" && !validPreparationUUID(r.ClaimID) || r.LeaseID < 0 || r.Purpose == "claim" && r.ClaimID == "" {
		return false
	}
	if r.StageID == "" && r.RequestID == "" && r.AttemptID == "" && r.Digest == "" {
		return true
	}
	return validDomainSegment(r.StageID) && validDomainSegment(r.RequestID) && validPreparationUUID(r.AttemptID) && validHexDigest(r.Digest)
}

// Validate checks the execution schema, closed state vocabulary and attribution.
// A valid record does not prove a native transition or an original run's join.
func (r cleanupExecutionRecord) Validate() error {
	if r.Domain != cleanupExecutionDomain || cleanupSlotRecordFromExecution(r).Validate() != nil {
		return ErrInvalidRecord
	}
	switch r.Confirmed {
	case "queued", "reserved", "claimed", "closed", "quiescent", "drained", "archive-bound", "local-complete", "terminated", "owner-released", "settled":
	default:
		return ErrInvalidRecord
	}
	if r.Run != nil && (!cleanupRunUUID(r.Run.ID) || r.Run.Worker.Validate() != nil) || r.Pending != nil && !r.Pending.valid() || r.Archive != nil && (!validPreparationUUID(r.Archive.StoreID) || r.Archive.Slot >= 64 || !validHexDigest(r.Archive.BindingDigest)) || r.Release != nil && (r.Release.CreateRevision <= 0 || !validHexDigest(r.Release.Digest)) {
		return ErrInvalidRecord
	}
	switch r.Phase {
	case "selecting":
		if r.Confirmed != "queued" || r.Run != nil || r.Pending != nil || r.Archive != nil || r.Release != nil {
			return ErrInvalidRecord
		}
	case "occupied":
		if r.Confirmed == "queued" || r.Confirmed == "settled" || r.Release != nil || r.Run == nil && (r.Confirmed != "reserved" || r.Pending != nil || r.Archive != nil) {
			return ErrInvalidRecord
		}
	case "settled":
		if r.Confirmed != "settled" || r.Release == nil || r.Pending != nil || r.Run != nil && !r.Run.Joined {
			return ErrInvalidRecord
		}
	default:
		return ErrInvalidRecord
	}
	return nil
}

func cleanupRunUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id.Version() == 4 && id.Variant() == uuid.RFC4122 && id.String() == value
}

func encodeCleanupExecution(r cleanupExecutionRecord) (string, error) {
	return encodeOperationMetadata(r, cleanupExecutionBytes)
}

func encodeCleanupExecutionSlot(r cleanupExecutionSlotRecord) (string, error) {
	return encodeOperationMetadata(r, cleanupExecutionBytes)
}

func (n Namespace) cleanupExecutionKey(ref TaskReference) (string, error) {
	if ref.Validate() != nil || ref.Namespace != n.Root() {
		return "", ErrInvalidRecord
	}
	return n.Key("p", fmt.Sprintf("%02x", ref.Partition), "tasks", ref.TaskID, "execution")
}

func (n Namespace) cleanupExecutionSlotKey(slot uint32) (string, error) {
	if slot >= cleanupExecutionSlots {
		return "", ErrInvalidRecord
	}
	return n.Key("cleanup-execution-slots", fmt.Sprintf("%02d", slot))
}

// Strict decoding is atomic and canonical. Exact re-encoding rejects missing,
// duplicate, unknown and null fields, including optional nulls and nested worker
// fields, without changing the older required-fields-only metadata parser.
// Header/key/Lease checks remain part of this evidence envelope.
func decodeCleanupExecution(kv *mvccpb.KeyValue, revision int64, ref TaskReference, dst *cleanupExecutionRecord) error {
	if dst == nil || kv == nil || kv.Lease != 0 || kv.Version < 1 || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > revision || (kv.Version == 1) != (kv.CreateRevision == kv.ModRevision) || string(kv.Key) != fmt.Sprintf("%sp/%02x/tasks/%s/execution", ref.Namespace, ref.Partition, ref.TaskID) {
		return ErrCorruptRecord
	}
	var fresh cleanupExecutionRecord
	if len(kv.Value) > cleanupExecutionBytes || !utf8.Valid(kv.Value) || json.Unmarshal(kv.Value, &fresh) != nil || fresh.Task != ref || kv.CreateRevision <= fresh.TaskBirth {
		return ErrCorruptRecord
	}
	wire, err := encodeCleanupExecution(fresh)
	if err != nil || wire != string(kv.Value) {
		return ErrCorruptRecord
	}
	*dst = fresh
	return nil
}

func decodeCleanupExecutionSlot(kv *mvccpb.KeyValue, revision int64, root string, slot uint32, dst *cleanupExecutionSlotRecord) error {
	if dst == nil || kv == nil || kv.Lease != 0 || kv.Version != 1 || kv.CreateRevision <= 0 || kv.ModRevision != kv.CreateRevision || kv.ModRevision > revision || string(kv.Key) != fmt.Sprintf("%scleanup-execution-slots/%02d", root, slot) {
		return ErrCorruptRecord
	}
	var fresh cleanupExecutionSlotRecord
	if len(kv.Value) > cleanupExecutionBytes || !utf8.Valid(kv.Value) || json.Unmarshal(kv.Value, &fresh) != nil || fresh.Task.Namespace != root || fresh.Slot != slot || kv.CreateRevision <= fresh.TaskBirth {
		return ErrCorruptRecord
	}
	wire, err := encodeCleanupExecutionSlot(fresh)
	if err != nil || wire != string(kv.Value) {
		return ErrCorruptRecord
	}
	*dst = fresh
	return nil
}
