package sandbox

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/google/uuid"
)

var errSharedDataCapacity = errors.New("shared data operation registry is full")

type sharedDataOptions struct {
	ttl, renewInterval, idle time.Duration
	maxGroups                int
	now                      func() time.Time
}

func (o sharedDataOptions) defaults() sharedDataOptions {
	if o.ttl <= 0 {
		o.ttl = 30 * time.Second
	}
	if o.renewInterval <= 0 {
		o.renewInterval = 10 * time.Second
	}
	if o.idle <= 0 {
		o.idle = 15 * time.Second
	}
	if o.maxGroups <= 0 {
		o.maxGroups = 1024
	}
	if o.now == nil {
		o.now = time.Now
	}
	return o
}

type sharedDataOperations struct {
	mu         sync.Mutex
	groups     map[string]*sharedDataOperationGroup
	closed     bool
	ctx        context.Context
	cancel     context.CancelFunc
	repo       state.ActiveSandboxRepository
	capability state.ActiveDataOperationRepository
	hub        *activeChangeHub
	instance   string
	options    sharedDataOptions
}

func newSharedDataOperations(ctx context.Context, repo state.ActiveSandboxRepository, capability state.ActiveDataOperationRepository, hub *activeChangeHub, instanceID string, options sharedDataOptions) *sharedDataOperations {
	groupCtx, cancel := context.WithCancel(ctx)
	return &sharedDataOperations{groups: make(map[string]*sharedDataOperationGroup), ctx: groupCtx, cancel: cancel, repo: repo, capability: capability, hub: hub, instance: instanceID + "/" + uuid.NewString(), options: options.defaults()}
}

func (r *sharedDataOperations) Acquire(ctx context.Context, id string) (*state.ActiveSandboxRecord, context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, nil, nil, ErrSandboxNotReady
	}
	g := r.groups[id]
	if g == nil {
		if len(r.groups) >= r.options.maxGroups {
			r.mu.Unlock()
			return nil, nil, nil, errSharedDataCapacity
		}
		groupCtx, cancel := context.WithCancelCause(r.ctx)
		g = &sharedDataOperationGroup{registry: r, id: id, ready: make(chan struct{}), done: make(chan struct{}), wake: make(chan struct{}, 1), ctx: groupCtx, cancel: cancel}
		r.groups[id] = g
		go g.create()
	}
	r.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	case <-g.ready:
	}
	return g.acquire(ctx)
}

func (r *sharedDataOperations) remove(g *sharedDataOperationGroup) {
	r.mu.Lock()
	if r.groups[g.id] == g {
		delete(r.groups, g.id)
	}
	r.mu.Unlock()
}

func (r *sharedDataOperations) CloseAdmission() {
	r.mu.Lock()
	r.closed = true
	groups := make([]*sharedDataOperationGroup, 0, len(r.groups))
	for _, g := range r.groups {
		groups = append(groups, g)
	}
	r.mu.Unlock()
	for _, g := range groups {
		g.drain()
	}
}

// Stop cancels requests but keeps their participant alive until their release
// joins remote helpers. A caller deadline is not permission to delete the key.
func (r *sharedDataOperations) Stop(ctx context.Context) error {
	r.CloseAdmission()
	r.mu.Lock()
	groups := make([]*sharedDataOperationGroup, 0, len(r.groups))
	for _, g := range r.groups {
		groups = append(groups, g)
	}
	r.mu.Unlock()
	for _, g := range groups {
		g.cancel(ErrSandboxNotReady)
		g.signal()
	}
	for _, g := range groups {
		select {
		case <-g.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.cancel()
	return nil
}

func (r *sharedDataOperations) invalidate(id string, err error) {
	r.mu.Lock()
	g := r.groups[id]
	r.mu.Unlock()
	if g != nil {
		g.mu.Lock()
		g.failLocked(err)
		g.mu.Unlock()
		g.signal()
	}
}
