package launcher

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

var ErrWorkspaceArchive = errors.New("workspace archive unavailable")

// WorkspaceArchiveSummary describes a completed local stream. It grants no
// permission to settle remote effects, open gates, or release an owner.
type WorkspaceArchiveSummary struct {
	ArchiveBytes uint64
	EntryCount   uint64
	SHA256       [32]byte
}

// WorkspaceArchiveSource is an original, single-use pinned /workspace handle.
// Close waits for a synchronous stream to physically return; cancellation does
// not interrupt an arbitrary blocked reader or caller writer.
type WorkspaceArchiveSource struct {
	mu          sync.Mutex
	self        *WorkspaceArchiveSource
	origin      *KernelBoundary
	observation *UserNamespaceQuiescenceObservation
	root        *os.File
	dev, ino    uint64
	used        bool
	limits      archiveLimits
}

type archiveLimits struct {
	depth, pathBytes int
	entries, bytes   uint64
}

func productionArchiveLimits() archiveLimits {
	return archiveLimits{depth: 128, pathBytes: 4096, entries: 1_000_000, bytes: 1 << 40}
}

func (l archiveLimits) checkEntry(depth, pathBytes int, entries uint64) error {
	if depth > l.depth || pathBytes > l.pathBytes || entries >= l.entries {
		return fmt.Errorf("%w: traversal limit", ErrWorkspaceArchive)
	}
	return nil
}

func archiveComponentValid(name string) bool {
	if name == "" || name == "." || name == ".." || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// archiveOutput accounts every byte, including tar padding and end records.
// A destination short write is a terminal failure, never a successful prefix.
type archiveOutput struct {
	ctx          context.Context
	dst          io.Writer
	hash         hash.Hash
	bytes, limit uint64
}

func (w *archiveOutput) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.bytes > w.limit || uint64(len(p)) > w.limit-w.bytes {
		return 0, fmt.Errorf("%w: byte limit", ErrWorkspaceArchive)
	}
	n, err := w.dst.Write(p)
	if n < 0 || n > len(p) {
		return 0, fmt.Errorf("%w: invalid writer count", ErrWorkspaceArchive)
	}
	if n > 0 {
		w.bytes += uint64(n)
		_, _ = w.hash.Write(p[:n])
	}
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func (s *WorkspaceArchiveSource) WriteArchive(ctx context.Context, dst io.Writer) (summary WorkspaceArchiveSummary, err error) {
	if s == nil || s.self != s {
		return WorkspaceArchiveSummary{}, ErrWorkspaceArchive
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.used || s.root == nil {
		return WorkspaceArchiveSummary{}, ErrWorkspaceArchive
	}
	s.used = true
	defer func() {
		closeErr := s.root.Close()
		s.root = nil
		if closeErr != nil {
			summary = WorkspaceArchiveSummary{}
			if err == nil {
				err = closeErr
			}
		}
	}()
	return s.writeArchive(ctx, dst)
}

func (s *WorkspaceArchiveSource) Close() error {
	if s == nil || s.self != s {
		return ErrWorkspaceArchive
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.used = true
	if s.root == nil {
		return nil
	}
	err := s.root.Close()
	s.root = nil
	return err
}
