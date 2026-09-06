package sandbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
)

func (m *Manager) coordinatedSyncWorkspace() bool {
	return m.config.WorkspaceCoordinator != nil && m.fsMeta != nil &&
		m.fsMeta.Provider != storage.ProviderLocal && m.fsMeta.StorageIdentity != ""
}

func (m *Manager) workspaceRuntimeType() string {
	if m.config.RuntimeType != "" {
		return m.config.RuntimeType
	}
	if m.fusePool != nil && m.fusePool.spec.WorkspaceFUSE != nil {
		return m.fusePool.spec.WorkspaceFUSE.RuntimeType
	}
	return "runtime"
}

// prepareSyncWorkspace owns all work which must finish before a sync workspace
// becomes visible: acquire the cross-mode lease, start renewal, and sync in.
func (m *Manager) prepareSyncWorkspace(ctx context.Context, sb *Sandbox, gate *operationGate, rootPath string, exclude []string) (storage.ScopedFS, *syncSandboxLifecycle, error) {
	if sb == nil || gate == nil {
		return nil, nil, ErrSandboxNotReady
	}
	scoped, err := storage.NewScopedFS(m.filesystem, rootPath)
	if err != nil {
		return nil, nil, fmt.Errorf("create scoped filesystem: %w", err)
	}

	var lease *WorkspaceLease
	var renewal *WorkspaceLeaseRenewal
	lifecycle := &syncSandboxLifecycle{sandboxID: sb.ID, gate: gate}
	if m.coordinatedSyncWorkspace() {
		prefix, prefixErr := storage.BuildWorkspacePrefix(m.fsMeta.SubPath, rootPath)
		if prefixErr != nil {
			return nil, nil, prefixErr
		}
		lease, err = m.config.WorkspaceCoordinator.Acquire(ctx, WorkspaceLeaseRequest{
			MountType: WorkspaceMountSync, Provider: string(m.fsMeta.Provider),
			StorageIdentity: m.fsMeta.StorageIdentity, Bucket: m.fsMeta.Bucket, Prefix: prefix,
			SandboxID: sb.ID, Runtime: m.workspaceRuntimeType(), RuntimeID: sb.RuntimeID, RuntimeUID: sb.RuntimeUID,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("acquire workspace lease: %w", err)
		}
		lifecycle.lease = lease
		mountCtx, cancelMount := context.WithCancel(ctx)
		defer cancelMount()
		renewal, err = m.config.WorkspaceCoordinator.StartRenewal(context.Background(), lease, func(lost error) {
			cancelMount()
			m.markSyncLeaseLost(lifecycle, lost)
		})
		if err != nil {
			_ = m.config.WorkspaceCoordinator.Release(context.Background(), lease, runtime.TerminationEvidence{})
			return nil, nil, fmt.Errorf("start workspace lease renewal: %w", err)
		}
		lifecycle.renewal = renewal
		ctx = mountCtx
	}

	if err := m.syncToContainer(ctx, scoped, sb.RuntimeID); err != nil {
		if renewal != nil {
			renewal.Stop()
		}
		if lease != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), workspaceCleanupTimeout)
			_ = m.config.WorkspaceCoordinator.Release(cleanupCtx, lease, runtime.TerminationEvidence{})
			cancel()
		}
		return nil, nil, fmt.Errorf("sync to container: %w", err)
	}
	if lease == nil {
		return scoped, nil, nil
	}
	return scoped, lifecycle, nil
}

func (m *Manager) releasePreparedSyncWorkspace(lifecycle *syncSandboxLifecycle) error {
	if lifecycle == nil {
		return nil
	}
	if lifecycle.renewal != nil {
		lifecycle.renewal.Stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), workspaceCleanupTimeout)
	defer cancel()
	return m.config.WorkspaceCoordinator.Release(ctx, lifecycle.lease, runtime.TerminationEvidence{})
}

func (m *Manager) scheduleSyncFinalization(lifecycle *syncSandboxLifecycle, cause error) {
	if lifecycle == nil {
		return
	}
	lifecycle.once.Do(func() {
		go func() {
			_ = cause
			backoff := 100 * time.Millisecond
			for {
				if err := m.destroySyncSandbox(context.Background(), lifecycle); err == nil || errors.Is(err, ErrSandboxNotFound) {
					return
				}
				select {
				case <-m.controlCtx.Done():
					return
				case <-time.After(backoff):
				}
				if backoff < 5*time.Second {
					backoff *= 2
				}
			}
		}()
	})
}

func (m *Manager) markSyncLeaseLost(lifecycle *syncSandboxLifecycle, cause error) {
	if lifecycle == nil {
		return
	}
	lifecycle.mu.Lock()
	lifecycle.lost = true
	published := lifecycle.published
	lifecycle.mu.Unlock()
	lifecycle.gate.closeAdmission()
	if published {
		m.scheduleSyncFinalization(lifecycle, cause)
	}
}

func publishSyncLifecycle(lifecycle *syncSandboxLifecycle) bool {
	if lifecycle == nil {
		return true
	}
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	if lifecycle.lost {
		return false
	}
	lifecycle.published = true
	return true
}

func (m *Manager) restartSyncRenewal(lifecycle *syncSandboxLifecycle) error {
	if lifecycle == nil || lifecycle.lease == nil {
		return nil
	}
	renewal, err := m.config.WorkspaceCoordinator.StartRenewal(context.Background(), lifecycle.lease, func(lost error) {
		m.markSyncLeaseLost(lifecycle, lost)
	})
	if err != nil {
		return errors.Join(ErrWorkspaceLeaseLost, err)
	}
	lifecycle.renewal = renewal
	return nil
}

func (m *Manager) destroySyncSandbox(ctx context.Context, lifecycle *syncSandboxLifecycle) error {
	if lifecycle == nil {
		return ErrSandboxNotReady
	}
	lifecycle.finalizeMu.Lock()
	defer lifecycle.finalizeMu.Unlock()
	if err := lifecycle.gate.CloseAndWait(ctx); err != nil {
		return errors.Join(ErrSandboxCleanupPending, fmt.Errorf("drain sync sandbox operations: %w", err))
	}
	m.mu.RLock()
	sb := m.sandboxes[lifecycle.sandboxID]
	m.mu.RUnlock()
	if sb == nil {
		return fmt.Errorf("%w: %s", ErrSandboxNotFound, lifecycle.sandboxID)
	}
	if sb.State != StateDestroying {
		snapshot := cloneSandbox(sb)
		snapshot.State = StateDestroying
		snapshot.UpdatedAt = time.Now()
		if sb.Config.Mode == ModePersistent {
			if m.sessions == nil {
				return errors.Join(ErrSandboxCleanupPending, ErrSandboxNotReady)
			}
			if err := m.sessions.Save(ctx, &snapshot); err != nil {
				return errors.Join(ErrSandboxCleanupPending, fmt.Errorf("persist destroying sandbox: %w", err))
			}
		} else if lifecycle.ephemeralRecord != nil && lifecycle.ephemeralRecord.State == EphemeralActive {
			next, err := m.ephemeral.Transition(ctx, sb.ID, lifecycle.ephemeralRecord.Revision, EphemeralFinalizing)
			if err != nil {
				return errors.Join(ErrSandboxCleanupPending, fmt.Errorf("persist sync finalization: %w", err))
			}
			lifecycle.ephemeralRecord = next
		}
		m.mu.Lock()
		if current := m.sandboxes[sb.ID]; current == sb {
			current.State = StateDestroying
			current.UpdatedAt = snapshot.UpdatedAt
		}
		m.mu.Unlock()
	}
	var exclude []string
	if sb.Workspace != nil {
		exclude = append([]string(nil), sb.Workspace.SyncExclude...)
	}
	if !lifecycle.finalSyncDone && (lifecycle.ephemeralRecord == nil || lifecycle.ephemeralRecord.State == EphemeralFinalizing) {
		if err := m.syncFromContainer(ctx, sb.ID, sb.RuntimeID, exclude); err != nil {
			return errors.Join(ErrSandboxCleanupPending, fmt.Errorf("final sync from container: %w", err))
		}
		lifecycle.finalSyncDone = true
	}
	if lifecycle.ephemeralRecord != nil && lifecycle.ephemeralRecord.State == EphemeralFinalizing {
		next, err := m.ephemeral.Transition(ctx, sb.ID, lifecycle.ephemeralRecord.Revision, EphemeralRemovingRuntime)
		if err != nil {
			return errors.Join(ErrSandboxCleanupPending, fmt.Errorf("persist completed final sync: %w", err))
		}
		lifecycle.ephemeralRecord = next
	}
	if !lifecycle.runtimeRemoved && (lifecycle.ephemeralRecord == nil || lifecycle.ephemeralRecord.State == EphemeralRemovingRuntime) {
		if err := m.runtime.RemoveSandbox(ctx, sb.RuntimeID); err != nil {
			return errors.Join(ErrSandboxCleanupPending, fmt.Errorf("remove sandbox: %w", err))
		}
		lifecycle.runtimeRemoved = true
	}
	if lifecycle.ephemeralRecord != nil && lifecycle.ephemeralRecord.State == EphemeralRemovingRuntime {
		next, err := m.ephemeral.Transition(ctx, sb.ID, lifecycle.ephemeralRecord.Revision, EphemeralReleasingLease)
		if err != nil {
			return errors.Join(ErrSandboxCleanupPending, fmt.Errorf("persist removed runtime: %w", err))
		}
		lifecycle.ephemeralRecord = next
	}
	if lifecycle.ephemeralRecord != nil && lifecycle.ephemeralRecord.State != EphemeralReleasingLease {
		return errors.Join(ErrSandboxCleanupPending, ErrEphemeralLifecycleConflict)
	}
	if lifecycle.renewal != nil {
		lifecycle.renewal.Stop()
	}
	if lifecycle.lease != nil {
		if err := m.config.WorkspaceCoordinator.Release(ctx, lifecycle.lease, runtime.TerminationEvidence{}); err != nil {
			return errors.Join(ErrSandboxCleanupPending, fmt.Errorf("release workspace lease: %w", err))
		}
	}
	if err := m.removeWorkspaceLifecycle(ctx, sb, lifecycle.ephemeralRecord); err != nil {
		return errors.Join(ErrSandboxCleanupPending, fmt.Errorf("remove workspace lifecycle: %w", err))
	}
	m.mu.Lock()
	if m.syncLifecycles[sb.ID] == lifecycle {
		delete(m.syncLifecycles, sb.ID)
		delete(m.operationGates, sb.ID)
		delete(m.workspaces, sb.ID)
		delete(m.sandboxes, sb.ID)
	}
	m.mu.Unlock()
	m.pool.NotifyRemoved()
	return nil
}

func (m *Manager) restoreSyncSandbox(ctx context.Context, sb *Sandbox) error {
	if sb == nil || sb.Workspace == nil || m.config.WorkspaceCoordinator == nil || m.fsMeta == nil {
		return ErrSandboxNotReady
	}
	workspace := sb.Workspace
	owner := workspace.Owner
	prefix, err := storage.BuildWorkspacePrefix(m.fsMeta.SubPath, workspace.RootPath)
	if err != nil || owner.MountType != WorkspaceMountSync || owner.MountAttempt != 0 ||
		owner.SandboxID != sb.ID || owner.RuntimeID != sb.RuntimeID || owner.RuntimeUID == "" || owner.RuntimeUID != sb.RuntimeUID ||
		owner.Generation != workspace.LeaseGeneration || owner.Provider != string(m.fsMeta.Provider) ||
		owner.StorageIdentityHash != storageIdentityHash(m.fsMeta.StorageIdentity) || owner.Bucket != m.fsMeta.Bucket ||
		owner.Prefix != prefix || owner.Runtime != m.workspaceRuntimeType() {
		return ErrSandboxNotReady
	}
	info, err := m.runtime.GetSandbox(ctx, sb.RuntimeID)
	if err != nil || info == nil || info.State != "running" || info.RuntimeID != sb.RuntimeID || info.RuntimeUID != sb.RuntimeUID {
		return ErrSandboxNotReady
	}
	scoped, err := storage.NewScopedFS(m.filesystem, workspace.RootPath)
	if err != nil {
		return fmt.Errorf("restore sync scoped filesystem: %w", err)
	}
	lease, err := m.config.WorkspaceCoordinator.Restore(ctx, owner)
	if err != nil {
		return fmt.Errorf("restore sync workspace lease: %w", err)
	}
	gate := newOperationGate(true)
	lifecycle := &syncSandboxLifecycle{sandboxID: sb.ID, gate: gate, lease: lease}
	renewal, err := m.config.WorkspaceCoordinator.StartRenewal(context.Background(), lease, func(lost error) {
		m.markSyncLeaseLost(lifecycle, lost)
	})
	if err != nil {
		return fmt.Errorf("restart sync workspace lease renewal: %w", err)
	}
	lifecycle.renewal = renewal
	m.mu.Lock()
	if m.sandboxes[sb.ID] != nil || !publishSyncLifecycle(lifecycle) {
		m.mu.Unlock()
		renewal.Stop()
		return ErrSandboxNotReady
	}
	m.sandboxes[sb.ID] = sb
	m.workspaces[sb.ID] = scoped
	m.operationGates[sb.ID] = gate
	m.syncLifecycles[sb.ID] = lifecycle
	m.mu.Unlock()
	return nil
}

func (m *Manager) finalizeRecoveredPersistentSync(ctx context.Context, sb *Sandbox) error {
	if sb == nil || sb.Workspace == nil || sb.Workspace.MountType != WorkspaceMountSync ||
		sb.State != StateDestroying || m.config.WorkspaceCoordinator == nil || m.sessions == nil {
		return ErrSandboxCleanupPending
	}
	lease, err := m.config.WorkspaceCoordinator.Restore(ctx, sb.Workspace.Owner)
	if err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	renewal, err := m.config.WorkspaceCoordinator.StartRenewal(context.Background(), lease, nil)
	if err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	defer renewal.Stop()
	info, err := m.runtime.GetSandbox(ctx, sb.RuntimeID)
	if err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	if info != nil {
		if info.RuntimeUID != sb.RuntimeUID || info.State != "running" {
			return errors.Join(ErrSandboxCleanupPending, ErrSandboxNotReady)
		}
		scoped, err := storage.NewScopedFS(m.filesystem, sb.Workspace.RootPath)
		if err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
		if err := m.fullSyncFromContainer(ctx, scoped, sb.RuntimeID, sb.Workspace.SyncExclude); err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
		if err := m.runtime.RemoveSandbox(ctx, sb.RuntimeID); err != nil {
			return errors.Join(ErrSandboxCleanupPending, err)
		}
	}
	if err := m.config.WorkspaceCoordinator.Release(ctx, lease, runtime.TerminationEvidence{}); err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	if err := m.sessions.Remove(ctx, sb.ID); err != nil {
		return errors.Join(ErrSandboxCleanupPending, err)
	}
	return nil
}
