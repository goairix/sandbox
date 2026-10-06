package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func managementTestKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return public, private
}
func managementTestClaims(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, CommandIssuerCertificateClaims, time.Time) {
	t.Helper()
	root, private := managementTestKey(t)
	delegate, _ := managementTestKey(t)
	now := time.Date(2026, 10, 6, 3, 0, 0, 0, time.UTC)
	return root, private, CommandIssuerCertificateClaims{Version: 1, RootKeyID: "caller-must-not-select-root", CertificateID: "4a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", Role: "command_issuer", Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "restore-1", PublicKey: delegate, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}, now
}
func managementTestBinding(c CommandIssuerCertificateClaims) TrustBinding {
	return TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}
}

// This intentionally signs malformed claims directly, so verifier rejection is
// tested independently of the signer's validation and domain implementation.
func managementRawWire(t *testing.T, key ed25519.PrivateKey, c CommandIssuerCertificateClaims, domain string) []byte {
	t.Helper()
	sum := sha256.Sum256(key[ed25519.SeedSize:])
	c.RootKeyID = hex.EncodeToString(sum[:])
	claims, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(struct {
		Claims    CommandIssuerCertificateClaims `json:"claims"`
		Signature []byte                         `json:"signature"`
	}{c, ed25519.Sign(key, append([]byte(domain), claims...))})
	if err != nil {
		t.Fatal(err)
	}
	return wire
}
func managementReject(t *testing.T, v *ManagementVerifier, wire []byte, now time.Time) {
	t.Helper()
	identity, err := v.VerifyCommandIssuerCertificate(wire, now)
	if err == nil {
		t.Fatal("accepted invalid command issuer certificate")
	}
	if !reflect.DeepEqual(identity, CommandIssuerIdentity{}) {
		t.Fatalf("returned partial identity on rejection: %+v", identity)
	}
}

func TestCommandIssuerIdentity(t *testing.T) {
	root, private, claims, now := managementTestClaims(t)
	v, err := NewManagementVerifier(managementTestBinding(claims), []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := SignCommandIssuerCertificate(private, claims)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Claims    CommandIssuerCertificateClaims `json:"claims"`
		Signature []byte                         `json:"signature"`
	}
	if err := json.Unmarshal(wire, &envelope); err != nil {
		t.Fatal(err)
	}
	rootHash := sha256.Sum256(root)
	if envelope.Claims.RootKeyID != hex.EncodeToString(rootHash[:]) {
		t.Fatal("root key ID was not derived from signing root")
	}
	serializedClaims, _ := json.Marshal(envelope.Claims)
	if !ed25519.Verify(root, append([]byte("sandbox-command-issuer-certificate:v1\x00"), serializedClaims...), envelope.Signature) {
		t.Fatal("certificate does not use command issuer domain with actual NUL")
	}
	originalWire := append([]byte(nil), wire...)
	originalPublic := append(ed25519.PublicKey(nil), claims.PublicKey...)
	claims.PublicKey[0] ^= 1
	private[0] ^= 1
	identity, err := v.VerifyCommandIssuerCertificate(wire, now)
	if err != nil {
		t.Fatal(err)
	}
	if identity.CertificateID() != claims.CertificateID || !identity.NotBefore().Equal(claims.NotBefore) || !identity.NotAfter().Equal(claims.NotAfter) || !bytes.Equal(identity.PublicKey(), originalPublic) {
		t.Fatal("identity attributes mismatch")
	}
	digest := sha256.Sum256(originalWire)
	if identity.Digest() != hex.EncodeToString(digest[:]) {
		t.Fatal("identity digest mismatch")
	}
	wire[0] ^= 1
	returnedWire, returnedKey := identity.Wire(), identity.PublicKey()
	returnedWire[0] ^= 1
	returnedKey[0] ^= 1
	if !bytes.Equal(identity.Wire(), originalWire) || !bytes.Equal(identity.PublicKey(), originalPublic) {
		t.Fatal("mutable bytes changed authenticated identity")
	}
	// Whitespace is normalized before digest/evidence creation.
	pretty := new(bytes.Buffer)
	if err := json.Indent(pretty, originalWire, "", " "); err != nil {
		t.Fatal(err)
	}
	normalized, err := v.VerifyCommandIssuerCertificate(pretty.Bytes(), now)
	if err != nil || normalized.Digest() != identity.Digest() || !bytes.Equal(normalized.Wire(), identity.Wire()) {
		t.Fatalf("normalization failed: %v", err)
	}
	// Lower boundary equality is accepted; upper boundary equality is rejected.
	if _, err := v.VerifyCommandIssuerCertificate(originalWire, claims.NotBefore.Add(time.Second)); err != nil {
		t.Fatalf("lower equality rejected: %v", err)
	}
	managementReject(t, v, originalWire, claims.NotBefore.Add(time.Second-time.Nanosecond))
	if _, err := v.VerifyCommandIssuerCertificate(originalWire, claims.NotAfter.Add(-time.Second-time.Nanosecond)); err != nil {
		t.Fatalf("last conservative instant rejected: %v", err)
	}
	managementReject(t, v, originalWire, claims.NotAfter.Add(-time.Second))
}

func TestCommandIssuerIdentityRejects(t *testing.T) {
	root, private, claims, now := managementTestClaims(t)
	v, err := NewManagementVerifier(managementTestBinding(claims), []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	valid := managementRawWire(t, private, claims, "sandbox-command-issuer-certificate:v1\x00")
	rootHash := sha256.Sum256(root)
	rootID := hex.EncodeToString(rootHash[:])
	invalidClaims := []struct {
		name   string
		change func(*CommandIssuerCertificateClaims)
	}{
		{"version", func(c *CommandIssuerCertificateClaims) { c.Version = 2 }},
		{"role", func(c *CommandIssuerCertificateClaims) { c.Role = "runtime_receipt" }},
		{"empty-role", func(c *CommandIssuerCertificateClaims) { c.Role = "" }},
		{"nil-uuid", func(c *CommandIssuerCertificateClaims) { c.CertificateID = "00000000-0000-0000-0000-000000000000" }},
		{"noncanonical-uuid", func(c *CommandIssuerCertificateClaims) { c.CertificateID = strings.ToUpper(c.CertificateID) }},
		{"namespace", func(c *CommandIssuerCertificateClaims) { c.Namespace = "/sandbox/control/other/" }},
		{"invalid-namespace", func(c *CommandIssuerCertificateClaims) { c.Namespace = "bad" }},
		{"authority", func(c *CommandIssuerCertificateClaims) { c.AuthorityID = "other" }},
		{"authority-length", func(c *CommandIssuerCertificateClaims) { c.AuthorityID = strings.Repeat("a", 129) }},
		{"authority-control", func(c *CommandIssuerCertificateClaims) { c.AuthorityID = "a\n" }},
		{"target", func(c *CommandIssuerCertificateClaims) { c.Target = "other" }},
		{"empty-target", func(c *CommandIssuerCertificateClaims) { c.Target = "" }},
		{"restore", func(c *CommandIssuerCertificateClaims) { c.RestoreEpoch = "other" }},
		{"invalid-restore", func(c *CommandIssuerCertificateClaims) { c.RestoreEpoch = ".." }},
		{"short-delegate", func(c *CommandIssuerCertificateClaims) { c.PublicKey = c.PublicKey[:31] }},
		{"long-delegate", func(c *CommandIssuerCertificateClaims) { c.PublicKey = append(c.PublicKey, 0) }},
		{"root-self-delegate", func(c *CommandIssuerCertificateClaims) { c.PublicKey = root }},
		{"equal-interval", func(c *CommandIssuerCertificateClaims) { c.NotAfter = c.NotBefore }},
		{"reversed-interval", func(c *CommandIssuerCertificateClaims) { c.NotAfter = c.NotBefore.Add(-time.Second) }},
		{"zero-notbefore", func(c *CommandIssuerCertificateClaims) { c.NotBefore = time.Time{} }},
		{"zero-notafter", func(c *CommandIssuerCertificateClaims) { c.NotAfter = time.Time{} }},
	}
	for _, tc := range invalidClaims {
		t.Run(tc.name, func(t *testing.T) {
			c := claims
			c.PublicKey = append([]byte(nil), claims.PublicKey...)
			tc.change(&c)
			managementReject(t, v, managementRawWire(t, private, c, "sandbox-command-issuer-certificate:v1\x00"), now)
		})
	}
	for _, domain := range []string{"sandbox-runtime-certificate:v1\x00", "sandbox-runtime-ready:v1\x00", "sandbox-runtime-identity-certificate:v1\x00", "sandbox-command-issuer-certificate:v1\\x00", "sandbox-command-issuer-certificate:v1"} {
		t.Run("domain-"+domain, func(t *testing.T) { managementReject(t, v, managementRawWire(t, private, claims, domain), now) })
	}
	_, delegatePrivate := managementTestKey(t)
	managementReject(t, v, managementRawWire(t, delegatePrivate, claims, "sandbox-command-issuer-certificate:v1\x00"), now)
	// A delegate also cannot forge a certificate that names a known pinned root.
	var forged struct {
		Claims    CommandIssuerCertificateClaims `json:"claims"`
		Signature []byte                         `json:"signature"`
	}
	if err := json.Unmarshal(valid, &forged); err != nil {
		t.Fatal(err)
	}
	forgedClaims, err := json.Marshal(forged.Claims)
	if err != nil {
		t.Fatal(err)
	}
	forged.Signature = ed25519.Sign(delegatePrivate, append([]byte("sandbox-command-issuer-certificate:v1\x00"), forgedClaims...))
	forgedWire, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	managementReject(t, v, forgedWire, now)
	// Every flat claims field and both envelope fields are required and non-null.
	for _, field := range []string{"version", "root_key_id", "certificate_id", "role", "namespace", "authority_id", "target", "restore_epoch", "public_key", "not_before", "not_after"} {
		for _, null := range []bool{false, true} {
			t.Run(fmt.Sprintf("required-%s-null-%t", field, null), func(t *testing.T) {
				var envelope map[string]json.RawMessage
				if err := json.Unmarshal(valid, &envelope); err != nil {
					t.Fatal(err)
				}
				var flat map[string]json.RawMessage
				if err := json.Unmarshal(envelope["claims"], &flat); err != nil {
					t.Fatal(err)
				}
				if null {
					flat[field] = json.RawMessage(`null`)
				} else {
					delete(flat, field)
				}
				flatWire, err := json.Marshal(flat)
				if err != nil {
					t.Fatal(err)
				}
				envelope["claims"] = flatWire
				wire, err := json.Marshal(envelope)
				if err != nil {
					t.Fatal(err)
				}
				managementReject(t, v, wire, now)
			})
		}
	}
	for _, field := range []string{"claims", "signature"} {
		for _, null := range []bool{false, true} {
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(valid, &envelope); err != nil {
				t.Fatal(err)
			}
			if null {
				envelope[field] = json.RawMessage(`null`)
			} else {
				delete(envelope, field)
			}
			wire, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			managementReject(t, v, wire, now)
		}
	}
	mutations := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"empty", func(_ []byte) []byte { return nil }},
		{"oversize", func(w []byte) []byte { return append(w, bytes.Repeat([]byte(" "), 4097-len(w))...) }},
		{"at-limit", func(w []byte) []byte { return append(w, bytes.Repeat([]byte(" "), 4096-len(w))...) }},
		{"unknown", func(w []byte) []byte { return bytes.Replace(w, []byte(`"role":`), []byte(`"unknown":1,"role":`), 1) }},
		{"duplicate", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"role":`), []byte(`"role":"command_issuer","role":`), 1)
		}},
		{"unknown-envelope", func(w []byte) []byte { return append([]byte(`{"extra":1,`), w[1:]...) }},
		{"missing", func(w []byte) []byte { return bytes.Replace(w, []byte(`"role":"command_issuer",`), nil, 1) }},
		{"null", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"role":"command_issuer"`), []byte(`"role":null`), 1)
		}},
		{"null-claims", func(w []byte) []byte {
			var e map[string]json.RawMessage
			json.Unmarshal(w, &e)
			e["claims"] = json.RawMessage(`null`)
			out, _ := json.Marshal(e)
			return out
		}},
		{"fraction", func(w []byte) []byte { return bytes.Replace(w, []byte(`"version":1`), []byte(`"version":1.0`), 1) }},
		{"overflow", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"version":1`), []byte(`"version":4294967296`), 1)
		}},
		{"invalid-utf8", func(w []byte) []byte { return bytes.Replace(w, []byte(`command_issuer`), []byte{0xff}, 1) }},
		{"surrogate", func(w []byte) []byte { return bytes.Replace(w, []byte(`command_issuer`), []byte(`\ud800`), 1) }},
		{"offset-time", func(w []byte) []byte { return bytes.Replace(w, []byte(`Z"`), []byte(`+00:00"`), 1) }},
		{"trailing-json", func(w []byte) []byte { return append(w, []byte(`{}`)...) }},
		{"root-id", func(w []byte) []byte {
			return bytes.Replace(w, []byte(rootID), []byte(strings.Repeat("f", 64)), 1)
		}},
		{"invalid-root-hash", func(w []byte) []byte {
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(w, &envelope); err != nil {
				t.Fatal(err)
			}
			var flat map[string]json.RawMessage
			if err := json.Unmarshal(envelope["claims"], &flat); err != nil {
				t.Fatal(err)
			}
			flat["root_key_id"] = json.RawMessage(`"not-a-hash"`)
			envelope["claims"], _ = json.Marshal(flat)
			out, _ := json.Marshal(envelope)
			return out
		}},
		{"invalid-base64", func(w []byte) []byte {
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(w, &envelope); err != nil {
				t.Fatal(err)
			}
			envelope["signature"] = json.RawMessage(`"@invalid"`)
			out, _ := json.Marshal(envelope)
			return out
		}},
		{"bad-signature", func(w []byte) []byte {
			var e map[string]json.RawMessage
			json.Unmarshal(w, &e)
			e["signature"] = json.RawMessage(`"AA=="`)
			out, _ := json.Marshal(e)
			return out
		}},
		{"tamper-signature", func(w []byte) []byte {
			var e struct {
				Claims    CommandIssuerCertificateClaims `json:"claims"`
				Signature []byte                         `json:"signature"`
			}
			json.Unmarshal(w, &e)
			e.Signature[0] ^= 1
			out, _ := json.Marshal(e)
			return out
		}},
		{"tamper-claims", func(w []byte) []byte {
			return bytes.Replace(w, []byte(claims.CertificateID), []byte("5a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"), 1)
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			w := tc.mutate(append([]byte(nil), valid...))
			if tc.name == "at-limit" {
				if _, err := v.VerifyCommandIssuerCertificate(w, now); err != nil {
					t.Fatalf("4096 byte envelope rejected: %v", err)
				}
				return
			}
			managementReject(t, v, w, now)
		})
	}
	for _, clock := range []time.Time{time.Time{}, now.In(time.FixedZone("offset", 0)), now.Add(-2 * time.Hour), now.Add(2 * time.Hour)} {
		managementReject(t, v, valid, clock)
	}
	managementReject(t, nil, valid, now)
	managementReject(t, &ManagementVerifier{}, valid, now)
	// Even valid publication birth certificates must not acquire management roles.
	publication := RuntimeCertificateClaims{Version: 1, Namespace: claims.Namespace, AuthorityID: claims.AuthorityID, Target: claims.Target, RestoreEpoch: claims.RestoreEpoch, IntentID: "intent", SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, OperationID: claims.CertificateID, PayloadDigest: strings.Repeat("b", 64), Snapshot: SnapshotReference{Version: "v1", Digest: strings.Repeat("c", 64)}, ExpiresAt: claims.NotAfter, Runtime: RuntimeReference{ID: "id", UID: "uid", BootID: "boot"}, WorkspaceMode: "plain", PublicKey: claims.PublicKey, NotBefore: claims.NotBefore, NotAfter: claims.NotAfter}
	birth, err := SignRuntimeCertificate(private, publication)
	if err != nil {
		t.Fatal(err)
	}
	managementReject(t, v, birth, now)
}

func TestCommandIssuerIdentitySignRejects(t *testing.T) {
	root, private, claims, _ := managementTestClaims(t)
	cases := []struct {
		name   string
		change func(*CommandIssuerCertificateClaims)
	}{
		{"role", func(c *CommandIssuerCertificateClaims) { c.Role = "runtime_receipt" }},
		{"version", func(c *CommandIssuerCertificateClaims) { c.Version = 0 }},
		{"uuid", func(c *CommandIssuerCertificateClaims) { c.CertificateID = "bad" }},
		{"binding", func(c *CommandIssuerCertificateClaims) { c.Namespace = "bad" }},
		{"key", func(c *CommandIssuerCertificateClaims) { c.PublicKey = nil }},
		{"self-delegate", func(c *CommandIssuerCertificateClaims) { c.PublicKey = root }},
		{"interval", func(c *CommandIssuerCertificateClaims) { c.NotAfter = c.NotBefore }},
		{"nonutc", func(c *CommandIssuerCertificateClaims) { c.NotBefore = c.NotBefore.In(time.FixedZone("offset", 0)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := claims
			tc.change(&c)
			if wire, err := SignCommandIssuerCertificate(private, c); err == nil || wire != nil {
				t.Fatalf("invalid claims signed: %v", err)
			}
		})
	}
	badSuffix := append(ed25519.PrivateKey(nil), private...)
	badSuffix[63] ^= 1
	for _, key := range []ed25519.PrivateKey{nil, private[:32], append(append(ed25519.PrivateKey(nil), private...), 0), badSuffix} {
		if wire, err := SignCommandIssuerCertificate(key, claims); err == nil || wire != nil {
			t.Fatal("invalid private key signed certificate")
		}
	}
}

func TestManagementVerifierPinsTrust(t *testing.T) {
	rootA, privateA, claims, now := managementTestClaims(t)
	rootB, privateB := managementTestKey(t)
	binding := managementTestBinding(claims)
	roots := []ed25519.PublicKey{append(ed25519.PublicKey(nil), rootA...), append(ed25519.PublicKey(nil), rootB...)}
	v, err := NewManagementVerifier(binding, roots)
	if err != nil {
		t.Fatal(err)
	}
	roots[0][0] ^= 1
	roots[1] = rootA
	binding.Target = "changed"
	wireA := managementRawWire(t, privateA, claims, "sandbox-command-issuer-certificate:v1\x00")
	wireB := managementRawWire(t, privateB, claims, "sandbox-command-issuer-certificate:v1\x00")
	for _, wire := range [][]byte{wireA, wireB} {
		if _, err := v.VerifyCommandIssuerCertificate(wire, now); err != nil {
			t.Fatalf("copied overlapping trust changed: %v", err)
		}
	}
	removed, err := NewManagementVerifier(managementTestBinding(claims), []ed25519.PublicKey{rootB})
	if err != nil {
		t.Fatal(err)
	}
	managementReject(t, removed, wireA, now)
	if _, err := removed.VerifyCommandIssuerCertificate(wireB, now); err != nil {
		t.Fatal(err)
	}
	// A root may delegate to another key when it is not a root in that verifier;
	// the overlap verifier must reject that same certificate.
	delegatedRoot := claims
	delegatedRoot.PublicKey = rootB
	wire, err := SignCommandIssuerCertificate(privateA, delegatedRoot)
	if err != nil {
		t.Fatal(err)
	}
	managementReject(t, v, wire, now)
	single, err := NewManagementVerifier(managementTestBinding(claims), []ed25519.PublicKey{rootA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := single.VerifyCommandIssuerCertificate(wire, now); err != nil {
		t.Fatal(err)
	}
	for i, roots := range [][]ed25519.PublicKey{nil, {}, {rootA, rootB, rootA}, {nil}, {rootA[:31]}, {append(append(ed25519.PublicKey(nil), rootA...), 0)}} {
		t.Run(fmt.Sprintf("invalid-roots-%d", i), func(t *testing.T) {
			if v, err := NewManagementVerifier(managementTestBinding(claims), roots); err == nil || v != nil {
				t.Fatal("accepted invalid pinned roots")
			}
		})
	}
	for _, change := range []func(*TrustBinding){func(b *TrustBinding) { b.Namespace = "bad" }, func(b *TrustBinding) { b.AuthorityID = "" }, func(b *TrustBinding) { b.Target = "" }, func(b *TrustBinding) { b.RestoreEpoch = "/" }} {
		b := managementTestBinding(claims)
		change(&b)
		if v, err := NewManagementVerifier(b, []ed25519.PublicKey{rootA}); err == nil || v != nil {
			t.Fatal("accepted invalid trust binding")
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				identity, err := v.VerifyCommandIssuerCertificate(wireA, now)
				if err != nil {
					t.Errorf("concurrent verify: %v", err)
					return
				}
				key := identity.PublicKey()
				key[0] ^= 1
				out := identity.Wire()
				out[0] ^= 1
			}
		}()
	}
	wg.Wait()
}
