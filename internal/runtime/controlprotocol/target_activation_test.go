package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

type activationFixture struct {
	verifier                    *ManagementVerifier
	root, runtimeKey, issuerKey ed25519.PrivateKey
	runtimeWire, issuerWire     []byte
	claims                      TargetActivationClaims
	birth                       BirthContext
	now                         time.Time
}

func newActivationFixture(t *testing.T) activationFixture {
	t.Helper()
	root, key, issuer, now := managementTestClaims(t)
	rp, rk := managementTestKey(t)
	ip, ik := managementTestKey(t)
	issuer.PublicKey = ip
	iw, err := SignCommandIssuerCertificate(key, issuer)
	if err != nil {
		t.Fatal(err)
	}
	identity := RuntimeIdentityContext{SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), Generation: 7, Runtime: RuntimeReference{ID: "pod", UID: "pod-uid", BootID: "6a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"}}
	rc := RuntimeIdentityCertificateClaims{Version: 1, CertificateID: "5a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", Role: "runtime_receipt", Namespace: issuer.Namespace, AuthorityID: issuer.AuthorityID, Target: issuer.Target, RestoreEpoch: issuer.RestoreEpoch, PublicKey: rp, NotBefore: issuer.NotBefore, NotAfter: issuer.NotAfter, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, Runtime: identity.Runtime}
	rw, err := SignRuntimeIdentityCertificate(key, rc)
	if err != nil {
		t.Fatal(err)
	}
	binding := managementTestBinding(issuer)
	v, err := NewManagementVerifier(binding, []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	birth := BirthContext{BootID: identity.Runtime.BootID, RuntimePublicKey: rp, UID: 1000, GID: 1001, NetworkAllowed: true, ContractDigest: strings.Repeat("b", 64)}
	c := TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: "7a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", Binding: binding, Identity: identity, DataGateEpoch: 1, UID: birth.UID, GID: birth.GID, NetworkAllowed: birth.NetworkAllowed, ContractDigest: birth.ContractDigest, RuntimeCertificateDigest: wireDigest(rw), IssuerCertificateDigest: wireDigest(iw), NotBefore: now.Add(-30 * time.Second), NotAfter: now.Add(30 * time.Minute)}
	return activationFixture{v, key, rk, ik, rw, iw, c, birth, now}
}
func activationRaw(t *testing.T, key ed25519.PrivateKey, c TargetActivationClaims, domain string) []byte {
	t.Helper()
	c.RootKeyID = wireDigest(key[32:])
	// Independent DTO retains the canonical field order used by the protocol.
	b, err := json.Marshal(struct {
		Version                  uint32    `json:"version"`
		Role                     string    `json:"role"`
		RootKeyID                string    `json:"root_key_id"`
		ActivationID             string    `json:"activation_id"`
		DataGateEpoch            int64     `json:"data_gate_epoch"`
		UID                      uint32    `json:"uid"`
		GID                      uint32    `json:"gid"`
		NetworkAllowed           bool      `json:"network_allowed"`
		ContractDigest           string    `json:"contract_digest"`
		RuntimeCertificateDigest string    `json:"runtime_certificate_digest"`
		IssuerCertificateDigest  string    `json:"issuer_certificate_digest"`
		NotBefore                time.Time `json:"not_before"`
		NotAfter                 time.Time `json:"not_after"`
		Binding                  any       `json:"binding"`
		Identity                 any       `json:"identity"`
	}{c.Version, c.Role, c.RootKeyID, c.ActivationID, c.DataGateEpoch, c.UID, c.GID, c.NetworkAllowed, c.ContractDigest, c.RuntimeCertificateDigest, c.IssuerCertificateDigest, c.NotBefore, c.NotAfter, struct {
		Namespace    string `json:"namespace"`
		AuthorityID  string `json:"authority_id"`
		Target       string `json:"target"`
		RestoreEpoch string `json:"restore_epoch"`
	}{c.Binding.Namespace, c.Binding.AuthorityID, c.Binding.Target, c.Binding.RestoreEpoch}, struct {
		SandboxID     string           `json:"sandbox_id"`
		WorkspaceHash string           `json:"workspace_hash"`
		Generation    int64            `json:"generation"`
		Runtime       RuntimeReference `json:"runtime"`
	}{c.Identity.SandboxID, c.Identity.WorkspaceHash, c.Identity.Generation, c.Identity.Runtime}})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(struct {
		Claims    json.RawMessage `json:"claims"`
		Signature []byte          `json:"signature"`
	}{b, ed25519.Sign(key, append([]byte(domain), b...))})
	if err != nil {
		t.Fatal(err)
	}
	return wire
}
func TestTargetActivationRoundTrip(t *testing.T) {
	f := newActivationFixture(t)
	w, err := SignTargetActivation(f.root, f.claims)
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.verifier.VerifyTargetActivation(w, f.runtimeWire, f.issuerWire, f.birth, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if e.Identity() != f.claims.Identity || e.Binding() != f.claims.Binding || e.DataGateEpoch() != 1 || e.ActivationID() != f.claims.ActivationID || e.Digest() != wireDigest(w) || !e.NotBefore().Equal(f.claims.NotBefore) || !e.NotAfter().Equal(f.claims.NotAfter) || !reflect.DeepEqual(e.Birth(), f.birth) {
		t.Fatal("activation evidence mismatch")
	}
	for _, b := range [][]byte{e.Wire(), e.RuntimeCertificate(), e.IssuerCertificate(), e.Birth().RuntimePublicKey} {
		b[0] ^= 1
	}
	w[0] ^= 1
	f.birth.RuntimePublicKey[0] ^= 1
	f.runtimeWire[0] ^= 1
	f.issuerWire[0] ^= 1
	if !bytes.Equal(e.Wire(), activationRaw(t, f.root, f.claims, "sandbox-target-activation:v1\x00")) || bytes.Equal(e.Birth().RuntimePublicKey, f.birth.RuntimePublicKey) || bytes.Equal(e.RuntimeCertificate(), f.runtimeWire) || bytes.Equal(e.IssuerCertificate(), f.issuerWire) {
		t.Fatal("mutable evidence")
	}
}
func TestTargetActivationRejects(t *testing.T) {
	f := newActivationFixture(t)
	reject := func(t *testing.T, w, r, i []byte, b BirthContext, now time.Time) {
		t.Helper()
		e, err := f.verifier.VerifyTargetActivation(w, r, i, b, now)
		if err == nil || !reflect.DeepEqual(e, TargetActivationEvidence{}) {
			t.Fatal("accepted invalid activation or returned partial evidence")
		}
	}
	valid := activationRaw(t, f.root, f.claims, "sandbox-target-activation:v1\x00")
	for name, mutate := range map[string]func(*TargetActivationClaims){
		"version": func(c *TargetActivationClaims) { c.Version = 2 }, "role": func(c *TargetActivationClaims) { c.Role = "command_issuer" }, "id": func(c *TargetActivationClaims) { c.ActivationID = "bad" }, "binding": func(c *TargetActivationClaims) { c.Binding.Target = "other" }, "boot": func(c *TargetActivationClaims) { c.Identity.Runtime.BootID = "boot" }, "uid": func(c *TargetActivationClaims) { c.UID = 0 }, "gid": func(c *TargetActivationClaims) { c.GID = 1 << 31 }, "contract": func(c *TargetActivationClaims) { c.ContractDigest = strings.Repeat("c", 64) }, "gate": func(c *TargetActivationClaims) { c.DataGateEpoch = 0 }, "runtime-digest": func(c *TargetActivationClaims) { c.RuntimeCertificateDigest = strings.Repeat("c", 64) }, "issuer-digest": func(c *TargetActivationClaims) { c.IssuerCertificateDigest = strings.Repeat("c", 64) }, "hour": func(c *TargetActivationClaims) { c.NotAfter = c.NotBefore.Add(time.Hour + time.Nanosecond) }, "wider": func(c *TargetActivationClaims) { c.NotBefore = f.now.Add(-2 * time.Minute) }, "runtime-uid": func(c *TargetActivationClaims) { c.Identity.Runtime.UID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			c := f.claims
			mutate(&c)
			reject(t, activationRaw(t, f.root, c, "sandbox-target-activation:v1\x00"), f.runtimeWire, f.issuerWire, f.birth, f.now)
		})
	}
	for name, mutate := range map[string]func(*BirthContext){"boot": func(b *BirthContext) { b.BootID = "8a1c1595-cbfd-4d2e-9ef7-f4dd3c84b172" }, "key": func(b *BirthContext) { b.RuntimePublicKey = f.issuerKey[32:] }, "uid": func(b *BirthContext) { b.UID++ }, "gid": func(b *BirthContext) { b.GID++ }, "network": func(b *BirthContext) { b.NetworkAllowed = false }, "contract": func(b *BirthContext) { b.ContractDigest = strings.Repeat("c", 64) }} {
		t.Run("birth-"+name, func(t *testing.T) { b := f.birth; mutate(&b); reject(t, valid, f.runtimeWire, f.issuerWire, b, f.now) })
	}
	for _, domain := range []string{"sandbox-exec-start-ticket:v1\x00", "sandbox-command-issuer-certificate:v1\x00", "sandbox-runtime-identity-certificate:v1\x00"} {
		reject(t, activationRaw(t, f.root, f.claims, domain), f.runtimeWire, f.issuerWire, f.birth, f.now)
	}
	for _, w := range [][]byte{append([]byte(`{"unknown":1,`), valid[1:]...), bytes.Replace(valid, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), append(valid, make([]byte, 16385)...), bytes.Replace(valid, []byte(`"network_allowed":true`), []byte(`"network_allowed":null`), 1)} {
		reject(t, w, f.runtimeWire, f.issuerWire, f.birth, f.now)
	}
	reject(t, valid, f.runtimeWire, f.issuerWire, f.birth, f.now.Add(time.Hour))
	reject(t, activationRaw(t, f.issuerKey, f.claims, "sandbox-target-activation:v1\x00"), f.runtimeWire, f.issuerWire, f.birth, f.now)
	bad := append([]byte(nil), valid...)
	bad[len(bad)-10] ^= 1
	reject(t, bad, f.runtimeWire, f.issuerWire, f.birth, f.now)
	bad = append([]byte(nil), f.runtimeWire...)
	bad[len(bad)-10] ^= 1
	reject(t, valid, bad, f.issuerWire, f.birth, f.now)
}

func TestTargetActivationIndependentDelegates(t *testing.T) {
	for _, which := range []string{"runtime", "root"} {
		t.Run(which, func(t *testing.T) {
			f := newActivationFixture(t)
			var issuer commandIssuerEnvelope
			if err := json.Unmarshal(f.issuerWire, &issuer); err != nil {
				t.Fatal(err)
			}
			if which == "runtime" {
				issuer.Claims.PublicKey = f.runtimeKey[32:]
			} else {
				issuer.Claims.PublicKey = f.root[32:]
			}
			f.issuerWire = managementRawWire(t, f.root, issuer.Claims, "sandbox-command-issuer-certificate:v1\x00")
			f.claims.IssuerCertificateDigest = wireDigest(f.issuerWire)
			w := activationRaw(t, f.root, f.claims, "sandbox-target-activation:v1\x00")
			if _, err := f.verifier.VerifyTargetActivation(w, f.runtimeWire, f.issuerWire, f.birth, f.now); err == nil {
				t.Fatal("accepted nonindependent issuer")
			}
		})
	}
}
