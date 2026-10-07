package controlprotocol

import (
	"bytes"
	"testing"
)

func TestManagementTLSCredentialMatchesOnlyOwnedCertificate(t *testing.T) {
	f := newActivationFixture(t)
	input := bytes.Clone(f.issuerWire)
	c, err := NewManagementTLSCredential(f.issuerKey, input, "command_issuer")
	if err != nil {
		t.Fatal(err)
	}
	input[0] ^= 1
	if !c.MatchesCertificate(f.issuerWire) {
		t.Fatal("owned certificate mutated by caller")
	}
	for _, wire := range [][]byte{nil, {}, input, f.runtimeWire} {
		if c.MatchesCertificate(wire) {
			t.Fatal("different or empty certificate matched")
		}
	}
	var nilCredential *ManagementTLSCredential
	if nilCredential.MatchesCertificate(nil) || (&ManagementTLSCredential{}).MatchesCertificate(nil) {
		t.Fatal("missing credential matched")
	}
}
