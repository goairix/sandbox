package kubernetes

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// bridgeConn has one wire reader and one deadline waiter, both explicitly
// joined. Slots cover queued stdout and every entered caller I/O until Close.
type bridgeConn struct {
	self          *bridgeConn
	op            *bridgeOperation
	ws            *websocket.Conn
	mu            sync.Mutex
	abortErr      error
	terminal      error
	stop          chan struct{}
	readerDone    chan struct{}
	deadlineDone  chan struct{}
	changed       chan struct{}
	stdout        chan []byte
	readDeadline  time.Time
	writeDeadline time.Time
	ioWG          sync.WaitGroup
	readMu        sync.Mutex
	pending       []byte
	writeMu       sync.Mutex
	writeClosed   bool
	closeOnce     sync.Once
	closeDone     chan struct{}
}

func newBridgeConn(op *bridgeOperation, ws *websocket.Conn) *bridgeConn {
	c := &bridgeConn{op: op, ws: ws, stop: make(chan struct{}), readerDone: make(chan struct{}), deadlineDone: make(chan struct{}), changed: make(chan struct{}, 1), stdout: make(chan []byte, 16), closeDone: make(chan struct{})}
	c.self = c
	ws.SetReadLimit(64 << 10)
	// Do not let the protocol's default close handler write an unrequested reply
	// while the only reader is establishing terminal status.
	ws.SetCloseHandler(func(code int, text string) error { return &websocket.CloseError{Code: code, Text: text} })
	go c.readWire()
	go c.watchDeadline()
	return c
}

func (c *bridgeConn) begin() error {
	if c == nil || c.self != c {
		return ErrEnrolledBridge
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.abortErr != nil {
		return c.abortErr
	}
	c.ioWG.Add(1)
	return nil
}

func (c *bridgeConn) abort(err error) {
	c.mu.Lock()
	if c.abortErr == nil {
		c.abortErr = err
		close(c.stop)
	}
	c.mu.Unlock()
	// The original raw socket is closed first, including blocked frame writes.
	c.op.abort()
}

func (c *bridgeConn) Close() error {
	if c == nil || c.self != c {
		return ErrEnrolledBridge
	}
	c.closeOnce.Do(func() {
		c.abort(net.ErrClosed)
		c.ws.Close()
		<-c.readerDone
		<-c.deadlineDone
		c.ioWG.Wait()
		c.pending = nil
		for range c.stdout {
		}
		c.op.finish()
		close(c.closeDone)
	})
	<-c.closeDone
	return nil
}

func (c *bridgeConn) Read(p []byte) (int, error) {
	if err := c.begin(); err != nil {
		return 0, err
	}
	defer c.ioWG.Done()
	c.readMu.Lock()
	defer c.readMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	for len(c.pending) == 0 {
		select {
		case <-c.stop:
			c.mu.Lock()
			err := c.abortErr
			c.mu.Unlock()
			return 0, err
		case data, ok := <-c.stdout:
			if !ok {
				c.mu.Lock()
				err := c.terminal
				c.mu.Unlock()
				return 0, err
			}
			c.pending = data
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *bridgeConn) Write(p []byte) (int, error) {
	if err := c.begin(); err != nil {
		return 0, err
	}
	defer c.ioWG.Done()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeClosed {
		return 0, io.ErrClosedPipe
	}
	n := 0
	for len(p) != 0 {
		size := min(len(p), 32<<10)
		wire := make([]byte, size+1)
		copy(wire[1:], p[:size])
		if err := c.ws.WriteMessage(websocket.BinaryMessage, wire); err != nil {
			c.abort(err)
			return n, err
		}
		n += size
		p = p[size:]
	}
	return n, nil
}

func (c *bridgeConn) CloseWrite() error {
	if err := c.begin(); err != nil {
		return err
	}
	defer c.ioWG.Done()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeClosed {
		return nil
	}
	c.writeClosed = true
	if err := c.ws.WriteMessage(websocket.BinaryMessage, []byte{255, 0}); err != nil {
		c.abort(err)
		return err
	}
	return nil
}

func (c *bridgeConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *bridgeConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }

func (c *bridgeConn) SetDeadline(t time.Time) error      { return c.setDeadline(t, true, true) }
func (c *bridgeConn) SetReadDeadline(t time.Time) error  { return c.setDeadline(t, true, false) }
func (c *bridgeConn) SetWriteDeadline(t time.Time) error { return c.setDeadline(t, false, true) }

func (c *bridgeConn) setDeadline(t time.Time, read, write bool) error {
	if err := c.begin(); err != nil {
		return err
	}
	defer c.ioWG.Done()
	c.mu.Lock()
	if read {
		c.readDeadline = t
	}
	if write {
		c.writeDeadline = t
	}
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
	return nil
}

func (c *bridgeConn) deadline() time.Time {
	d := c.readDeadline
	if d.IsZero() || !c.writeDeadline.IsZero() && c.writeDeadline.Before(d) {
		d = c.writeDeadline
	}
	return d
}

func (c *bridgeConn) watchDeadline() {
	defer close(c.deadlineDone)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	for {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		c.mu.Lock()
		d := c.deadline()
		c.mu.Unlock()
		var expired <-chan time.Time
		if !d.IsZero() {
			timer.Reset(time.Until(d))
			expired = timer.C
		}
		select {
		case <-c.stop:
			return
		case <-c.changed:
		case <-expired:
			c.mu.Lock()
			current := c.deadline()
			due := !current.IsZero() && !current.After(time.Now())
			c.mu.Unlock()
			if due {
				c.abort(os.ErrDeadlineExceeded)
				return
			}
		}
	}
}

func (c *bridgeConn) readWire() {
	var status, stderr []byte
	terminal := error(ErrEnrolledBridge)
	defer func() {
		c.mu.Lock()
		c.terminal = terminal
		c.mu.Unlock()
		close(c.stdout)
		c.ws.Close()
		close(c.readerDone)
	}()
	for {
		kind, wire, err := c.ws.ReadMessage()
		if err != nil {
			terminal = err
			if websocket.IsCloseError(err, websocket.CloseNormalClosure) && len(stderr) == 0 && bridgeSuccess(status) {
				terminal = io.EOF
			}
			return
		}
		if kind != websocket.BinaryMessage || len(wire) == 0 {
			return
		}
		switch wire[0] {
		case 1:
			if len(wire) == 1 {
				continue
			}
			select {
			case c.stdout <- wire[1:]:
			case <-c.stop:
				return
			}
		case 2:
			if len(stderr)+len(wire)-1 > 4096 {
				return
			}
			stderr = append(stderr, wire[1:]...)
		case 3:
			if len(status)+len(wire)-1 > 16384 {
				return
			}
			status = append(status, wire[1:]...)
		default:
			return
		}
	}
}

func bridgeSuccess(wire []byte) bool {
	if len(wire) == 0 {
		return true
	}
	var status metav1.Status
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if d.Decode(&status) != nil || d.Decode(new(any)) != io.EOF {
		return false
	}
	return status.Status == metav1.StatusSuccess && status.Reason == "" && status.Details == nil && status.Message == "" && (status.Code == 0 || status.Code == 200) && (status.Kind == "" || status.Kind == "Status") && (status.APIVersion == "" || status.APIVersion == "v1")
}
