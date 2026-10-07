//go:build linux && (amd64 || arm64)

package etcd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"net"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	transport "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func (f *nativeExecutionFixture) rawTLS(t *testing.T, identity p.RuntimeIdentityContext) *tls.Conn {
	t.Helper()
	credential, err := p.NewManagementTLSCredential(f.secret.Issuer, f.secret.IssuerWire, "command_issuer")
	require.NoError(t, err)
	cfg, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: f.b.execVerifier, Clock: f.clock, Credential: credential, PeerCertificate: f.runtimeWire, PeerRuntime: &identity})
	require.NoError(t, err)
	raw, err := net.DialTimeout("unix", transport.ControlSocket, time.Second)
	require.NoError(t, err)
	conn := tls.Client(raw, cfg)
	require.NoError(t, conn.SetDeadline(time.Now().Add(6*time.Second)))
	t.Cleanup(func() { conn.Close() })
	return conn
}
func TestAuthenticatedTargetNativeFrames(t *testing.T) {
	f := nativeExecutionSetup(t)
	f.establishQueryControl(t)
	for _, defect := range []string{"duplicate", "unknown", "oversize", "truncated-input", "trailing-input", "wrong-purpose", "tampered-input", "missing-close-write"} {
		t.Run(defect, func(t *testing.T) {
			request := nativeRequest("quick", 5)
			request.Stdin = []byte{0, 1, 255}
			_, prepared := f.prepare(t, request)
			d := prepared.draft
			var frame bytes.Buffer
			require.NoError(t, transport.WriteRequest(&frame, transport.StartEnvelope(d.claims.Context, d.descriptor, d.ticket.Wire(), d.issuer.Record.Certificate)))
			wire := frame.Bytes()
			n := int(binary.BigEndian.Uint32(wire[:4]))
			header := bytes.Clone(wire[4 : 4+n])
			body := bytes.Clone(wire[4+n:])
			switch defect {
			case "duplicate":
				header = append([]byte(`{"version":1,`), header[1:]...)
			case "unknown":
				header = append([]byte(`{"unknown":true,`), header[1:]...)
			case "truncated-input":
				body = body[:len(body)-1]
			case "trailing-input":
				body = append(body, 42)
			case "wrong-purpose":
				header = bytes.Replace(header, []byte(`"purpose":"exec_start"`), []byte(`"purpose":"exec_query"`), 1)
			case "tampered-input":
				body[0] ^= 1
			}
			binary.BigEndian.PutUint32(wire[:4], uint32(len(header)))
			wire = append(append(bytes.Clone(wire[:4]), header...), body...)
			if defect == "oversize" {
				binary.BigEndian.PutUint32(wire[:4], 131073)
				wire = wire[:4]
			}
			conn := f.rawTLS(t, f.identity)
			require.NoError(t, conn.HandshakeContext(context.Background()))
			_, err := conn.Write(wire)
			require.NoError(t, err)
			if defect != "missing-close-write" {
				require.NoError(t, conn.CloseWrite())
			}
			kind, response, err := transport.ReadEvent(conn)
			require.True(t, err != nil || kind == transport.EventError, "malformed frame accepted kind=%d response=%s", kind, response)
			_ = conn.Close()
			f.absentCommand(t, prepared)
			t.Logf("ACTUAL_TLS_FRAME_REFUSED defect=%s command=%s", defect, d.commandID)
		})
	}
	t.Run("wrong-constructor-boot", func(t *testing.T) {
		identity := f.identity
		identity.Runtime.BootID = uuid.NewString()
		conn := f.rawTLS(t, identity)
		require.Error(t, conn.HandshakeContext(context.Background()))
		conn.Close()
	})
	t.Run("no-plaintext-downgrade", func(t *testing.T) {
		raw, err := net.DialTimeout("unix", transport.ControlSocket, time.Second)
		require.NoError(t, err)
		defer raw.Close()
		require.NoError(t, raw.SetDeadline(time.Now().Add(5*time.Second)))
		require.NoError(t, transport.WriteBootstrap(raw, transport.BootstrapRequest{Version: 1, Purpose: "hello"}))
		require.NoError(t, raw.(*net.UnixConn).CloseWrite())
		var response transport.BootstrapResponse
		require.Error(t, transport.ReadBootstrap(raw, &response))
	})
	require.Equal(t, float64(0), f.inspect(t, "MALFORMED_FINAL_IDLE")["active"])
}
