package launcherconfig

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
)

func TestEnrolledConfigCanonicalBoundary(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	c := Config{Version: 2, Binding: Binding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "restore"}, Roots: []ed25519.PublicKey{key}, UID: 1000, GID: 1000, ContractDigest: strings.Repeat("a", 64), ClockSocket: "/run/sandbox-clock/clock.sock", ClockPublicKey: key, ClockAudience: "launcher", MaxActive: 4, Enrollment: Enrollment{NodeUID: "22222222-2222-4222-8222-222222222222", NodeBootID: "33333333-3333-4333-8333-333333333333"}}
	wire, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(wire)
	if err != nil {
		t.Fatalf("canonical public configuration refused: %v", err)
	}
	if got.TrustBinding().AuthorityID != "authority" || got.Enrollment.NodeUID != c.Enrollment.NodeUID {
		t.Fatal("identity changed")
	}
	for _, invalid := range [][]byte{
		append(append([]byte(nil), wire...), '\n'),
		[]byte(strings.Replace(string(wire), `"version":2`, `"version":2,"version":2`, 1)),
		[]byte(strings.Replace(string(wire), `"version":2`, `"version":2,"private_key":"secret"`, 1)),
		[]byte(strings.Replace(string(wire), `"uid":1000`, `"uid":0`, 1)),
		[]byte(strings.Replace(string(wire), `"max_active":4`, `"max_active":65`, 1)),
		[]byte(strings.Replace(string(wire), `"enrollment":{`, `"enrollment":null,"enrollment":{`, 1)),
		[]byte(strings.Repeat(" ", 16385)),
	} {
		if _, err := Decode(invalid); err == nil {
			t.Fatal("unsafe public configuration accepted")
		}
	}
}
