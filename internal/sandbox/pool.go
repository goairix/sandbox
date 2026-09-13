package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
)

// PoolConfig configures the container pool.
type PoolConfig struct {
	MinSize         int
	MaxSize         int
	Image           string
	Memory          string
	MemoryRequest   string
	CPU             string
	CPURequest      string
	Disk            string
	TmpDisk         string
	PidLimit        int
	SeccompProfile  string
	RuntimeContract string
}

// Pool manages a pool of warm containers.
type Pool struct {
	runtime runtime.Runtime
	config  PoolConfig
	shared  *sharedOrdinaryPool

	mu           sync.Mutex
	available    []*runtime.SandboxInfo
	refilling    bool
	stopping     bool
	refillCtx    context.Context
	refillCancel context.CancelFunc
	refillWG     sync.WaitGroup
}

const (
	maxRefillRetries = 10
	maxRefillBackoff = 30 * time.Second
)

// NewPool creates a new container pool.
func NewPool(rt runtime.Runtime, cfg PoolConfig) *Pool {
	if cfg.PidLimit <= 0 {
		cfg.PidLimit = 100
	}
	if cfg.RuntimeContract == "" {
		if provider, ok := rt.(runtime.WarmPoolContractProvider); ok {
			cfg.RuntimeContract = provider.WarmPoolContract()
		}
	}
	refillCtx, refillCancel := context.WithCancel(context.Background())
	return &Pool{
		runtime:      rt,
		config:       cfg,
		refillCtx:    refillCtx,
		refillCancel: refillCancel,
	}
}

// EnableShared switches this pool to release-scoped durable inventory. It is
// intended for multi-replica Kubernetes deployments whose Pods outlive an API
// process. Local Docker/test pools keep the historical in-memory behavior.
func (p *Pool) EnableShared(store state.AtomicStore, scope string) {
	if store == nil || scope == "" {
		return
	}
	p.shared = newSharedOrdinaryPool(p, store, scope)
}

func (p *Pool) Shared() bool { return p.shared != nil }

// Start registers this API replica as an owner of its shared pool
// fingerprint. Compatible inventory survives even a full API restart.
func (p *Pool) Start(ctx context.Context) error {
	if p.shared == nil {
		return nil
	}
	return p.shared.start(ctx)
}

// WarmUp fills the pool to MinSize.
func (p *Pool) WarmUp(ctx context.Context) error {
	if p.shared != nil {
		return p.shared.warmUp(ctx)
	}
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
	return nil
}

// Acquire takes a warm container from the pool. If none available, creates one on-demand.
// Stale containers (e.g. removed by Docker restart) are automatically discarded.
// Records pool hit metrics: hit=true means a warm container was successfully reused;
// hit=false means an on-demand container was created.
func (p *Pool) Acquire(ctx context.Context) (*runtime.SandboxInfo, error) {
	if p.shared != nil {
		return p.shared.acquire(ctx)
	}
	for {
		p.mu.Lock()
		if len(p.available) == 0 {
			p.mu.Unlock()
			break
		}
		info := p.available[0]
		p.available = p.available[1:]
		p.mu.Unlock()

		// Verify container is still alive and running
		got, err := p.runtime.GetSandbox(ctx, info.RuntimeID)
		if err == nil && got != nil && got.State == "running" {
			metrics.SandboxPoolSize.Add(ctx, -1)
			metrics.RecordPoolAcquire(ctx, true)
			p.scheduleRefill()
			return info, nil
		}

		// Stale container, discard and try next
		logger.Info(ctx, "pool: discarding stale container",
			logger.AddField("runtime_id", info.RuntimeID),
		)
		_ = p.runtime.RemoveSandbox(ctx, info.RuntimeID)
		metrics.SandboxPoolSize.Add(ctx, -1)
	}

	// No healthy warm containers, create on-demand
	metrics.RecordPoolAcquire(ctx, false)
	return p.createWarm(ctx)
}

// ConfirmAcquired retires the durable claim after the Manager has atomically
// migrated the Pod from pool identity to its user-visible sandbox identity.
func (p *Pool) ConfirmAcquired(ctx context.Context, info *runtime.SandboxInfo) error {
	if p.shared == nil || info == nil {
		return nil
	}
	return p.shared.confirmAcquired(ctx, info.RuntimeID, info.RuntimeUID)
}

// Release destroys a used container (containers are single-use for security).
func (p *Pool) Release(ctx context.Context, id string) {
	_ = p.runtime.RemoveSandbox(ctx, id)

	// Trigger async refill
	p.scheduleRefill()
}

// Size returns the number of available warm containers.
func (p *Pool) Size() int {
	if p.shared != nil {
		return p.shared.size()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.available)
}

// NotifyRemoved notifies the pool that a container from this pool was removed,
// triggering an async refill if needed.
func (p *Pool) NotifyRemoved() {
	p.scheduleRefill()
}

// Reconcile repairs shared durable inventory and removes only pool runtimes
// that have no durable owner. Local pools keep the historical manager-owned
// startup cleanup path.
func (p *Pool) Reconcile(ctx context.Context, protectedRuntimeIDs map[string]struct{}) error {
	if p.shared == nil {
		return nil
	}
	return p.shared.reconcile(ctx, protectedRuntimeIDs)
}

// Drain stops this API's pool controller. Local warm containers are destroyed;
// shared Kubernetes inventory is preserved until contract retirement or an
// explicit DrainRelease.
func (p *Pool) Drain(ctx context.Context) {
	if p.shared != nil {
		p.shared.stop()
		return
	}
	p.mu.Lock()
	if !p.stopping {
		p.stopping = true
		p.refillCancel()
	}
	p.mu.Unlock()

	// Refill owns creation of new warm runtimes. Wait for every scheduled
	// refill to observe cancellation before taking the final inventory so none
	// can publish another runtime after the drain snapshot.
	p.refillWG.Wait()

	p.mu.Lock()
	items := make([]*runtime.SandboxInfo, len(p.available))
	copy(items, p.available)
	p.available = nil
	p.mu.Unlock()

	for _, info := range items {
		_ = p.runtime.RemoveSandbox(ctx, info.RuntimeID)
	}
}

// DrainRelease removes release-wide shared inventory. It must only be called
// after every serving API replica has been stopped.
func (p *Pool) DrainRelease(ctx context.Context) error {
	if p.shared != nil {
		return p.shared.drainRelease(ctx)
	}
	p.Drain(ctx)
	return nil
}

func (p *Pool) scheduleRefill() {
	if p.shared != nil {
		p.shared.scheduleRefill()
		return
	}
	p.mu.Lock()
	if p.stopping {
		p.mu.Unlock()
		return
	}
	ctx := p.refillCtx
	p.refillWG.Add(1)
	p.mu.Unlock()

	go func() {
		defer p.refillWG.Done()
		p.refillIfNeeded(ctx)
	}()
}

func (p *Pool) createWarm(ctx context.Context) (*runtime.SandboxInfo, error) {
	return p.createWarmWithLabels(ctx, map[string]string{"sandbox.pool": "true"})
}

func (p *Pool) createWarmWithLabels(ctx context.Context, labels map[string]string) (*runtime.SandboxInfo, error) {
	id := fmt.Sprintf("sandbox-pool-%s", randSuffix(randSuffixLen))

	spec := runtime.SandboxSpec{
		ID:             id,
		Image:          p.config.Image,
		Labels:         labels,
		ReadOnlyRootFS: false, // warm containers need writable FS for dependency install
		RunAsUser:      1000,
		PidLimit:       p.config.PidLimit,
		SeccompProfile: p.config.SeccompProfile,
		Memory:         p.config.Memory,
		MemoryRequest:  p.config.MemoryRequest,
		CPU:            p.config.CPU,
		CPURequest:     p.config.CPURequest,
		Disk:           p.config.Disk,
		TmpDisk:        p.config.TmpDisk,
	}

	return p.runtime.CreateSandbox(ctx, spec)
}

func (p *Pool) refillIfNeeded(ctx context.Context) {
	p.mu.Lock()
	if p.refilling || p.stopping {
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
		if p.stopping || len(p.available) >= p.config.MinSize {
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			logger.Info(ctx, "pool refill stopping: context cancelled")
			return
		default:
		}

		info, err := p.createWarm(ctx)
		if err != nil {
			consecutiveFailures++
			metrics.RecordPoolRefillFailure(ctx)
			if consecutiveFailures >= maxRefillRetries {
				logger.Error(ctx, "pool refill giving up after consecutive failures",
					logger.AddField("failures", consecutiveFailures),
					logger.ErrorField(err),
				)
				return
			}
			backoff := time.Duration(1<<(consecutiveFailures-1)) * time.Second
			if backoff > maxRefillBackoff {
				backoff = maxRefillBackoff
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				logger.Info(ctx, "pool refill stopping during backoff: context cancelled")
				return
			}
			continue
		}
		consecutiveFailures = 0
		p.mu.Lock()
		if p.stopping {
			// Publish the just-created runtime into the final drain inventory.
			// Drain waits for this refill before taking that inventory.
			p.available = append(p.available, info)
			p.mu.Unlock()
			return
		}
		if len(p.available) < p.config.MaxSize {
			p.available = append(p.available, info)
			p.mu.Unlock()
			metrics.SandboxPoolSize.Add(context.Background(), 1)
		} else {
			// Pool is full, discard
			runtimeID := info.RuntimeID
			p.mu.Unlock()
			go func() { _ = p.runtime.RemoveSandbox(context.Background(), runtimeID) }()
		}
	}
}
