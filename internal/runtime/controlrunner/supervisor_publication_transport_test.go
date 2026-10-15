package controlrunner

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/stretchr/testify/require"
)

// A host journal and real mTLS can exercise refusal routing, but never provide
// a native enrolled Supervisor or a successful publication proof.
func TestRuntimePublicationTransportRefusesUntrustedOwner(t *testing.T) {
	s, _, client := quiesceDeadlineTLS(t)
	raw, peer := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer raw.Close()
		s.serveConnection(ctx, raw)
	}()
	t.Cleanup(func() {
		cancel()
		_ = peer.Close()
		_ = raw.Close()
		<-done
	})
	conn := tls.Client(peer, client)
	deadline, _ := ctx.Deadline()
	require.NoError(t, conn.SetDeadline(deadline))
	require.NoError(t, conn.HandshakeContext(ctx))
	require.NoError(t, tr.WriteRuntimePublicationRequest(conn, tr.RuntimePublicationEnvelope{Version: 1, Purpose: tr.RuntimeEnrolledPublicationPurpose, PublicationCertificate: []byte("untrusted"), Claim: p.ClaimReference{ClaimID: "11111111-1111-4111-8111-111111111111", CreateRevision: 1, LeaseID: 2}}))
	require.NoError(t, conn.CloseWrite())
	wire, _ := io.ReadAll(conn)
	require.Empty(t, wire, "publication refusal must not emit legacy execution events")
	<-done
}
