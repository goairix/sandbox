package etcdgateway

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Serve owns HTTP accept, sockets, handler cancellation and physical join. The
// caller closes its Deployment only after this function has returned.
func Serve(ctx context.Context, listener net.Listener, gateway *Gateway) error {
	if ctx == nil || listener == nil || gateway == nil {
		return errors.New("invalid HTTP server configuration")
	}
	server := &http.Server{Handler: gateway, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: time.Duration(gateway.settings.MaxExecTimeoutSeconds)*time.Second + 30*time.Second, IdleTimeout: 120 * time.Second, MaxHeaderBytes: 32 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	var serveErr error
	consumed := false
	select {
	case serveErr = <-done:
		consumed = true
	case <-ctx.Done():
	}
	gateway.stop()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	shutdownErr := server.Shutdown(shutdown)
	cancel()
	if shutdownErr != nil {
		shutdownErr = errors.Join(shutdownErr, server.Close())
	}
	if !consumed {
		serveErr = <-done
	}
	gateway.Close()
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	return errors.Join(serveErr, shutdownErr)
}
