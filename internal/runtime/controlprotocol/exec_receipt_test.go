package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLocalExecReceiptStatesBindingsAndDomains(t *testing.T) {
	f := newActivationFixture(t)
	start := execStartSetup(t).claims
	issuer, err := f.verifier.VerifyCommandIssuerCertificate(f.issuerWire, f.now)
	if err != nil {
		t.Fatal(err)
	}
	c := start.Context
	c.Namespace = f.claims.Binding.Namespace
	c.AuthorityID = f.claims.Binding.AuthorityID
	c.Target = f.claims.Binding.Target
	c.RestoreEpoch = f.claims.Binding.RestoreEpoch
	c.SandboxID = f.claims.Identity.SandboxID
	c.WorkspaceHash = f.claims.Identity.WorkspaceHash
	c.Generation = f.claims.Identity.Generation
	c.Runtime = f.claims.Identity.Runtime
	c.IssuerCertificateID = issuer.CertificateID()
	c.IssuerCertificateDigest = issuer.Digest()
	claims := LocalExecReceiptClaims{Version: 1, State: "accepted", Context: c, DescriptorDigest: start.DescriptorDigest, TicketDigest: strings.Repeat("c", 64), NotBefore: f.now.Add(-2 * time.Second), NotAfter: f.now.Add(20 * time.Second), AuthorityDeadline: f.now.Add(20 * time.Second)}
	verify := func(w []byte, expected LocalExecReceiptClaims, now time.Time) (LocalExecReceiptEvidence, error) {
		return f.verifier.VerifyLocalExecReceipt(w, f.runtimeWire, expected.Context, expected.DescriptorDigest, expected.TicketDigest, expected.NotBefore, expected.NotAfter, expected.AuthorityDeadline, now)
	}
	for _, state := range []string{"accepted", "local_terminal", "unknown"} {
		x := claims
		x.State = state
		if state == "local_terminal" {
			x.RootPID = 42
			x.RootWaitStatus = 9
			x.DrainConfirmed = true
			x.Reason = "authority_expired"
		}
		if state == "unknown" {
			x.Reason = "monitor_lost"
		}
		w, err := SignLocalExecReceipt(f.runtimeKey, x)
		if err != nil {
			t.Fatal(err)
		}
		// Even after authority expiry the receipt only attributes a local statement.
		e, err := verify(w, x, f.now.Add(40*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if e.State() != state || e.Context() != c || e.DescriptorDigest() != x.DescriptorDigest || e.TicketDigest() != x.TicketDigest || e.RootPID() != x.RootPID || e.RootWaitStatus() != x.RootWaitStatus || e.DrainConfirmed() != x.DrainConfirmed || e.Reason() != x.Reason || e.AuthorityDeadline() != x.AuthorityDeadline {
			t.Fatal("receipt evidence mismatch")
		}
		e.Wire()[0] ^= 1
		if !bytes.Equal(w, e.Wire()) {
			t.Fatal("mutable receipt")
		}
		for _, change := range []func(*LocalExecReceiptClaims){func(x *LocalExecReceiptClaims) { x.NotBefore = x.NotBefore.Add(time.Second) }, func(x *LocalExecReceiptClaims) { x.NotAfter = x.NotAfter.Add(time.Second) }, func(x *LocalExecReceiptClaims) { x.AuthorityDeadline = x.AuthorityDeadline.Add(time.Second) }, func(x *LocalExecReceiptClaims) { x.TicketDigest = strings.Repeat("d", 64) }, func(x *LocalExecReceiptClaims) { x.DescriptorDigest = strings.Repeat("d", 64) }, func(x *LocalExecReceiptClaims) { x.Context.Runtime.BootID = x.Context.CommandID }, func(x *LocalExecReceiptClaims) { x.Context.DataGateEpoch++ }} {
			expected := x
			change(&expected)
			if _, err := verify(w, expected, f.now); err == nil {
				t.Fatal("unpinned receipt")
			}
		}
		for _, domain := range []string{execStartTicketDomain, execRenewTicketDomain, "sandbox-publication-receipt:v1\x00"} {
			b, _ := json.Marshal(x)
			bad, _ := json.Marshal(struct {
				Claims    LocalExecReceiptClaims `json:"claims"`
				Signature []byte                 `json:"signature"`
			}{x, ed25519.Sign(f.runtimeKey, append([]byte(domain), b...))})
			if _, err := verify(bad, x, f.now); err == nil {
				t.Fatal("cross-domain receipt")
			}
		}
		bad, _ := SignLocalExecReceipt(f.issuerKey, x)
		if _, err := verify(bad, x, f.now); err == nil {
			t.Fatal("wrong delegate")
		}
		if _, err := verify(w, x, f.claims.NotAfter.Add(time.Hour)); err == nil {
			t.Fatal("expired runtime cert")
		}
		for _, bad := range [][]byte{append(w, []byte("{}")...), bytes.Replace(w, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), bytes.Replace(w, []byte(`"drain_confirmed":false`), []byte(`"drain_confirmed":null`), 1)} {
			if bytes.Equal(bad, w) {
				continue
			}
			if _, err := verify(bad, x, f.now); err == nil {
				t.Fatal("strict receipt schema")
			}
		}
	}
	for _, change := range []func(*LocalExecReceiptClaims){func(x *LocalExecReceiptClaims) { x.State = "local_terminal" }, func(x *LocalExecReceiptClaims) { x.State = "unknown" }, func(x *LocalExecReceiptClaims) { x.RootPID = 42 }, func(x *LocalExecReceiptClaims) { x.DrainConfirmed = true }, func(x *LocalExecReceiptClaims) { x.AuthorityDeadline = x.NotAfter.Add(-time.Second) }, func(x *LocalExecReceiptClaims) { x.AuthorityDeadline = x.Context.ExpiresAt.Add(time.Second) }, func(x *LocalExecReceiptClaims) {
		x.State = "local_terminal"
		x.RootPID = 42
		x.DrainConfirmed = true
		x.Reason = "stopped"
		x.RootWaitStatus = 0x137f
	}} {
		x := claims
		change(&x)
		if _, err := SignLocalExecReceipt(f.runtimeKey, x); err == nil {
			t.Fatal("invalid receipt state")
		}
	}
}
