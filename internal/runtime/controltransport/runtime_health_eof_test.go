package controltransport

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The ciphertext comes from the original TLS server. Only its delivery is
// scripted, so packet boundaries or sleeps cannot hide coalesced tail bytes.
type healthCipherBatch struct {
	ready  chan struct{}
	once   sync.Once
	reader *bytes.Reader
}

func (b *healthCipherBatch) publish(wire []byte) {
	b.once.Do(func() { b.reader = bytes.NewReader(wire); close(b.ready) })
}

type healthCoalescedConn struct {
	*net.TCPConn
	batch    *healthCipherBatch
	ctx      context.Context
	response atomic.Bool
	first    atomic.Int32
}

func (c *healthCoalescedConn) CloseWrite() error {
	if err := c.TCPConn.CloseWrite(); err != nil {
		return err
	}
	c.response.Store(true)
	return nil
}

func (c *healthCoalescedConn) Read(p []byte) (int, error) {
	if !c.response.Load() {
		return c.TCPConn.Read(p)
	}
	select {
	case <-c.batch.ready:
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	}
	if c.batch.reader.Len() > 0 {
		n, err := c.batch.reader.Read(p)
		c.first.CompareAndSwap(0, int32(n))
		return n, err
	}
	return c.TCPConn.Read(p)
}

type healthCaptureConn struct {
	net.Conn
	capture    bool
	ciphertext bytes.Buffer
}

func (c *healthCaptureConn) Write(p []byte) (int, error) {
	if c.capture {
		return c.ciphertext.Write(p)
	}
	return c.Conn.Write(p)
}

func TestRuntimeHealthCoalescedCiphertextEOF(t *testing.T) {
	for _, tail := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "coalesced-tail"}[tail], func(t *testing.T) {
			o, server, _, _, _, _ := destinationFixture(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			batch := &healthCipherBatch{ready: make(chan struct{})}
			var original *healthCoalescedConn
			o.Dial = func(dialCtx context.Context) (net.Conn, error) {
				raw, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", listener.Addr().String())
				if err != nil {
					return nil, err
				}
				original = &healthCoalescedConn{TCPConn: raw.(*net.TCPConn), batch: batch, ctx: ctx}
				return original, nil
			}
			d, err := NewDestination(o)
			require.NoError(t, err)
			done, finished := make(chan error, 1), make(chan struct{})
			var total atomic.Int32
			go func() {
				defer close(finished)
				defer batch.publish(nil)
				raw, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
				capture := &healthCaptureConn{Conn: raw}
				conn := tls.Server(capture, server)
				request, err := ReadRuntimeRequest(conn)
				if err != nil {
					done <- err
					return
				}
				if request.Health == nil {
					done <- errors.New("wrong health request")
					return
				}
				capture.capture = true
				e := request.Health
				err = WriteRuntimeHealthResponse(conn, RuntimeHealthResponse{Version: 1, Purpose: "runtime_health", Identity: e.Identity, DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, GateState: "open"})
				if err == nil {
					err = conn.CloseWrite()
				}
				if err != nil {
					done <- err
					return
				}
				wire := bytes.Clone(capture.ciphertext.Bytes())
				if tail {
					wire = append(wire, 255)
				}
				if len(wire) > 512 {
					done <- errors.New("fixture batch exceeds Go minimum read buffer")
					return
				}
				total.Store(int32(len(wire)))
				batch.publish(wire)
				done <- nil
			}()
			var joined sync.Once
			join := func() {
				joined.Do(func() { cancel(); listener.Close(); serverErr := <-done; <-finished; require.NoError(t, serverErr) })
			}
			t.Cleanup(join)
			response, err := d.QueryRuntimeHealth(ctx, 2)
			join()
			if !tail {
				require.NoError(t, err)
				require.Equal(t, "open", response.GateState)
				return
			}
			if err == nil {
				require.Equal(t, total.Load(), original.first.Load(), "old TLS read swallowed the entire ciphertext/notify/tail batch")
				t.Fatalf("health accepted coalesced trailing byte: first raw read=%d batch=%d", original.first.Load(), total.Load())
			}
			require.Equal(t, RuntimeHealthResponse{}, response)
		})
	}
}
