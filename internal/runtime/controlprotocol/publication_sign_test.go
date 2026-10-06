package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"
)

// A signer that omits validation would manufacture authenticated invalid data.
func TestPublicationRejectsInvalidClaimsBeforeSigning(t *testing.T) {
	certChanges := map[string]func(*RuntimeCertificateClaims){
		"version": func(c *RuntimeCertificateClaims) { c.Version = 2 }, "namespace": func(c *RuntimeCertificateClaims) { c.Namespace = "/a/b/" }, "authority control": func(c *RuntimeCertificateClaims) { c.AuthorityID = "a\n" }, "target utf8": func(c *RuntimeCertificateClaims) { c.Target = string([]byte{255}) }, "restore segment": func(c *RuntimeCertificateClaims) { c.RestoreEpoch = "a/b" }, "intent length": func(c *RuntimeCertificateClaims) { c.IntentID = strings.Repeat("a", 129) }, "sandbox empty": func(c *RuntimeCertificateClaims) { c.SandboxID = "" }, "workspace uppercase": func(c *RuntimeCertificateClaims) { c.WorkspaceHash = strings.Repeat("A", 64) }, "generation zero": func(c *RuntimeCertificateClaims) { c.Generation = 0 }, "operation noncanonical": func(c *RuntimeCertificateClaims) { c.OperationID = "11111111111141118111111111111111" }, "payload invalid": func(c *RuntimeCertificateClaims) { c.PayloadDigest = "" }, "snapshot version": func(c *RuntimeCertificateClaims) { c.Snapshot.Version = ".." }, "snapshot digest": func(c *RuntimeCertificateClaims) { c.Snapshot.Digest = "a" }, "expiry zero": func(c *RuntimeCertificateClaims) { c.ExpiresAt = time.Time{} }, "runtime empty": func(c *RuntimeCertificateClaims) { c.Runtime.ID = "" }, "runtime control": func(c *RuntimeCertificateClaims) { c.Runtime.UID = "x\u0085" }, "runtime length": func(c *RuntimeCertificateClaims) { c.Runtime.BootID = strings.Repeat("x", 129) }, "workspace mode": func(c *RuntimeCertificateClaims) { c.WorkspaceMode = "overlay" }, "public key short": func(c *RuntimeCertificateClaims) { c.PublicKey = []byte{1} }, "public key long": func(c *RuntimeCertificateClaims) { c.PublicKey = bytes.Repeat([]byte{1}, 33) }, "certificate interval equal": func(c *RuntimeCertificateClaims) { c.NotAfter = c.NotBefore }, "certificate past expiry": func(c *RuntimeCertificateClaims) { c.NotAfter = c.ExpiresAt.Add(time.Nanosecond) }, "non UTC": func(c *RuntimeCertificateClaims) { c.NotBefore = c.NotBefore.In(time.FixedZone("custom", 3600)) },
	}
	for name, mutate := range certChanges {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			mutate(&f.cert)
			if _, err := SignRuntimeCertificate(f.root, f.cert); err == nil {
				t.Fatal("signed invalid certificate")
			}
		})
	}
	readyChanges := map[string]func(*fixture){
		"version": func(f *fixture) { f.ready.Version = 2 }, "digest": func(f *fixture) { f.ready.CertificateDigest = strings.Repeat("d", 64) }, "claim UUID": func(f *fixture) { f.ready.Claim.ClaimID = "bad" }, "revision zero": func(f *fixture) { f.ready.Claim.CreateRevision = 0 }, "lease negative": func(f *fixture) { f.ready.Claim.LeaseID = -1 }, "gate zero": func(f *fixture) { f.ready.DataGateEpoch = 0 }, "gate state": func(f *fixture) { f.ready.GateState = "closed" }, "mount attempt two": func(f *fixture) { f.ready.MountAttempt = 2 }, "plain with operation": func(f *fixture) { f.ready.MountOperationID = "33333333-3333-4333-8333-333333333333" }, "plain mounted": func(f *fixture) {
			f.ready.MountAttempt = 1
			f.ready.MountOperationID = "33333333-3333-4333-8333-333333333333"
		}, "fuse unmounted": func(f *fixture) { f.cert.WorkspaceMode = "fuse" }, "fuse non UUID": func(f *fixture) {
			f.cert.WorkspaceMode = "fuse"
			f.ready.MountAttempt = 1
			f.ready.MountOperationID = "bad"
		}, "observed before certificate": func(f *fixture) { f.ready.ObservedAt = f.cert.NotBefore.Add(-time.Nanosecond) }, "empty window": func(f *fixture) { f.ready.ValidUntil = f.ready.ObservedAt }, "window too long": func(f *fixture) { f.ready.ValidUntil = f.ready.ObservedAt.Add(5*time.Second + time.Nanosecond) }, "valid past certificate": func(f *fixture) {
			f.ready.ObservedAt = f.cert.NotAfter.Add(-time.Second)
			f.ready.ValidUntil = f.cert.NotAfter.Add(time.Nanosecond)
		}, "non UTC": func(f *fixture) { f.ready.ObservedAt = f.ready.ObservedAt.In(time.FixedZone("custom", 3600)) },
	}
	for name, mutate := range readyChanges {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			cert, _ := f.wires(t)
			mutate(&f)
			if name == "fuse unmounted" || name == "fuse non UUID" {
				var err error
				cert, err = SignRuntimeCertificate(f.root, f.cert)
				if err != nil {
					t.Fatal(err)
				}
				f.ready.CertificateDigest = wireDigest(cert)
			}
			if _, err := SignReadyReceipt(f.delegate, cert, f.ready); err == nil {
				t.Fatal("signed invalid ready claims")
			}
		})
	}
}

func TestPublicationRejectsMalformedKeysWithoutPanic(t *testing.T) {
	f := newFixture(t)
	cert, _ := f.wires(t)
	for _, key := range []ed25519.PrivateKey{nil, {1}, bytes.Repeat([]byte{1}, 63), bytes.Repeat([]byte{1}, 65), bytes.Repeat([]byte{1}, 64), f.root} {
		if _, err := SignReadyReceipt(key, cert, f.ready); err == nil {
			t.Fatal("accepted invalid or unrelated delegate private key")
		}
	}
	for _, key := range []ed25519.PrivateKey{nil, {1}, bytes.Repeat([]byte{1}, 63), bytes.Repeat([]byte{1}, 65), bytes.Repeat([]byte{1}, 64)} {
		if _, err := SignRuntimeCertificate(key, f.cert); err == nil {
			t.Fatal("accepted malformed issuer private key")
		}
	}
	binding := TrustBinding{f.cert.Namespace, f.cert.AuthorityID, f.cert.Target, f.cert.RestoreEpoch}
	for _, keys := range [][]ed25519.PublicKey{nil, {{1}}, {bytes.Repeat([]byte{1}, 33)}, {ed25519.PublicKey(f.root[32:]), ed25519.PublicKey(f.delegate[32:]), bytes.Repeat([]byte{1}, 32)}} {
		if _, err := NewPublicationVerifier(binding, keys); err == nil {
			t.Fatal("accepted invalid root set")
		}
	}
	var verifier *PublicationVerifier
	if _, err := verifier.VerifyHistoricalCertificate(cert, f.context.Certificate); err == nil {
		t.Fatal("nil verifier accepted certificate")
	}
}
