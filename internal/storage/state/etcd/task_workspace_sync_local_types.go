package etcd

import (
	"context"
	"path/filepath"
	"strings"
	"sync"

	"github.com/goairix/sandbox/internal/runtime/launcher"
)

// Copied physical diagnostics are part of the permanent charge, never authority.
type taskWorkspaceSyncLocalAttribution struct {
	Worker         launcher.WorkspaceSyncWorkerReference `json:"worker"`
	Root           string                                `json:"root"`
	Dev            uint64                                `json:"dev"`
	Ino            uint64                                `json:"ino"`
	MountID        uint64                                `json:"mount_id"`
	Filesystem     uint64                                `json:"filesystem"`
	FilesystemName string                                `json:"filesystem_name"`
	UID            uint32                                `json:"uid"`
	GID            uint32                                `json:"gid"`
}

func validWorkspaceSyncLocalRoot(root string) bool {
	return len(root) <= 4096 && filepath.IsAbs(root) && filepath.Clean(root) == root && root != "/" && validWorkspaceSyncPath(strings.TrimPrefix(root, "/"))
}

func workspaceSyncLocalFilesystemName(kind uint64, name string) bool {
	return kind == 0xef53 && name == "ext4" || kind == 0x58465342 && name == "xfs" || kind == 0x9123683e && name == "btrfs" || kind == 0x01021994 && name == "tmpfs"
}

func (p taskWorkspaceSyncLocalAttribution) Validate() error {
	if p.Worker.Validate() != nil || !validWorkspaceSyncLocalRoot(p.Root) || p.Dev == 0 || p.Ino == 0 || p.MountID == 0 || !workspaceSyncLocalFilesystemName(p.Filesystem, p.FilesystemName) || p.UID != p.Worker.UID || p.GID != p.Worker.GID {
		return ErrInvalidRecord
	}
	return nil
}

type taskWorkspaceSyncLocalTarget struct {
	self   *taskWorkspaceSyncLocalTarget
	worker *launcher.KernelBoundary
	plan   TaskWorkspaceSyncPlan
	pin    taskWorkspaceSyncLocalAttribution
	fd     int
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	sealed bool
	ops    sync.WaitGroup
	closed chan struct{}
	err    error
}

type taskWorkspaceSyncLocalWitness struct {
	self   *taskWorkspaceSyncLocalWitness
	origin *taskWorkspaceSyncLocalPut
	bytes  int64
	sha256 string
	dev    uint64
	ino    uint64
}

func (w *taskWorkspaceSyncLocalWitness) original() bool {
	return w != nil && w.self == w && w.origin != nil && w.origin.self == w.origin && w.origin.witness == w
}

func (t *taskWorkspaceSyncLocalTarget) begin(ctx context.Context) error {
	if t == nil || t.self != t || ctx == nil {
		return ErrInvalidRecord
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.sealed || ctx.Err() != nil || t.ctx.Err() != nil {
		return ErrGuardExpired
	}
	t.ops.Add(1)
	return nil
}

func (t *taskWorkspaceSyncLocalTarget) Close() error {
	if t == nil || t.self != t {
		return ErrInvalidRecord
	}
	t.mu.Lock()
	if t.sealed {
		t.mu.Unlock()
		<-t.closed
		return t.err
	}
	t.sealed = true
	t.cancel()
	t.mu.Unlock()
	t.ops.Wait()
	t.err = closeWorkspaceSyncLocalFD(t.fd)
	close(t.closed)
	return t.err
}

// prepareLocalPut returns this original one-shot owner with source/target Add
// registrations held. Task 2 consumes it directly and releases after real IO.
type taskWorkspaceSyncLocalPut struct {
	self          *taskWorkspaceSyncLocalPut
	source        *PreparedTaskWorkspaceSyncSource
	body          *taskWorkspaceSyncBody
	target        *taskWorkspaceSyncLocalTarget
	draft         *taskWorkspaceSyncIntentDraft
	ctx           context.Context
	cancel        context.CancelFunc
	stops         []func() bool
	once          sync.Once
	mu            sync.Mutex
	sealed        bool
	active        bool
	ioDone        chan struct{}
	guards        sync.WaitGroup
	guarding      bool
	nodes         int
	requestBytes  int
	successBytes  int
	failureBytes  int
	responseBytes int
	witness       *taskWorkspaceSyncLocalWitness
	// Test-only seam after an actual final target.check; protected by mu.
	probeComplete func()
}

func (p *taskWorkspaceSyncLocalPut) release() {
	p.once.Do(func() {
		p.mu.Lock()
		p.sealed = true
		p.cancel()
		p.mu.Unlock()
		p.guards.Wait() // no guarded RPC/readonly syscall outlives registration
		for _, stop := range p.stops {
			stop()
		}
		p.body.mu.Lock()
		p.body.active = false
		close(p.body.done)
		p.body.mu.Unlock()
		p.target.ops.Done()
		p.source.end()
	})
}

func (p *taskWorkspaceSyncLocalPut) Close() error {
	if p == nil || p.self != p {
		return ErrInvalidRecord
	}
	p.mu.Lock()
	p.sealed = true
	p.cancel()
	active, done := p.active, p.ioDone
	p.mu.Unlock()
	p.source.cancel()
	if active {
		<-done
	} else {
		p.release()
	}
	return nil
}

// The consumer must hold this registration until every real syscall returns.
func (p *taskWorkspaceSyncLocalPut) beginIO() error {
	if p == nil || p.self != p {
		return ErrInvalidRecord
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sealed || p.active || p.ctx.Err() != nil {
		return ErrGuardExpired
	}
	p.active = true
	p.ioDone = make(chan struct{})
	return nil
}

func (p *taskWorkspaceSyncLocalPut) finishIO() {
	p.release()
	p.mu.Lock()
	p.sealed = true
	p.active = false
	close(p.ioDone)
	p.mu.Unlock()
}
