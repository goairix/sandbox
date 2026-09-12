package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/goairix/sandbox/internal/storage/state"
)

const (
	activeSandboxRecordVersion = 1
	activeOperationTTL         = 30 * time.Second
	activeOperationEndTimeout  = 2 * time.Second
	activeControllerTTL        = 15 * time.Second
)

func (m *Manager) distributedStateEnabled() bool {
	return m.config.RuntimeType == "kubernetes" && m.activeSandboxes != nil
}

func marshalActiveSandbox(sb *Sandbox) ([]byte, error) {
	if sb == nil || sb.ID == "" || sb.RuntimeID == "" || sb.RuntimeUID == "" {
		return nil, state.ErrActiveSandboxCorrupt
	}
	snapshot := cloneSandbox(sb)
	return json.Marshal(&snapshot)
}

func decodeActiveSandbox(record *state.ActiveSandboxRecord, expectedID string) (*Sandbox, error) {
	return decodeActiveSandboxPhase(record, expectedID, state.ActiveSandboxActive)
}

func decodeActiveSandboxPhase(record *state.ActiveSandboxRecord, expectedID string, phases ...state.ActiveSandboxPhase) (*Sandbox, error) {
	if record == nil {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, expectedID)
	}
	phaseAllowed := false
	for _, phase := range phases {
		phaseAllowed = phaseAllowed || record.Phase == phase
	}
	if err := record.Validate(); err != nil || record.SandboxID != expectedID || !phaseAllowed {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotReady, expectedID)
	}
	var sb Sandbox
	if err := json.Unmarshal(record.Snapshot, &sb); err != nil || sb.ID != record.SandboxID ||
		sb.RuntimeID != record.RuntimeID || sb.RuntimeUID != record.RuntimeUID {
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotReady, expectedID)
	}
	sb.activeRevision = record.Revision
	sb.activeGeneration = record.Generation
	return &sb, nil
}

func (m *Manager) publishActiveSandbox(ctx context.Context, sb *Sandbox) error {
	if !m.distributedStateEnabled() {
		return nil
	}
	snapshot, err := marshalActiveSandbox(sb)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	record := state.ActiveSandboxRecord{
		Version: activeSandboxRecordVersion, SandboxID: sb.ID,
		Phase: state.ActiveSandboxPublishing, Revision: 1, Generation: 1,
		RuntimeID: sb.RuntimeID, RuntimeUID: sb.RuntimeUID, Snapshot: snapshot,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := m.activeSandboxes.Publish(ctx, record); err != nil {
		current, loadErr := m.activeSandboxes.Load(context.WithoutCancel(ctx), sb.ID)
		if loadErr != nil || current == nil || current.Phase != state.ActiveSandboxPublishing ||
			current.Generation != record.Generation || current.RuntimeID != record.RuntimeID ||
			current.RuntimeUID != record.RuntimeUID || !bytes.Equal(current.Snapshot, record.Snapshot) {
			return errors.Join(fmt.Errorf("publish active sandbox: %w", err), loadErr)
		}
		record = *current
	}
	active, activateErr := m.activeSandboxes.Activate(ctx, sb.ID, record.Revision, snapshot)
	if activateErr == nil && active != nil {
		return nil
	}
	current, loadErr := m.activeSandboxes.Load(context.WithoutCancel(ctx), sb.ID)
	if loadErr == nil && current != nil && current.Phase == state.ActiveSandboxActive &&
		current.Generation == record.Generation && current.RuntimeID == record.RuntimeID &&
		current.RuntimeUID == record.RuntimeUID && bytes.Equal(current.Snapshot, snapshot) {
		return nil
	}
	return errors.Join(fmt.Errorf("activate sandbox state: %w", activateErr), loadErr)
}

func (m *Manager) beginDistributedOperation(ctx context.Context, id string, kind state.ActiveOperationKind) (*Sandbox, func(), error) {
	token := uuid.NewString()
	record, operation, err := m.activeSandboxes.BeginOperation(ctx, id, token, kind, activeOperationTTL)
	if err != nil {
		if errors.Is(err, state.ErrActiveSandboxAdmissionClosed) {
			return nil, nil, fmt.Errorf("%w: %s", ErrSandboxNotReady, id)
		}
		return nil, nil, err
	}
	if record == nil || operation == nil {
		return nil, nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	sb, err := decodeActiveSandbox(record, id)
	if err == nil {
		var infoRuntimeUID string
		info, inspectErr := m.runtime.GetSandbox(ctx, sb.RuntimeID)
		if info != nil {
			infoRuntimeUID = info.RuntimeUID
		}
		if inspectErr != nil || info == nil || info.State != "running" || info.RuntimeID != sb.RuntimeID || infoRuntimeUID != sb.RuntimeUID {
			err = errors.Join(fmt.Errorf("%w: runtime identity changed for %s", ErrSandboxNotReady, id), inspectErr)
		}
	}
	if err != nil {
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activeOperationEndTimeout)
		_ = m.activeSandboxes.EndOperation(endCtx, *operation)
		cancel()
		return nil, nil, err
	}
	var released bool
	var releaseMu sync.Mutex
	release := func() {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		if released {
			return
		}
		released = true
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activeOperationEndTimeout)
		_ = m.activeSandboxes.EndOperation(endCtx, *operation)
		cancel()
	}
	return sb, release, nil
}

func (m *Manager) acquireSandboxMutation(ctx context.Context, id string) (*Sandbox, func(), error) {
	if m.distributedStateEnabled() {
		return m.beginDistributedOperation(ctx, id, state.ActiveOperationMutation)
	}
	return m.acquireSandboxOperation(ctx, id)
}

func (m *Manager) persistActiveSandboxUpdate(ctx context.Context, sb *Sandbox) error {
	if !m.distributedStateEnabled() {
		return nil
	}
	snapshot, err := marshalActiveSandbox(sb)
	if err != nil {
		return err
	}
	updated, updateErr := m.activeSandboxes.Update(ctx, sb.ID, sb.activeRevision, snapshot)
	if updateErr == nil && updated != nil {
		sb.activeRevision = updated.Revision
		return nil
	}
	current, loadErr := m.activeSandboxes.Load(context.WithoutCancel(ctx), sb.ID)
	if loadErr == nil && current != nil && current.Phase == state.ActiveSandboxActive &&
		current.Generation == sb.activeGeneration && bytes.Equal(current.Snapshot, snapshot) {
		sb.activeRevision = current.Revision
		return nil
	}
	return errors.Join(updateErr, loadErr, state.ErrActiveSandboxConflict)
}

func (m *Manager) destroyDistributedSandbox(ctx context.Context, id string) error {
	record, _, _, err := m.activeSandboxes.BeginDestroy(ctx, id)
	if err != nil {
		current, loadErr := m.activeSandboxes.Load(context.WithoutCancel(ctx), id)
		if loadErr != nil || current == nil || (current.Phase != state.ActiveSandboxDestroying && current.Phase != state.ActiveSandboxCleanupPending) {
			return errors.Join(err, loadErr)
		}
		record = current
	}
	if record == nil {
		return fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	sb, err := decodeActiveSandboxPhase(record, id, state.ActiveSandboxDestroying, state.ActiveSandboxCleanupPending)
	if err != nil {
		return err
	}
	lease := state.ActiveSandboxControllerLease{
		SandboxID: id, Token: uuid.NewString(), InstanceID: m.config.InstanceID,
		Generation: record.Generation, ExpiresAt: time.Now().Add(activeControllerTTL),
	}
	held, acquired, err := m.activeSandboxes.AcquireController(ctx, lease, activeControllerTTL)
	if err != nil {
		return err
	}
	if !acquired || held == nil {
		return fmt.Errorf("%w: cleanup already owned for %s", ErrSandboxCleanupPending, id)
	}
	defer func() { _ = m.activeSandboxes.ReleaseController(context.WithoutCancel(ctx), *held) }()

	deadline := time.NewTicker(25 * time.Millisecond)
	defer deadline.Stop()
	for {
		live, countErr := m.activeSandboxes.LiveOperations(ctx, id)
		if countErr != nil {
			return countErr
		}
		if live == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
		}
	}
	if sb.Workspace != nil && (sb.Workspace.MountType == WorkspaceMountFUSE || sb.Workspace.Owner.Generation > 0) {
		return fmt.Errorf("%w: workspace cleanup requires lifecycle coordinator for %s", ErrSandboxCleanupPending, id)
	}
	info, inspectErr := m.runtime.GetSandbox(ctx, sb.RuntimeID)
	if inspectErr != nil || info == nil || info.RuntimeID != sb.RuntimeID || info.RuntimeUID != sb.RuntimeUID {
		checkpoint, checkpointErr := m.activeSandboxes.Checkpoint(context.WithoutCancel(ctx), id, record.Revision, "runtime_identity_unconfirmed")
		_ = checkpoint
		return errors.Join(fmt.Errorf("%w: runtime identity unconfirmed", ErrSandboxCleanupPending), inspectErr, checkpointErr)
	}
	if err := m.runtime.RemoveSandbox(ctx, sb.RuntimeID); err != nil {
		_, checkpointErr := m.activeSandboxes.Checkpoint(context.WithoutCancel(ctx), id, record.Revision, "runtime_remove_pending")
		return errors.Join(fmt.Errorf("%w: remove runtime: %v", ErrSandboxCleanupPending, err), checkpointErr)
	}
	checkpoint, err := m.activeSandboxes.Checkpoint(context.WithoutCancel(ctx), id, record.Revision, "runtime_removed")
	if err != nil || checkpoint == nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	if m.sessions != nil && sb.Config.Mode == ModePersistent {
		if err := m.sessions.Remove(context.WithoutCancel(ctx), id); err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
	}
	if err := m.activeSandboxes.Delete(context.WithoutCancel(ctx), id, checkpoint.Revision, checkpoint.Generation); err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	m.mu.Lock()
	delete(m.sandboxes, id)
	delete(m.operationGates, id)
	delete(m.workspaces, id)
	m.mu.Unlock()
	m.pool.NotifyRemoved()
	return nil
}
