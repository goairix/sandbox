package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/goairix/sandbox/internal/storage/state"
	"github.com/goairix/sandbox/internal/telemetry/metrics"
)

const (
	fusePoolKeyVersion     = "workspace-fuse-pool/v1"
	fusePoolCleanupTimeout = 5 * time.Second
)

var (
	ErrInvalidFUSEPoolConfig  = errors.New("invalid FUSE pool configuration")
	ErrFUSEPoolStopped        = errors.New("FUSE pool is stopped")
	ErrFUSEPoolKeyMismatch    = errors.New("FUSE pool key mismatch")
	ErrFUSEPoolReturnUnproven = errors.New("FUSE pool pristine return cannot be proven")
	ErrFUSEPoolReturnFenced   = errors.New("FUSE pool return belongs to a later lifecycle")
	ErrFUSEPoolProtected      = errors.New("FUSE pool record is protected by manager state")
	ErrFUSEPoolRefillBusy     = errors.New("FUSE pool refill is already in progress")
	ErrFUSEPoolAtCapacity     = errors.New("FUSE pool is at active preparation capacity")
)

type FUSEPoolDisposition uint8

const (
	FUSEPoolDispositionUnknown FUSEPoolDisposition = iota
	FUSEPoolPristine
	FUSEPoolProtected
	FUSEPoolAbandoned
)

// PristineGuardFunc classifies manager-owned owner/session/gate state. An
// error or Unknown is fail-closed and never authorizes physical deletion.
type PristineGuardFunc func(context.Context, state.FUSEPoolRecord) (FUSEPoolDisposition, error)

// FUSEPoolConfig configures the sandbox-api-owned FUSE shell pool.
type FUSEPoolConfig struct {
	MinSize         int
	MaxSize         int
	RefillInterval  time.Duration
	PrepareTimeout  time.Duration
	ReservationTTL  time.Duration
	MaintainerToken string
	PristineGuard   PristineGuardFunc
}

// FUSEPool maintains prefix-free runtime shells. Redis is the shared inventory
// and lock; this process remains the only active controller.
type FUSEPool struct {
	runtime runtime.Runtime
	repo    state.FUSEPoolRepository
	config  FUSEPoolConfig
	spec    runtime.SandboxSpec
	poolKey string

	lifecycleMu  sync.Mutex
	starting     bool
	running      bool
	stopping     bool
	stopped      bool
	loopCtx      context.Context
	cancel       context.CancelFunc
	controllerWG sync.WaitGroup
	opWG         sync.WaitGroup
	startDone    chan struct{}
	startErr     error
	startWaiters int
	stopStarted  bool
	stopDone     chan struct{}
	stopErr      error
}

func NewFUSEPool(rt runtime.Runtime, repo state.FUSEPoolRepository, cfg FUSEPoolConfig, spec runtime.SandboxSpec) *FUSEPool {
	poolKey := ""
	if spec.WorkspaceFUSE != nil {
		poolKey = spec.WorkspaceFUSE.PoolKey
	}
	controlCtx, cancel := context.WithCancel(context.Background())
	return &FUSEPool{
		runtime:  rt,
		repo:     repo,
		config:   cfg,
		spec:     cloneFUSESandboxSpec(spec),
		poolKey:  poolKey,
		loopCtx:  controlCtx,
		cancel:   cancel,
		stopDone: make(chan struct{}),
	}
}

// Start performs the fail-closed initial reconciliation before starting the
// periodic controller. No goroutine is published when reconciliation fails.
func (p *FUSEPool) Start(ctx context.Context) error {
	if err := p.validate(); err != nil {
		return err
	}
	for {
		p.lifecycleMu.Lock()
		if p.stopped || p.stopping {
			p.lifecycleMu.Unlock()
			return ErrFUSEPoolStopped
		}
		if p.running {
			p.lifecycleMu.Unlock()
			return nil
		}
		if p.starting {
			done := p.startDone
			p.startWaiters++
			p.lifecycleMu.Unlock()
			select {
			case <-done:
				p.lifecycleMu.Lock()
				p.startWaiters--
				err := p.startErr
				stopped := p.stopped || p.stopping
				p.lifecycleMu.Unlock()
				if stopped {
					return ErrFUSEPoolStopped
				}
				// A caller-scoped cancellation only ends that caller's election.
				// A still-live waiter must compete to run its own initial pass.
				if (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) && ctx.Err() == nil {
					continue
				}
				return err
			case <-ctx.Done():
				p.lifecycleMu.Lock()
				p.startWaiters--
				p.lifecycleMu.Unlock()
				return ctx.Err()
			}
		}
		p.starting = true
		p.startDone = make(chan struct{})
		done := p.startDone
		p.opWG.Add(1)
		p.lifecycleMu.Unlock()

		err := p.waitInitialReconcile(ctx)
		p.opWG.Done()
		p.lifecycleMu.Lock()
		p.starting = false
		if err == nil && !p.stopped && !p.stopping {
			p.running = true
			p.controllerWG.Add(1)
			go p.reconcileLoop(p.loopCtx)
		} else if err == nil {
			err = ErrFUSEPoolStopped
		}
		p.startErr = err
		close(done)
		p.lifecycleMu.Unlock()
		if err != nil {
			return fmt.Errorf("initial FUSE pool reconciliation: %w", err)
		}
		return nil
	}
}

func (p *FUSEPool) waitInitialReconcile(ctx context.Context) error {
	linked, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.loopCtx, cancel)
	defer func() { stop(); cancel() }()
	for {
		ran, err := p.reconcileOnce(linked)
		if err != nil {
			if p.loopCtx.Err() != nil && ctx.Err() == nil {
				return ErrFUSEPoolStopped
			}
			return err
		}
		if ran {
			return err
		}
		timer := time.NewTimer(min(p.config.RefillInterval, 100*time.Millisecond))
		select {
		case <-timer.C:
		case <-linked.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrFUSEPoolStopped
		}
	}
}

// Running reports whether the periodic controller is active.
func (p *FUSEPool) Running() bool {
	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	return p.running && !p.stopping && !p.stopped
}

// WarmUp synchronously reconciles inventory to its configured minimum.
func (p *FUSEPool) WarmUp(ctx context.Context) error {
	if err := p.validate(); err != nil {
		return err
	}
	opCtx, done, err := p.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer done()
	for {
		ran, reconcileErr := p.reconcileOnce(opCtx)
		if reconcileErr != nil || ran {
			return reconcileErr
		}
		select {
		case <-time.After(min(p.config.RefillInterval, 100*time.Millisecond)):
		case <-opCtx.Done():
			return opCtx.Err()
		}
	}
}

// Acquire reserves a pristine prepared shell, or cold-prepares one carrying
// the request's token directly from preparing to reserved.
func (p *FUSEPool) Acquire(ctx context.Context, poolKey string) (*state.FUSEPoolRecord, error) {
	result := "error"
	defer func() {
		if p != nil && p.spec.WorkspaceFUSE != nil {
			metrics.RecordWorkspacePoolAcquire(ctx, p.spec.WorkspaceFUSE.RuntimeType, p.spec.WorkspaceFUSE.Provider, result)
		}
	}()
	if err := p.validate(); err != nil {
		return nil, err
	}
	if poolKey == "" || poolKey != p.poolKey {
		return nil, ErrFUSEPoolKeyMismatch
	}
	opCtx, done, err := p.beginOperation(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	for {
		if err := opCtx.Err(); err != nil {
			return nil, err
		}
		token := uuid.NewString()
		record, err := p.repo.ReservePrepared(opCtx, poolKey, token, p.config.ReservationTTL)
		if err != nil {
			return nil, fmt.Errorf("reserve prepared FUSE sandbox: %w", err)
		}
		if record == nil {
			record, err = p.prepareCold(opCtx, token)
			if err != nil {
				return nil, err
			}
			p.scheduleRefill()
			result = "cold"
			return record, nil
		}
		if err := p.validateReservedRecord(*record, token); err != nil {
			return nil, err
		}
		disposition, guardErr := p.guard(opCtx, *record)
		switch disposition {
		case FUSEPoolProtected:
			p.scheduleRefill()
			return nil, errors.Join(ErrFUSEPoolProtected, guardErr)
		case FUSEPoolDispositionUnknown:
			p.scheduleRefill()
			return nil, errors.Join(ErrFUSEPoolReturnUnproven, guardErr)
		case FUSEPoolAbandoned:
			if cleanupErr := p.claimAndDestroy(opCtx, *record); cleanupErr != nil {
				return nil, cleanupErr
			}
			continue
		case FUSEPoolPristine:
			if healthErr := p.runtime.PreparedSandboxHealth(opCtx, runtime.RuntimeRef{ID: record.RuntimeID, UID: record.RuntimeUID}, poolKey); healthErr != nil {
				cleanupErr := p.claimAndDestroy(opCtx, *record)
				if cleanupErr != nil {
					return nil, errors.Join(fmt.Errorf("prepared FUSE sandbox health: %w", healthErr), cleanupErr)
				}
				continue
			}
			if opCtx.Err() != nil {
				_ = p.claimAndDestroyWithCleanupContext(*record)
				return nil, ErrFUSEPoolStopped
			}
			p.scheduleRefill()
			result = "hit"
			return record, nil
		}
	}
}

func (p *FUSEPool) prepareCold(ctx context.Context, reservationToken string) (record *state.FUSEPoolRecord, returnErr error) {
	lockToken := p.config.MaintainerToken + ":cold:" + uuid.NewString()
	locked, err := p.repo.TryRefillLock(ctx, p.poolKey, lockToken, p.refillLockTTL())
	if err != nil {
		return nil, fmt.Errorf("acquire cold FUSE preparation lock: %w", err)
	}
	if !locked {
		return nil, ErrFUSEPoolRefillBusy
	}
	defer func() {
		if err := p.unlockRefill(ctx, lockToken); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("unlock cold FUSE preparation: %w", err))
		}
	}()
	lease := p.startRefillLease(ctx, lockToken)
	defer lease.stop()
	return p.prepareOne(lease.ctx, reservationToken, lockToken)
}

// ReturnPrepared returns a reservation only after both runtime health and the
// manager-owned owner/session/gate guard independently prove it pristine.
func (p *FUSEPool) ReturnPrepared(ctx context.Context, record state.FUSEPoolRecord) error {
	if err := p.validateReservedRecord(record, record.ReservationToken); err != nil {
		return err
	}
	opCtx, done, err := p.beginOperation(ctx)
	if err != nil {
		terminal, terminalErr := p.verifyReturnPreparedTerminal(record)
		if terminal {
			return terminalErr
		}
		return errors.Join(err, terminalErr)
	}
	defer done()
	defer p.scheduleRefill()
	terminal, terminalErr := p.verifyReturnPreparedTerminal(record)
	if terminal {
		return terminalErr
	}
	if terminalErr != nil {
		return terminalErr
	}
	disposition, guardErr := p.guard(opCtx, record)
	if disposition == FUSEPoolProtected {
		return errors.Join(ErrFUSEPoolProtected, guardErr)
	}
	if disposition == FUSEPoolDispositionUnknown {
		return errors.Join(ErrFUSEPoolReturnUnproven, guardErr)
	}
	if disposition == FUSEPoolAbandoned {
		return errors.Join(ErrFUSEPoolReturnUnproven, guardErr, p.claimAndDestroy(opCtx, record))
	}
	if healthErr := p.runtime.PreparedSandboxHealth(opCtx, runtime.RuntimeRef{ID: record.RuntimeID, UID: record.RuntimeUID}, record.PoolKey); healthErr != nil {
		cleanupErr := p.claimAndDestroy(opCtx, record)
		return errors.Join(healthErr, cleanupErr)
	}
	if err := opCtx.Err(); err != nil {
		return err
	}
	returned, err := p.repo.ReturnPreparedWithAdmission(opCtx, record.PreparationID, record.ReservationToken, record.Revision, p.config.MaxSize)
	if err != nil {
		compensationErr := p.compensatePublication(record, expectedPublication(record, state.FUSEPoolPrepared, ""))
		if errors.Is(err, state.ErrFUSEPoolConflict) {
			return errors.Join(ErrFUSEPoolAtCapacity, compensationErr)
		}
		return errors.Join(fmt.Errorf("return FUSE sandbox to prepared: %w", err), compensationErr)
	}
	if stopErr := p.publicationStopError(opCtx); stopErr != nil {
		return errors.Join(stopErr, p.compensatePublication(record, *returned))
	}
	return nil
}

func (p *FUSEPool) verifyReturnPreparedTerminal(original state.FUSEPoolRecord) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fusePoolCleanupTimeout)
	defer cancel()
	records, err := p.repo.ListByPoolKey(ctx, original.PoolKey)
	if err != nil {
		return false, fmt.Errorf("verify returned FUSE sandbox: %w", err)
	}
	for _, current := range records {
		if current.PreparationID != original.PreparationID {
			continue
		}
		if current.RuntimeID != original.RuntimeID || current.RuntimeUID != original.RuntimeUID || current.PoolKey != original.PoolKey || current.MaintainerToken != original.MaintainerToken {
			return true, ErrFUSEPoolReturnFenced
		}
		if current.State == state.FUSEPoolPrepared && current.Revision == original.Revision+1 && current.ReservationToken == "" {
			return true, nil
		}
		if current.State == state.FUSEPoolCleanup {
			return false, nil
		}
		if current.State != state.FUSEPoolReserved || current.Revision > original.Revision || current.ReservationToken != original.ReservationToken {
			return true, ErrFUSEPoolReturnFenced
		}
		return false, nil
	}
	return true, nil
}

// ReleaseConsumed removes the exact runtime before deleting its inventory
// record. A failed removal leaves the record intact and retryable.
func (p *FUSEPool) ReleaseConsumed(ctx context.Context, record state.FUSEPoolRecord) error {
	if record.PoolKey != p.poolKey || (record.State != state.FUSEPoolBinding && record.State != state.FUSEPoolConsumed) {
		return state.ErrFUSEPoolInvalidRecord
	}
	claimed, err := p.ClaimSingleUseCleanup(ctx, record)
	if err != nil {
		return fmt.Errorf("release consumed FUSE sandbox: %w", err)
	}
	if err := p.RemoveClaimedRuntime(ctx, *claimed); err != nil {
		return fmt.Errorf("release consumed FUSE sandbox: %w", err)
	}
	if err := p.CompleteClaimedCleanup(ctx, *claimed); err != nil {
		return fmt.Errorf("release consumed FUSE sandbox: %w", err)
	}
	p.scheduleRefill()
	return nil
}

// ClaimSingleUseCleanup fences one reserved-or-used shell into cleanup without
// deleting either the runtime or tombstone. Manager uses this two-phase form
// to retain a recovery anchor until runtime evidence and owner/session cleanup
// have completed.
func (p *FUSEPool) ClaimSingleUseCleanup(ctx context.Context, record state.FUSEPoolRecord) (*state.FUSEPoolRecord, error) {
	if record.PoolKey != p.poolKey || (record.State != state.FUSEPoolReserved && record.State != state.FUSEPoolBinding && record.State != state.FUSEPoolConsumed && record.State != state.FUSEPoolCleanup) {
		return nil, state.ErrFUSEPoolInvalidRecord
	}
	return p.claimCleanup(ctx, record)
}

func (p *FUSEPool) RemoveClaimedRuntime(ctx context.Context, claimed state.FUSEPoolRecord) error {
	if claimed.PoolKey != p.poolKey || claimed.State != state.FUSEPoolCleanup || claimed.CleanupToken == "" || claimed.RuntimeID == "" || claimed.RuntimeUID == "" {
		return state.ErrFUSEPoolInvalidRecord
	}
	if err := p.exactRemover().RemovePreparedSandbox(ctx, claimed.RuntimeID, claimed.RuntimeUID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return fmt.Errorf("remove claimed FUSE runtime: %w", err)
	}
	return nil
}

func (p *FUSEPool) CompleteClaimedCleanup(ctx context.Context, claimed state.FUSEPoolRecord) error {
	if claimed.PoolKey != p.poolKey || claimed.State != state.FUSEPoolCleanup || claimed.CleanupToken == "" {
		return state.ErrFUSEPoolInvalidRecord
	}
	deleted, err := p.repo.DeleteCleanup(ctx, claimed.PreparationID, claimed.CleanupToken, claimed.Revision)
	if err != nil || !deleted {
		records, verifyErr := p.repo.ListByPoolKey(ctx, claimed.PoolKey)
		if verifyErr != nil {
			primary := state.ErrFUSEPoolConflict
			if err != nil {
				primary = fmt.Errorf("delete cleanup record: %w", err)
			}
			return errors.Join(primary, fmt.Errorf("verify cleanup record deletion: %w", verifyErr))
		}
		for _, current := range records {
			if current.PreparationID != claimed.PreparationID {
				continue
			}
			if current.State != state.FUSEPoolCleanup || current.CleanupToken != claimed.CleanupToken || current.Revision != claimed.Revision {
				return state.ErrFUSEPoolConflict
			}
			if err != nil {
				return fmt.Errorf("delete cleanup record: %w", err)
			}
			return state.ErrFUSEPoolConflict
		}
		// An uncertain reply after the exact delete is success. Absence is also
		// success for a retry of the same claimed cleanup capability.
		p.scheduleRefill()
		return nil
	}
	p.scheduleRefill()
	return nil
}

func (p *FUSEPool) claimAmbiguousCleanup(before, possibleAfter state.FUSEPoolRecord) (*state.FUSEPoolRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), fusePoolCleanupTimeout)
	defer cancel()
	claimed, afterErr := p.claimCleanup(ctx, possibleAfter)
	if afterErr == nil || !isStalePoolMutation(afterErr) {
		return claimed, afterErr
	}
	claimed, beforeErr := p.claimCleanup(ctx, before)
	if beforeErr == nil {
		return claimed, nil
	}
	return nil, errors.Join(afterErr, beforeErr)
}

// Reconcile repairs the configured PoolKey while holding its distributed
// lock. Reserved/binding records are never recycled; consumed belongs to the
// persistent session lifecycle.
func (p *FUSEPool) Reconcile(ctx context.Context) (returnErr error) {
	if err := p.validate(); err != nil {
		return err
	}
	opCtx, done, err := p.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer done()
	_, err = p.reconcileOnce(opCtx)
	return err
}

func (p *FUSEPool) reconcileOnce(ctx context.Context) (ran bool, returnErr error) {
	lockToken := p.config.MaintainerToken + ":" + uuid.NewString()
	locked, err := p.repo.TryRefillLock(ctx, p.poolKey, lockToken, p.refillLockTTL())
	if err != nil {
		return false, fmt.Errorf("acquire FUSE refill lock: %w", err)
	}
	if !locked {
		return false, nil
	}
	defer func() {
		if err := p.unlockRefill(ctx, lockToken); err != nil && !errors.Is(err, state.ErrFUSEPoolTokenMismatch) {
			returnErr = errors.Join(returnErr, fmt.Errorf("unlock FUSE refill lock: %w", err))
		}
	}()
	lease := p.startRefillLease(ctx, lockToken)
	defer lease.stop()
	ctx = lease.ctx
	now, err := p.repo.ServerTime(ctx)
	if err != nil {
		return true, fmt.Errorf("read Redis server time: %w", err)
	}
	inspectionErr := p.inspectPrepared(ctx)
	now, err = p.repo.ServerTime(ctx)
	if err != nil {
		return true, errors.Join(inspectionErr, fmt.Errorf("refresh Redis server time: %w", err))
	}

	records, err := p.repo.ListByPoolKey(ctx, p.poolKey)
	if err != nil {
		return true, fmt.Errorf("list FUSE pool inventory: %w", err)
	}
	active := make([]state.FUSEPoolRecord, 0)
	for _, record := range records {
		switch record.State {
		case state.FUSEPoolPreparing:
			if record.PrepareUntil.IsZero() || !record.PrepareUntil.After(now) {
				disposition, _ := p.guard(ctx, record)
				if disposition == FUSEPoolPristine || disposition == FUSEPoolAbandoned {
					if err := p.claimAndDestroy(ctx, record); err != nil && !isStalePoolMutation(err) {
						return true, fmt.Errorf("clean expired preparing FUSE sandbox: %w", err)
					}
				}
				continue
			}
			active = append(active, record)
		case state.FUSEPoolPrepared:
			// Every prepared record was atomically inspection-reserved and only
			// returned here after the manager and runtime proved it pristine.
			active = append(active, record)
		case state.FUSEPoolReserved:
			if record.ReservedUntil.IsZero() || !record.ReservedUntil.After(now) {
				disposition, _ := p.guard(ctx, record)
				if disposition == FUSEPoolPristine || disposition == FUSEPoolAbandoned {
					if err := p.claimAndDestroy(ctx, record); err != nil && !isStalePoolMutation(err) {
						return true, fmt.Errorf("clean expired reserved FUSE sandbox: %w", err)
					}
				}
			}
		case state.FUSEPoolBinding:
			disposition, _ := p.guard(ctx, record)
			if disposition == FUSEPoolAbandoned {
				if err := p.claimAndDestroy(ctx, record); err != nil && !isStalePoolMutation(err) {
					return true, fmt.Errorf("clean abandoned binding FUSE sandbox: %w", err)
				}
			}
		case state.FUSEPoolConsumed:
			// Persistent session teardown owns consumed instances.
		case state.FUSEPoolCleanup:
			if record.CleanupUntil.IsZero() || !record.CleanupUntil.After(now) {
				if err := p.claimAndDestroy(ctx, record); err != nil && !isStalePoolMutation(err) {
					return true, fmt.Errorf("recover cleanup FUSE sandbox: %w", err)
				}
			}
		default:
			return true, state.ErrFUSEPoolCorrupt
		}
	}

	activeCount, err := p.repo.CountPreparingAndPrepared(ctx, p.poolKey)
	if err != nil {
		return true, fmt.Errorf("count active FUSE pool inventory: %w", err)
	}

	// MaxSize applies only to preparing+prepared. Prefer deleting prepared
	// shells; an in-flight preparing record is left for its exact owner/CAS.
	if activeCount > p.config.MaxSize {
		sort.Slice(active, func(i, j int) bool {
			if active[i].State != active[j].State {
				return active[i].State == state.FUSEPoolPrepared
			}
			return active[i].RuntimeUID > active[j].RuntimeUID
		})
		toRemove := activeCount - p.config.MaxSize
		kept := active[:0]
		for _, record := range active {
			if toRemove > 0 && record.State == state.FUSEPoolPrepared {
				if err := p.claimAndDestroy(ctx, record); err != nil {
					return true, fmt.Errorf("trim FUSE pool: %w", err)
				}
				toRemove--
				continue
			}
			kept = append(kept, record)
		}
		active = kept
	}
	activeCount, err = p.repo.CountPreparingAndPrepared(ctx, p.poolKey)
	if err != nil {
		return true, fmt.Errorf("recount active FUSE pool inventory: %w", err)
	}

	prepared := 0
	current, err := p.repo.ListByPoolKey(ctx, p.poolKey)
	if err != nil {
		return true, fmt.Errorf("refresh FUSE pool inventory: %w", err)
	}
	p.recordInventoryMetrics(ctx, current)
	for _, record := range current {
		if record.State == state.FUSEPoolPrepared {
			prepared++
		}
	}
	for prepared < p.config.MinSize && activeCount < p.config.MaxSize {
		_, err := p.prepareOne(ctx, "", lockToken)
		if err != nil {
			return true, fmt.Errorf("prepare FUSE pool sandbox: %w", err)
		}
		activeCount++
		prepared++
	}
	return true, inspectionErr
}

func (p *FUSEPool) recordInventoryMetrics(ctx context.Context, records []state.FUSEPoolRecord) {
	if p == nil || p.spec.WorkspaceFUSE == nil {
		return
	}
	counts := make(map[state.FUSEPoolState]int64)
	for _, record := range records {
		counts[record.State]++
	}
	for _, poolState := range []state.FUSEPoolState{state.FUSEPoolPreparing, state.FUSEPoolPrepared, state.FUSEPoolReserved, state.FUSEPoolBinding, state.FUSEPoolConsumed, state.FUSEPoolCleanup} {
		metrics.RecordWorkspacePoolSize(ctx, p.spec.WorkspaceFUSE.RuntimeType, p.spec.WorkspaceFUSE.Provider, p.poolKey, string(poolState), counts[poolState])
	}
}

func (p *FUSEPool) inspectPrepared(ctx context.Context) error {
	var result error
	verified := make([]state.FUSEPoolRecord, 0)
	for {
		token := p.config.MaintainerToken + ":inspect:" + uuid.NewString()
		record, err := p.repo.ReservePrepared(ctx, p.poolKey, token, p.config.ReservationTTL)
		if err != nil {
			result = errors.Join(result, err)
			break
		}
		if record == nil {
			break
		}
		disposition, guardErr := p.guard(ctx, *record)
		switch disposition {
		case FUSEPoolPristine:
			if healthErr := p.runtime.PreparedSandboxHealth(ctx, runtime.RuntimeRef{ID: record.RuntimeID, UID: record.RuntimeUID}, p.poolKey); healthErr != nil {
				result = errors.Join(result, healthErr, p.claimAndDestroy(ctx, *record))
			} else {
				verified = append(verified, *record)
			}
		case FUSEPoolAbandoned:
			result = errors.Join(result, p.claimAndDestroy(ctx, *record))
		case FUSEPoolProtected:
			result = errors.Join(result, ErrFUSEPoolProtected, guardErr)
		default:
			result = errors.Join(result, ErrFUSEPoolReturnUnproven, guardErr)
		}
	}
	for _, record := range verified {
		returned, err := p.repo.ReturnPreparedWithAdmission(ctx, record.PreparationID, record.ReservationToken, record.Revision, p.config.MaxSize)
		if err != nil {
			compensationErr := p.compensatePublication(record, expectedPublication(record, state.FUSEPoolPrepared, ""))
			if errors.Is(err, state.ErrFUSEPoolConflict) {
				result = errors.Join(result, compensationErr)
			} else {
				result = errors.Join(result, err, compensationErr)
			}
			continue
		}
		if stopErr := p.publicationStopError(ctx); stopErr != nil {
			result = errors.Join(result, stopErr, p.compensatePublication(record, *returned))
		}
	}
	return result
}

// Drain deletes only unreserved prepared shells from one exact PoolKey.
func (p *FUSEPool) Drain(ctx context.Context, poolKey string) error {
	if poolKey == "" || poolKey != p.poolKey {
		return ErrFUSEPoolKeyMismatch
	}
	records, err := p.repo.ListByPoolKey(ctx, poolKey)
	if err != nil {
		return err
	}
	var result error
	for _, record := range records {
		if record.State == state.FUSEPoolPrepared {
			disposition, guardErr := p.guard(ctx, record)
			switch disposition {
			case FUSEPoolPristine, FUSEPoolAbandoned:
				result = errors.Join(result, p.claimAndDestroy(ctx, record))
			case FUSEPoolProtected:
				result = errors.Join(result, ErrFUSEPoolProtected, guardErr)
			default:
				result = errors.Join(result, ErrFUSEPoolReturnUnproven, guardErr)
			}
		}
	}
	return result
}

// DrainRelease stops this pool and removes every unprotected record in the
// release store, including records left under a former configuration's
// PoolKey. Callers must hold release-wide exclusivity (for example, after
// scaling the API to zero).
func (p *FUSEPool) DrainRelease(ctx context.Context) error {
	result := p.Stop(ctx)
	poolKeys, err := p.repo.ListPoolKeys(ctx)
	if err != nil {
		return errors.Join(result, err)
	}
	for _, poolKey := range poolKeys {
		records, listErr := p.repo.ListByPoolKey(ctx, poolKey)
		if listErr != nil {
			result = errors.Join(result, listErr)
			continue
		}
		for _, record := range records {
			disposition, guardErr := p.guard(ctx, record)
			switch disposition {
			case FUSEPoolPristine, FUSEPoolAbandoned:
				result = errors.Join(result, p.claimAndDestroy(ctx, record))
			case FUSEPoolProtected:
				result = errors.Join(result, ErrFUSEPoolProtected, guardErr)
			default:
				result = errors.Join(result, ErrFUSEPoolReturnUnproven, guardErr)
			}
		}
	}
	result = errors.Join(result, p.repo.DrainRefillLocks(ctx))
	return result
}

// Stop terminates all controller goroutines and removes only preparing or
// prepared shells owned by this API instance. It is idempotent.
func (p *FUSEPool) Stop(ctx context.Context) error {
	p.lifecycleMu.Lock()
	if !p.stopStarted {
		p.stopStarted = true
		p.stopping = true
		p.cancel()
		go p.finishStop(context.WithoutCancel(ctx))
	}
	done := p.stopDone
	p.lifecycleMu.Unlock()
	select {
	case <-done:
		return p.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *FUSEPool) finishStop(ctx context.Context) {
	p.controllerWG.Wait()
	p.opWG.Wait()
	cleanupCtx, cancel := context.WithTimeout(ctx, fusePoolCleanupTimeout)
	p.stopErr = p.drainOwned(cleanupCtx)
	cancel()
	p.lifecycleMu.Lock()
	p.running, p.stopping, p.stopped = false, false, true
	close(p.stopDone)
	p.lifecycleMu.Unlock()
}

func (p *FUSEPool) drainOwned(ctx context.Context) error {
	if err := p.validate(); err != nil {
		return err
	}
	records, err := p.repo.ListByPoolKey(ctx, p.poolKey)
	if err != nil {
		return err
	}
	var result error
	for _, record := range records {
		if record.MaintainerToken == p.config.MaintainerToken && (record.State == state.FUSEPoolPreparing || record.State == state.FUSEPoolPrepared) {
			disposition, guardErr := p.guard(ctx, record)
			switch disposition {
			case FUSEPoolPristine, FUSEPoolAbandoned:
				result = errors.Join(result, p.claimAndDestroy(ctx, record))
			case FUSEPoolProtected:
				result = errors.Join(result, ErrFUSEPoolProtected, guardErr)
			default:
				result = errors.Join(result, ErrFUSEPoolReturnUnproven, guardErr)
			}
		}
	}
	return result
}

func (p *FUSEPool) reconcileLoop(ctx context.Context) {
	defer p.controllerWG.Done()
	for {
		delay := p.config.RefillInterval
		if delay > 5*time.Nanosecond {
			delay += time.Duration(rand.Int64N(int64(delay / 5)))
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
			if _, err := p.reconcileOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
				logger.Error(ctx, "FUSE pool reconciliation failed", logger.AddField("pool_key", p.poolKey), logger.ErrorField(err))
			}
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}

func (p *FUSEPool) scheduleRefill() {
	p.lifecycleMu.Lock()
	if p.stopping || p.stopped {
		p.lifecycleMu.Unlock()
		return
	}
	ctx := p.loopCtx
	p.controllerWG.Add(1)
	p.lifecycleMu.Unlock()
	go func() {
		defer p.controllerWG.Done()
		if _, err := p.reconcileOnce(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, ErrFUSEPoolStopped) {
			logger.Error(ctx, "FUSE pool refill failed", logger.AddField("pool_key", p.poolKey), logger.ErrorField(err))
		}
	}()
}

func (p *FUSEPool) prepareOne(ctx context.Context, reservationToken, refillToken string) (*state.FUSEPoolRecord, error) {
	started := time.Now()
	result := "error"
	defer func() {
		if p.spec.WorkspaceFUSE != nil {
			metrics.RecordWorkspacePoolPrepare(ctx, p.spec.WorkspaceFUSE.RuntimeType, p.spec.WorkspaceFUSE.Provider, result, time.Since(started).Seconds())
		}
	}()
	prepareCtx, cancel := context.WithTimeout(ctx, p.config.PrepareTimeout)
	defer cancel()
	spec := cloneFUSESandboxSpec(p.spec)
	spec.ID = "sandbox-pool-" + randSuffix(randSuffixLen)
	record := state.FUSEPoolRecord{
		PreparationID: spec.ID, PoolKey: p.poolKey, State: state.FUSEPoolPreparing,
		MaintainerToken: p.config.MaintainerToken, Revision: 1,
	}
	if err := p.repo.CreatePreparingWithAdmission(prepareCtx, record, refillToken, p.config.MaxSize, p.config.PrepareTimeout); err != nil {
		if errors.Is(err, state.ErrFUSEPoolConflict) {
			return nil, ErrFUSEPoolAtCapacity
		}
		return nil, fmt.Errorf("register preparing FUSE sandbox: %w", err)
	}
	info, err := p.runtime.PrepareSandbox(prepareCtx, spec)
	if err != nil {
		cleanupErr := p.claimAndDestroyWithCleanupContext(record)
		return nil, errors.Join(fmt.Errorf("prepare FUSE sandbox runtime: %w", err), cleanupErr)
	}
	if info == nil || info.RuntimeID == "" || info.RuntimeUID == "" {
		cleanupErr := p.claimAndDestroyWithCleanupContext(record)
		return nil, errors.Join(state.ErrFUSEPoolInvalidRecord, errors.New("runtime returned incomplete identity"), cleanupErr)
	}
	bound, err := p.repo.BindPreparingRuntime(prepareCtx, record.PreparationID, info.RuntimeID, info.RuntimeUID, refillToken, record.Revision)
	if err != nil {
		cleanupErr := p.claimAndDestroyWithRuntimeEvidence(record, info.RuntimeID, info.RuntimeUID)
		return nil, errors.Join(fmt.Errorf("bind preparing FUSE sandbox runtime: %w", err), cleanupErr)
	}
	record = *bound
	if err := p.runtime.PreparedSandboxHealth(prepareCtx, runtime.RuntimeRef{ID: record.RuntimeID, UID: record.RuntimeUID}, p.poolKey); err != nil {
		cleanupErr := p.claimAndDestroyWithCleanupContext(record)
		return nil, errors.Join(fmt.Errorf("verify preparing FUSE sandbox: %w", err), cleanupErr)
	}
	to := state.FUSEPoolPrepared
	token := p.config.MaintainerToken
	reserveTTL := time.Duration(0)
	if reservationToken != "" {
		to = state.FUSEPoolReserved
		token = reservationToken
		reserveTTL = p.config.ReservationTTL
	}
	transitioned, err := p.repo.TransitionWithRefillLock(prepareCtx, record.PreparationID, state.FUSEPoolPreparing, to, token, refillToken, record.Revision, reserveTTL)
	if err != nil {
		cleanupErr := p.compensatePublication(record, expectedPublication(record, to, token))
		return nil, errors.Join(fmt.Errorf("publish prepared FUSE sandbox: %w", err), cleanupErr)
	}
	if stopErr := p.publicationStopError(prepareCtx); stopErr != nil {
		return nil, errors.Join(stopErr, p.compensatePublication(record, *transitioned))
	}
	result = "success"
	return transitioned, nil
}

// expectedPublication describes the only revision/state/token result that the
// attempted transition can have committed. Server-authored timestamps are not
// used as deletion authority; ClaimCleanup validates the stored record and its
// deadline indexes while CAS-fencing this exact revision and token tuple.
func expectedPublication(before state.FUSEPoolRecord, to state.FUSEPoolState, token string) state.FUSEPoolRecord {
	after := before
	after.State = to
	after.Revision++
	switch {
	case before.State == state.FUSEPoolPreparing && to == state.FUSEPoolReserved:
		after.ReservationToken = token
	case before.State == state.FUSEPoolReserved && to == state.FUSEPoolPrepared:
		after.ReservationToken = ""
		after.ReservedUntil = time.Time{}
	}
	return after
}

// compensatePublication handles both outcomes of an uncertain Redis reply. It
// first claims the exact possible committed revision, then the exact pre-state.
// A later Acquire/transition changes revision or token and fences both claims,
// so this path can never delete a runtime now owned by another operation.
func (p *FUSEPool) compensatePublication(before, possibleAfter state.FUSEPoolRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), fusePoolCleanupTimeout)
	defer cancel()
	afterErr := p.claimAndDestroy(ctx, possibleAfter)
	if afterErr == nil || !isStalePoolMutation(afterErr) {
		return afterErr
	}
	beforeErr := p.claimAndDestroy(ctx, before)
	if beforeErr == nil {
		return nil
	}
	return errors.Join(afterErr, beforeErr)
}

func (p *FUSEPool) publicationStopError(ctx context.Context) error {
	p.lifecycleMu.Lock()
	stopping := p.stopping || p.stopped
	p.lifecycleMu.Unlock()
	if stopping {
		return ErrFUSEPoolStopped
	}
	return ctx.Err()
}

func (p *FUSEPool) claimAndDestroyWithCleanupContext(record state.FUSEPoolRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), fusePoolCleanupTimeout)
	defer cancel()
	return p.claimAndDestroy(ctx, record)
}

func (p *FUSEPool) claimAndDestroyWithRuntimeEvidence(record state.FUSEPoolRecord, runtimeID, runtimeUID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), fusePoolCleanupTimeout)
	defer cancel()
	claimed, err := p.claimCleanupWithEvidence(ctx, record, runtimeID, runtimeUID)
	if err != nil {
		return err
	}
	if err := p.exactRemover().RemovePreparedSandbox(ctx, runtimeID, runtimeUID); err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return err
	}
	_, err = p.repo.DeleteCleanup(ctx, claimed.PreparationID, claimed.CleanupToken, claimed.Revision)
	return err
}

func (p *FUSEPool) claimAndDestroy(ctx context.Context, record state.FUSEPoolRecord) error {
	claimed, err := p.claimCleanup(ctx, record)
	if err != nil {
		return err
	}
	if claimed.RuntimeID == "" {
		err = p.runtime.RemoveSandbox(ctx, claimed.PreparationID)
	} else {
		err = p.exactRemover().RemovePreparedSandbox(ctx, claimed.RuntimeID, claimed.RuntimeUID)
	}
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return fmt.Errorf("remove claimed FUSE runtime: %w", err)
	}
	deleted, err := p.repo.DeleteCleanup(ctx, claimed.PreparationID, claimed.CleanupToken, claimed.Revision)
	if err != nil {
		return fmt.Errorf("delete cleanup record: %w", err)
	}
	if !deleted {
		return state.ErrFUSEPoolConflict
	}
	if p.spec.WorkspaceFUSE != nil {
		metrics.RecordWorkspacePoolDiscard(ctx, p.spec.WorkspaceFUSE.RuntimeType, p.spec.WorkspaceFUSE.Provider, string(record.State))
	}
	return nil
}

func (p *FUSEPool) claimCleanup(ctx context.Context, record state.FUSEPoolRecord) (*state.FUSEPoolRecord, error) {
	return p.claimCleanupWithEvidence(ctx, record, record.RuntimeID, record.RuntimeUID)
}

func (p *FUSEPool) claimCleanupWithEvidence(ctx context.Context, record state.FUSEPoolRecord, runtimeID, runtimeUID string) (*state.FUSEPoolRecord, error) {
	digest := sha256.Sum256([]byte(record.PreparationID))
	token := p.config.MaintainerToken + ":cleanup:" + hex.EncodeToString(digest[:])
	return p.repo.ClaimCleanup(ctx, record.PreparationID, record.State, record.MaintainerToken, record.ReservationToken, record.Revision, runtimeID, runtimeUID, token, p.config.PrepareTimeout)
}

func (p *FUSEPool) exactRemover() runtime.PreparedSandboxRemover {
	return p.runtime.(runtime.PreparedSandboxRemover)
}

func (p *FUSEPool) unlockRefill(parent context.Context, token string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), fusePoolCleanupTimeout)
	defer cancel()
	return p.repo.UnlockRefill(ctx, p.poolKey, token)
}

type refillLease struct {
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

func (p *FUSEPool) startRefillLease(parent context.Context, token string) *refillLease {
	ctx, cancel := context.WithCancel(parent)
	lease := &refillLease{ctx: ctx, cancel: cancel, done: make(chan struct{})}
	interval := p.refillLockTTL() / 3
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	go func() {
		defer close(lease.done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ok, err := p.repo.RenewRefillLock(ctx, p.poolKey, token, p.refillLockTTL())
				if err != nil || !ok {
					cancel()
					return
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return lease
}

func (l *refillLease) stop() { l.cancel(); <-l.done }

func (p *FUSEPool) beginOperation(parent context.Context) (context.Context, func(), error) {
	p.lifecycleMu.Lock()
	if p.stopping || p.stopped {
		p.lifecycleMu.Unlock()
		return nil, nil, ErrFUSEPoolStopped
	}
	p.opWG.Add(1)
	control := p.loopCtx
	p.lifecycleMu.Unlock()
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(control, cancel)
	return ctx, func() { stop(); cancel(); p.opWG.Done() }, nil
}

func (p *FUSEPool) guard(ctx context.Context, record state.FUSEPoolRecord) (FUSEPoolDisposition, error) {
	if p.config.PristineGuard == nil {
		return FUSEPoolDispositionUnknown, ErrFUSEPoolReturnUnproven
	}
	disposition, err := p.config.PristineGuard(ctx, record)
	if err != nil {
		return FUSEPoolDispositionUnknown, err
	}
	if disposition < FUSEPoolPristine || disposition > FUSEPoolAbandoned {
		return FUSEPoolDispositionUnknown, errors.Join(ErrFUSEPoolReturnUnproven, err)
	}
	return disposition, err
}

func (p *FUSEPool) validateReservedRecord(record state.FUSEPoolRecord, token string) error {
	if record.PreparationID == "" || record.PoolKey != p.poolKey || record.RuntimeID == "" || record.RuntimeUID == "" || record.State != state.FUSEPoolReserved || token == "" || record.ReservationToken != token || record.Revision == 0 {
		return state.ErrFUSEPoolInvalidRecord
	}
	return nil
}

func (p *FUSEPool) validate() error {
	if p == nil || p.runtime == nil || p.repo == nil || p.config.PristineGuard == nil || p.spec.WorkspaceFUSE == nil || p.poolKey == "" || p.config.MinSize < 0 || p.config.MaxSize <= 0 || p.config.MinSize > p.config.MaxSize || p.config.RefillInterval <= 0 || p.config.PrepareTimeout <= 0 || p.config.ReservationTTL <= 0 || p.config.MaintainerToken == "" {
		return ErrInvalidFUSEPoolConfig
	}
	if _, ok := p.runtime.(runtime.PreparedSandboxRemover); !ok {
		return ErrInvalidFUSEPoolConfig
	}
	return nil
}

func (p *FUSEPool) refillLockTTL() time.Duration {
	// One lock covers a full worst-case fill plus inventory work.
	const maxDuration = time.Duration(1<<63 - 1)
	multiplier := p.config.MaxSize + 1
	if multiplier < 1 || uint64(multiplier) > uint64(maxDuration/p.config.PrepareTimeout) {
		return maxDuration
	}
	ttl := p.config.PrepareTimeout * time.Duration(multiplier)
	minimumTTL := p.config.RefillInterval
	if minimumTTL > maxDuration/2 {
		minimumTTL = maxDuration
	} else {
		minimumTTL *= 2
	}
	if ttl < minimumTTL {
		ttl = minimumTTL
	}
	return ttl
}

func isStalePoolMutation(err error) bool {
	return errors.Is(err, state.ErrFUSEPoolNotFound) || errors.Is(err, state.ErrFUSEPoolConflict) || errors.Is(err, state.ErrFUSEPoolCASMismatch) || errors.Is(err, state.ErrFUSEPoolTokenMismatch)
}

type fusePoolKeyProjection struct {
	Version              string                         `json:"version"`
	RuntimeType          string                         `json:"runtime_type"`
	Image                string                         `json:"image"`
	Memory               string                         `json:"memory"`
	MemoryRequest        string                         `json:"memory_request"`
	CPU                  string                         `json:"cpu"`
	CPURequest           string                         `json:"cpu_request"`
	Disk                 string                         `json:"disk"`
	TmpDisk              string                         `json:"tmp_disk"`
	PidLimit             int                            `json:"pid_limit"`
	ReadOnlyRootFS       bool                           `json:"read_only_root_fs"`
	RunAsUser            int64                          `json:"run_as_user"`
	SeccompProfile       string                         `json:"seccomp_profile"`
	Provider             string                         `json:"provider"`
	Driver               string                         `json:"driver"`
	Profile              string                         `json:"profile"`
	StorageIdentity      string                         `json:"storage_identity"`
	CredentialGeneration string                         `json:"credential_generation"`
	MounterImage         string                         `json:"mounter_image"`
	DockerImage          string                         `json:"docker_image"`
	SecretName           string                         `json:"secret_name"`
	CASecretKey          string                         `json:"ca_secret_key"`
	EndpointHostIPs      []string                       `json:"endpoint_host_ips"`
	Bucket               string                         `json:"bucket"`
	Endpoint             string                         `json:"endpoint"`
	Region               string                         `json:"region"`
	UseSSL               bool                           `json:"use_ssl"`
	CacheSize            string                         `json:"cache_size"`
	CacheMedium          string                         `json:"cache_medium"`
	MountTimeout         time.Duration                  `json:"mount_timeout"`
	FlushTimeout         time.Duration                  `json:"flush_timeout"`
	UnmountTimeout       time.Duration                  `json:"unmount_timeout"`
	LSMProfile           string                         `json:"lsm_profile"`
	MounterResources     fusePoolResourcesProjection    `json:"mounter_resources"`
	SystemEgress         fusePoolSystemEgressProjection `json:"system_egress"`
}

type fusePoolResourcesProjection struct {
	CPURequest              string `json:"cpu_request"`
	CPULimit                string `json:"cpu_limit"`
	MemoryRequest           string `json:"memory_request"`
	MemoryLimit             string `json:"memory_limit"`
	EphemeralStorageRequest string `json:"ephemeral_storage_request"`
	EphemeralStorageLimit   string `json:"ephemeral_storage_limit"`
}

type fusePoolSystemEgressProjection struct {
	Mode          runtime.SystemEgressMode `json:"mode"`
	DNSCIDRs      []string                 `json:"dns_cidrs"`
	DNSPorts      []int32                  `json:"dns_ports"`
	EndpointCIDRs []string                 `json:"endpoint_cidrs"`
	EndpointFQDNs []string                 `json:"endpoint_fqdns"`
	EndpointPorts []int32                  `json:"endpoint_ports"`
	ProxyURL      string                   `json:"proxy_url"`
}

// ComputeFUSEPoolKey hashes an explicit versioned projection. It deliberately
// cannot start including request fields when SandboxSpec evolves.
func ComputeFUSEPoolKey(spec runtime.SandboxSpec) (string, error) {
	if spec.WorkspaceFUSE == nil || spec.WorkspaceFUSE.RuntimeType == "" || spec.WorkspaceFUSE.CredentialGeneration == "" {
		return "", ErrInvalidFUSEPoolConfig
	}
	fuse := spec.WorkspaceFUSE
	projection := fusePoolKeyProjection{
		Version: fusePoolKeyVersion, RuntimeType: fuse.RuntimeType,
		Image: spec.Image, Memory: spec.Memory, MemoryRequest: spec.MemoryRequest,
		CPU: spec.CPU, CPURequest: spec.CPURequest, Disk: spec.Disk, TmpDisk: spec.TmpDisk,
		PidLimit: spec.PidLimit, ReadOnlyRootFS: spec.ReadOnlyRootFS, RunAsUser: spec.RunAsUser, SeccompProfile: spec.SeccompProfile,
		Provider: fuse.Provider, Driver: fuse.Driver, Profile: fuse.Profile, StorageIdentity: fuse.StorageIdentity,
		CredentialGeneration: fuse.CredentialGeneration, MounterImage: fuse.MounterImage, DockerImage: fuse.DockerImage,
		SecretName: fuse.SecretName, CASecretKey: fuse.CASecretKey, EndpointHostIPs: canonicalStrings(fuse.EndpointHostIPs),
		Bucket: fuse.Bucket, Endpoint: fuse.Endpoint, Region: fuse.Region, UseSSL: fuse.UseSSL,
		CacheSize: fuse.CacheSize, CacheMedium: fuse.CacheMedium, MountTimeout: fuse.MountTimeout, FlushTimeout: fuse.FlushTimeout, UnmountTimeout: fuse.UnmountTimeout,
		LSMProfile: fuse.LSMProfile,
		MounterResources: fusePoolResourcesProjection{
			CPURequest: fuse.MounterResources.CPURequest, CPULimit: fuse.MounterResources.CPULimit,
			MemoryRequest: fuse.MounterResources.MemoryRequest, MemoryLimit: fuse.MounterResources.MemoryLimit,
			EphemeralStorageRequest: fuse.MounterResources.EphemeralStorageRequest,
			EphemeralStorageLimit:   fuse.MounterResources.EphemeralStorageLimit,
		},
		SystemEgress: fusePoolSystemEgressProjection{
			Mode: fuse.SystemEgress.Mode, DNSCIDRs: canonicalStrings(fuse.SystemEgress.DNSCIDRs), DNSPorts: canonicalPorts(fuse.SystemEgress.DNSPorts),
			EndpointCIDRs: canonicalStrings(fuse.SystemEgress.EndpointCIDRs), EndpointFQDNs: canonicalStrings(fuse.SystemEgress.EndpointFQDNs),
			EndpointPorts: canonicalPorts(fuse.SystemEgress.EndpointPorts), ProxyURL: fuse.SystemEgress.ProxyURL,
		},
	}
	raw, err := json.Marshal(projection)
	if err != nil {
		return "", fmt.Errorf("marshal FUSE pool key: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return dedupeSorted(result)
}

func canonicalPorts(values []int32) []int32 {
	result := append([]int32(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	if len(result) == 0 {
		return result
	}
	out := result[:1]
	for _, value := range result[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func dedupeSorted(values []string) []string {
	if len(values) == 0 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func cloneFUSESandboxSpec(spec runtime.SandboxSpec) runtime.SandboxSpec {
	clone := spec
	clone.NetworkWhitelist = append([]string(nil), spec.NetworkWhitelist...)
	clone.Mounts = append([]runtime.Mount(nil), spec.Mounts...)
	if spec.Labels != nil {
		clone.Labels = make(map[string]string, len(spec.Labels))
		for key, value := range spec.Labels {
			clone.Labels[key] = value
		}
	}
	if spec.WorkspaceFUSE != nil {
		fuse := *spec.WorkspaceFUSE
		fuse.EndpointHostIPs = append([]string(nil), spec.WorkspaceFUSE.EndpointHostIPs...)
		fuse.SystemEgress.DNSCIDRs = append([]string(nil), spec.WorkspaceFUSE.SystemEgress.DNSCIDRs...)
		fuse.SystemEgress.DNSPorts = append([]int32(nil), spec.WorkspaceFUSE.SystemEgress.DNSPorts...)
		fuse.SystemEgress.EndpointCIDRs = append([]string(nil), spec.WorkspaceFUSE.SystemEgress.EndpointCIDRs...)
		fuse.SystemEgress.EndpointFQDNs = append([]string(nil), spec.WorkspaceFUSE.SystemEgress.EndpointFQDNs...)
		fuse.SystemEgress.EndpointPorts = append([]int32(nil), spec.WorkspaceFUSE.SystemEgress.EndpointPorts...)
		clone.WorkspaceFUSE = &fuse
	}
	return clone
}
