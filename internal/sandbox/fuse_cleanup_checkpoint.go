package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

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
	if current == nil {
		// A missing leader read may follow a delete whose replication ACK was
		// lost. Do not skip normal Release unless both absences are durable.
		absent, err := confirmStateAbsence(ctx, c.store, keys.owner)
		if err != nil {
			return nil, false, err
		}
		if !absent {
			return nil, false, ErrWorkspaceOwnerLost
		}
		absent, err = confirmStateAbsence(ctx, c.store, keys.lease)
		if err != nil {
			return nil, false, err
		}
		if !absent {
			return nil, false, ErrWorkspaceLeaseLost
		}
		return lease, true, nil
	}
	return lease, false, nil
}

// A failed create can claim the pool shell just after active publication and
// before the active cleanup checkpoint is written. Recover only the exact
// claimed shell after independent terminal proof; then use the ordinary
// checkpointed teardown path for all remaining effects.
func (m *Manager) restoreClaimedFUSECleanup(ctx context.Context, sb *Sandbox, controller *activeController, active *state.ActiveSandboxRecord, claimed state.FUSEPoolRecord) error {
	w := sb.Workspace
	if w == nil || m.fusePool.spec.WorkspaceFUSE == nil || m.fsMeta == nil || m.config.WorkspaceCoordinator == nil ||
		active.CleanupCheckpoint != "" || (sb.State != StateReady && sb.State != StateDestroying) ||
		w.MountType != WorkspaceMountFUSE || w.MountState != WorkspaceMountReady ||
		!sameFUSECleanupIdentity(claimed, sb) || claimed.State != state.FUSEPoolCleanup || claimed.CleanupToken == "" ||
		w.FUSERecordRevision == 0 || claimed.Revision <= w.FUSERecordRevision || claimed.Revision-w.FUSERecordRevision > 2 {
		return ErrSandboxNotReady
	}
	prefix, err := storage.BuildWorkspacePrefix(m.fsMeta.SubPath, w.RootPath)
	spec, owner := m.fusePool.spec.WorkspaceFUSE, w.Owner
	if err != nil || owner.SandboxID != sb.ID || owner.RuntimeID != sb.RuntimeID || owner.RuntimeUID != sb.RuntimeUID ||
		owner.Generation != w.LeaseGeneration || owner.Generation <= 0 || owner.MountAttempt != 1 ||
		owner.Provider != spec.Provider || owner.StorageIdentityHash != storageIdentityHash(spec.StorageIdentity) ||
		owner.Bucket != spec.Bucket || owner.Prefix != prefix || owner.Runtime != spec.RuntimeType {
		return ErrSandboxNotReady
	}
	evidence, persisted := poolTerminationEvidence(claimed)
	if claimed.Revision == w.FUSERecordRevision+1 && (claimed.CleanupPhase != state.FUSEPoolCleanupTerminating || persisted) {
		return ErrSandboxNotReady
	}
	if claimed.Revision == w.FUSERecordRevision+2 && !persisted {
		return ErrSandboxNotReady
	}
	info, err := m.runtime.GetSandbox(ctx, sb.RuntimeID)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return err
	}
	if info != nil && (info.RuntimeID != sb.RuntimeID || info.RuntimeUID != sb.RuntimeUID || info.State == "running") {
		return ErrSandboxNotReady
	}
	if !persisted {
		cleaner, ok := m.runtime.(runtime.PreparedSandboxCleanup)
		if !ok {
			return ErrSandboxNotReady
		}
		evidence, err = cleaner.ConfirmPreparedSandboxTermination(ctx, sb.RuntimeID, sb.RuntimeUID)
		if err != nil {
			return err
		}
	}
	if evidence.RuntimeUID != sb.RuntimeUID || !evidence.GracefulUnmount || !evidence.ProcessExited {
		return ErrSandboxNotReady
	}
	if !persisted {
		claimedRecord, err := m.fusePool.repo.ConfirmCleanupTermination(ctx, claimed.PreparationID, claimed.CleanupToken,
			claimed.Revision, claimed.RuntimeID, claimed.RuntimeUID, stateTerminationEvidence(evidence))
		if err != nil || claimedRecord == nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
		claimed = *claimedRecord
	}
	lease, err := m.config.WorkspaceCoordinator.Restore(ctx, owner)
	if err != nil {
		return err
	}
	// ClaimCleanup preserves immutable pool fields and advances the revision
	// once; ConfirmCleanupTermination may advance it once more. This prior
	// record is only a checkpoint identity, never used to issue a new claim.
	consumed := claimed
	consumed.State = state.FUSEPoolConsumed
	consumed.Revision = w.FUSERecordRevision
	consumed.CleanupToken = ""
	consumed.CleanupUntil = time.Time{}
	consumed.CleanupPhase = ""
	consumed.TerminationEvidence = nil
	cp := fuseCleanupCheckpoint{Consumed: consumed, Claimed: &claimed, LeaseValue: append([]byte(nil), lease.Value...)}
	raw, err := json.Marshal(cp)
	if err != nil || len(raw)+len(fuseCleanupCheckpointPrefix) > 64<<10 {
		return ErrSandboxNotReady
	}
	updated, err := controller.checkpoint(ctx, active.Revision, fuseCleanupCheckpointPrefix+string(raw))
	if err != nil || updated == nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	return m.restoreCheckpointedFUSECleanup(ctx, sb, controller, updated)
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
	record, err := readFUSEPoolRecord(ctx, m.fusePool.repo, sb.Workspace.FUSEPoolKey, sb.Workspace.FUSEPreparationID)
	if err != nil {
		return err
	}
	var claimed *state.FUSEPoolRecord
	if record != nil {
		if !sameFUSECleanupIdentity(*record, sb) {
			return ErrSandboxNotReady
		}
		if record.State == state.FUSEPoolConsumed && record.Revision == cp.Consumed.Revision {
			if cp.Claimed != nil {
				return ErrSandboxNotReady
			}
		} else if record.State != state.FUSEPoolCleanup || record.CleanupToken == "" || record.Revision <= cp.Consumed.Revision ||
			(cp.Claimed != nil && record.CleanupToken != cp.Claimed.CleanupToken) {
			return ErrSandboxNotReady
		} else {
			claimed = record
		}
	}
	poolRemoved := false
	if record == nil {
		if cp.Claimed == nil || !sameFUSECleanupIdentity(*cp.Claimed, sb) || cp.Claimed.State != state.FUSEPoolCleanup || cp.Claimed.CleanupToken == "" {
			return ErrSandboxNotReady
		}
		claimed = cp.Claimed
		poolRemoved = true
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
