package mounter

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/goairix/sandbox/internal/fuseprotocol"
)

const (
	// Checks retain the established local one-second response budget. Samples
	// expire after three missed one-second refreshes; a blocked call owns its
	// original worker and publishes unknown, regardless of context timeout.
	readinessRefreshInterval = time.Second
	readinessMaximumAge      = fuseprotocol.MounterReadinessMaximumAge
	// A pending cache heartbeat gets a bounded atomic-state retry so equal
	// one-second timers cannot remain locked to its brief unknown window.
	readinessPendingInterval = 250 * time.Millisecond
)

type readinessIdentity struct {
	runtimeUID   string
	poolKey      string
	generation   int64
	mountAttempt uint8
	mountID      uint64
	processDone  chan struct{}
	mountPath    string
	cacheRoot    string
	cacheLimit   int64
}

type readinessSample struct {
	identity   readinessIdentity
	observedAt time.Time
	sequence   uint64
}

// readinessObserver owns exactly one mount-verification worker and one cache
// monitor for its entire lifetime. Management establishes the initial ready
// mount through ReadyStatus; this observer only verifies that existing identity.
// Requests read sampled local observations, never management capabilities or
// remote-write/flush evidence. Concurrent filesystem changes can race sampling.
type readinessObserver struct {
	ctx        context.Context
	supervisor *Supervisor
	startCache func(context.Context, CacheMonitorConfig) (*CacheMonitor, error)
	cache      atomic.Pointer[CacheMonitor]
	sample     atomic.Pointer[readinessSample]
}

func newReadinessObserver(ctx context.Context, supervisor *Supervisor, startCache func(context.Context, CacheMonitorConfig) (*CacheMonitor, error)) *readinessObserver {
	return &readinessObserver{ctx: ctx, supervisor: supervisor, startCache: startCache}
}

func (o *readinessObserver) run() (result error) {
	// The original resident loop is also the Unix health consumer's owner.
	// Retain registration until its syscall and cache backend have actually
	// joined: cancellation must not allow a replacement probe to reuse the slot.
	s := o.supervisor
	if err := s.checkNativeBinding(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.readiness != nil {
		s.mu.Unlock()
		return fmt.Errorf("resident readiness observer is already running")
	}
	s.readiness = o
	s.mu.Unlock()
	defer func() {
		o.sample.Store(nil)
		if cache := o.cache.Load(); cache != nil {
			closeErr := cache.Close()
			result = errors.Join(result, closeErr)
			if closeErr != nil {
				// A failed backend Close does not certify its reader joined.
				// Retain this stopped owner so a replacement observer cannot
				// reuse the original accounting slot after uncertain teardown.
				return
			}
		}
		s.mu.Lock()
		if s.readiness == o {
			s.readiness = nil
		}
		s.mu.Unlock()
	}()

	for {
		if o.ctx.Err() != nil {
			return nil
		}
		pendingCache, err := o.refresh()
		if err != nil {
			return err
		}
		interval := readinessRefreshInterval
		if pendingCache {
			interval = readinessPendingInterval
		}
		timer := time.NewTimer(interval)
		select {
		case <-o.ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (o *readinessObserver) refresh() (bool, error) {
	// Invalidate before any potentially blocking syscall. This single worker
	// stays occupied until MountInfo actually returns, even after cancellation.
	o.sample.Store(nil)
	s := o.supervisor
	if !s.mu.TryLock() {
		return false, nil
	}
	bootstrapped := s.bootstrapped
	config := CacheMonitorConfig{Root: s.bootstrap.CacheDir, LimitBytes: s.bootstrap.CacheLimitBytes}
	identity, ready := s.readinessIdentityLocked()
	mountInfo := s.config.MountInfo
	s.mu.Unlock()
	if !bootstrapped {
		return false, nil
	}

	cache := o.cache.Load()
	if cache == nil {
		var err error
		cache, err = o.startCache(o.ctx, config)
		if err != nil {
			// A pure cancellation before allocation is normal shutdown. Keep
			// wrapped/joined backend failures: cancellation cannot certify them.
			if o.ctx.Err() != nil && err == o.ctx.Err() {
				return false, nil
			}
			return false, fmt.Errorf("start resident readiness cache: %w", err)
		}
		o.cache.Store(cache)
	}
	if !ready || mountInfo == nil {
		return false, nil
	}
	usage := cache.Snapshot()
	if !cacheReady(usage, identity.cacheLimit, time.Now()) {
		select {
		case <-cache.Done():
			return false, nil
		default:
			return !usage.Known, nil
		}
	}
	mount, err := mountInfo()
	if err != nil || mount.ID != identity.mountID || mount.MountPoint != identity.mountPath || mount.FilesystemType != "fuse.s3fs" {
		return false, nil
	}

	// Management may have stopped or changed the exact generation/process
	// while the syscall was in flight; its old result cannot bless a successor.
	if !s.mu.TryLock() {
		return false, nil
	}
	current, ready := s.readinessIdentityLocked()
	if ready && current == identity && o.ctx.Err() == nil && cacheReady(cache.Snapshot(), identity.cacheLimit, time.Now()) {
		if s.readinessSequence == ^uint64(0) {
			s.mu.Unlock()
			return false, fmt.Errorf("resident readiness sequence exhausted")
		}
		s.readinessSequence++
		o.sample.Store(&readinessSample{identity: identity, observedAt: time.Now(), sequence: s.readinessSequence})
	}
	s.mu.Unlock()
	return false, nil
}

func (o *readinessObserver) ready(now time.Time) bool {
	if !o.supervisor.mu.TryLock() {
		return false
	}
	defer o.supervisor.mu.Unlock()
	_, err := o.statusLocked(now)
	return err == nil
}

// statusLocked returns only a resident observation bound to this exact mount
// and process completion channel. The caller holds Supervisor.mu. It performs
// no filesystem work, never extends sample age and grants no flush or release
// authority. An in-flight probe has already cleared sample before its syscall.
func (o *readinessObserver) statusLocked(now time.Time) (fuseprotocol.MounterStatus, error) {
	_, usage, err := o.snapshotLocked(now)
	if err != nil {
		return fuseprotocol.MounterStatus{}, err
	}
	status := o.supervisor.statusLocked()
	status.CacheBytes, status.CacheLimitBytes = usage.Bytes, usage.LimitBytes
	status.CacheExceeded = usage.Bytes >= usage.LimitBytes
	return status, nil
}

// Both wire versions consume one checked sample/cache snapshot under the same
// supervisor lock. In particular, Version2 cannot validate Version1 then load a
// different cache observation which has become unknown between those reads.
func (o *readinessObserver) snapshotLocked(now time.Time) (*readinessSample, CacheUsage, error) {
	if o.ctx.Err() != nil {
		return nil, CacheUsage{}, fmt.Errorf("resident readiness is stopped")
	}
	sample := o.sample.Load()
	if sample == nil || !readinessFresh(sample.observedAt, now) {
		return nil, CacheUsage{}, fmt.Errorf("resident readiness sample is unknown")
	}
	cache := o.cache.Load()
	if cache == nil {
		return nil, CacheUsage{}, fmt.Errorf("resident cache sample is unknown")
	}
	usage := cache.Snapshot()
	if !cacheReady(usage, sample.identity.cacheLimit, now) {
		return nil, CacheUsage{}, fmt.Errorf("resident cache sample is unknown")
	}
	current, ready := o.supervisor.readinessIdentityLocked()
	if !ready || current != sample.identity {
		return nil, CacheUsage{}, fmt.Errorf("resident readiness identity changed")
	}
	return sample, usage, nil
}

// residentReadiness returns no management capability and never establishes a
// first mount. Its finite lifetime uses the source times' monotonic components;
// UTC serialization happens only after both ages and the exact tuple are checked.
func (s *Supervisor) residentReadiness(ctx context.Context) (fuseprotocol.MounterReadiness, error) {
	if err := s.checkNativeBinding(); err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	if !s.mu.TryLock() {
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("resident readiness is busy")
	}
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	if err := s.validateNativeLocked(); err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	if s.readiness == nil || s.readiness.supervisor != s {
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("resident readiness observer is unavailable")
	}
	now := time.Now()
	sample, usage, err := s.readiness.snapshotLocked(now)
	if err != nil {
		return fuseprotocol.MounterReadiness{}, err
	}
	remaining := min(readinessMaximumAge-now.Sub(sample.observedAt), readinessMaximumAge-now.Sub(usage.ObservedAt))
	if sample.sequence == 0 || remaining <= 0 || remaining > readinessMaximumAge {
		return fuseprotocol.MounterReadiness{}, fmt.Errorf("resident readiness lifetime is unavailable")
	}
	i := sample.identity
	return fuseprotocol.MounterReadiness{
		Version: fuseprotocol.MounterReadinessVersion, RuntimeUID: i.runtimeUID, PoolKey: i.poolKey,
		Generation: i.generation, MountAttempt: i.mountAttempt, MountID: i.mountID, SampleSequence: sample.sequence,
		SampledAt: sample.observedAt.UTC(), CacheObservedAt: usage.ObservedAt.UTC(), ValidForNanoseconds: int64(remaining),
	}, nil
}

// readinessIdentityLocked reads only resident state and the already-owned child
// completion channel. It must not call Status, MountInfo or any cache syscall.
// A native seal invalidates existing samples immediately without changing the
// mount state that the original Shutdown still needs for cleanup.
func (s *Supervisor) readinessIdentityLocked() (readinessIdentity, bool) {
	if s.checkNativeBinding() != nil {
		return readinessIdentity{}, false
	}
	if s.nativeOwner != nil && (s.nativeOwner.revoked || s.nativeOwner.closed) {
		return readinessIdentity{}, false
	}
	if !s.bootstrapped || s.state != StateReady || s.stopping || s.shutdownTried || s.process == nil || s.processDone == nil || s.mountID == 0 || s.auth.LeaseGeneration <= 0 || s.auth.MountAttempt != 1 || s.bootstrap.RuntimeUID == "" || s.bootstrap.PoolKey == "" {
		return readinessIdentity{}, false
	}
	select {
	case <-s.processDone:
		return readinessIdentity{}, false
	default:
	}
	return readinessIdentity{
		runtimeUID:   s.bootstrap.RuntimeUID,
		poolKey:      s.bootstrap.PoolKey,
		generation:   s.auth.LeaseGeneration,
		mountAttempt: s.auth.MountAttempt,
		mountID:      s.mountID,
		processDone:  s.processDone,
		mountPath:    s.bootstrap.MountPath,
		cacheRoot:    s.bootstrap.CacheDir,
		cacheLimit:   s.bootstrap.CacheLimitBytes,
	}, true
}

func cacheReady(usage CacheUsage, limit int64, now time.Time) bool {
	return usage.Known && limit > 0 && usage.LimitBytes == limit && usage.Bytes >= 0 && usage.Bytes < limit && readinessFresh(usage.ObservedAt, now)
}

func readinessFresh(observedAt, now time.Time) bool {
	return !observedAt.IsZero() && !now.Before(observedAt) && now.Sub(observedAt) <= readinessMaximumAge
}
