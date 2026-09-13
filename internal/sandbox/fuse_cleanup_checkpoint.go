package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
)

const fuseCleanupCheckpointPrefix = "fuse_cleanup_v1:"

// Written only after durable flush, and retained beyond deletion of the pool
// tombstone. LeaseValue is an opaque existing capability, never a new token.
type fuseCleanupCheckpoint struct {
	Consumed   state.FUSEPoolRecord  `json:"consumed"`
	Claimed    *state.FUSEPoolRecord `json:"claimed,omitempty"`
	LeaseValue []byte                `json:"lease_value"`
}

func sameFUSECleanupIdentity(record state.FUSEPoolRecord, sb *Sandbox) bool {
	w := sb.Workspace
	return record.PreparationID == w.FUSEPreparationID && record.RuntimeID == sb.RuntimeID && record.RuntimeUID == sb.RuntimeUID &&
		record.PoolKey == w.FUSEPoolKey && record.ReservationToken == w.FUSEReservationToken
}

func (m *Manager) checkpointFUSECleanup(ctx context.Context, lifecycle *fuseSandboxLifecycle) error {
	if !m.distributedStateEnabled() {
		return nil
	}
	if lifecycle.controller == nil || !lifecycle.flushAttempted {
		return ErrSandboxCleanupPending
	}
	if err := lifecycle.controller.Fence(ctx); err != nil {
		return err
	}
	record, err := m.activeSandboxes.Load(ctx, lifecycle.sandboxID)
	if err != nil {
		return err
	}
	if record == nil || record.RuntimeID != lifecycle.record.RuntimeID || record.RuntimeUID != lifecycle.record.RuntimeUID || record.Generation != lifecycle.sandbox.activeGeneration {
		return state.ErrActiveSandboxConflict
	}
	cp := fuseCleanupCheckpoint{Consumed: lifecycle.record, Claimed: lifecycle.claimed, LeaseValue: append([]byte(nil), lifecycle.lease.Value...)}
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	value := fuseCleanupCheckpointPrefix + string(raw)
	updated, err := lifecycle.controller.checkpoint(ctx, record.Revision, value)
	if err == nil && updated != nil {
		return nil
	}
	// A leader readback cannot prove replication durability after a lost ACK.
	// Retain the pending intent; the next bounded attempt rewrites the same
	// checkpoint under the live controller and must obtain a successful ACK.
	return errors.Join(ErrSandboxCleanupPending, err)
}

func (c *WorkspaceCoordinator) restoreFUSECleanupLease(ctx context.Context, owner WorkspaceOwner, value []byte, evidence runtime.TerminationEvidence) (*WorkspaceLease, bool, error) {
	keys, err := workspaceStateKeysFromOwner(owner)
	if err != nil {
		return nil, false, err
	}
	raw, err := c.store.Get(ctx, keys.owner)
	if err != nil {
		return nil, false, err
	}
	if raw != nil {
		lease, err := c.Restore(ctx, owner)
		return lease, false, err
	}
	if !safeToReleaseOwner(owner, evidence) {
		return nil, false, ErrRuntimeExitUnconfirmed
	}
	initialOwner := owner
	initialOwner.MountAttempt = 0 // opaque lease was allocated before single-use mount consumption
	capability, err := parseActiveWorkspaceLeaseRecord(value)
	if err != nil {
		return nil, false, err
	}
	initialOwner.RuntimeUID = capability.RuntimeUID // FUSE allocation precedes UID binding
	lease := &WorkspaceLease{Key: keys.lease, Value: append([]byte(nil), value...), Prefix: owner.Prefix, WorkspaceHash: owner.WorkspaceHash,
		Owner: initialOwner, ownerKey: keys.owner, generationKey: keys.generation, boundRuntimeUID: owner.RuntimeUID, authoritativeOwner: owner}
	if _, err := c.validateLeaseLocked(lease); err != nil {
		return nil, false, err
	}
	current, err := c.store.Get(ctx, keys.lease)
	if err != nil {
		return nil, false, err
	}
	if current != nil && !bytes.Equal(current, value) {
		return nil, false, ErrWorkspaceLeaseLost
	}
	return lease, current == nil, nil
}

func (m *Manager) restoreCheckpointedFUSECleanup(ctx context.Context, sb *Sandbox, controller *activeController, active *state.ActiveSandboxRecord) error {
	if sb.Workspace == nil || m.fusePool == nil || m.fusePool.repo == nil || m.fusePool.spec.WorkspaceFUSE == nil || m.fsMeta == nil || m.config.WorkspaceCoordinator == nil || m.config.WorkspaceObjectClient == nil {
		return ErrSandboxNotReady
	}
	if (sb.Config.Mode == ModePersistent && m.sessions == nil) || (sb.Config.Mode == ModeEphemeral && m.ephemeral == nil) {
		return ErrSandboxNotReady
	}
	if (sb.Config.Mode != ModePersistent && sb.Config.Mode != ModeEphemeral) || (sb.State != StateReady && sb.State != StateDestroying) {
		return ErrSandboxNotReady
	}
	w, spec := sb.Workspace, m.fusePool.spec.WorkspaceFUSE
	prefix, prefixErr := storage.BuildWorkspacePrefix(m.fsMeta.SubPath, w.RootPath)
	owner := w.Owner
	if prefixErr != nil || w.MountType != WorkspaceMountFUSE || owner.SandboxID != sb.ID || owner.RuntimeID != sb.RuntimeID || owner.RuntimeUID != sb.RuntimeUID ||
		owner.Generation != w.LeaseGeneration || owner.Generation <= 0 || owner.MountAttempt != 1 || owner.Provider != spec.Provider || owner.StorageIdentityHash != storageIdentityHash(spec.StorageIdentity) ||
		owner.Bucket != spec.Bucket || owner.Prefix != prefix || owner.Runtime != spec.RuntimeType {
		return ErrSandboxNotReady
	}
	if len(active.CleanupCheckpoint) > 64<<10 {
		return ErrSandboxNotReady
	}
	var cp fuseCleanupCheckpoint
	decoder := json.NewDecoder(strings.NewReader(strings.TrimPrefix(active.CleanupCheckpoint, fuseCleanupCheckpointPrefix)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cp) != nil || decoder.Decode(&struct{}{}) != io.EOF || cp.Consumed.State != state.FUSEPoolConsumed || !sameFUSECleanupIdentity(cp.Consumed, sb) || cp.Consumed.Revision != sb.Workspace.FUSERecordRevision || len(cp.LeaseValue) == 0 {
		return ErrSandboxNotReady
	}
	if cp.Claimed != nil && (!sameFUSECleanupIdentity(*cp.Claimed, sb) || cp.Claimed.State != state.FUSEPoolCleanup ||
		cp.Claimed.CleanupToken == "" || cp.Claimed.Revision <= cp.Consumed.Revision) {
		return ErrSandboxNotReady
	}
	records, err := m.fusePool.repo.ListByPoolKey(ctx, sb.Workspace.FUSEPoolKey)
	if err != nil {
		return err
	}
	var claimed *state.FUSEPoolRecord
	for _, record := range records {
		if record.PreparationID != sb.Workspace.FUSEPreparationID {
			continue
		}
		if !sameFUSECleanupIdentity(record, sb) {
			return ErrSandboxNotReady
		}
		if record.State == state.FUSEPoolConsumed && record.Revision == cp.Consumed.Revision {
			if cp.Claimed != nil {
				return ErrSandboxNotReady
			}
			break
		}
		if record.State != state.FUSEPoolCleanup || record.CleanupToken == "" || record.Revision <= cp.Consumed.Revision ||
			(cp.Claimed != nil && record.CleanupToken != cp.Claimed.CleanupToken) {
			return ErrSandboxNotReady
		}
		copy := record
		claimed = &copy
		break
	}
	poolRemoved := false
	if claimed == nil {
		foundConsumed := false
		for _, record := range records {
			foundConsumed = foundConsumed || (record.PreparationID == cp.Consumed.PreparationID && record.State == state.FUSEPoolConsumed)
		}
		if !foundConsumed {
			if cp.Claimed == nil || !sameFUSECleanupIdentity(*cp.Claimed, sb) || cp.Claimed.State != state.FUSEPoolCleanup || cp.Claimed.CleanupToken == "" {
				return ErrSandboxNotReady
			}
			claimed = cp.Claimed
			poolRemoved = true
		}
	}
	var evidence runtime.TerminationEvidence
	confirmed := false
	if claimed != nil {
		evidence, confirmed = poolTerminationEvidence(*claimed)
	}
	if poolRemoved && !confirmed {
		return ErrSandboxNotReady
	}
	info, err := m.runtime.GetSandbox(ctx, sb.RuntimeID)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return err
	}
	if info != nil && (info.RuntimeUID != sb.RuntimeUID || info.RuntimeID != sb.RuntimeID) {
		return ErrSandboxNotReady
	}
	if info == nil && !confirmed {
		return ErrSandboxNotReady
	}
	lease, released, err := m.config.WorkspaceCoordinator.restoreFUSECleanupLease(ctx, sb.Workspace.Owner, cp.LeaseValue, evidence)
	if err != nil {
		return err
	}
	var ephemeral *EphemeralLifecycleRecord
	if sb.Config.Mode == ModeEphemeral {
		ephemeral, err = m.ephemeral.Load(ctx, sb.ID)
		if err != nil && !errors.Is(err, ErrSandboxNotFound) {
			return err
		}
		if ephemeral != nil && (ephemeral.RuntimeID != sb.RuntimeID || ephemeral.RuntimeUID != sb.RuntimeUID || ephemeral.Owner != sb.Workspace.Owner) {
			return ErrEphemeralLifecycleConflict
		}
	}
	lifecycle := &fuseSandboxLifecycle{sandboxID: sb.ID, sandbox: sb, gate: newOperationGate(false), lease: lease, record: cp.Consumed,
		claimed: claimed, cancel: func() {}, ephemeralRecord: ephemeral, gateClosed: true, renewalStopped: true,
		quiesceAttempted: true, quiesced: true, flushAttempted: true, evidence: evidence, leaseReleased: released, poolRemoved: poolRemoved,
		runtimeRemoved: false}
	m.bindFUSEController(lifecycle, controller)
	if err := controller.Fence(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fuseLifecycles[sb.ID] != nil {
		return ErrSandboxCleanupPending
	}
	m.sandboxes[sb.ID] = sb
	m.operationGates[sb.ID] = lifecycle.gate
	m.fuseLifecycles[sb.ID] = lifecycle
	return nil
}
