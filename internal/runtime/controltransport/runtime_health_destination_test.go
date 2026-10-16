package controltransport

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Each mode mutates the actual authenticated exchange, not a health stub.
func TestRuntimeHealthDestinationTLS(t *testing.T) {
	for _, mode := range []string{"success", "nonce", "identity", "gate", "closed", "trailing", "no-close-notify", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			o, server, _, _, _, _ := destinationFixture(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			o.Dial = func(ctx context.Context) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
			}
			destination, err := NewDestination(o)
			require.NoError(t, err)
			done := make(chan error, 1)
			finished := make(chan struct{})
			var once sync.Once
			var serverErr error
			join := func() { once.Do(func() { cancel(); listener.Close(); serverErr = <-done; <-finished }) }
			t.Cleanup(join)
			go func() {
				defer close(finished)
				raw, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(2 * time.Second))
				conn := tls.Server(raw, server)
				request, err := ReadRuntimeRequest(conn)
				if err != nil {
					done <- err
					return
				}
				if request.Health == nil || request.Health.Identity != o.Identity || request.Health.DataGateEpoch != 2 || len(request.Health.Nonce) != 64 {
					done <- errors.New("wrong health attribution")
					return
				}
				e := *request.Health
				response := RuntimeHealthResponse{Version: 1, Purpose: "runtime_health", Identity: e.Identity, DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, GateState: "open"}
				switch mode {
				case "nonce":
					response.Nonce = strings.Repeat("f", 64)
				case "identity":
					response.Identity.Runtime.UID = "other"
				case "gate":
					response.DataGateEpoch++
				case "closed":
					response.GateState = "closed"
				case "cancel":
					cancel()
					done <- nil
					return
				}
				err = WriteRuntimeHealthResponse(conn, response)
				if err == nil && mode == "trailing" {
					_, err = conn.Write([]byte{0})
				}
				if err == nil && mode != "no-close-notify" {
					err = conn.CloseWrite()
				}
				done <- err
			}()
			response, err := destination.QueryRuntimeHealth(ctx, 2)
			if mode == "success" || mode == "closed" {
				require.NoError(t, err)
				want := "open"
				if mode == "closed" {
					want = "closed"
				}
				require.Equal(t, want, response.GateState)
				require.Equal(t, o.Identity, response.Identity)
			} else {
				require.Error(t, err)
				require.Equal(t, RuntimeHealthResponse{}, response)
			}
			join()
			if mode == "success" || mode == "closed" {
				require.NoError(t, serverErr, "original TLS handler joined")
			}
		})
	}
}

func TestRuntimeHealthDestinationTerminal(t *testing.T) {
	for _, trailing := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "trailing-adapter"}[trailing], func(t *testing.T) {
			o, server, _, _, _, _ := destinationFixture(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			terminal := make(chan struct{})
			o.Dial = func(ctx context.Context) (net.Conn, error) {
				raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
				if err != nil {
					return nil, err
				}
				return &publicationTerminalTCP{TCPConn: raw.(*net.TCPConn), terminal: terminal}, nil
			}
			d, err := NewDestination(o)
			require.NoError(t, err)
			gate := make(chan struct{})
			done := make(chan error, 1)
			finished := make(chan struct{})
			client := make(chan error, 1)
			var release, joined sync.Once
			unblock := func() { release.Do(func() { close(gate) }) }
			var serverErr error
			t.Cleanup(func() {
				cancel()
				unblock()
				listener.Close()
				joined.Do(func() { <-client })
				serverErr = <-done
				<-finished
				require.NoError(t, serverErr, "original TLS handler and raw terminal writer joined")
			})
			go func() {
				defer close(finished)
				raw, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
				conn := tls.Server(raw, server)
				r, err := ReadRuntimeRequest(conn)
				if err == nil {
					e := r.Health
					err = WriteRuntimeHealthResponse(conn, RuntimeHealthResponse{Version: 1, Purpose: "runtime_health", Identity: e.Identity, DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, GateState: "open"})
				}
				if err == nil {
					err = conn.CloseWrite()
				}
				if err == nil {
					<-gate
					if trailing {
						err = raw.SetWriteDeadline(time.Now().Add(time.Second))
						if err == nil {
							_, err = raw.Write([]byte{255})
						}
					}
				}
				done <- err
			}()
			go func() { _, err := d.QueryRuntimeHealth(ctx, 2); client <- err }()
			select {
			case err := <-client:
				joined.Do(func() {})
				t.Fatalf("returned before original adapter EOF: %v", err)
			case <-terminal:
			case <-ctx.Done():
				t.Fatal("no original adapter terminal read")
			}
			unblock()
			var result error
			joined.Do(func() { result = <-client })
			if trailing {
				require.Error(t, result)
			} else {
				require.NoError(t, result)
			}
		})
	}
}
