package controltransport

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type observedHalfCloser struct {
	net.Conn
	calls atomic.Int32
	fail  error
}

func (c *observedHalfCloser) CloseWrite() error {
	c.calls.Add(1)
	if c.fail != nil {
		return c.fail
	}
	return c.Conn.(*net.TCPConn).CloseWrite()
}

// Actual TLS must terminate its application stream before raw write EOF, while
// response bytes remain readable. The callback has no payload/credential input.
func TestSessionFiniteUnderlyingHalfClose(t *testing.T) {
	for _, mode := range []string{"supported", "unsupported", "error"} {
		t.Run(mode, func(t *testing.T) {
			options, server, _, _, _, _ := destinationFixture(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			marker := errors.New("owned half-close failed")
			var observed *observedHalfCloser
			options.Dial = func(ctx context.Context) (net.Conn, error) {
				raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
				if err != nil {
					return nil, err
				}
				if mode == "unsupported" {
					return struct{ net.Conn }{raw}, nil
				}
				observed = &observedHalfCloser{Conn: raw}
				if mode == "error" {
					observed.fail = marker
				}
				return observed, nil
			}
			destination, err := NewDestination(options)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				raw, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(time.Second))
				conn := tls.Server(raw, server)
				if err = conn.Handshake(); err == nil {
					_, err = ReadRequest(conn)
				}
				if err == nil && mode == "supported" {
					var b [1]byte
					var n int
					n, err = raw.Read(b[:])
					if n != 0 || !errors.Is(err, io.EOF) {
						err = errors.New("TLS EOF was not followed by real transport EOF")
					} else {
						err = nil
					}
				}
				if err == nil && mode != "error" {
					err = WriteEvent(conn, EventAccepted, []byte("read-side-preserved"))
				}
				done <- err
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			session, err := destination.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			sendErr := session.Send(ctx, frameRequest(t))
			if mode == "error" {
				if !errors.Is(sendErr, marker) {
					t.Errorf("lost underlying half-close error: %v", sendErr)
				}
			} else {
				if sendErr != nil {
					t.Error(sendErr)
				}
				kind, wire, err := session.Read(ctx)
				if err != nil || kind != EventAccepted || string(wire) != "read-side-preserved" {
					t.Errorf("response side lost: kind=%d data=%q err=%v", kind, wire, err)
				}
			}
			if observed != nil && observed.calls.Load() != 1 {
				t.Errorf("underlying half-close calls=%d want1", observed.calls.Load())
			}
			session.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-ctx.Done():
				t.Fatal("server did not join", ctx.Err())
			}
		})
	}
}
