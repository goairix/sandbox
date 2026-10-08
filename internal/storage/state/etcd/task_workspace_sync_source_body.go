package etcd

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

func (s *PreparedTaskWorkspaceSyncSource) openBody(ctx context.Context, e *taskWorkspaceSyncEntry) (*taskWorkspaceSyncBody, error) {
	if err := s.begin(ctx); err != nil {
		return nil, err
	}
	defer s.end()
	if s.broken || e == nil || e.self != e || e.origin != s || s.current != e || e.kind != tar.TypeReg || s.body != nil {
		return nil, errTaskArchive
	}
	if err := s.verify(ctx); err != nil {
		return nil, err
	}
	owned, cancel := context.WithCancel(ctx)
	b := &taskWorkspaceSyncBody{source: s, entry: e, ctx: owned, cancel: cancel, hash: sha256.New(), reader: io.NewSectionReader(taskWorkspaceSyncReadAt{s, owned}, e.offset, e.size)}
	b.self = b
	s.body = b
	return b, nil
}

func (b *taskWorkspaceSyncBody) Read(data []byte) (int, error) {
	if b == nil || b.self != b {
		return 0, errTaskArchive
	}
	b.mu.Lock()
	if b.closed || b.active {
		b.mu.Unlock()
		return 0, errTaskArchive
	}
	b.active = true
	b.done = make(chan struct{})
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.active = false; close(b.done); b.mu.Unlock() }()
	s := b.source
	if err := s.begin(b.ctx); err != nil {
		return 0, err
	}
	defer s.end()
	if err := s.guard(b.ctx); err != nil {
		return 0, err
	}
	if len(data) > 32768 {
		data = data[:32768]
	}
	n, err := b.reader.Read(data)
	if e := s.guard(b.ctx); e != nil {
		b.source.cancel()
		return n, errors.Join(err, e)
	}
	b.mu.Lock()
	b.bytes += int64(n)
	b.hash.Write(data[:n])
	if b.bytes == b.entry.size && (err == nil || err == io.EOF) {
		if hex.EncodeToString(b.hash.Sum(nil)) != b.entry.sha256 {
			err = errTaskArchive
		} else {
			b.complete = true
			err = io.EOF
		}
	}
	complete := b.complete
	b.mu.Unlock()
	if err != nil && err != io.EOF || err == io.EOF && !complete {
		s.cancel()
		return n, errors.Join(errTaskArchive, err)
	}
	return n, err
}

func (b *taskWorkspaceSyncBody) Close() error {
	if b == nil || b.self != b {
		return errTaskArchive
	}
	b.mu.Lock()
	b.closed = true
	active, done := b.active, b.done
	if active {
		b.cancel()
	}
	b.mu.Unlock()
	if active {
		<-done
	}
	b.mu.Lock()
	complete := b.complete
	b.mu.Unlock()
	if !complete {
		b.source.cancel()
		return errTaskArchive
	}
	b.cancel()
	return nil
}
