package etcd

import (
	"archive/tar"
	"context"
	"io"
)

// Retained original bounded stream adapter; TAR traversal itself uses the
// shared mechanical cursor, and the original ReadAt authority stays strict.
type taskWorkspaceSyncStream struct {
	source *PreparedTaskWorkspaceSyncSource
	ctx    context.Context
	offset int64
}

func (r *taskWorkspaceSyncStream) Read(data []byte) (int, error) {
	c := taskWorkspaceSyncArchiveCursor{bytes: r.source.diagnostics.Bytes, offset: r.offset, ctx: r.ctx, readAt: func(ctx context.Context, data []byte, offset int64) (int, error) {
		return (taskWorkspaceSyncReadAt{r.source, ctx}).ReadAt(data, offset)
	}}
	n, err := c.Read(data)
	r.offset = c.offset
	return n, err
}

func (s *PreparedTaskWorkspaceSyncSource) next(ctx context.Context) (entry *taskWorkspaceSyncEntry, resultErr error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	defer s.end()
	defer func() {
		if resultErr != nil && resultErr != io.EOF {
			s.broken = true
			s.cancel()
		}
	}()
	if s.broken {
		return nil, errTaskArchive
	}
	if s.current != nil && s.current.kind == tar.TypeReg {
		if s.body == nil {
			return nil, errTaskArchive
		}
		s.body.mu.Lock()
		complete := s.body.complete && !s.body.active
		if s.archive.owner.backend.workspaceSyncBudget != nil && s.archive.owner.backend.workspaceSyncBudget.profile == 2 && s.body.localConsumed {
			d := s.body.intent
			complete = complete && d != nil && d.localSettlement.original() && d.localSettlement.completed
		}
		s.body.mu.Unlock()
		if !complete {
			return nil, errTaskArchive
		}
	}
	s.current, s.body = nil, nil
	if err := s.verify(ctx); err != nil {
		return nil, err
	}
	e, err := s.cursor.next(ctx)
	s.count = s.cursor.count
	if err != nil {
		if err == io.EOF {
			if guardErr := s.guard(ctx); guardErr != nil {
				return nil, guardErr
			}
		}
		return nil, err
	}
	if err := s.verify(ctx); err != nil {
		return nil, err
	}
	entry = &taskWorkspaceSyncEntry{origin: s, path: e.path, kind: e.kind, mode: e.mode, size: e.size, offset: e.offset, sha256: e.sha256}
	entry.self, s.current = entry, entry
	return entry, nil
}
