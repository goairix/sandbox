package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/goairix/sandbox/internal/runtime"
)

// WorkspaceOwnerAudit contains no lease capability or token.
type WorkspaceOwnerAudit struct {
	Owner       WorkspaceOwner `json:"owner"`
	Status      string         `json:"status"`
	Recoverable bool           `json:"recoverable"`
}

func (m *Manager) AuditWorkspaceOwners(ctx context.Context) ([]WorkspaceOwnerAudit, error) {
	if m.config.WorkspaceCoordinator == nil {
		return nil, ErrSandboxNotReady
	}
	c := m.config.WorkspaceCoordinator
	keys, err := c.store.Keys(ctx, workspaceOwnerKeyPrefix+"*")
	if err != nil {
		return nil, err
	}
	result := make([]WorkspaceOwnerAudit, 0, len(keys))
	for _, key := range keys {
		raw, err := c.store.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		var owner WorkspaceOwner
		if strictDecodeFlatJSONObject(raw, &owner) != nil || validateStoredOwner(owner) != nil {
			return nil, ErrWorkspaceOwnerLost
		}
		stateKeys, err := workspaceStateKeysFromOwner(owner)
		if err != nil || key != stateKeys.owner {
			return nil, ErrWorkspaceOwnerLost
		}
		status, err := m.workspaceOwnerRecoveryStatus(ctx, owner)
		if err != nil {
			return nil, err
		}
		result = append(result, WorkspaceOwnerAudit{Owner: owner, Status: status, Recoverable: status == "orphan_confirmed"})
	}
	return result, nil
}

func (m *Manager) RecoverWorkspaceOwner(ctx context.Context, expected WorkspaceOwner) error {
	if validateStoredOwner(expected) != nil || m.config.WorkspaceCoordinator == nil {
		return ErrWorkspaceOwnerLost
	}
	status, err := m.workspaceOwnerRecoveryStatus(ctx, expected)
	if err != nil || status != "orphan_confirmed" {
		return errors.Join(ErrSandboxCleanupPending, err, runtime.ErrTerminationUnconfirmed)
	}
	c := m.config.WorkspaceCoordinator
	keys, err := workspaceStateKeysFromOwner(expected)
	if err != nil {
		return err
	}
	raw, err := c.store.Get(ctx, keys.owner)
	if err != nil || raw == nil {
		return errors.Join(ErrWorkspaceOwnerLost, err)
	}
	var current WorkspaceOwner
	if json.Unmarshal(raw, &current) != nil || current != expected {
		return ErrWorkspaceOwnerLost
	}
	store, ok := c.store.(interface {
		CompareAndDeleteIfAbsent(context.Context, string, []byte, string) (bool, error)
	})
	if !ok {
		return ErrSandboxNotReady
	}
	deleted, err := store.CompareAndDeleteIfAbsent(ctx, keys.owner, raw, keys.lease)
	if err != nil || !deleted {
		return errors.Join(ErrWorkspaceOwnerLost, err)
	}
	return nil
}

func (m *Manager) workspaceOwnerRecoveryStatus(ctx context.Context, owner WorkspaceOwner) (string, error) {
	if owner.Runtime != m.workspaceRuntimeType() || owner.RuntimeUID == "" {
		return "runtime_unconfirmed", nil
	}
	keys, err := workspaceStateKeysFromOwner(owner)
	if err != nil {
		return "", err
	}
	lease, err := m.config.WorkspaceCoordinator.store.Get(ctx, keys.lease)
	if err != nil {
		return "", err
	}
	if lease != nil {
		return "lease_live", nil
	}
	if m.activeSandboxes != nil {
		record, err := m.activeSandboxes.Load(ctx, owner.SandboxID)
		if err != nil {
			return "", err
		}
		if record != nil {
			return "active_record_present", nil
		}
	}
	if m.sessions != nil {
		_, err := m.sessions.LoadReadOnly(ctx, owner.SandboxID)
		if err == nil {
			return "session_present", nil
		}
		if !errors.Is(err, ErrSandboxNotFound) {
			return "", err
		}
	}
	if m.ephemeral != nil {
		record, err := m.ephemeral.Load(ctx, owner.SandboxID)
		if err != nil && !errors.Is(err, ErrSandboxNotFound) {
			return "", err
		}
		if record != nil {
			return "ephemeral_record_present", nil
		}
	}
	// Owners predate namespace/release scope binding. A shared Redis scan is
	// not evidence that an owner belongs to this backend or Kubernetes scope.
	if m.fsMeta == nil || owner.Provider != string(m.fsMeta.Provider) || owner.Bucket != m.fsMeta.Bucket ||
		owner.StorageIdentityHash != storageIdentityHash(m.fsMeta.StorageIdentity) {
		return "backend_scope_unconfirmed", nil
	}
	if subPath := strings.Trim(m.fsMeta.SubPath, "/"); subPath != "" && !strings.HasPrefix(owner.Prefix, subPath+"/") {
		return "backend_scope_unconfirmed", nil
	}
	info, err := m.runtime.GetSandbox(ctx, owner.RuntimeID)
	if err == nil && info != nil {
		if info.RuntimeUID != owner.RuntimeUID {
			return "runtime_uid_replaced", nil
		}
		return "runtime_present", nil
	}
	if !errors.Is(err, runtime.ErrNotFound) {
		return "runtime_unconfirmed", err
	}
	if owner.Runtime == "kubernetes" {
		// NotFound only applies to this namespace, while the persisted owner
		// does not bind a namespace. Retain it rather than delete another
		// release's owner. Normal journal-backed lifecycle recovery is separate.
		return "runtime_scope_unconfirmed", nil
	}
	if owner.MountType == WorkspaceMountSync {
		return "sync_output_unconfirmed", nil
	}
	if owner.MountType == WorkspaceMountFUSE {
		fencer, ok := m.runtime.(runtime.RuntimeFencer)
		if !ok {
			return "termination_unconfirmed", nil
		}
		evidence, err := fencer.ConfirmTerminated(ctx, owner.RuntimeID, owner.RuntimeUID)
		if err != nil || !safeToReleaseOwner(owner, evidence) || !evidence.GracefulUnmount || !evidence.ProcessExited {
			return "termination_unconfirmed", nil
		}
	}
	return "orphan_confirmed", nil
}
