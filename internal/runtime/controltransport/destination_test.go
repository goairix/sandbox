package controltransport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
)

type testClock struct {
	now   time.Time
	calls atomic.Int32
}

func (c *testClock) Observe(context.Context) (p.ClockObservation, error) {
	c.calls.Add(1)
	return p.ClockObservation{UTC: c.now, Uncertainty: time.Millisecond}, nil
}
func destinationFixture(t *testing.T) (DestinationOptions, *tls.Config, []byte, *testClock, ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	root, key, _ := ed25519.GenerateKey(rand.Reader)
	_, ik, _ := ed25519.GenerateKey(rand.Reader)
	_, rk, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	binding := p.TrustBinding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "epoch"}
	identity := p.RuntimeIdentityContext{SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: p.RuntimeReference{ID: "runtime", UID: "uid", BootID: "6a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"}}
	iw, err := p.SignCommandIssuerCertificate(key, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: "5a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: ik.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	rw, err := p.SignRuntimeIdentityCertificate(key, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: "7a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: rk.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, Runtime: identity.Runtime})
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	ic, err := p.NewManagementTLSCredential(ik, iw, "command_issuer")
	if err != nil {
		t.Fatal(err)
	}
	rc, err := p.NewManagementTLSCredential(rk, rw, "runtime_receipt")
	if err != nil {
		t.Fatal(err)
	}
	clock := &testClock{now: now}
	server, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: v, Clock: clock, Credential: rc, PeerCertificate: iw, Server: true})
	if err != nil {
		t.Fatal(err)
	}
	return DestinationOptions{Identity: identity, Credential: ic, Verifier: v, Clock: clock, RuntimeCertificate: rw}, server, iw, clock, rk, root
}
func TestDestinationActualTLSFiniteRequest(t *testing.T) {
	o, server, iw, clock, _, _ := destinationFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	o.Dial = func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", listener.Addr().String())
	}
	d, err := NewDestination(o)
	if err != nil {
		t.Fatal(err)
	}
	if clock.calls.Load() != 0 {
		t.Fatal("idle clock call")
	}
	c := p.ExecStartContext{SandboxID: o.Identity.SandboxID, WorkspaceHash: o.Identity.WorkspaceHash, Generation: o.Identity.Generation, Runtime: o.Identity.Runtime}
	if !d.Match(c, iw) {
		t.Fatal("exact destination mismatch")
	}
	c.Runtime.BootID = "wrong"
	if d.Match(c, iw) {
		t.Fatal("wrong birth matched")
	}
	copied := *d
	if copied.Match(p.ExecStartContext{}, iw) {
		t.Fatal("copy matched")
	}
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
			_, err = ReadRequest(conn)
		}
		if err == nil {
			err = WriteEvent(conn, EventAccepted, []byte("accepted"))
		}
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := d.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Send(ctx, frameRequest(t)); err != nil {
		t.Fatal(err)
	}
	k, wire, err := s.Read(ctx)
	if err != nil || k != EventAccepted || string(wire) != "accepted" {
		t.Fatalf("%d %s %v", k, wire, err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if clock.calls.Load() != 2 {
		t.Fatalf("handshake clock calls %d", clock.calls.Load())
	}
	if err = s.Send(ctx, frameRequest(t)); err == nil {
		t.Fatal("connection replay accepted")
	}
}
func TestDestinationRejectsZeroAndCanceled(t *testing.T) {
	for _, d := range []*Destination{nil, {}} {
		if _, err := d.Open(context.Background()); err == nil {
			t.Fatal("zero destination opened")
		}
	}
	o, _, _, _, _, _ := destinationFixture(t)
	called := false
	o.Dial = func(context.Context) (net.Conn, error) { called = true; return nil, nil }
	d, err := NewDestination(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = d.Open(ctx); err == nil || called {
		t.Fatal("canceled dial reached transport")
	}
}

func TestDestinationReceiptPinsExactRetainedDeadline(t *testing.T) {
	o, _, iw, clock, key, _ := destinationFixture(t)
	o.Dial = func(context.Context) (net.Conn, error) { return nil, nil }
	d, err := NewDestination(o)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(iw)
	c := p.ExecStartContext{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "epoch", IssuerCertificateID: "5a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", IssuerCertificateDigest: hex.EncodeToString(digest[:]), CommandID: "4a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", OperationID: "3a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", RequestID: "request", OperationDigest: strings.Repeat("b", 64), SandboxID: o.Identity.SandboxID, WorkspaceHash: o.Identity.WorkspaceHash, Generation: 1, DataGateEpoch: 1, ControlRevision: 1, AdmissionRevision: 2, LeaseID: 3, Runtime: o.Identity.Runtime, ExpiresAt: clock.now.Add(time.Minute)}
	before, after := clock.now.Add(-2*time.Second), clock.now.Add(20*time.Second)
	hash := strings.Repeat("c", 64)
	wire, err := p.SignLocalExecReceipt(key, p.LocalExecReceiptClaims{Version: 1, State: "accepted", Context: c, DescriptorDigest: hash, TicketDigest: hash, NotBefore: before, NotAfter: after, AuthorityDeadline: after})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.VerifyReceipt(context.Background(), wire, c, hash, hash, before, after, after); err != nil {
		t.Fatal(err)
	}
	if _, err = d.VerifyReceipt(context.Background(), wire, c, hash, hash, before, after, after.Add(time.Second)); err == nil {
		t.Fatal("receipt selected expected deadline")
	}
	wire[len(wire)-4] ^= 1
	if _, err = d.VerifyReceipt(context.Background(), wire, c, hash, hash, before, after, after); err == nil {
		t.Fatal("tampered receipt accepted")
	}
}
