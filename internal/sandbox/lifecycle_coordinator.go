package sandbox

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/storage/state"
)

const (
	defaultControllerTTL           = 15 * time.Second
	defaultControllerRenewInterval = 5 * time.Second
	activeLifecycleScanInterval    = 5 * time.Second
	activeLifecycleScanPageSize    = 128
	// Each tick processes at most this many pages per index. Cursors carry
	// progress across ticks so one repeatedly failing cleanup cannot starve
	// candidates with later keys; the next pass wraps at the end.
	activeLifecycleIndexedPagesPerTick = 8
	activePublishingRecoveryGrace      = time.Minute
	// Wakeups are an optimization for local destroys, not a durable queue.
	// Overflow is recovered from the durable due index or legacy full scan.
	activeCleanupWakeQueueSize = 256
)

// activeController is a generation-fenced capability. Every lifecycle side
// effect calls Fence; the background renewal only keeps takeover latency low.
type activeController struct {
	repository state.ActiveSandboxRepository
	mu         sync.Mutex
	lease      state.ActiveSandboxControllerLease
	validUntil time.Time
	ttl        time.Duration
	interval   time.Duration
	cancel     context.CancelFunc
	done       chan struct{}
	stopOnce   sync.Once
	lost       error
	onLost     func(error)
}

func (m *Manager) controllerDurations() (time.Duration, time.Duration) {
	ttl := m.config.ControllerTTL
	if ttl <= 0 {
		ttl = defaultControllerTTL
	}
	interval := m.config.ControllerRenewInterval
	if interval <= 0 || interval >= ttl {
		interval = defaultControllerRenewInterval
		if interval >= ttl {
			interval = ttl / 3
		}
	}
	return ttl, interval
}

func (m *Manager) acquireActiveController(ctx context.Context, sb *Sandbox) (*activeController, bool, error) {
	if !m.distributedStateEnabled() || sb == nil || sb.ID == "" || sb.activeGeneration <= 0 {
		return nil, false, state.ErrActiveSandboxCorrupt
	}
	ttl, interval := m.controllerDurations()
	lease := state.ActiveSandboxControllerLease{
		SandboxID: sb.ID, Token: uuid.NewString(), InstanceID: m.config.InstanceID,
		PodUID: m.config.InstanceID, Generation: sb.activeGeneration, ExpiresAt: time.Now().Add(ttl),
	}
	acquiredLease, acquired, err := m.activeSandboxes.AcquireController(ctx, lease, ttl)
	if err != nil || !acquired || acquiredLease == nil {
		if err == nil && acquired && acquiredLease == nil {
			err = state.ErrActiveSandboxCorrupt
		}
		return nil, acquired, err
	}
	controlCtx, cancel := context.WithCancel(m.controlCtx)
	controller := &activeController{
		repository: m.activeSandboxes, lease: *acquiredLease, ttl: ttl,
		validUntil: time.Now().Add(ttl), interval: interval, cancel: cancel, done: make(chan struct{}),
	}
	go controller.run(controlCtx)
	return controller, true, nil
}

func (c *activeController) run(ctx context.Context) {
	defer close(c.done)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.Fence(ctx); err != nil {
				c.mu.Lock()
				lost := c.lost
				onLost := c.onLost
				c.mu.Unlock()
				if lost != nil {
					c.cancel()
					if onLost != nil {
						onLost(lost)
					}
					return
				}
			}
		}
	}
}

func (c *activeController) SetOnLost(onLost func(error)) {
	if c == nil || onLost == nil {
		return
	}
	c.mu.Lock()
	c.onLost = onLost
	lost := c.lost
	c.mu.Unlock()
	if lost != nil {
		onLost(lost)
	}
}

func (c *activeController) Fence(ctx context.Context) error {
	if c == nil {
		return state.ErrActiveSandboxStaleToken
	}
	if err := c.lockContext(ctx); err != nil {
		return err
	}
	defer c.mu.Unlock()
	if c.lost != nil {
		return errors.Join(state.ErrActiveSandboxStaleToken, c.lost)
	}
	renewed, err := c.repository.RenewController(ctx, c.lease, c.ttl)
	if err != nil || renewed == nil {
		if err == nil {
			err = state.ErrActiveSandboxStaleToken
		}
		if errors.Is(err, state.ErrActiveSandboxStaleToken) ||
			errors.Is(err, state.ErrActiveSandboxLeaseExpired) ||
			!time.Now().Before(c.validUntil.Add(-c.interval)) {
			c.lost = err
		}
		return err
	}
	c.lease = *renewed
	c.validUntil = time.Now().Add(c.ttl)
	return nil
}

// A background renewal holds mu across its RPC. Requests must be able to stop
// waiting for that renewal when their own deadline expires.
func (c *activeController) lockContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.mu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if c.mu.TryLock() {
				return nil
			}
		}
	}
}

func (c *activeController) Stop(ctx context.Context) error {
	if c == nil {
		return nil
	}
	var releaseErr error
	c.stopOnce.Do(func() {
		c.cancel()
		<-c.done
		c.mu.Lock()
		lease := c.lease
		c.mu.Unlock()
		releaseErr = c.repository.ReleaseController(ctx, lease)
		if errors.Is(releaseErr, state.ErrActiveSandboxStaleToken) {
			releaseErr = nil
		}
	})
	return releaseErr
}

func (c *activeController) checkpoint(ctx context.Context, revision uint64, checkpoint string) (*state.ActiveSandboxRecord, error) {
	if err := c.Fence(ctx); err != nil {
		return nil, err
	}
	c.mu.Lock()
	lease := c.lease
	c.mu.Unlock()
	repository, ok := c.repository.(state.ActiveSandboxCheckpointRepository)
	if !ok {
		return nil, state.ErrActiveSandboxCorrupt
	}
	return repository.CheckpointController(ctx, lease, revision, checkpoint)
}

func (c *activeController) deleteRecordWithResult(ctx context.Context, revision uint64) (bool, error) {
	if err := c.Fence(ctx); err != nil {
		return false, err
	}
	c.mu.Lock()
	lease := c.lease
	c.mu.Unlock()
	if repository, ok := c.repository.(state.ActiveSandboxCleanupCompletionRepository); ok {
		return repository.DeleteControllerWithResult(ctx, lease, revision)
	}
	repository, ok := c.repository.(state.ActiveSandboxCheckpointRepository)
	if !ok {
		return false, state.ErrActiveSandboxCorrupt
	}
	// Legacy repositories can confirm successful cleanup but do not distinguish
	// actual deletion from historical absence. Do not charge guessed metrics.
	return false, repository.DeleteController(ctx, lease, revision)
}

// runActiveLifecycleCoordinator discovers due and workspace lifecycles after
// Start's full recovery pass. Work is off the request path and jittered per
// replica; the controller lease remains the final single-writer fence.
func (m *Manager) runActiveLifecycleCoordinator() {
	defer m.wg.Done()
	delay := lifecycleScanDelay(m.config.InstanceID)
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-m.controlCtx.Done():
			return
		case <-timer.C:
			// Start completed the one full recovery pass before accepting
			// requests. Subsequent ticks only discover indexed candidates.
			_ = m.reconcileActiveLifecycleControllers(m.controlCtx)
			timer.Reset(activeLifecycleScanInterval + lifecycleScanDelay(m.config.InstanceID))
		case id := <-m.cleanupWake:
			if err := m.reconcileActiveLifecycleID(m.controlCtx, id); err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn(m.controlCtx, "scheduled sandbox lifecycle reconciliation deferred",
					logger.AddField("sandbox_id", id), logger.ErrorField(err))
			}
		}
	}
}

// A request already supplied the durable sandbox ID. Load and reconcile only
// that record; the periodic scan remains the fallback for a full wake queue,
// another replica's writes, process restarts, and lost notifications.
func (m *Manager) reconcileActiveLifecycleID(ctx context.Context, id string) error {
	record, err := m.activeSandboxes.Load(ctx, id)
	if err != nil || record == nil {
		return err
	}
	return m.reconcileActiveLifecycle(ctx, record)
}

func lifecycleScanDelay(instanceID string) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(instanceID))
	return time.Duration(h.Sum32()%1000) * time.Millisecond
}

// scanActiveLifecycleInventory is the one full startup recovery pass for an
// indexed repository. Legacy repositories also use it on later ticks.
func (m *Manager) scanActiveLifecycleInventory(ctx context.Context) error {
	var cursor uint64
	for {
		page, err := m.activeSandboxes.Scan(ctx, cursor, activeLifecycleScanPageSize)
		if err != nil {
			return err
		}
		for i := range page.Records {
			if err := m.reconcileActiveLifecycle(ctx, &page.Records[i]); err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn(ctx, "active sandbox lifecycle reconciliation deferred",
					logger.AddField("sandbox_id", page.Records[i].SandboxID),
					logger.ErrorField(err),
				)
			}
		}
		if page.Cursor == 0 {
			return nil
		}
		cursor = page.Cursor
	}
}

// reconcileActiveLifecycleControllers uses bounded durable indexes when the
// repository provides them. A legacy repository retains its full-scan path.
// Each candidate is reloaded: an index entry does not establish current TTL,
// controller ownership, runtime identity or permission to release resources.
func (m *Manager) reconcileActiveLifecycleControllers(ctx context.Context) error {
	indexed, ok := m.activeSandboxes.(state.ActiveSandboxLifecycleIndexRepository)
	if !ok {
		return m.scanActiveLifecycleInventory(ctx)
	}

	m.lifecycleIndexMu.Lock()
	defer m.lifecycleIndexMu.Unlock()

	now := time.Now()
	for range activeLifecycleIndexedPagesPerTick {
		page, err := indexed.ScanDue(ctx, now, m.lifecycleDueCursor, activeLifecycleScanPageSize)
		if err != nil {
			return err
		}
		if err := m.reconcileActiveLifecycleCandidates(ctx, page.Records); err != nil {
			return err
		}
		m.lifecycleDueCursor = page.Cursor
		if page.Cursor == "" {
			break
		}
	}
	for range activeLifecycleIndexedPagesPerTick {
		page, err := indexed.ScanWorkspace(ctx, m.lifecycleWorkspaceCursor, activeLifecycleScanPageSize)
		if err != nil {
			return err
		}
		if err := m.reconcileActiveLifecycleCandidates(ctx, page.Records); err != nil {
			return err
		}
		m.lifecycleWorkspaceCursor = page.Cursor
		if page.Cursor == 0 {
			break
		}
	}
	return nil
}

func (m *Manager) reconcileActiveLifecycleCandidates(ctx context.Context, candidates []state.ActiveSandboxRecord) error {
	for _, candidate := range candidates {
		current, err := m.activeSandboxes.Load(ctx, candidate.SandboxID)
		if err != nil {
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				return err
			}
			logger.Warn(ctx, "active sandbox lifecycle candidate read deferred",
				logger.AddField("sandbox_id", candidate.SandboxID), logger.ErrorField(err))
			continue
		}
		if current == nil {
			continue
		}
		if err := m.reconcileActiveLifecycle(ctx, current); err != nil && !errors.Is(err, context.Canceled) {
			logger.Warn(ctx, "active sandbox lifecycle reconciliation deferred",
				logger.AddField("sandbox_id", current.SandboxID), logger.ErrorField(err))
		}
	}
	return nil
}

func (m *Manager) reconcileActiveLifecycle(ctx context.Context, record *state.ActiveSandboxRecord) error {
	if record == nil {
		return nil
	}
	if record.Phase == state.ActiveSandboxExclusive {
		// BeginOperation atomically recovers an expired exclusive gate. A live
		// holder rejects admission; no background controller races its effects.
		_, operation, err := m.activeSandboxes.BeginOperation(ctx, record.SandboxID, uuid.NewString(), state.ActiveOperationData, defaultActiveOperationTTL)
		if operation != nil {
			_ = m.activeSandboxes.EndOperation(ctx, *operation)
		}
		if err != nil && !errors.Is(err, state.ErrActiveSandboxAdmissionClosed) && !errors.Is(err, state.ErrActiveSandboxConflict) {
			return err
		}
		current, err := m.activeSandboxes.Load(ctx, record.SandboxID)
		if err != nil || current == nil || current.Phase == state.ActiveSandboxExclusive {
			return err
		}
		record = current
	}
	if record.Phase == state.ActiveSandboxPublishing {
		if time.Since(record.UpdatedAt) < activePublishingRecoveryGrace {
			return nil
		}
		_, err := m.destroyDistributedSandboxWithWait(ctx, record.SandboxID, false)
		return err
	}
	if record.Phase != state.ActiveSandboxActive &&
		record.Phase != state.ActiveSandboxDestroying && record.Phase != state.ActiveSandboxCleanupPending {
		return nil
	}
	sb, err := decodeActiveSandboxPhase(record, record.SandboxID,
		state.ActiveSandboxActive, state.ActiveSandboxDestroying, state.ActiveSandboxCleanupPending)
	if err != nil {
		return err
	}
	if record.Phase == state.ActiveSandboxActive && sb.Timeout > 0 &&
		!sb.CreatedAt.Add(sb.Timeout).After(time.Now()) {
		// The scan reads durable state, so expiry survives a replica restart.
		// Fence the inspected snapshot: a concurrent TTL renewal must win over
		// this old observation instead of losing a live sandbox.
		conditional, ok := m.activeSandboxes.(state.ActiveSandboxConditionalDestroyRepository)
		if !ok {
			return state.ErrActiveSandboxCorrupt
		}
		closed, closeErr := conditional.BeginDestroyExpected(ctx, *record)
		if errors.Is(closeErr, state.ErrActiveSandboxConflict) {
			return nil
		}
		if closeErr != nil {
			return closeErr
		}
		if closed == nil {
			return nil
		}
		record = closed
	}
	// A live local sync worker may already have checkpointed final output
	// while waiting for Pod termination. Schedule it before crash recovery,
	// which must never release that worker's controller capability.
	if sb.WorkspaceTransition == "" && sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountSync {
		m.mu.RLock()
		syncLifecycle := m.syncLifecycles[sb.ID]
		m.mu.RUnlock()
		if syncLifecycle != nil {
			if m.supersededSyncController(syncLifecycle) {
				return nil
			}
			if record.Phase != state.ActiveSandboxActive {
				m.scheduleSyncFinalization(syncLifecycle, ErrSandboxCleanupPending)
			}
			return nil
		}
	}
	if sb.Workspace != nil && sb.Workspace.MountType == WorkspaceMountSync &&
		(record.CleanupCheckpoint == "sync_final_output_done" || record.CleanupCheckpoint == "sync_runtime_removed") {
		if record.Phase == state.ActiveSandboxActive {
			return nil
		}
		cleanupCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		return m.cleanupCheckpointedSyncWorkspace(cleanupCtx, sb, false)
	}
	if sb.WorkspaceTransition != "" || sb.Workspace == nil {
		if record.Phase != state.ActiveSandboxActive {
			cleanupCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			defer cancel()
			_, err := m.destroyDistributedSandboxWithWait(cleanupCtx, sb.ID, false)
			return err
		}
		return nil
	}
	if sb.Workspace.MountType != WorkspaceMountFUSE && sb.Workspace.Owner.Generation <= 0 {
		return err
	}

	m.mu.RLock()
	fuseLifecycle := m.fuseLifecycles[sb.ID]
	syncLifecycle := m.syncLifecycles[sb.ID]
	m.mu.RUnlock()
	if fuseLifecycle != nil || syncLifecycle != nil {
		if syncLifecycle != nil && m.supersededSyncController(syncLifecycle) {
			return nil
		}
		if record.Phase != state.ActiveSandboxActive {
			if fuseLifecycle != nil {
				m.scheduleFUSETeardown(fuseLifecycle, ErrSandboxCleanupPending)
			} else {
				m.scheduleSyncFinalization(syncLifecycle, ErrSandboxCleanupPending)
			}
		}
		return nil
	}

	controller, acquired, err := m.acquireActiveController(ctx, sb)
	if err != nil || !acquired {
		return err
	}
	keepController := false
	defer func() {
		if !keepController {
			_ = controller.Stop(ctx)
		}
	}()

	switch sb.Workspace.MountType {
	case WorkspaceMountFUSE:
		if record.Phase == state.ActiveSandboxActive {
			err = m.restoreFUSESandboxWithController(ctx, sb, controller)
		} else {
			err = m.restoreFUSECleanupWithController(ctx, sb, controller)
		}
	case WorkspaceMountSync:
		err = m.restoreSyncSandboxWithController(ctx, sb, controller)
	default:
		return fmt.Errorf("%w: unsupported coordinated mount %q", ErrSandboxNotReady, sb.Workspace.MountType)
	}
	if err != nil {
		return err
	}
	keepController = true
	if record.Phase != state.ActiveSandboxActive {
		m.mu.RLock()
		fuseLifecycle = m.fuseLifecycles[sb.ID]
		syncLifecycle = m.syncLifecycles[sb.ID]
		m.mu.RUnlock()
		if fuseLifecycle != nil {
			m.scheduleFUSETeardown(fuseLifecycle, ErrSandboxCleanupPending)
		} else if syncLifecycle != nil {
			m.scheduleSyncFinalization(syncLifecycle, ErrSandboxCleanupPending)
		}
	}
	return nil
}

func (m *Manager) bindFUSEController(lifecycle *fuseSandboxLifecycle, controller *activeController) {
	if lifecycle == nil || controller == nil {
		return
	}
	lifecycle.controller = controller
	controller.SetOnLost(func(error) {
		lifecycle.cancel()
		lifecycle.gate.closeAdmission()
		if lifecycle.renewal != nil {
			lifecycle.renewal.Stop()
		}
		m.mu.Lock()
		if m.fuseLifecycles[lifecycle.sandboxID] == lifecycle {
			delete(m.fuseLifecycles, lifecycle.sandboxID)
			delete(m.operationGates, lifecycle.sandboxID)
			delete(m.sandboxes, lifecycle.sandboxID)
		}
		m.mu.Unlock()
	})
}

func (m *Manager) bindSyncController(lifecycle *syncSandboxLifecycle, controller *activeController) {
	if lifecycle == nil || controller == nil {
		return
	}
	lifecycle.controller = controller
	controller.SetOnLost(func(error) {
		lifecycle.gate.closeAdmission()
		if lifecycle.renewal != nil {
			lifecycle.renewal.Stop()
		}
		m.mu.Lock()
		if m.syncLifecycles[lifecycle.sandboxID] == lifecycle {
			delete(m.syncLifecycles, lifecycle.sandboxID)
			delete(m.operationGates, lifecycle.sandboxID)
			delete(m.workspaces, lifecycle.sandboxID)
			delete(m.sandboxes, lifecycle.sandboxID)
		}
		m.mu.Unlock()
	})
}
