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
	RuntimeID        string        `json:"runtime_id"`
	RuntimeUID       string        `json:"runtime_uid"`
	PoolKey          string        `json:"pool_key"`
	State            FUSEPoolState `json:"state"`
	MaintainerToken  string        `json:"maintainer_token"`
	ReservationToken string        `json:"reservation_token,omitempty"`
	ReservedUntil    time.Time     `json:"reserved_until,omitempty"`
	UpdatedAt        time.Time     `json:"updated_at"`
	Revision         uint64        `json:"revision"`
}

type FUSEPoolRepository interface {
	CreatePreparing(ctx context.Context, record FUSEPoolRecord) error
	ReservePrepared(ctx context.Context, poolKey, token string, ttl time.Duration) (*FUSEPoolRecord, error)
	Transition(ctx context.Context, runtimeUID string, from, to FUSEPoolState, token string, expectedRevision uint64) (*FUSEPoolRecord, error)
	ListByPoolKey(ctx context.Context, poolKey string) ([]FUSEPoolRecord, error)
	CountPreparingAndPrepared(ctx context.Context, poolKey string) (int, error)
	ConditionalDelete(ctx context.Context, runtimeUID string, expectedState FUSEPoolState, maintainerToken, reservationToken string, expectedRevision uint64) (bool, error)
	TryRefillLock(ctx context.Context, poolKey, token string, ttl time.Duration) (bool, error)
	UnlockRefill(ctx context.Context, poolKey, token string) error
}
