package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Real management mTLS and verified activation; the retained host journal is
// used only for refusal eligibility, never as a physical quiescence assertion.
func quiesceDeadlineTLS(t *testing.T) (*Supervisor, *tls.Config, *tls.Config) {
	t.Helper()
	s, _ := taskCloseTerminalHistory(t)
	b, i := s.activation.Binding(), s.activation.Identity()
	root, rootKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	ip, ik, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	v, err := p.NewManagementVerifier(b, []ed25519.PublicKey{root})
	require.NoError(t, err)
	now := time.Now().UTC()
	iw, err := p.SignCommandIssuerCertificate(rootKey, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, PublicKey: ip, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, err)
	rw, err := p.SignRuntimeIdentityCertificate(rootKey, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, PublicKey: s.key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute), SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, Runtime: i.Runtime})
	require.NoError(t, err)
	birth := p.BirthContext{BootID: i.Runtime.BootID, RuntimePublicKey: s.key.Public().(ed25519.PublicKey), UID: 1000, GID: 1000, ContractDigest: strings.Repeat("e", 64)}
	digest := func(w []byte) string { h := sha256.Sum256(w); return hex.EncodeToString(h[:]) }
	aw, err := p.SignTargetActivation(rootKey, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: b, Identity: i, DataGateEpoch: 1, UID: 1000, GID: 1000, ContractDigest: birth.ContractDigest, RuntimeCertificateDigest: digest(rw), IssuerCertificateDigest: digest(iw), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, err)
	a, err := v.VerifyTargetActivation(aw, rw, iw, birth, now)
	require.NoError(t, err)
	s.activation, s.options.Verifier = &a, v
	clock := &fixedClock{observation: p.ClockObservation{UTC: now}}
	rc, err := p.NewManagementTLSCredential(s.key, rw, "runtime_receipt")
	require.NoError(t, err)
	ic, err := p.NewManagementTLSCredential(ik, iw, "command_issuer")
	require.NoError(t, err)
	server, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: v, Clock: clock, Credential: rc, PeerCertificate: iw, Server: true})
	require.NoError(t, err)
	client, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: v, Clock: clock, Credential: ic, PeerCertificate: rw, PeerRuntime: &i})
	require.NoError(t, err)
	s.tlsConfig = server
	return s, server, client
}

// The pipe is intentionally unbuffered: after the genuine handshake, a peer
// that never reads must block even a small encrypted response. No fake IO.
func TestTaskQuiesceResponseOriginalDeadline(t *testing.T) {
	for _, kind := range []byte{tr.EventAccepted, tr.EventReceipt} {
		t.Run(map[byte]string{tr.EventAccepted: "accepted", tr.EventReceipt: "terminal"}[kind], func(t *testing.T) {
			s, server, client := quiesceDeadlineTLS(t)
			raw, peer := net.Pipe()
			defer raw.Close()
			defer peer.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
			defer cancel()
			borrow, call := s.transport.borrow(ctx, raw)
			done := make(chan error, 1)
			ready := make(chan struct{})
			release := make(chan struct{})
			go func() {
				defer borrow.release()
				defer raw.Close()
				conn := tls.Server(raw, server)
				deadline, _ := ctx.Deadline()
				_ = conn.SetDeadline(deadline)
				if err := conn.HandshakeContext(call); err != nil {
					done <- err
					return
				}
				close(ready)
				<-release
				// Opaque transport payload, not receipt or drain authority.
				done <- writeFiniteTransportEvent(call, conn, kind, []byte("transport-only"))
			}()
			joined := false
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
				raw.Close()
				peer.Close()
				if !joined {
					<-done
				}
			}()
			conn := tls.Client(peer, client)
			require.NoError(t, conn.HandshakeContext(ctx))
			<-ready
			deadline, _ := ctx.Deadline()
			timer := time.NewTimer(time.Until(deadline.Add(-75 * time.Millisecond)))
			<-timer.C
			close(release)
			select {
			case err := <-done:
				joined = true
				var timeout net.Error
				require.ErrorAs(t, err, &timeout)
				require.True(t, timeout.Timeout(), "must reach blocked encrypted response IO")
			case <-time.After(time.Until(deadline.Add(150 * time.Millisecond))):
				t.Fatal("nonreading TLS response retained borrow past original deadline")
			}
			s.transport.workers.Wait()
			s.transport.mu.Lock()
			defer s.transport.mu.Unlock()
			require.Empty(t, s.transport.users)
		})
	}
}

type quiesceDeadlineClock struct {
	entered chan context.Context
	release chan struct{}
	armed   *atomic.Bool
}

func (c *quiesceDeadlineClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	c.armed.Store(true)
	c.entered <- ctx
	select {
	case <-c.release:
		return p.ClockObservation{}, errors.New("delayed dispatch refusal")
	case <-ctx.Done():
		return p.ClockObservation{}, ctx.Err()
	}
}

type quiesceDeadlineConn struct {
	net.Conn
	armed    atomic.Bool
	writes   atomic.Int32
	deadline chan time.Time
}

func (c *quiesceDeadlineConn) SetDeadline(d time.Time) error {
	if c.deadline != nil {
		select {
		case c.deadline <- d:
		default:
		}
	}
	return c.Conn.SetDeadline(d)
}

func (c *quiesceDeadlineConn) Write(b []byte) (int, error) {
	if c.armed.Load() {
		c.writes.Add(1)
	}
	return c.Conn.Write(b)
}

// Catches both a renewed error-write deadline and a late error write after
// canceled/expired dispatch. Runs the actual parser/TLS/dispatch/error path.
func TestTaskQuiesceRefusalOriginalDeadline(t *testing.T) {
	for _, fault := range []string{"delayed", "expired", "canceled", "original-five-second"} {
		t.Run(fault, func(t *testing.T) {
			s, _, client := quiesceDeadlineTLS(t)
			raw, peer := net.Pipe()
			tracked := &quiesceDeadlineConn{Conn: raw, deadline: make(chan time.Time, 1)}
			clock := &quiesceDeadlineClock{entered: make(chan context.Context, 1), release: make(chan struct{}), armed: &tracked.armed}
			s.options.Clock = clock
			ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
			if fault == "original-five-second" {
				cancel()
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			borrow, call := s.transport.borrow(ctx, tracked)
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer borrow.release()
				defer tracked.Close()
				s.serveConnection(call, tracked)
			}()
			defer func() { cancel(); tracked.Close(); peer.Close(); <-done }()
			conn := tls.Client(peer, client)
			require.NoError(t, conn.HandshakeContext(ctx))
			b, i := s.activation.Binding(), s.activation.Identity()
			c := p.TaskCloseDataContext{Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, Runtime: i.Runtime, DataGateEpoch: 1}
			e := tr.TaskQuiesceEnvelope{Version: 1, Purpose: "task_quiesce_users", Context: p.TaskUserQuiescenceContext{Current: c}, TicketDigest: strings.Repeat("a", 64), Ticket: []byte("invalid"), IssuerCertificate: s.activation.IssuerCertificate(), OriginalCloseDataReceipt: []byte("invalid")}
			require.NoError(t, tr.WriteTaskQuiesceRequest(conn, e))
			if fault == "original-five-second" {
				// Wait at the real request's EOF boundary. Authority clock calls
				// have their own shorter one-second bound, so they cannot model
				// five seconds of request parsing/handshake consumption.
				original := <-tracked.deadline
				timer := time.NewTimer(time.Until(original.Add(-150 * time.Millisecond)))
				<-timer.C
			}
			require.NoError(t, conn.CloseWrite())
			var dispatched context.Context
			select {
			case dispatched = <-clock.entered:
			case <-ctx.Done():
				t.Fatal("did not reach actual quiesce dispatch")
			}
			deadline, _ := dispatched.Deadline()
			if fault == "delayed" || fault == "original-five-second" {
				timer := time.NewTimer(time.Until(deadline.Add(-75 * time.Millisecond)))
				<-timer.C
				close(clock.release)
			} else if fault == "canceled" {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Until(deadline.Add(150 * time.Millisecond))):
				t.Fatal("refused TLS request retained borrow past original deadline")
			}
			if fault == "expired" || fault == "canceled" {
				require.Zero(t, tracked.writes.Load(), "late error attempted encrypted application write")
			} else {
				require.Positive(t, tracked.writes.Load(), "must reach blocked encrypted refusal IO")
			}
			s.transport.workers.Wait()
			s.transport.mu.Lock()
			defer s.transport.mu.Unlock()
			require.Empty(t, s.transport.users)
		})
	}
}

// The finite repair must not turn the unchanged execution-output writer into
// an absolute connection timeout. This is transport IO, not execution proof.
func TestExecOutputWriterRetainsPerFrameDeadline(t *testing.T) {
	_, server, client := quiesceDeadlineTLS(t)
	raw, peer := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		defer raw.Close()
		conn := tls.Server(raw, server)
		deadline, _ := ctx.Deadline()
		_ = conn.SetDeadline(deadline)
		if err := conn.HandshakeContext(ctx); err != nil {
			done <- err
			return
		}
		<-ctx.Done()
		done <- writeTransportEvent(conn, tr.EventStdout, []byte("later execution output"))
	}()
	joined := false
	defer func() {
		cancel()
		raw.Close()
		peer.Close()
		if !joined {
			<-done
		}
	}()
	conn := tls.Client(peer, client)
	require.NoError(t, conn.HandshakeContext(ctx))
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	kind, wire, err := tr.ReadEvent(conn)
	require.NoError(t, err)
	require.Equal(t, tr.EventStdout, kind)
	require.Equal(t, "later execution output", string(wire))
	require.NoError(t, <-done)
	joined = true
}
