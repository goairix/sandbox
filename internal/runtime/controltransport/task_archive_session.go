package controltransport

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync/atomic"
)

type requestMode uint8

const (
	requestEvents requestMode = iota
	requestArchive
	requestPublication
	requestHealth
	requestRenewal
)

// Go TLS accepts a raw EOF between records as io.EOF without close_notify.
// Archive and enrolled publication sessions require authenticated EOF instead.
// Translate raw EOF only in those modes, preserving existing event behavior.
type archiveTLSConn struct {
	net.Conn
	strict atomic.Bool
}

func (c *archiveTLSConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if c.strict.Load() && (err == io.EOF || err == io.ErrUnexpectedEOF) {
		err = fmt.Errorf("archive transport ended without TLS close_notify: %w", io.ErrUnexpectedEOF)
	}
	return n, err
}

// Preserve CloseWrite capability only when the original adapter provides it.
type archiveHalfTLSConn struct {
	*archiveTLSConn
	half interface{ CloseWrite() error }
}

func (c *archiveHalfTLSConn) CloseWrite() error { return c.half.CloseWrite() }

func (s *Session) SendTaskWorkspaceArchive(ctx context.Context, e TaskWorkspaceArchiveEnvelope) error {
	return s.sendMode(ctx, requestArchive, func(w io.Writer) error { return WriteTaskWorkspaceArchiveRequest(w, e) })
}

// ReadTaskWorkspaceArchiveFrame enforces bounded framing and terminal ordering.
// It does not validate cumulative bytes, TAR entries, digest, or safe release.
func (s *Session) ReadTaskWorkspaceArchiveFrame(ctx context.Context) (byte, []byte, error) {
	if s == nil || s.self != s || isNil(ctx) {
		return 0, nil, ErrDestination
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sent || s.mode != requestArchive || s.archiveComplete {
		return 0, nil, ErrFrame
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	stop := closeOnContext(ctx, s.conn)
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := s.conn.SetReadDeadline(deadline); err != nil {
		return 0, nil, err
	}
	kind, wire, err := ReadTaskWorkspaceArchiveFrame(s.conn)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && kind == ArchiveComplete {
		s.archiveComplete = true
	}
	return kind, wire, err
}

// RequireTaskWorkspaceArchiveEOF reads exactly one byte after COMPLETE. Only
// authenticated TLS close_notify succeeds; trailing bytes and truncation fail.
func (s *Session) RequireTaskWorkspaceArchiveEOF(ctx context.Context) error {
	if s == nil || s.self != s || isNil(ctx) {
		return ErrDestination
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sent || s.mode != requestArchive || !s.archiveComplete || s.archiveEOFChecked {
		return ErrFrame
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := closeOnContext(ctx, s.conn)
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := s.conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	// A failed check cannot be retried to discard trailing data and turn an
	// invalid stream into a later apparent EOF proof.
	s.archiveEOFChecked = true
	var extra [1]byte
	n, err := s.conn.Read(extra[:])
	if n != 0 {
		return ErrFrame
	}
	if err != io.EOF {
		if err == nil {
			return ErrFrame
		}
		return err
	}
	return ctx.Err()
}
