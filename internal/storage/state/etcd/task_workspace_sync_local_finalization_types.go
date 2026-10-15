package etcd

// TaskWorkspaceSyncLocalFinalizationResult is owned diagnostic data. It grants
// neither a physical effect nor permission to terminate or release an owner.
type TaskWorkspaceSyncLocalFinalizationResult struct {
	Checkpoint                                                *TaskWorkspaceSyncCheckpoint
	Completion                                                *TaskWorkspaceSyncLocalCompletionReference
	LocalComplete                                             bool
	CompletedFiles, CompletedBytes                            uint64
	CreatedDirectories, RemovedEntries, UpdatedDirectoryModes uint64
}

type TaskWorkspaceSyncLocalCompletionReference struct {
	Task           TaskReference
	CreateRevision int64
	SHA256         string
}

type taskWorkspaceSyncLocalLedger struct {
	Path   string      `json:"path"`
	Frames [][3]uint64 `json:"frames"`
}

type taskWorkspaceSyncLocalProgress struct {
	Version            uint32                       `json:"version"`
	Task               TaskReference                `json:"task"`
	TaskBirth          int64                        `json:"task_birth"`
	Binding            string                       `json:"binding"`
	Claim              taskCloseClaimWire           `json:"claim"`
	Phase              string                       `json:"phase"`
	CheckpointRevision int64                        `json:"checkpoint_revision"`
	CheckpointDigest   string                       `json:"checkpoint_digest"`
	IntentID           string                       `json:"intent_id"`
	IntentBirth        int64                        `json:"intent_birth"`
	EvidenceDigest     string                       `json:"evidence_digest"`
	Ledger             taskWorkspaceSyncLocalLedger `json:"ledger"`
}

type taskWorkspaceSyncLocalComplete struct {
	Version            uint32             `json:"version"`
	Domain             string             `json:"domain"`
	Task               TaskReference      `json:"task"`
	TaskBirth          int64              `json:"task_birth"`
	Binding            string             `json:"binding"`
	Claim              taskCloseClaimWire `json:"claim"`
	ProgressDigest     string             `json:"progress_digest"`
	EvidenceDigest     string             `json:"evidence_digest"`
	CheckpointRevision int64              `json:"checkpoint_revision"`
	CheckpointDigest   string             `json:"checkpoint_digest"`
	RootDigest         string             `json:"root_digest"`
}
