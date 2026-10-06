package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
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

func runtimeIdentityFixture(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, RuntimeIdentityCertificateClaims, RuntimeIdentityContext, time.Time) {
	t.Helper()
	root, private, issuer, now := managementTestClaims(t)
	context := RuntimeIdentityContext{SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), Generation: 7, Runtime: RuntimeReference{ID: "pod-name", UID: "pod-uid", BootID: "boot-1"}}
	claims := RuntimeIdentityCertificateClaims{Version: 1, RootKeyID: "caller-must-not-select-root", CertificateID: issuer.CertificateID, Role: "runtime_receipt", Namespace: issuer.Namespace, AuthorityID: issuer.AuthorityID, Target: issuer.Target, RestoreEpoch: issuer.RestoreEpoch, PublicKey: issuer.PublicKey, NotBefore: issuer.NotBefore, NotAfter: issuer.NotAfter, SandboxID: context.SandboxID, WorkspaceHash: context.WorkspaceHash, Generation: context.Generation, Runtime: context.Runtime}
	return root, private, claims, context, now
}
func runtimeIdentityBinding(c RuntimeIdentityCertificateClaims) TrustBinding {
	return TrustBinding{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch}
}

// Independent test encoder bypasses signer validation and uses a literal domain.
func runtimeIdentityRawWire(t *testing.T, key ed25519.PrivateKey, c RuntimeIdentityCertificateClaims, domain string) []byte {
	t.Helper()
	sum := sha256.Sum256(key[ed25519.SeedSize:])
	c.RootKeyID = hex.EncodeToString(sum[:])
	claims, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(struct {
		Claims    RuntimeIdentityCertificateClaims `json:"claims"`
		Signature []byte                           `json:"signature"`
	}{c, ed25519.Sign(key, append([]byte(domain), claims...))})
	if err != nil {
		t.Fatal(err)
	}
	return wire
}
func runtimeIdentityReject(t *testing.T, v *ManagementVerifier, wire []byte, context RuntimeIdentityContext, now time.Time) {
	t.Helper()
	identity, err := v.VerifyRuntimeIdentityCertificate(wire, context, now)
	if err == nil {
		t.Fatal("accepted invalid runtime receipt certificate")
	}
	if !reflect.DeepEqual(identity, RuntimeReceiptIdentity{}) {
		t.Fatalf("partial identity on error: %+v", identity)
	}
}
func runtimeIdentityMutate(t *testing.T, wire []byte, path []string, value json.RawMessage, remove bool) []byte {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(wire, &object); err != nil {
		t.Fatal(err)
	}
	if len(path) == 1 {
		if remove {
			delete(object, path[0])
		} else {
			object[path[0]] = value
		}
	} else {
		object[path[0]] = runtimeIdentityMutate(t, object[path[0]], path[1:], value, remove)
	}
	out, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRuntimeReceiptIdentity(t *testing.T) {
	root, private, c, context, now := runtimeIdentityFixture(t)
	roots := []ed25519.PublicKey{append(ed25519.PublicKey(nil), root...)}
	v, err := NewManagementVerifier(runtimeIdentityBinding(c), roots)
	if err != nil {
		t.Fatal(err)
	}
	// This real root signature must authenticate before checking signer roundtrip.
	raw := runtimeIdentityRawWire(t, private, c, "sandbox-runtime-identity-certificate:v1\x00")
	if _, err := v.VerifyRuntimeIdentityCertificate(raw, context, now); err != nil {
		t.Fatalf("valid root-certified exact runtime rejected: %v", err)
	}
	wire, err := SignRuntimeIdentityCertificate(private, c)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Claims    RuntimeIdentityCertificateClaims `json:"claims"`
		Signature []byte                           `json:"signature"`
	}
	if err := json.Unmarshal(wire, &envelope); err != nil {
		t.Fatal(err)
	}
	rootHash := sha256.Sum256(root)
	if envelope.Claims.RootKeyID != hex.EncodeToString(rootHash[:]) {
		t.Fatal("root ID not derived")
	}
	serialized, err := json.Marshal(envelope.Claims)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(root, append([]byte("sandbox-runtime-identity-certificate:v1\x00"), serialized...), envelope.Signature) {
		t.Fatal("wrong runtime certificate domain or signature")
	}
	originalWire := append([]byte(nil), wire...)
	originalKey := append(ed25519.PublicKey(nil), c.PublicKey...)
	roots[0][0] ^= 1
	c.PublicKey[0] ^= 1
	private[0] ^= 1
	identity, err := v.VerifyRuntimeIdentityCertificate(wire, context, now)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Context() != context || identity.CertificateID() != c.CertificateID || !identity.NotBefore().Equal(c.NotBefore) || !identity.NotAfter().Equal(c.NotAfter) || !bytes.Equal(identity.PublicKey(), originalKey) {
		t.Fatal("authenticated attributes differ")
	}
	sum := sha256.Sum256(originalWire)
	if identity.Digest() != hex.EncodeToString(sum[:]) {
		t.Fatal("wrong evidence digest")
	}
	wire[0] ^= 1
	returnedWire := identity.Wire()
	returnedWire[0] ^= 1
	returnedKey := identity.PublicKey()
	returnedKey[0] ^= 1
	returnedContext := identity.Context()
	returnedContext.Runtime.BootID = "changed"
	if !bytes.Equal(identity.Wire(), originalWire) || !bytes.Equal(identity.PublicKey(), originalKey) || identity.Context() != context {
		t.Fatal("mutable aliases changed evidence")
	}
	pretty := new(bytes.Buffer)
	if err := json.Indent(pretty, originalWire, "", " "); err != nil {
		t.Fatal(err)
	}
	normalized, err := v.VerifyRuntimeIdentityCertificate(pretty.Bytes(), context, now)
	if err != nil || normalized.Digest() != identity.Digest() || !bytes.Equal(normalized.Wire(), identity.Wire()) {
		t.Fatalf("normalization: %v", err)
	}
	for _, at := range []time.Time{c.NotBefore.Add(time.Second), c.NotAfter.Add(-time.Second - time.Nanosecond)} {
		if _, err := v.VerifyRuntimeIdentityCertificate(originalWire, context, at); err != nil {
			t.Fatalf("valid boundary rejected: %v", err)
		}
	}
	for _, at := range []time.Time{c.NotBefore.Add(time.Second - time.Nanosecond), c.NotAfter.Add(-time.Second)} {
		runtimeIdentityReject(t, v, originalWire, context, at)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				e, err := v.VerifyRuntimeIdentityCertificate(originalWire, context, now)
				if err != nil {
					t.Errorf("concurrent verification: %v", err)
					return
				}
				e.PublicKey()[0] ^= 1
				e.Wire()[0] ^= 1
			}
		}()
	}
	wg.Wait()
}

func TestRuntimeReceiptIdentityBusinessExpiry(t *testing.T) {
	root, private, c, context, now := runtimeIdentityFixture(t)
	binding := runtimeIdentityBinding(c)
	v, err := NewManagementVerifier(binding, []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	// Both proofs name the same incarnation. Its business TTL has expired, while
	// the independent ongoing identity's interval remains valid.
	businessExpiry := now.Add(-10 * time.Second)
	birth := RuntimeCertificateClaims{Version: 1, Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, IntentID: "intent", SandboxID: context.SandboxID, WorkspaceHash: context.WorkspaceHash, Generation: context.Generation, OperationID: c.CertificateID, PayloadDigest: strings.Repeat("b", 64), Snapshot: SnapshotReference{Version: "v1", Digest: strings.Repeat("c", 64)}, ExpiresAt: businessExpiry, Runtime: context.Runtime, WorkspaceMode: "plain", PublicKey: c.PublicKey, NotBefore: c.NotBefore, NotAfter: businessExpiry}
	birthWire, err := SignRuntimeCertificate(private, birth)
	if err != nil {
		t.Fatal(err)
	}
	publication, err := NewPublicationVerifier(binding, []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	if evidence, err := publication.VerifyCertificate(birthWire, certificateContext(birth), now); err == nil || !reflect.DeepEqual(evidence, CertificateEvidence{}) {
		t.Fatal("expired birth certificate granted fresh evidence")
	}
	runtimeIdentityReject(t, v, birthWire, context, now)
	ongoing, err := SignRuntimeIdentityCertificate(private, c)
	if err != nil {
		t.Fatal(err)
	}
	if evidence, err := v.VerifyRuntimeIdentityCertificate(ongoing, context, now); err != nil || evidence.Context() != context {
		t.Fatalf("business expiry blocked ongoing identity: %v", err)
	}
	birth.NotAfter = c.NotAfter
	if wire, err := SignRuntimeCertificate(private, birth); err == nil || wire != nil {
		t.Fatal("birth certificate escaped business expiry")
	}
}

func TestRuntimeReceiptIdentityRejects(t *testing.T) {
	root, private, c, context, now := runtimeIdentityFixture(t)
	v, err := NewManagementVerifier(runtimeIdentityBinding(c), []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	valid := runtimeIdentityRawWire(t, private, c, "sandbox-runtime-identity-certificate:v1\x00")
	changes := []struct {
		name   string
		change func(*RuntimeIdentityCertificateClaims)
	}{
		{"version", func(c *RuntimeIdentityCertificateClaims) { c.Version = 2 }},
		{"role", func(c *RuntimeIdentityCertificateClaims) { c.Role = "command_issuer" }},
		{"uuid-nil", func(c *RuntimeIdentityCertificateClaims) { c.CertificateID = "00000000-0000-0000-0000-000000000000" }},
		{"uuid-canonical", func(c *RuntimeIdentityCertificateClaims) { c.CertificateID = strings.ToUpper(c.CertificateID) }},
		{"namespace", func(c *RuntimeIdentityCertificateClaims) { c.Namespace = "/sandbox/control/other/" }},
		{"authority", func(c *RuntimeIdentityCertificateClaims) { c.AuthorityID = "other" }},
		{"target", func(c *RuntimeIdentityCertificateClaims) { c.Target = "other" }},
		{"restore", func(c *RuntimeIdentityCertificateClaims) { c.RestoreEpoch = "restore-2" }},
		{"sandbox", func(c *RuntimeIdentityCertificateClaims) { c.SandboxID = "other" }},
		{"workspace", func(c *RuntimeIdentityCertificateClaims) { c.WorkspaceHash = strings.Repeat("b", 64) }},
		{"generation", func(c *RuntimeIdentityCertificateClaims) { c.Generation++ }},
		{"runtime-id", func(c *RuntimeIdentityCertificateClaims) { c.Runtime.ID = "other" }},
		{"runtime-uid", func(c *RuntimeIdentityCertificateClaims) { c.Runtime.UID = "other" }},
		{"runtime-boot-id", func(c *RuntimeIdentityCertificateClaims) { c.Runtime.BootID = "boot-2" }},
		{"short-key", func(c *RuntimeIdentityCertificateClaims) { c.PublicKey = c.PublicKey[:31] }},
		{"long-key", func(c *RuntimeIdentityCertificateClaims) { c.PublicKey = append(c.PublicKey, 0) }},
		{"self-delegate", func(c *RuntimeIdentityCertificateClaims) { c.PublicKey = root }},
		{"equal-time", func(c *RuntimeIdentityCertificateClaims) { c.NotAfter = c.NotBefore }},
		{"reverse-time", func(c *RuntimeIdentityCertificateClaims) { c.NotAfter = c.NotBefore.Add(-time.Second) }},
		{"zero-before", func(c *RuntimeIdentityCertificateClaims) { c.NotBefore = time.Time{} }},
		{"zero-after", func(c *RuntimeIdentityCertificateClaims) { c.NotAfter = time.Time{} }},
	}
	for _, tc := range changes {
		t.Run(tc.name, func(t *testing.T) {
			changed := c
			changed.PublicKey = append([]byte(nil), c.PublicKey...)
			tc.change(&changed)
			runtimeIdentityReject(t, v, runtimeIdentityRawWire(t, private, changed, "sandbox-runtime-identity-certificate:v1\x00"), context, now)
		})
	}
	// Expected context is an independent trusted value, never adopted from proof.
	for _, tc := range changes[8:14] {
		t.Run("expected-"+tc.name, func(t *testing.T) {
			changed := c
			tc.change(&changed)
			expected := RuntimeIdentityContext{SandboxID: changed.SandboxID, WorkspaceHash: changed.WorkspaceHash, Generation: changed.Generation, Runtime: changed.Runtime}
			runtimeIdentityReject(t, v, valid, expected, now)
		})
	}
	for _, bad := range []RuntimeIdentityContext{{}, {SandboxID: "..", WorkspaceHash: context.WorkspaceHash, Generation: 1, Runtime: context.Runtime}, {SandboxID: context.SandboxID, WorkspaceHash: strings.Repeat("A", 64), Generation: 1, Runtime: context.Runtime}, {SandboxID: context.SandboxID, WorkspaceHash: context.WorkspaceHash, Generation: 0, Runtime: context.Runtime}, {SandboxID: context.SandboxID, WorkspaceHash: context.WorkspaceHash, Generation: 1, Runtime: RuntimeReference{ID: "id", UID: "uid", BootID: ""}}} {
		runtimeIdentityReject(t, v, valid, bad, now)
	}
	for _, domain := range []string{"sandbox-command-issuer-certificate:v1\x00", "sandbox-runtime-certificate:v1\x00", "sandbox-runtime-ready:v1\x00", "sandbox-runtime-identity-certificate:v1\\x00", "sandbox-runtime-identity-certificate:v1"} {
		t.Run("domain-"+domain, func(t *testing.T) {
			runtimeIdentityReject(t, v, runtimeIdentityRawWire(t, private, c, domain), context, now)
		})
	}
	paths := [][]string{{"claims"}, {"signature"}}
	for _, field := range []string{"version", "root_key_id", "certificate_id", "role", "namespace", "authority_id", "target", "restore_epoch", "public_key", "not_before", "not_after", "sandbox_id", "workspace_hash", "generation", "runtime"} {
		paths = append(paths, []string{"claims", field})
	}
	for _, field := range []string{"id", "uid", "boot_id"} {
		paths = append(paths, []string{"claims", "runtime", field})
	}
	for _, path := range paths {
		for _, remove := range []bool{true, false} {
			t.Run(fmt.Sprintf("required-%s-remove-%t", strings.Join(path, "/"), remove), func(t *testing.T) {
				runtimeIdentityReject(t, v, runtimeIdentityMutate(t, valid, path, json.RawMessage(`null`), remove), context, now)
			})
		}
	}
	mutations := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"empty", func(w []byte) []byte { return nil }},
		{"oversize", func(w []byte) []byte { return append(w, bytes.Repeat([]byte(" "), 4097-len(w))...) }},
		{"unknown-envelope", func(w []byte) []byte { return append([]byte(`{"extra":1,`), w[1:]...) }},
		{"unknown-claims", func(w []byte) []byte {
			return runtimeIdentityMutate(t, w, []string{"claims", "extra"}, json.RawMessage(`1`), false)
		}},
		{"duplicate", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"role":`), []byte(`"role":"runtime_receipt","role":`), 1)
		}},
		{"duplicate-runtime", func(w []byte) []byte {
			return bytes.Replace(w, []byte(`"boot_id":`), []byte(`"boot_id":"boot-1","boot_id":`), 1)
		}},
		{"fraction-version", func(w []byte) []byte {
			return runtimeIdentityMutate(t, w, []string{"claims", "version"}, json.RawMessage(`1.0`), false)
		}},
		{"overflow-version", func(w []byte) []byte {
			return runtimeIdentityMutate(t, w, []string{"claims", "version"}, json.RawMessage(`4294967296`), false)
		}},
		{"fraction-generation", func(w []byte) []byte {
			return runtimeIdentityMutate(t, w, []string{"claims", "generation"}, json.RawMessage(`7.0`), false)
		}},
		{"overflow-generation", func(w []byte) []byte {
			return runtimeIdentityMutate(t, w, []string{"claims", "generation"}, json.RawMessage(`9223372036854775808`), false)
		}},
		{"invalid-utf8", func(w []byte) []byte { return bytes.Replace(w, []byte("boot-1"), []byte{255}, 1) }},
		{"surrogate", func(w []byte) []byte { return bytes.Replace(w, []byte("boot-1"), []byte(`\ud800`), 1) }},
		{"offset-time", func(w []byte) []byte { return bytes.Replace(w, []byte(`Z"`), []byte(`+00:00"`), 1) }},
		{"trailing-json", func(w []byte) []byte { return append(w, []byte(`{}`)...) }},
		{"root-id", func(w []byte) []byte {
			return runtimeIdentityMutate(t, w, []string{"claims", "root_key_id"}, json.RawMessage(`"`+strings.Repeat("f", 64)+`"`), false)
		}},
		{"invalid-root-hash", func(w []byte) []byte {
			return runtimeIdentityMutate(t, w, []string{"claims", "root_key_id"}, json.RawMessage(`"bad"`), false)
		}},
		{"invalid-base64", func(w []byte) []byte {
			return runtimeIdentityMutate(t, w, []string{"signature"}, json.RawMessage(`"@invalid"`), false)
		}},
		{"short-signature", func(w []byte) []byte {
			return runtimeIdentityMutate(t, w, []string{"signature"}, json.RawMessage(`"AA=="`), false)
		}},
		{"tamper-signature", func(w []byte) []byte {
			var e struct {
				Claims    RuntimeIdentityCertificateClaims `json:"claims"`
				Signature []byte                           `json:"signature"`
			}
			if err := json.Unmarshal(w, &e); err != nil {
				t.Fatal(err)
			}
			e.Signature[0] ^= 1
			out, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			return out
		}},
		{"tamper-certificate-id", func(w []byte) []byte {
			return bytes.Replace(w, []byte(c.CertificateID), []byte("5a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"), 1)
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			runtimeIdentityReject(t, v, tc.mutate(append([]byte(nil), valid...)), context, now)
		})
	}
	atLimit := append(append([]byte(nil), valid...), bytes.Repeat([]byte(" "), 4096-len(valid))...)
	if _, err := v.VerifyRuntimeIdentityCertificate(atLimit, context, now); err != nil {
		t.Fatalf("4096 byte wire rejected: %v", err)
	}
	for _, clock := range []time.Time{time.Time{}, now.In(time.FixedZone("offset", 0)), now.Add(-2 * time.Hour), now.Add(2 * time.Hour)} {
		runtimeIdentityReject(t, v, valid, context, clock)
	}
	runtimeIdentityReject(t, nil, valid, context, now)
	runtimeIdentityReject(t, &ManagementVerifier{}, valid, context, now)
}

func TestRuntimeReceiptIdentitySignRejects(t *testing.T) {
	root, private, c, _, _ := runtimeIdentityFixture(t)
	invalid := []struct {
		name   string
		change func(*RuntimeIdentityCertificateClaims)
	}{
		{"role", func(c *RuntimeIdentityCertificateClaims) { c.Role = "command_issuer" }},
		{"version", func(c *RuntimeIdentityCertificateClaims) { c.Version = 0 }},
		{"uuid", func(c *RuntimeIdentityCertificateClaims) { c.CertificateID = "bad" }},
		{"namespace", func(c *RuntimeIdentityCertificateClaims) { c.Namespace = "bad" }},
		{"namespace-length", func(c *RuntimeIdentityCertificateClaims) {
			c.Namespace = "/" + strings.Repeat("a", 500) + "/control/authority/"
		}},
		{"authority-control", func(c *RuntimeIdentityCertificateClaims) { c.AuthorityID = "a\n" }},
		{"target-length", func(c *RuntimeIdentityCertificateClaims) { c.Target = strings.Repeat("a", 129) }},
		{"restore-segment", func(c *RuntimeIdentityCertificateClaims) { c.RestoreEpoch = ".." }},
		{"sandbox-invalid", func(c *RuntimeIdentityCertificateClaims) { c.SandboxID = "a/b" }},
		{"sandbox-length", func(c *RuntimeIdentityCertificateClaims) { c.SandboxID = strings.Repeat("a", 129) }},
		{"hash", func(c *RuntimeIdentityCertificateClaims) { c.WorkspaceHash = strings.Repeat("A", 64) }},
		{"generation", func(c *RuntimeIdentityCertificateClaims) { c.Generation = 0 }},
		{"runtime-empty-id", func(c *RuntimeIdentityCertificateClaims) { c.Runtime.ID = "" }},
		{"runtime-invalid-uid", func(c *RuntimeIdentityCertificateClaims) { c.Runtime.UID = string([]byte{255}) }},
		{"runtime-control-boot", func(c *RuntimeIdentityCertificateClaims) { c.Runtime.BootID = "a\n" }},
		{"runtime-length", func(c *RuntimeIdentityCertificateClaims) { c.Runtime.BootID = strings.Repeat("a", 129) }},
		{"key", func(c *RuntimeIdentityCertificateClaims) { c.PublicKey = nil }},
		{"self-delegate", func(c *RuntimeIdentityCertificateClaims) { c.PublicKey = root }},
		{"interval", func(c *RuntimeIdentityCertificateClaims) { c.NotAfter = c.NotBefore }},
		{"nonutc", func(c *RuntimeIdentityCertificateClaims) {
			c.NotBefore = c.NotBefore.In(time.FixedZone("offset", 3600))
		}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			changed := c
			tc.change(&changed)
			if wire, err := SignRuntimeIdentityCertificate(private, changed); err == nil || wire != nil {
				t.Fatal("signed invalid runtime claims")
			}
			runtimeIdentityReject(t, mustRuntimeVerifier(t, root, c), runtimeIdentityRawWire(t, private, changed, "sandbox-runtime-identity-certificate:v1\x00"), RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}, c.NotBefore.Add(time.Minute))
		})
	}
	badSuffix := append(ed25519.PrivateKey(nil), private...)
	badSuffix[63] ^= 1
	for _, key := range []ed25519.PrivateKey{nil, private[:32], append(append(ed25519.PrivateKey(nil), private...), 0), badSuffix} {
		if wire, err := SignRuntimeIdentityCertificate(key, c); err == nil || wire != nil {
			t.Fatal("signed invalid private key")
		}
	}
}
func mustRuntimeVerifier(t *testing.T, root ed25519.PublicKey, c RuntimeIdentityCertificateClaims) *ManagementVerifier {
	t.Helper()
	v, err := NewManagementVerifier(runtimeIdentityBinding(c), []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestManagementIdentityRoleSeparation(t *testing.T) {
	root, private, c, context, now := runtimeIdentityFixture(t)
	v := mustRuntimeVerifier(t, root, c)
	issuer := CommandIssuerCertificateClaims{Version: 1, CertificateID: c.CertificateID, Role: "command_issuer", Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, NotBefore: c.NotBefore, NotAfter: c.NotAfter}
	issuerKey, privateIssuer := managementTestKey(t)
	issuer.PublicKey = issuerKey
	issuerWire, err := SignCommandIssuerCertificate(private, issuer)
	if err != nil {
		t.Fatal(err)
	}
	runtimeWire, err := SignRuntimeIdentityCertificate(private, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.VerifyCommandIssuerCertificate(issuerWire, now); err != nil {
		t.Fatal(err)
	}
	if _, err := v.VerifyRuntimeIdentityCertificate(runtimeWire, context, now); err != nil {
		t.Fatal(err)
	}
	managementReject(t, v, runtimeWire, now)
	runtimeIdentityReject(t, v, issuerWire, context, now)
	// A valid issuer delegate's signature cannot acquire root authority.
	delegated, err := SignRuntimeIdentityCertificate(privateIssuer, c)
	if err != nil {
		t.Fatal(err)
	}
	runtimeIdentityReject(t, v, delegated, context, now)
	// Even when it lies about a known root ID, the fixed-domain signature fails.
	var forged struct {
		Claims    RuntimeIdentityCertificateClaims `json:"claims"`
		Signature []byte                           `json:"signature"`
	}
	if err := json.Unmarshal(runtimeWire, &forged); err != nil {
		t.Fatal(err)
	}
	serialized, err := json.Marshal(forged.Claims)
	if err != nil {
		t.Fatal(err)
	}
	forged.Signature = ed25519.Sign(privateIssuer, append([]byte("sandbox-runtime-identity-certificate:v1\x00"), serialized...))
	forgedWire, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	runtimeIdentityReject(t, v, forgedWire, context, now)
	rootB, privateB := managementTestKey(t)
	overlap, err := NewManagementVerifier(runtimeIdentityBinding(c), []ed25519.PublicKey{root, rootB})
	if err != nil {
		t.Fatal(err)
	}
	wireB, err := SignRuntimeIdentityCertificate(privateB, c)
	if err != nil {
		t.Fatal(err)
	}
	for _, wire := range [][]byte{runtimeWire, wireB} {
		if _, err := overlap.VerifyRuntimeIdentityCertificate(wire, context, now); err != nil {
			t.Fatal(err)
		}
	}
	removed := mustRuntimeVerifier(t, rootB, c)
	runtimeIdentityReject(t, removed, runtimeWire, context, now)
	if _, err := removed.VerifyRuntimeIdentityCertificate(wireB, context, now); err != nil {
		t.Fatal(err)
	}
	delegatedRoot := c
	delegatedRoot.PublicKey = rootB
	rootDelegateWire, err := SignRuntimeIdentityCertificate(private, delegatedRoot)
	if err != nil {
		t.Fatal(err)
	}
	runtimeIdentityReject(t, overlap, rootDelegateWire, context, now)
	if _, err := v.VerifyRuntimeIdentityCertificate(rootDelegateWire, context, now); err != nil {
		t.Fatal(err)
	}
	restoredBinding := runtimeIdentityBinding(c)
	restoredBinding.RestoreEpoch = "restore-2"
	restored, err := NewManagementVerifier(restoredBinding, []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	runtimeIdentityReject(t, restored, runtimeWire, context, now)
	samePodNewBoot := context
	samePodNewBoot.Runtime.BootID = "boot-2"
	runtimeIdentityReject(t, v, runtimeWire, samePodNewBoot, now)
}
