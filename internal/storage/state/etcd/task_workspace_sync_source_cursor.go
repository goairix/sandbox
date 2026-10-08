package etcd

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
)

// Offset counts bytes actually consumed by archive/tar, including PAX headers.
type taskWorkspaceSyncStream struct {
	source *PreparedTaskWorkspaceSyncSource
	ctx    context.Context
	offset int64
}

func (r *taskWorkspaceSyncStream) Read(data []byte) (int, error) {
	if r.offset == int64(r.source.diagnostics.Bytes) {
		return 0, io.EOF
	}
	if int64(len(data)) > int64(r.source.diagnostics.Bytes)-r.offset {
		data = data[:int64(r.source.diagnostics.Bytes)-r.offset]
	}
	n, err := (taskWorkspaceSyncReadAt{r.source, r.ctx}).ReadAt(data, r.offset)
	r.offset += int64(n)
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
		s.body.mu.Unlock()
		if !complete {
			return nil, errTaskArchive
		}
	}
	s.current, s.body = nil, nil
	if err := s.verify(ctx); err != nil {
		return nil, err
	}
	s.stream.ctx = ctx
	var buffer [32768]byte
	for {
		h, err := s.reader.Next()
		if err == io.EOF {
			if s.count != s.diagnostics.Entries || s.stream.offset != int64(s.diagnostics.Bytes) {
				return nil, errTaskArchive
			}
			if err := s.guard(ctx); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}
		if err != nil {
			return nil, err
		}
		if s.count >= s.diagnostics.Entries || h.Size < 0 || h.Size > int64(s.diagnostics.Bytes)-s.stream.offset || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) {
			return nil, errTaskArchive
		}
		name, err := taskArchivePath(h, s.count == 0)
		if err != nil {
			return nil, err
		}
		kind, err := taskArchiveIndex(ctx, s.archive.owner.spool.index, name, 0, false)
		if err != nil || kind != h.Typeflag {
			return nil, errTaskArchive
		}
		offset := s.stream.offset
		hash := sha256.New()
		n, err := io.CopyBuffer(hash, s.reader, buffer[:])
		if err != nil || n != h.Size {
			return nil, errors.Join(errTaskArchive, err)
		}
		s.count++
		if name == "workspace" {
			continue
		}
		path := strings.TrimPrefix(name, "workspace/")
		excluded, err := s.plan.Excludes(path)
		if err != nil {
			return nil, err
		}
		if excluded {
			continue
		}
		if err := s.verify(ctx); err != nil {
			return nil, err
		}
		e := &taskWorkspaceSyncEntry{origin: s, path: path, kind: h.Typeflag, mode: h.Mode, size: h.Size, offset: offset, sha256: hex.EncodeToString(hash.Sum(nil))}
		e.self = e
		s.current = e
		return e, nil
	}
}
