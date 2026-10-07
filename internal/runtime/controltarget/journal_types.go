// Package controltarget stores passive runtime diagnostic metadata. Journal
// history does not authorize execution or prove a physical command outcome.
package controltarget

import (
	"errors"
	"time"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

var (
	ErrInvalidConfiguration = errors.New("invalid journal configuration")
	ErrInvalidRecord        = errors.New("invalid journal record")
	ErrIdentityMismatch     = errors.New("journal identity mismatch")
	ErrConflict             = errors.New("journal record conflict")
	ErrExecutionExists      = errors.New("execution already exists")
	ErrStartConsumed        = errors.New("execution start already consumed")
	ErrBusy                 = errors.New("journal busy")
	ErrCapacity             = errors.New("journal capacity exceeded")
	ErrJournalUnavailable   = errors.New("journal unavailable")
	ErrClosed               = errors.New("journal closed")
)

type JournalIdentity struct {
	Namespace     string                           `json:"namespace"`
	AuthorityID   string                           `json:"authority_id"`
	Target        string                           `json:"target"`
	RestoreEpoch  string                           `json:"restore_epoch"`
	SandboxID     string                           `json:"sandbox_id"`
	WorkspaceHash string                           `json:"workspace_hash"`
	Generation    int64                            `json:"generation"`
	Runtime       controlprotocol.RuntimeReference `json:"runtime"`
}

type GateManifest struct {
	Version       uint32          `json:"version"`
	Identity      JournalIdentity `json:"identity"`
	DataGateEpoch int64           `json:"data_gate_epoch"`
	GateState     string          `json:"gate_state"`
}

// ExecJournalRecord records an intent without physical terminal proof.
// It carries digests and scalar context, never command payloads or tickets.
type ExecJournalRecord struct {
	Version           uint32                           `json:"version"`
	State             string                           `json:"state"`
	Context           controlprotocol.ExecStartContext `json:"context"`
	DescriptorDigest  string                           `json:"descriptor_digest"`
	TicketDigest      string                           `json:"ticket_digest"`
	NotBefore         time.Time                        `json:"not_before"`
	NotAfter          time.Time                        `json:"not_after"`
	AuthorityDeadline time.Time                        `json:"authority_deadline"`
	RootPID           int                              `json:"root_pid"`
	RootWaitStatus    uint32                           `json:"root_wait_status"`
	DrainConfirmed    bool                             `json:"drain_confirmed"`
	Reason            string                           `json:"reason"`
}

// Active configuration is all-or-none. Birth and its key are copied; Verifier
// and Clock are borrowed immutable, concurrency-safe dependencies for the whole
// Journal lifetime. Neither may be mutated or closed by the caller during use.
type JournalOptions struct {
	Birth         *controlprotocol.BirthContext
	Verifier      *controlprotocol.ManagementVerifier
	Clock         controlprotocol.AuthorityClock
	Directory     string
	Identity      JournalIdentity
	DataGateEpoch int64
	ManagementUID uint32
	MaxBytes      int64
}

// JournalStatus is a copied diagnostic snapshot, never authorization evidence.
type JournalStatus struct {
	Gate              GateManifest
	LogicalBytes      int64
	Records           uint64
	TemporaryFiles    uint64
	Warning           bool
	NewRecordsStopped bool
	Poisoned          bool
	AccountingKnown   bool
	Closed            bool
}
