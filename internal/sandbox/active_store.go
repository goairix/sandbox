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
	defaultActiveOperationTTL  = 30 * time.Second
	activeOperationEndTimeout  = 2 * time.Second
)

type activeOperationContextKey struct{}

func (m *Manager) distributedStateEnabled() bool {
	return m.config.RuntimeType == "kubernetes" && m.activeSandboxes != nil
}

func (m *Manager) activeOperationDurations() (time.Duration, time.Duration) {
	ttl := m.config.ActiveOperationTTL
	if ttl <= 0 {
		ttl = defaultActiveOperationTTL
	}
	interval := m.config.OperationRenewInterval
	if interval <= 0 || interval >= ttl {
		interval = ttl / 3
	}
	return ttl, interval
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
	_, err := m.publishActiveSandboxState(ctx, sb, false)
	return err
}

func (m *Manager) publishActiveSandboxState(ctx context.Context, sb *Sandbox, claimController bool) (*activeController, error) {
	if !m.distributedStateEnabled() {
		return nil, nil
	}
	snapshot, err := marshalActiveSandbox(sb)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	record := state.ActiveSandboxRecord{
		Version: activeSandboxRecordVersion, SandboxID: sb.ID,
		Phase: state.ActiveSandboxPublishing, Revision: 1, Generation: 1,
		RuntimeID: sb.RuntimeID, RuntimeUID: sb.RuntimeUID, Snapshot: snapshot,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := m.activeSandboxes.Publish(ctx, record); err != nil {
		if errors.Is(err, state.ErrDurabilityUnconfirmed) {
			return nil, err
		}
		current, loadErr := m.activeSandboxes.Load(context.WithoutCancel(ctx), sb.ID)
		if loadErr != nil || current == nil || current.Phase != state.ActiveSandboxPublishing ||
			current.Generation != record.Generation || current.RuntimeID != record.RuntimeID ||
			current.RuntimeUID != record.RuntimeUID || !bytes.Equal(current.Snapshot, record.Snapshot) {
			return nil, errors.Join(fmt.Errorf("publish active sandbox: %w", err), loadErr)
		}
		record = *current
	}
	sb.activeRevision = record.Revision
	sb.activeGeneration = record.Generation
	var controller *activeController
	if claimController {
		var acquired bool
		controller, acquired, err = m.acquireActiveController(ctx, sb)
		if err != nil || !acquired {
			return nil, errors.Join(ErrSandboxCleanupPending, err)
		}
	}
	active, activateErr := m.activeSandboxes.Activate(ctx, sb.ID, record.Revision, snapshot)
	if activateErr == nil && active != nil {
		sb.activeRevision = active.Revision
		sb.activeGeneration = active.Generation
		return controller, nil
	}
	if errors.Is(activateErr, state.ErrDurabilityUnconfirmed) {
		if controller != nil {
			_ = controller.Stop(context.WithoutCancel(ctx))
		}
		return nil, activateErr
	}
	current, loadErr := m.activeSandboxes.Load(context.WithoutCancel(ctx), sb.ID)
	if loadErr == nil && current != nil && current.Phase == state.ActiveSandboxActive &&
		current.Generation == record.Generation && current.RuntimeID == record.RuntimeID &&
		current.RuntimeUID == record.RuntimeUID && bytes.Equal(current.Snapshot, snapshot) {
		// A leader read is not a durability acknowledgement. Re-CAS only
		// this identical snapshot and require the repository write ACK.
		confirmed, confirmErr := m.activeSandboxes.Update(ctx, sb.ID, current.Revision, snapshot)
		if confirmErr == nil && confirmed != nil {
			sb.activeRevision = confirmed.Revision
			sb.activeGeneration = confirmed.Generation
			return controller, nil
		}
		activateErr = errors.Join(activateErr, confirmErr)
	}
	if controller != nil {
		_ = controller.Stop(context.WithoutCancel(ctx))
	}
	return nil, errors.Join(fmt.Errorf("activate sandbox state: %w", activateErr), loadErr)
}

func (m *Manager) beginDistributedOperation(ctx context.Context, id string, kind state.ActiveOperationKind) (*Sandbox, context.Context, func(), error) {
	token := uuid.NewString()
	ttl, renewInterval := m.activeOperationDurations()
	record, operation, err := m.activeSandboxes.BeginOperation(ctx, id, token, kind, ttl)
	if err != nil {
		if errors.Is(err, state.ErrActiveSandboxAdmissionClosed) {
			return nil, nil, nil, fmt.Errorf("%w: %s", ErrSandboxNotReady, id)
		}
		return nil, nil, nil, err
	}
	if record == nil || operation == nil {
		return nil, nil, nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, id)
	}
	var sb *Sandbox
	if kind == state.ActiveOperationExclusive {
		sb, err = decodeActiveSandboxPhase(record, id, state.ActiveSandboxExclusive)
	} else {
		sb, err = decodeActiveSandbox(record, id)
	}
	if err == nil && sb.WorkspaceTransition != "" {
		err = fmt.Errorf("%w: workspace transition pending for %s", ErrSandboxNotReady, id)
	}
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
		return nil, nil, nil, err
	}
	opCtx, cancelOperation := context.WithCancelCause(context.WithValue(ctx, activeOperationContextKey{}, *operation))
	renewCtx, stopRenewal := context.WithCancel(ctx)
	done := make(chan struct{})
	var operationMu sync.Mutex
	currentOperation := *operation
	go func() {
		defer close(done)
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				operationMu.Lock()
				current := currentOperation
				operationMu.Unlock()
				renewed, renewErr := m.activeSandboxes.RenewOperation(renewCtx, current, ttl)
				if renewErr != nil || renewed == nil {
					if renewErr == nil {
						renewErr = state.ErrActiveSandboxStaleToken
					}
					cancelOperation(errors.Join(ErrSandboxNotReady, renewErr))
					return
				}
				operationMu.Lock()
				currentOperation = *renewed
				operationMu.Unlock()
			}
		}
	}()
	var released bool
	var releaseMu sync.Mutex
	release := func() {
		releaseMu.Lock()
		defer releaseMu.Unlock()
		if released {
			return
		}
		released = true
		stopRenewal()
		<-done
		cancelOperation(nil)
		operationMu.Lock()
		current := currentOperation
		operationMu.Unlock()
		endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activeOperationEndTimeout)
		_ = m.activeSandboxes.EndOperation(endCtx, current)
		cancel()
	}
	return sb, opCtx, release, nil
}

func (m *Manager) acquireSandboxMutation(ctx context.Context, id string) (*Sandbox, context.Context, func(), error) {
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
	var updated *state.ActiveSandboxRecord
	var updateErr error
	if operation, ok := ctx.Value(activeOperationContextKey{}).(state.ActiveSandboxOperation); ok {
		repository, supported := m.activeSandboxes.(state.ActiveSandboxOperationRepository)
		if !supported {
			return state.ErrActiveSandboxCorrupt
		}
		updated, updateErr = repository.UpdateOperation(ctx, operation, sb.activeRevision, snapshot)
	} else {
		updated, updateErr = m.activeSandboxes.Update(ctx, sb.ID, sb.activeRevision, snapshot)
	}
	if updateErr == nil && updated != nil {
		sb.activeRevision = updated.Revision
		return nil
	}
	if errors.Is(updateErr, state.ErrDurabilityUnconfirmed) {
		return updateErr
	}
	current, loadErr := m.activeSandboxes.Load(context.WithoutCancel(ctx), sb.ID)
	if loadErr == nil && current != nil && (current.Phase == state.ActiveSandboxActive || current.Phase == state.ActiveSandboxExclusive) &&
		current.Generation == sb.activeGeneration && bytes.Equal(current.Snapshot, snapshot) {
		if operation, ok := ctx.Value(activeOperationContextKey{}).(state.ActiveSandboxOperation); ok {
			updated, updateErr = m.activeSandboxes.(state.ActiveSandboxOperationRepository).UpdateOperation(ctx, operation, current.Revision, snapshot)
		} else {
			updated, updateErr = m.activeSandboxes.Update(ctx, sb.ID, current.Revision, snapshot)
		}
		if updateErr == nil && updated != nil {
			sb.activeRevision = updated.Revision
			return nil
		}
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
	if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountSync &&
		(record.CleanupCheckpoint == "sync_final_output_done" || record.CleanupCheckpoint == "sync_runtime_removed") {
		// Final output was durably checkpointed before deletion. Cleanup must
		// not require a running runtime or replay a completed output sync.
		sb.WorkspaceTransition = workspaceUnmountSynced
		return m.cleanupInterruptedSyncWorkspace(ctx, sb)
	}
	if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountSync && sb.WorkspaceTransition != "" {
		return m.cleanupInterruptedSyncWorkspace(ctx, sb)
	}
	if sb.Workspace != nil && (sb.Workspace.MountType == WorkspaceMountFUSE || sb.Workspace.Owner.Generation > 0) {
		return m.destroyDistributedWorkspace(ctx, sb)
	}
	controller, acquired, err := m.acquireActiveController(ctx, sb)
	if err != nil {
		return err
	}
	if !acquired || controller == nil {
		return fmt.Errorf("%w: cleanup already owned for %s", ErrSandboxCleanupPending, id)
	}
	defer func() { _ = controller.Stop(context.WithoutCancel(ctx)) }()
	if err := controller.Fence(ctx); err != nil {
		return err
	}
	if removeErr := m.removeExactOrdinaryRuntime(ctx, sb); removeErr != nil {
		checkpoint, checkpointErr := controller.checkpoint(ctx, record.Revision, "runtime_identity_unconfirmed")
		_ = checkpoint
		return errors.Join(ErrSandboxCleanupPending, removeErr, checkpointErr)
	}
	checkpoint, err := controller.checkpoint(ctx, record.Revision, "runtime_removed")
	if err != nil || checkpoint == nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	if m.sessions != nil && sb.Config.Mode == ModePersistent {
		if err := m.sessions.RemoveMatchingRuntime(ctx, sb); err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
	}
	if err := controller.deleteRecord(ctx, checkpoint.Revision); err != nil {
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

func (m *Manager) destroyDistributedWorkspace(ctx context.Context, sb *Sandbox) error {
	m.mu.RLock()
	fuseLifecycle := m.fuseLifecycles[sb.ID]
	syncLifecycle := m.syncLifecycles[sb.ID]
	m.mu.RUnlock()
	if fuseLifecycle != nil {
		m.runFUSETeardown(ctx, fuseLifecycle, ErrSandboxCleanupPending)
		fuseLifecycle.teardownMu.Lock()
		done := fuseLifecycle.teardownDone
		fuseLifecycle.teardownMu.Unlock()
		if done {
			return nil
		}
		return ErrSandboxCleanupPending
	}
	if syncLifecycle != nil {
		return m.destroySyncSandbox(ctx, syncLifecycle)
	}

	controller, acquired, err := m.acquireActiveController(ctx, sb)
	if err != nil {
		return err
	}
	if !acquired || controller == nil {
		return m.waitForActiveCleanup(ctx, sb.ID)
	}
	keepController := false
	defer func() {
		if !keepController {
			_ = controller.Stop(context.WithoutCancel(ctx))
		}
	}()
	switch sb.Workspace.MountType {
	case WorkspaceMountFUSE:
		if err := m.restoreFUSECleanupWithController(ctx, sb, controller); err != nil {
			return err
		}
		keepController = true
		m.mu.RLock()
		lifecycle := m.fuseLifecycles[sb.ID]
		m.mu.RUnlock()
		m.runFUSETeardown(ctx, lifecycle, ErrSandboxCleanupPending)
		if lifecycle == nil {
			return ErrSandboxCleanupPending
		}
		lifecycle.teardownMu.Lock()
		done := lifecycle.teardownDone
		lifecycle.teardownMu.Unlock()
		if !done {
			return ErrSandboxCleanupPending
		}
		return nil
	case WorkspaceMountSync:
		if err := m.restoreSyncSandboxWithController(ctx, sb, controller); err != nil {
			return err
		}
		keepController = true
		m.mu.RLock()
		lifecycle := m.syncLifecycles[sb.ID]
		m.mu.RUnlock()
		return m.destroySyncSandbox(ctx, lifecycle)
	default:
		return ErrSandboxNotReady
	}
}

func (m *Manager) waitForActiveCleanup(ctx context.Context, sandboxID string) error {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		record, err := m.activeSandboxes.Load(ctx, sandboxID)
		if err != nil {
			return err
		}
		if record == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrSandboxCleanupPending, ctx.Err())
		case <-ticker.C:
		}
	}
}

func (m *Manager) beginActiveCleanup(ctx context.Context, sandboxID string) error {
	if !m.distributedStateEnabled() {
		return nil
	}
	record, _, _, err := m.activeSandboxes.BeginDestroy(ctx, sandboxID)
	if err != nil {
		current, loadErr := m.activeSandboxes.Load(context.WithoutCancel(ctx), sandboxID)
		if loadErr != nil || current == nil ||
			(current.Phase != state.ActiveSandboxDestroying && current.Phase != state.ActiveSandboxCleanupPending) {
			return errors.Join(err, loadErr)
		}
		record = current
	}
	if record == nil {
		return fmt.Errorf("%w: %s", ErrSandboxNotFound, sandboxID)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		live, countErr := m.activeSandboxes.LiveOperations(ctx, sandboxID)
		if countErr != nil {
			return countErr
		}
		if live == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *Manager) completeActiveSandboxCleanup(ctx context.Context, sandboxID string, controller *activeController) error {
	if !m.distributedStateEnabled() {
		return nil
	}
	for attempts := 0; attempts < 3; attempts++ {
		record, err := m.activeSandboxes.Load(ctx, sandboxID)
		if err != nil {
			return err
		}
		if record == nil {
			if repo, ok := m.activeSandboxes.(interface {
				ConfirmRecordAbsence(context.Context, string) error
			}); ok {
				return repo.ConfirmRecordAbsence(ctx, sandboxID)
			}
			return nil
		}
		// Preserve the last recovery capability if deletion fails or the
		// controller expires. A generic completion checkpoint would erase
		// post-runtime/pool cleanup proof before the record is actually gone.
		if err := controller.deleteRecord(ctx, record.Revision); err != nil {
			if errors.Is(err, state.ErrActiveSandboxConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return state.ErrActiveSandboxConflict
}

// drainDistributedActiveSandboxes is used only after every API Deployment Pod
// has been scaled to zero. It enumerates this release's exact state scope and
// drives each record through the same fenced cleanup path as online destroy.
func (m *Manager) drainDistributedActiveSandboxes(ctx context.Context) error {
	var records []state.ActiveSandboxRecord
	var cursor uint64
	for {
		page, err := m.activeSandboxes.Scan(ctx, cursor, activeLifecycleScanPageSize)
		if err != nil {
			return fmt.Errorf("scan active sandboxes for release drain: %w", err)
		}
		records = append(records, page.Records...)
		if page.Cursor == 0 {
			break
		}
		cursor = page.Cursor
	}
	var drainErr error
	for i := range records {
		record := &records[i]
		sb, err := decodeActiveSandboxPhase(record, record.SandboxID,
			state.ActiveSandboxPublishing, state.ActiveSandboxActive,
			state.ActiveSandboxDestroying, state.ActiveSandboxCleanupPending)
		if err != nil {
			drainErr = errors.Join(drainErr, err)
			continue
		}
		if sb.Workspace == nil || (sb.Workspace.MountType != WorkspaceMountFUSE && sb.Workspace.Owner.Generation <= 0) {
			if err := m.destroyDistributedSandbox(ctx, sb.ID); err != nil {
				drainErr = errors.Join(drainErr, err)
			}
			continue
		}
		controller, err := m.waitForActiveController(ctx, sb)
		if err != nil {
			drainErr = errors.Join(drainErr, err)
			continue
		}
		var cleanupErr error
		switch sb.Workspace.MountType {
		case WorkspaceMountFUSE:
			cleanupErr = m.restoreFUSESandboxWithController(ctx, sb, controller)
			if cleanupErr == nil {
				m.mu.RLock()
				lifecycle := m.fuseLifecycles[sb.ID]
				m.mu.RUnlock()
				m.runFUSETeardown(ctx, lifecycle, ErrSandboxCleanupPending)
				if lifecycle == nil {
					cleanupErr = ErrSandboxCleanupPending
				} else {
					lifecycle.teardownMu.Lock()
					if !lifecycle.teardownDone {
						cleanupErr = ErrSandboxCleanupPending
					}
					lifecycle.teardownMu.Unlock()
				}
			}
		case WorkspaceMountSync:
			cleanupErr = m.restoreSyncSandboxWithController(ctx, sb, controller)
			if cleanupErr == nil {
				m.mu.RLock()
				lifecycle := m.syncLifecycles[sb.ID]
				m.mu.RUnlock()
				cleanupErr = m.destroySyncSandbox(ctx, lifecycle)
			}
		default:
			cleanupErr = ErrSandboxNotReady
		}
		if cleanupErr != nil {
			_ = controller.Stop(context.WithoutCancel(ctx))
			drainErr = errors.Join(drainErr, fmt.Errorf("drain active sandbox %s: %w", sb.ID, cleanupErr))
		}
	}
	return drainErr
}

func (m *Manager) waitForActiveController(ctx context.Context, sb *Sandbox) (*activeController, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		controller, acquired, err := m.acquireActiveController(ctx, sb)
		if err != nil {
			return nil, err
		}
		if acquired {
			return controller, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
