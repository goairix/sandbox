package etcd

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

var errTaskArchive = errors.New("invalid private task archive")

type taskArchiveLimits struct {
	Jobs            uint64
	JobBytes        uint64
	TotalBytes      uint64
	Entries         uint64
	JobIndexBytes   uint64
	TotalIndexBytes uint64
}

// These types grant neither current KV authority nor an external effect.
type taskArchiveStore struct {
	self               *taskArchiveStore
	mu                 sync.Mutex
	root               *os.File
	limits             taskArchiveLimits
	indexBytes         uint64
	jobs, disk, memory uint64
	spools             map[*taskArchiveSpool]struct{}
	closed             bool
	closeDone          chan struct{}
	closeErr           error
	recovery           *taskArchiveRecoveryStore
}

type taskArchiveSpool struct {
	self                     *taskArchiveSpool
	store                    *taskArchiveStore
	name                     string
	file                     *os.File
	identity                 taskArchiveIdentity
	index                    []byte
	ctx                      context.Context
	cancel                   context.CancelFunc
	active, started, closing bool
	ownerDone, closeDone     chan struct{}
	closeErr                 error
	witness                  *taskReceivedArchive
	consumer                 *PreparedTaskWorkspaceSyncSource // one immutable registration, store.mu
	recovery                 *taskArchiveRecoveryOwner
	recoverySeedTask         TaskReference // immutable at registration, including failed initialization
}

type taskArchiveExpected struct {
	verifier                    *p.ManagementVerifier
	destination                 *t.Destination
	envelope                    t.TaskWorkspaceArchiveEnvelope
	context                     p.TaskWorkspaceArchiveContext
	digest                      string
	before, after, sessionAfter time.Time
	now                         func(context.Context) (time.Time, error)
	beforeOpen, beforeSend      func(context.Context) error
	beforeWitness               func(context.Context) error
}

type taskArchiveDiagnostics struct {
	Bytes, Entries              uint64
	SHA256                      string
	Context                     p.TaskWorkspaceArchiveContext
	TicketDigest                string
	Before, After, SessionAfter time.Time
}

type taskReceivedArchive struct {
	self        *taskReceivedArchive
	origin      *taskArchiveSpool
	diagnostics taskArchiveDiagnostics
}

func (w *taskReceivedArchive) inspect() (taskArchiveDiagnostics, error) {
	if w == nil || w.self != w || w.origin == nil || w.origin.self != w.origin || w.origin.store == nil || w.origin.store.self != w.origin.store {
		return taskArchiveDiagnostics{}, errTaskArchive
	}
	s := w.origin.store
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || w.origin.closing || w.origin.witness != w {
		return taskArchiveDiagnostics{}, errTaskArchive
	}
	return w.diagnostics, nil
}
