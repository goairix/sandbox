package etcd

import (
	"context"
	"sync"
)

const (
	taskArchiveRecoveryMetadataBytes = 65536
	taskArchiveRecoveryWireBound     = 53121
	taskArchiveRecoverySlots         = 64
)

type taskArchiveRecoverySeed struct {
	Task              TaskReference `json:"task"`
	TaskBirth         int64         `json:"task_birth"`
	TaskDigest        string        `json:"task_digest"`
	ClaimID           string        `json:"claim_id"`
	WorkerID          string        `json:"worker_id"`
	LeaseID           int64         `json:"lease_id"`
	ClaimRevision     int64         `json:"claim_revision"`
	ClaimDigest       string        `json:"claim_digest"`
	FinalSyncAttempt  string        `json:"final_sync_attempt"`
	FinalSyncRevision int64         `json:"final_sync_revision"`
	FinalSyncDigest   string        `json:"final_sync_digest"`
	ArchiveCommand    string        `json:"archive_command"`
	ArchiveRevision   int64         `json:"archive_revision"`
	ArchiveDigest     string        `json:"archive_digest"`
	IssuerRevision    int64         `json:"issuer_revision"`
	IssuerDigest      string        `json:"issuer_digest"`
	TicketDigest      string        `json:"ticket_digest"`
}

type taskArchiveRecoveryRoot struct {
	Dev            uint64 `json:"dev"`
	Ino            uint64 `json:"ino"`
	MountID        uint64 `json:"mount_id"`
	Filesystem     uint64 `json:"filesystem"`
	UID            uint32 `json:"uid"`
	GID            uint32 `json:"gid"`
	BootID         string `json:"boot_id"`
	MountNamespace uint64 `json:"mount_namespace"`
}

type taskArchiveRecoveryArtifact struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

type taskArchiveRecoveryReceived struct {
	Bytes   uint64 `json:"bytes"`
	Entries uint64 `json:"entries"`
	SHA256  string `json:"sha256"`
}

type taskArchiveRecoveryPlanWire struct {
	Provider            string   `json:"provider"`
	StorageIdentityHash string   `json:"storage_identity_hash"`
	Bucket              string   `json:"bucket"`
	Prefix              string   `json:"prefix"`
	Hash                string   `json:"hash"`
	Partition           uint8    `json:"partition"`
	Exclude             []string `json:"exclude"`
	DeleteExtraneous    bool     `json:"delete_extraneous"`
	Digest              string   `json:"digest"`
}

type taskArchiveRecoveryBinding struct {
	Plan         taskArchiveRecoveryPlanWire       `json:"plan"`
	Local        taskWorkspaceSyncLocalAttribution `json:"local"`
	TargetDigest string                            `json:"target_digest"`
}

type taskArchiveRecoveryManifest struct {
	Version      uint32                       `json:"version"`
	Slot         uint32                       `json:"slot"`
	State        string                       `json:"state"`
	StoreID      string                       `json:"store_id"`
	LimitsDigest string                       `json:"limits_digest"`
	Root         taskArchiveRecoveryRoot      `json:"root"`
	Seed         taskArchiveRecoverySeed      `json:"seed"`
	Artifact     *taskArchiveRecoveryArtifact `json:"artifact,omitempty"`
	Received     *taskArchiveRecoveryReceived `json:"received,omitempty"`
	Binding      *taskArchiveRecoveryBinding  `json:"binding,omitempty"`
}

// A cold snapshot has only fixed value fields. In particular it contains no
// file, index, plan, target, source, claim or witness capability and no slices.
type taskArchiveRecoveryDiagnostic struct {
	Occupied, Quarantined                     bool
	State                                     uint8 // 1 pending, 2 received, 3 bound
	TaskID                                    [36]byte
	StoreID, SHA256, PlanDigest, TargetDigest [32]byte
	Bytes, Entries                            uint64
}

type taskArchiveRecoverySlot struct {
	diagnostic taskArchiveRecoveryDiagnostic
}

type taskArchiveRecoveryBuffers struct {
	input, output [taskArchiveRecoveryMetadataBytes]byte
}

type taskArchiveRecoveryStore struct {
	root         taskArchiveRecoveryRoot
	directory    string
	storeID      string
	limitsDigest string
	memoryBytes  uint64
	gate         chan struct{}
	buffers      *taskArchiveRecoveryBuffers // allocated only after a slot charge
	slots        [taskArchiveRecoverySlots]taskArchiveRecoverySlot
	// Test-only suspension at real filesystem/postguard windows. Never exposed
	// by options or cold diagnostics; set before starting the original operation.
	probe func(string)
}

type taskArchiveRecoveryOwner struct {
	slot     uint32
	manifest taskArchiveRecoveryManifest // only accessed with metadata gate held
	metadata sync.WaitGroup              // Add under store.mu; Close seals then Wait
	ctx      context.Context             // independent Close seal, not send/session expiry
	cancel   context.CancelFunc
}
