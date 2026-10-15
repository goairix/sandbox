package kubernetes

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func bridgeWireServer(t *testing.T, secure bool, serve func(*websocket.Conn) error) (*httptest.Server, EnrolledBridgeOptions, EnrolledBridgeTarget, <-chan error) {
	t.Helper()
	pod, node, target := bridgeCandidate(t)
	result := make(chan error, 1)
	var mu sync.Mutex
	var paths []string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer fixed-test-token" {
			http.Error(w, "unexpected fixed request", 400)
			return
		}
		switch r.URL.Path {
		case "/api/v1/namespaces/runtimes/pods/" + pod.Name:
			_ = json.NewEncoder(w).Encode(pod)
		case "/api/v1/nodes/" + node.Name:
			_ = json.NewEncoder(w).Encode(node)
		case "/api/v1/namespaces/runtimes/pods/" + pod.Name + "/exec":
			expected := map[string][]string{"container": {"sandbox"}, "command": {"/usr/local/bin/sandbox-launcher", "bridge"}, "stdin": {"true"}, "stdout": {"true"}, "stderr": {"true"}, "tty": {"false"}}
			if !reflect.DeepEqual(expected, map[string][]string(r.URL.Query())) || !reflect.DeepEqual(websocket.Subprotocols(r), []string{"v5.channel.k8s.io"}) {
				result <- fmt.Errorf("unexpected exec query or protocol")
				http.Error(w, "bad exec", 400)
				return
			}
			upgrader := websocket.Upgrader{Subprotocols: []string{"v5.channel.k8s.io"}}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				result <- err
				return
			}
			defer conn.Close()
			result <- serve(conn)
		default:
			http.NotFound(w, r)
		}
	}))
	if secure {
		server.StartTLS()
	} else {
		server.Start()
	}
	t.Cleanup(server.Close)
	o := bridgeOptions(server.URL)
	o.Config.BearerToken = "fixed-test-token"
	if secure {
		cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
		require.NoError(t, err)
		o.Config.CAData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	}
	return server, o, target, result
}

func TestEnrolledBridgeWireBinaryHalfClose(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(fmt.Sprint(secure), func(t *testing.T) {
			payload := bytes.Repeat([]byte{0, 255, 1, 2, 13, 10}, 14000)
			_, options, target, result := bridgeWireServer(t, secure, func(ws *websocket.Conn) error {
				var received []byte
				for {
					kind, wire, err := ws.ReadMessage()
					if err != nil {
						return err
					}
					if kind != websocket.BinaryMessage {
						return fmt.Errorf("nonbinary stdin")
					}
					if bytes.Equal(wire, []byte{255, 0}) {
						break
					}
					if len(wire) < 1 || wire[0] != 0 || len(wire) > 32769 {
						return fmt.Errorf("stdin channel/chunk")
					}
					received = append(received, wire[1:]...)
				}
				if !bytes.Equal(received, payload) {
					return fmt.Errorf("stdin mismatch")
				}
				for len(received) != 0 {
					n := min(len(received), 32000)
					if err := ws.WriteMessage(websocket.BinaryMessage, append([]byte{1}, received[:n]...)); err != nil {
						return err
					}
					received = received[n:]
				}
				if err := ws.WriteMessage(websocket.BinaryMessage, []byte{2}); err != nil {
					return err
				}
				if err := ws.WriteMessage(websocket.BinaryMessage, append([]byte{3}, []byte(`{"status":"Success"}`)...)); err != nil {
					return err
				}
				return ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
			})
			client, err := NewEnrolledBridgeClient(options)
			require.NoError(t, err)
			defer client.Close()
			ctx, cancel := context.WithCancel(context.Background())
			conn, err := client.Dial(ctx, target)
			require.NoError(t, err)
			cancel() // Establishment context must not remain attached.
			require.NotNil(t, conn.LocalAddr())
			require.NotNil(t, conn.RemoteAddr())
			n, err := conn.Write(payload)
			require.NoError(t, err)
			require.Equal(t, len(payload), n)
			half := conn.(interface{ CloseWrite() error })
			require.NoError(t, half.CloseWrite())
			require.NoError(t, half.CloseWrite())
			got, err := io.ReadAll(conn)
			require.NoError(t, err)
			require.Equal(t, payload, got)
			require.NoError(t, <-result, "original server handler exited")
			require.NoError(t, conn.Close())
			require.NoError(t, conn.Close())
		})
	}
}
