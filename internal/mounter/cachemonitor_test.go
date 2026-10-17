package mounter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"
)

// These tests exercise the real watcher and file metadata: replacing incremental
// accounting with a fixed snapshot misses both growth and deletion assertions.
func TestCacheMonitorTracksGrowthRemovalAndIdleFreshness(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "entry")
	require.NoError(t, os.WriteFile(path, []byte("abc"), 0o600))
	monitor, err := startTestCacheMonitor(context.Background(), CacheMonitorConfig{Root: root, LimitBytes: 100})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, monitor.Close()) })

	waitCacheBytes(t, monitor, 3)
	first := monitor.Snapshot().ObservedAt
	require.Eventually(t, func() bool {
		usage := monitor.Snapshot()
		return usage.Known && usage.Bytes == 3 && usage.ObservedAt.After(first)
	}, 3*time.Second, 10*time.Millisecond)

	require.NoError(t, os.WriteFile(path, []byte("abcdefgh"), 0o600))
	waitCacheBytes(t, monitor, 8)
	require.NoError(t, os.Remove(path))
	waitCacheBytes(t, monitor, 0)
}

func TestCacheMonitorAccountsNewSubtree(t *testing.T) {
	root := t.TempDir()
	monitor, err := startTestCacheMonitor(context.Background(), CacheMonitorConfig{Root: root, LimitBytes: 100})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, monitor.Close()) })
	waitCacheBytes(t, monitor, 0)

	directory := filepath.Join(root, "new", "nested")
	require.NoError(t, os.MkdirAll(directory, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "file"), []byte("abcde"), 0o600))
	waitCacheBytes(t, monitor, 5)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "file"), []byte("abcdefg"), 0o600))
	waitCacheBytes(t, monitor, 7)
}

func TestCacheMonitorRejectsUntrustedEntryAndRetainsUnknown(t *testing.T) {
	root := t.TempDir()
	monitor, err := startTestCacheMonitor(context.Background(), CacheMonitorConfig{Root: root, LimitBytes: 100})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, monitor.Close()) })
	waitCacheBytes(t, monitor, 0)

	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(t.TempDir(), link))
	require.Eventually(t, func() bool { return !monitor.Snapshot().Known }, time.Second, time.Millisecond)
	require.NoError(t, os.Remove(link))
	require.Never(t, func() bool { return monitor.Snapshot().Known }, 50*time.Millisecond, time.Millisecond)
}

func TestCacheMonitorFailsClosedOnInventoryBudgets(t *testing.T) {
	for _, test := range []struct {
		name   string
		config CacheMonitorConfig
	}{
		{name: "entries", config: CacheMonitorConfig{MaxEntries: 1}},
		{name: "directories", config: CacheMonitorConfig{MaxDirectories: 1}},
		{name: "path bytes", config: CacheMonitorConfig{MaxPathBytes: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(root, "child"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "one"), []byte("a"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "two"), []byte("b"), 0o600))
			test.config.Root, test.config.LimitBytes = root, 100
			monitor, err := startTestCacheMonitor(context.Background(), test.config)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, monitor.Close()) })

			select {
			case <-monitor.Done():
			case <-time.After(time.Second):
				t.Fatal("inventory did not stop at its configured budget")
			}
			require.False(t, monitor.Snapshot().Known)
		})
	}
}

func TestCacheMonitorCloseInvalidatesSnapshotAndJoins(t *testing.T) {
	monitor, err := startTestCacheMonitor(context.Background(), CacheMonitorConfig{Root: t.TempDir(), LimitBytes: 100})
	require.NoError(t, err)
	waitCacheBytes(t, monitor, 0)
	require.NoError(t, monitor.Close())
	require.NoError(t, monitor.Close())
	require.False(t, monitor.Snapshot().Known)
	select {
	case <-monitor.Done():
	default:
		t.Fatal("Close returned before the worker joined")
	}
}

func waitCacheBytes(t *testing.T, monitor *CacheMonitor, bytes int64) {
	t.Helper()
	require.Eventually(t, func() bool {
		usage := monitor.Snapshot()
		return usage.Known && usage.Bytes == bytes && usage.LimitBytes == 100
	}, 3*time.Second, 5*time.Millisecond, "cache accounting did not settle to %d bytes", bytes)
}

// Production permits only Linux/inotify, whose watches do not enumerate an
// unbounded number of children. Tiny native fixtures also exercise accounting
// and close ownership using kqueue; these runs do not certify Linux delivery.
func startTestCacheMonitor(ctx context.Context, config CacheMonitorConfig) (*CacheMonitor, error) {
	if runtime.GOOS == "linux" {
		return StartCacheMonitor(ctx, config)
	}
	return startCacheMonitor(ctx, config, newFSNotifyCacheWatcher)
}

func TestCacheMonitorWatcherOverflowInvalidatesProof(t *testing.T) {
	watcher := &testCacheWatcher{events: make(chan fsnotify.Event, 4), errors: make(chan error, 1)}
	monitor, err := startCacheMonitor(context.Background(), CacheMonitorConfig{Root: t.TempDir(), LimitBytes: 100}, func(uint) (cacheWatcher, error) {
		return watcher, nil
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, monitor.Close()) })
	waitCacheBytes(t, monitor, 0)

	watcher.errors <- fsnotify.ErrEventOverflow
	select {
	case <-monitor.Done():
	case <-time.After(time.Second):
		t.Fatal("overflow did not stop the monitor")
	}
	require.False(t, monitor.Snapshot().Known)
}

func TestCacheMonitorDirectoryRenameAndRemovalStayUnknown(t *testing.T) {
	for _, operation := range []string{"rename", "remove"} {
		t.Run(operation, func(t *testing.T) {
			root := t.TempDir()
			directory := filepath.Join(root, "child")
			require.NoError(t, os.Mkdir(directory, 0o700))
			monitor, err := startTestCacheMonitor(context.Background(), CacheMonitorConfig{Root: root, LimitBytes: 100})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, monitor.Close()) })
			waitCacheBytes(t, monitor, 0)

			if operation == "rename" {
				require.NoError(t, os.Rename(directory, filepath.Join(root, "renamed")))
			} else {
				require.NoError(t, os.Remove(directory))
			}
			select {
			case <-monitor.Done():
			case <-time.After(time.Second):
				t.Fatal("ambiguous directory change did not stop accounting")
			}
			require.False(t, monitor.Snapshot().Known)
		})
	}
}

func TestCacheMonitorEventBacklogExhaustionStaysUnknown(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "entry"), []byte("abc"), 0o600))
	watcher := &testCacheWatcher{events: make(chan fsnotify.Event, 4), errors: make(chan error, 1)}
	watcher.events <- fsnotify.Event{Name: filepath.Join(root, "entry"), Op: fsnotify.Write}
	watcher.events <- fsnotify.Event{Name: filepath.Join(root, "entry"), Op: fsnotify.Write}
	monitor, err := startCacheMonitor(context.Background(), CacheMonitorConfig{Root: root, LimitBytes: 100, EventBuffer: 1}, func(uint) (cacheWatcher, error) {
		return watcher, nil
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, monitor.Close()) })
	select {
	case <-monitor.Done():
	case <-time.After(time.Second):
		t.Fatal("event backlog was not bounded")
	}
	require.False(t, monitor.Snapshot().Known)
}

func TestCacheMonitorCancellationInvalidatesBeforeBlockedWorkJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	root := t.TempDir()
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	watcher := &testCacheWatcher{events: make(chan fsnotify.Event, 4), errors: make(chan error, 1)}
	watcher.add = func(path string) error {
		if filepath.Base(path) == "blocked" {
			close(entered)
			<-release
		}
		return nil
	}
	monitor, err := startCacheMonitor(ctx, CacheMonitorConfig{Root: root, LimitBytes: 100}, func(uint) (cacheWatcher, error) {
		return watcher, nil
	})
	require.NoError(t, err)
	waitCacheBytes(t, monitor, 0)

	require.NoError(t, os.Mkdir(filepath.Join(root, "blocked"), 0o700))
	watcher.events <- fsnotify.Event{Name: filepath.Join(root, "blocked"), Op: fsnotify.Create}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("watch installation did not begin")
	}
	cancel()
	require.False(t, monitor.Snapshot().Known)
	closed := make(chan struct{})
	go func() {
		_ = monitor.Close()
		close(closed)
	}()
	require.Never(t, func() bool {
		select {
		case <-closed:
			return true
		default:
			return false
		}
	}, 30*time.Millisecond, time.Millisecond)
	unblock.Do(func() { close(release) })
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not join the released worker")
	}
}

func TestCacheMonitorAtLimitRemainsAnObservation(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "entry"), make([]byte, 100), 0o600))
	monitor, err := startTestCacheMonitor(context.Background(), CacheMonitorConfig{Root: root, LimitBytes: 100})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, monitor.Close()) })
	waitCacheBytes(t, monitor, 100)
	usage := monitor.Snapshot()
	require.GreaterOrEqual(t, usage.Bytes, usage.LimitBytes)
}

func TestCacheMonitorPublicStartRejectsUnsupportedPlatform(t *testing.T) {
	monitor, err := StartCacheMonitor(context.Background(), CacheMonitorConfig{Root: t.TempDir(), LimitBytes: 100})
	if runtime.GOOS != "linux" {
		require.Error(t, err)
		require.Nil(t, monitor)
		return
	}
	require.NoError(t, err)
	require.NoError(t, monitor.Close())
}

func TestCacheMonitorCancellationCannotReturnPositiveObservation(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		ctx, cancel := context.WithCancel(context.Background())
		monitor, err := startTestCacheMonitor(ctx, CacheMonitorConfig{Root: t.TempDir(), LimitBytes: 100})
		require.NoError(t, err)
		waitCacheBytes(t, monitor, 0)

		cancel()
		usage := monitor.Snapshot()
		require.NoError(t, monitor.Close())
		require.False(t, usage.Known, "canceled observer returned a positive snapshot")
	}
}

func TestCacheMonitorLostWatchCannotRefreshPositiveObservation(t *testing.T) {
	watcher := &testCacheWatcher{events: make(chan fsnotify.Event, 4), errors: make(chan error, 1)}
	monitor, err := startCacheMonitor(context.Background(), CacheMonitorConfig{Root: t.TempDir(), LimitBytes: 100}, func(uint) (cacheWatcher, error) {
		return watcher, nil
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, monitor.Close()) })
	waitCacheBytes(t, monitor, 0)

	// Linux/inotify can remove a watch without a public fsnotify event when
	// its filesystem unmounts. The heartbeat must detect missing coverage.
	watcher.watches.Store(0)
	select {
	case <-monitor.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("lost directory coverage remained positive")
	}
	require.False(t, monitor.Snapshot().Known)
}

func TestCacheMonitorInvalidContextDoesNotAllocateWatcher(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "nil"},
		{name: "canceled", ctx: canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			var monitor *CacheMonitor
			var err error
			require.NotPanics(t, func() {
				monitor, err = startCacheMonitor(test.ctx, CacheMonitorConfig{Root: t.TempDir(), LimitBytes: 100}, func(uint) (cacheWatcher, error) {
					calls++
					return &testCacheWatcher{events: make(chan fsnotify.Event), errors: make(chan error)}, nil
				})
			})
			if monitor != nil {
				_ = monitor.Close()
			}
			require.Error(t, err)
			require.Zero(t, calls, "invalid context allocated a watcher")
		})
	}
}

func TestCacheMonitorCloseRetainsBackendFailureAndWaitsForReturn(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	want := errors.New("backend close did not prove its reader joined")
	watcher := &testCacheWatcher{events: make(chan fsnotify.Event), errors: make(chan error)}
	watcher.close = func() error {
		close(entered)
		<-release
		return want
	}
	monitor, err := startCacheMonitor(context.Background(), CacheMonitorConfig{Root: t.TempDir(), LimitBytes: 100}, func(uint) (cacheWatcher, error) {
		return watcher, nil
	})
	require.NoError(t, err)
	waitCacheBytes(t, monitor, 0)

	results := make(chan error, 2)
	go func() { results <- monitor.Close() }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("backend close did not begin")
	}
	go func() { results <- monitor.Close() }()
	require.False(t, monitor.Snapshot().Known)
	require.Never(t, func() bool {
		select {
		case <-monitor.Done():
			return true
		default:
			return false
		}
	}, 30*time.Millisecond, time.Millisecond)
	unblock.Do(func() { close(release) })
	for caller := 0; caller < 2; caller++ {
		select {
		case result := <-results:
			require.ErrorIs(t, result, want)
		case <-time.After(time.Second):
			t.Fatal("Close did not report backend failure after its return")
		}
	}
	require.ErrorIs(t, monitor.Close(), want)
	require.False(t, monitor.Snapshot().Known)
}

type testCacheWatcher struct {
	events  chan fsnotify.Event
	errors  chan error
	add     func(string) error
	close   func() error
	watches atomic.Int32
}

func (w *testCacheWatcher) Add(path string) error {
	if w.add != nil {
		if err := w.add(path); err != nil {
			return err
		}
	}
	w.watches.Add(1)
	return nil
}
func (w *testCacheWatcher) WatchCount() int { return int(w.watches.Load()) }
func (w *testCacheWatcher) Close() error {
	if w.close != nil {
		return w.close()
	}
	return nil
}
func (w *testCacheWatcher) Events() <-chan fsnotify.Event { return w.events }
func (w *testCacheWatcher) Errors() <-chan error          { return w.errors }
