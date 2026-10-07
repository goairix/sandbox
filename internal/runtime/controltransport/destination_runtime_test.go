package controltransport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"io"
	"net"
	"testing"
	"time"
)

func TestDestinationOriginalRuntimeTrustCannotWiden(t *testing.T) {
	original, _, _, _, _, _ := destinationFixture(t)
	rogue, _, _, clock, _, _ := destinationFixture(t)
	rogue.Dial = func(context.Context) (net.Conn, error) { return nil, nil }
	d, err := NewDestination(rogue)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.VerifyRuntime(original.Verifier, original.Identity, clock.now); err == nil {
		t.Fatal("same tuple signed by caller's additional root widened original backend trust")
	}
	if err = d.VerifyRuntime(rogue.Verifier, rogue.Identity, clock.now); err != nil {
		t.Fatal(err)
	}
	wrong := rogue.Identity
	wrong.Runtime.UID = "wrong"
	if err = d.VerifyRuntime(rogue.Verifier, wrong, clock.now); err == nil {
		t.Fatal("wrong original identity accepted")
	}
	if err = d.VerifyRuntime(rogue.Verifier, rogue.Identity, clock.now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired original runtime accepted")
	}
	root, _, _ := ed25519.GenerateKey(rand.Reader)
	wrongBinding, err := p.NewManagementVerifier(p.TrustBinding{Namespace: "/sandbox/control/other/", AuthorityID: "authority", Target: "target", RestoreEpoch: "epoch"}, []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.VerifyRuntime(wrongBinding, rogue.Identity, clock.now); err == nil {
		t.Fatal("wrong backend binding accepted")
	}
}

func TestDestinationActualAddedRootPeerRejectedByOriginalTrust(t *testing.T) {
	original, _, issuerWire, _, _, originalRoot := destinationFixture(t)
	rogue, _, _, clock, runtimeKey, rogueRoot := destinationFixture(t)
	union, err := p.NewManagementVerifier(p.TrustBinding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "epoch"}, []ed25519.PublicKey{originalRoot, rogueRoot})
	if err != nil {
		t.Fatal(err)
	}
	rogue.Verifier = union
	rogue.Credential = original.Credential
	credential, err := p.NewManagementTLSCredential(runtimeKey, rogue.RuntimeCertificate, "runtime_receipt")
	if err != nil {
		t.Fatal(err)
	}
	server, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: union, Clock: clock, Credential: credential, PeerCertificate: issuerWire, Server: true})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	rogue.Dial = func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}
	d, err := NewDestination(rogue)
	if err != nil {
		t.Fatal(err)
	}
	c := p.ExecStartContext{SandboxID: original.Identity.SandboxID, WorkspaceHash: original.Identity.WorkspaceHash, Generation: original.Identity.Generation, Runtime: original.Identity.Runtime}
	if !d.Match(c, issuerWire) {
		t.Fatal("adversarial peer did not preserve exact original tuple and issuer")
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
		if err = conn.Handshake(); err != nil {
			done <- err
			return
		}
		var b [1]byte
		n, err := conn.Read(b[:])
		if n != 0 || err != io.EOF {
			done <- ErrFrame
			return
		}
		done <- nil
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := d.Open(ctx)
	if err != nil {
		t.Fatal("expanded destination TLS should demonstrate the attack precondition:", err)
	}
	verifyErr := d.VerifyRuntime(original.Verifier, original.Identity, clock.now)
	s.Close()
	if err = <-done; err != nil {
		t.Fatal("application byte leaked or handshake failed:", err)
	}
	if verifyErr == nil {
		t.Fatal("actual same-tuple peer under added root bypassed original backend runtime trust")
	}
}
