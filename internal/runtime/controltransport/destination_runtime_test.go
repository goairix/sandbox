package controltransport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"net"
	"testing"
	"time"
)

func TestDestinationOriginalRuntimeTrustCannotWiden(t *testing.T) {
	original, _, _, _, _ := destinationFixture(t)
	rogue, _, _, clock, _ := destinationFixture(t)
	rogue.Dial = func(context.Context) (net.Conn, error) { return nil, nil }
	d, err := NewDestination(rogue)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.VerifyRuntime(original.Verifier, original.Identity, clock.now); err == nil {
		t.Fatal("same tuple signed by caller's additional root widened original backend trust")
	}
	if err = d.VerifyRuntime(rogue.Verifier, rogue.Identity, clock.now); err != nil {
		t.Fatal(err)
	}
	wrong := rogue.Identity
	wrong.Runtime.UID = "wrong"
	if err = d.VerifyRuntime(rogue.Verifier, wrong, clock.now); err == nil {
		t.Fatal("wrong original identity accepted")
	}
	if err = d.VerifyRuntime(rogue.Verifier, rogue.Identity, clock.now.Add(2*time.Hour)); err == nil {
		t.Fatal("expired original runtime accepted")
	}
	root, _, _ := ed25519.GenerateKey(rand.Reader)
	wrongBinding, err := p.NewManagementVerifier(p.TrustBinding{Namespace: "/sandbox/control/other/", AuthorityID: "authority", Target: "target", RestoreEpoch: "epoch"}, []ed25519.PublicKey{root})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.VerifyRuntime(wrongBinding, rogue.Identity, clock.now); err == nil {
		t.Fatal("wrong backend binding accepted")
	}
}
