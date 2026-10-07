package controlprotocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestExecRenewRoundTripAndDomains(t *testing.T) {
	f := execStartSetup(t)
	c := ExecRenewTicketClaims(f.claims)
	c.Purpose = "operation_exec_renew"
	w, err := SignExecRenewTicket(f.delegateKey, c)
	if err != nil {
		t.Fatal(err)
	}
	e, err := f.verifier.VerifyExecRenewTicket(w, f.issuerWire, c.Context, c.DescriptorDigest, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if e.Context() != c.Context || e.DescriptorDigest() != c.DescriptorDigest || e.Digest() != wireDigest(w) || !e.NotBefore().Equal(c.NotBefore) || !e.NotAfter().Equal(c.NotAfter) {
		t.Fatal("wrong renewal evidence")
	}
	e.Wire()[0] ^= 1
	if !bytes.Equal(e.Wire(), w) {
		t.Fatal("mutable evidence")
	}
	for _, domain := range []string{testExecStartDomain, "sandbox-publication-ticket:v1\x00", "sandbox-local-exec-receipt:v1\x00"} {
		b, _ := json.Marshal(c)
		bad, _ := json.Marshal(struct {
			Claims    ExecRenewTicketClaims `json:"claims"`
			Signature []byte                `json:"signature"`
		}{c, ed25519.Sign(f.delegateKey, append([]byte(domain), b...))})
		if _, err := f.verifier.VerifyExecRenewTicket(bad, f.issuerWire, c.Context, c.DescriptorDigest, f.now); err == nil {
			t.Fatal("cross-domain renewal", domain)
		}
	}
	if _, err := f.verifier.VerifyExecStartTicket(w, f.issuerWire, c.Context, f.descriptor, f.now); err == nil {
		t.Fatal("renew used as start")
	}
	for _, change := range execStartContextMutations() {
		x := c.Context
		change.change(&x)
		if _, err := f.verifier.VerifyExecRenewTicket(w, f.issuerWire, x, c.DescriptorDigest, f.now); err == nil {
			t.Fatal(change.name)
		}
	}
	if _, err := f.verifier.VerifyExecRenewTicket(w, f.issuerWire, c.Context, strings.Repeat("c", 64), f.now); err == nil {
		t.Fatal("descriptor")
	}
	for _, now := range []time.Time{c.NotBefore, c.NotAfter.Add(-time.Second), {}} {
		if _, err := f.verifier.VerifyExecRenewTicket(w, f.issuerWire, c.Context, c.DescriptorDigest, now); err == nil {
			t.Fatal("time")
		}
	}
	for _, change := range []func(*ExecRenewTicketClaims){func(c *ExecRenewTicketClaims) { c.Purpose = "operation_exec_start" }, func(c *ExecRenewTicketClaims) { c.NotAfter = c.NotBefore.Add(31 * time.Second) }, func(c *ExecRenewTicketClaims) { c.Context.ExpiresAt = c.NotAfter.Add(-time.Second) }} {
		x := c
		change(&x)
		if _, err := SignExecRenewTicket(f.delegateKey, x); err == nil {
			t.Fatal("invalid renew signed")
		}
	}
	bad, _ := SignExecRenewTicket(f.rootKey, c)
	if _, err := f.verifier.VerifyExecRenewTicket(bad, f.issuerWire, c.Context, c.DescriptorDigest, f.now); err == nil {
		t.Fatal("wrong delegate")
	}
	for _, bad := range [][]byte{append(w, []byte("{}")...), bytes.Replace(w, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1), bytes.Replace(w, []byte(`"purpose":`), []byte(`"extra":0,"purpose":`), 1)} {
		if _, err := f.verifier.VerifyExecRenewTicket(bad, f.issuerWire, c.Context, c.DescriptorDigest, f.now); err == nil {
			t.Fatal("strict schema")
		}
	}
}
