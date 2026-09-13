package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
)

// ScheduleDestroy durably closes admission before returning. The existing
// bounded lifecycle coordinator owns cleanup; another replica can recover it
// after shutdown, so no request-scoped fire-and-forget goroutine is needed.
func (m *Manager) ScheduleDestroy(ctx context.Context, id string) error {
	if !m.distributedStateEnabled() {
		return m.Destroy(ctx, id)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	record, _, _, err := m.activeSandboxes.BeginDestroy(ctx, id)
	if err != nil {
		return err
	}
	if record == nil {
		return nil
	}
	if record.Phase != state.ActiveSandboxDestroying && record.Phase != state.ActiveSandboxCleanupPending {
		return state.ErrActiveSandboxConflict
	}
	select {
	case m.cleanupWake <- struct{}{}:
	default:
	}
	return nil
}

func (m *Manager) removeExactOrdinaryRuntime(ctx context.Context, sb *Sandbox) error {
	info, err := m.runtime.GetSandbox(ctx, sb.RuntimeID)
	if errors.Is(err, runtime.ErrNotFound) {
		if cleaner, ok := m.runtime.(runtime.OrdinarySandboxPolicyCleaner); ok {
			return cleaner.CleanupOrdinarySandboxPolicies(ctx, runtime.RuntimeRef{ID: sb.RuntimeID, UID: sb.RuntimeUID}, sb.ID)
		}
		return nil
	}
	if err != nil {
		return err
	}
	if info == nil || info.RuntimeID != sb.RuntimeID || info.RuntimeUID != sb.RuntimeUID {
		return fmt.Errorf("%w: ordinary runtime identity differs", ErrSandboxCleanupPending)
	}
	if remover, ok := m.runtime.(runtime.OrdinarySandboxRemover); ok {
		err = remover.RemoveOrdinarySandbox(ctx, runtime.RuntimeRef{ID: sb.RuntimeID, UID: sb.RuntimeUID})
	} else {
		err = m.runtime.RemoveSandbox(ctx, sb.RuntimeID)
	}
	if errors.Is(err, runtime.ErrNotFound) {
		// A same-name replacement is not proof that cleanup of our policies
		// succeeded. Retain durable state in that case.
		_, inspectErr := m.runtime.GetSandbox(ctx, sb.RuntimeID)
		if errors.Is(inspectErr, runtime.ErrNotFound) {
			if cleaner, ok := m.runtime.(runtime.OrdinarySandboxPolicyCleaner); ok {
				return cleaner.CleanupOrdinarySandboxPolicies(ctx, runtime.RuntimeRef{ID: sb.RuntimeID, UID: sb.RuntimeUID}, sb.ID)
			}
			return nil
		}
	}
	return err
}

// checkpointSyncCleanup is written before runtime deletion. It lets a peer
// distinguish durable final output from a missing container with lost output.
func (m *Manager) checkpointSyncCleanup(ctx context.Context, sb *Sandbox, controller *activeController, checkpoint string) error {
	if !m.distributedStateEnabled() {
		return nil
	}
	if controller == nil {
		return ErrSandboxCleanupPending
	}
	if err := controller.Fence(ctx); err != nil {
		return err
	}
	record, err := m.activeSandboxes.Load(ctx, sb.ID)
	if err != nil {
		return err
	}
	if record == nil || record.Generation != sb.activeGeneration || record.RuntimeID != sb.RuntimeID || record.RuntimeUID != sb.RuntimeUID {
		return state.ErrActiveSandboxConflict
	}
	_, err = controller.checkpoint(ctx, record.Revision, checkpoint)
	return err
}

func (m *Manager) cleanupInterruptedSyncWorkspace(ctx context.Context, sb *Sandbox) error {
	m.mu.RLock()
	lifecycle := m.syncLifecycles[sb.ID]
	m.mu.RUnlock()
	if lifecycle != nil && lifecycle.controller != nil {
		// Relinquish the old local worker before recovery acquires a fresh
		// capability. Another replica cannot do this without the exact token.
		m.retireLocalSyncController(sb)
		if err := lifecycle.controller.Stop(ctx); err != nil {
			return err
		}
	}
	controller, acquired, err := m.acquireActiveController(ctx, sb)
	if err != nil || !acquired {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	defer func() { _ = controller.Stop(context.WithoutCancel(ctx)) }()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	controller.SetOnLost(cancel)
	if err := controller.Fence(ctx); err != nil {
		return err
	}
	if sb.WorkspaceTransition == workspaceSyncFromPending {
		ctx = runtime.WithExactRuntimeRef(ctx, runtime.RuntimeRef{ID: sb.RuntimeID, UID: sb.RuntimeUID})
		scoped, err := storage.NewScopedFS(m.filesystem, sb.Workspace.RootPath)
		if err != nil {
			return err
		}
		if err := m.syncFromContainerSnapshot(ctx, sb, scoped, sb.WorkspaceTransitionExclude); err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
		if err := m.checkpointSyncCleanup(ctx, sb, controller, "sync_final_output_done"); err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
	}
	if err := m.removeExactOrdinaryRuntime(ctx, sb); err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	// Incomplete mount input must never be treated as authoritative output.
	// Unmount stages already checkpointed the final output before release.
	req, err := m.workspaceLeaseRequest(sb, sb.Workspace.RootPath)
	if err != nil {
		return err
	}
	if err := controller.Fence(ctx); err != nil {
		return err
	}
	if err := m.config.WorkspaceCoordinator.releaseInterruptedSync(ctx, req, sb.Workspace.Owner); err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	if sb.Config.Mode == ModePersistent && m.sessions != nil {
		if err := m.sessions.RemoveMatchingRuntime(ctx, sb); err != nil {
			return err
		}
	}
	if sb.Config.Mode == ModeEphemeral && m.ephemeral != nil {
		record, err := m.ephemeral.Load(ctx, sb.ID)
		if err != nil {
			if !errors.Is(err, ErrSandboxNotFound) {
				return err
			}
		}
		if record != nil {
			if record.RuntimeUID != sb.RuntimeUID || record.RuntimeID != sb.RuntimeID {
				return ErrEphemeralLifecycleConflict
			}
			if err := m.ephemeral.RemoveExact(ctx, *record); err != nil {
				return err
			}
		}
	}
	if err := controller.Fence(ctx); err != nil {
		return err
	}
	if err := m.completeActiveSandboxCleanup(ctx, sb.ID, controller); err != nil {
		return err
	}
	m.retireLocalSyncController(sb)
	m.pool.NotifyRemoved()
	return nil
}
