package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/storage/state"
)

// sharedFUSEPool records release-scoped generation membership rather than
// shell ownership. Its registry persists; live owner leases fence rollouts.
type sharedFUSEPool struct {
	pool       *FUSEPool
	store      state.AtomicStore
	base       string
	mu         sync.Mutex
	registered bool
	wg         sync.WaitGroup
	healthy    atomic.Bool
}

func newSharedFUSEPool(pool *FUSEPool) *sharedFUSEPool {
	return &sharedFUSEPool{pool: pool, store: pool.config.InventoryStore,
		base: "fusepool-rollout:v1:" + shortDigest([]byte(pool.config.InventoryScope), 16) + ":"}
}

func (p *sharedFUSEPool) ownerKey() string {
	return p.base + p.pool.poolKey + ":owner:" + p.pool.config.MaintainerToken
}

func (p *sharedFUSEPool) start(ctx context.Context) error {
	for {
		err := withPoolLock(ctx, p.store, p.base+"rollout-lock", func(ctx context.Context) error {
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.registered {
				return nil
			}
			token := []byte(p.pool.config.MaintainerToken)
			if err := p.store.Set(ctx, p.base+"generation:"+p.pool.poolKey, []byte(p.pool.poolKey), 0); err != nil {
				return err
			}
			ok, err := p.store.SetNX(ctx, p.ownerKey(), token, ordinaryPoolLockTTL)
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("FUSE pool owner token collision")
			}
			p.registered = true
			p.healthy.Store(true)
			p.wg.Add(1)
			go p.renewOwner()
			return nil
		})
		if !errors.Is(err, errOrdinaryPoolLockBusy) {
			return err
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *sharedFUSEPool) renewOwner() {
	defer p.wg.Done()
	ticker := time.NewTicker(ordinaryPoolLockTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-p.pool.loopCtx.Done():
			return
		case <-ticker.C:
			if err := p.refreshOwner(p.pool.loopCtx); err != nil {
				logger.Error(context.Background(), "shared FUSE pool owner lease lost", logger.ErrorField(err))
			}
		}
	}
}

func (p *sharedFUSEPool) refreshOwner(ctx context.Context) error {
	err := refreshPoolOwner(ctx, p.store, p.ownerKey(), []byte(p.pool.config.MaintainerToken), p.base+"rollout-lock", p.base+"generation:"+p.pool.poolKey, p.pool.poolKey)
	p.healthy.Store(err == nil)
	return err
}

func (p *sharedFUSEPool) stop(ctx context.Context) error {
	p.wg.Wait() // Stop has already cancelled loopCtx and joined operations.
	p.mu.Lock()
	registered := p.registered
	p.mu.Unlock()
	if !registered {
		return nil
	}
	_, err := p.store.CompareAndDelete(ctx, p.ownerKey(), []byte(p.pool.config.MaintainerToken))
	return err
}

func (p *sharedFUSEPool) drainState(ctx context.Context) error {
	keys, err := p.store.Keys(ctx, p.base+"*")
	if err != nil {
		return err
	}
	var result error
	for _, key := range keys {
		result = errors.Join(result, p.store.Delete(ctx, key))
	}
	return result
}

// retire is controller-only. Scope locking fences registration and an old
// per-key refill lease fences a controller unwinding after owner expiry.
func (p *sharedFUSEPool) retire(ctx context.Context) error {
	return withPoolLock(ctx, p.store, p.base+"rollout-lock", func(ctx context.Context) error {
		own, err := p.store.Get(ctx, p.ownerKey())
		if err != nil {
			return err
		}
		if string(own) != p.pool.config.MaintainerToken {
			return errors.New("FUSE rollout owner lease is absent")
		}
		current, err := p.pool.repo.ListByPoolKey(ctx, p.pool.poolKey)
		if err != nil {
			return err
		}
		prepared := 0
		for _, r := range current {
			if r.State == state.FUSEPoolPrepared {
				prepared++
			}
		}
		if prepared < p.pool.config.MinSize {
			return nil
		}
		keys, err := p.store.Keys(ctx, p.base+"generation:*")
		if err != nil {
			return err
		}
		for _, key := range keys {
			oldKey := strings.TrimPrefix(key, p.base+"generation:")
			if oldKey == p.pool.poolKey {
				continue
			}
			owners, err := p.store.Keys(ctx, p.base+oldKey+":owner:*")
			if err != nil {
				return err
			}
			if len(owners) != 0 {
				continue
			}
			if err := p.retireGeneration(ctx, oldKey); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *sharedFUSEPool) retireGeneration(ctx context.Context, poolKey string) (result error) {
	old := &FUSEPool{runtime: p.pool.runtime, repo: p.pool.repo, config: p.pool.config,
		spec: p.pool.spec, poolKey: poolKey}
	token := p.pool.config.MaintainerToken + ":retire:" + randSuffix(24)
	locked, err := old.repo.TryRefillLock(ctx, poolKey, token, old.refillLockTTL())
	if err != nil || !locked {
		return err
	}
	defer func() { result = errors.Join(result, old.unlockRefill(ctx, token)) }()
	lease := old.startRefillLease(ctx, token)
	defer lease.stop()
	records, err := old.repo.ListByPoolKey(lease.ctx, poolKey)
	if err != nil {
		return err
	}
	now, err := old.repo.ServerTime(lease.ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		// Business reservations/bindings/consumed records never become cleanup
		// merely because their contract is no longer current.
		eligible := record.State == state.FUSEPoolPrepared ||
			(record.State == state.FUSEPoolPreparing && !record.PrepareUntil.After(now)) ||
			(record.State == state.FUSEPoolCleanup && !record.CleanupUntil.After(now))
		if !eligible {
			continue
		}
		disposition, err := old.guard(lease.ctx, record)
		if err != nil {
			result = errors.Join(result, fmt.Errorf("inspect obsolete FUSE record %s: %w", record.PreparationID, err))
			continue
		}
		if disposition != FUSEPoolPristine && disposition != FUSEPoolAbandoned {
			continue
		}
		if err := old.claimAndDestroy(lease.ctx, record); err != nil && !isStalePoolMutation(err) {
			return fmt.Errorf("retire obsolete FUSE pool %s: %w", poolKey, err)
		}
	}
	remaining, err := old.repo.ListByPoolKey(lease.ctx, poolKey)
	if err != nil {
		return err
	}
	if len(remaining) == 0 {
		_, err = p.store.CompareAndDelete(lease.ctx, p.base+"generation:"+poolKey, []byte(poolKey))
	}
	return errors.Join(result, err)
}
