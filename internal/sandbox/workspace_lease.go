package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
)

const (
	workspaceLeaseKeyPrefix        = "sandbox:workspace:lease:"
	workspaceOwnerKeyPrefix        = "sandbox:workspace:owner:"
	workspaceGenerationKeyPrefix   = "sandbox:workspace:generation:"
	workspaceLeaseTokenBytes       = 32
	workspaceCleanupTimeout        = 5 * time.Second
	workspaceLeaseRecordVersion    = 1
	workspaceLeasePhaseProvisional = "provisional"
	workspaceLeasePhaseActive      = "active"
)

var (
	// ErrInvalidWorkspaceLease indicates invalid coordinator configuration,
	// workspace identity input, or a malformed lease handle.
	ErrInvalidWorkspaceLease = errors.New("invalid workspace lease")
	// ErrWorkspaceLeased indicates another caller currently holds the TTL lease.
	ErrWorkspaceLeased = errors.New("workspace lease is already held")
	// ErrWorkspaceOwned indicates a persistent owner blocks automatic takeover.
	ErrWorkspaceOwned = errors.New("workspace has a persistent owner")
	// ErrWorkspaceLeaseLost indicates the exact lease token is no longer current.
	ErrWorkspaceLeaseLost = errors.New("workspace lease was lost")
	// ErrWorkspaceOwnerLost indicates persistent owner state is missing or changed.
	ErrWorkspaceOwnerLost = errors.New("workspace owner was lost")
	// ErrWorkspaceRuntimeBound indicates an owner is already bound to a runtime.
	ErrWorkspaceRuntimeBound = errors.New("workspace owner runtime is already bound")
	// ErrMountAuthorizationConsumed indicates the one mount attempt was used.
	ErrMountAuthorizationConsumed = errors.New("workspace mount authorization was consumed")
	// ErrRuntimeExitUnconfirmed indicates release lacks exact safe termination evidence.
	ErrRuntimeExitUnconfirmed = errors.New("runtime exit is not confirmed")
	// ErrWorkspaceGenerationExhausted indicates the persistent fencing counter
	// could not produce another positive generation.
	ErrWorkspaceGenerationExhausted = errors.New("workspace generation is exhausted")
)

// WorkspaceLeaseRequest identifies one canonical object-store workspace and
// the sandbox that wants to own it. Prefix must be the exact output of
// storage.BuildWorkspacePrefix. RuntimeUID may be empty when acquisition
// happens before runtime preparation; otherwise it becomes immutable owner state.
type WorkspaceLeaseRequest struct {
	Provider        string
	StorageIdentity string
	Bucket          string
	Prefix          string
	SandboxID       string
	Runtime         string
	RuntimeID       string
	RuntimeUID      string
}

// WorkspaceOwner is the persistent, non-expiring fencing record for one
// workspace. StorageIdentityHash deliberately avoids persisting the configured
// storage identity in Redis keys or owner values.
type WorkspaceOwner struct {
	Provider            string    `json:"provider"`
	StorageIdentityHash string    `json:"storage_identity_hash"`
	Bucket              string    `json:"bucket"`
	Prefix              string    `json:"prefix"`
	WorkspaceHash       string    `json:"workspace_hash"`
	SandboxID           string    `json:"sandbox_id"`
	Runtime             string    `json:"runtime"`
	RuntimeID           string    `json:"runtime_id"`
	RuntimeUID          string    `json:"runtime_uid"`
	Generation          int64     `json:"generation"`
	MountAttempt        uint8     `json:"mount_attempt"`
	UpdatedAt           time.Time `json:"updated_at"`
}

// WorkspaceLease is an opaque capability for mutating one workspace owner.
// Callers must not modify Value or synthesize lease handles.
type WorkspaceLease struct {
	Key           string
	Value         []byte `json:"-"`
	Prefix        string
	WorkspaceHash string
	Owner         WorkspaceOwner
	ExpiresAt     time.Time

	ownerKey      string
	generationKey string
	runtimeMu     sync.RWMutex
}

type workspaceLeaseRecord struct {
	Version             uint8     `json:"version"`
	Phase               string    `json:"phase"`
	Token               string    `json:"token"`
	Provider            string    `json:"provider"`
	StorageIdentityHash string    `json:"storage_identity_hash"`
	Bucket              string    `json:"bucket"`
	Prefix              string    `json:"prefix"`
	WorkspaceHash       string    `json:"workspace_hash"`
	SandboxID           string    `json:"sandbox_id"`
	Runtime             string    `json:"runtime"`
	RuntimeID           string    `json:"runtime_id"`
	RuntimeUID          string    `json:"runtime_uid"`
	CreatedAt           time.Time `json:"created_at"`
	Generation          int64     `json:"generation"`
}

type workspaceKeys struct {
	lease         string
	owner         string
	generation    string
	workspaceHash string
}

// WorkspaceCoordinator coordinates multi-replica lease and owner mutations
// exclusively through an AtomicStore.
type WorkspaceCoordinator struct {
	store         state.AtomicStore
	leaseTTL      time.Duration
	renewInterval time.Duration
	configErr     error
}

// NewWorkspaceCoordinator constructs a workspace lease coordinator. Invalid
// durations are reported fail-closed by operations so existing call sites do
// not need a separate constructor error path.
func NewWorkspaceCoordinator(store state.AtomicStore, leaseTTL, renewInterval time.Duration) *WorkspaceCoordinator {
	c := &WorkspaceCoordinator{store: store, leaseTTL: leaseTTL, renewInterval: renewInterval}
	if store == nil || leaseTTL <= 0 || renewInterval <= 0 || renewInterval > leaseTTL/3 {
		c.configErr = ErrInvalidWorkspaceLease
	}
	return c
}

// Acquire obtains the TTL lease and creates a persistent owner with a fresh,
// strictly increasing generation. A stale owner is never taken over, even if
// its TTL lease has already disappeared.
func (c *WorkspaceCoordinator) Acquire(ctx context.Context, req WorkspaceLeaseRequest) (*WorkspaceLease, error) {
	if err := c.validateRequest(req); err != nil {
		return nil, err
	}
	keys, err := workspaceStateKeys(req)
	if err != nil {
		return nil, err
	}
	token := make([]byte, workspaceLeaseTokenBytes)
	if _, err := rand.Read(token); err != nil {
		return nil, fmt.Errorf("create workspace lease token: %w", err)
	}
	defer clear(token)
	now := time.Now().UTC()
	provisionalRecord := workspaceLeaseRecord{
		Version:             workspaceLeaseRecordVersion,
		Phase:               workspaceLeasePhaseProvisional,
		Token:               base64.RawURLEncoding.EncodeToString(token),
		Provider:            req.Provider,
		StorageIdentityHash: storageIdentityHash(req.StorageIdentity),
		Bucket:              req.Bucket,
		Prefix:              req.Prefix,
		WorkspaceHash:       keys.workspaceHash,
		SandboxID:           req.SandboxID,
		Runtime:             req.Runtime,
		RuntimeID:           req.RuntimeID,
		RuntimeUID:          req.RuntimeUID,
		CreatedAt:           now,
	}
	provisionalRaw, err := json.Marshal(provisionalRecord)
	if err != nil {
		return nil, fmt.Errorf("marshal provisional workspace lease: %w", err)
	}

	acquired, err := c.store.SetNX(ctx, keys.lease, append([]byte(nil), provisionalRaw...), c.leaseTTL)
	if err != nil {
		primary := fmt.Errorf("acquire workspace lease: %w", err)
		return nil, c.compensateAcquire(ctx, keys, [][]byte{provisionalRaw}, nil, primary)
	}
	if !acquired {
		return nil, ErrWorkspaceLeased
	}
	ownerRaw, err := c.store.Get(ctx, keys.owner)
	if err != nil {
		return nil, c.compensateAcquire(ctx, keys, [][]byte{provisionalRaw}, nil, fmt.Errorf("check workspace owner: %w", err))
	}
	if ownerRaw != nil {
		return nil, c.compensateAcquire(ctx, keys, [][]byte{provisionalRaw}, nil, ErrWorkspaceOwned)
	}

	generation, err := c.store.Increment(ctx, keys.generation)
	if err != nil {
		return nil, c.compensateAcquire(ctx, keys, [][]byte{provisionalRaw}, nil, fmt.Errorf("increment workspace generation: %w", err))
	}
	if generation <= 0 {
		return nil, c.compensateAcquire(ctx, keys, [][]byte{provisionalRaw}, nil, ErrWorkspaceGenerationExhausted)
	}

	owner := WorkspaceOwner{
		Provider:            req.Provider,
		StorageIdentityHash: storageIdentityHash(req.StorageIdentity),
		Bucket:              req.Bucket,
		Prefix:              req.Prefix,
		WorkspaceHash:       keys.workspaceHash,
		SandboxID:           req.SandboxID,
		Runtime:             req.Runtime,
		RuntimeID:           req.RuntimeID,
		RuntimeUID:          req.RuntimeUID,
		Generation:          generation,
		UpdatedAt:           now,
	}
	ownerRaw, err = json.Marshal(owner)
	if err != nil {
		return nil, c.compensateAcquire(ctx, keys, [][]byte{provisionalRaw}, nil, fmt.Errorf("marshal workspace owner: %w", err))
	}
	activeRecord := provisionalRecord
	activeRecord.Phase = workspaceLeasePhaseActive
	activeRecord.Generation = generation
	activeRaw, err := json.Marshal(activeRecord)
	if err != nil {
		return nil, c.compensateAcquire(ctx, keys, [][]byte{provisionalRaw}, nil, fmt.Errorf("marshal active workspace lease: %w", err))
	}
	published, err := c.store.CompareAndSwap(ctx, keys.lease, provisionalRaw, activeRaw, c.leaseTTL)
	if err != nil {
		primary := errors.Join(ErrWorkspaceLeaseLost, fmt.Errorf("publish workspace lease: %w", err))
		return nil, c.compensateAcquire(ctx, keys, [][]byte{activeRaw, provisionalRaw}, nil, primary)
	}
	if !published {
		return nil, c.compensateAcquire(ctx, keys, [][]byte{activeRaw, provisionalRaw}, nil, ErrWorkspaceLeaseLost)
	}
	created, err := c.store.SetNX(ctx, keys.owner, append([]byte(nil), ownerRaw...), 0)
	if err != nil {
		return nil, c.compensateAcquire(ctx, keys, [][]byte{activeRaw, provisionalRaw}, ownerRaw, fmt.Errorf("create workspace owner: %w", err))
	}
	if !created {
		return nil, c.compensateAcquire(ctx, keys, [][]byte{activeRaw, provisionalRaw}, nil, ErrWorkspaceOwned)
	}

	lease := &WorkspaceLease{
		Key:           keys.lease,
		Value:         append([]byte(nil), activeRaw...),
		Prefix:        req.Prefix,
		WorkspaceHash: keys.workspaceHash,
		Owner:         owner,
		ExpiresAt:     now.Add(c.leaseTTL),
		ownerKey:      keys.owner,
		generationKey: keys.generation,
	}
	if err := c.Renew(ctx, lease); err != nil {
		return nil, c.compensateAcquire(ctx, keys, [][]byte{activeRaw, provisionalRaw}, ownerRaw, err)
	}
	lease.ExpiresAt = time.Now().UTC().Add(c.leaseTTL)
	return lease, nil
}

// BindRuntime atomically binds an initially empty owner RuntimeUID. It never
// overwrites a non-empty identity, including with the same value.
func (c *WorkspaceCoordinator) BindRuntime(ctx context.Context, lease *WorkspaceLease, runtimeUID string) error {
	if err := validateOpaqueText(runtimeUID, false); err != nil {
		return ErrInvalidWorkspaceLease
	}
	if lease == nil {
		return ErrInvalidWorkspaceLease
	}
	lease.runtimeMu.Lock()
	defer lease.runtimeMu.Unlock()
	record, err := c.validateLeaseLocked(lease)
	if err != nil {
		return err
	}
	if record.RuntimeUID != "" {
		return ErrWorkspaceRuntimeBound
	}
	if err := c.renewLocked(ctx, lease); err != nil {
		return err
	}
	owner, ownerRaw, err := c.loadMatchingOwner(ctx, lease)
	if err != nil {
		return err
	}
	if owner.RuntimeUID != "" {
		return ErrWorkspaceRuntimeBound
	}

	nextRecord := record
	nextRecord.RuntimeUID = runtimeUID
	nextLeaseRaw, err := json.Marshal(nextRecord)
	if err != nil {
		return fmt.Errorf("marshal bound workspace lease: %w", err)
	}
	leaseUpdated, err := c.store.CompareAndSwap(ctx, lease.Key, lease.Value, nextLeaseRaw, c.leaseTTL)
	if err != nil {
		leaseUpdated, err = c.resolveAmbiguousSwap(ctx, lease.Key, lease.Value, nextLeaseRaw, err)
	}
	if err != nil {
		return errors.Join(ErrWorkspaceLeaseLost, err)
	}
	if !leaseUpdated {
		return ErrWorkspaceLeaseLost
	}
	previousLeaseRaw := append([]byte(nil), lease.Value...)
	lease.Value = append([]byte(nil), nextLeaseRaw...)

	owner.RuntimeUID = runtimeUID
	owner.UpdatedAt = time.Now().UTC()
	nextOwnerRaw, err := json.Marshal(owner)
	if err != nil {
		return fmt.Errorf("marshal workspace owner: %w", err)
	}
	swapped, err := c.store.CompareAndSwap(ctx, lease.ownerKey, ownerRaw, nextOwnerRaw, 0)
	if err != nil {
		swapped, err = c.resolveAmbiguousSwap(ctx, lease.ownerKey, ownerRaw, nextOwnerRaw, err)
	}
	if err == nil && swapped {
		return nil
	}
	rollbackErr := c.rollbackLeaseBinding(ctx, lease, previousLeaseRaw)
	if err != nil {
		return errors.Join(fmt.Errorf("bind workspace runtime: %w", err), rollbackErr)
	}
	if rollbackErr != nil {
		return errors.Join(ErrWorkspaceRuntimeBound, rollbackErr)
	}
	return ErrWorkspaceRuntimeBound
}

func (c *WorkspaceCoordinator) resolveAmbiguousSwap(ctx context.Context, key string, oldValue, newValue []byte, original error) (bool, error) {
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceCleanupTimeout)
	defer cancel()
	current, err := c.store.Get(checkCtx, key)
	if err != nil {
		return false, errors.Join(original, fmt.Errorf("verify ambiguous compare-and-swap: %w", err))
	}
	if bytes.Equal(current, newValue) {
		return true, nil
	}
	if bytes.Equal(current, oldValue) {
		return false, original
	}
	return false, ErrWorkspaceLeaseLost
}

func (c *WorkspaceCoordinator) rollbackLeaseBinding(ctx context.Context, lease *WorkspaceLease, previous []byte) error {
	current := append([]byte(nil), lease.Value...)
	rolledBack, err := c.store.CompareAndSwap(ctx, lease.Key, current, previous, c.leaseTTL)
	if err != nil {
		rolledBack, err = c.resolveAmbiguousSwap(ctx, lease.Key, current, previous, err)
	}
	if err != nil {
		return fmt.Errorf("rollback workspace lease binding: %w", err)
	}
	if !rolledBack {
		return ErrWorkspaceLeaseLost
	}
	lease.Value = append([]byte(nil), previous...)
	return nil
}

// ConsumeMountAttempt atomically consumes the owner's only mount attempt and
// returns the complete authorization needed by the trusted runtime channel.
func (c *WorkspaceCoordinator) ConsumeMountAttempt(ctx context.Context, lease *WorkspaceLease, poolKey string) (runtime.WorkspaceMountAuthorization, error) {
	var zero runtime.WorkspaceMountAuthorization
	if err := validateOpaqueText(poolKey, false); err != nil {
		return zero, ErrInvalidWorkspaceLease
	}
	if lease == nil {
		return zero, ErrInvalidWorkspaceLease
	}
	lease.runtimeMu.RLock()
	defer lease.runtimeMu.RUnlock()
	if _, err := c.validateLeaseLocked(lease); err != nil {
		return zero, err
	}
	if err := c.renewLocked(ctx, lease); err != nil {
		return zero, err
	}

	owner, raw, err := c.loadMatchingOwner(ctx, lease)
	if err != nil {
		return zero, err
	}
	if owner.RuntimeUID == "" {
		return zero, ErrWorkspaceOwnerLost
	}
	if owner.MountAttempt != 0 {
		return zero, ErrMountAuthorizationConsumed
	}
	owner.MountAttempt = 1
	owner.UpdatedAt = time.Now().UTC()
	next, err := json.Marshal(owner)
	if err != nil {
		return zero, fmt.Errorf("marshal workspace owner: %w", err)
	}
	swapped, err := c.store.CompareAndSwap(ctx, lease.ownerKey, raw, next, 0)
	if err != nil {
		return zero, fmt.Errorf("consume workspace mount attempt: %w", err)
	}
	if !swapped {
		current, _, loadErr := c.loadMatchingOwner(ctx, lease)
		if loadErr == nil && current.MountAttempt != 0 {
			return zero, ErrMountAuthorizationConsumed
		}
		if loadErr != nil {
			return zero, loadErr
		}
		return zero, ErrWorkspaceOwnerLost
	}
	// If the lease disappeared during the owner CAS, leave mount_attempt=1 as
	// a non-replayable fence but do not return an authorization.
	if err := c.renewLocked(ctx, lease); err != nil {
		return zero, err
	}
	return runtime.WorkspaceMountAuthorization{
		RuntimeUID:      owner.RuntimeUID,
		PoolKey:         poolKey,
		WorkspaceHash:   owner.WorkspaceHash,
		Prefix:          owner.Prefix,
		LeaseGeneration: owner.Generation,
		MountAttempt:    owner.MountAttempt,
	}, nil
}

// Renew atomically verifies the exact lease token while extending its TTL.
func (c *WorkspaceCoordinator) Renew(ctx context.Context, lease *WorkspaceLease) error {
	if lease == nil {
		return ErrInvalidWorkspaceLease
	}
	lease.runtimeMu.RLock()
	defer lease.runtimeMu.RUnlock()
	if _, err := c.validateLeaseLocked(lease); err != nil {
		return err
	}
	return c.renewLocked(ctx, lease)
}

func (c *WorkspaceCoordinator) renewLocked(ctx context.Context, lease *WorkspaceLease) error {
	value := append([]byte(nil), lease.Value...)
	swapped, err := c.store.CompareAndSwap(ctx, lease.Key, value, append([]byte(nil), value...), c.leaseTTL)
	if err != nil {
		return errors.Join(ErrWorkspaceLeaseLost, fmt.Errorf("renew workspace lease: %w", err))
	}
	if !swapped {
		return ErrWorkspaceLeaseLost
	}
	return nil
}

// Release removes the exact lease first, then compare-deletes the exact owner
// bytes that were validated. A bound runtime requires evidence for that exact
// UID proving process exit or infrastructure fencing. The generation counter
// is intentionally retained forever.
func (c *WorkspaceCoordinator) Release(ctx context.Context, lease *WorkspaceLease, evidence runtime.TerminationEvidence) error {
	if lease == nil {
		return ErrInvalidWorkspaceLease
	}
	lease.runtimeMu.RLock()
	defer lease.runtimeMu.RUnlock()
	if _, err := c.validateLeaseLocked(lease); err != nil {
		return err
	}
	owner, raw, err := c.loadMatchingOwner(ctx, lease)
	if err != nil {
		return err
	}
	if !safeToReleaseOwner(owner, evidence) {
		return ErrRuntimeExitUnconfirmed
	}
	deleted, err := c.store.CompareAndDelete(ctx, lease.Key, append([]byte(nil), lease.Value...))
	if err != nil {
		return fmt.Errorf("release workspace lease: %w", err)
	}
	if !deleted {
		return ErrWorkspaceLeaseLost
	}
	deleted, err = c.store.CompareAndDelete(ctx, lease.ownerKey, raw)
	if err != nil {
		return fmt.Errorf("release workspace owner: %w", err)
	}
	if !deleted {
		return ErrWorkspaceOwnerLost
	}
	return nil
}

// WorkspaceLeaseRenewal owns the lifecycle of a background renewal loop.
type WorkspaceLeaseRenewal struct {
	cancel context.CancelFunc
	done   chan struct{}
	state  atomic.Uint32
}

const (
	renewalRunning uint32 = iota
	renewalStopped
	renewalLost
)

// Stop cancels renewal and waits for the loop when it won the race with lease
// loss. Stopping never calls onLost and is safe to repeat.
func (r *WorkspaceLeaseRenewal) Stop() {
	if r == nil {
		return
	}
	if r.state.CompareAndSwap(renewalRunning, renewalStopped) {
		r.cancel()
		<-r.done
	}
}

// StartRenewal immediately renews once before starting its ticker. Any lease
// failure wins a single atomic race with Stop and invokes onLost exactly once.
func (c *WorkspaceCoordinator) StartRenewal(ctx context.Context, lease *WorkspaceLease, onLost func(error)) (*WorkspaceLeaseRenewal, error) {
	if onLost == nil {
		onLost = func(error) {}
	}
	if err := c.Renew(ctx, lease); err != nil {
		onLost(err)
		return nil, err
	}
	loopCtx, cancel := context.WithCancel(ctx)
	renewal := &WorkspaceLeaseRenewal{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(renewal.done)
		ticker := time.NewTicker(c.renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-loopCtx.Done():
				renewal.state.CompareAndSwap(renewalRunning, renewalStopped)
				return
			case <-ticker.C:
				if err := c.Renew(loopCtx, lease); err != nil {
					if renewal.state.CompareAndSwap(renewalRunning, renewalLost) {
						onLost(err)
					}
					return
				}
			}
		}
	}()
	return renewal, nil
}

func (c *WorkspaceCoordinator) validateRequest(req WorkspaceLeaseRequest) error {
	if c == nil || c.configErr != nil {
		return ErrInvalidWorkspaceLease
	}
	if _, err := workspaceStateKeys(req); err != nil {
		return err
	}
	if err := validateOpaqueText(req.SandboxID, false); err != nil {
		return ErrInvalidWorkspaceLease
	}
	if err := validateOpaqueText(req.Runtime, true); err != nil {
		return ErrInvalidWorkspaceLease
	}
	if err := validateOpaqueText(req.RuntimeID, true); err != nil {
		return ErrInvalidWorkspaceLease
	}
	if err := validateOpaqueText(req.RuntimeUID, true); err != nil {
		return ErrInvalidWorkspaceLease
	}
	return nil
}

func (c *WorkspaceCoordinator) validateLeaseLocked(lease *WorkspaceLease) (workspaceLeaseRecord, error) {
	if c == nil || c.configErr != nil || lease == nil {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	owner := lease.Owner
	keys, err := workspaceStateKeysFromOwner(owner)
	if err != nil || owner.Generation <= 0 || owner.SandboxID == "" || owner.MountAttempt != 0 || owner.UpdatedAt.IsZero() {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	record, err := parseActiveWorkspaceLeaseRecord(lease.Value)
	if err != nil {
		return workspaceLeaseRecord{}, err
	}
	if lease.Key != keys.lease || lease.ownerKey != keys.owner || lease.generationKey != keys.generation ||
		lease.Prefix != owner.Prefix || lease.WorkspaceHash != keys.workspaceHash || owner.WorkspaceHash != keys.workspaceHash ||
		record.Provider != owner.Provider || record.StorageIdentityHash != owner.StorageIdentityHash ||
		record.Bucket != owner.Bucket || record.Prefix != owner.Prefix || record.WorkspaceHash != owner.WorkspaceHash ||
		record.SandboxID != owner.SandboxID || record.Runtime != owner.Runtime || record.RuntimeID != owner.RuntimeID ||
		record.Generation != owner.Generation || (owner.RuntimeUID != "" && record.RuntimeUID != owner.RuntimeUID) {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	return record, nil
}

func (c *WorkspaceCoordinator) loadMatchingOwner(ctx context.Context, lease *WorkspaceLease) (WorkspaceOwner, []byte, error) {
	raw, err := c.store.Get(ctx, lease.ownerKey)
	if err != nil {
		return WorkspaceOwner{}, nil, fmt.Errorf("get workspace owner: %w", err)
	}
	if raw == nil {
		return WorkspaceOwner{}, nil, ErrWorkspaceOwnerLost
	}
	var owner WorkspaceOwner
	if err := json.Unmarshal(raw, &owner); err != nil {
		return WorkspaceOwner{}, nil, ErrWorkspaceOwnerLost
	}
	if err := validateStoredOwner(owner); err != nil ||
		owner.Provider != lease.Owner.Provider ||
		owner.StorageIdentityHash != lease.Owner.StorageIdentityHash ||
		owner.Bucket != lease.Owner.Bucket ||
		owner.Prefix != lease.Prefix ||
		owner.WorkspaceHash != lease.WorkspaceHash ||
		owner.SandboxID != lease.Owner.SandboxID ||
		owner.Runtime != lease.Owner.Runtime ||
		owner.RuntimeID != lease.Owner.RuntimeID ||
		owner.Generation != lease.Owner.Generation ||
		owner.RuntimeUID != leaseRuntimeUID(lease) {
		return WorkspaceOwner{}, nil, ErrWorkspaceOwnerLost
	}
	return owner, append([]byte(nil), raw...), nil
}

func leaseRuntimeUID(lease *WorkspaceLease) string {
	record, err := parseActiveWorkspaceLeaseRecord(lease.Value)
	if err != nil {
		return ""
	}
	return record.RuntimeUID
}

func validateStoredOwner(owner WorkspaceOwner) error {
	if _, err := workspaceStateKeysFromOwner(owner); err != nil {
		return err
	}
	if err := validateOpaqueText(owner.SandboxID, false); err != nil {
		return err
	}
	if err := validateOpaqueText(owner.Runtime, true); err != nil {
		return err
	}
	if err := validateOpaqueText(owner.RuntimeID, true); err != nil {
		return err
	}
	if err := validateOpaqueText(owner.RuntimeUID, true); err != nil {
		return err
	}
	if owner.Generation <= 0 || owner.MountAttempt > 1 || owner.UpdatedAt.IsZero() {
		return ErrInvalidWorkspaceLease
	}
	if owner.RuntimeUID == "" && owner.MountAttempt != 0 {
		return ErrInvalidWorkspaceLease
	}
	return nil
}

func parseActiveWorkspaceLeaseRecord(raw []byte) (workspaceLeaseRecord, error) {
	var record workspaceLeaseRecord
	if len(raw) == 0 || json.Unmarshal(raw, &record) != nil {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	if record.Version != workspaceLeaseRecordVersion || record.Phase != workspaceLeasePhaseActive ||
		record.Generation <= 0 || record.CreatedAt.IsZero() {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	token, err := base64.RawURLEncoding.DecodeString(record.Token)
	if err == nil {
		defer clear(token)
	}
	if err != nil || len(token) != workspaceLeaseTokenBytes || base64.RawURLEncoding.EncodeToString(token) != record.Token {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	if err := validateOpaqueText(record.Provider, false); err != nil {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	if err := validateStorageIdentityHash(record.StorageIdentityHash); err != nil {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	if err := validateOpaqueText(record.Bucket, false); err != nil {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	if err := validateCanonicalWorkspacePrefix(record.Prefix); err != nil {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	if err := validateStorageIdentityHash(record.WorkspaceHash); err != nil {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	if err := validateOpaqueText(record.SandboxID, false); err != nil {
		return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
	}
	for _, value := range []string{record.Runtime, record.RuntimeID, record.RuntimeUID} {
		if err := validateOpaqueText(value, true); err != nil {
			return workspaceLeaseRecord{}, ErrInvalidWorkspaceLease
		}
	}
	return record, nil
}

func safeToReleaseOwner(owner WorkspaceOwner, evidence runtime.TerminationEvidence) bool {
	if owner.RuntimeUID == "" {
		return evidence == (runtime.TerminationEvidence{})
	}
	if evidence.RuntimeUID != owner.RuntimeUID {
		return false
	}
	if evidence.InfrastructureFenced {
		return true
	}
	if owner.Runtime == "docker" {
		return evidence.ProcessExited
	}
	return evidence.GracefulUnmount && evidence.ProcessExited
}

func (c *WorkspaceCoordinator) compensateAcquire(ctx context.Context, keys workspaceKeys, leaseValues [][]byte, ownerRaw []byte, primary error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workspaceCleanupTimeout)
	defer cancel()
	var cleanupErr error
	if ownerRaw != nil {
		if _, err := c.store.CompareAndDelete(cleanupCtx, keys.owner, append([]byte(nil), ownerRaw...)); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("compensate workspace owner: %w", err))
		}
	}
	for _, value := range leaseValues {
		if _, err := c.store.CompareAndDelete(cleanupCtx, keys.lease, append([]byte(nil), value...)); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("compensate workspace lease: %w", err))
		}
	}
	return errors.Join(primary, cleanupErr)
}

func workspaceStateKeys(req WorkspaceLeaseRequest) (workspaceKeys, error) {
	if err := validateOpaqueText(req.Provider, false); err != nil {
		return workspaceKeys{}, ErrInvalidWorkspaceLease
	}
	if err := validateOpaqueText(req.StorageIdentity, false); err != nil {
		return workspaceKeys{}, ErrInvalidWorkspaceLease
	}
	if err := validateOpaqueText(req.Bucket, false); err != nil {
		return workspaceKeys{}, ErrInvalidWorkspaceLease
	}
	if err := validateCanonicalWorkspacePrefix(req.Prefix); err != nil {
		return workspaceKeys{}, ErrInvalidWorkspaceLease
	}
	return makeWorkspaceStateKeys(req.Provider, storageIdentityHash(req.StorageIdentity), req.Bucket, req.Prefix)
}

func workspaceStateKeysFromOwner(owner WorkspaceOwner) (workspaceKeys, error) {
	if err := validateOpaqueText(owner.Provider, false); err != nil {
		return workspaceKeys{}, ErrInvalidWorkspaceLease
	}
	if err := validateStorageIdentityHash(owner.StorageIdentityHash); err != nil {
		return workspaceKeys{}, ErrInvalidWorkspaceLease
	}
	if err := validateOpaqueText(owner.Bucket, false); err != nil {
		return workspaceKeys{}, ErrInvalidWorkspaceLease
	}
	if err := validateCanonicalWorkspacePrefix(owner.Prefix); err != nil {
		return workspaceKeys{}, ErrInvalidWorkspaceLease
	}
	return makeWorkspaceStateKeys(owner.Provider, owner.StorageIdentityHash, owner.Bucket, owner.Prefix)
}

func makeWorkspaceStateKeys(provider, identityHash, bucket, prefix string) (workspaceKeys, error) {
	workspaceHash := hashWorkspaceIdentity(provider, identityHash, bucket, prefix)
	encode := base64.RawURLEncoding.EncodeToString
	suffix := encode([]byte(provider)) + ":" + identityHash + ":" + encode([]byte(bucket)) + ":" + encode([]byte(prefix))
	return workspaceKeys{
		lease:         workspaceLeaseKeyPrefix + suffix,
		owner:         workspaceOwnerKeyPrefix + suffix,
		generation:    workspaceGenerationKeyPrefix + suffix,
		workspaceHash: workspaceHash,
	}, nil
}

func storageIdentityHash(identity string) string {
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func hashWorkspaceIdentity(parts ...string) string {
	h := sha256.New()
	var length [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(part))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validateStorageIdentityHash(value string) error {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return ErrInvalidWorkspaceLease
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return ErrInvalidWorkspaceLease
	}
	return nil
}

func validateCanonicalWorkspacePrefix(prefix string) error {
	if !strings.HasSuffix(prefix, "/") || strings.HasSuffix(prefix, "//") {
		return ErrInvalidWorkspaceLease
	}
	path := strings.TrimSuffix(prefix, "/")
	rebuilt, err := storage.BuildWorkspacePrefix("", path)
	if err != nil || rebuilt != prefix {
		return ErrInvalidWorkspaceLease
	}
	return nil
}

func validateOpaqueText(value string, allowEmpty bool) error {
	if value == "" {
		if allowEmpty {
			return nil
		}
		return ErrInvalidWorkspaceLease
	}
	if !utf8.ValidString(value) {
		return ErrInvalidWorkspaceLease
	}
	for _, r := range value {
		if r == 0 || unicode.IsControl(r) {
			return ErrInvalidWorkspaceLease
		}
	}
	return nil
}

func clear(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
