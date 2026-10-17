package mounter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

const (
	// ReadinessPort is the resident, read-only kubelet HTTP port. Management
	// remains exclusively on the separately authenticated Unix control socket.
	ReadinessPort = 9808
	// ReadinessPath is the only path exposed by the resident HTTP listener.
	ReadinessPath = "/readyz"

	// Eight active connections and one-second I/O deadlines bound slow-client
	// work per helper. net/http adds its fixed 4 KiB parsing buffer allowance
	// to the configured 1 KiB maximum header size; request bodies are rejected.
	maxReadinessConnections = 8
	readinessIOTimeout      = time.Second
	readinessHeaderBytes    = 1 << 10
)

// ReadinessServer serves fixed boolean health on the Pod network for kubelet.
// It deliberately has no authentication credential or management dispatcher:
// any reachable caller, including a user sharing the Pod network namespace,
// can observe only readiness. No Service or execution capability is created.
// The owning process must join Serve before tearing down the supervisor.
type ReadinessServer struct {
	Supervisor *Supervisor
	// Address defaults to :9808 because kubelet reaches the Pod IP rather than
	// container loopback. A specific address is useful for an explicit binding.
	Address string
}

// Serve owns the listener, bounded HTTP connections, one local observer and its
// original cache monitor. Cancellation closes network admission immediately,
// then joins every server/connection and the observer's actual syscall return.
// An uncertain cache backend close error is retained in the returned error.
func (s *ReadinessServer) Serve(ctx context.Context) error {
	if ctx == nil || s.Supervisor == nil {
		return fmt.Errorf("resident readiness configuration is invalid")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	address := s.Address
	if address == "" {
		address = fmt.Sprintf(":%d", ReadinessPort)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("listen for resident readiness: %w", err)
	}
	return s.serve(ctx, listener, StartCacheMonitor)
}

func (s *ReadinessServer) serve(ctx context.Context, listener net.Listener, startCache func(context.Context, CacheMonitorConfig) (*CacheMonitor, error)) error {
	defer func() { _ = listener.Close() }()
	serveContext, cancel := context.WithCancel(ctx)
	defer cancel()
	observer := newReadinessObserver(serveContext, s.Supervisor, startCache)
	var connections sync.WaitGroup
	server := &http.Server{
		Handler:           newReadinessHTTPHandler(observer),
		ReadHeaderTimeout: readinessIOTimeout,
		ReadTimeout:       readinessIOTimeout,
		WriteTimeout:      readinessIOTimeout,
		IdleTimeout:       readinessIOTimeout,
		MaxHeaderBytes:    readinessHeaderBytes,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				connections.Add(1)
			case http.StateClosed:
				connections.Done()
			}
		},
	}
	// There is one bounded request per connection; callers cannot retain idle
	// keepalive slots or gain a multiplexed channel into management.
	server.SetKeepAlivesEnabled(false)
	results := make(chan error, 2)
	closed := make(chan error, 1)
	go func() {
		<-serveContext.Done()
		closed <- server.Close()
	}()
	go func() { results <- observer.run() }()
	go func() {
		err := server.Serve(newReadinessListener(listener, maxReadinessConnections))
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		results <- err
	}()

	first := <-results
	cancel()
	second := <-results
	closeError := <-closed
	// Serve has returned, so ConnState cannot add another connection. Close
	// interrupts socket reads; StateClosed certifies each owned handler exited.
	connections.Wait()
	return errors.Join(first, second, closeError)
}

func newReadinessHTTPHandler(observer *readinessObserver) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		writer.Header().Set("Connection", "close")
		request.Close = true
		status, body := http.StatusServiceUnavailable, "not ready\n"
		switch {
		case request.URL.Path != ReadinessPath || request.URL.RawQuery != "":
			status = http.StatusNotFound
		case request.Method != http.MethodGet:
			status = http.StatusMethodNotAllowed
		case request.ContentLength != 0 || len(request.TransferEncoding) != 0:
			status = http.StatusBadRequest
		case observer.ready(time.Now()):
			status, body = http.StatusOK, "ready\n"
		}
		writer.WriteHeader(status)
		_, _ = io.WriteString(writer, body)
	})
}

// readinessListener reserves a bounded slot before Accept, so the HTTP server
// never starts an unbounded number of slow connection goroutines. Closing the
// listener also wakes a blocked admission wait; only actual Conn.Close returns
// its slot. The wrapper has one acceptor, owned by http.Server.Serve.
type readinessListener struct {
	net.Listener
	slots      chan struct{}
	closed     chan struct{}
	closeOnce  sync.Once
	closeError error
}

func newReadinessListener(listener net.Listener, limit int) *readinessListener {
	return &readinessListener{Listener: listener, slots: make(chan struct{}, limit), closed: make(chan struct{})}
}

func (l *readinessListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case l.slots <- struct{}{}:
	}
	connection, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &readinessConnection{Conn: connection, release: func() { <-l.slots }}, nil
}

func (l *readinessListener) Close() error {
	l.closeOnce.Do(func() {
		close(l.closed)
		l.closeError = l.Listener.Close()
	})
	return l.closeError
}

type readinessConnection struct {
	net.Conn
	release    func()
	closeOnce  sync.Once
	closeError error
}

func (c *readinessConnection) Close() error {
	c.closeOnce.Do(func() {
		c.closeError = c.Conn.Close()
		c.release()
	})
	return c.closeError
}
