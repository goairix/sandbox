//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// This leaf requires the controller's actual protected PID1. It drives the
// real TLS owner after genuine terminal persistence wins before the ACK signer.
func TestTaskQuiesceTransportNativeTerminalBeforeACK(t *testing.T) {
	f := newNativeFixture(t)
	request := nativeQuiescenceRequest(t, f)
	a, err := f.s.beginUserQuiescence(context.Background(), request)
	require.NoError(t, err)
	awaitNativeQuiescence(t, f, a)
	_, err = f.s.quiescenceAcceptedReceipt(context.Background(), a)
	require.Error(t, err, "pending-only signer must lose after terminal")
	// Outside the strict journal leaf, inside the original root-only protected
	// parent. Management uses Unix mTLS under the selected no-IP topology.
	socket := filepath.Join("/journal/protected", "quiesce-transport-"+uuid.NewString()+".sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(socket, 0600))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.s.Serve(ctx, listener) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("Serve owner unjoined")
		}
	}()
	credential, err := p.NewManagementTLSCredential(f.issuer, f.issuerWire, "command_issuer")
	require.NoError(t, err)
	d, err := tr.NewDestination(tr.DestinationOptions{Identity: f.s.activation.Identity(), RuntimeCertificate: f.s.activation.RuntimeCertificate(), Credential: credential, Verifier: f.s.options.Verifier, Clock: f.s.options.Clock, Dial: func(ctx context.Context) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "unix", socket)
	}})
	require.NoError(t, err)
	for _, purpose := range []string{"task_quiesce_users", "task_quiesce_users_query"} {
		bounded, finish := context.WithTimeout(context.Background(), 5*time.Second)
		s, err := d.Open(bounded)
		require.NoError(t, err)
		e := tr.TaskQuiesceEnvelope{Version: 1, Purpose: purpose, Context: request.evidence.Context(), TicketDigest: request.evidence.Digest()}
		if purpose == "task_quiesce_users" {
			e.Ticket = request.evidence.Wire()
			e.IssuerCertificate = f.issuerWire
			e.OriginalCloseDataReceipt = request.originalCloseReceipt
		}
		require.NoError(t, s.SendTaskQuiesce(bounded, e))
		kind, wire, err := s.Read(bounded)
		s.Close()
		finish()
		require.NoError(t, err)
		require.Equal(t, tr.EventReceipt, kind)
		proof, err := d.VerifyTaskQuiesceReceipt(f.s.options.Verifier, wire, e.Context, e.TicketDigest, request.evidence.NotBefore(), request.evidence.NotAfter(), time.Now().UTC())
		require.NoError(t, err)
		require.Equal(t, "users_quiesced", proof.State())
		require.Same(t, a, f.s.quiescence)
		require.False(t, f.s.closed, "management stays alive")
	}
	// Same live management connection owner refuses foreign attribution.
	bounded, finish := context.WithTimeout(context.Background(), 5*time.Second)
	defer finish()
	s, err := d.Open(bounded)
	require.NoError(t, err)
	defer s.Close()
	e := tr.TaskQuiesceEnvelope{Version: 1, Purpose: "task_quiesce_users_query", Context: request.evidence.Context(), TicketDigest: request.evidence.Digest()}
	e.Context.Current.CommandID = "00000000-0000-4000-8000-000000000001"
	require.NoError(t, s.SendTaskQuiesce(bounded, e))
	kind, _, err := s.Read(bounded)
	require.NoError(t, err)
	require.Equal(t, tr.EventError, kind)
}
