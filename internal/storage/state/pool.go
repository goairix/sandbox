package state

import (
	"context"
	"errors"
	"time"
)

type FUSEPoolState string

const (
	FUSEPoolPreparing FUSEPoolState = "preparing"
	FUSEPoolPrepared  FUSEPoolState = "prepared"
	FUSEPoolReserved  FUSEPoolState = "reserved"
	FUSEPoolBinding   FUSEPoolState = "binding"
	FUSEPoolConsumed  FUSEPoolState = "consumed"
	FUSEPoolCleanup   FUSEPoolState = "cleanup"
)

var (
	ErrFUSEPoolNotFound          = errors.New("fuse pool record not found")
	ErrFUSEPoolConflict          = errors.New("fuse pool state conflict")
	ErrFUSEPoolCASMismatch       = errors.New("fuse pool revision mismatch")
	ErrFUSEPoolTokenMismatch     = errors.New("fuse pool token mismatch")
	ErrFUSEPoolInvalidTransition = errors.New("invalid fuse pool state transition")
	ErrFUSEPoolInvalidRecord     = errors.New("invalid fuse pool record")
	ErrFUSEPoolCorrupt           = errors.New("corrupt fuse pool state")
)

type FUSEPoolRecord struct {
	PreparationID    string        `json:"preparation_id"`
	RuntimeID        string        `json:"runtime_id"`
	RuntimeUID       string        `json:"runtime_uid"`
	PoolKey          string        `json:"pool_key"`
	State            FUSEPoolState `json:"state"`
	MaintainerToken  string        `json:"maintainer_token"`
	ReservationToken string        `json:"reservation_token,omitempty"`
	ReservedUntil    time.Time     `json:"reserved_until,omitempty"`
	PrepareUntil     time.Time     `json:"prepare_until,omitempty"`
	CleanupToken     string        `json:"cleanup_token,omitempty"`
	CleanupUntil     time.Time     `json:"cleanup_until,omitempty"`
	UpdatedAt        time.Time     `json:"updated_at"`
	Revision         uint64        `json:"revision"`
}

type FUSEPoolRepository interface {
	CreatePreparingWithAdmission(ctx context.Context, record FUSEPoolRecord, refillToken string, maxSize int, prepareTTL time.Duration) error
	BindPreparingRuntime(ctx context.Context, preparationID, runtimeID, runtimeUID, refillToken string, expectedRevision uint64) (*FUSEPoolRecord, error)
	ReservePrepared(ctx context.Context, poolKey, token string, ttl time.Duration) (*FUSEPoolRecord, error)
	Transition(ctx context.Context, preparationID string, from, to FUSEPoolState, token string, expectedRevision uint64) (*FUSEPoolRecord, error)
	ReturnPreparedWithAdmission(ctx context.Context, preparationID, reservationToken string, expectedRevision uint64, maxSize int) (*FUSEPoolRecord, error)
	TransitionWithRefillLock(ctx context.Context, preparationID string, from, to FUSEPoolState, token, refillToken string, expectedRevision uint64, reservationTTL time.Duration) (*FUSEPoolRecord, error)
	ClaimCleanup(ctx context.Context, preparationID string, from FUSEPoolState, maintainerToken, reservationToken string, expectedRevision uint64, runtimeID, runtimeUID, cleanupToken string, ttl time.Duration) (*FUSEPoolRecord, error)
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
