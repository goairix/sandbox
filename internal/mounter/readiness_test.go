package mounter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestResidentReadinessKeepsIdleMountHealthyWithoutControlCalls(t *testing.T) {
	supervisor, mount := residentReadySupervisor(t)
	var checks atomic.Int32
	supervisor.mu.Lock()
	supervisor.config.MountInfo = func() (Mount, error) {
		checks.Add(1)
		return mount, nil
	}
	supervisor.mu.Unlock()
	observer, cancel, joined := runTestReadinessObserver(supervisor)
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-joined)
	})

	waitResidentReady(t, observer)
	// More than the three-second positive-proof lifetime passes with no Unix
	// health RPC. The resident observer must refresh idle readiness itself.
	require.Eventually(t, func() bool { return checks.Load() >= 4 && observer.ready(time.Now()) }, 6*time.Second, 5*time.Millisecond)
}

func TestResidentReadinessRejectsStaleFutureTerminalAndContendedState(t *testing.T) {
	supervisor, _ := residentReadySupervisor(t)
	observer := sampledResidentObserver(supervisor)
	require.True(t, observer.ready(time.Now()))

	now := time.Now()
	require.False(t, observer.ready(now.Add(4*time.Second)))
	require.False(t, observer.ready(now.Add(-time.Second)))
	for _, state := range []State{StateLocked, StatePrepared, StateMounting, StateUnhealthy, StateRestartDetected, StateStopped} {
		supervisor.mu.Lock()
		supervisor.state = state
		supervisor.mu.Unlock()
		require.False(t, observer.ready(time.Now()), "state %s was reported ready", state)
	}

	supervisor.mu.Lock()
	result := make(chan bool, 1)
	go func() { result <- observer.ready(time.Now()) }()
	select {
	case ready := <-result:
		require.False(t, ready)
	case <-time.After(100 * time.Millisecond):
		t.Error("readiness blocked behind supervisor ownership")
	}
	supervisor.mu.Unlock()
}

func TestResidentReadinessRejectsIdentityChangeAndProcessExit(t *testing.T) {
	supervisor, _ := residentReadySupervisor(t)
	observer := sampledResidentObserver(supervisor)
	require.True(t, observer.ready(time.Now()))

	supervisor.mu.Lock()
	supervisor.auth.LeaseGeneration++
	supervisor.mu.Unlock()
	require.False(t, observer.ready(time.Now()))
	supervisor.mu.Lock()
	supervisor.auth.LeaseGeneration--
	supervisor.mountID++
	supervisor.mu.Unlock()
	require.False(t, observer.ready(time.Now()))
	supervisor.mu.Lock()
	supervisor.mountID--
	processDone := supervisor.processDone
	supervisor.processDone = make(chan struct{})
	close(supervisor.processDone)
	supervisor.mu.Unlock()
	require.False(t, observer.ready(time.Now()))
	supervisor.mu.Lock()
	supervisor.processDone = processDone
	supervisor.mu.Unlock()
}

func TestResidentReadinessBlockedMountRetainsOriginalWorker(t *testing.T) {
	supervisor, mount := residentReadySupervisor(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	var calls atomic.Int32
	supervisor.mu.Lock()
	supervisor.config.MountInfo = func() (Mount, error) {
		calls.Add(1)
		close(entered)
		<-release
		return mount, nil
	}
	supervisor.mu.Unlock()
	// Settle initial accounting before testing the independently owned mount
	// syscall slot; this test does not depend on two heartbeat phases aligning.
	supervisor.mu.Lock()
	cacheConfig := CacheMonitorConfig{Root: supervisor.bootstrap.CacheDir, LimitBytes: supervisor.bootstrap.CacheLimitBytes}
	supervisor.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cache, err := startTestCacheMonitor(ctx, cacheConfig)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return cache.Snapshot().Known }, time.Second, time.Millisecond)
	observer := newReadinessObserver(ctx, supervisor, func(context.Context, CacheMonitorConfig) (*CacheMonitor, error) { return cache, nil })
	joined := make(chan error, 1)
	go func() { joined <- observer.run() }()
	t.Cleanup(cancel)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		if cache := observer.cache.Load(); cache != nil {
			stopped := false
			select {
			case <-cache.Done():
				stopped = true
			default:
			}
			t.Fatalf("resident mount verification did not begin: cache=%+v stopped=%t state=%s", cache.Snapshot(), stopped, supervisor.State())
		}
		t.Fatal("resident mount verification did not begin: cache was not started")
	}
	require.False(t, observer.ready(time.Now()))
	// The potentially blocked kernel operation must not hold the management
	// mutex. Cancellation cannot certify that operation or launch another one.
	require.True(t, supervisor.mu.TryLock())
	supervisor.mu.Unlock()
	cancel()
	select {
	case <-joined:
		t.Fatal("observer returned before its original mount call exited")
	case <-time.After(50 * time.Millisecond):
	}
	require.Equal(t, int32(1), calls.Load())
	unblock.Do(func() { close(release) })
	require.NoError(t, <-joined)
	require.False(t, observer.ready(time.Now()))
}

func TestResidentReadinessCacheLimitAndUnknownFailClosed(t *testing.T) {
	supervisor, _ := residentReadySupervisor(t)
	observer := sampledResidentObserver(supervisor)
	require.True(t, observer.ready(time.Now()))
	cache := observer.cache.Load()
	usage := cache.Snapshot()
	usage.Bytes = usage.LimitBytes
	cache.usage.Store(&usage)
	require.False(t, observer.ready(time.Now()))
	cache.usage.Store(&CacheUsage{LimitBytes: usage.LimitBytes})
	require.False(t, observer.ready(time.Now()))
}

func TestResidentReadinessAvoidsAlignedUnknownCacheHeartbeats(t *testing.T) {
	supervisor, _ := residentReadySupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	observer := newReadinessObserver(ctx, supervisor, func(parent context.Context, config CacheMonitorConfig) (*CacheMonitor, error) {
		cacheContext, stop := context.WithCancel(parent)
		cache := &CacheMonitor{ctx: cacheContext, cancel: stop, done: make(chan struct{})}
		go func() {
			defer close(cache.done)
			unknownDuration := 100 * time.Millisecond
			for {
				// Model a bounded metadata check at the start of each one-second
				// cache heartbeat. A one-second-only observer can stay phase-locked
				// to this unknown window despite the cache being settled otherwise.
				cache.usage.Store(&CacheUsage{LimitBytes: config.LimitBytes})
				select {
				case <-cacheContext.Done():
					return
				case <-time.After(unknownDuration):
				}
				cache.usage.Store(&CacheUsage{Known: true, LimitBytes: config.LimitBytes, ObservedAt: time.Now()})
				select {
				case <-cacheContext.Done():
					return
				case <-time.After(800 * time.Millisecond):
				}
				unknownDuration = 200 * time.Millisecond
			}
		}()
		return cache, nil
	})
	joined := make(chan error, 1)
	go func() { joined <- observer.run() }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-joined)
	})
	waitResidentReady(t, observer)
}

func residentReadySupervisor(t *testing.T) (*Supervisor, Mount) {
	t.Helper()
	runner := &fakeRunner{}
	supervisor, bootstrap := newTestSupervisor(t, runner)
	require.NoError(t, supervisor.Authorize(context.Background(), validAuthorization()))
	mount := Mount{ID: 41, MountPoint: bootstrap.MountPath, FilesystemType: "fuse.s3fs"}
	supervisor.mu.Lock()
	supervisor.config.MountInfo = func() (Mount, error) { return mount, nil }
	supervisor.mu.Unlock()
	_, err := supervisor.ReadyStatus(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { runner.process.exit <- nil })
	return supervisor, mount
}

// sampledResidentObserver supplies an immutable settled observation for request
// boundary tests. No accounting producer can overwrite a deliberately injected
// stale/over-limit/unknown fixture during an assertion.
func sampledResidentObserver(supervisor *Supervisor) *readinessObserver {
	supervisor.mu.Lock()
	identity, _ := supervisor.readinessIdentityLocked()
	supervisor.mu.Unlock()
	now := time.Now()
	cache := &CacheMonitor{ctx: context.Background()}
	cache.usage.Store(&CacheUsage{Known: true, LimitBytes: identity.cacheLimit, ObservedAt: now})
	observer := newReadinessObserver(context.Background(), supervisor, nil)
	observer.cache.Store(cache)
	observer.sample.Store(&readinessSample{identity: identity, observedAt: now})
	return observer
}

func runTestReadinessObserver(supervisor *Supervisor) (*readinessObserver, context.CancelFunc, <-chan error) {
	ctx, cancel := context.WithCancel(context.Background())
	observer := newReadinessObserver(ctx, supervisor, startTestCacheMonitor)
	joined := make(chan error, 1)
	go func() { joined <- observer.run() }()
	return observer, cancel, joined
}

func waitResidentReady(t *testing.T, observer *readinessObserver) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	known := 0
	for !observer.ready(time.Now()) {
		if cache := observer.cache.Load(); cache != nil && cache.Snapshot().Known {
			known++
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			if cache := observer.cache.Load(); cache != nil {
				stopped := false
				select {
				case <-cache.Done():
					stopped = true
				default:
				}
				t.Fatalf("resident readiness did not settle: cache=%+v knownPolls=%d stopped=%t sample=%+v state=%s", cache.Snapshot(), known, stopped, observer.sample.Load(), observer.supervisor.State())
			}
			t.Fatal("resident readiness did not start cache accounting")
		}
	}
}
