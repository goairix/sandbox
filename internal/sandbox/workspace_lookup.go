package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
)

var (
	// ErrWorkspaceLookupConflict means ownership or availability cannot be proved.
	ErrWorkspaceLookupConflict = errors.New("workspace sandbox is not safely reusable")
	// ErrWorkspaceLookupAmbiguous means multiple sandboxes claim the workspace.
	ErrWorkspaceLookupAmbiguous = errors.New("multiple sandboxes claim the workspace")
	// ErrWorkspaceLookupUnavailable means an inspection dependency is unavailable.
	ErrWorkspaceLookupUnavailable = errors.New("workspace sandbox lookup is unavailable")
)

// WorkspaceLookupConflict identifies the failed proof without exposing lease
// capabilities. The API code remains WORKSPACE_SANDBOX_CONFLICT.
type WorkspaceLookupConflict struct {
	Reason    string
	SandboxID string
}

func (e *WorkspaceLookupConflict) Error() string {
	if e.SandboxID != "" {
		return fmt.Sprintf("%s: reason=%s sandbox_id=%s", ErrWorkspaceLookupConflict, e.Reason, e.SandboxID)
	}
	return fmt.Sprintf("%s: reason=%s", ErrWorkspaceLookupConflict, e.Reason)
}

func (e *WorkspaceLookupConflict) Unwrap() error { return ErrWorkspaceLookupConflict }

func lookupConflict(reason, id string) error {
	return &WorkspaceLookupConflict{Reason: reason, SandboxID: id}
}

// GetByWorkspace finds a usable sandbox without migrating sessions, acquiring
// workspace leases, changing configuration, or cleaning up existing owners.
// The returned snapshot does not reserve the sandbox; subsequent operations
// still use the normal lifecycle admission checks.
func (m *Manager) GetByWorkspace(ctx context.Context, root string) (Sandbox, error) {
	if _, err := storage.BuildWorkspacePrefix("", root); err != nil {
		return Sandbox{}, err
	}
	if m.fsMeta == nil {
		return Sandbox{}, ErrWorkspaceLookupUnavailable
	}
	prefix, err := storage.BuildWorkspacePrefix(m.fsMeta.SubPath, root)
	if err != nil {
		return Sandbox{}, lookupUnavailable(err)
	}
	coordinator := m.config.WorkspaceCoordinator
	var owner *WorkspaceOwner
	var keys workspaceKeys
	var ownerRaw, leaseRaw []byte
	if coordinator != nil && m.fsMeta.Provider != storage.ProviderLocal {
		if coordinator.configErr != nil || coordinator.store == nil {
			return Sandbox{}, ErrWorkspaceLookupUnavailable
		}
		keys, err = workspaceStateKeys(WorkspaceLeaseRequest{Provider: string(m.fsMeta.Provider),
			StorageIdentity: m.fsMeta.StorageIdentity, Bucket: m.fsMeta.Bucket, Prefix: prefix})
		if err != nil {
			return Sandbox{}, lookupUnavailable(err)
		}
		ownerRaw, err = coordinator.store.Get(ctx, keys.owner)
		if err != nil {
			return Sandbox{}, lookupUnavailable(err)
		}
		leaseRaw, err = coordinator.store.Get(ctx, keys.lease)
		if err != nil {
			return Sandbox{}, lookupUnavailable(err)
		}
		if ownerRaw != nil {
			var stored WorkspaceOwner
			if strictDecodeFlatJSONObject(ownerRaw, &stored) != nil {
				return Sandbox{}, lookupConflict("owner_invalid", "")
			}
			storedKeys, keyErr := workspaceStateKeysFromOwner(stored)
			if keyErr != nil || validateStoredOwner(stored) != nil || storedKeys != keys || stored.WorkspaceHash != keys.workspaceHash || stored.Runtime != m.workspaceRuntimeType() {
				return Sandbox{}, lookupConflict("owner_scope_mismatch", stored.SandboxID)
			}
			// Restore already supports an expired TTL lease for the exact durable
			// owner. Lookup must inspect that sandbox rather than reject recovery
			// before checking its identity and health. A present lease must match.
			if leaseRaw != nil {
				lease, leaseErr := parseActiveWorkspaceLeaseRecord(leaseRaw)
				if leaseErr != nil {
					return Sandbox{}, lookupConflict("lease_invalid", stored.SandboxID)
				}
				if !lookupLeaseMatchesOwner(lease, stored) {
					return Sandbox{}, lookupConflict("lease_owner_mismatch", stored.SandboxID)
				}
			}
			owner = &stored
		} else if leaseRaw != nil {
			// Acquisition may have published the provisional lease before owner.
			lease, _ := parseActiveWorkspaceLeaseRecord(leaseRaw)
			return Sandbox{}, lookupConflict("lease_without_owner", lease.SandboxID)
		}
	}

	var sb Sandbox
	if owner != nil {
		sb, err = m.lookupSandboxSnapshot(ctx, owner.SandboxID)
		if errors.Is(err, ErrSandboxNotFound) {
			return Sandbox{}, lookupConflict("sandbox_record_missing", owner.SandboxID)
		}
	} else {
		sb, err = m.lookupHistoricalWorkspace(ctx, root)
	}
	if err != nil {
		if errors.Is(err, ErrSandboxNotFound) {
			if verifyErr := m.verifyWorkspaceLookupState(ctx, keys, ownerRaw, leaseRaw); verifyErr != nil {
				return Sandbox{}, verifyErr
			}
		}
		return Sandbox{}, err
	}
	if err := m.validateWorkspaceLookup(ctx, &sb, root, owner); err != nil {
		return Sandbox{}, err
	}
	// Re-read lifecycle and ownership after runtime inspection. A transition
	// racing the lookup cannot turn a stale snapshot into a successful result.
	current, err := m.lookupSandboxSnapshot(ctx, sb.ID)
	if err != nil {
		if errors.Is(err, ErrSandboxNotFound) {
			return Sandbox{}, lookupConflict("sandbox_record_disappeared", sb.ID)
		}
		return Sandbox{}, err
	}
	if !reflect.DeepEqual(sb, current) {
		return Sandbox{}, lookupConflict("sandbox_changed_during_lookup", sb.ID)
	}
	if err := m.verifyWorkspaceLookupState(ctx, keys, ownerRaw, leaseRaw); err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}

func (m *Manager) verifyWorkspaceLookupState(ctx context.Context, keys workspaceKeys, ownerRaw, leaseRaw []byte) error {
	if keys.owner == "" {
		return nil
	}
	store := m.config.WorkspaceCoordinator.store
	currentOwner, err := store.Get(ctx, keys.owner)
	if err != nil {
		return lookupUnavailable(err)
	}
	currentLease, err := store.Get(ctx, keys.lease)
	if err != nil {
		return lookupUnavailable(err)
	}
	if !bytes.Equal(ownerRaw, currentOwner) || !bytes.Equal(leaseRaw, currentLease) {
		var owner WorkspaceOwner
		_ = json.Unmarshal(ownerRaw, &owner)
		return lookupConflict("ownership_changed_during_lookup", owner.SandboxID)
	}
	return nil
}

func lookupUnavailable(err error) error {
	return fmt.Errorf("%w: %w", ErrWorkspaceLookupUnavailable, err)
}

func lookupLeaseMatchesOwner(lease workspaceLeaseRecord, owner WorkspaceOwner) bool {
	return lease.Provider == owner.Provider && lease.StorageIdentityHash == owner.StorageIdentityHash &&
		lease.Bucket == owner.Bucket && lease.Prefix == owner.Prefix && lease.WorkspaceHash == owner.WorkspaceHash &&
		lease.SandboxID == owner.SandboxID && lease.Runtime == owner.Runtime && lease.RuntimeID == owner.RuntimeID &&
		(lease.RuntimeUID == "" || lease.RuntimeUID == owner.RuntimeUID) && lease.Generation == owner.Generation && lease.MountType == owner.MountType
}

func (m *Manager) lookupSandboxSnapshot(ctx context.Context, id string) (Sandbox, error) {
	if m.distributedStateEnabled() {
		record, err := m.activeSandboxes.Load(ctx, id)
		if err != nil {
			return Sandbox{}, lookupUnavailable(err)
		}
		sb, err := decodeActiveSandbox(record, id)
		if errors.Is(err, ErrSandboxNotFound) {
			return Sandbox{}, err
		}
		if err != nil {
			reason := "sandbox_record_invalid"
			if record != nil && record.Phase != "active" {
				reason = "sandbox_phase_" + string(record.Phase)
			}
			return Sandbox{}, lookupConflict(reason, id)
		}
		return cloneSandbox(sb), nil
	}
	m.mu.RLock()
	local := m.sandboxes[id]
	gate := m.operationGates[id]
	snapshot := cloneSandbox(local)
	m.mu.RUnlock()
	if local != nil {
		if gate != nil && !gate.isOpen() {
			return Sandbox{}, lookupConflict("sandbox_admission_closed", id)
		}
		return snapshot, nil
	}
	if m.sessions != nil {
		sb, err := m.sessions.LoadReadOnly(ctx, id)
		if errors.Is(err, ErrSandboxNotFound) {
			return Sandbox{}, err
		}
		if err != nil {
			return Sandbox{}, lookupUnavailable(err)
		}
		return cloneSandbox(sb), nil
	}
	return Sandbox{}, ErrSandboxNotFound
}

func (m *Manager) lookupHistoricalWorkspace(ctx context.Context, root string) (Sandbox, error) {
	candidates := make(map[string]Sandbox)
	if m.distributedStateEnabled() {
		var cursor uint64
		for {
			page, err := m.activeSandboxes.Scan(ctx, cursor, 100)
			if err != nil {
				return Sandbox{}, lookupUnavailable(err)
			}
			for _, record := range page.Records {
				var sb Sandbox
				if err := json.Unmarshal(record.Snapshot, &sb); err != nil {
					return Sandbox{}, lookupConflict("sandbox_record_invalid", record.SandboxID)
				}
				if sb.Workspace != nil && sb.Workspace.RootPath == root {
					checked, err := decodeActiveSandbox(&record, record.SandboxID)
					if err != nil {
						return Sandbox{}, lookupConflict("sandbox_record_not_active", record.SandboxID)
					}
					candidates[sb.ID] = cloneSandbox(checked)
				}
			}
			cursor = page.Cursor
			if cursor == 0 {
				break
			}
		}
	} else {
		ids := make(map[string]struct{})
		m.mu.RLock()
		for id, sb := range m.sandboxes {
			if sb.Workspace != nil && sb.Workspace.RootPath == root {
				ids[id] = struct{}{}
			}
		}
		m.mu.RUnlock()
		if m.sessions != nil {
			storedIDs, err := m.sessions.ListReadOnly(ctx)
			if err != nil {
				return Sandbox{}, lookupUnavailable(err)
			}
			for _, id := range storedIDs {
				stored, err := m.sessions.LoadReadOnly(ctx, id)
				if errors.Is(err, ErrSandboxNotFound) {
					continue
				}
				if err != nil {
					return Sandbox{}, lookupUnavailable(err)
				}
				if stored.Workspace != nil && stored.Workspace.RootPath == root {
					ids[id] = struct{}{}
				}
			}
		}
		for id := range ids {
			sb, err := m.lookupSandboxSnapshot(ctx, id)
			if errors.Is(err, ErrSandboxNotFound) {
				continue // Session expired between inventory and inspection.
			}
			if err != nil {
				return Sandbox{}, err
			}
			if sb.Workspace != nil && sb.Workspace.RootPath == root {
				candidates[id] = sb
			}
		}
	}
	if len(candidates) > 1 {
		return Sandbox{}, ErrWorkspaceLookupAmbiguous
	}
	for _, sb := range candidates {
		return sb, nil
	}
	return Sandbox{}, ErrSandboxNotFound
}

func (m *Manager) validateWorkspaceLookup(ctx context.Context, sb *Sandbox, root string, owner *WorkspaceOwner) error {
	if sb.ID == "" || sb.RuntimeID == "" || sb.Workspace == nil || sb.Workspace.RootPath != root || sb.CreatedAt.IsZero() {
		return lookupConflict("sandbox_identity_invalid", sb.ID)
	}
	if sb.WorkspaceTransition != "" {
		return lookupConflict("workspace_transition_in_progress", sb.ID)
	}
	if sb.Timeout > 0 && !sb.CreatedAt.Add(sb.Timeout).After(time.Now()) {
		return lookupConflict("sandbox_expired", sb.ID)
	}
	switch sb.State {
	case StateReady, StateRunning, StateIdle:
	default:
		return lookupConflict("sandbox_state_"+string(sb.State), sb.ID)
	}
	w := sb.Workspace
	if w.MountState != "" && w.MountState != WorkspaceMountReady {
		return lookupConflict("workspace_mount_not_ready", sb.ID)
	}
	if owner != nil {
		if w.Owner != *owner || w.LeaseGeneration != owner.Generation || sb.ID != owner.SandboxID || sb.RuntimeID != owner.RuntimeID ||
			sb.RuntimeUID == "" || sb.RuntimeUID != owner.RuntimeUID || w.MountType != owner.MountType {
			return lookupConflict("sandbox_owner_mismatch", sb.ID)
		}
		// Match Restore's owner preconditions even when the lease is absent.
		if (owner.MountType == WorkspaceMountFUSE && owner.MountAttempt != 1) ||
			(owner.MountType == WorkspaceMountSync && owner.MountAttempt != 0) {
			return lookupConflict("owner_mount_attempt_invalid", sb.ID)
		}
		if !m.distributedStateEnabled() {
			m.mu.RLock()
			local, gate := m.sandboxes[sb.ID], m.operationGates[sb.ID]
			m.mu.RUnlock()
			if local == nil || !gate.isOpen() {
				return lookupConflict("local_lifecycle_unavailable", sb.ID)
			}
		}
	} else if w.Owner != (WorkspaceOwner{}) || w.LeaseGeneration != 0 || w.MountType == WorkspaceMountFUSE {
		return lookupConflict("workspace_owner_missing", sb.ID)
	}
	info, err := m.runtime.GetSandbox(ctx, sb.RuntimeID)
	if errors.Is(err, runtime.ErrNotFound) {
		return lookupConflict("runtime_missing", sb.ID)
	}
	if err != nil {
		return lookupUnavailable(err)
	}
	if info == nil || info.State != "running" || info.RuntimeID != sb.RuntimeID {
		return lookupConflict("runtime_not_running", sb.ID)
	}
	uid := sb.RuntimeUID
	if uid == "" && owner == nil && m.workspaceRuntimeType() == "docker" && info.RuntimeUID == sb.RuntimeID {
		// Historical Docker sessions stored the full immutable container ID
		// before the separate RuntimeUID field existed. Never infer a pod UID.
		uid = sb.RuntimeID
	}
	if uid == "" || info.RuntimeUID != uid {
		return lookupConflict("runtime_uid_mismatch", sb.ID)
	}
	if owner == nil && (m.fsMeta.Provider != storage.ProviderLocal || info.WorkspaceHostPath != m.resolveLocalWorkspacePath(root)) {
		// Old sync snapshots do not bind a physical object-store namespace.
		// A local bind mount can instead prove its exact backing directory.
		return lookupConflict("workspace_backend_unconfirmed", sb.ID)
	}
	if w.MountType == WorkspaceMountFUSE {
		health, err := m.runtime.WorkspaceHealth(ctx, runtime.RuntimeRef{ID: sb.RuntimeID, UID: sb.RuntimeUID})
		if err != nil {
			return lookupUnavailable(err)
		}
		if health == nil || !health.Ready || health.MountType != "fuse" || health.RuntimeUID != sb.RuntimeUID ||
			health.Generation != w.LeaseGeneration || health.RestartCount != 0 || health.RestartDetected {
			return lookupConflict("fuse_health_mismatch", sb.ID)
		}
	}
	return nil
}
