package kubernetes

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestEnrolledBridgeDirectionalDeadlines(t *testing.T) {
	for _, mode := range []string{"idle-write", "active-read", "rejected-write"} {
		t.Run(mode, func(t *testing.T) {
			_, options, target, done := bridgeWireServer(t, false, func(ws *websocket.Conn) error {
				if mode != "active-read" {
					if err := ws.WriteMessage(websocket.BinaryMessage, []byte{1, 'p'}); err != nil {
						return err
					}
				}
				_, wire, err := ws.ReadMessage()
				if err != nil {
					return err
				}
				if !bytes.Equal(wire, []byte{0, 'q'}) {
					return fmt.Errorf("unexpected request %q", wire)
				}
				if err := ws.WriteMessage(websocket.BinaryMessage, []byte{1, 'r'}); err != nil {
					return err
				}
				return ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			})
			client, err := NewEnrolledBridgeClient(options)
			require.NoError(t, err)
			defer client.Close()
			conn, err := client.Dial(context.Background(), target)
			require.NoError(t, err)
			defer conn.Close()
			join := startupJoinWireHandler(t, client, conn, done)
			if mode == "active-read" {
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Millisecond)))
				_, err = conn.Read(make([]byte, 1))
				require.ErrorIs(t, err, os.ErrDeadlineExceeded)
			} else {
				require.NoError(t, conn.SetWriteDeadline(time.Now().Add(-time.Second)))
				if mode == "rejected-write" {
					n, err := conn.Write([]byte{'x'})
					require.Zero(t, n, "expired admission must send no bytes")
					require.ErrorIs(t, err, os.ErrDeadlineExceeded)
				}
				// Allow the actual deadline waiter to observe the idle expiration.
				time.Sleep(20 * time.Millisecond)
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
				var prefix [1]byte
				_, err = io.ReadFull(conn, prefix[:])
				require.NoError(t, err, "idle expired write deadline must preserve reads")
				require.Equal(t, byte('p'), prefix[0])
			}
			require.NoError(t, conn.SetWriteDeadline(time.Now().Add(time.Second)))
			_, err = conn.Write([]byte{'q'})
			require.NoError(t, err, "expired read/idle-write deadline must preserve future writes")
			require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
			response, err := io.ReadAll(conn)
			require.NoError(t, err)
			require.Equal(t, "r", string(response))
			require.NoError(t, join(), "original WS handler joined")
		})
	}
}

func startupJoinWireHandler(t *testing.T, client *EnrolledBridgeClient, conn net.Conn, done <-chan error) func() error {
	t.Helper()
	var once sync.Once
	var result error
	join := func() error {
		once.Do(func() { result = <-done })
		return result
	}
	t.Cleanup(func() {
		conn.Close()
		client.Close()
		err := join()
		if t.Failed() {
			t.Logf("original WS/TLS failure-path handler joined: %v", err)
		} else {
			require.NoError(t, err)
		}
	})
	return join
}

// This finite adapter carries actual TLS records through the real server-side
// v5 WebSocket, without substituting the production EnrolledBridgeClient.
type bridgeTLSServerWire struct {
	net.Conn
	ws      *websocket.Conn
	pending []byte
	fin     bool
}

func (c *bridgeTLSServerWire) Read(data []byte) (int, error) {
	for len(c.pending) == 0 {
		if c.fin {
			return 0, io.EOF
		}
		kind, frame, err := c.ws.ReadMessage()
		if err != nil {
			return 0, err
		}
		if kind != websocket.BinaryMessage || len(frame) == 0 {
			return 0, fmt.Errorf("invalid stdin frame")
		}
		if bytes.Equal(frame, []byte{255, 0}) {
			c.fin = true
			continue
		}
		if frame[0] != 0 {
			return 0, fmt.Errorf("non-stdin frame")
		}
		c.pending = frame[1:]
	}
	n := copy(data, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *bridgeTLSServerWire) Write(data []byte) (int, error) {
	if err := c.ws.WriteMessage(websocket.BinaryMessage, append([]byte{1}, data...)); err != nil {
		return 0, err
	}
	return len(data), nil
}

func TestEnrolledBridgeActualTLSCloseWrite(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Now()
	leaf := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "startup.test"}, DNSNames: []string{"startup.test"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, public, private)
	require.NoError(t, err)
	certificate, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	_, options, target, done := bridgeWireServer(t, false, func(ws *websocket.Conn) error {
		raw := &bridgeTLSServerWire{Conn: ws.UnderlyingConn(), ws: ws}
		conn := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: private}}})
		if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			return err
		}
		request, err := io.ReadAll(conn)
		if err != nil || string(request) != "request" {
			return fmt.Errorf("TLS request %q: %v", request, err)
		}
		var b [1]byte
		if n, err := raw.Read(b[:]); n != 0 || err != io.EOF || !raw.fin {
			return fmt.Errorf("missing original transport FIN: n=%d err=%v", n, err)
		}
		if _, err := conn.Write([]byte("response")); err != nil {
			return err
		}
		if err := conn.CloseWrite(); err != nil {
			return err
		}
		if err := raw.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
			return err
		}
		return ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	})
	client, err := NewEnrolledBridgeClient(options)
	require.NoError(t, err)
	defer client.Close()
	raw, err := client.Dial(context.Background(), target)
	require.NoError(t, err)
	defer raw.Close()
	join := startupJoinWireHandler(t, client, raw, done)
	conn := tls.Client(raw, &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "startup.test"})
	require.NoError(t, conn.SetDeadline(time.Now().Add(2*time.Second)))
	_, err = conn.Write([]byte("request"))
	require.NoError(t, err)
	require.NoError(t, conn.CloseWrite())
	// Real crypto/tls leaves a past write deadline. Its idle expiration must
	// not kill the response while the transport owner restores its FIN budget.
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, raw.SetWriteDeadline(time.Now().Add(time.Second)))
	require.NoError(t, raw.(interface{ CloseWrite() error }).CloseWrite())
	response, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.Equal(t, "response", string(response))
	require.NoError(t, join(), "original TLS/WS handler joined")
}
