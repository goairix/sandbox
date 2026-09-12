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
	activePublishingRecoveryGrace  = time.Minute
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
	c.mu.Lock()
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

// runActiveLifecycleCoordinator discovers durable workspace lifecycles. The
// scan is deliberately off the request path and jittered per replica; the
// controller lease remains the final single-writer arbitration mechanism.
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
			if err := m.reconcileActiveLifecycleControllers(m.controlCtx); err != nil && !errors.Is(err, context.Canceled) {
				// The next bounded pass retries. Existing controllers keep renewing
				// independently, so a transient scan failure does not interrupt them.
			}
			timer.Reset(activeLifecycleScanInterval + lifecycleScanDelay(m.config.InstanceID))
		}
	}
}

func lifecycleScanDelay(instanceID string) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(instanceID))
	return time.Duration(h.Sum32()%1000) * time.Millisecond
}

func (m *Manager) reconcileActiveLifecycleControllers(ctx context.Context) error {
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

func (m *Manager) reconcileActiveLifecycle(ctx context.Context, record *state.ActiveSandboxRecord) error {
	if record == nil {
		return nil
	}
	if record.Phase == state.ActiveSandboxPublishing {
		if time.Since(record.UpdatedAt) < activePublishingRecoveryGrace {
			return nil
		}
		return m.destroyDistributedSandbox(ctx, record.SandboxID)
	}
	if record.Phase != state.ActiveSandboxActive &&
		record.Phase != state.ActiveSandboxDestroying && record.Phase != state.ActiveSandboxCleanupPending {
		return nil
	}
	sb, err := decodeActiveSandboxPhase(record, record.SandboxID,
		state.ActiveSandboxActive, state.ActiveSandboxDestroying, state.ActiveSandboxCleanupPending)
	if err != nil || sb.Workspace == nil ||
		(sb.Workspace.MountType != WorkspaceMountFUSE && sb.Workspace.Owner.Generation <= 0) {
		return err
	}

	m.mu.RLock()
	fuseLifecycle := m.fuseLifecycles[sb.ID]
	syncLifecycle := m.syncLifecycles[sb.ID]
	m.mu.RUnlock()
	if fuseLifecycle != nil || syncLifecycle != nil {
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
			_ = controller.Stop(context.WithoutCancel(ctx))
		}
	}()

	switch sb.Workspace.MountType {
	case WorkspaceMountFUSE:
		err = m.restoreFUSESandboxWithController(ctx, sb, controller)
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
