package sandbox

import (
	"context"
	"fmt"
	"sync"

	"github.com/goairix/sandbox/internal/runtime"
)

// PoolConfig configures a container pool for a specific language.
type PoolConfig struct {
	Language Language
	MinSize  int
	MaxSize  int
	Image    string
}

// Pool manages a pool of warm containers for a specific language.
type Pool struct {
	runtime runtime.Runtime
	config  PoolConfig

	mu        sync.Mutex
	available []*runtime.SandboxInfo
	counter   int
}

// NewPool creates a new container pool.
func NewPool(rt runtime.Runtime, cfg PoolConfig) *Pool {
	return &Pool{
		runtime: rt,
		config:  cfg,
	}
}

// WarmUp fills the pool to MinSize.
func (p *Pool) WarmUp(ctx context.Context) {
	p.mu.Lock()
	need := p.config.MinSize - len(p.available)
	p.mu.Unlock()

	for i := 0; i < need; i++ {
		info, err := p.createWarm(ctx)
		if err != nil {
			continue
		}
		p.mu.Lock()
		p.available = append(p.available, info)
		p.mu.Unlock()
	}
}

// Acquire takes a warm container from the pool. If none available, creates one on-demand.
func (p *Pool) Acquire(ctx context.Context) (*runtime.SandboxInfo, error) {
	p.mu.Lock()
	if len(p.available) > 0 {
		info := p.available[0]
		p.available = p.available[1:]
		p.mu.Unlock()

		// Trigger async refill if below min
		go p.refillIfNeeded(context.Background())

		return info, nil
	}
	p.mu.Unlock()

	// No warm containers, create on-demand
	return p.createWarm(ctx)
}

// Release destroys a used container (containers are single-use for security).
func (p *Pool) Release(ctx context.Context, id string) {
	_ = p.runtime.RemoveSandbox(ctx, id)

	// Trigger async refill
	go p.refillIfNeeded(context.Background())
}

// Size returns the number of available warm containers.
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.available)
}

// Drain destroys all warm containers in the pool.
func (p *Pool) Drain(ctx context.Context) {
	p.mu.Lock()
	items := make([]*runtime.SandboxInfo, len(p.available))
	copy(items, p.available)
	p.available = nil
	p.mu.Unlock()

	for _, info := range items {
		_ = p.runtime.RemoveSandbox(ctx, info.ID)
	}
}

func (p *Pool) createWarm(ctx context.Context) (*runtime.SandboxInfo, error) {
	p.mu.Lock()
	p.counter++
	id := fmt.Sprintf("pool-%s-%d", p.config.Language, p.counter)
	p.mu.Unlock()

	spec := runtime.SandboxSpec{
		ID:    id,
		Image: p.config.Image,
		Labels: map[string]string{
			"sandbox.pool":     "true",
			"sandbox.language": string(p.config.Language),
		},
		ReadOnlyRootFS: false, // warm containers need writable FS for dependency install
		RunAsUser:      1000,
		PidLimit:       100,
	}

	return p.runtime.CreateSandbox(ctx, spec)
}

func (p *Pool) refillIfNeeded(ctx context.Context) {
	p.mu.Lock()
	need := p.config.MinSize - len(p.available)
	if need <= 0 {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()

	for i := 0; i < need; i++ {
		info, err := p.createWarm(ctx)
		if err != nil {
			continue
		}
		p.mu.Lock()
		if len(p.available) < p.config.MaxSize {
			p.available = append(p.available, info)
		} else {
			// Pool is full, discard
			go func() { _ = p.runtime.RemoveSandbox(context.Background(), info.ID) }()
		}
		p.mu.Unlock()
	}
}
