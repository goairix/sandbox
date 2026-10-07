package controltransport

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"reflect"
	"sync"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

var ErrDestination = errors.New("invalid control destination")

type DestinationOptions struct {
	Identity           p.RuntimeIdentityContext
	RuntimeCertificate []byte
	Credential         *p.ManagementTLSCredential
	Verifier           *p.ManagementVerifier
	Clock              p.AuthorityClock
	// Dial transfers exclusive byte-connection ownership and must honor context.
	// It never receives command payloads, tickets, credentials or private keys.
	// An optional CloseWrite() error must provide real bounded write EOF while
	// preserving reads. Send invokes it only after successful TLS CloseWrite;
	// arbitrary custom connection methods remain a trusted, non-preemptible seam.
	Dial func(context.Context) (net.Conn, error)
}

// Destination binds an immutable exact target, trust, clock and TLS credential.
// It is origin-sealed: zero and copied values cannot open a connection.
type Destination struct {
	self        *Destination
	identity    p.RuntimeIdentityContext
	certificate []byte
	credential  *p.ManagementTLSCredential
	verifier    *p.ManagementVerifier
	clock       p.AuthorityClock
	config      *tls.Config
	dial        func(context.Context) (net.Conn, error)
}

func NewDestination(o DestinationOptions) (*Destination, error) {
	if o.Dial == nil {
		return nil, ErrDestination
	}
	config, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: o.Verifier, Clock: o.Clock, Credential: o.Credential, PeerCertificate: o.RuntimeCertificate, PeerRuntime: &o.Identity})
	if err != nil {
		return nil, err
	}
	d := &Destination{identity: o.Identity, certificate: bytes.Clone(o.RuntimeCertificate), credential: o.Credential, verifier: o.Verifier, clock: o.Clock, config: config, dial: o.Dial}
	d.self = d
	return d, nil
}
func (d *Destination) Match(c p.ExecStartContext, issuer []byte) bool {
	return d != nil && d.self == d && d.identity == (p.RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}) && d.credential.MatchesCertificate(issuer)
}
func isNil(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	}
	return false
}

// Open performs only a fresh pinned mutual TLS handshake. The original backend
// must repeat all current authority fences before sending any application bytes.
func (d *Destination) Open(ctx context.Context) (*Session, error) {
	if d == nil || d.self != d || isNil(ctx) {
		return nil, ErrDestination
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := d.dial(bounded)
	if err != nil {
		if !isNil(raw) {
			raw.Close()
		}
		return nil, err
	}
	if isNil(raw) {
		return nil, ErrDestination
	}
	conn := tls.Client(raw, d.config)
	stop := closeOnContext(bounded, conn)
	defer stop()
	deadline, _ := bounded.Deadline()
	if err = conn.SetDeadline(deadline); err == nil {
		err = conn.HandshakeContext(bounded)
	}
	if err == nil {
		err = bounded.Err()
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	if err = conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	s := &Session{conn: conn}
	s.self = s
	return s, nil
}

// VerifyReceipt authenticates diagnostics against caller-retained exact accepted
// attribution. Wire values cannot select the expected deadline or runtime.
func (d *Destination) VerifyReceipt(ctx context.Context, wire []byte, c p.ExecStartContext, descriptor, ticket string, before, after, deadline time.Time) (p.LocalExecReceiptEvidence, error) {
	if d == nil || d.self != d || isNil(ctx) {
		return p.LocalExecReceiptEvidence{}, ErrDestination
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	o, err := d.clock.Observe(bounded)
	elapsed := time.Since(start)
	if err != nil {
		return p.LocalExecReceiptEvidence{}, err
	}
	if err = bounded.Err(); err != nil {
		return p.LocalExecReceiptEvidence{}, err
	}
	if o.UTC.IsZero() || o.UTC.Location() != time.UTC || o.Uncertainty < 0 || o.Uncertainty > time.Second || elapsed > time.Second-o.Uncertainty {
		return p.LocalExecReceiptEvidence{}, ErrDestination
	}
	return d.verifier.VerifyLocalExecReceipt(wire, d.certificate, c, descriptor, ticket, before, after, deadline, o.UTC.Add(elapsed))
}

// Session carries one finite request and its response. It owns no payload buffer.
type Session struct {
	self *Session
	conn *tls.Conn
	mu   sync.Mutex
	sent bool
}

func (s *Session) Close() error {
	if s == nil || s.self != s {
		return ErrDestination
	}
	return s.conn.Close()
}
func (s *Session) Send(ctx context.Context, e Envelope) error {
	return s.send(ctx, func(w io.Writer) error { return WriteRequest(w, e) })
}
func (s *Session) SendTaskClose(ctx context.Context, e TaskCloseEnvelope) error {
	return s.send(ctx, func(w io.Writer) error { return WriteTaskCloseRequest(w, e) })
}
func (s *Session) send(ctx context.Context, write func(io.Writer) error) error {
	if s == nil || s.self != s || isNil(ctx) {
		return ErrDestination
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent {
		return ErrFrame
	}
	s.sent = true
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	stop := closeOnContext(bounded, s.conn)
	defer stop()
	deadline, _ := bounded.Deadline()
	if err := s.conn.SetDeadline(deadline); err != nil {
		return err
	}
	if err := bounded.Err(); err != nil {
		return err
	}
	if err := write(s.conn); err != nil {
		return err
	}
	if err := s.conn.CloseWrite(); err != nil {
		return err
	}
	if err := bounded.Err(); err != nil {
		return err
	}
	// TLS EOF precedes transport EOF. A finite pipe/SSH/Unix adapter can then
	// stop its input owner without closing the response side. No raw getter is
	// exposed; unsupported transports retain the existing TLS-only behavior.
	if half, ok := s.conn.NetConn().(interface{ CloseWrite() error }); ok {
		if err := half.CloseWrite(); err != nil {
			return err
		}
	}
	return bounded.Err()
}
func (s *Session) Read(ctx context.Context) (byte, []byte, error) {
	if s == nil || s.self != s || isNil(ctx) {
		return 0, nil, ErrDestination
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.sent {
		return 0, nil, ErrFrame
	}
	if err := ctx.Err(); err != nil {
		return 0, nil, err
	}
	stop := closeOnContext(ctx, s.conn)
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := s.conn.SetReadDeadline(deadline); err != nil {
		return 0, nil, err
	}
	return ReadEvent(s.conn)
}
func closeOnContext(ctx context.Context, c net.Conn) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); c.Close() })
	return func() {
		if !stop() {
			<-done
		}
	}
}

// VerifyRuntime applies the original backend's immutable verifier and exact
// trusted identity to this destination's private certificate. A caller-supplied
// destination verifier cannot add roots or alter the backend's trust binding.
func (d *Destination) VerifyRuntime(verifier *p.ManagementVerifier, expected p.RuntimeIdentityContext, now time.Time) error {
	if d == nil || d.self != d || verifier == nil || d.identity != expected {
		return ErrDestination
	}
	_, err := verifier.VerifyRuntimeIdentityCertificate(d.certificate, expected, now)
	return err
}
