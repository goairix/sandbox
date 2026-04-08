package sandbox

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
)

// PoolConfig configures the container pool.
type PoolConfig struct {
	MinSize int
	MaxSize int
	Image   string
}

// Pool manages a pool of warm containers.
type Pool struct {
	runtime runtime.Runtime
	config  PoolConfig

	mu        sync.Mutex
	available []*runtime.SandboxInfo
	refilling bool
}

const (
	maxRefillRetries = 10
	maxRefillBackoff = 30 * time.Second
)

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
// Stale containers (e.g. removed by Docker restart) are automatically discarded.
func (p *Pool) Acquire(ctx context.Context) (*runtime.SandboxInfo, error) {
	for {
		p.mu.Lock()
		if len(p.available) == 0 {
			p.mu.Unlock()
			break
		}
		info := p.available[0]
		p.available = p.available[1:]
		p.mu.Unlock()

		// Verify container is still alive
		got, err := p.runtime.GetSandbox(ctx, info.RuntimeID)
		if err == nil && got != nil && got.State != "exited" && got.State != "dead" && got.State != "" {
			go p.refillIfNeeded(context.Background())
			return info, nil
		}

		// Stale container, discard and try next
		log.Printf("pool: discarding stale container %s", info.RuntimeID)
		_ = p.runtime.RemoveSandbox(ctx, info.RuntimeID)
	}

	// No healthy warm containers, create on-demand
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

// NotifyRemoved notifies the pool that a container from this pool was removed,
// triggering an async refill if needed.
func (p *Pool) NotifyRemoved() {
	go p.refillIfNeeded(context.Background())
}

// Drain destroys all warm containers in the pool.
func (p *Pool) Drain(ctx context.Context) {
	p.mu.Lock()
	items := make([]*runtime.SandboxInfo, len(p.available))
	copy(items, p.available)
	p.available = nil
	p.mu.Unlock()

	for _, info := range items {
		_ = p.runtime.RemoveSandbox(ctx, info.RuntimeID)
	}
}

func (p *Pool) createWarm(ctx context.Context) (*runtime.SandboxInfo, error) {
	id := fmt.Sprintf("sandbox-pool-%s", randSuffix(randSuffixLen))

	spec := runtime.SandboxSpec{
		ID:    id,
		Image: p.config.Image,
		Labels: map[string]string{
			"sandbox.pool": "true",
		},
		ReadOnlyRootFS: false, // warm containers need writable FS for dependency install
		RunAsUser:      1000,
		PidLimit:       100,
	}

	return p.runtime.CreateSandbox(ctx, spec)
}

func (p *Pool) refillIfNeeded(ctx context.Context) {
	p.mu.Lock()
	if p.refilling {
		p.mu.Unlock()
		return
	}
	if len(p.available) >= p.config.MinSize {
		p.mu.Unlock()
		return
	}
	p.refilling = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.refilling = false
		p.mu.Unlock()
	}()

	consecutiveFailures := 0

	for {
		p.mu.Lock()
		if len(p.available) >= p.config.MinSize {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			log.Printf("refillIfNeeded: context cancelled, stopping refill")
			return
		default:
		}

		info, err := p.createWarm(ctx)
		if err != nil {
			consecutiveFailures++
			if consecutiveFailures >= maxRefillRetries {
				log.Printf("refillIfNeeded: giving up after %d consecutive failures", consecutiveFailures)
				return
			}
			backoff := time.Duration(1<<(consecutiveFailures-1)) * time.Second
			if backoff > maxRefillBackoff {
				backoff = maxRefillBackoff
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				log.Printf("refillIfNeeded: context cancelled during backoff")
				return
			}
			continue
		}
		consecutiveFailures = 0
		p.mu.Lock()
		if len(p.available) < p.config.MaxSize {
			p.available = append(p.available, info)
		} else {
			// Pool is full, discard
			runtimeID := info.RuntimeID
			go func() { _ = p.runtime.RemoveSandbox(context.Background(), runtimeID) }()
		}
		p.mu.Unlock()
	}
}
