package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
)

// ErrWorkspaceRecoveryRequired means durable ownership cannot be safely
// recovered automatically. It is an operational failure, not caller contention.
var ErrWorkspaceRecoveryRequired = errors.New("workspace sandbox requires server-side recovery")

const workspaceRequestWait = 30 * time.Second
const workspaceRequestRetry = 50 * time.Millisecond

type workspaceRequestLock struct {
	ready chan struct{}
	users int
}

func (m *Manager) lockWorkspaceRequest(ctx context.Context, root string) (func(), error) {
	m.workspaceRequestsMu.Lock()
	if m.workspaceRequests == nil {
		m.workspaceRequests = make(map[string]*workspaceRequestLock)
	}
	lock := m.workspaceRequests[root]
	if lock == nil {
		lock = &workspaceRequestLock{ready: make(chan struct{}, 1)}
		m.workspaceRequests[root] = lock
	}
	lock.users++
	m.workspaceRequestsMu.Unlock()
	drop := func() {
		m.workspaceRequestsMu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(m.workspaceRequests, root)
		}
		m.workspaceRequestsMu.Unlock()
	}
	select {
	case lock.ready <- struct{}{}:
		return func() { <-lock.ready; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

// GetOrCreate returns the workspace's existing usable sandbox or creates it
// once. Creation settings apply only to a new sandbox. Existing configuration
// and TTL are retained. The boolean reports reuse for this request. Normal
// publication and lease contention are handled within the request; an ownership
// conflict never authorizes sandbox deletion.
func (m *Manager) GetOrCreate(ctx context.Context, cfg SandboxConfig) (*Sandbox, bool, error) {
	if cfg.WorkspacePath == "" {
		sb, err := m.Create(ctx, cfg)
		return sb, false, err
	}
	cfg = cloneSandboxConfig(cfg)
	if _, err := storage.BuildWorkspacePrefix("", cfg.WorkspacePath); err != nil {
		return nil, false, err
	}
	if _, err := m.resolveWorkspaceMountMode(cfg); err != nil {
		return nil, false, err
	}
	m.lifecycleMu.Lock()
	if m.stopping {
		m.lifecycleMu.Unlock()
		return nil, false, ErrSandboxNotReady
	}
	m.createWG.Add(1)
	m.lifecycleMu.Unlock()
	defer m.createWG.Done()
	requestCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.controlCtx, cancel)
	defer stop()
	defer cancel()
	lockWaitCtx, cancelLockWait := context.WithTimeout(requestCtx, workspaceRequestWait)
	defer cancelLockWait()
	unlock, err := m.lockWorkspaceRequest(lockWaitCtx, cfg.WorkspacePath)
	if err != nil {
		return nil, false, fmt.Errorf("%w: waiting for workspace request: %w", ErrWorkspaceLookupUnavailable, err)
	}
	defer unlock()
	var result *Sandbox
	var reused bool
	request := func(ctx context.Context) error {
		var err error
		result, reused, err = m.getOrCreateWorkspace(ctx, cfg)
		return err
	}
	coordinator := m.config.WorkspaceCoordinator
	if coordinator == nil || m.fsMeta == nil || m.fsMeta.Provider == storage.ProviderLocal {
		err = request(requestCtx)
	} else {
		prefix, prefixErr := storage.BuildWorkspacePrefix(m.fsMeta.SubPath, cfg.WorkspacePath)
		if prefixErr != nil {
			return nil, false, prefixErr
		}
		keys, keyErr := workspaceStateKeys(WorkspaceLeaseRequest{Provider: string(m.fsMeta.Provider), StorageIdentity: m.fsMeta.StorageIdentity, Bucket: m.fsMeta.Bucket, Prefix: prefix})
		if keyErr != nil {
			return nil, false, lookupUnavailable(keyErr)
		}
		if coordinator.configErr != nil || coordinator.store == nil {
			return nil, false, ErrWorkspaceLookupUnavailable
		}
		for {
			err = withPoolLock(requestCtx, coordinator.store, "sandbox:workspace:request:"+keys.workspaceHash, request)
			if !errors.Is(err, errOrdinaryPoolLockBusy) {
				break
			}
			if err = waitWorkspaceRequest(lockWaitCtx); err != nil {
				err = lookupUnavailable(fmt.Errorf("waiting for another workspace request: %w", err))
				break
			}
		}
	}
	if err != nil {
		fields := []logger.Field{logger.AddField("workspace_path", cfg.WorkspacePath), logger.ErrorField(err)}
		var conflict *WorkspaceLookupConflict
		if errors.As(err, &conflict) {
			fields = append(fields, logger.AddField("reason", conflict.Reason), logger.AddField("sandbox_id", conflict.SandboxID))
		}
		logger.Warn(ctx, "workspace sandbox request failed", fields...)
		return nil, false, err
	}
	return result, reused, nil
}

func waitWorkspaceRequest(ctx context.Context) error {
	timer := time.NewTimer(workspaceRequestRetry)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *Manager) getOrCreateWorkspace(ctx context.Context, cfg SandboxConfig) (*Sandbox, bool, error) {
	waitCtx, cancelWait := context.WithTimeout(ctx, workspaceRequestWait)
	defer cancelWait()
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, lookupUnavailable(err)
		}
		sb, err := m.GetByWorkspace(ctx, cfg.WorkspacePath)
		if err == nil {
			if err = m.prepareWorkspaceReuse(ctx, &sb); err == nil {
				return &sb, true, nil
			}
			if !errors.Is(err, ErrSandboxNotReady) && !errors.Is(err, ErrWorkspaceLeased) && !errors.Is(err, ErrWorkspaceLeaseLost) {
				return nil, false, lookupUnavailable(err)
			}
		} else if errors.Is(err, ErrSandboxNotFound) {
			created, createErr := m.Create(ctx, cfg)
			if createErr == nil {
				return created, false, nil
			}
			if !errors.Is(createErr, ErrWorkspaceLeased) && !errors.Is(createErr, ErrWorkspaceOwned) {
				return nil, false, createErr
			}
			err = createErr
		} else {
			var conflict *WorkspaceLookupConflict
			if !errors.As(err, &conflict) {
				if errors.Is(err, ErrWorkspaceLookupAmbiguous) {
					return nil, false, fmt.Errorf("%w: %w", ErrWorkspaceRecoveryRequired, err)
				}
				return nil, false, err
			}
			switch conflict.Reason {
			case "sandbox_expired", "sandbox_phase_destroying", "sandbox_phase_cleanup_pending", "sandbox_state_destroying":
				// Only the recorded expired/destroying lifecycle is retired through its
				// normal, UID-fenced cleanup. Healthy owners are never displaced.
				if cleanupErr := m.cleanupRequestedWorkspace(ctx, cfg.WorkspacePath, conflict.SandboxID); cleanupErr != nil {
					if errors.Is(cleanupErr, state.ErrActiveSandboxConflict) || errors.Is(cleanupErr, ErrSandboxNotReady) {
						if waitErr := waitWorkspaceRequest(waitCtx); waitErr != nil {
							return nil, false, lookupUnavailable(errors.Join(cleanupErr, waitErr))
						}
						continue
					}
					return nil, false, fmt.Errorf("%w: %w", ErrWorkspaceRecoveryRequired, cleanupErr)
				}
				continue
			case "sandbox_phase_publishing", "sandbox_phase_workspace_exclusive", "sandbox_changed_during_lookup", "ownership_changed_during_lookup", "lease_without_owner", "lease_provisional", "sandbox_admission_closed", "workspace_transition_in_progress":
				// Another request or workspace operation is still publishing its result.
			case "sandbox_record_missing":
				// A fresh owner precedes publication. An old owner without lifecycle
				// evidence needs server recovery rather than a speculative replacement.
				if !m.workspaceOwnerPublishing(ctx, cfg.WorkspacePath) {
					return nil, false, fmt.Errorf("%w: %w", ErrWorkspaceRecoveryRequired, err)
				}
			default:
				return nil, false, fmt.Errorf("%w: %w", ErrWorkspaceRecoveryRequired, err)
			}
		}
		if waitErr := waitWorkspaceRequest(waitCtx); waitErr != nil {
			return nil, false, lookupUnavailable(errors.Join(err, waitErr))
		}
	}
}

func (m *Manager) workspaceOwnerPublishing(ctx context.Context, root string) bool {
	if m.fsMeta == nil || m.config.WorkspaceCoordinator == nil {
		return false
	}
	prefix, err := storage.BuildWorkspacePrefix(m.fsMeta.SubPath, root)
	if err != nil {
		return false
	}
	keys, err := workspaceStateKeys(WorkspaceLeaseRequest{Provider: string(m.fsMeta.Provider), StorageIdentity: m.fsMeta.StorageIdentity, Bucket: m.fsMeta.Bucket, Prefix: prefix})
	if err != nil {
		return false
	}
	raw, err := m.config.WorkspaceCoordinator.store.Get(ctx, keys.owner)
	if err != nil {
		return false
	}
	var owner WorkspaceOwner
	if strictDecodeFlatJSONObject(raw, &owner) != nil {
		return false
	}
	age := time.Since(owner.UpdatedAt)
	return age >= 0 && age < activePublishingRecoveryGrace
}

func (m *Manager) prepareWorkspaceReuse(ctx context.Context, sb *Sandbox) error {
	if sb.Workspace == nil || sb.Workspace.Owner.Generation == 0 {
		return nil
	}
	coordinator := m.config.WorkspaceCoordinator
	keys, err := workspaceStateKeysFromOwner(sb.Workspace.Owner)
	if err != nil {
		return err
	}
	raw, err := coordinator.store.Get(ctx, keys.lease)
	if err != nil {
		return err
	}
	if raw != nil {
		lease, leaseErr := parseActiveWorkspaceLeaseRecord(raw)
		if leaseErr != nil || !lookupLeaseMatchesOwner(lease, sb.Workspace.Owner) {
			return ErrWorkspaceLeaseLost
		}
		return nil
	}
	if !m.distributedStateEnabled() {
		return ErrSandboxNotReady
	}
	record, err := m.activeSandboxes.Load(ctx, sb.ID)
	if err != nil {
		return err
	}
	if err = m.reconcileActiveLifecycle(ctx, record); err != nil {
		return err
	}
	raw, err = coordinator.store.Get(ctx, keys.lease)
	if err != nil {
		return err
	}
	if raw == nil {
		return ErrSandboxNotReady
	}
	lease, err := parseActiveWorkspaceLeaseRecord(raw)
	if err != nil || !lookupLeaseMatchesOwner(lease, sb.Workspace.Owner) {
		return ErrWorkspaceLeaseLost
	}
	return nil
}

// workspaceCleanupIdentity proves the requested workspace against the durable
// owner before allowing normal lifecycle cleanup. Expiry alone is not proof of
// ownership. Cleanup still applies the repository's generation and UID fences.
func (m *Manager) workspaceCleanupIdentity(ctx context.Context, root, id string) (bool, error) {
	var sb Sandbox
	pending := false
	if m.distributedStateEnabled() {
		record, err := m.activeSandboxes.Load(ctx, id)
		if err != nil {
			return false, lookupUnavailable(err)
		}
		if record == nil {
			return false, lookupConflict("sandbox_record_missing", id)
		}
		snapshot, err := decodeActiveSandboxPhase(record, id, state.ActiveSandboxActive, state.ActiveSandboxDestroying, state.ActiveSandboxCleanupPending)
		if err != nil {
			return false, lookupConflict("sandbox_record_invalid", id)
		}
		sb = cloneSandbox(snapshot)
		pending = record.Phase == state.ActiveSandboxDestroying || record.Phase == state.ActiveSandboxCleanupPending
	} else {
		m.mu.RLock()
		sb = cloneSandbox(m.sandboxes[id])
		m.mu.RUnlock()
		if sb.ID == "" {
			return false, lookupConflict("local_lifecycle_unavailable", id)
		}
	}
	if sb.ID != id || sb.RuntimeID == "" || sb.RuntimeUID == "" || sb.Workspace == nil || sb.Workspace.RootPath != root || m.fsMeta == nil {
		return false, lookupConflict("cleanup_workspace_identity_mismatch", id)
	}
	w := sb.Workspace
	var proofKeys workspaceKeys
	var proofOwner, proofLease []byte
	if m.config.WorkspaceCoordinator != nil && m.fsMeta.Provider != storage.ProviderLocal {
		prefix, err := storage.BuildWorkspacePrefix(m.fsMeta.SubPath, root)
		if err != nil {
			return false, err
		}
		keys, err := workspaceStateKeys(WorkspaceLeaseRequest{Provider: string(m.fsMeta.Provider), StorageIdentity: m.fsMeta.StorageIdentity, Bucket: m.fsMeta.Bucket, Prefix: prefix})
		if err != nil {
			return false, err
		}
		raw, err := m.config.WorkspaceCoordinator.store.Get(ctx, keys.owner)
		if err != nil {
			return false, lookupUnavailable(err)
		}
		var owner WorkspaceOwner
		if strictDecodeFlatJSONObject(raw, &owner) != nil || validateStoredOwner(owner) != nil {
			return false, lookupConflict("cleanup_owner_invalid", id)
		}
		storedKeys, keyErr := workspaceStateKeysFromOwner(owner)
		if keyErr != nil || storedKeys != keys || owner.WorkspaceHash != keys.workspaceHash || owner.Runtime != m.workspaceRuntimeType() ||
			owner != w.Owner || owner.SandboxID != sb.ID || owner.RuntimeID != sb.RuntimeID || owner.RuntimeUID != sb.RuntimeUID ||
			owner.MountType != w.MountType || owner.Generation != w.LeaseGeneration {
			return false, lookupConflict("cleanup_owner_mismatch", id)
		}
		proofKeys, proofOwner = keys, raw
		leaseRaw, err := m.config.WorkspaceCoordinator.store.Get(ctx, keys.lease)
		if err != nil {
			return false, lookupUnavailable(err)
		}
		proofLease = leaseRaw
		if leaseRaw != nil {
			lease, err := parseActiveWorkspaceLeaseRecord(leaseRaw)
			if err != nil || !lookupLeaseMatchesOwner(lease, owner) {
				return false, lookupConflict("cleanup_lease_owner_mismatch", id)
			}
		}
	} else if m.fsMeta.Provider != storage.ProviderLocal || w.Owner != (WorkspaceOwner{}) || w.LeaseGeneration != 0 || w.MountType == WorkspaceMountFUSE {
		return false, lookupConflict("cleanup_owner_unconfirmed", id)
	}
	info, err := m.runtime.GetSandbox(ctx, sb.RuntimeID)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return false, lookupUnavailable(err)
	}
	if err == nil && (info == nil || info.RuntimeID != sb.RuntimeID || info.RuntimeUID != sb.RuntimeUID) {
		return false, lookupConflict("cleanup_runtime_uid_mismatch", id)
	}
	if m.fsMeta.Provider == storage.ProviderLocal && info != nil && info.WorkspaceHostPath != m.resolveLocalWorkspacePath(root) {
		return false, lookupConflict("cleanup_workspace_backing_mismatch", id)
	}
	if proofKeys.owner != "" {
		currentOwner, err := m.config.WorkspaceCoordinator.store.Get(ctx, proofKeys.owner)
		if err != nil {
			return false, lookupUnavailable(err)
		}
		currentLease, err := m.config.WorkspaceCoordinator.store.Get(ctx, proofKeys.lease)
		if err != nil {
			return false, lookupUnavailable(err)
		}
		if !bytes.Equal(currentOwner, proofOwner) || !bytes.Equal(currentLease, proofLease) {
			return false, lookupConflict("cleanup_ownership_changed", id)
		}
	}
	pending = pending || sb.State == StateDestroying || (sb.Timeout > 0 && !sb.CreatedAt.Add(sb.Timeout).After(time.Now()))
	return pending, nil
}

func (m *Manager) cleanupRequestedWorkspace(ctx context.Context, root, id string) error {
	if m.distributedStateEnabled() {
		repo, ok := m.activeSandboxes.(state.ActiveSandboxConditionalDestroyRepository)
		if !ok {
			return lookupConflict("cleanup_fencing_unavailable", id)
		}
		expected, err := m.activeSandboxes.Load(ctx, id)
		if err != nil {
			return lookupUnavailable(err)
		}
		if expected == nil {
			return nil
		}
		pending, err := m.workspaceCleanupIdentity(ctx, root, id)
		if err != nil {
			return err
		}
		if !pending {
			return nil
		}
		record, err := repo.BeginDestroyExpected(ctx, *expected)
		if err != nil {
			return err
		}
		if record == nil {
			return nil
		}
		completed, cleanupErr := m.finishDistributedSandboxCleanup(ctx, record, true)
		if completed && cleanupErr == nil {
			metrics.SandboxActiveGauge.Add(ctx, -1)
			metrics.RecordSandboxDestroy(ctx, "timeout")
		}
		return cleanupErr
	}
	m.mu.RLock()
	gate := m.operationGates[id]
	m.mu.RUnlock()
	exclusive, err := gate.BeginExclusive(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = exclusive.Reopen() }()
	pending := false
	err = exclusive.withOwnership(func() error {
		var err error
		pending, err = m.workspaceCleanupIdentity(ctx, root, id)
		if err == nil && pending {
			exclusive.Close()
		}
		return err
	})
	if err != nil || !pending {
		return err
	}
	return m.Destroy(ctx, id)
}
