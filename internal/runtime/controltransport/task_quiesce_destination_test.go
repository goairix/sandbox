package controltransport

import (
	"context"
	"crypto/tls"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	"net"
	"strings"
	"testing"
	"time"
)

func quiesceTransportContext(t *testing.T, o DestinationOptions, issuer []byte, now time.Time) p.TaskUserQuiescenceContext {
	t.Helper()
	i, err := o.Verifier.VerifyCommandIssuerCertificate(issuer, now)
	require.NoError(t, err)
	c := p.TaskCloseDataContext{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "epoch", IssuerCertificateID: i.CertificateID(), IssuerCertificateDigest: i.Digest(), CommandID: "4a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", TaskID: "3a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", TaskDigest: strings.Repeat("b", 64), ClaimID: "2a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", WorkerID: "worker", SandboxID: o.Identity.SandboxID, WorkspaceHash: o.Identity.WorkspaceHash, Generation: o.Identity.Generation, Runtime: o.Identity.Runtime, DataGateEpoch: 1, ControlRevision: 10, ClaimCreateRevision: 11, LeaseID: 12}
	return p.TaskUserQuiescenceContext{Current: c, CloseDataContext: c, CloseDataTicketDigest: strings.Repeat("c", 64), CloseDataReceiptDigest: strings.Repeat("d", 64), CloseDataIntentRevision: 13}
}

func TestTaskQuiesceTransportIndependentTrustAndHistoricalTerminal(t *testing.T) {
	o, _, iw, clock, key, _ := destinationFixture(t)
	o.Dial = func(context.Context) (net.Conn, error) { return nil, nil }
	d, err := NewDestination(o)
	require.NoError(t, err)
	c := quiesceTransportContext(t, o, iw, clock.now)
	require.True(t, d.MatchTaskQuiesce(c, iw))
	before, after := clock.now.Add(-2*time.Second), clock.now.Add(20*time.Second)
	digest := strings.Repeat("e", 64)
	aw, err := p.SignTaskUserQuiescenceAccepted(key, p.TaskUserQuiescenceAcceptedClaims{Version: 1, State: "quiescence_accepted", Context: c, TicketDigest: digest, NotBefore: before, NotAfter: after})
	require.NoError(t, err)
	rw, err := p.SignTaskUserQuiescenceReceipt(key, p.TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: c, TicketDigest: digest, NotBefore: before, NotAfter: after, ExecutionSetDigest: strings.Repeat("f", 64)})
	require.NoError(t, err)
	_, err = d.VerifyTaskQuiesceAccepted(o.Verifier, aw, c, digest, before, after, clock.now)
	require.NoError(t, err)
	proof, err := d.VerifyTaskQuiesceReceipt(o.Verifier, rw, c, digest, before, after, clock.now.Add(35*time.Second))
	require.NoError(t, err)
	require.Equal(t, "users_quiesced", proof.State())
	_, err = d.VerifyTaskQuiesceReceipt(o.Verifier, aw, c, digest, before, after, clock.now)
	require.Error(t, err)
	_, err = d.VerifyTaskQuiesceAccepted(o.Verifier, rw, c, digest, before, after, clock.now)
	require.Error(t, err)
	other, _, _, _, _, _ := destinationFixture(t)
	_, err = d.VerifyTaskQuiesceReceipt(other.Verifier, rw, c, digest, before, after, clock.now)
	require.Error(t, err, "same tuple, foreign root must fail")
	_, err = d.VerifyTaskQuiesceReceipt(o.Verifier, rw, c, digest, before, after, clock.now.Add(2*time.Hour))
	require.Error(t, err, "current certificate expired")
	_, err = d.VerifyTaskQuiesceReceipt(o.Verifier, rw, c, digest, before, after.Add(time.Second), clock.now)
	require.Error(t, err, "exact original interval")
	copied := *d
	require.False(t, copied.MatchTaskQuiesce(c, iw))
	c.Current.Runtime.BootID = "foreign"
	require.False(t, d.MatchTaskQuiesce(c, iw))
}

func TestTaskQuiesceTransportActualTLSAndEOF(t *testing.T) {
	o, server, iw, clock, _, _ := destinationFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	o.Dial = func(ctx context.Context) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "tcp", listener.Addr().String())
	}
	d, err := NewDestination(o)
	require.NoError(t, err)
	e := quiesceFrame()
	e.Context = quiesceTransportContext(t, o, iw, clock.now)
	done := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()
		raw.SetDeadline(time.Now().Add(2 * time.Second))
		conn := tls.Server(raw, server)
		if err = conn.Handshake(); err == nil {
			var r RuntimeRequest
			r, err = ReadRuntimeRequest(conn)
			if err == nil && (r.Quiesce == nil || r.Quiesce.Purpose != "task_quiesce_users") {
				err = ErrFrame
			}
		}
		if err == nil {
			err = WriteEvent(conn, EventAccepted, []byte("owned"))
		}
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := d.Open(ctx)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.SendTaskQuiesce(ctx, e))
	kind, wire, err := s.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, EventAccepted, kind)
	require.Equal(t, "owned", string(wire))
	require.NoError(t, <-done)
	require.Error(t, s.SendTaskQuiesce(ctx, e), "one finite request")
}

func TestTaskQuiesceTransportTLSRejectsForeignPeerAndCurrentClock(t *testing.T) {
	for _, fault := range []string{"foreign-peer", "expired-current-clock"} {
		t.Run(fault, func(t *testing.T) {
			o, server, _, clock, _, _ := destinationFixture(t)
			if fault == "foreign-peer" {
				_, server, _, _, _, _ = destinationFixture(t)
			} else {
				clock.now = clock.now.Add(2 * time.Hour)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			o.Dial = func(ctx context.Context) (net.Conn, error) {
				var dial net.Dialer
				return dial.DialContext(ctx, "tcp", listener.Addr().String())
			}
			d, err := NewDestination(o)
			require.NoError(t, err)
			done := make(chan struct{})
			go func() {
				defer close(done)
				raw, err := listener.Accept()
				if err != nil {
					return
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(time.Second))
				_ = tls.Server(raw, server).Handshake()
			}()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			s, err := d.Open(ctx)
			require.Error(t, err)
			require.Nil(t, s)
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("handshake owner unjoined")
			}
		})
	}
}
