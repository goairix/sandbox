package sandbox

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage"
	"github.com/goairix/sandbox/internal/storage/state"
)

const (
	workspaceMountPreparing   = "mount_preparing"
	workspaceMountOwned       = "mount_owned"
	workspaceUnmountSynced    = "unmount_synced"
	workspaceUnmountReleasing = "unmount_releasing"
	workspaceFlushPending     = "flush_pending"
	workspaceSyncFromPending  = "sync_from_pending"
	workspaceSyncToPending    = "sync_to_pending"
)

func (m *Manager) beginDistributedWorkspaceOperation(ctx context.Context, id string) (*Sandbox, context.Context, func(), error) {
	sb, opCtx, release, err := m.beginDistributedOperation(ctx, id, state.ActiveOperationExclusive)
	if err != nil {
		return nil, nil, nil, err
	}
	opCtx = runtime.WithExactRuntimeRef(opCtx, runtime.RuntimeRef{ID: sb.RuntimeID, UID: sb.RuntimeUID})
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		live, err := m.activeSandboxes.LiveOperations(opCtx, id)
		if err != nil {
			release()
			return nil, nil, nil, err
		}
		if live == 1 {
			if err := m.fenceWorkspaceOperation(opCtx); err != nil {
				release()
				return nil, nil, nil, err
			}
			return sb, opCtx, release, nil
		}
		select {
		case <-opCtx.Done():
			release()
			return nil, nil, nil, context.Cause(opCtx)
		case <-ticker.C:
		}
	}
}

func (m *Manager) fenceWorkspaceOperation(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	op, ok := ctx.Value(activeOperationContextKey{}).(state.ActiveSandboxOperation)
	if !ok || op.Kind != state.ActiveOperationExclusive {
		return state.ErrActiveSandboxStaleToken
	}
	ttl, _ := m.activeOperationDurations()
	if _, err := m.activeSandboxes.RenewOperation(ctx, op, ttl); err != nil {
		return err
	}
	ref, ok := runtime.ExactRuntimeRefFromContext(ctx)
	if !ok || ref.Validate() != nil {
		return runtime.ErrInvalidRuntimeRef
	}
	info, err := m.runtime.GetSandbox(ctx, ref.ID)
	if err != nil || info == nil || info.RuntimeUID != ref.UID || info.RuntimeID != ref.ID || info.State != "running" {
		return errors.Join(ErrSandboxNotReady, err)
	}
	return nil
}

func (m *Manager) deferWorkspaceCleanup(ctx context.Context, sb *Sandbox, cause error) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), activeOperationEndTimeout)
	defer cancel()
	_, _, _, err := m.activeSandboxes.BeginDestroy(cleanupCtx, sb.ID)
	return errors.Join(ErrSandboxCleanupPending, cause, err)
}

func (m *Manager) workspaceLeaseRequest(sb *Sandbox, root string) (WorkspaceLeaseRequest, error) {
	if !m.coordinatedSyncWorkspace() {
		return WorkspaceLeaseRequest{}, ErrSandboxNotReady
	}
	prefix, err := storage.BuildWorkspacePrefix(m.fsMeta.SubPath, root)
	if err != nil {
		return WorkspaceLeaseRequest{}, err
	}
	return WorkspaceLeaseRequest{MountType: WorkspaceMountSync, Provider: string(m.fsMeta.Provider), StorageIdentity: m.fsMeta.StorageIdentity,
		Bucket: m.fsMeta.Bucket, Prefix: prefix, SandboxID: sb.ID, Runtime: m.workspaceRuntimeType(), RuntimeID: sb.RuntimeID, RuntimeUID: sb.RuntimeUID}, nil
}

func (m *Manager) mountDistributedWorkspace(ctx context.Context, id, root string, exclude []string) error {
	sb, ctx, release, err := m.beginDistributedWorkspaceOperation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	if sb.Workspace != nil {
		if sb.Workspace.MountType == WorkspaceMountFUSE {
			return ErrFUSEWorkspaceImmutable
		}
		return ErrWorkspaceAlreadyMounted
	}
	if len(m.config.EnabledMountModes) > 0 && !m.config.EnabledMountModes[WorkspaceMountSync] {
		return ErrInvalidWorkspaceMountMode
	}
	if m.filesystem == nil {
		return ErrSandboxNotReady
	}
	scoped, err := storage.NewScopedFS(m.filesystem, root)
	if err != nil {
		return err
	}
	req, err := m.workspaceLeaseRequest(sb, root)
	if err != nil {
		return err
	}
	now := time.Now()
	sb.Workspace = &WorkspaceInfo{RootPath: root, MountType: WorkspaceMountSync, MountState: WorkspaceMountMounting,
		MountedAt: now, SyncExclude: append([]string(nil), exclude...)}
	sb.Config.WorkspacePath = root
	sb.Config.WorkspaceMountMode = WorkspaceMountMode(WorkspaceMountSync)
	sb.Config.WorkspaceSyncExclude = append([]string(nil), exclude...)
	sb.WorkspaceTransition = workspaceMountPreparing
	sb.UpdatedAt = now
	// The intent is durable before Acquire, so a crash in its cross-key write
	// can be recovered using the canonical prefix and exact sandbox/runtime.
	if err := m.persistActiveSandboxUpdate(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	lease, err := m.config.WorkspaceCoordinator.Acquire(ctx, req)
	if err != nil {
		if !errors.Is(err, ErrWorkspaceAcquireCleanupUnconfirmed) {
			sb.Workspace = nil
			sb.Config.WorkspacePath = ""
			sb.Config.WorkspaceSyncExclude = nil
			sb.Config.WorkspaceMountMode = ""
			sb.WorkspaceTransition = ""
			if revertErr := m.persistActiveSandboxUpdate(ctx, sb); revertErr == nil {
				return err
			} else {
				err = errors.Join(err, revertErr)
			}
		}
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	sb.Workspace.Owner = lease.OwnerSnapshot()
	sb.Workspace.LeaseGeneration = sb.Workspace.Owner.Generation
	sb.WorkspaceTransition = workspaceMountOwned
	if err := m.persistActiveSandboxUpdate(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	controller, acquired, err := m.acquireActiveController(ctx, sb)
	if err != nil || !acquired {
		return m.deferWorkspaceCleanup(ctx, sb, errors.Join(ErrSandboxNotReady, err))
	}
	keepController := false
	defer func() {
		if !keepController {
			_ = controller.Stop(context.WithoutCancel(ctx))
		}
	}()
	mountCtx, cancelMount := context.WithCancelCause(ctx)
	defer cancelMount(nil)
	renewal, err := m.config.WorkspaceCoordinator.StartRenewal(mountCtx, lease, func(lost error) { cancelMount(lost) })
	if err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	defer renewal.Stop()
	if err := m.fenceWorkspaceOperation(mountCtx); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	if err := m.syncToContainer(mountCtx, scoped, sb.RuntimeID); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	sb.Workspace.MountState = WorkspaceMountReady
	sb.Workspace.LastSyncedAt = time.Now()
	sb.UpdatedAt = sb.Workspace.LastSyncedAt
	if _, err := m.createWorkspaceLifecycle(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	if err := controller.Fence(ctx); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	sb.WorkspaceTransition = ""
	if err := m.persistActiveSandboxUpdate(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	renewal.Stop()
	if err := m.verifyWorkspaceLifecycle(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	// A local ordinary cache is not authority and must not prevent installing
	// the one controller elected for this newly mounted workspace.
	m.mu.Lock()
	delete(m.sandboxes, id)
	delete(m.operationGates, id)
	m.mu.Unlock()
	if err := m.restoreSyncSandboxWithController(ctx, sb, controller); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	keepController = true
	return nil
}

// Only the elected local controller initiates periodic copies. Each copy still
// takes the shared exclusive gate and reloads authority; sessions are never
// used to resurrect a workspace another replica already unmounted.
func (m *Manager) autoSyncDistributedWorkspaces() {
	m.mu.RLock()
	lifecycles := make([]*syncSandboxLifecycle, 0, len(m.syncLifecycles))
	for _, lifecycle := range m.syncLifecycles {
		lifecycles = append(lifecycles, lifecycle)
	}
	m.mu.RUnlock()
	for _, lifecycle := range lifecycles {
		if lifecycle.controller == nil || m.supersededSyncController(lifecycle) {
			continue
		}
		ctx, cancel := context.WithTimeout(m.controlCtx, 30*time.Second)
		if lifecycle.controller.Fence(ctx) == nil {
			_ = m.SyncWorkspace(ctx, lifecycle.sandboxID, "from_container", nil)
		}
		cancel()
	}
}

func (m *Manager) syncDistributedWorkspace(ctx context.Context, id, direction string, exclude []string) error {
	sb, ctx, release, err := m.beginDistributedWorkspaceOperation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	if sb.Workspace == nil {
		return ErrNoWorkspaceMounted
	}
	if sb.Workspace.MountType == WorkspaceMountFUSE {
		if direction == "to_container" {
			return nil
		}
		return m.flushDistributedWorkspace(ctx, sb)
	}
	if m.filesystem == nil || m.config.WorkspaceCoordinator == nil {
		return ErrSandboxNotReady
	}
	lease, err := m.config.WorkspaceCoordinator.Restore(ctx, sb.Workspace.Owner)
	if err != nil {
		return err
	}
	scoped, err := storage.NewScopedFS(m.filesystem, sb.Workspace.RootPath)
	if err != nil {
		return err
	}
	if err := m.fenceWorkspaceOperation(ctx); err != nil {
		return err
	}
	if direction == "to_container" {
		sb.WorkspaceTransition = workspaceSyncToPending
	} else {
		sb.WorkspaceTransition = workspaceSyncFromPending
		sb.WorkspaceTransitionExclude = append(append([]string(nil), sb.Workspace.SyncExclude...), exclude...)
	}
	if err := m.persistActiveSandboxUpdate(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	if direction == "to_container" {
		err = m.syncToContainer(ctx, scoped, sb.RuntimeID)
		if err == nil {
			sb.Workspace.LastSyncedAt = time.Now()
		}
	} else {
		err = m.syncFromContainerSnapshot(ctx, sb, scoped, sb.WorkspaceTransitionExclude)
	}
	if err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	if err := m.config.WorkspaceCoordinator.Renew(ctx, lease); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	if err := m.verifyWorkspaceLifecycle(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	sb.WorkspaceTransition = ""
	sb.WorkspaceTransitionExclude = nil
	if err := m.persistActiveSandboxUpdate(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	return nil
}

func (m *Manager) unmountDistributedWorkspace(ctx context.Context, id string) error {
	sb, ctx, release, err := m.beginDistributedWorkspaceOperation(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	if sb.Workspace == nil {
		return ErrNoWorkspaceMounted
	}
	if sb.Workspace.MountType == WorkspaceMountFUSE {
		return ErrFUSEWorkspaceImmutable
	}
	lease, err := m.config.WorkspaceCoordinator.Restore(ctx, sb.Workspace.Owner)
	if err != nil {
		return err
	}
	scoped, err := storage.NewScopedFS(m.filesystem, sb.Workspace.RootPath)
	if err != nil {
		return err
	}
	if err := m.fenceWorkspaceOperation(ctx); err != nil {
		return err
	}
	if err := m.syncFromContainerSnapshot(ctx, sb, scoped, sb.Workspace.SyncExclude); err != nil {
		return err
	}
	sb.WorkspaceTransition = workspaceUnmountSynced
	if err := m.persistActiveSandboxUpdate(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	if err := m.verifyWorkspaceLifecycle(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	if err := m.fenceWorkspaceOperation(ctx); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	sb.WorkspaceTransition = workspaceUnmountReleasing
	if err := m.persistActiveSandboxUpdate(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	m.retireLocalSyncController(sb)
	if err := m.config.WorkspaceCoordinator.Release(ctx, lease, runtime.TerminationEvidence{}); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	if sb.Config.Mode == ModeEphemeral {
		record, err := m.ephemeral.Load(ctx, id)
		if err != nil {
			return m.deferWorkspaceCleanup(ctx, sb, err)
		}
		if record == nil {
			return m.deferWorkspaceCleanup(ctx, sb, ErrEphemeralLifecycleInvalid)
		}
		if err := m.ephemeral.RemoveExact(ctx, *record); err != nil {
			return m.deferWorkspaceCleanup(ctx, sb, err)
		}
	}
	sb.Workspace = nil
	sb.Config.WorkspacePath = ""
	sb.Config.WorkspaceMountMode = ""
	sb.Config.WorkspaceSyncExclude = nil
	sb.WorkspaceTransition = ""
	sb.UpdatedAt = time.Now()
	if sb.Config.Mode == ModePersistent && m.sessions != nil {
		if err := m.sessions.Save(ctx, sb); err != nil {
			return m.deferWorkspaceCleanup(ctx, sb, err)
		}
	}
	if err := m.persistActiveSandboxUpdate(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	return nil
}

func (m *Manager) retireLocalSyncController(sb *Sandbox) {
	m.mu.Lock()
	lifecycle := m.syncLifecycles[sb.ID]
	if lifecycle == nil || lifecycle.lease.OwnerSnapshot().Generation != sb.Workspace.LeaseGeneration {
		m.mu.Unlock()
		return
	}
	delete(m.syncLifecycles, sb.ID)
	delete(m.workspaces, sb.ID)
	delete(m.sandboxes, sb.ID)
	delete(m.operationGates, sb.ID)
	m.mu.Unlock()
	lifecycle.gate.closeAdmission()
	if lifecycle.renewal != nil {
		lifecycle.renewal.Stop()
	}
	if lifecycle.controller != nil {
		// Do not wait for controller.done from a lease-loss callback: the
		// controller's own onLost can concurrently be stopping this renewal.
		lifecycle.controller.cancel()
	}
}

func (m *Manager) flushDistributedWorkspace(ctx context.Context, sb *Sandbox) error {
	ref := runtime.RuntimeRef{ID: sb.RuntimeID, UID: sb.RuntimeUID}
	generation := sb.Workspace.LeaseGeneration
	sb.Workspace.Flushed = false
	sb.WorkspaceTransition = workspaceFlushPending
	if err := m.persistActiveSandboxUpdate(ctx, sb); err != nil {
		return m.deferWorkspaceCleanup(ctx, sb, err)
	}
	fail := func(err error) error { return m.deferWorkspaceCleanup(ctx, sb, err) }
	if err := m.fenceWorkspaceOperation(ctx); err != nil {
		return fail(err)
	}
	token, err := m.runtime.QuiesceWorkspace(ctx, ref, generation)
	if err != nil {
		return fail(err)
	}
	if token.RuntimeUID != ref.UID || token.Generation != generation || token.Opaque == "" {
		return fail(ErrSandboxNotReady)
	}
	if err := m.fenceWorkspaceOperation(ctx); err != nil {
		return fail(err)
	}
	if err := m.runtime.FlushWorkspace(ctx, ref, generation); err != nil {
		return fail(err)
	}
	if err := m.fenceWorkspaceOperation(ctx); err != nil {
		return fail(err)
	}
	if err := m.runtime.ResumeWorkspace(ctx, ref, token); err != nil {
		return fail(err)
	}
	health, err := m.runtime.WorkspaceHealth(ctx, ref)
	if err != nil {
		return fail(err)
	}
	if health == nil || !health.Ready || health.RuntimeUID != ref.UID || health.Generation != generation || health.MountType != string(WorkspaceMountFUSE) {
		return fail(ErrSandboxNotReady)
	}
	now := time.Now()
	sb.Workspace.Flushed = true
	sb.Workspace.LastFlushedAt = &now
	sb.Workspace.LastSyncedAt = now
	sb.UpdatedAt = now
	if err := m.verifyWorkspaceLifecycle(ctx, sb); err != nil {
		return fail(err)
	}
	sb.WorkspaceTransition = ""
	if err := m.persistActiveSandboxUpdate(ctx, sb); err != nil {
		return fail(err)
	}
	return nil
}

// supersededSyncController prevents an old local renewal from destroying a
// workspace which another replica already unmounted or replaced.
func (m *Manager) supersededSyncController(lifecycle *syncSandboxLifecycle) bool {
	if !m.distributedStateEnabled() || lifecycle == nil || lifecycle.lease == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(m.controlCtx, activeOperationEndTimeout)
	defer cancel()
	record, err := m.activeSandboxes.Load(ctx, lifecycle.sandboxID)
	if err != nil {
		return false
	}
	if record == nil {
		m.retireLocalSyncController(&Sandbox{ID: lifecycle.sandboxID, Workspace: &WorkspaceInfo{LeaseGeneration: lifecycle.lease.OwnerSnapshot().Generation}})
		return true
	}
	sb, err := decodeActiveSandboxPhase(record, lifecycle.sandboxID, state.ActiveSandboxActive, state.ActiveSandboxExclusive, state.ActiveSandboxDestroying, state.ActiveSandboxCleanupPending)
	if err != nil {
		return false
	}
	if sb.Workspace == nil || sb.Workspace.LeaseGeneration != lifecycle.lease.OwnerSnapshot().Generation || strings.HasPrefix(sb.WorkspaceTransition, "unmount_") {
		m.retireLocalSyncController(&Sandbox{ID: lifecycle.sandboxID, Workspace: &WorkspaceInfo{LeaseGeneration: lifecycle.lease.OwnerSnapshot().Generation}})
		return true
	}
	return false
}
