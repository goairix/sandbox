package controltransport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
)

func TestNodeObserverTransportFixedFrames(t *testing.T) {
	for _, size := range []int{1, p.NodeEnrollmentFrameBytes} {
		var b bytes.Buffer
		wire := bytes.Repeat([]byte{'a'}, size)
		require.NoError(t, WriteNodeObserverFrame(&b, wire))
		actual, err := ReadNodeObserverFrame(&b)
		require.NoError(t, err)
		require.Equal(t, wire, actual)
	}
	for _, size := range []uint32{0, p.NodeEnrollmentFrameBytes + 1} {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], size)
		_, err := ReadNodeObserverFrame(bytes.NewReader(b[:]))
		require.Error(t, err)
	}
	q := p.NodeTerminationQuery{Version: 1, Purpose: "task_runtime_terminate_query", TaskID: "81111111-1111-4111-8111-111111111111", CommandID: "91111111-1111-4111-8111-111111111111", IntentCreateRevision: 8, IntentDigest: strings.Repeat("a", 64), EnrollmentDigest: strings.Repeat("b", 64)}
	r := NodeTerminationRequest{Version: 1, Purpose: q.Purpose, Query: &q}
	wire, err := EncodeNodeTerminationRequest(r)
	require.NoError(t, err)
	_, err = DecodeNodeTerminationRequest(wire)
	require.NoError(t, err)
	for _, invalid := range [][]byte{append(wire, ' '), append([]byte(`{"unknown":1,`), wire[1:]...), []byte(strings.Replace(string(wire), `"version":1`, `"version":1,"version":1`, 1))} {
		_, err = DecodeNodeTerminationRequest(invalid)
		require.Error(t, err)
	}
	r.Ticket = []byte("execute")
	_, err = EncodeNodeTerminationRequest(r)
	require.Error(t, err)
}

func TestNodeObserverTransportUninitializedRefuses(t *testing.T) {
	var s NodeEnrollmentSession
	require.NotPanics(t, func() { require.NoError(t, s.Close()) })
	_, err := s.Enroll(context.Background(), p.NodeEnrollmentResponse{})
	require.Error(t, err)
	var d TerminationDestination
	require.NotPanics(t, func() { require.NoError(t, d.Close()) })
	_, err = d.Query(context.Background(), p.NodeTerminationQuery{})
	require.Error(t, err)
}

func TestNodeObserverTransportPinsBeforeSend(t *testing.T) {
	d := TerminationDestination{enrollment: strings.Repeat("a", 64), nodeUID: "node", nodeBootID: "boot", certificateDigest: strings.Repeat("b", 64)}
	c := p.TaskRuntimeTerminationTicketClaims{EnrollmentDigest: strings.Repeat("c", 64), NodeUID: "node", NodeBootID: "boot", ObserverCertificateDigest: d.certificateDigest}
	wire, err := json.Marshal(struct {
		Claims p.TaskRuntimeTerminationTicketClaims `json:"claims"`
	}{c})
	require.NoError(t, err)
	require.ErrorContains(t, d.matchTicketPins(wire), "pin mismatch")
	c.EnrollmentDigest = d.enrollment
	wire, err = json.Marshal(struct {
		Claims p.TaskRuntimeTerminationTicketClaims `json:"claims"`
	}{c})
	require.NoError(t, err)
	require.NoError(t, d.matchTicketPins(wire))
}

func TestNodeObserverTransportCloseJoinsBlockedIO(t *testing.T) {
	t.Run("TLS handshake", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		defer listener.Close()
		ctx, cancel := context.WithCancel(context.Background())
		d := &TerminationDestination{ctx: ctx, cancel: cancel, address: listener.Addr().String(), config: &tls.Config{MinVersion: tls.VersionTLS13}, connections: make(map[net.Conn]struct{})}
		accepted := make(chan net.Conn, 1)
		go func() { conn, _ := listener.Accept(); accepted <- conn }()
		done := make(chan error, 1)
		go func() {
			_, err := d.call(context.Background(), NodeTerminationRequest{Version: 1, Purpose: "task_runtime_terminate", Ticket: []byte("bounded"), IssuerCertificate: []byte("bounded")})
			done <- err
		}()
		conn := <-accepted
		require.NotNil(t, conn)
		defer conn.Close()
		require.NoError(t, d.Close())
		select {
		case err := <-done:
			require.Error(t, err)
		case <-time.After(time.Second):
			t.Fatal("Close returned before original handshake joined")
		}
		require.Empty(t, d.connections)
		require.Zero(t, d.active)
	})
	t.Run("Unix enrollment", func(t *testing.T) {
		dir, err := os.MkdirTemp("/tmp", "node-observer-uds-")
		require.NoError(t, err)
		defer os.RemoveAll(dir)
		listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "observer.sock"), Net: "unix"})
		require.NoError(t, err)
		defer listener.Close()
		client, err := net.DialUnix("unix", nil, listener.Addr().(*net.UnixAddr))
		require.NoError(t, err)
		server, err := listener.AcceptUnix()
		require.NoError(t, err)
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		s := &NodeEnrollmentSession{conn: client, ctx: ctx, cancel: cancel, joined: make(chan struct{})}
		s.stop = context.AfterFunc(ctx, func() { defer close(s.joined); _ = client.Close() })
		done := make(chan error, 1)
		go func() {
			_, err := s.Enroll(context.Background(), p.NodeEnrollmentResponse{Version: 1, Purpose: "runtime_node_enrollment_response", Signature: bytes.Repeat([]byte{1}, 64)})
			done <- err
		}()
		_ = server.SetReadDeadline(time.Now().Add(time.Second))
		_, err = ReadNodeObserverFrame(server)
		require.NoError(t, err)
		require.NoError(t, s.Close())
		select {
		case err := <-done:
			require.Error(t, err)
		case <-time.After(time.Second):
			t.Fatal("Close returned before original Unix IO joined")
		}
	})
}
