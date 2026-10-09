package etcd

// This profile is distinct from the legacy absence-based permanent slot budget.
type taskWorkspaceSyncRecyclingBudgetRecord struct {
	Version uint32 `json:"version"`
	TaskWorkspaceSyncBudgetOptions
}

type taskWorkspaceSyncRecyclingSlotRecord struct {
	Version        uint32 `json:"version"`
	BudgetID       string `json:"budget_id"`
	Partition      uint8  `json:"partition"`
	Slot           uint32 `json:"slot"`
	State          string `json:"state"`
	IntentID       string `json:"intent_id"`
	IntentDigest   string `json:"intent_digest"`
	IntentBirth    int64  `json:"intent_birth"`
	EvidenceDigest string `json:"evidence_digest"`
}

// Captured before the one original reserve request escapes. ACK birth is kept
// separately in the original intent draft; copied diagnostics grant no IO.
type taskWorkspaceSyncRecyclingGeneration struct {
	create, mod, version int64
	value                string
}
