package controltransport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTaskCloseSessionActualTLS(t *testing.T) {
	o, config, _, _, _, _ := destinationFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	o.Dial = func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", listener.Addr().String())
	}
	d, err := NewDestination(o)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
		c := tls.Server(raw, config)
		if err = c.Handshake(); err == nil {
			_, e, readErr := ReadControlRequest(c)
			err = readErr
			if err == nil && (e == nil || e.Purpose != "task_close_data") {
				err = ErrFrame
			}
			if err == nil {
				err = WriteEvent(c, EventReceipt, []byte("closed"))
			}
		}
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s, err := d.Open(ctx)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.SendTaskClose(ctx, taskFrame()))
	kind, wire, err := s.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, EventReceipt, kind)
	require.Equal(t, "closed", string(wire))
	require.Error(t, s.Send(ctx, frameRequest(t)))
	require.Error(t, s.SendTaskClose(ctx, taskFrame()))
	require.NoError(t, <-done)
}
func TestTaskCloseDestinationPinsTrustAndInterval(t *testing.T) {
	o, _, issuer, clock, key, _ := destinationFixture(t)
	o.Dial = func(context.Context) (net.Conn, error) { return nil, nil }
	d, err := NewDestination(o)
	require.NoError(t, err)
	issued, err := o.Verifier.VerifyCommandIssuerCertificate(issuer, clock.now)
	require.NoError(t, err)
	c := p.TaskCloseDataContext{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "epoch", IssuerCertificateID: issued.CertificateID(), IssuerCertificateDigest: issued.Digest(), CommandID: "81111111-1111-4111-8111-111111111111", TaskID: "91111111-1111-4111-8111-111111111111", ClaimID: "a1111111-1111-4111-8111-111111111111", TaskDigest: strings.Repeat("b", 64), WorkerID: "worker", SandboxID: o.Identity.SandboxID, WorkspaceHash: o.Identity.WorkspaceHash, Generation: 1, DataGateEpoch: 1, ControlRevision: 10, ClaimCreateRevision: 11, LeaseID: 12, Runtime: o.Identity.Runtime}
	before, after := clock.now.Add(-20*time.Second), clock.now.Add(-2*time.Second)
	require.True(t, d.MatchTaskClose(c, issuer))
	require.False(t, d.MatchTaskClose(c, []byte("other issuer")))
	wire, err := p.SignTaskDataClosedReceipt(key, p.TaskDataClosedReceiptClaims{Version: 1, State: "data_closed", Context: c, TicketDigest: strings.Repeat("c", 64), NotBefore: before, NotAfter: after})
	require.NoError(t, err)
	_, err = d.VerifyTaskCloseReceipt(o.Verifier, wire, c, strings.Repeat("c", 64), before, after, clock.now)
	require.NoError(t, err)
	other := c
	other.Runtime.BootID = "81111111-1111-4111-8111-111111111111"
	require.False(t, d.MatchTaskClose(other, issuer))
	require.Error(t, d.VerifyTaskCloseRuntime(o.Verifier, other, before, after, clock.now))
	require.Error(t, d.VerifyTaskCloseRuntime(o.Verifier, c, before, clock.now.Add(2*time.Hour), clock.now))
	require.Error(t, d.VerifyTaskCloseRuntime(o.Verifier, c, before, after, clock.now.Add(2*time.Hour)))
	root, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	foreign, err := p.NewManagementVerifier(p.TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}, []ed25519.PublicKey{root})
	require.NoError(t, err)
	require.Error(t, d.VerifyTaskCloseRuntime(foreign, c, before, after, clock.now))
	other = c
	other.ClaimID = other.TaskID
	_, err = d.VerifyTaskCloseReceipt(o.Verifier, wire, other, strings.Repeat("c", 64), before, after, clock.now)
	require.Error(t, err)
}
