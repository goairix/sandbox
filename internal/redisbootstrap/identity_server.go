package redisbootstrap

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// IdentityServerOptions fixes the local observer and this member's signing key.
type IdentityServerOptions struct {
	Observer   LocalObserverOptions
	PrivateKey ed25519.PrivateKey
}

var errIdentityServer = errors.New("identity server failed")

// RunIdentityServer listens on the fixed sidecar port. Construction does not
// inspect the PVC, authenticate Redis, reserve an identity or await quorum.
func RunIdentityServer(ctx context.Context, options IdentityServerOptions) error {
	if ctx == nil {
		return errIdentityServer
	}
	handler, err := newIdentityServerHandler(options)
	if err != nil {
		return errIdentityServer
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	listener, err := net.Listen("tcp", ":18080")
	if err != nil {
		return errIdentityServer
	}
	return runIdentityServer(ctx, listener, handler)
}

func newIdentityServerHandler(options IdentityServerOptions) (http.Handler, error) {
	if !validProofPrivateKey(options.PrivateKey) {
		return nil, errIdentityServer
	}
	provider, err := NewLocalObserver(options.Observer)
	if err != nil {
		return nil, errIdentityServer
	}
	session, err := NewProofSession()
	if err != nil {
		return nil, errIdentityServer
	}
	handler, err := NewIdentityHandler(options.Observer.Cluster, options.Observer.Member, session, options.PrivateKey, provider)
	if err != nil {
		return nil, errIdentityServer
	}
	return handler, nil
}

func runIdentityServer(ctx context.Context, listener net.Listener, handler http.Handler) error {
	requestContext, cancel := context.WithCancel(ctx)
	defer cancel()
	server := &http.Server{
		ReadHeaderTimeout: time.Second,
		ReadTimeout:       3 * time.Second,
		WriteTimeout:      3 * time.Second,
		IdleTimeout:       time.Second,
		MaxHeaderBytes:    4096,
		ErrorLog:          log.New(io.Discard, "", 0),
		BaseContext:       func(net.Listener) context.Context { return requestContext },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet && r.URL.Path == "/healthz" && r.URL.RawPath == "" && r.URL.RawQuery == "" && !r.URL.ForceQuery {
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Content-Type", "text/plain")
				_, _ = io.WriteString(w, "ok\n")
				return
			}
			handler.ServeHTTP(w, r)
		}),
	}
	server.SetKeepAlivesEnabled(false)
	done := make(chan error, 1)
	go func() { done <- server.Serve(newIdentityLimitedListener(listener)) }()
	select {
	case <-ctx.Done():
		cancel()
		shutdownContext, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer shutdownCancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			_ = server.Close()
		}
		<-done
		return ctx.Err()
	case <-done:
		cancel()
		_ = server.Close()
		if err := ctx.Err(); err != nil {
			return err
		}
		return errIdentityServer
	}
}

// The cap applies before net/http creates a request goroutine. Overflow is
// discarded, not queued behind a capacity semaphore (including slow headers).
func newIdentityLimitedListener(listener net.Listener) net.Listener {
	return &identityLimitedListener{Listener: listener, slots: make(chan struct{}, 32)}
}

type identityLimitedListener struct {
	net.Listener
	slots chan struct{}
}

func (l *identityLimitedListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.slots <- struct{}{}:
			return &identityLimitedConnection{Conn: connection, release: func() { <-l.slots }}, nil
		default:
			_ = connection.Close()
		}
	}
}

type identityLimitedConnection struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *identityLimitedConnection) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
