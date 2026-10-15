package kubernetes

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

func TestEnrolledBridgeStrictStatus(t *testing.T) {
	for _, status := range []string{
		`{"status":"Failure","status":"Success"}`,
		`{"Status":"Success"}`,
		`{"status":"Success","unknown":true}`,
		`{"status":"Failure","reason":"NonZeroExitCode","details":{"causes":[{"reason":"ExitCode","message":"1"}]}}`,
		`{"status":"Success","code":1}`,
		`{"status":"Success"} {"status":"Success"}`,
	} {
		t.Run(status, func(t *testing.T) {
			_, o, target, done := bridgeWireServer(t, false, func(ws *websocket.Conn) error {
				if err := ws.WriteMessage(websocket.BinaryMessage, append([]byte{3}, status...)); err != nil {
					return err
				}
				return ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
			})
			client, err := NewEnrolledBridgeClient(o)
			require.NoError(t, err)
			defer client.Close()
			conn, err := client.Dial(context.Background(), target)
			require.NoError(t, err)
			_, err = io.ReadAll(conn)
			require.Error(t, err)
			require.NoError(t, conn.Close())
			require.NoError(t, <-done)
		})
	}
}

func TestEnrolledBridgeWireFaults(t *testing.T) {
	for name, test := range map[string]struct {
		kind      int
		wire      []byte
		closeCode int
		success   bool
	}{
		"empty success":          {websocket.BinaryMessage, []byte{3}, 1000, true},
		"API metadata success":   {websocket.BinaryMessage, append([]byte{3}, []byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Success"}`)...), 1000, true},
		"bare disconnect":        {websocket.BinaryMessage, []byte{3}, 0, false},
		"going away":             {websocket.BinaryMessage, []byte{3}, 1001, false},
		"protocol close":         {websocket.BinaryMessage, []byte{3}, 1002, false},
		"malformed status":       {websocket.BinaryMessage, []byte{3, '{'}, 1000, false},
		"status bound":           {websocket.BinaryMessage, append([]byte{3}, bytes.Repeat([]byte{' '}, 16385)...), 1000, false},
		"stderr nonempty":        {websocket.BinaryMessage, []byte{2, 'x'}, 1000, false},
		"stderr bound":           {websocket.BinaryMessage, append([]byte{2}, bytes.Repeat([]byte{'x'}, 4097)...), 1000, false},
		"message bound":          {websocket.BinaryMessage, append([]byte{1}, make([]byte, 65536)...), 1000, false},
		"text":                   {websocket.TextMessage, []byte{1, 'x'}, 1000, false},
		"unknown channel":        {websocket.BinaryMessage, []byte{4}, 1000, false},
		"incoming stdin":         {websocket.BinaryMessage, []byte{0}, 1000, false},
		"incoming close channel": {websocket.BinaryMessage, []byte{255, 0}, 1000, false},
		"empty message":          {websocket.BinaryMessage, nil, 1000, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, o, target, done := bridgeWireServer(t, false, func(ws *websocket.Conn) error {
				if err := ws.WriteMessage(test.kind, test.wire); err != nil {
					return err
				}
				if test.closeCode != 0 {
					return ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(test.closeCode, ""), time.Now().Add(time.Second))
				}
				return nil
			})
			client, err := NewEnrolledBridgeClient(o)
			require.NoError(t, err)
			defer client.Close()
			conn, err := client.Dial(context.Background(), target)
			require.NoError(t, err)
			_, err = io.ReadAll(conn)
			if test.success {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			client.mu.Lock()
			active := len(client.active)
			client.mu.Unlock()
			require.Equal(t, 1, active, "terminal wire status does not release an owned connection slot")
			require.NoError(t, conn.Close())
			<-done // Join the original handler, including a failed write after hard close.
		})
	}
}

func TestEnrolledBridgeTLSHandshakeCancellation(t *testing.T) {
	for _, cause := range []string{"caller", "timeout", "close"} {
		t.Run(cause, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			entered, exited := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(exited)
				raw, err := listener.Accept()
				if err != nil {
					return
				}
				defer raw.Close()
				var first [1]byte
				if _, err := io.ReadFull(raw, first[:]); err != nil {
					return
				}
				close(entered)
				_, _ = io.Copy(io.Discard, raw)
			}()
			o := bridgeOptions("https://" + listener.Addr().String())
			if cause == "timeout" {
				o.RequestTimeout = 100 * time.Millisecond
			}
			client, err := NewEnrolledBridgeClient(o)
			require.NoError(t, err)
			defer client.Close()
			_, _, target := bridgeCandidate(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := client.Dial(ctx, target); result <- err }()
			<-entered
			if cause == "caller" {
				cancel()
			}
			if cause == "close" {
				require.NoError(t, client.Close())
			}
			require.Error(t, <-result)
			select {
			case <-exited:
			case <-time.After(time.Second):
				t.Fatal("original TLS handshake socket did not exit")
			}
		})
	}
}

func TestEnrolledBridgeHTTPFaults(t *testing.T) {
	for _, fault := range []string{"pod kind", "pod uid", "pod deletion", "node kind", "node uid", "node boot", "node os", "node deletion", "headers", "body", "encoding", "get upgrade", "redirect get", "redirect exec", "unknown protocol", "missing protocol", "duplicate protocol", "bad upgrade", "compression", "upgrade headers"} {
		t.Run(fault, func(t *testing.T) {
			pod, node, target := bridgeCandidate(t)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if strings.HasSuffix(r.URL.Path, "/exec") {
					switch fault {
					case "redirect exec":
						http.Redirect(w, r, "http://127.0.0.1:1/never", 302)
					case "unknown protocol", "missing protocol", "duplicate protocol", "compression", "upgrade headers":
						raw, rw, err := w.(http.Hijacker).Hijack()
						if err != nil {
							return
						}
						defer raw.Close()
						digest := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
						extra := "Sec-WebSocket-Protocol: v5.channel.k8s.io\r\n"
						switch fault {
						case "unknown protocol":
							extra = "Sec-WebSocket-Protocol: v4.channel.k8s.io\r\n"
						case "missing protocol":
							extra = ""
						case "duplicate protocol":
							extra += extra
						case "compression":
							extra += "Sec-WebSocket-Extensions: permessage-deflate; server_no_context_takeover; client_no_context_takeover\r\n"
						case "upgrade headers":
							extra += "X-Big: " + strings.Repeat("x", 65536) + "\r\n"
						}
						_, _ = fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n%s\r\n", base64.StdEncoding.EncodeToString(digest[:]), extra)
						_ = rw.Flush()
					default:
						w.WriteHeader(101)
					}
					return
				}
				if strings.Contains(r.URL.Path, "/nodes/") {
					copy := node.DeepCopy()
					switch fault {
					case "node kind":
						copy.Kind = "Pod"
					case "node uid":
						copy.UID = "88888888-8888-4888-8888-888888888888"
					case "node boot":
						copy.Status.NodeInfo.BootID = "88888888-8888-4888-8888-888888888888"
					case "node os":
						copy.Status.NodeInfo.OperatingSystem = "windows"
					case "node deletion":
						now := metav1.Now()
						copy.DeletionTimestamp = &now
					}
					_ = json.NewEncoder(w).Encode(copy)
					return
				}
				copy := pod.DeepCopy()
				switch fault {
				case "pod kind":
					copy.Kind = "Node"
				case "pod uid":
					copy.UID = "88888888-8888-4888-8888-888888888888"
				case "pod deletion":
					now := metav1.Now()
					copy.DeletionTimestamp = &now
				case "headers":
					w.Header().Set("X-Big", strings.Repeat("x", 65536))
				case "body":
					_, _ = io.WriteString(w, strings.Repeat(" ", 262145))
					return
				case "encoding":
					w.Header().Set("Content-Encoding", "gzip")
				case "get upgrade":
					w.Header().Set("Upgrade", "websocket")
				case "redirect get":
					http.Redirect(w, r, "http://127.0.0.1:1/never", 302)
					return
				}
				_ = json.NewEncoder(w).Encode(copy)
			}))
			defer server.Close()
			client, err := NewEnrolledBridgeClient(bridgeOptions(server.URL))
			require.NoError(t, err)
			defer client.Close()
			_, err = client.Dial(context.Background(), target)
			require.Error(t, err)
			require.LessOrEqual(t, requests.Load(), int32(3), "no retry or redirected request")
		})
	}
}

func TestEnrolledBridgeBounds(t *testing.T) {
	for _, mutate := range []func(*EnrolledBridgeOptions){
		func(o *EnrolledBridgeOptions) { o.MaxActive = 0 },
		func(o *EnrolledBridgeOptions) { o.MaxActive = 17 },
		func(o *EnrolledBridgeOptions) { o.RequestTimeout = 0 },
		func(o *EnrolledBridgeOptions) { o.RequestTimeout = 6 * time.Second },
		func(o *EnrolledBridgeOptions) { o.Namespace = "../forbidden" },
	} {
		o := bridgeOptions("http://127.0.0.1:1")
		mutate(&o)
		_, err := NewEnrolledBridgeClient(o)
		require.Error(t, err)
	}
	_, _, target := bridgeCandidate(t)
	client, err := NewEnrolledBridgeClient(bridgeOptions("http://127.0.0.1:1"))
	require.NoError(t, err)
	defer client.Close()
	for _, mutate := range []func(*EnrolledBridgeTarget){
		func(v *EnrolledBridgeTarget) { v.PodName = "../escape" },
		func(v *EnrolledBridgeTarget) { v.NodeName = strings.Repeat("n", 129) },
		func(v *EnrolledBridgeTarget) { v.PodUID = "00000000-0000-0000-0000-000000000000" },
		func(v *EnrolledBridgeTarget) { v.NodeUID = "not-a-uuid" },
		func(v *EnrolledBridgeTarget) { v.UID = 0 },
		func(v *EnrolledBridgeTarget) { v.GID = 2147483648 },
		func(v *EnrolledBridgeTarget) { v.Generation = 0 },
		func(v *EnrolledBridgeTarget) { v.Authority.RestoreEpoch = "../escape" },
	} {
		copy := target
		mutate(&copy)
		_, err := client.Dial(context.Background(), copy)
		require.ErrorIs(t, err, ErrEnrolledBridge, "rejected before any unavailable-origin dial")
	}
}

func TestEnrolledBridgeEstablishmentCancellation(t *testing.T) {
	for _, stage := range []string{"pod", "node", "exec"} {
		for _, cause := range []string{"caller", "timeout", "close"} {
			t.Run(stage+"/"+cause, func(t *testing.T) {
				pod, node, target := bridgeCandidate(t)
				entered, exited := make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					current := "pod"
					if strings.Contains(r.URL.Path, "/nodes/") {
						current = "node"
					}
					if strings.HasSuffix(r.URL.Path, "/exec") {
						current = "exec"
					}
					if current == stage {
						close(entered)
						<-r.Context().Done()
						close(exited)
						return
					}
					if current == "pod" {
						_ = json.NewEncoder(w).Encode(pod)
					} else {
						_ = json.NewEncoder(w).Encode(node)
					}
				}))
				defer server.Close()
				o := bridgeOptions(server.URL)
				if cause == "timeout" {
					o.RequestTimeout = 100 * time.Millisecond
				}
				client, err := NewEnrolledBridgeClient(o)
				require.NoError(t, err)
				defer client.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() { _, err := client.Dial(ctx, target); result <- err }()
				<-entered
				if cause == "caller" {
					cancel()
				}
				if cause == "close" {
					require.NoError(t, client.Close())
				}
				require.Error(t, <-result)
				select {
				case <-exited:
				case <-time.After(time.Second):
					t.Fatal("original server I/O was not closed")
				}
				client.mu.Lock()
				active := len(client.active)
				client.mu.Unlock()
				require.Zero(t, active, "slot refunded after original establishment exited")
			})
		}
	}
}

func TestEnrolledBridgeCloseAndLimits(t *testing.T) {
	_, o, target, done := bridgeWireServer(t, false, func(ws *websocket.Conn) error { _, _, err := ws.ReadMessage(); return err })
	o.MaxActive = 16
	client, err := NewEnrolledBridgeClient(o)
	require.NoError(t, err)
	defer client.Close()
	var conns []*bridgeConn
	for range 16 {
		conn, err := client.Dial(context.Background(), target)
		require.NoError(t, err)
		conns = append(conns, conn.(*bridgeConn))
	}
	start := time.Now()
	_, err = client.Dial(context.Background(), target)
	require.ErrorIs(t, err, ErrEnrolledBridge)
	require.Less(t, time.Since(start), 100*time.Millisecond, "capacity rejection does not queue")
	reads := make(chan error, 16)
	for _, conn := range conns {
		go func(c *bridgeConn) { _, err := c.Read(make([]byte, 1)); reads <- err }(conn)
	}
	require.NoError(t, client.Close())
	for _, conn := range conns {
		select {
		case <-conn.readerDone:
		default:
			t.Fatal("reader not joined")
		}
		select {
		case <-conn.deadlineDone:
		default:
			t.Fatal("deadline waiter not joined")
		}
		require.Error(t, <-reads)
		require.Error(t, <-done, "each original handler returned from ReadMessage and physically closed its socket")
	}
	require.Empty(t, client.active)
	_, err = client.Dial(context.Background(), target)
	require.ErrorIs(t, err, ErrEnrolledBridge)
}

func TestEnrolledBridgeDeadlineAndBlockedIO(t *testing.T) {
	for _, mode := range []string{"read deadline", "write deadline", "whole deadline", "blocked write close", "queued stdout close"} {
		t.Run(mode, func(t *testing.T) {
			release := make(chan struct{})
			_, o, target, done := bridgeWireServer(t, false, func(ws *websocket.Conn) error {
				if mode == "queued stdout close" {
					for range 32 {
						if err := ws.WriteMessage(websocket.BinaryMessage, append([]byte{1}, make([]byte, 1024)...)); err != nil {
							return err
						}
					}
				}
				<-release
				return nil
			})
			client, err := NewEnrolledBridgeClient(o)
			require.NoError(t, err)
			defer client.Close()
			conn, err := client.Dial(context.Background(), target)
			require.NoError(t, err)
			c := conn.(*bridgeConn)
			if mode == "queued stdout close" {
				require.Eventually(t, func() bool { return len(c.stdout) == 16 }, time.Second, time.Millisecond)
				require.NoError(t, c.Close())
			} else if mode == "blocked write close" {
				require.NoError(t, c.op.raw.(*net.TCPConn).SetWriteBuffer(1024))
				result := make(chan error, 1)
				go func() { _, err := c.Write(make([]byte, 32<<20)); result <- err }()
				require.Eventually(t, func() bool {
					if c.writeMu.TryLock() {
						c.writeMu.Unlock()
						return false
					}
					return true
				}, time.Second, time.Millisecond)
				select {
				case err := <-result:
					t.Fatalf("write did not block: %v", err)
				case <-time.After(20 * time.Millisecond):
				}
				require.NoError(t, c.Close())
				require.Error(t, <-result)
			} else {
				// Clearing the original temporary deadline must leave the session usable.
				require.NoError(t, c.SetDeadline(time.Now().Add(time.Second)))
				require.NoError(t, c.SetDeadline(time.Time{}))
				d := time.Now().Add(20 * time.Millisecond)
				switch mode {
				case "read deadline":
					err = c.SetReadDeadline(d)
				case "write deadline":
					err = c.SetWriteDeadline(d)
				default:
					err = c.SetDeadline(d)
				}
				require.NoError(t, err)
				if mode == "write deadline" {
					require.NoError(t, c.op.raw.(*net.TCPConn).SetWriteBuffer(1024))
					_, err = c.Write(make([]byte, 32<<20))
				} else {
					_, err = c.Read(make([]byte, 1))
				}
				require.True(t, errors.Is(err, os.ErrDeadlineExceeded), "got %v", err)
				require.NoError(t, c.Close())
			}
			select {
			case <-c.readerDone:
			default:
				t.Fatal("actual reader not joined")
			}
			select {
			case <-c.deadlineDone:
			default:
				t.Fatal("actual deadline waiter not joined")
			}
			require.Empty(t, client.active)
			close(release)
			<-done
		})
	}
}

func TestEnrolledBridgeRejectsStaticHooks(t *testing.T) {
	for name, mutate := range map[string]func(*rest.Config){
		"transport": func(c *rest.Config) { c.Transport = http.DefaultTransport },
		"wrapper": func(c *rest.Config) {
			c.WrapTransport = func(r http.RoundTripper) http.RoundTripper { panic("must not invoke") }
		},
		"dial": func(c *rest.Config) {
			c.Dial = func(context.Context, string, string) (net.Conn, error) { panic("must not invoke") }
		},
		"auth persister": func(c *rest.Config) { c.AuthConfigPersister = noBridgePersister{} },
		"auth plugin":    func(c *rest.Config) { c.AuthProvider = &clientcmdapi.AuthProviderConfig{Name: "forbidden"} },
		"exec plugin":    func(c *rest.Config) { c.ExecProvider = &clientcmdapi.ExecConfig{Command: "forbidden"} },
		"files":          func(c *rest.Config) { c.CAFile = "must-not-read" },
		"impersonation":  func(c *rest.Config) { c.Impersonate.UserName = "user" },
		"config bound":   func(c *rest.Config) { c.BearerToken = strings.Repeat("x", 262145) },
	} {
		t.Run(name, func(t *testing.T) {
			o := bridgeOptions("http://127.0.0.1:1")
			mutate(o.Config)
			_, err := NewEnrolledBridgeClient(o)
			require.Error(t, err)
		})
	}
}

type noBridgePersister struct{}

func (noBridgePersister) Persist(map[string]string) error { panic("must not invoke") }

func TestEnrolledBridgeOriginMarkers(t *testing.T) {
	for _, suffix := range []string{"?", "#", "?query=value", "#fragment"} {
		_, err := NewEnrolledBridgeClient(bridgeOptions("http://127.0.0.1:1" + suffix))
		require.Error(t, err, "query and fragment delimiters are not part of an API origin: %s", suffix)
	}
}
