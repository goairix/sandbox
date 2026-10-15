package kubernetes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestEnrolledBridgeEnteredWriteDeadline(t *testing.T) {
	for _, mode := range []string{"write", "whole connection"} {
		t.Run(mode, func(t *testing.T) {
			releaseServer := make(chan struct{})
			peerNotReading := make(chan struct{})
			_, options, target, serverDone := bridgeWireServer(t, false, func(ws *websocket.Conn) error {
				// Consume exactly one chunk to make the completed byte count
				// observable, then leave the real peer socket open without reads.
				kind, wire, err := ws.ReadMessage()
				close(peerNotReading)
				if err != nil {
					return err
				}
				if kind != websocket.BinaryMessage || len(wire) != 32769 || wire[0] != 0 {
					return fmt.Errorf("unexpected initial stdin frame")
				}
				<-releaseServer
				return nil
			})
			var serverOnce sync.Once
			joinServer := func() {
				serverOnce.Do(func() {
					close(releaseServer)
					require.NoError(t, <-serverDone, "original handler physically closed its socket")
				})
			}
			defer joinServer()
			options.MaxActive = 1
			client, err := NewEnrolledBridgeClient(options)
			require.NoError(t, err)
			defer client.Close()
			conn, err := client.Dial(context.Background(), target)
			require.NoError(t, err)
			c := conn.(*bridgeConn)
			require.NoError(t, c.op.raw.(*net.TCPConn).SetWriteBuffer(1024))
			type writeResult struct {
				n   int
				err error
			}
			writeDone := make(chan writeResult, 1)
			const payloadSize = 32 << 20
			go func() {
				n, err := conn.Write(make([]byte, payloadSize))
				writeDone <- writeResult{n: n, err: err}
			}()
			<-peerNotReading
			// Verify the actual network call, rather than a timer or a local mutex,
			// is holding the writer. This fixed-toolchain regression has no I/O hook.
			require.Eventually(t, func() bool {
				return bridgeWriteStack("(*bridgeConn).Write(", "internal/poll.(*FD).Write(")
			}, time.Second, time.Millisecond)
			closeWriteDone := make(chan error, 1)
			go func() { closeWriteDone <- c.CloseWrite() }()
			require.Eventually(t, func() bool {
				return bridgeWriteStack("(*bridgeConn).CloseWrite(", "sync.(*Mutex).Lock(")
			}, time.Second, time.Millisecond)
			client.mu.Lock()
			charged := len(client.active)
			client.mu.Unlock()
			require.Equal(t, 1, charged)
			_, err = client.Dial(context.Background(), target)
			require.ErrorIs(t, err, ErrEnrolledBridge, "blocked I/O retains its slot")
			deadline := time.Now().Add(20 * time.Millisecond)
			if mode == "write" {
				err = conn.SetWriteDeadline(deadline)
			} else {
				err = conn.SetDeadline(deadline)
			}
			require.NoError(t, err)
			written := <-writeDone
			finErr := <-closeWriteDone
			// Both original entered calls have returned before their results are
			// classified. In particular, FIN was admitted before the deadline.
			require.Positive(t, written.n, "preserve completed stdin chunks")
			require.Less(t, written.n, payloadSize)
			for operation, err := range map[string]error{"Write": written.err, "CloseWrite": finErr} {
				if !errors.Is(err, os.ErrDeadlineExceeded) {
					t.Errorf("%s lost the original deadline cause: %v", operation, err)
				}
				var timeout net.Error
				if !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Errorf("%s did not retain net.Error.Timeout: %v", operation, err)
				}
			}
			client.mu.Lock()
			charged = len(client.active)
			client.mu.Unlock()
			require.Equal(t, 1, charged, "even completed deadline failure retains ownership until Close")
			require.NoError(t, conn.Close())
			select {
			case <-c.readerDone:
			default:
				t.Fatal("original wire reader was not joined")
			}
			select {
			case <-c.deadlineDone:
			default:
				t.Fatal("original deadline waiter was not joined")
			}
			joinServer()
			client.mu.Lock()
			charged = len(client.active)
			client.mu.Unlock()
			require.Zero(t, charged)
			c.mu.Lock()
			firstCause := c.abortErr
			c.mu.Unlock()
			require.ErrorIs(t, firstCause, os.ErrDeadlineExceeded, "later Close cannot replace the first cause")
		})
	}
}

func bridgeWriteStack(call, blocked string) bool {
	buffer := make([]byte, 1<<20)
	n := runtime.Stack(buffer, true)
	for _, stack := range strings.Split(string(buffer[:n]), "\n\n") {
		if strings.Contains(stack, call) && strings.Contains(stack, blocked) {
			return true
		}
	}
	return false
}
