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
	ErrFUSEPoolRefillBusy     = errors.New("FUSE pool refill is already in progress")
	ErrFUSEPoolAtCapacity     = errors.New("FUSE pool is at active preparation capacity")
)

// CanReturnPreparedFunc checks manager-owned owner/session/gate state. It must
// return nil only when the reserved shell has no published or pending owner.
type CanReturnPreparedFunc func(context.Context, state.FUSEPoolRecord) error

// FUSEPoolConfig configures the sandbox-api-owned FUSE shell pool.
type FUSEPoolConfig struct {
	MinSize           int
	MaxSize           int
	RefillInterval    time.Duration
	PrepareTimeout    time.Duration
	ReservationTTL    time.Duration
	MaintainerToken   string
	CanReturnPrepared CanReturnPreparedFunc
}

// FUSEPool maintains prefix-free runtime shells. Redis is the shared inventory
// and lock; this process remains the only active controller.
type FUSEPool struct {
	runtime runtime.Runtime
	repo    state.FUSEPoolRepository
	config  FUSEPoolConfig
	spec    runtime.SandboxSpec
	poolKey string

	lifecycleMu sync.Mutex
	starting    bool
	running     bool
	stopping    bool
	stopped     bool
	loopCtx     context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup
	stopOnce    sync.Once
	stopDone    chan struct{}
	stopErr     error
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
	p.lifecycleMu.Lock()
	if p.stopped || p.stopping {
		p.lifecycleMu.Unlock()
		return ErrFUSEPoolStopped
	}
	if p.running || p.starting {
		p.lifecycleMu.Unlock()
		return nil
	}
	p.starting = true
	p.wg.Add(1)
	p.lifecycleMu.Unlock()
	defer p.wg.Done()

	if err := p.Reconcile(ctx); err != nil {
		p.lifecycleMu.Lock()
		p.starting = false
		p.lifecycleMu.Unlock()
		return fmt.Errorf("initial FUSE pool reconciliation: %w", err)
	}

	p.lifecycleMu.Lock()
	defer p.lifecycleMu.Unlock()
	p.starting = false
	if p.stopped || p.stopping {
		return ErrFUSEPoolStopped
	}
	p.running = true
	p.wg.Add(1)
	go p.reconcileLoop(p.loopCtx)
	return nil
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
	return p.Reconcile(ctx)
}

// Acquire reserves a pristine prepared shell, or cold-prepares one carrying
// the request's token directly from preparing to reserved.
func (p *FUSEPool) Acquire(ctx context.Context, poolKey string) (*state.FUSEPoolRecord, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if poolKey == "" || poolKey != p.poolKey {
		return nil, ErrFUSEPoolKeyMismatch
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token := uuid.NewString()
		record, err := p.repo.ReservePrepared(ctx, poolKey, token, p.config.ReservationTTL)
		if err != nil {
			return nil, fmt.Errorf("reserve prepared FUSE sandbox: %w", err)
		}
		if record == nil {
			record, err = p.prepareCold(ctx, token)
			if err != nil {
				return nil, err
			}
			p.scheduleRefill()
			return record, nil
		}
		if err := p.validateReservedRecord(*record, token); err != nil {
			return nil, err
		}
		if err := p.runtime.PreparedSandboxHealth(ctx, record.RuntimeID, poolKey); err == nil {
			p.scheduleRefill()
			return record, nil
		} else {
			cleanupErr := p.destroyExact(ctx, *record)
			if cleanupErr != nil {
				return nil, errors.Join(fmt.Errorf("prepared FUSE sandbox health: %w", err), cleanupErr)
			}
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
	active, err := p.repo.CountPreparingAndPrepared(ctx, p.poolKey)
	if err != nil {
		return nil, fmt.Errorf("count cold FUSE preparation capacity: %w", err)
	}
	if active >= p.config.MaxSize {
		return nil, ErrFUSEPoolAtCapacity
	}
	return p.prepareOne(ctx, reservationToken)
}

// ReturnPrepared returns a reservation only after both runtime health and the
// manager-owned owner/session/gate guard independently prove it pristine.
func (p *FUSEPool) ReturnPrepared(ctx context.Context, record state.FUSEPoolRecord) error {
	if err := p.validateReservedRecord(record, record.ReservationToken); err != nil {
		return err
	}
	healthErr := p.runtime.PreparedSandboxHealth(ctx, record.RuntimeID, record.PoolKey)
	guardErr := error(nil)
	if p.config.CanReturnPrepared == nil {
		guardErr = ErrFUSEPoolReturnUnproven
	} else if err := p.config.CanReturnPrepared(ctx, record); err != nil {
		guardErr = errors.Join(ErrFUSEPoolReturnUnproven, err)
	}
	if healthErr != nil || guardErr != nil {
		cleanupErr := p.destroyExact(ctx, record)
		p.scheduleRefill()
		return errors.Join(guardErr, healthErr, cleanupErr)
	}
	_, err := p.repo.Transition(ctx, record.RuntimeUID, state.FUSEPoolReserved, state.FUSEPoolPrepared, record.ReservationToken, record.Revision)
	if err != nil {
		return fmt.Errorf("return FUSE sandbox to prepared: %w", err)
	}
	return nil
}

// ReleaseConsumed removes the exact runtime before deleting its inventory
// record. A failed removal leaves the record intact and retryable.
func (p *FUSEPool) ReleaseConsumed(ctx context.Context, record state.FUSEPoolRecord) error {
	if record.PoolKey != p.poolKey || (record.State != state.FUSEPoolBinding && record.State != state.FUSEPoolConsumed) {
		return state.ErrFUSEPoolInvalidRecord
	}
	if err := p.removeRuntime(ctx, record.RuntimeID); err != nil {
		return fmt.Errorf("remove consumed FUSE sandbox: %w", err)
	}
	deleted, err := p.repo.ConditionalDelete(ctx, record.RuntimeUID, record.State, record.MaintainerToken, record.ReservationToken, record.Revision)
	if err != nil {
		return fmt.Errorf("delete consumed FUSE pool record: %w", err)
	}
	if !deleted {
		return state.ErrFUSEPoolConflict
	}
	p.scheduleRefill()
	return nil
}

// Reconcile repairs the configured PoolKey while holding its distributed
// lock. Reserved/binding records are never recycled; consumed belongs to the
// persistent session lifecycle.
func (p *FUSEPool) Reconcile(ctx context.Context) (returnErr error) {
	if err := p.validate(); err != nil {
		return err
	}
	lockToken := p.config.MaintainerToken + ":" + uuid.NewString()
	locked, err := p.repo.TryRefillLock(ctx, p.poolKey, lockToken, p.refillLockTTL())
	if err != nil {
		return fmt.Errorf("acquire FUSE refill lock: %w", err)
	}
	if !locked {
		return nil
	}
	defer func() {
		if err := p.unlockRefill(ctx, lockToken); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("unlock FUSE refill lock: %w", err))
		}
	}()

	records, err := p.repo.ListByPoolKey(ctx, p.poolKey)
	if err != nil {
		return fmt.Errorf("list FUSE pool inventory: %w", err)
	}
	now := time.Now()
	active := make([]state.FUSEPoolRecord, 0)
	for _, record := range records {
		switch record.State {
		case state.FUSEPoolPreparing:
			expired := record.UpdatedAt.IsZero() || !record.UpdatedAt.Add(p.config.PrepareTimeout).After(now)
			if record.ReservationToken != "" && (record.ReservedUntil.IsZero() || !record.ReservedUntil.After(now)) {
				expired = true
			}
			if expired {
				if err := p.destroyExact(ctx, record); err != nil {
					return errors.Join(fmt.Errorf("clean expired preparing FUSE sandbox: %w", err))
				}
				continue
			}
			active = append(active, record)
		case state.FUSEPoolPrepared:
			if err := p.runtime.PreparedSandboxHealth(ctx, record.RuntimeID, p.poolKey); err != nil {
				if cleanupErr := p.destroyExact(ctx, record); cleanupErr != nil {
					return errors.Join(fmt.Errorf("probe prepared FUSE sandbox: %w", err), cleanupErr)
				}
				continue
			}
			active = append(active, record)
		case state.FUSEPoolReserved:
			if record.ReservedUntil.IsZero() || !record.ReservedUntil.After(now) {
				if err := p.destroyExact(ctx, record); err != nil {
					return fmt.Errorf("clean expired reserved FUSE sandbox: %w", err)
				}
			}
		case state.FUSEPoolBinding:
			if err := p.destroyExact(ctx, record); err != nil {
				return fmt.Errorf("clean binding FUSE sandbox: %w", err)
			}
		case state.FUSEPoolConsumed:
			// Persistent session teardown owns consumed instances.
		default:
			return state.ErrFUSEPoolCorrupt
		}
	}

	activeCount, err := p.repo.CountPreparingAndPrepared(ctx, p.poolKey)
	if err != nil {
		return fmt.Errorf("count active FUSE pool inventory: %w", err)
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
				if err := p.destroyExact(ctx, record); err != nil {
					return fmt.Errorf("trim FUSE pool: %w", err)
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
		return fmt.Errorf("recount active FUSE pool inventory: %w", err)
	}

	prepared := 0
	current, err := p.repo.ListByPoolKey(ctx, p.poolKey)
	if err != nil {
		return fmt.Errorf("refresh FUSE pool inventory: %w", err)
	}
	for _, record := range current {
		if record.State == state.FUSEPoolPrepared {
			prepared++
		}
	}
	for prepared < p.config.MinSize && activeCount < p.config.MaxSize {
		_, err := p.prepareOne(ctx, "")
		if err != nil {
			return fmt.Errorf("prepare FUSE pool sandbox: %w", err)
		}
		activeCount++
		prepared++
	}
	return nil
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
			result = errors.Join(result, p.destroyExact(ctx, record))
		}
	}
	return result
}

// Stop terminates all controller goroutines and removes only preparing or
// prepared shells owned by this API instance. It is idempotent.
func (p *FUSEPool) Stop(ctx context.Context) error {
	p.stopOnce.Do(func() {
		p.lifecycleMu.Lock()
		p.stopping = true
		if p.cancel != nil {
			p.cancel()
		}
		p.lifecycleMu.Unlock()
		p.wg.Wait()
		p.stopErr = p.drainOwned(ctx)
		p.lifecycleMu.Lock()
		p.running = false
		p.stopping = false
		p.stopped = true
		p.lifecycleMu.Unlock()
		close(p.stopDone)
	})
	select {
	case <-p.stopDone:
		return p.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
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
			result = errors.Join(result, p.destroyExact(ctx, record))
		}
	}
	return result
}

func (p *FUSEPool) reconcileLoop(ctx context.Context) {
	defer p.wg.Done()
	for {
		delay := p.config.RefillInterval
		if delay > 5*time.Nanosecond {
			delay += time.Duration(rand.Int64N(int64(delay / 5)))
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
			if err := p.Reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
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
	p.wg.Add(1)
	p.lifecycleMu.Unlock()
	go func() {
		defer p.wg.Done()
		if err := p.Reconcile(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error(ctx, "FUSE pool refill failed", logger.AddField("pool_key", p.poolKey), logger.ErrorField(err))
		}
	}()
}

func (p *FUSEPool) prepareOne(ctx context.Context, reservationToken string) (*state.FUSEPoolRecord, error) {
	prepareCtx, cancel := context.WithTimeout(ctx, p.config.PrepareTimeout)
	defer cancel()
	spec := cloneFUSESandboxSpec(p.spec)
	spec.ID = "sandbox-fuse-pool-" + randSuffix(randSuffixLen)
	info, err := p.runtime.PrepareSandbox(prepareCtx, spec)
	if err != nil {
		return nil, fmt.Errorf("prepare FUSE sandbox runtime: %w", err)
	}
	if info == nil || info.RuntimeID == "" || info.RuntimeUID == "" {
		if info != nil && info.RuntimeID != "" {
			_ = p.removeRuntimeWithCleanupContext(info.RuntimeID)
		}
		return nil, errors.Join(state.ErrFUSEPoolInvalidRecord, errors.New("runtime returned incomplete identity"))
	}
	record := state.FUSEPoolRecord{
		RuntimeID: info.RuntimeID, RuntimeUID: info.RuntimeUID, PoolKey: p.poolKey,
		State: state.FUSEPoolPreparing, MaintainerToken: p.config.MaintainerToken,
		ReservationToken: reservationToken, Revision: 1,
	}
	if reservationToken != "" {
		record.ReservedUntil = time.Now().Add(p.config.ReservationTTL)
	}
	if err := p.repo.CreatePreparing(prepareCtx, record); err != nil {
		cleanupErr := p.removeRuntimeWithCleanupContext(info.RuntimeID)
		return nil, errors.Join(fmt.Errorf("register preparing FUSE sandbox: %w", err), cleanupErr)
	}
	if err := p.runtime.PreparedSandboxHealth(prepareCtx, info.RuntimeID, p.poolKey); err != nil {
		cleanupErr := p.destroyExactWithCleanupContext(record)
		return nil, errors.Join(fmt.Errorf("verify preparing FUSE sandbox: %w", err), cleanupErr)
	}
	to := state.FUSEPoolPrepared
	token := p.config.MaintainerToken
	if reservationToken != "" {
		to = state.FUSEPoolReserved
		token = reservationToken
	}
	transitioned, err := p.repo.Transition(prepareCtx, record.RuntimeUID, state.FUSEPoolPreparing, to, token, record.Revision)
	if err != nil {
		cleanupErr := p.destroyExactWithCleanupContext(record)
		return nil, errors.Join(fmt.Errorf("publish prepared FUSE sandbox: %w", err), cleanupErr)
	}
	return transitioned, nil
}

func (p *FUSEPool) destroyExact(ctx context.Context, record state.FUSEPoolRecord) error {
	if err := p.removeRuntime(ctx, record.RuntimeID); err != nil {
		return fmt.Errorf("remove runtime %q: %w", record.RuntimeID, err)
	}
	deleted, err := p.repo.ConditionalDelete(ctx, record.RuntimeUID, record.State, record.MaintainerToken, record.ReservationToken, record.Revision)
	if err != nil {
		if isStalePoolMutation(err) {
			return nil
		}
		return fmt.Errorf("delete exact FUSE pool record: %w", err)
	}
	if !deleted {
		return state.ErrFUSEPoolConflict
	}
	return nil
}

func (p *FUSEPool) removeRuntime(ctx context.Context, runtimeID string) error {
	if runtimeID == "" {
		return state.ErrFUSEPoolInvalidRecord
	}
	err := p.runtime.RemoveSandbox(ctx, runtimeID)
	if errors.Is(err, runtime.ErrNotFound) {
		return nil
	}
	return err
}

func (p *FUSEPool) removeRuntimeWithCleanupContext(runtimeID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), fusePoolCleanupTimeout)
	defer cancel()
	return p.removeRuntime(ctx, runtimeID)
}

func (p *FUSEPool) destroyExactWithCleanupContext(record state.FUSEPoolRecord) error {
	ctx, cancel := context.WithTimeout(context.Background(), fusePoolCleanupTimeout)
	defer cancel()
	return p.destroyExact(ctx, record)
}

func (p *FUSEPool) unlockRefill(parent context.Context, token string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), fusePoolCleanupTimeout)
	defer cancel()
	return p.repo.UnlockRefill(ctx, p.poolKey, token)
}

func (p *FUSEPool) validateReservedRecord(record state.FUSEPoolRecord, token string) error {
	if record.PoolKey != p.poolKey || record.RuntimeID == "" || record.RuntimeUID == "" || record.State != state.FUSEPoolReserved || token == "" || record.ReservationToken != token || record.Revision == 0 {
		return state.ErrFUSEPoolInvalidRecord
	}
	return nil
}

func (p *FUSEPool) validate() error {
	if p == nil || p.runtime == nil || p.repo == nil || p.spec.WorkspaceFUSE == nil || p.poolKey == "" || p.config.MinSize < 0 || p.config.MaxSize <= 0 || p.config.MinSize > p.config.MaxSize || p.config.RefillInterval <= 0 || p.config.PrepareTimeout <= 0 || p.config.ReservationTTL <= 0 || p.config.MaintainerToken == "" {
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
		CredentialGeneration: fuse.CredentialGeneration, MounterImage: fuse.MounterImage,
		SecretName: fuse.SecretName, CASecretKey: fuse.CASecretKey, EndpointHostIPs: canonicalStrings(fuse.EndpointHostIPs),
		Bucket: fuse.Bucket, Endpoint: fuse.Endpoint, Region: fuse.Region, UseSSL: fuse.UseSSL,
		CacheSize: fuse.CacheSize, CacheMedium: fuse.CacheMedium, MountTimeout: fuse.MountTimeout, FlushTimeout: fuse.FlushTimeout,
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
