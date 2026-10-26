package state

import (
	"context"
	"errors"
	"time"
)

type FUSEPoolState string

type FUSEPoolCleanupPhase string

const (
	FUSEPoolPreparing FUSEPoolState = "preparing"
	FUSEPoolPrepared  FUSEPoolState = "prepared"
	FUSEPoolReserved  FUSEPoolState = "reserved"
	FUSEPoolBinding   FUSEPoolState = "binding"
	FUSEPoolConsumed  FUSEPoolState = "consumed"
	FUSEPoolCleanup   FUSEPoolState = "cleanup"

	FUSEPoolCleanupTerminating FUSEPoolCleanupPhase = "terminating"
	FUSEPoolCleanupTerminated  FUSEPoolCleanupPhase = "terminated"
)

var (
	ErrFUSEPoolNotFound          = errors.New("fuse pool record not found")
	ErrFUSEPoolConflict          = errors.New("fuse pool state conflict")
	ErrFUSEPoolCASMismatch       = errors.New("fuse pool revision mismatch")
	ErrFUSEPoolTokenMismatch     = errors.New("fuse pool token mismatch")
	ErrFUSEPoolInvalidTransition = errors.New("invalid fuse pool state transition")
	ErrFUSEPoolInvalidRecord     = errors.New("invalid fuse pool record")
	ErrFUSEPoolCorrupt           = errors.New("corrupt fuse pool state")
	ErrFUSEPoolUnboundedRead     = errors.New("unbounded fuse pool read is unsupported")
	ErrFUSEPoolNoServerTime      = errors.New("fuse pool server time is unavailable")
)

type FUSEPoolTerminationEvidence struct {
	RuntimeUID           string `json:"runtime_uid"`
	NodeName             string `json:"node_name,omitempty"`
	GracefulUnmount      bool   `json:"graceful_unmount,omitempty"`
	ProcessExited        bool   `json:"process_exited,omitempty"`
	InfrastructureFenced bool   `json:"infrastructure_fenced,omitempty"`
}

type FUSEPoolRecord struct {
	PreparationID       string                       `json:"preparation_id"`
	RuntimeID           string                       `json:"runtime_id"`
	RuntimeUID          string                       `json:"runtime_uid"`
	PoolKey             string                       `json:"pool_key"`
	State               FUSEPoolState                `json:"state"`
	MaintainerToken     string                       `json:"maintainer_token"`
	ReservationToken    string                       `json:"reservation_token,omitempty"`
	ReservedUntil       time.Time                    `json:"reserved_until,omitempty"`
	PrepareUntil        time.Time                    `json:"prepare_until,omitempty"`
	CleanupToken        string                       `json:"cleanup_token,omitempty"`
	CleanupUntil        time.Time                    `json:"cleanup_until,omitempty"`
	CleanupPhase        FUSEPoolCleanupPhase         `json:"cleanup_phase,omitempty"`
	TerminationEvidence *FUSEPoolTerminationEvidence `json:"termination_evidence,omitempty"`
	UpdatedAt           time.Time                    `json:"updated_at"`
	Revision            uint64                       `json:"revision"`
}

// FUSEPoolPublication names the consumed shell that must still own a runtime
// when a user-visible lifecycle reference is first written. Storage backends
// that support this contract compare the pool record in the same transaction
// as the publication, closing the cleanup-claim/publication race.
type FUSEPoolPublication struct {
	PoolKey          string
	PreparationID    string
	RuntimeID        string
	RuntimeUID       string
	ReservationToken string
}

type FUSEPoolBoundStore interface {
	SetFUSEBound(ctx context.Context, key string, value []byte, ttl time.Duration, binding FUSEPoolPublication) error
	SetNXFUSEBound(ctx context.Context, key string, value []byte, ttl time.Duration, binding FUSEPoolPublication) (bool, error)
}

// FUSEPoolBoundPublicationRequired marks a pool repository whose cleanup
// claim relies on atomic lifecycle publications. Managers must reject FUSE
// creation if a configured store or active repository hides that capability.
type FUSEPoolBoundPublicationRequired interface {
	RequiresFUSEPublicationFence()
}

type FUSEPoolRepository interface {
	CreatePreparingWithAdmission(ctx context.Context, record FUSEPoolRecord, refillToken string, maxSize int, prepareTTL time.Duration) error
	BindPreparingRuntime(ctx context.Context, preparationID, runtimeID, runtimeUID, refillToken string, expectedRevision uint64) (*FUSEPoolRecord, error)
	ReservePrepared(ctx context.Context, poolKey, token string, ttl time.Duration) (*FUSEPoolRecord, error)
	Transition(ctx context.Context, preparationID string, from, to FUSEPoolState, token string, expectedRevision uint64) (*FUSEPoolRecord, error)
	ReturnPreparedWithAdmission(ctx context.Context, preparationID, reservationToken string, expectedRevision uint64, maxSize int) (*FUSEPoolRecord, error)
	TransitionWithRefillLock(ctx context.Context, preparationID string, from, to FUSEPoolState, token, refillToken string, expectedRevision uint64, reservationTTL time.Duration) (*FUSEPoolRecord, error)
	ClaimCleanup(ctx context.Context, preparationID string, from FUSEPoolState, maintainerToken, reservationToken string, expectedRevision uint64, runtimeID, runtimeUID, cleanupToken string, ttl time.Duration) (*FUSEPoolRecord, error)
	ConfirmCleanupTermination(ctx context.Context, preparationID, cleanupToken string, expectedRevision uint64, runtimeID, runtimeUID string, evidence FUSEPoolTerminationEvidence) (*FUSEPoolRecord, error)
	ListPoolKeys(ctx context.Context) ([]string, error)
	ListByPoolKey(ctx context.Context, poolKey string) ([]FUSEPoolRecord, error)
	CountPreparingAndPrepared(ctx context.Context, poolKey string) (int, error)
	DeleteCleanup(ctx context.Context, preparationID, cleanupToken string, expectedRevision uint64) (bool, error)
	ServerTime(ctx context.Context) (time.Time, error)
	DrainRefillLocks(ctx context.Context) error
	TryRefillLock(ctx context.Context, poolKey, token string, ttl time.Duration) (bool, error)
	RenewRefillLock(ctx context.Context, poolKey, token string, ttl time.Duration) (bool, error)
	UnlockRefill(ctx context.Context, poolKey, token string) error
}

// FUSEPoolRecordReader is an optional exact lookup for request paths that
// already know a preparation ID. It avoids materializing every consumed
// sandbox sharing the same PoolKey. Nil means the record is absent.
type FUSEPoolRecordReader interface {
	ReadByPreparationID(ctx context.Context, preparationID string) (*FUSEPoolRecord, error)
}

// FUSEPoolStatePageReader is an optional bounded maintenance reader. The
// cursor is opaque to callers and names the last index entry returned. Each
// page is internally consistent, but mutations between pages require the
// next maintenance pass to revisit records that moved behind the cursor.
type FUSEPoolStatePageReader interface {
	ListStatePage(ctx context.Context, poolKey string, poolState FUSEPoolState, after string, limit int) ([]FUSEPoolRecord, string, error)
}

// FUSEPoolUnpublishedPageReader scans only consumed runtimes whose active
// lifecycle publication has not committed. Resident consumed records never
// enter this maintenance path.
type FUSEPoolUnpublishedPageReader interface {
	ListUnpublishedPage(ctx context.Context, poolKey, after string, limit int) ([]FUSEPoolRecord, string, error)
}

// FUSEPoolInventory is a fixed-size snapshot of one pool's state counts.
// Counts are observations for maintenance and metrics, never authorization
// for checkout or cleanup; those transitions still compare exact records.
type FUSEPoolInventory struct {
	Preparing int64
	Prepared  int64
	Reserved  int64
	Binding   int64
	Consumed  int64
	Cleanup   int64
}

// FUSEPoolInventoryReader avoids traversing resident sandboxes to report
// pool size or decide whether more prepared shells are needed.
type FUSEPoolInventoryReader interface {
	ReadInventory(ctx context.Context, poolKey string) (FUSEPoolInventory, error)
}

// FUSEPoolDeadlineReader checks the server-owned deadline key for an exact
// record snapshot. Local timestamps remain scheduling hints and never grant
// cleanup authority when this interface is available.
type FUSEPoolDeadlineReader interface {
	DeadlineLive(ctx context.Context, record FUSEPoolRecord) (bool, error)
}
