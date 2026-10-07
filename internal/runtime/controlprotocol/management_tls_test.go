package controlprotocol

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func tlsFixture(t *testing.T) (activationFixture, *tls.Config, *tls.Config, *atomic.Int32) {
	t.Helper()
	f := newActivationFixture(t)
	calls := new(atomic.Int32)
	clock := clockFunc(func(context.Context) (ClockObservation, error) {
		calls.Add(1)
		return ClockObservation{UTC: f.now, Uncertainty: time.Millisecond}, nil
	})
	runtime, err := NewManagementTLSCredential(f.runtimeKey, f.runtimeWire, "runtime_receipt")
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := NewManagementTLSCredential(f.issuerKey, f.issuerWire, "command_issuer")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewManagementTLSConfig(ManagementTLSOptions{Verifier: f.verifier, Clock: clock, Credential: runtime, PeerCertificate: f.issuerWire, Server: true})
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewManagementTLSConfig(ManagementTLSOptions{Verifier: f.verifier, Clock: clock, Credential: issuer, PeerCertificate: f.runtimeWire, PeerRuntime: &f.claims.Identity})
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("config creation must not observe idle clock")
	}
	return f, client, server, calls
}
func actualTLSHandshake(client, server *tls.Config) (error, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err, err
	}
	defer listener.Close()
	a, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		return err, err
	}
	defer a.Close()
	b, err := listener.Accept()
	if err != nil {
		return err, err
	}
	defer b.Close()
	deadline := time.Now().Add(2 * time.Second)
	a.SetDeadline(deadline)
	b.SetDeadline(deadline)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { defer b.Close(); done <- tls.Server(b, server).HandshakeContext(ctx) }()
	ce := tls.Client(a, client).HandshakeContext(ctx)
	if ce != nil {
		a.Close()
	}
	se := <-done
	return ce, se
}
func TestManagementTLSHandshake(t *testing.T) {
	_, client, server, calls := tlsFixture(t)
	if ce, se := actualTLSHandshake(client, server); ce != nil || se != nil {
		t.Fatalf("handshake failed client=%v server=%v", ce, se)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected two fresh observations, got %d", calls.Load())
	}
	if ce, se := actualTLSHandshake(client, server); ce != nil || se != nil {
		t.Fatalf("second handshake failed: %v %v", ce, se)
	}
	if calls.Load() != 4 {
		t.Fatal("connection reused clock verification")
	}
	for _, c := range []*tls.Config{client, server} {
		if c.MinVersion != tls.VersionTLS13 || c.MaxVersion != tls.VersionTLS13 || !c.SessionTicketsDisabled || c.ClientSessionCache != nil || len(c.NextProtos) != 1 || c.NextProtos[0] != "sandbox-control/v1" || c.VerifyConnection == nil {
			t.Fatal("TLS policy missing")
		}
	}
	if server.ClientAuth != tls.RequireAnyClientCert {
		t.Fatal("mutual TLS not mandatory")
	}
}
func TestManagementTLSRestrictedSigner(t *testing.T) {
	_, client, server, _ := tlsFixture(t)
	for _, tc := range []struct {
		c    *tls.Config
		role string
	}{{client, "client"}, {server, "server"}} {
		t.Run(tc.role, func(t *testing.T) {
			raw := tc.c.Certificates[0].PrivateKey
			if _, ok := raw.(ed25519.PrivateKey); ok {
				t.Fatal("raw key exposed")
			}
			signer, ok := raw.(crypto.Signer)
			if !ok {
				t.Fatal("missing signer")
			}
			public := signer.Public().(ed25519.PublicKey)
			public[0] ^= 1
			if bytes.Equal(public, signer.Public().(ed25519.PublicKey)) {
				t.Fatal("mutable public key")
			}
			correct := append(bytes.Repeat([]byte(" "), 64), []byte("TLS 1.3, "+tc.role+" CertificateVerify\x00")...)
			for _, n := range []int{32, 48} {
				input := append(append([]byte(nil), correct...), make([]byte, n)...)
				sig, err := signer.Sign(rand.Reader, input, crypto.Hash(0))
				if err != nil || !ed25519.Verify(signer.Public().(ed25519.PublicKey), input, sig) {
					t.Fatal("valid TLS input rejected")
				}
			}
			wrongRole := "client"
			if tc.role == "client" {
				wrongRole = "server"
			}
			forbidden := [][]byte{[]byte("sandbox-exec-start-ticket:v1\x00"), []byte("sandbox-operation:v1\x00"), []byte("sandbox-exec-renew:v1\x00"), []byte("sandbox-target-activation:v1\x00"), []byte("sandbox-authority-clock-observation:v1\x00"), []byte("sandbox-runtime-ready:v1\x00"), append(append(bytes.Repeat([]byte(" "), 64), []byte("TLS 1.3, "+wrongRole+" CertificateVerify\x00")...), make([]byte, 32)...), append(correct, make([]byte, 31)...)}
			for _, input := range forbidden {
				if _, err := signer.Sign(rand.Reader, input, crypto.Hash(0)); err == nil {
					t.Fatal("signer accepted non-TLS or wrong-role input")
				}
			}
			valid := append(append([]byte(nil), correct...), make([]byte, 32)...)
			for _, opts := range []crypto.SignerOpts{nil, crypto.SHA256, &ed25519.Options{Hash: crypto.SHA512}} {
				if _, err := signer.Sign(rand.Reader, valid, opts); err == nil {
					t.Fatal("signer accepted invalid options")
				}
			}
		})
	}
}
func TestManagementTLSRejectsActualPeers(t *testing.T) {
	for _, name := range []string{"missing-client", "wrong-key", "role", "tampered-leaf", "chain", "alpn", "tls12", "ca", "eku", "usage", "expired-leaf"} {
		t.Run(name, func(t *testing.T) {
			f, client, server, _ := tlsFixture(t)
			switch name {
			case "missing-client":
				client.Certificates = nil
			case "alpn":
				client.NextProtos = []string{"other"}
			case "tls12":
				client.MinVersion = tls.VersionTLS12
				client.MaxVersion = tls.VersionTLS12
			case "chain":
				client.Certificates[0].Certificate = append(client.Certificates[0].Certificate, client.Certificates[0].Certificate[0])
			case "tampered-leaf":
				raw := client.Certificates[0].Certificate[0]
				raw[len(raw)-1] ^= 1
			default:
				key := f.issuerKey
				leaf, err := x509.ParseCertificate(client.Certificates[0].Certificate[0])
				if err != nil {
					t.Fatal(err)
				}
				leaf.SerialNumber = big.NewInt(2)
				switch name {
				case "wrong-key":
					_, key = managementTestKey(t)
				case "role":
					leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
				case "ca":
					leaf.IsCA = true
				case "eku":
					leaf.ExtKeyUsage = append(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
				case "usage":
					leaf.KeyUsage |= x509.KeyUsageCertSign
				case "expired-leaf":
					leaf.NotAfter = f.now.Add(-time.Second)
				}
				leaf.PublicKey = key.Public()
				der, err := x509.CreateCertificate(rand.Reader, leaf, leaf, key.Public(), key)
				if err != nil {
					t.Fatal(err)
				}
				client.Certificates = []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}
			}
			if ce, se := actualTLSHandshake(client, server); ce == nil && se == nil {
				t.Fatal("accepted invalid peer")
			}
		})
	}
}
func TestManagementTLSFreshPinnedIdentity(t *testing.T) {
	for _, name := range []string{"uid", "boot", "binding", "root", "issuer", "clock", "nil-clock", "nil-credential"} {
		t.Run(name, func(t *testing.T) {
			f, client, server, _ := tlsFixture(t)
			credential, err := NewManagementTLSCredential(f.issuerKey, f.issuerWire, "command_issuer")
			if err != nil {
				t.Fatal(err)
			}
			options := ManagementTLSOptions{Verifier: f.verifier, Clock: clockFunc(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: f.now}, nil }), Credential: credential, PeerCertificate: f.runtimeWire, PeerRuntime: &f.claims.Identity}
			switch name {
			case "uid":
				options.PeerRuntime.Runtime.UID = "wrong"
			case "boot":
				options.PeerRuntime.Runtime.BootID = "8a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
			case "binding":
				b := f.claims.Binding
				b.Target = "other"
				options.Verifier, _ = NewManagementVerifier(b, []ed25519.PublicKey{ed25519.PublicKey(f.root[32:])})
			case "root":
				p, _ := managementTestKey(t)
				options.Verifier, _ = NewManagementVerifier(f.claims.Binding, []ed25519.PublicKey{p})
			case "issuer":
				options.PeerCertificate = f.issuerWire
			case "clock":
				options.Clock = clockFunc(func(context.Context) (ClockObservation, error) {
					return ClockObservation{UTC: f.now.Add(2 * time.Hour)}, nil
				})
			case "nil-clock":
				var c clockFunc
				options.Clock = c
			case "nil-credential":
				options.Credential = nil
			}
			client, err = NewManagementTLSConfig(options)
			if err == nil {
				if ce, se := actualTLSHandshake(client, server); ce == nil && se == nil {
					t.Fatal("accepted mismatched trusted identity")
				}
			}
		})
	}
}

func TestManagementTLSCredentialCopiesAndRejects(t *testing.T) {
	f := newActivationFixture(t)
	for _, tc := range []struct {
		key  ed25519.PrivateKey
		wire []byte
		role string
	}{{f.runtimeKey, f.issuerWire, "command_issuer"}, {f.issuerKey, f.issuerWire, "runtime_receipt"}, {f.issuerKey, f.issuerWire, "other"}, {nil, f.issuerWire, "command_issuer"}, {f.issuerKey, []byte(`{}`), "command_issuer"}} {
		if _, err := NewManagementTLSCredential(tc.key, tc.wire, tc.role); err == nil {
			t.Fatal("accepted credential mismatch")
		}
	}
	key := append(ed25519.PrivateKey(nil), f.issuerKey...)
	wire := append([]byte(nil), f.issuerWire...)
	credential, err := NewManagementTLSCredential(key, wire, "command_issuer")
	if err != nil {
		t.Fatal(err)
	}
	key[0] ^= 1
	wire[0] ^= 1
	peer := append([]byte(nil), f.runtimeWire...)
	identity := f.claims.Identity
	opts := ManagementTLSOptions{Verifier: f.verifier, Clock: clockFunc(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: f.now}, nil }), Credential: credential, PeerCertificate: peer, PeerRuntime: &identity}
	client, err := NewManagementTLSConfig(opts)
	if err != nil {
		t.Fatal(err)
	}
	peer[0] ^= 1
	identity.Runtime.UID = "changed"
	runtime, err := NewManagementTLSCredential(f.runtimeKey, f.runtimeWire, "runtime_receipt")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewManagementTLSConfig(ManagementTLSOptions{Verifier: f.verifier, Clock: opts.Clock, Credential: runtime, PeerCertificate: f.issuerWire, Server: true})
	if err != nil {
		t.Fatal(err)
	}
	if ce, se := actualTLSHandshake(client, server); ce != nil || se != nil {
		t.Fatalf("copied credential/options mutated: %v %v", ce, se)
	}
}
func TestManagementTLSClockFreshnessAndResumption(t *testing.T) {
	f, client, server, _ := tlsFixture(t)
	credential, err := NewManagementTLSCredential(f.issuerKey, f.issuerWire, "command_issuer")
	if err != nil {
		t.Fatal(err)
	}
	var expired atomic.Bool
	clock := clockFunc(func(context.Context) (ClockObservation, error) {
		now := f.now
		if expired.Load() {
			now = now.Add(2 * time.Hour)
		}
		return ClockObservation{UTC: now}, nil
	})
	client, err = NewManagementTLSConfig(ManagementTLSOptions{Verifier: f.verifier, Clock: clock, Credential: credential, PeerCertificate: f.runtimeWire, PeerRuntime: &f.claims.Identity})
	if err != nil {
		t.Fatal(err)
	}
	if ce, se := actualTLSHandshake(client, server); ce != nil || se != nil {
		t.Fatalf("initial handshake: %v %v", ce, se)
	}
	expired.Store(true)
	if ce, se := actualTLSHandshake(client, server); ce == nil && se == nil {
		t.Fatal("reused stale custom certificate verification")
	}
	state := tls.ConnectionState{Version: tls.VersionTLS13, NegotiatedProtocol: "sandbox-control/v1", DidResume: true}
	for _, c := range []*tls.Config{client, server} {
		if c.VerifyConnection(state) == nil {
			t.Fatal("resumption accepted")
		}
	}
}
