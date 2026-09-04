package sandbox

import (
	"context"
	"sync"
)

// operationGate controls admission to one published sandbox. References cover
// the complete lifetime of an operation, including returned streams.
type operationGate struct {
	mu         sync.Mutex
	open       bool
	permanent  bool
	exclusive  bool
	generation uint64
	refs       int
	drained    chan struct{}
}

func newOperationGate(open bool) *operationGate {
	g := &operationGate{open: open, drained: make(chan struct{})}
	close(g.drained)
	return g
}

// Acquire obtains one live operation reference. The returned release is safe
// to invoke more than once.
func (g *operationGate) Acquire() (func(), error) {
	if g == nil {
		return nil, ErrSandboxNotReady
	}
	g.mu.Lock()
	if !g.open || g.permanent || g.exclusive {
		g.mu.Unlock()
		return nil, ErrSandboxNotReady
	}
	if g.refs == 0 {
		g.drained = make(chan struct{})
	}
	g.refs++
	g.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.refs--
			if g.refs == 0 {
				close(g.drained)
			}
			g.mu.Unlock()
		})
	}, nil
}

// CloseAndWait permanently rejects new operations and waits for all existing
// references to end.
func (g *operationGate) CloseAndWait(ctx context.Context) error {
	if g == nil {
		return ErrSandboxNotReady
	}
	g.mu.Lock()
	g.open = false
	g.permanent = true
	g.exclusive = false
	g.generation++
	drained := g.drained
	g.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// BeginExclusive closes admission and drains earlier operations. The caller
// must resolve the returned token by reopening the same generation or closing
// it permanently.
func (g *operationGate) BeginExclusive(ctx context.Context) (*operationGateExclusive, error) {
	if g == nil {
		return nil, ErrSandboxNotReady
	}
	g.mu.Lock()
	if !g.open || g.permanent || g.exclusive {
		g.mu.Unlock()
		return nil, ErrSandboxNotReady
	}
	g.open = false
	g.exclusive = true
	g.generation++
	generation := g.generation
	drained := g.drained
	g.mu.Unlock()

	select {
	case <-drained:
		return &operationGateExclusive{gate: g, generation: generation}, nil
	case <-ctx.Done():
		g.mu.Lock()
		if g.exclusive && !g.permanent && g.generation == generation {
			g.exclusive = false
			g.open = true
		}
		g.mu.Unlock()
		return nil, ctx.Err()
	}
}

type operationGateExclusive struct {
	gate       *operationGate
	generation uint64
	once       sync.Once
}

func (t *operationGateExclusive) Reopen() error {
	if t == nil || t.gate == nil {
		return ErrSandboxNotReady
	}
	result := ErrSandboxNotReady
	t.once.Do(func() {
		g := t.gate
		g.mu.Lock()
		defer g.mu.Unlock()
		if g.exclusive && !g.permanent && g.generation == t.generation {
			g.exclusive = false
			g.open = true
			result = nil
		}
	})
	return result
}

func (t *operationGateExclusive) Close() {
	if t == nil || t.gate == nil {
		return
	}
	t.once.Do(func() {
		g := t.gate
		g.mu.Lock()
		if g.generation == t.generation {
			g.open = false
			g.exclusive = false
			g.permanent = true
			g.generation++
		}
		g.mu.Unlock()
	})
}

func (g *operationGate) isOpen() bool {
	if g == nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open && !g.permanent && !g.exclusive
}
