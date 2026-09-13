package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
)

type ordinaryPoolState string

const (
	ordinaryPoolPreparing ordinaryPoolState = "preparing"
	ordinaryPoolPrepared  ordinaryPoolState = "prepared"
	ordinaryPoolClaimed   ordinaryPoolState = "claimed"
	ordinaryPoolCleanup   ordinaryPoolState = "cleanup"

	ordinaryPoolLockTTL  = 45 * time.Second
	ordinaryPoolClaimTTL = time.Minute
	ordinaryPoolStaleTTL = 2 * time.Minute
)

var errOrdinaryPoolLockBusy = errors.New("shared ordinary pool lock is busy")

type ordinaryPoolRecord struct {
	PreparationID string            `json:"preparation_id"`
	RuntimeID     string            `json:"runtime_id,omitempty"`
	RuntimeUID    string            `json:"runtime_uid,omitempty"`
	PoolKey       string            `json:"pool_key"`
	State         ordinaryPoolState `json:"state"`
	ClaimToken    string            `json:"claim_token,omitempty"`
	ClaimUntil    time.Time         `json:"claim_until,omitempty"`
	UpdatedAt     time.Time         `json:"updated_at"`
	Revision      uint64            `json:"revision"`
}

type ordinaryPoolEntry struct {
	key    string
	raw    []byte
	record ordinaryPoolRecord
}

type sharedOrdinaryPool struct {
	pool        *Pool
	store       state.AtomicStore
	scopeDigest string
	poolKey     string
	recordBase  string
	lockKey     string
	ownerBase   string
	ownerKey    string
	ownerToken  []byte

	ctx           context.Context
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	mu            sync.Mutex
	stopping      bool
	refilling     bool
	refillPending bool

	knownSize    atomic.Int64
	ownerHealthy atomic.Bool
}

func newSharedOrdinaryPool(pool *Pool, store state.AtomicStore, scope string) *sharedOrdinaryPool {
	scopeDigest := shortDigest([]byte(scope), 16)
	identity := pool.config
	identity.RuntimeContract += ":scope:" + scopeDigest
	poolKey := ordinaryPoolFingerprint(identity)
	ctx, cancel := context.WithCancel(context.Background())
	base := "ordinarypool:v1:" + scopeDigest + ":"
	return &sharedOrdinaryPool{
		pool:        pool,
		store:       store,
		scopeDigest: scopeDigest,
		poolKey:     poolKey,
		recordBase:  base + poolKey + ":record:",
		lockKey:     base + "refill-lock",
		ownerBase:   base + poolKey + ":owner:",
		ctx:         ctx,
		cancel:      cancel,
	}
}

func (p *sharedOrdinaryPool) start(ctx context.Context) error {
	for {
		err := p.withLock(ctx, p.registerOwner)
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

func (p *sharedOrdinaryPool) registerOwner(ctx context.Context) error {
	p.mu.Lock()
	if p.ownerKey != "" {
		p.mu.Unlock()
		return nil
	}
	token := []byte(randSuffix(24))
	key := p.ownerBase + string(token)
	p.mu.Unlock()
	created, err := p.store.SetNX(ctx, key, token, ordinaryPoolLockTTL)
	if err != nil {
		return fmt.Errorf("register shared ordinary pool owner: %w", err)
	}
	if !created {
		return errors.New("shared ordinary pool owner token collision")
	}
	p.mu.Lock()
	if p.stopping || p.ctx.Err() != nil {
		p.mu.Unlock()
		_, _ = p.store.CompareAndDelete(context.WithoutCancel(ctx), key, token)
		return context.Canceled
	}
	p.ownerKey, p.ownerToken = key, token
	p.ownerHealthy.Store(true)
	p.wg.Add(1)
	ownerCtx := p.ctx
	p.mu.Unlock()
	go func() {
		defer p.wg.Done()
		ticker := time.NewTicker(ordinaryPoolLockTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ownerCtx.Done():
				return
			case <-ticker.C:
				if renewErr := p.refreshOwner(ownerCtx); renewErr != nil {
					logger.Error(context.Background(), "shared ordinary pool owner renewal failed", logger.ErrorField(renewErr))
					continue
				}
				// Periodic refill also retires obsolete generations after their
				// final owner exits or crashes, without waiting for traffic.
				p.scheduleRefill()
			}
		}
	}()
	return nil
}

func ordinaryPoolFingerprint(cfg PoolConfig) string {
	pidLimit := cfg.PidLimit
	if pidLimit <= 0 {
		pidLimit = 100
	}
	identity := struct {
		Version         string `json:"version"`
		RuntimeContract string `json:"runtime_contract"`
		Image           string `json:"image"`
		Memory          string `json:"memory"`
		MemoryRequest   string `json:"memory_request"`
		CPU             string `json:"cpu"`
		CPURequest      string `json:"cpu_request"`
		Disk            string `json:"disk"`
		TmpDisk         string `json:"tmp_disk"`
		PidLimit        int    `json:"pid_limit"`
		RunAsUser       int64  `json:"run_as_user"`
		ReadOnlyRootFS  bool   `json:"read_only_root_fs"`
		SeccompProfile  string `json:"seccomp_profile"`
	}{"ordinary-pool/v2", cfg.RuntimeContract, cfg.Image, cfg.Memory, cfg.MemoryRequest, cfg.CPU, cfg.CPURequest, cfg.Disk, cfg.TmpDisk, pidLimit, 1000, false, cfg.SeccompProfile}
	raw, _ := json.Marshal(identity)
	return shortDigest(raw, 32)
}

func (p *sharedOrdinaryPool) refreshOwner(ctx context.Context) error {
	err := refreshPoolOwner(ctx, p.store, p.ownerKey, p.ownerToken, p.lockKey, "", p.poolKey)
	p.ownerHealthy.Store(err == nil)
	return err
}

func shortDigest(raw []byte, chars int) string {
	sum := sha256.Sum256(raw)
	encoded := hex.EncodeToString(sum[:])
	return encoded[:chars]
}

func (p *sharedOrdinaryPool) recordKey(preparationID string) string {
	return p.recordBase + preparationID
}

func (p *sharedOrdinaryPool) scopePattern() string {
	return "ordinarypool:v1:" + p.scopeDigest + ":*"
}

func ordinaryPoolRecordKeys(keys []string) []string {
	result := keys[:0]
	for _, key := range keys {
		if strings.Contains(key, ":record:") {
			result = append(result, key)
		}
	}
	return result
}

func (p *sharedOrdinaryPool) warmUp(ctx context.Context) error {
	if err := p.reconcile(ctx, nil); err != nil {
		return err
	}
	if err := p.refill(ctx); errors.Is(err, errOrdinaryPoolLockBusy) {
		p.scheduleRefill()
		return nil
	} else {
		return err
	}
}

func (p *sharedOrdinaryPool) size() int {
	return int(p.knownSize.Load())
}

func (p *sharedOrdinaryPool) stop() {
	p.mu.Lock()
	if !p.stopping {
		p.stopping = true
		p.cancel()
	}
	ownerKey := p.ownerKey
	ownerToken := append([]byte(nil), p.ownerToken...)
	p.mu.Unlock()
	p.wg.Wait()
	if ownerKey == "" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), ordinaryPoolLockTTL)
	defer cancel()
	if _, err := p.store.CompareAndDelete(cleanupCtx, ownerKey, ownerToken); err != nil {
		logger.Error(cleanupCtx, "failed to unregister shared ordinary pool owner", logger.ErrorField(err))
		return
	}
}

func (p *sharedOrdinaryPool) scheduleRefill() {
	p.mu.Lock()
	if p.stopping || p.ctx.Err() != nil || !p.ownerHealthy.Load() {
		p.mu.Unlock()
		return
	}
	if p.refilling {
		p.refillPending = true
		p.mu.Unlock()
		return
	}
	p.wg.Add(1)
	p.refilling = true
	p.refillPending = false
	ctx := p.ctx
	p.mu.Unlock()
	go func() {
		defer p.wg.Done()
		defer func() {
			p.mu.Lock()
			pending := p.refillPending
			p.refilling, p.refillPending = false, false
			p.mu.Unlock()
			if pending {
				p.scheduleRefill()
			}
		}()
		var err error
		for attempt := 0; attempt < maxRefillRetries; attempt++ {
			err = p.refill(ctx)
			if !errors.Is(err, errOrdinaryPoolLockBusy) {
				break
			}
			backoff := time.Duration(1<<attempt) * time.Second
			if backoff > maxRefillBackoff {
				backoff = maxRefillBackoff
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			metrics.RecordPoolRefillFailure(context.Background())
			logger.Error(context.Background(), "shared ordinary pool refill failed", logger.ErrorField(err))
		}
	}()
}

func (p *sharedOrdinaryPool) acquire(ctx context.Context) (*runtime.SandboxInfo, error) {
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	if !p.ownerHealthy.Load() {
		return nil, errors.New("ordinary pool owner lease is unavailable")
	}
	linked, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	defer func() { stop(); cancel() }()
	ctx = linked
	entries, err := p.listCurrentRecords(ctx)
	if err != nil {
		return nil, fmt.Errorf("list shared ordinary pool: %w", err)
	}
	for _, entry := range entries {
		if entry.record.State != ordinaryPoolPrepared {
			continue
		}
		claimed := entry.record
		claimed.State = ordinaryPoolClaimed
		claimed.ClaimToken = randSuffix(24)
		claimed.ClaimUntil = time.Now().UTC().Add(ordinaryPoolClaimTTL)
		claimed.UpdatedAt = time.Now().UTC()
		claimed.Revision++
		claimedRaw, err := json.Marshal(claimed)
		if err != nil {
			return nil, err
		}
		swapped, err := p.store.CompareAndSwap(ctx, entry.key, entry.raw, claimedRaw, 0)
		if err != nil {
			return nil, fmt.Errorf("claim shared ordinary pool record: %w", err)
		}
		if !swapped {
			continue
		}

		info, healthErr := p.pool.runtime.GetSandbox(ctx, claimed.RuntimeID)
		if healthErr != nil {
			return nil, fmt.Errorf("verify claimed ordinary runtime: %w", healthErr)
		}
		if info == nil || info.State != "running" || info.RuntimeUID != claimed.RuntimeUID {
			if discardErr := p.discardClaimed(ctx, entry.key, claimedRaw, claimed, info); discardErr != nil {
				return nil, discardErr
			}
			continue
		}
		p.decrementKnownSize()
		metrics.SandboxPoolSize.Add(ctx, -1)
		metrics.RecordPoolAcquire(ctx, true)
		p.scheduleRefill()
		return info, nil
	}

	metrics.RecordPoolAcquire(ctx, false)
	// Keep the transient pool marker so the Kubernetes runtime can perform its
	// policy-first pool-to-sandbox identity migration. It deliberately has no
	// shared pool key and therefore cannot be adopted as warm inventory.
	info, err := p.pool.createWarmWithLabels(ctx, map[string]string{"sandbox.pool": "true"})
	if err != nil {
		return nil, err
	}
	p.scheduleRefill()
	return info, nil
}

func (p *sharedOrdinaryPool) confirmAcquired(ctx context.Context, runtimeID, runtimeUID string) error {
	entries, err := p.listCurrentRecords(ctx)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.record.State != ordinaryPoolClaimed || entry.record.RuntimeID != runtimeID || entry.record.RuntimeUID != runtimeUID {
			continue
		}
		deleted, err := p.store.CompareAndDelete(ctx, entry.key, entry.raw)
		if err != nil {
			return err
		}
		if !deleted {
			return errors.New("ordinary pool claim changed before confirmation")
		}
		return nil
	}
	// Reconciliation may already have observed the removed pool label.
	return nil
}

func (p *sharedOrdinaryPool) discardClaimed(ctx context.Context, key string, raw []byte, record ordinaryPoolRecord, info *runtime.SandboxInfo) error {
	cleanupCtx := context.WithoutCancel(ctx)
	if info != nil {
		if info.RuntimeID != record.RuntimeID || info.RuntimeUID != record.RuntimeUID {
			return errors.New("claimed ordinary runtime identity changed")
		}
		if err := p.pool.runtime.RemoveSandbox(cleanupCtx, record.RuntimeID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return fmt.Errorf("remove rejected ordinary runtime: %w", err)
		}
	}
	deleted, err := p.store.CompareAndDelete(cleanupCtx, key, raw)
	if err != nil {
		return fmt.Errorf("retire rejected ordinary pool record: %w", err)
	}
	if !deleted {
		return errors.New("rejected ordinary pool record changed during cleanup")
	}
	p.decrementKnownSize()
	metrics.SandboxPoolSize.Add(context.Background(), -1)
	return nil
}

func (p *sharedOrdinaryPool) decrementKnownSize() {
	for {
		current := p.knownSize.Load()
		if current <= 0 || p.knownSize.CompareAndSwap(current, current-1) {
			return
		}
	}
}

func (p *sharedOrdinaryPool) refill(ctx context.Context) error {
	if !p.ownerHealthy.Load() {
		return errors.New("ordinary pool owner lease is unavailable")
	}
	// Repair interrupted preparation/claim state before counting capacity. This
	// keeps a crashed preparer from suppressing refill indefinitely.
	if err := p.reconcile(ctx, nil); err != nil {
		return err
	}
	return p.withLock(ctx, func(lockCtx context.Context) error {
		entries, err := p.listCurrentRecords(lockCtx)
		if err != nil {
			return err
		}
		inventory := 0
		for _, entry := range entries {
			if entry.record.State == ordinaryPoolPreparing || entry.record.State == ordinaryPoolPrepared {
				inventory++
			}
		}
		p.knownSize.Store(int64(inventory))
		target := p.pool.config.MinSize
		if p.pool.config.MaxSize >= 0 && target > p.pool.config.MaxSize {
			target = p.pool.config.MaxSize
		}
		for inventory < target {
			if err := lockCtx.Err(); err != nil {
				return err
			}
			if _, err := p.prepareOne(lockCtx); err != nil {
				return err
			}
			inventory++
			p.knownSize.Store(int64(inventory))
			metrics.SandboxPoolSize.Add(context.Background(), 1)
		}
		retireCtx, cancel := context.WithTimeout(lockCtx, fusePoolCleanupTimeout)
		defer cancel()
		if err := p.retireObsolete(retireCtx, target); err != nil {
			logger.Error(lockCtx, "obsolete ordinary pool retirement will retry", logger.ErrorField(err))
		}
		return nil
	})
}

func (p *sharedOrdinaryPool) prepareOne(ctx context.Context) (*runtime.SandboxInfo, error) {
	preparationID := randSuffix(24)
	now := time.Now().UTC()
	record := ordinaryPoolRecord{
		PreparationID: preparationID,
		PoolKey:       p.poolKey,
		State:         ordinaryPoolPreparing,
		UpdatedAt:     now,
		Revision:      1,
	}
	raw, _ := json.Marshal(record)
	created, err := p.store.SetNX(ctx, p.recordKey(preparationID), raw, 0)
	if err != nil {
		return nil, fmt.Errorf("admit ordinary pool preparation: %w", err)
	}
	if !created {
		return nil, errors.New("ordinary pool preparation ID collision")
	}
	labels := map[string]string{
		"sandbox.pool":          "true",
		"sandbox.pool.key":      p.poolKey,
		"sandbox.pool.instance": preparationID,
		"sandbox.pool.state":    string(ordinaryPoolPreparing),
	}
	info, createErr := p.pool.createWarmWithLabels(ctx, labels)
	if createErr != nil {
		_, _ = p.store.CompareAndDelete(context.WithoutCancel(ctx), p.recordKey(preparationID), raw)
		return nil, createErr
	}
	publisher, ok := p.pool.runtime.(runtime.OrdinaryPoolStatePublisher)
	if !ok {
		_ = p.pool.runtime.RemoveSandbox(context.WithoutCancel(ctx), info.RuntimeID)
		_, _ = p.store.CompareAndDelete(context.WithoutCancel(ctx), p.recordKey(preparationID), raw)
		return nil, errors.New("runtime does not support ordinary pool state publication")
	}
	ref, refErr := runtime.NewRuntimeRef(info.RuntimeID, info.RuntimeUID)
	if refErr != nil {
		_ = p.pool.runtime.RemoveSandbox(context.WithoutCancel(ctx), info.RuntimeID)
		_, _ = p.store.CompareAndDelete(context.WithoutCancel(ctx), p.recordKey(preparationID), raw)
		return nil, refErr
	}
	if publishErr := publisher.PublishOrdinaryPoolPrepared(ctx, ref, p.poolKey, preparationID); publishErr != nil {
		_ = p.pool.runtime.RemoveSandbox(context.WithoutCancel(ctx), info.RuntimeID)
		_, _ = p.store.CompareAndDelete(context.WithoutCancel(ctx), p.recordKey(preparationID), raw)
		return nil, fmt.Errorf("publish ordinary pool Pod prepared state: %w", publishErr)
	}
	prepared := record
	prepared.RuntimeID = info.RuntimeID
	prepared.RuntimeUID = info.RuntimeUID
	prepared.State = ordinaryPoolPrepared
	prepared.UpdatedAt = time.Now().UTC()
	prepared.Revision++
	preparedRaw, _ := json.Marshal(prepared)
	swapped, swapErr := p.store.CompareAndSwap(ctx, p.recordKey(preparationID), raw, preparedRaw, 0)
	if swapErr != nil || !swapped {
		_ = p.pool.runtime.RemoveSandbox(context.WithoutCancel(ctx), info.RuntimeID)
		_, _ = p.store.CompareAndDelete(context.WithoutCancel(ctx), p.recordKey(preparationID), raw)
		if swapErr != nil {
			return nil, fmt.Errorf("publish ordinary pool runtime: %w", swapErr)
		}
		return nil, errors.New("ordinary pool preparation admission lost")
	}
	return info, nil
}

func (p *sharedOrdinaryPool) listCurrentRecords(ctx context.Context) ([]ordinaryPoolEntry, error) {
	keys, err := p.store.Keys(ctx, p.recordBase+"*")
	if err != nil {
		return nil, err
	}
	return p.loadRecords(ctx, keys)
}

func (p *sharedOrdinaryPool) loadRecords(ctx context.Context, keys []string) ([]ordinaryPoolEntry, error) {
	sort.Strings(keys)
	entries := make([]ordinaryPoolEntry, 0, len(keys))
	for _, key := range keys {
		raw, err := p.store.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if raw == nil {
			continue
		}
		var record ordinaryPoolRecord
		if err := json.Unmarshal(raw, &record); err != nil || !validOrdinaryPoolRecord(key, record) {
			return nil, fmt.Errorf("corrupt ordinary pool record %q", key)
		}
		entries = append(entries, ordinaryPoolEntry{key: key, raw: raw, record: record})
	}
	return entries, nil
}

func validOrdinaryPoolRecord(key string, record ordinaryPoolRecord) bool {
	if record.PreparationID == "" || record.PoolKey == "" || record.Revision == 0 || record.UpdatedAt.IsZero() ||
		!strings.HasSuffix(key, ":"+record.PoolKey+":record:"+record.PreparationID) ||
		(record.RuntimeID == "") != (record.RuntimeUID == "") {
		return false
	}
	switch record.State {
	case ordinaryPoolPreparing:
		return record.ClaimToken == "" && record.ClaimUntil.IsZero()
	case ordinaryPoolPrepared:
		return record.RuntimeID != "" && record.ClaimToken == "" && record.ClaimUntil.IsZero()
	case ordinaryPoolClaimed:
		return record.RuntimeID != "" && record.ClaimToken != "" && !record.ClaimUntil.IsZero()
	case ordinaryPoolCleanup:
		return record.ClaimToken == "" && record.ClaimUntil.IsZero()
	default:
		return false
	}
}

func (p *sharedOrdinaryPool) cleanupRecord(ctx context.Context, entry ordinaryPoolEntry, pod *runtime.SandboxInfo) error {
	record, raw := entry.record, entry.raw
	if record.State != ordinaryPoolCleanup {
		cleanup := record
		cleanup.State = ordinaryPoolCleanup
		cleanup.ClaimToken = ""
		cleanup.ClaimUntil = time.Time{}
		cleanup.UpdatedAt = time.Now().UTC()
		cleanup.Revision++
		if pod != nil && cleanup.RuntimeID == "" {
			cleanup.RuntimeID, cleanup.RuntimeUID = pod.RuntimeID, pod.RuntimeUID
		}
		cleanupRaw, _ := json.Marshal(cleanup)
		swapped, err := p.store.CompareAndSwap(ctx, entry.key, raw, cleanupRaw, 0)
		if err != nil {
			return err
		}
		if !swapped {
			return errors.New("ordinary pool record changed before cleanup claim")
		}
		record, raw = cleanup, cleanupRaw
	}
	if pod != nil {
		if record.RuntimeID != pod.RuntimeID || record.RuntimeUID != pod.RuntimeUID {
			return errors.New("ordinary pool cleanup runtime identity changed")
		}
		remover, ok := p.pool.runtime.(runtime.OrdinarySandboxRemover)
		if !ok {
			return errors.New("shared ordinary pool requires exact runtime removal")
		}
		if err := remover.RemoveOrdinarySandbox(ctx, runtime.RuntimeRef{ID: record.RuntimeID, UID: record.RuntimeUID}); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return err
		}
	}
	deleted, err := p.store.CompareAndDelete(ctx, entry.key, raw)
	if err != nil {
		return err
	}
	if !deleted {
		return errors.New("ordinary pool cleanup record changed")
	}
	return nil
}

func (p *sharedOrdinaryPool) withLock(ctx context.Context, fn func(context.Context) error) error {
	return withPoolLock(ctx, p.store, p.lockKey, fn)
}

func withPoolLock(ctx context.Context, store state.AtomicStore, lockKey string, fn func(context.Context) error) error {
	token := []byte(randSuffix(24))
	locked, err := store.SetNX(ctx, lockKey, token, ordinaryPoolLockTTL)
	if err != nil {
		return fmt.Errorf("lock shared ordinary pool: %w", err)
	}
	if !locked {
		return errOrdinaryPoolLockBusy
	}
	lockCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(ordinaryPoolLockTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-lockCtx.Done():
				return
			case <-ticker.C:
				renewed, renewErr := store.CompareAndSwap(lockCtx, lockKey, token, token, ordinaryPoolLockTTL)
				if renewErr != nil || !renewed {
					cancel()
					return
				}
			}
		}
	}()
	runErr := fn(lockCtx)
	cancel()
	<-done
	unlockCtx, unlockCancel := context.WithTimeout(context.WithoutCancel(ctx), fusePoolCleanupTimeout)
	defer unlockCancel()
	_, unlockErr := store.CompareAndDelete(unlockCtx, lockKey, token)
	return errors.Join(runErr, unlockErr)
}

func (p *sharedOrdinaryPool) reconcile(ctx context.Context, protectedRuntimeIDs map[string]struct{}) error {
	err := p.withLock(ctx, func(lockCtx context.Context) error {
		keys, err := p.store.Keys(lockCtx, p.recordBase+"*")
		if err != nil {
			return err
		}
		keys = ordinaryPoolRecordKeys(keys)
		entries, err := p.loadRecords(lockCtx, keys)
		if err != nil {
			return err
		}
		pods, err := p.pool.runtime.ListSandboxes(lockCtx, map[string]string{"sandbox.pool": "true"})
		if err != nil {
			return err
		}
		podByInstance := make(map[string]runtime.SandboxInfo)
		for _, pod := range pods {
			if pod.Labels["sandbox.workspace.mode"] == string(WorkspaceMountFUSE) {
				continue
			}
			if instance := pod.Labels["sandbox.pool.instance"]; instance != "" {
				podByInstance[instance] = pod
			}
		}
		recordByInstance := make(map[string]ordinaryPoolEntry)
		now := time.Now().UTC()
		for _, entry := range entries {
			record := entry.record
			if record.PoolKey != p.poolKey {
				continue
			}
			recordByInstance[record.PreparationID] = entry
			pod, hasPod := podByInstance[record.PreparationID]
			sameRuntime := hasPod && pod.RuntimeID == record.RuntimeID && (record.RuntimeUID == "" || pod.RuntimeUID == record.RuntimeUID)
			switch record.State {
			case ordinaryPoolPreparing:
				if hasPod && pod.State == "running" && (record.RuntimeID == "" || sameRuntime) {
					prepared := record
					prepared.RuntimeID, prepared.RuntimeUID = pod.RuntimeID, pod.RuntimeUID
					prepared.State = ordinaryPoolPrepared
					prepared.UpdatedAt = now
					prepared.Revision++
					preparedRaw, _ := json.Marshal(prepared)
					if swapped, swapErr := p.store.CompareAndSwap(lockCtx, entry.key, entry.raw, preparedRaw, 0); swapErr != nil {
						return swapErr
					} else if swapped {
						recordByInstance[record.PreparationID] = ordinaryPoolEntry{key: entry.key, raw: preparedRaw, record: prepared}
					} else {
						return errors.New("ordinary preparation changed during recovery")
					}
				} else if now.Sub(record.UpdatedAt) >= ordinaryPoolStaleTTL {
					var podRef *runtime.SandboxInfo
					if hasPod {
						podRef = &pod
					}
					if err := p.cleanupRecord(lockCtx, entry, podRef); err != nil {
						return fmt.Errorf("clean stale ordinary preparation: %w", err)
					}
					delete(recordByInstance, record.PreparationID)
				}
			case ordinaryPoolPrepared:
				if !sameRuntime || pod.State != "running" {
					var podRef *runtime.SandboxInfo
					if hasPod {
						podRef = &pod
					}
					if err := p.cleanupRecord(lockCtx, entry, podRef); err != nil {
						return fmt.Errorf("clean unhealthy ordinary runtime: %w", err)
					}
					delete(recordByInstance, record.PreparationID)
				} else if pod.Labels["sandbox.pool.state"] != string(ordinaryPoolPrepared) {
					if pod.Labels["sandbox.pool.state"] != string(ordinaryPoolPreparing) {
						return errors.New("prepared ordinary runtime state label changed")
					}
					publisher, ok := p.pool.runtime.(runtime.OrdinaryPoolStatePublisher)
					if !ok {
						return errors.New("runtime does not support ordinary pool state publication")
					}
					ref, refErr := runtime.NewRuntimeRef(record.RuntimeID, record.RuntimeUID)
					if refErr != nil {
						return refErr
					}
					if err := publisher.PublishOrdinaryPoolPrepared(lockCtx, ref, p.poolKey, record.PreparationID); err != nil {
						return fmt.Errorf("repair ordinary pool prepared state label: %w", err)
					}
				}
			case ordinaryPoolClaimed:
				// Neither an elapsed claim deadline nor missing API membership
				// fences an in-flight Kubernetes identity migration. Normal
				// reconciliation never physically removes claimed runtimes;
				// explicit release-exclusive drain handles abandoned claims.
				if !hasPod || pod.Labels["sandbox.pool"] != "true" {
					deleted, deleteErr := p.store.CompareAndDelete(lockCtx, entry.key, entry.raw)
					if deleteErr != nil {
						return deleteErr
					}
					if !deleted {
						return errors.New("claimed ordinary record changed during retirement")
					}
					delete(recordByInstance, record.PreparationID)
				}
			case ordinaryPoolCleanup:
				var podRef *runtime.SandboxInfo
				if hasPod {
					podRef = &pod
				}
				if err := p.cleanupRecord(lockCtx, entry, podRef); err != nil {
					return fmt.Errorf("resume ordinary pool cleanup: %w", err)
				}
				delete(recordByInstance, record.PreparationID)
			default:
				return fmt.Errorf("invalid ordinary pool state %q", record.State)
			}
		}

		for _, pod := range pods {
			if pod.Labels["sandbox.workspace.mode"] == string(WorkspaceMountFUSE) {
				continue
			}
			if _, protected := protectedRuntimeIDs[pod.RuntimeID]; protected {
				continue
			}
			// A rolling upgrade may overlap replicas using different pool
			// fingerprints, and the first protocol rollout overlaps legacy Pods
			// without a fingerprint. They are not deletion or adoption candidates
			// for this process; release drain removes them after all replicas stop.
			if pod.Labels["sandbox.pool.key"] != p.poolKey {
				continue
			}
			instance := pod.Labels["sandbox.pool.instance"]
			if instance != "" {
				if entry, exists := recordByInstance[instance]; exists && entry.record.RuntimeID == pod.RuntimeID && entry.record.RuntimeUID == pod.RuntimeUID {
					continue
				}
				if pod.State == "running" {
					record := ordinaryPoolRecord{PreparationID: instance, RuntimeID: pod.RuntimeID, RuntimeUID: pod.RuntimeUID,
						PoolKey: p.poolKey, State: ordinaryPoolPrepared, UpdatedAt: now, Revision: 1}
					raw, _ := json.Marshal(record)
					if adopted, adoptErr := p.store.SetNX(lockCtx, p.recordKey(instance), raw, 0); adoptErr != nil {
						return adoptErr
					} else if adopted {
						recordByInstance[instance] = ordinaryPoolEntry{key: p.recordKey(instance), raw: raw, record: record}
						continue
					} else {
						return errors.New("ordinary pool adoption record already exists")
					}
				}
			}
			logger.Info(lockCtx, "removing obsolete ordinary pool runtime", logger.AddField("runtime_id", pod.RuntimeID))
			if err := p.pool.runtime.RemoveSandbox(lockCtx, pod.RuntimeID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
				return err
			}
		}
		inventory := 0
		for _, entry := range recordByInstance {
			if entry.record.State == ordinaryPoolPreparing || entry.record.State == ordinaryPoolPrepared {
				inventory++
			}
		}
		p.knownSize.Store(int64(inventory))
		return nil
	})
	if errors.Is(err, errOrdinaryPoolLockBusy) {
		return nil
	}
	return err
}

func (p *sharedOrdinaryPool) drainRelease(ctx context.Context) error {
	p.stop()
	pods, err := p.pool.runtime.ListSandboxes(ctx, map[string]string{"sandbox.pool": "true"})
	if err != nil {
		return err
	}
	var result error
	for _, pod := range pods {
		if pod.Labels["sandbox.workspace.mode"] == string(WorkspaceMountFUSE) {
			continue
		}
		if removeErr := p.pool.runtime.RemoveSandbox(ctx, pod.RuntimeID); removeErr != nil && !errors.Is(removeErr, runtime.ErrNotFound) {
			result = errors.Join(result, removeErr)
		}
	}
	keys, keysErr := p.store.Keys(ctx, p.scopePattern())
	if keysErr != nil {
		return errors.Join(result, keysErr)
	}
	for _, key := range keys {
		if !strings.HasPrefix(key, "ordinarypool:v1:"+p.scopeDigest+":") {
			continue
		}
		result = errors.Join(result, p.store.Delete(ctx, key))
	}
	p.knownSize.Store(0)
	return result
}
