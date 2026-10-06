package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	root, delegate ed25519.PrivateKey
	verifier       *PublicationVerifier
	cert           RuntimeCertificateClaims
	ready          ReadyReceiptClaims
	context        PublicationContext
	now            time.Time
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, 32))
	delegate := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, 32))
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	cc := CertificateContext{Namespace: "/sandbox/team/cell/", AuthorityID: "authority", Target: "target", RestoreEpoch: "epoch-1", IntentID: "intent", SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, OperationID: "11111111-1111-4111-8111-111111111111", PayloadDigest: strings.Repeat("b", 64), Snapshot: SnapshotReference{Version: "v1", Digest: strings.Repeat("c", 64)}, ExpiresAt: now.Add(time.Minute)}
	cert := RuntimeCertificateClaims{Version: 1, Namespace: cc.Namespace, AuthorityID: cc.AuthorityID, Target: cc.Target, RestoreEpoch: cc.RestoreEpoch, IntentID: cc.IntentID, SandboxID: cc.SandboxID, WorkspaceHash: cc.WorkspaceHash, Generation: cc.Generation, OperationID: cc.OperationID, PayloadDigest: cc.PayloadDigest, Snapshot: cc.Snapshot, ExpiresAt: cc.ExpiresAt, Runtime: RuntimeReference{ID: "runtime", UID: "uid", BootID: "boot"}, WorkspaceMode: "plain", PublicKey: delegate.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(30 * time.Second)}
	binding := TrustBinding{cc.Namespace, cc.AuthorityID, cc.Target, cc.RestoreEpoch}
	v, err := NewPublicationVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	if err != nil {
		t.Fatal(err)
	}
	claim := ClaimReference{ClaimID: "22222222-2222-4222-8222-222222222222", CreateRevision: 2, LeaseID: 3}
	ready := ReadyReceiptClaims{Version: 1, Claim: claim, DataGateEpoch: 4, GateState: "open", ObservedAt: now.Add(-time.Second), ValidUntil: now.Add(2 * time.Second)}
	return fixture{root, delegate, v, cert, ready, PublicationContext{Certificate: cc, Runtime: cert.Runtime, Claim: claim, DataGateEpoch: 4}, now}
}

func (f *fixture) wires(t *testing.T) ([]byte, []byte) {
	t.Helper()
	cert, err := SignRuntimeCertificate(f.root, f.cert)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cert)
	f.context.CertificateDigest = hex.EncodeToString(sum[:])
	f.ready.CertificateDigest = f.context.CertificateDigest
	proof, err := SignReadyReceipt(f.delegate, cert, f.ready)
	if err != nil {
		t.Fatal(err)
	}
	return cert, proof
}

// Removing root or delegate authentication must reject authentic-looking data.
func TestPublicationAuthenticatesAndBindsContext(t *testing.T) {
	f := newFixture(t)
	cert, proof := f.wires(t)
	ce, err := f.verifier.VerifyCertificate(cert, f.context.Certificate, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if ce.Runtime() != f.cert.Runtime || ce.WorkspaceMode() != "plain" {
		t.Fatal("certificate identity lost")
	}
	pe, err := f.verifier.Verify(proof, f.context, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if pe.Runtime() != f.cert.Runtime || pe.MountAttempt() != 0 {
		t.Fatal("publication identity lost")
	}
	sum := sha256.Sum256(pe.Wire())
	if pe.Digest() != hex.EncodeToString(sum[:]) {
		t.Fatal("digest differs from wire")
	}
	certsum := sha256.Sum256(ce.Wire())
	if ce.Digest() != hex.EncodeToString(certsum[:]) {
		t.Fatal("certificate digest differs from wire")
	}
	mutations := map[string]func(*PublicationContext){
		"namespace": func(c *PublicationContext) { c.Certificate.Namespace = "/sandbox/other/cell/" }, "authority": func(c *PublicationContext) { c.Certificate.AuthorityID = "other" }, "target": func(c *PublicationContext) { c.Certificate.Target = "other" }, "restore": func(c *PublicationContext) { c.Certificate.RestoreEpoch = "other" }, "intent": func(c *PublicationContext) { c.Certificate.IntentID = "other" }, "sandbox": func(c *PublicationContext) { c.Certificate.SandboxID = "other" }, "workspace": func(c *PublicationContext) { c.Certificate.WorkspaceHash = strings.Repeat("d", 64) }, "generation": func(c *PublicationContext) { c.Certificate.Generation++ }, "operation": func(c *PublicationContext) { c.Certificate.OperationID = "33333333-3333-4333-8333-333333333333" }, "payload": func(c *PublicationContext) { c.Certificate.PayloadDigest = strings.Repeat("d", 64) }, "snapshot version": func(c *PublicationContext) { c.Certificate.Snapshot.Version = "v2" }, "snapshot digest": func(c *PublicationContext) { c.Certificate.Snapshot.Digest = strings.Repeat("d", 64) }, "expiry": func(c *PublicationContext) { c.Certificate.ExpiresAt = c.Certificate.ExpiresAt.Add(time.Second) }, "runtime": func(c *PublicationContext) { c.Runtime.ID = "other" }, "uid": func(c *PublicationContext) { c.Runtime.UID = "other" }, "boot": func(c *PublicationContext) { c.Runtime.BootID = "other" }, "digest": func(c *PublicationContext) { c.CertificateDigest = strings.Repeat("d", 64) }, "claim": func(c *PublicationContext) { c.Claim.ClaimID = "33333333-3333-4333-8333-333333333333" }, "revision": func(c *PublicationContext) { c.Claim.CreateRevision++ }, "lease": func(c *PublicationContext) { c.Claim.LeaseID++ }, "gate": func(c *PublicationContext) { c.DataGateEpoch++ }, "attempt": func(c *PublicationContext) { c.MountAttempt = 1 }, "mount operation": func(c *PublicationContext) { c.MountOperationID = "33333333-3333-4333-8333-333333333333" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			c := f.context
			mutate(&c)
			if _, err := f.verifier.Verify(proof, c, f.now); err == nil {
				t.Fatal("accepted context mismatch")
			}
			if _, err := f.verifier.VerifyHistorical(proof, c); err == nil {
				t.Fatal("historical accepted mismatch")
			}
		})
	}
	for name, wire := range map[string][]byte{"tampered certificate": bytes.Replace(cert, []byte(`"runtime"`), []byte(`"other"`), 1), "tampered proof": bytes.Replace(proof, []byte(`"open"`), []byte(`"shut"`), 1)} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.verifier.Verify(wire, f.context, f.now); err == nil {
				t.Fatal("accepted tampering")
			}
		})
	}
	other := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))
	f.root = other
	_, proof = f.wires(t)
	if _, err := f.verifier.Verify(proof, f.context, f.now); err == nil {
		t.Fatal("accepted unregistered root")
	}
}

// Dropping conservative time margins would authorize a stale or future proof.
func TestPublicationTimeBoundariesAndHistorical(t *testing.T) {
	f := newFixture(t)
	cert, proof := f.wires(t)
	for _, now := range []time.Time{time.Time{}, f.cert.NotBefore.Add(time.Second - time.Nanosecond), f.cert.NotAfter.Add(-time.Second), f.cert.ExpiresAt, f.now.Add(time.Hour)} {
		if _, err := f.verifier.VerifyCertificate(cert, f.context.Certificate, now); err == nil {
			t.Fatalf("certificate accepted time %v", now)
		}
	}
	if _, err := f.verifier.VerifyCertificate(cert, f.context.Certificate, f.cert.NotBefore.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, now := range []time.Time{time.Time{}, f.ready.ObservedAt.Add(-2*time.Second - time.Nanosecond), f.ready.ObservedAt.Add(3*time.Second + time.Nanosecond), f.ready.ValidUntil.Add(-time.Second), f.now.Add(time.Hour)} {
		if _, err := f.verifier.Verify(proof, f.context, now); err == nil {
			t.Fatalf("proof accepted time %v", now)
		}
	}
	if _, err := f.verifier.VerifyHistoricalCertificate(cert, f.context.Certificate); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifier.VerifyHistorical(proof, f.context); err != nil {
		t.Fatal(err)
	}
	// A five-second signed window is legal, but its usable lifetime is narrowed.
	f.ready.ObservedAt = f.now.Add(-3 * time.Second)
	f.ready.ValidUntil = f.now.Add(2 * time.Second)
	_, proof = f.wires(t)
	if _, err := f.verifier.Verify(proof, f.context, f.now); err != nil {
		t.Fatal(err)
	}
	f.ready.ObservedAt = f.now.Add(2 * time.Second)
	f.ready.ValidUntil = f.now.Add(4 * time.Second)
	_, proof = f.wires(t)
	if _, err := f.verifier.Verify(proof, f.context, f.now); err != nil {
		t.Fatal(err)
	}
}

func TestPublicationFuseMountAndCertificateReuse(t *testing.T) {
	f := newFixture(t)
	f.cert.WorkspaceMode = "fuse"
	f.ready.MountAttempt = 1
	f.ready.MountOperationID = "33333333-3333-4333-8333-333333333333"
	f.context.MountAttempt = 1
	f.context.MountOperationID = f.ready.MountOperationID
	cert, proof := f.wires(t)
	if _, err := f.verifier.Verify(proof, f.context, f.now); err != nil {
		t.Fatal(err)
	}
	f.ready.Claim.CreateRevision++
	f.context.Claim = f.ready.Claim
	proof, err := SignReadyReceipt(f.delegate, cert, f.ready)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifier.Verify(proof, f.context, f.now); err != nil {
		t.Fatal("certificate could not be reused with new claim:", err)
	}
}

func TestPublicationCopyIsolationAndRootRotation(t *testing.T) {
	f := newFixture(t)
	rootpub := append(ed25519.PublicKey(nil), f.root.Public().(ed25519.PublicKey)...)
	second := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32))
	secondpub := second.Public().(ed25519.PublicKey)
	v, err := NewPublicationVerifier(TrustBinding{f.cert.Namespace, f.cert.AuthorityID, f.cert.Target, f.cert.RestoreEpoch}, []ed25519.PublicKey{rootpub, secondpub})
	if err != nil {
		t.Fatal(err)
	}
	rootpub[0] ^= 1
	cert, proof := f.wires(t)
	ce, err := v.VerifyCertificate(cert, f.context.Certificate, f.now)
	if err != nil {
		t.Fatal(err)
	}
	pe, err := v.Verify(proof, f.context, f.now)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), proof...)
	cert[0] = 0
	proof[0] = 0
	wire := pe.Wire()
	wire[0] = 0
	cw := ce.Wire()
	cw[0] = 0
	if !bytes.Equal(pe.Wire(), want) || ce.Wire()[0] != '{' {
		t.Fatal("evidence aliases caller memory")
	}
	f.root = second
	_, proof = f.wires(t)
	if _, err := v.Verify(proof, f.context, f.now); err != nil {
		t.Fatal(err)
	}
	duplicate, err := NewPublicationVerifier(TrustBinding{f.cert.Namespace, f.cert.AuthorityID, f.cert.Target, f.cert.RestoreEpoch}, []ed25519.PublicKey{secondpub, secondpub})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := duplicate.Verify(proof, f.context, f.now); err != nil {
		t.Fatal(err)
	}
}

// Hand-built envelope signing checks the documented domain, rather than using
// the production signer as the only source of verification expectations.
func TestPublicationSignatureDomainsAndNormalization(t *testing.T) {
	f := newFixture(t)
	cert, proof := f.wires(t)
	var c struct {
		Claims    json.RawMessage `json:"claims"`
		Signature []byte          `json:"signature"`
	}
	if err := json.Unmarshal(cert, &c); err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(f.root.Public().(ed25519.PublicKey), append([]byte("sandbox-runtime-certificate:v1\x00"), c.Claims...), c.Signature) {
		t.Fatal("wrong certificate signature domain")
	}
	var p struct {
		Certificate json.RawMessage `json:"certificate"`
		Claims      json.RawMessage `json:"claims"`
		Signature   []byte          `json:"signature"`
	}
	if err := json.Unmarshal(proof, &p); err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(f.delegate.Public().(ed25519.PublicKey), append([]byte("sandbox-runtime-ready:v1\x00"), p.Claims...), p.Signature) {
		t.Fatal("wrong ready signature domain")
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, proof, "", "  "); err != nil {
		t.Fatal(err)
	}
	e, err := f.verifier.Verify(pretty.Bytes(), f.context, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(e.Wire(), proof) {
		t.Fatal("wire was not normalized")
	}
	// The root key ID supplied by caller cannot impersonate a different root.
	f.cert.RootKeyID = strings.Repeat("f", 64)
	cert, _ = f.wires(t)
	if bytes.Contains(cert, []byte(strings.Repeat("f", 64))) {
		t.Fatal("root key ID was not derived")
	}
}

// Authentic signatures cannot bypass semantic validation, including historical
// reads. Fixtures sign directly to avoid the public signer's input safeguards.
func TestPublicationAuthenticatedInvalidStatements(t *testing.T) {
	f := newFixture(t)
	cert, proof := f.wires(t)
	var original certificateEnvelope
	if err := json.Unmarshal(cert, &original); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*RuntimeCertificateClaims){
		"version":          func(c *RuntimeCertificateClaims) { c.Version = 2 },
		"empty runtime":    func(c *RuntimeCertificateClaims) { c.Runtime.ID = "" },
		"invalid mode":     func(c *RuntimeCertificateClaims) { c.WorkspaceMode = "overlay" },
		"invalid interval": func(c *RuntimeCertificateClaims) { c.NotAfter = c.NotBefore },
		"past expiry":      func(c *RuntimeCertificateClaims) { c.NotAfter = c.ExpiresAt.Add(time.Second) },
	} {
		t.Run("certificate "+name, func(t *testing.T) {
			c := original
			c.Claims.PublicKey = append([]byte(nil), original.Claims.PublicKey...)
			mutate(&c.Claims)
			claims, err := json.Marshal(c.Claims)
			if err != nil {
				t.Fatal(err)
			}
			c.Signature = ed25519.Sign(f.root, append([]byte("sandbox-runtime-certificate:v1\x00"), claims...))
			wire, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.verifier.VerifyHistoricalCertificate(wire, f.context.Certificate); err == nil {
				t.Fatal("authenticated invalid certificate accepted")
			}
		})
	}
	var ready readyEnvelope
	if err := json.Unmarshal(proof, &ready); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*readyEnvelope, *PublicationContext){
		"version":     func(p *readyEnvelope, c *PublicationContext) { p.Claims.Version = 2 },
		"closed gate": func(p *readyEnvelope, c *PublicationContext) { p.Claims.GateState = "closed" },
		"zero gate":   func(p *readyEnvelope, c *PublicationContext) { p.Claims.DataGateEpoch = 0; c.DataGateEpoch = 0 },
		"invalid claim": func(p *readyEnvelope, c *PublicationContext) {
			p.Claims.Claim.ClaimID = "bad"
			c.Claim = p.Claims.Claim
		},
		"zero revision": func(p *readyEnvelope, c *PublicationContext) {
			p.Claims.Claim.CreateRevision = 0
			c.Claim = p.Claims.Claim
		},
		"zero lease": func(p *readyEnvelope, c *PublicationContext) { p.Claims.Claim.LeaseID = 0; c.Claim = p.Claims.Claim },
		"mount plain": func(p *readyEnvelope, c *PublicationContext) {
			p.Claims.MountAttempt = 1
			p.Claims.MountOperationID = "33333333-3333-4333-8333-333333333333"
			c.MountAttempt = 1
			c.MountOperationID = p.Claims.MountOperationID
		},
		"oversize window": func(p *readyEnvelope, c *PublicationContext) {
			p.Claims.ValidUntil = p.Claims.ObservedAt.Add(6 * time.Second)
		},
		"outside certificate": func(p *readyEnvelope, c *PublicationContext) {
			p.Claims.ObservedAt = p.Certificate.Claims.NotBefore.Add(-time.Nanosecond)
			p.Claims.ValidUntil = p.Claims.ObservedAt.Add(time.Second)
		},
	} {
		t.Run("ready "+name, func(t *testing.T) {
			p := ready
			c := f.context
			mutate(&p, &c)
			claims, err := json.Marshal(p.Claims)
			if err != nil {
				t.Fatal(err)
			}
			p.Signature = ed25519.Sign(f.delegate, append([]byte("sandbox-runtime-ready:v1\x00"), claims...))
			wire, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.verifier.VerifyHistorical(wire, c); err == nil {
				t.Fatal("authenticated invalid ready statement accepted")
			}
		})
	}
	// Both signatures must be authenticated even when all claims match.
	original.Signature[0] ^= 1
	badcert, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifier.VerifyHistoricalCertificate(badcert, f.context.Certificate); err == nil {
		t.Fatal("invalid root signature accepted")
	}
	ready.Signature[0] ^= 1
	badproof, err := json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifier.VerifyHistorical(badproof, f.context); err == nil {
		t.Fatal("invalid delegate signature accepted")
	}
}

func TestPublicationExpiryMarginAndExactIntegerRoundTrip(t *testing.T) {
	f := newFixture(t)
	f.cert.NotAfter = f.cert.ExpiresAt
	f.cert.Generation = 9007199254740993
	f.context.Certificate.Generation = f.cert.Generation
	f.ready.Claim.CreateRevision = 9007199254740993
	f.context.Claim = f.ready.Claim
	cert, proof := f.wires(t)
	if _, err := f.verifier.Verify(proof, f.context, f.now); err != nil {
		t.Fatal("int64 lost precision:", err)
	}
	if _, err := f.verifier.VerifyCertificate(cert, f.context.Certificate, f.cert.ExpiresAt.Add(-time.Second-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifier.VerifyCertificate(cert, f.context.Certificate, f.cert.ExpiresAt.Add(-time.Second)); err == nil {
		t.Fatal("expiry margin boundary accepted")
	}
	if _, err := f.verifier.VerifyCertificate(cert, f.context.Certificate, f.now.In(time.FixedZone("offset", 3600))); err == nil {
		t.Fatal("non-UTC clock accepted")
	}
}

func TestPublicationOpaqueIdentitiesAndInputIsolation(t *testing.T) {
	f := newFixture(t)
	f.cert.Runtime = RuntimeReference{ID: "runtime/实例", UID: "UID: α", BootID: strings.Repeat("x", 128)}
	f.context.Runtime = f.cert.Runtime
	pub := f.cert.PublicKey
	cert, proof := f.wires(t)
	pub[0] ^= 1
	f.root[0] ^= 1
	f.delegate[0] ^= 1
	if _, err := f.verifier.VerifyCertificate(cert, f.context.Certificate, f.now); err != nil {
		t.Fatal("certificate aliases signer input:", err)
	}
	if _, err := f.verifier.Verify(proof, f.context, f.now); err != nil {
		t.Fatal("proof aliases signer input:", err)
	}
}
