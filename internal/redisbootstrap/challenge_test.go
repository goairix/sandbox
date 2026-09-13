package redisbootstrap

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"filippo.io/edwards25519"
)

func proofFixture(t *testing.T, purpose ProofPurpose) (ed25519.PublicKey, ed25519.PrivateKey, ClusterState, Member, IdentityChallenge, LocalObservation) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := testCluster()
	m := testMember(c, 0)
	id := testIdentity(c, 0)
	p := PersistedState{Member: m, Role: Primary, PrimaryDNS: m.DNS, SentinelEpoch: 4}
	o := LocalObservation{Volume: VolumeState{Identity: &id, Persisted: &p}, Snapshot: &PersistentConfigSnapshot{State: p, SentinelID: strings.Repeat("a", 40), CurrentEpoch: 5}, ConfigDigest: strings.Repeat("b", 64)}
	if purpose == LiveProof {
		o.RunID = strings.Repeat("c", 40)
	}
	ch, err := NewIdentityChallenge(purpose)
	if err != nil {
		t.Fatal(err)
	}
	return pub, key, c, m, ch, o
}

func TestIdentityProofRoundTrip(t *testing.T) {
	for _, purpose := range []ProofPurpose{InventoryProof, LiveProof} {
		t.Run(string(purpose), func(t *testing.T) {
			pub, key, c, m, ch, o := proofFixture(t, purpose)
			session, err := NewProofSession()
			if err != nil {
				t.Fatal(err)
			}
			proof, err := SignIdentityProof(key, c, m, session, ch, o)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := &AuthenticatedEndpoint{Member: m, RunID: o.RunID, Authenticated: true}
			if err := VerifyIdentityProof(pub, c, m, ch, proof, o.Volume.Identity, endpoint); err != nil {
				t.Fatal(err)
			}
			if len(proof.Signature) != 128 {
				t.Fatal("missing Ed25519 signature")
			}
		})
	}
}

func TestIdentityProofBindings(t *testing.T) {
	mutations := map[string]func(*IdentityProof){
		"version": func(p *IdentityProof) { p.Version++ }, "purpose": func(p *IdentityProof) { p.Purpose = InventoryProof }, "nonce": func(p *IdentityProof) { p.Nonce = strings.Repeat("0", 64) }, "session": func(p *IdentityProof) { p.Session = strings.Repeat("0", 64) }, "cluster": func(p *IdentityProof) { p.ClusterID = "other" }, "member": func(p *IdentityProof) { p.Member.Ordinal = 1 }, "marker": func(p *IdentityProof) { p.Observation.Volume.Identity.MarkerID = strings.Repeat("0", 32) }, "digest": func(p *IdentityProof) { p.Observation.ConfigDigest = strings.Repeat("0", 64) }, "epoch": func(p *IdentityProof) { p.Observation.Snapshot.CurrentEpoch++ }, "role": func(p *IdentityProof) { p.Observation.Volume.Persisted.Role = Replica }, "runid": func(p *IdentityProof) { p.Observation.RunID = strings.Repeat("0", 40) }, "signature": func(p *IdentityProof) { p.Signature = strings.Repeat("0", 128) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			pub, key, c, m, ch, o := proofFixture(t, LiveProof)
			session, _ := NewProofSession()
			p, err := SignIdentityProof(key, c, m, session, ch, o)
			if err != nil {
				t.Fatal(err)
			}
			expected := *o.Volume.Identity
			mutate(&p)
			if VerifyIdentityProof(pub, c, m, ch, p, &expected, &AuthenticatedEndpoint{Member: m, RunID: strings.Repeat("c", 40), Authenticated: true}) == nil {
				t.Fatal("accepted tampering")
			}
		})
	}
	pub, key, c, m, ch, o := proofFixture(t, LiveProof)
	session, _ := NewProofSession()
	p, _ := SignIdentityProof(key, c, m, session, ch, o)
	for name, current := range map[string]*AuthenticatedEndpoint{"nil": nil, "unauthenticated": {Member: m, RunID: o.RunID}, "old process": {Member: m, RunID: strings.Repeat("d", 40), Authenticated: true}, "wrong member": {Member: testMember(c, 1), RunID: o.RunID, Authenticated: true}} {
		t.Run(name, func(t *testing.T) {
			if VerifyIdentityProof(pub, c, m, ch, p, o.Volume.Identity, current) == nil {
				t.Fatal("accepted unbound endpoint")
			}
		})
	}
	if VerifyIdentityProof(pub, c, m, ch, p, nil, &AuthenticatedEndpoint{Member: m, RunID: o.RunID, Authenticated: true}) == nil {
		t.Fatal("accepted missing expected marker")
	}
	next, _ := NewIdentityChallenge(LiveProof)
	if VerifyIdentityProof(pub, c, m, next, p, o.Volume.Identity, &AuthenticatedEndpoint{Member: m, RunID: o.RunID, Authenticated: true}) == nil {
		t.Fatal("accepted stale nonce")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if VerifyIdentityProof(other, c, m, ch, p, o.Volume.Identity, &AuthenticatedEndpoint{Member: m, RunID: o.RunID, Authenticated: true}) == nil {
		t.Fatal("accepted wrong key")
	}
	expected := *o.Volume.Identity
	expected.MarkerID = strings.Repeat("d", 32)
	if VerifyIdentityProof(pub, c, m, ch, p, &expected, &AuthenticatedEndpoint{Member: m, RunID: o.RunID, Authenticated: true}) == nil {
		t.Fatal("accepted old marker")
	}
}

func TestIdentityProofObservations(t *testing.T) {
	bad := map[string]func(*LocalObservation){"missing identity": func(o *LocalObservation) { o.Volume.Identity = nil }, "empty conflict": func(o *LocalObservation) { o.Volume.Empty = true }, "reserved conflict": func(o *LocalObservation) { o.Volume.Identity.InitialConfig = Reserved }, "missing snapshot": func(o *LocalObservation) { o.Snapshot = nil }, "missing persisted": func(o *LocalObservation) { o.Volume.Persisted = nil }, "snapshot mismatch": func(o *LocalObservation) { o.Snapshot.State.SentinelEpoch++ }, "epoch regression": func(o *LocalObservation) { o.Snapshot.CurrentEpoch = 0 }, "invalid id": func(o *LocalObservation) { o.Snapshot.SentinelID = "bad" }, "invalid digest": func(o *LocalObservation) { o.ConfigDigest = "bad" }, "self replica": func(o *LocalObservation) { o.Volume.Persisted.Role = Replica; o.Snapshot.State.Role = Replica }, "wrong primary": func(o *LocalObservation) {
		o.Volume.Persisted.PrimaryDNS = "other"
		o.Snapshot.State.PrimaryDNS = "other"
	}, "wrong local": func(o *LocalObservation) { o.Volume.Identity.Member.Ordinal = 1 }, "invalid state": func(o *LocalObservation) { o.Volume.Identity.InitialConfig = "bad" }, "invalid role": func(o *LocalObservation) { o.Volume.Persisted.Role = "bad"; o.Snapshot.State.Role = "bad" }, "inventory runid": func(o *LocalObservation) { o.RunID = strings.Repeat("c", 40) }}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			_, key, c, m, ch, o := proofFixture(t, InventoryProof)
			mutate(&o)
			session, _ := NewProofSession()
			if _, err := SignIdentityProof(key, c, m, session, ch, o); err == nil {
				t.Fatal("signed invalid observation")
			}
		})
	}
	pub, key, c, m, ch, o := proofFixture(t, InventoryProof)
	session, _ := NewProofSession()
	for _, volume := range []VolumeState{{Empty: true}, {Identity: &VolumeIdentity{ClusterID: c.ClusterID, Member: m, MarkerID: o.Volume.Identity.MarkerID, InitialConfig: Reserved}}} {
		empty := LocalObservation{Volume: volume}
		proof, err := SignIdentityProof(key, c, m, session, ch, empty)
		if err != nil {
			t.Fatal(err)
		}
		if VerifyIdentityProof(pub, c, m, ch, proof, nil, nil) != nil {
			t.Fatal("rejected first registration")
		}
		if volume.Identity == nil && VerifyIdentityProof(pub, c, m, ch, proof, o.Volume.Identity, nil) == nil {
			t.Fatal("accepted empty existing marker")
		}
	}
	reserved := *o.Volume.Identity
	reserved.InitialConfig = Reserved
	p, err := SignIdentityProof(key, c, m, session, ch, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyIdentityProof(pub, c, m, ch, p, &reserved, nil); err != nil {
		t.Fatal("rejected Reserved -> Configured marker", err)
	}
	live, _ := NewIdentityChallenge(LiveProof)
	if _, err := SignIdentityProof(key, c, m, session, live, LocalObservation{Volume: VolumeState{Empty: true}}); err == nil {
		t.Fatal("signed empty live")
	}
	for _, invalid := range []IdentityChallenge{{Version: 2, Purpose: InventoryProof, Nonce: strings.Repeat("a", 64)}, {Version: 1, Purpose: "bad", Nonce: strings.Repeat("a", 64)}, {Version: 1, Purpose: InventoryProof, Nonce: strings.Repeat("A", 64)}} {
		if _, err := SignIdentityProof(key, c, m, session, invalid, o); err == nil {
			t.Fatal("signed invalid challenge")
		}
	}
	if _, err := SignIdentityProof(nil, c, m, session, ch, o); err == nil {
		t.Fatal("accepted invalid private key")
	}
	if _, err := SignIdentityProof(key, c, m, "bad", ch, o); err == nil {
		t.Fatal("accepted invalid session")
	}
}

func TestIdentityProofPublicKeySet(t *testing.T) {
	var keys [3]ed25519.PublicKey
	for i := range keys {
		keys[i], _, _ = ed25519.GenerateKey(rand.Reader)
	}
	digest, err := PublicKeySetDigest(keys)
	if err != nil || len(digest) != 64 {
		t.Fatal("invalid key set digest", err)
	}
	reordered := keys
	reordered[0], reordered[1] = keys[1], keys[0]
	other, _ := PublicKeySetDigest(reordered)
	if other == digest {
		t.Fatal("digest ignored member order")
	}
	for _, bad := range []ed25519.PublicKey{nil, make([]byte, 32), keys[1]} {
		invalid := keys
		invalid[0] = bad
		if _, err := PublicKeySetDigest(invalid); err == nil {
			t.Fatal("accepted malformed/duplicate key")
		}
	}
	if _, err := NewIdentityChallenge("invalid"); err == nil {
		t.Fatal("accepted purpose")
	}
	a, _ := NewProofSession()
	b, _ := NewProofSession()
	if len(a) != 64 || a == b {
		t.Fatal("sessions not random")
	}
}

func TestIdentityProofCanonicalDomainAndConfiguredLive(t *testing.T) {
	pub, key, c, m, ch, o := proofFixture(t, LiveProof)
	session, _ := NewProofSession()
	proof, err := SignIdentityProof(key, c, m, session, ch, o)
	if err != nil {
		t.Fatal(err)
	}
	signature, _ := hex.DecodeString(proof.Signature)
	unsigned := proof
	unsigned.Signature = ""
	data, _ := json.Marshal(unsigned)
	if !ed25519.Verify(pub, append([]byte("sandbox/redisbootstrap/identity-proof/v1\x00"), data...), signature) || ed25519.Verify(pub, data, signature) {
		t.Fatal("wrong canonical signature/domain separation")
	}
	o.RunID = ""
	if _, err := SignIdentityProof(key, c, m, session, ch, o); err == nil {
		t.Fatal("signed missing live runid")
	}
	reserved := *o.Volume.Identity
	reserved.InitialConfig = Reserved
	o = LocalObservation{Volume: VolumeState{Identity: &reserved}, ConfigDigest: strings.Repeat("a", 64)}
	inventory, _ := NewIdentityChallenge(InventoryProof)
	if _, err := SignIdentityProof(key, c, m, session, inventory, o); err == nil {
		t.Fatal("signed reserved digest")
	}
	o = LocalObservation{Volume: VolumeState{Empty: true}, Snapshot: &PersistentConfigSnapshot{}}
	if _, err := SignIdentityProof(key, c, m, session, inventory, o); err == nil {
		t.Fatal("signed empty snapshot")
	}
}

func TestIdentityProofRejectIdentityKeyForgery(t *testing.T) {
	_, key, c, m, ch, o := proofFixture(t, LiveProof)
	session, _ := NewProofSession()
	proof, err := SignIdentityProof(key, c, m, session, ch, o)
	if err != nil {
		t.Fatal(err)
	}
	identityKey := ed25519.PublicKey(edwards25519.NewIdentityPoint().Bytes())
	forgedSignature := make([]byte, ed25519.SignatureSize)
	copy(forgedSignature, identityKey) // R = identity, S = 0, without private key.
	proof.Signature = hex.EncodeToString(forgedSignature)
	if !ed25519.Verify(identityKey, proofSigningBytes(proof), forgedSignature) {
		t.Fatal("fixture does not reproduce Go Ed25519 identity-key forgery")
	}
	if VerifyIdentityProof(identityKey, c, m, ch, proof, o.Volume.Identity, &AuthenticatedEndpoint{Member: m, RunID: o.RunID, Authenticated: true}) == nil {
		t.Error("accepted actual signature forgery with identity public key")
	}
	var keys [3]ed25519.PublicKey
	for i := range keys {
		keys[i], _, _ = ed25519.GenerateKey(rand.Reader)
	}
	keys[0] = identityKey
	if _, err := PublicKeySetDigest(keys); err == nil {
		t.Error("registered identity public key")
	}
}

func TestIdentityProofRejectInvalidCurveKeys(t *testing.T) {
	orderTwoBytes, _ := hex.DecodeString("ecffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f")
	torsion, err := new(edwards25519.Point).SetBytes(orderTwoBytes)
	if err != nil {
		t.Fatal(err)
	}
	if new(edwards25519.Point).MultByCofactor(torsion).Equal(edwards25519.NewIdentityPoint()) != 1 {
		t.Fatal("fixture is not torsion")
	}
	mixed := new(edwards25519.Point).Add(edwards25519.NewGeneratorPoint(), torsion)
	malformed, _ := hex.DecodeString("efffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f")
	noncanonical, _ := hex.DecodeString("eeffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff7f")
	negativeIdentity := edwards25519.NewIdentityPoint().Bytes()
	negativeIdentity[31] |= 0x80
	for name, raw := range map[string][]byte{"malformed": malformed, "noncanonical y": noncanonical, "negative identity": negativeIdentity, "small order": orderTwoBytes, "mixed torsion": mixed.Bytes()} {
		t.Run(name, func(t *testing.T) {
			var keys [3]ed25519.PublicKey
			for i := range keys {
				keys[i], _, _ = ed25519.GenerateKey(rand.Reader)
			}
			keys[0] = raw
			if _, err := PublicKeySetDigest(keys); err == nil {
				t.Fatal("registered malformed/non-prime-subgroup public key")
			}
		})
	}
	// Exercise every nonidentity point in an order-eight torsion subgroup and
	// its mixed-torsion translate, not only an order-two special case.
	orderEightBytes, _ := hex.DecodeString("26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc85")
	orderEight, err := new(edwards25519.Point).SetBytes(orderEightBytes)
	if err != nil {
		t.Fatal(err)
	}
	current := edwards25519.NewIdentityPoint()
	for i := 1; i < 8; i++ {
		current.Add(current, orderEight)
		if current.Equal(edwards25519.NewIdentityPoint()) == 1 {
			t.Fatal("fixture has order less than eight")
		}
		if validProofPublicKey(current.Bytes()) || validProofPublicKey(new(edwards25519.Point).Add(edwards25519.NewGeneratorPoint(), current).Bytes()) {
			t.Fatal("accepted torsion or mixed-torsion subgroup point", i)
		}
	}
	if new(edwards25519.Point).Add(current, orderEight).Equal(edwards25519.NewIdentityPoint()) != 1 {
		t.Fatal("fixture order is not eight")
	}
}

func TestIdentityProofRejectIncoherentPrivateKey(t *testing.T) {
	_, key, c, m, ch, o := proofFixture(t, InventoryProof)
	otherPublic, _, _ := ed25519.GenerateKey(rand.Reader)
	mixedKey := append(ed25519.PrivateKey(nil), key...)
	copy(mixedKey[ed25519.SeedSize:], otherPublic)
	session, _ := NewProofSession()
	if _, err := SignIdentityProof(mixedKey, c, m, session, ch, o); err == nil {
		t.Fatal("signed with incoherent private seed/public halves")
	}
}
