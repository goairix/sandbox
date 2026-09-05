package sandbox

import (
	"context"
	"io"
	"sync"

	"github.com/goairix/sandbox/internal/runtime"
)

// operationGate controls admission to one published sandbox. References cover
// the complete lifetime of an operation, including returned streams.
type operationGate struct {
	mu           sync.Mutex
	exclusiveUse sync.Mutex
	open         bool
	permanent    bool
	exclusive    bool
	generation   uint64
	refs         int
	drained      chan struct{}
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
	drained := g.closeAdmission()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// closeAdmission permanently rejects new work immediately without waiting for
// already-admitted operations. Teardown uses the returned channel to drain.
func (g *operationGate) closeAdmission() <-chan struct{} {
	if g == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	g.mu.Lock()
	if !g.permanent {
		g.open = false
		g.permanent = true
		g.exclusive = false
		g.generation++
	}
	drained := g.drained
	g.mu.Unlock()

	// An exclusive workspace transition is not counted in refs. Wait until its
	// current side effect completes before teardown removes the runtime/session.
	// Admission only takes mu, so new requests still fail immediately.
	g.exclusiveUse.Lock()
	g.exclusiveUse.Unlock()
	return drained
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
		g.mu.Lock()
		valid := g.exclusive && !g.permanent && g.generation == generation
		g.mu.Unlock()
		if !valid {
			return nil, ErrSandboxNotReady
		}
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

func (t *operationGateExclusive) withOwnership(fn func() error) error {
	if t == nil || t.gate == nil {
		return ErrSandboxNotReady
	}
	g := t.gate
	g.exclusiveUse.Lock()
	defer g.exclusiveUse.Unlock()
	g.mu.Lock()
	valid := g.exclusive && !g.permanent && g.generation == t.generation
	g.mu.Unlock()
	if !valid {
		return ErrSandboxNotReady
	}
	return fn()
}

// commit runs the final state transition while teardown is excluded, then
// atomically reopens admission. A failed commit permanently closes the gate.
func (t *operationGateExclusive) commit(fn func() error) error {
	if t == nil || t.gate == nil {
		return ErrSandboxNotReady
	}
	result := ErrSandboxNotReady
	t.once.Do(func() {
		g := t.gate
		g.exclusiveUse.Lock()
		defer g.exclusiveUse.Unlock()

		g.mu.Lock()
		if !g.exclusive || g.permanent || g.generation != t.generation {
			g.mu.Unlock()
			return
		}
		g.mu.Unlock()

		if err := fn(); err != nil {
			g.mu.Lock()
			g.open = false
			g.exclusive = false
			g.permanent = true
			g.generation++
			g.mu.Unlock()
			result = err
			return
		}

		g.mu.Lock()
		defer g.mu.Unlock()
		if !g.exclusive || g.permanent || g.generation != t.generation {
			return
		}
		g.exclusive = false
		g.open = true
		result = nil
	})
	return result
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

type gatedReadCloser struct {
	reader  io.ReadCloser
	release func()
	once    sync.Once
}

func (r *gatedReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil {
		r.releaseOnce()
	}
	return n, err
}

func (r *gatedReadCloser) Close() error {
	err := r.reader.Close()
	r.releaseOnce()
	return err
}

func (r *gatedReadCloser) releaseOnce() {
	r.once.Do(r.release)
}

func holdGateForFileContents(files []runtime.FileContent, release func()) []runtime.FileContent {
	count := 0
	for i := range files {
		if files[i].Content != nil {
			count++
		}
	}
	if count == 0 {
		release()
		return files
	}
	var mu sync.Mutex
	remaining := count
	releaseOne := func() {
		mu.Lock()
		remaining--
		last := remaining == 0
		mu.Unlock()
		if last {
			release()
		}
	}
	for i := range files {
		if files[i].Content != nil {
			files[i].Content = &gatedReadCloser{reader: files[i].Content, release: releaseOne}
		}
	}
	return files
}
