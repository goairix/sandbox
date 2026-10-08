package etcd

import (
	"archive/tar"
	"context"
	"hash"
	"io"
	"sync"
)

// PreparedTaskWorkspaceSyncSource is an original private source owner.
// It grants no provider operation or remote settlement evidence.
type PreparedTaskWorkspaceSyncSource struct {
	self        *PreparedTaskWorkspaceSyncSource
	archive     *ReceivedTaskWorkspaceArchive
	plan        TaskWorkspaceSyncPlan
	diagnostics taskArchiveDiagnostics
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	mu          sync.Mutex
	sealed      bool
	busy        bool
	ops         sync.WaitGroup
	reader      *tar.Reader
	stream      *taskWorkspaceSyncStream
	count       uint64
	current     *taskWorkspaceSyncEntry
	body        *taskWorkspaceSyncBody
	broken      bool
	// Test-only completion seam: runs after an actual original file.ReadAt.
	// Accessed under mu; never a public reader or substitute file.
	readComplete func()
}

type taskWorkspaceSyncEntry struct {
	self   *taskWorkspaceSyncEntry
	origin *PreparedTaskWorkspaceSyncSource
	path   string
	kind   byte
	mode   int64
	size   int64
	offset int64
	sha256 string
}

type taskWorkspaceSyncBody struct {
	self     *taskWorkspaceSyncBody
	source   *PreparedTaskWorkspaceSyncSource
	entry    *taskWorkspaceSyncEntry
	ctx      context.Context
	mu       sync.Mutex
	cancel   context.CancelFunc
	active   bool
	closed   bool
	done     chan struct{}
	reader   *io.SectionReader
	hash     hash.Hash
	bytes    int64
	complete bool
}
