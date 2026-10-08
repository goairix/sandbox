package controlrunner

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"time"

	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
)

// TLS close_notify sets its own five-second write deadline. Clamp that setter
// to the original archive/query bound; other request modes remain unsealed.
type archiveDeadlineConn struct {
	net.Conn
	mu     sync.Mutex
	sealed time.Time
}

func (c *archiveDeadlineConn) seal(at time.Time) {
	c.mu.Lock()
	if c.sealed.IsZero() || at.Before(c.sealed) {
		c.sealed = at
	}
	c.mu.Unlock()
}

func (c *archiveDeadlineConn) limited(at time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.sealed.IsZero() && (at.IsZero() || c.sealed.Before(at)) {
		return c.sealed
	}
	return at
}

func (c *archiveDeadlineConn) SetDeadline(at time.Time) error {
	return c.Conn.SetDeadline(c.limited(at))
}
func (c *archiveDeadlineConn) SetWriteDeadline(at time.Time) error {
	return c.Conn.SetWriteDeadline(c.limited(at))
}

type archiveResponseConn struct {
	*tls.Conn
	deadline *archiveDeadlineConn
}

func (c *archiveResponseConn) sealArchiveDeadline(at time.Time) { c.deadline.seal(at) }

type archiveFrameWriter struct {
	ctx  context.Context
	conn net.Conn
}

func (w archiveFrameWriter) Write(data []byte) (int, error) {
	n := 0
	for len(data) > 0 {
		if err := w.ctx.Err(); err != nil {
			return n, err
		}
		size := min(len(data), 32768)
		if err := tr.WriteTaskWorkspaceArchiveFrame(w.conn, tr.ArchiveData, data[:size]); err != nil {
			return n, err
		}
		n += size
		data = data[size:]
	}
	return n, nil
}

func (s *Supervisor) dispatchTaskArchive(admission, session context.Context, conn net.Conn, e tr.TaskWorkspaceArchiveEnvelope) error {
	var err error
	if e.Purpose == "task_workspace_archive_query" {
		var wire []byte
		wire, err = s.queryWorkspaceArchive(admission, e)
		if err == nil {
			err = tr.WriteTaskWorkspaceArchiveFrame(conn, tr.ArchiveComplete, wire)
		}
		if err == nil {
			err = conn.(interface{ CloseWrite() error }).CloseWrite()
		}
	} else {
		var a *workspaceArchiveAttempt
		a, err = s.registerWorkspaceArchive(admission, session, conn, e)
		if err == nil {
			// A cancellation does not release the original handler's borrower.
			<-a.done
			s.finishWorkspaceArchive(a)
			return nil
		}
	}
	if err != nil && admission.Err() == nil {
		_ = writeFiniteTransportEvent(admission, conn, tr.EventError, []byte("request refused or outcome unknown"))
	}
	return err
}
