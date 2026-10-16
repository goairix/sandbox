package controltransport

import (
	"fmt"
	"io"
	"net"
	"sync"
)

// NewFiniteTLSRecordConn preserves TLS record boundaries from the first read,
// including the handshake. Go TLS alone validates and authenticates the records.
// Raw EOF is truncation for TLS; a caller checks original adapter EOF separately
// after authenticated TLS close_notify. No ciphertext payload is buffered here.
func NewFiniteTLSRecordConn(raw net.Conn) net.Conn {
	c := &finiteTLSRecordConn{Conn: raw}
	if half, ok := raw.(interface{ CloseWrite() error }); ok {
		return &finiteHalfTLSRecordConn{finiteTLSRecordConn: c, half: half}
	}
	return c
}

type finiteTLSRecordConn struct {
	net.Conn
	mu          sync.Mutex
	header      [5]byte
	headerBytes int
	remaining   int
	pendingEOF  bool
	failure     error
}

func (c *finiteTLSRecordConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failure != nil {
		return 0, c.failure
	}
	if c.pendingEOF {
		c.failure = fmt.Errorf("finite TLS transport ended without close_notify: %w", io.ErrUnexpectedEOF)
		return 0, c.failure
	}
	limit := c.remaining
	header := limit == 0
	if header {
		limit = len(c.header) - c.headerBytes
	}
	if len(p) > limit {
		p = p[:limit]
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		if header {
			copy(c.header[c.headerBytes:], p[:n])
			c.headerBytes += n
			if c.headerBytes == len(c.header) {
				c.remaining = int(c.header[3])<<8 | int(c.header[4])
				if c.remaining == 0 {
					c.headerBytes = 0
				}
			}
		} else {
			c.remaining -= n
			if c.remaining == 0 {
				c.headerBytes = 0
			}
		}
	}
	if err == io.EOF && n > 0 {
		// net.Conn may return the final bytes together with EOF. Let TLS
		// authenticate a complete close_notify before exposing truncation on
		// another read. An incomplete record still fails on that next read.
		c.pendingEOF = true
		return n, nil
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		// Go TLS converts exact io.ErrUnexpectedEOF at a record boundary into
		// EOF. A wrapped error preserves the distinction from close_notify.
		err = fmt.Errorf("finite TLS transport ended without close_notify: %w", io.ErrUnexpectedEOF)
	}
	if err != nil {
		c.failure = err
	}
	return n, err
}

type finiteHalfTLSRecordConn struct {
	*finiteTLSRecordConn
	half interface{ CloseWrite() error }
}

func (c *finiteHalfTLSRecordConn) CloseWrite() error { return c.half.CloseWrite() }
