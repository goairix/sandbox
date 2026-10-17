package mounter

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	// These are hard per-helper ceilings, not preallocated capacities. Names
	// and entries consume at most tens of MiB; Linux keeps one watch per dir.
	maxCacheEntries     = 32_768
	maxCacheDirectories = 1_024
	maxCachePathBytes   = 8 << 20
	maxCacheEvents      = 1_024
	cacheHeartbeat      = time.Second
)

// CacheMonitorConfig bounds resident cache accounting. Zero budget fields use
// the hard defaults; positive values may only tighten them. Root must be the
// trusted, private s3fs cache, never the FUSE mount or a shared writable tree.
type CacheMonitorConfig struct {
	Root           string
	LimitBytes     int64
	MaxEntries     int
	MaxDirectories int
	MaxPathBytes   int
	EventBuffer    int
}

// CacheUsage is a point-in-time local accounting observation. Known means the
// initial inventory completed and all delivered events settled. Callers must
// independently reject old/future ObservedAt values and Bytes >= LimitBytes.
// It is an observation, never a remote-flush or ownership-release receipt.
// Concurrent writes can race event delivery; this is sampled local health,
// not a linearizable filesystem quota or proof of physical disk consumption.
type CacheUsage struct {
	Bytes      int64
	LimitBytes int64
	ObservedAt time.Time
	Known      bool
}

// CacheMonitor owns one watcher and one accounting worker. Startup and newly
// created subtrees have bounded inventories; steady state uses per-path deltas.
// Any event loss, ambiguous directory move or exhausted budget is terminally
// unknown until restart. No periodic full-tree reconciliation is performed.
type CacheMonitor struct {
	usage  atomic.Pointer[CacheUsage]
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	// closeError is written before done closes and read only after that join.
	// A backend close failure is permanent; retrying fsnotify.Close cannot
	// repair its early-error path after its internal closed flag was set.
	closeError error
}

// StartCacheMonitor starts Linux/inotify accounting without waiting for the
// initial inventory. Snapshots remain unknown until that inventory settles.
// Other platforms fail closed: kqueue can enumerate/open every child inside Add
// before our inventory limits apply, so it cannot honor these production caps.
func StartCacheMonitor(ctx context.Context, config CacheMonitorConfig) (*CacheMonitor, error) {
	if runtime.GOOS != "linux" {
		return nil, fmt.Errorf("bounded cache monitoring requires Linux inotify")
	}
	return startCacheMonitor(ctx, config, newFSNotifyCacheWatcher)
}

// Snapshot returns a bounded, lock-free copy and performs no filesystem access.
func (m *CacheMonitor) Snapshot() CacheUsage {
	if usage := m.usage.Load(); usage != nil {
		if m.ctx.Err() != nil {
			return CacheUsage{LimitBytes: usage.LimitBytes}
		}
		return *usage
	}
	return CacheUsage{}
}

// Done closes after the accounting worker and its backend Close call return.
// Only a nil error from Close certifies that the backend reader also joined.
func (m *CacheMonitor) Done() <-chan struct{} { return m.done }

// Close invalidates readiness, cancels the worker and joins actual work. A
// blocked filesystem syscall retains this worker until it really returns;
// cancellation does not start a replacement or falsely release its resources.
// A backend close error leaves its reader lifetime uncertain and is retained
// for every concurrent or repeated caller; it cannot be repaired by retrying.
func (m *CacheMonitor) Close() error {
	m.cancel()
	<-m.done
	return m.closeError
}

type cacheWatcher interface {
	Add(string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
	WatchCount() int
}

type fsNotifyCacheWatcher struct{ *fsnotify.Watcher }

func (w fsNotifyCacheWatcher) Events() <-chan fsnotify.Event { return w.Watcher.Events }
func (w fsNotifyCacheWatcher) Errors() <-chan error          { return w.Watcher.Errors }
func (w fsNotifyCacheWatcher) WatchCount() int               { return len(w.Watcher.WatchList()) }

func newFSNotifyCacheWatcher(capacity uint) (cacheWatcher, error) {
	watcher, err := fsnotify.NewBufferedWatcher(capacity)
	if err != nil {
		return nil, err
	}
	return fsNotifyCacheWatcher{Watcher: watcher}, nil
}

func startCacheMonitor(ctx context.Context, config CacheMonitorConfig, factory func(uint) (cacheWatcher, error)) (*CacheMonitor, error) {
	if ctx == nil {
		return nil, fmt.Errorf("cache monitor context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := normalizeCacheMonitorConfig(&config); err != nil {
		return nil, err
	}
	watcher, err := factory(uint(config.EventBuffer))
	if err != nil {
		return nil, fmt.Errorf("start cache event watcher: %w", err)
	}
	workerContext, cancel := context.WithCancel(ctx)
	monitor := &CacheMonitor{ctx: workerContext, cancel: cancel, done: make(chan struct{})}
	monitor.usage.Store(&CacheUsage{LimitBytes: config.LimitBytes})
	go monitor.run(workerContext, config, watcher)
	return monitor, nil
}

func normalizeCacheMonitorConfig(config *CacheMonitorConfig) error {
	if !filepath.IsAbs(config.Root) || filepath.Clean(config.Root) != config.Root || config.LimitBytes <= 0 {
		return fmt.Errorf("cache monitor identity is invalid")
	}
	for _, budget := range []struct {
		value   *int
		ceiling int
	}{
		{&config.MaxEntries, maxCacheEntries},
		{&config.MaxDirectories, maxCacheDirectories},
		{&config.MaxPathBytes, maxCachePathBytes},
		{&config.EventBuffer, maxCacheEvents},
	} {
		if *budget.value == 0 {
			*budget.value = budget.ceiling
		}
		if *budget.value < 0 || *budget.value > budget.ceiling {
			return fmt.Errorf("cache monitor budget exceeds its hard ceiling")
		}
	}
	return nil
}

func (m *CacheMonitor) run(ctx context.Context, config CacheMonitorConfig, watcher cacheWatcher) {
	// Invalidate before closing the backend. fsnotify can return a close error
	// before its reader join, so Done proves worker return and Close's retained
	// result separately reports whether backend quiescence was certified.
	defer close(m.done)
	defer func() { m.closeError = watcher.Close() }()
	defer func() { m.usage.Store(&CacheUsage{LimitBytes: config.LimitBytes}) }()

	info, err := os.Lstat(config.Root)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	root, err := os.OpenRoot(config.Root)
	if err != nil {
		return
	}
	defer func() { _ = root.Close() }()
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		return
	}
	index := cacheIndex{config: config, root: root, rootIdentity: info, watcher: watcher, entries: make(map[string]cacheEntry)}
	if err := index.inventory(ctx, "."); err != nil {
		return
	}

	ticker := time.NewTicker(cacheHeartbeat)
	defer ticker.Stop()
	for {
		// Publish only after a bounded drain, including after initial inventory.
		// A sustained backlog becomes unknown instead of exposing half-applied
		// accounting or monopolizing the event loop indefinitely.
		m.usage.Store(&CacheUsage{LimitBytes: config.LimitBytes})
		if err := index.drain(ctx); err != nil {
			return
		}
		if err := index.verifyCoverage(); err != nil {
			return
		}
		m.usage.Store(&CacheUsage{Bytes: index.bytes, LimitBytes: config.LimitBytes, ObservedAt: time.Now(), Known: true})
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case event, ok := <-watcher.Events():
			m.usage.Store(&CacheUsage{LimitBytes: config.LimitBytes})
			if !ok || index.apply(ctx, event) != nil {
				return
			}
		case <-watcher.Errors():
			return
		}
	}
}

type cacheEntry struct {
	bytes     int64
	directory bool
}

// cacheIndex is exclusively owned by the accounting goroutine. Directory
// identities are never repaired after rename/remove because child watch names
// could refer to a different tree; that uncertainty must not become positive.
type cacheIndex struct {
	config       CacheMonitorConfig
	root         *os.Root
	rootIdentity os.FileInfo
	watcher      cacheWatcher
	entries      map[string]cacheEntry
	directories  int
	pathBytes    int
	bytes        int64
}

func (i *cacheIndex) verifyCoverage() error {
	// fsnotify suppresses Linux IN_IGNORED/IN_UNMOUNT events. Its bounded
	// watch list must still cover every indexed directory before a heartbeat
	// becomes positive. This scans watch metadata only, never the file tree.
	if i.watcher.WatchCount() != i.directories {
		return fmt.Errorf("cache directory watch coverage changed")
	}
	current, err := os.Lstat(i.config.Root)
	if err != nil || !os.SameFile(current, i.rootIdentity) || !current.IsDir() {
		return fmt.Errorf("cache root identity changed")
	}
	return nil
}

func (i *cacheIndex) inventory(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := i.root.Lstat(name)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cache directory is not trusted")
	}
	if _, exists := i.entries[name]; exists {
		return nil
	}
	if err := i.record(name, cacheEntry{directory: true}); err != nil {
		return err
	}
	// Linux Add installs one directory watch, before enumeration. The rooted
	// descriptor and O_NOFOLLOW reject symlink leaves and prevent tree escape.
	directory, err := i.root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	opened, err := directory.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return fmt.Errorf("cache directory identity changed")
	}
	if err := i.watcher.Add(filepath.Join(i.config.Root, name)); err != nil {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// Never ask ReadDir to allocate a slice for an entire large directory.
		children, readErr := directory.ReadDir(64)
		for _, child := range children {
			childName := filepath.Join(name, child.Name())
			if err := i.reconcile(ctx, childName); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func (i *cacheIndex) reconcile(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := i.root.Lstat(name)
	if os.IsNotExist(err) {
		old := i.entries[name]
		if old.directory {
			return fmt.Errorf("cache directory disappeared")
		}
		if _, exists := i.entries[name]; exists {
			i.bytes -= old.bytes
			i.pathBytes -= len(name)
			delete(i.entries, name)
		}
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("cache entry metadata is unknown")
	}
	if info.IsDir() {
		if old, exists := i.entries[name]; exists && !old.directory {
			return fmt.Errorf("cache entry kind changed")
		}
		return i.inventory(ctx, name)
	}
	if !info.Mode().IsRegular() || info.Size() < 0 {
		return fmt.Errorf("cache entry is unsupported")
	}
	return i.record(name, cacheEntry{bytes: info.Size()})
}

func (i *cacheIndex) record(name string, entry cacheEntry) error {
	old, exists := i.entries[name]
	if exists && old.directory != entry.directory {
		return fmt.Errorf("cache entry kind changed")
	}
	if !exists {
		if len(i.entries) >= i.config.MaxEntries || len(name) > i.config.MaxPathBytes-i.pathBytes {
			return fmt.Errorf("cache inventory budget exhausted")
		}
		if entry.directory && i.directories >= i.config.MaxDirectories {
			return fmt.Errorf("cache directory budget exhausted")
		}
		i.pathBytes += len(name)
		if entry.directory {
			i.directories++
		}
	}
	base := i.bytes - old.bytes
	if entry.bytes > math.MaxInt64-base {
		return fmt.Errorf("cache byte count overflow")
	}
	i.bytes = base + entry.bytes
	i.entries[name] = entry
	return nil
}

func (i *cacheIndex) apply(ctx context.Context, event fsnotify.Event) error {
	name, err := filepath.Rel(i.config.Root, event.Name)
	if err != nil || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) || filepath.IsAbs(name) {
		return fmt.Errorf("cache event is outside root")
	}
	if name == "." && event.Has(fsnotify.Remove|fsnotify.Rename) {
		return fmt.Errorf("cache root identity changed")
	}
	if old := i.entries[name]; old.directory && event.Has(fsnotify.Remove|fsnotify.Rename) {
		return fmt.Errorf("cache directory watch identity changed")
	}
	return i.reconcile(ctx, name)
}

func (i *cacheIndex) drain(ctx context.Context) error {
	for processed := 0; ; processed++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-i.watcher.Errors():
			return fmt.Errorf("cache event stream failed")
		default:
		}
		select {
		case event, ok := <-i.watcher.Events():
			if !ok || processed >= i.config.EventBuffer {
				return fmt.Errorf("cache event backlog exceeds budget")
			}
			if err := i.apply(ctx, event); err != nil {
				return err
			}
		default:
			return nil
		}
	}
}
