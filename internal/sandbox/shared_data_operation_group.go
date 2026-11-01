package sandbox

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/google/uuid"
)

type sharedDataOperationGroup struct {
	mu                    sync.Mutex
	renewalMu             sync.Mutex
	registry              *sharedDataOperations
	id                    string
	ready, done           chan struct{}
	wake                  chan struct{}
	ctx                   context.Context
	cancel                context.CancelCauseFunc
	lease                 state.ActiveDataLease
	runtimeID, runtimeUID string
	generation            int64
	deadline, idleSince   time.Time
	refs                  int
	closed, lost          bool
	err                   error
}

func (g *sharedDataOperationGroup) signal() {
	select {
	case g.wake <- struct{}{}:
	default:
	}
}
func (g *sharedDataOperationGroup) failLocked(err error) {
	g.closed = true
	g.lost = true
	g.err = errors.Join(ErrSandboxNotReady, err)
	g.cancel(g.err)
}
func (g *sharedDataOperationGroup) drain() { g.mu.Lock(); g.closed = true; g.mu.Unlock(); g.signal() }
func (g *sharedDataOperationGroup) validIdentity(v *state.ActiveDataView) bool {
	return v != nil && v.Record != nil && v.Record.Generation == g.generation && v.Record.RuntimeID == g.runtimeID && v.Record.RuntimeUID == g.runtimeUID && v.Lease.LeaseID == g.lease.LeaseID && v.Lease.Operation.Token == g.lease.Operation.Token
}
func (g *sharedDataOperationGroup) margin(ttl time.Duration) time.Duration {
	return min(time.Second, ttl/10)
}

func (g *sharedDataOperationGroup) create() {
	r := g.registry
	started := r.options.now()
	v, err := r.capability.OpenDataOperation(r.ctx, g.id, r.instance+"/"+uuid.NewString(), r.options.ttl)
	g.mu.Lock()
	if err == nil && (v == nil || v.Record == nil || v.Lease.LeaseID == 0) {
		err = state.ErrActiveSandboxStaleToken
	}
	if err != nil {
		g.failLocked(err)
	} else {
		g.lease = v.Lease
		g.generation = v.Record.Generation
		g.runtimeID = v.Record.RuntimeID
		g.runtimeUID = v.Record.RuntimeUID
		g.deadline = started.Add(r.options.ttl - g.margin(r.options.ttl))
		g.idleSince = r.options.now()
	}
	close(g.ready)
	g.mu.Unlock()
	if err != nil {
		r.remove(g)
		close(g.done)
		return
	}
	g.run()
}

func (g *sharedDataOperationGroup) acquire(ctx context.Context) (*state.ActiveSandboxRecord, context.Context, func(), error) {
	g.mu.Lock()
	if !g.lost && !g.registry.options.now().Before(g.deadline) {
		g.failLocked(state.ErrActiveSandboxLeaseExpired)
	}
	if g.closed || g.err != nil {
		err := g.err
		if err == nil {
			err = ErrSandboxNotReady
		}
		g.mu.Unlock()
		g.signal()
		return nil, nil, nil, err
	}
	g.refs++
	lease := g.lease
	g.mu.Unlock()
	g.signal()
	var once sync.Once
	base := context.WithValue(g.ctx, activeOperationContextKey{}, lease.Operation)
	deadlineCancel := func() {}
	if deadline, ok := ctx.Deadline(); ok {
		base, deadlineCancel = context.WithDeadline(base, deadline)
	}
	child, cancel := context.WithCancelCause(base)
	requestCtx := sharedDataRequestContext{Context: child, values: ctx}
	stopPropagation := context.AfterFunc(ctx, func() { cancel(context.Cause(ctx)) })
	release := func() {
		once.Do(func() {
			stopPropagation()
			deadlineCancel()
			cancel(nil)
			g.mu.Lock()
			g.refs--
			if g.refs == 0 {
				g.idleSince = g.registry.options.now()
			}
			g.mu.Unlock()
			g.signal()
		})
	}
	view, err := g.registry.capability.ReadDataOperation(requestCtx, lease, true)
	g.mu.Lock()
	if err != nil {
		if errors.Is(err, state.ErrActiveSandboxAdmissionClosed) {
			g.closed = true
		} else if ctx.Err() == nil && context.Cause(g.ctx) == nil {
			g.failLocked(err)
		}
	} else if !g.validIdentity(view) {
		err = ErrSandboxNotReady
		g.failLocked(state.ErrActiveSandboxStaleToken)
	}
	if !g.lost && !g.registry.options.now().Before(g.deadline) {
		g.failLocked(state.ErrActiveSandboxLeaseExpired)
	}
	if g.lost {
		err = g.err
	}
	g.mu.Unlock()
	if err != nil {
		release()
		return nil, nil, nil, errors.Join(ErrSandboxNotReady, err)
	}
	// Recheck group shutdown before allowing this request to reach the runtime.
	if cause := context.Cause(g.ctx); cause != nil {
		cancel(cause)
		release()
		return nil, nil, nil, cause
	}
	if cause := context.Cause(ctx); cause != nil {
		cancel(cause)
		release()
		return nil, nil, nil, cause
	}
	return view.Record, requestCtx, release, nil
}

func (g *sharedDataOperationGroup) renew() {
	g.renewalMu.Lock()
	defer g.renewalMu.Unlock()
	g.mu.Lock()
	if g.lost || !g.registry.options.now().Before(g.deadline) {
		if !g.lost {
			g.failLocked(state.ErrActiveSandboxLeaseExpired)
		}
		g.mu.Unlock()
		g.signal()
		return
	}
	lease := g.lease
	g.mu.Unlock()
	started := g.registry.options.now()
	view, ttl, err := g.registry.capability.KeepAliveDataOperation(g.registry.ctx, lease)
	g.mu.Lock()
	if err != nil || ttl <= 0 || !g.validIdentity(view) {
		g.failLocked(errors.Join(err, state.ErrActiveSandboxStaleToken))
	} else {
		deadline := started.Add(ttl - g.margin(ttl))
		// A late response cannot revive a protection whose previous confirmed
		// deadline passed while the call was in flight.
		if g.lost || !g.registry.options.now().Before(g.deadline) || !g.registry.options.now().Before(deadline) {
			g.failLocked(state.ErrActiveSandboxLeaseExpired)
		} else {
			g.deadline = deadline
			g.lease = view.Lease
			if view.Record.Phase != state.ActiveSandboxActive {
				g.closed = true
			}
		}
	}
	g.mu.Unlock()
	g.signal()
}

func (g *sharedDataOperationGroup) run() {
	defer close(g.done)
	defer g.registry.remove(g)
	r := g.registry
	interval := r.options.renewInterval
	interval = interval - interval/10 + time.Duration(rand.Int64N(int64(interval/5)+1))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	var notifications <-chan state.ActiveChangeBatch
	if r.hub != nil {
		var unsubscribe func()
		notifications, unsubscribe = r.hub.Subscribe(g.id, 0)
		defer unsubscribe()
	}
	for {
		g.mu.Lock()
		now := r.options.now()
		if !g.lost && !now.Before(g.deadline) {
			g.failLocked(state.ErrActiveSandboxLeaseExpired)
		}
		if !g.closed && g.refs == 0 && !now.Before(g.idleSince.Add(r.options.idle)) {
			g.closed = true
		}
		if g.closed && g.refs == 0 {
			lease := g.lease
			g.mu.Unlock()
			// KeepAlive cannot recreate a deleted key. Serializing completion also
			// prevents ending a participant while a renewal is still being applied.
			g.renewalMu.Lock()
			endCtx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), activeOperationEndTimeout)
			_ = r.repo.EndOperation(endCtx, lease.Operation)
			cancel()
			g.renewalMu.Unlock()
			g.cancel(nil)
			return
		}
		delay := time.Hour
		if !g.lost {
			delay = g.deadline.Sub(now)
		}
		if !g.closed && g.refs == 0 {
			delay = min(delay, g.idleSince.Add(r.options.idle).Sub(now))
		}
		g.mu.Unlock()
		timer.Reset(max(delay, time.Millisecond))
		select {
		case <-ticker.C:
			g.renew()
		case <-notifications:
			g.renew()
		case <-timer.C:
		case <-g.wake:
		}
	}
}

// Cancellation belongs to the group tree; request-scoped tracing/auth values
// remain available without changing the context package's cancellation keys.
type sharedDataRequestContext struct {
	context.Context
	values context.Context
}

func (c sharedDataRequestContext) Value(key any) any {
	if value := c.Context.Value(key); value != nil {
		return value
	}
	return c.values.Value(key)
}
