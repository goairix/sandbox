package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrActiveSandboxConflict        = errors.New("active sandbox state conflict")
	ErrActiveSandboxAdmissionClosed = errors.New("active sandbox admission closed")
	ErrActiveSandboxLeaseExpired    = errors.New("active sandbox lease expired")
	ErrActiveSandboxStaleToken      = errors.New("active sandbox stale token")
	ErrActiveSandboxCorrupt         = errors.New("active sandbox state is corrupt")
	ErrDurabilityUnconfirmed        = errors.New("state durability is unconfirmed")
)

type ActiveSandboxPhase string

const (
	ActiveSandboxPublishing     ActiveSandboxPhase = "publishing"
	ActiveSandboxActive         ActiveSandboxPhase = "active"
	ActiveSandboxDestroying     ActiveSandboxPhase = "destroying"
	ActiveSandboxCleanupPending ActiveSandboxPhase = "cleanup_pending"
)

type ActiveOperationKind string

const (
	ActiveOperationData     ActiveOperationKind = "data"
	ActiveOperationMutation ActiveOperationKind = "mutation"
)

// ActiveSandboxRecord is the authoritative Kubernetes lifecycle record. The
// snapshot is owned by the sandbox package; the state package treats it as an
// opaque, size-bounded JSON document.
type ActiveSandboxRecord struct {
	Version           uint32             `json:"version"`
	SandboxID         string             `json:"sandbox_id"`
	Phase             ActiveSandboxPhase `json:"phase"`
	Revision          uint64             `json:"revision"`
	Generation        int64              `json:"generation"`
	RuntimeID         string             `json:"runtime_id"`
	RuntimeUID        string             `json:"runtime_uid"`
	Snapshot          json.RawMessage    `json:"snapshot"`
	CleanupCheckpoint string             `json:"cleanup_checkpoint,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
}

func (r ActiveSandboxRecord) Validate() error {
	if r.Version == 0 || r.SandboxID == "" || r.Revision == 0 || r.Generation <= 0 ||
		r.RuntimeID == "" || r.RuntimeUID == "" || len(r.Snapshot) == 0 ||
		r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() {
		return ErrActiveSandboxCorrupt
	}
	switch r.Phase {
	case ActiveSandboxPublishing, ActiveSandboxActive, ActiveSandboxDestroying, ActiveSandboxCleanupPending:
		return nil
	default:
		return ErrActiveSandboxCorrupt
	}
}

type ActiveSandboxOperation struct {
	SandboxID  string              `json:"sandbox_id"`
	Token      string              `json:"token"`
	Generation int64               `json:"generation"`
	Kind       ActiveOperationKind `json:"kind"`
	ExpiresAt  time.Time           `json:"expires_at"`
}

func (o ActiveSandboxOperation) Validate(now time.Time) error {
	if o.SandboxID == "" || o.Token == "" || o.Generation <= 0 || o.ExpiresAt.IsZero() {
		return ErrActiveSandboxCorrupt
	}
	if o.Kind != ActiveOperationData && o.Kind != ActiveOperationMutation {
		return ErrActiveSandboxCorrupt
	}
	if !o.ExpiresAt.After(now) {
		return ErrActiveSandboxLeaseExpired
	}
	return nil
}

type ActiveSandboxControllerLease struct {
	SandboxID  string    `json:"sandbox_id"`
	Token      string    `json:"token"`
	InstanceID string    `json:"instance_id"`
	PodUID     string    `json:"pod_uid"`
	Generation int64     `json:"generation"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (l ActiveSandboxControllerLease) Validate(now time.Time) error {
	if l.SandboxID == "" || l.Token == "" || l.InstanceID == "" || l.Generation <= 0 || l.ExpiresAt.IsZero() {
		return ErrActiveSandboxCorrupt
	}
	if !l.ExpiresAt.After(now) {
		return ErrActiveSandboxLeaseExpired
	}
	return nil
}

type ActiveSandboxPage struct {
	Records []ActiveSandboxRecord
	Cursor  uint64
}

// ActiveSandboxRepository supplies the atomic coordination required by a
// stateless Kubernetes manager. Implementations must use store/server time for
// lease expiry and fence every token with the immutable sandbox generation.
type ActiveSandboxRepository interface {
	Publish(ctx context.Context, record ActiveSandboxRecord) error
	Load(ctx context.Context, sandboxID string) (*ActiveSandboxRecord, error)
	Activate(ctx context.Context, sandboxID string, expectedRevision uint64, snapshot json.RawMessage) (*ActiveSandboxRecord, error)
	Update(ctx context.Context, sandboxID string, expectedRevision uint64, snapshot json.RawMessage) (*ActiveSandboxRecord, error)
	BeginOperation(ctx context.Context, sandboxID, token string, kind ActiveOperationKind, ttl time.Duration) (*ActiveSandboxRecord, *ActiveSandboxOperation, error)
	RenewOperation(ctx context.Context, operation ActiveSandboxOperation, ttl time.Duration) (*ActiveSandboxOperation, error)
	EndOperation(ctx context.Context, operation ActiveSandboxOperation) error
	BeginDestroy(ctx context.Context, sandboxID string) (*ActiveSandboxRecord, int64, bool, error)
	LiveOperations(ctx context.Context, sandboxID string) (int64, error)
	Checkpoint(ctx context.Context, sandboxID string, expectedRevision uint64, checkpoint string) (*ActiveSandboxRecord, error)
	Delete(ctx context.Context, sandboxID string, expectedRevision uint64, generation int64) error
	AcquireController(ctx context.Context, lease ActiveSandboxControllerLease, ttl time.Duration) (*ActiveSandboxControllerLease, bool, error)
	RenewController(ctx context.Context, lease ActiveSandboxControllerLease, ttl time.Duration) (*ActiveSandboxControllerLease, error)
	ReleaseController(ctx context.Context, lease ActiveSandboxControllerLease) error
	Scan(ctx context.Context, cursor uint64, count int64) (ActiveSandboxPage, error)
	Ping(ctx context.Context) error
}

func ValidateActiveSandboxTTL(ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("%w: lease TTL must be positive", ErrActiveSandboxCorrupt)
	}
	return nil
}
