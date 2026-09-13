package redisbootstrap

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
)

type registrationFixture struct {
	cluster    ClusterState
	keys       [3]ed25519.PublicKey
	private    [3]ed25519.PrivateKey
	challenges [3]IdentityChallenge
	proofs     [3]IdentityProof
}

func fixtureRegistration(f registrationFixture) BootstrapRegistration {
	digest, _ := PublicKeySetDigest(f.keys)
	r := BootstrapRegistration{Cluster: f.cluster, KeyDigest: digest}
	for i, p := range f.proofs {
		r.MarkerIDs[i] = p.Observation.Volume.Identity.MarkerID
	}
	return r
}

func configuredRegistrationObservation(f registrationFixture, i int, purpose ProofPurpose) LocalObservation {
	identity := *f.proofs[i].Observation.Volume.Identity
	identity.InitialConfig = Configured
	member := testMember(f.cluster, i)
	role := Replica
	if i == 0 {
		role = Primary
	}
	persisted := PersistedState{Member: member, Role: role, PrimaryDNS: f.cluster.Members[0], SentinelEpoch: 3}
	o := LocalObservation{Volume: VolumeState{Identity: &identity, Persisted: &persisted}, Snapshot: &PersistentConfigSnapshot{State: persisted, SentinelID: strings.Repeat("a", 40), CurrentEpoch: 4}, ConfigDigest: strings.Repeat("b", 64)}
	if purpose == LiveProof {
		o.RunID = strings.Repeat("c", 40)
	}
	return o
}

func TestBootstrapRegistrationStrictRoundTrip(t *testing.T) {
	f := newRegistrationFixture(t)
	r := fixtureRegistration(f)
	if err := r.Validate(); err != nil {
		t.Fatal("valid registration rejected:", err)
	}
	data, _ := json.Marshal(r)
	got, err := ParseBootstrapRegistration(data)
	if err != nil || got != r {
		t.Fatal("valid registration JSON rejected:", err)
	}
}

func TestBootstrapRegistrationRejectsMalformedJSON(t *testing.T) {
	f := newRegistrationFixture(t)
	r := fixtureRegistration(f)
	data, _ := json.Marshal(r)
	valid := string(data)
	for name, input := range map[string]string{
		"empty": "", "null": "null", "array": "[]", "trailing": valid + " {}",
		"duplicate":         strings.Replace(valid, `"keyDigest":`, `"keyDigest":"`+r.KeyDigest+`","keyDigest":`, 1),
		"unknown":           strings.Replace(valid, `"keyDigest":`, `"extra":"PRIVATE-CM-CONTENT","keyDigest":`, 1),
		"noncanonical":      strings.Replace(valid, `"keyDigest":`, `"KeyDigest":`, 1),
		"null digest":       strings.Replace(valid, `"`+r.KeyDigest+`"`, "null", 1),
		"null cluster":      strings.Replace(valid, `"cluster":`+mustRegistrationJSON(t, r.Cluster), `"cluster":null`, 1),
		"null markers":      strings.Replace(valid, mustRegistrationJSON(t, r.MarkerIDs), "null", 1),
		"two markers":       strings.Replace(valid, mustRegistrationJSON(t, r.MarkerIDs), mustRegistrationJSON(t, r.MarkerIDs[:2]), 1),
		"four markers":      strings.Replace(valid, mustRegistrationJSON(t, r.MarkerIDs), mustRegistrationJSON(t, append(r.MarkerIDs[:], strings.Repeat("d", 32))), 1),
		"duplicate markers": strings.Replace(valid, r.MarkerIDs[1], r.MarkerIDs[0], 1),
		"uppercase marker":  strings.Replace(valid, r.MarkerIDs[0], strings.Repeat("A", 32), 1),
		"short marker":      strings.Replace(valid, r.MarkerIDs[0], "aa", 1),
		"uppercase digest":  strings.Replace(valid, r.KeyDigest, strings.Repeat("A", 64), 1),
		"short digest":      strings.Replace(valid, r.KeyDigest, "aa", 1),
		"unknown cluster":   strings.Replace(valid, `"clusterID":`, `"extra":1,"clusterID":`, 1),
		"duplicate cluster": strings.Replace(valid, `"phase":"Pending"`, `"phase":"Pending","phase":"Pending"`, 1),
		"null phase":        strings.Replace(valid, `"phase":"Pending"`, `"phase":null`, 1),
		"phase":             strings.Replace(valid, `"phase":"Pending"`, `"phase":"Other"`, 1),
		"two members":       strings.Replace(valid, mustRegistrationJSON(t, r.Cluster.Members), mustRegistrationJSON(t, r.Cluster.Members[:2]), 1),
		"size":              strings.Repeat(" ", maximumStateBytes+1) + valid,
		"invalid utf8":      string(append([]byte(valid), 0xff)),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBootstrapRegistration([]byte(input)); err == nil {
				t.Fatal("accepted malformed registration")
			} else if strings.Contains(err.Error(), "PRIVATE-CM-CONTENT") {
				t.Fatal("leaked JSON")
			}
		})
	}
}

func mustRegistrationJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRegisterFreshVolumesRejectsUnfreshAndUnreservedProofs(t *testing.T) {
	for _, name := range []string{"Initialized", "empty", "configured", "live", "replayed proof", "same nonce", "swapped proofs", "duplicate marker", "wrong key", "reused key", "weak key", "foreign cluster", "tampered signature", "reserved config"} {
		t.Run(name, func(t *testing.T) {
			f := newRegistrationFixture(t)
			switch name {
			case "Initialized":
				f.cluster.Phase = Initialized
			case "empty":
				f.proofs[1] = f.sign(t, 1, f.challenges[1], LocalObservation{Volume: VolumeState{Empty: true}})
			case "configured":
				f.proofs[1] = f.sign(t, 1, f.challenges[1], configuredRegistrationObservation(f, 1, InventoryProof))
			case "live":
				f.challenges[1], _ = NewIdentityChallenge(LiveProof)
				f.proofs[1] = f.sign(t, 1, f.challenges[1], configuredRegistrationObservation(f, 1, LiveProof))
			case "replayed proof":
				f.challenges[1], _ = NewIdentityChallenge(InventoryProof)
			case "same nonce":
				f.challenges[1] = f.challenges[0]
				f.proofs[1] = f.sign(t, 1, f.challenges[1], f.proofs[1].Observation)
			case "swapped proofs":
				f.proofs[0], f.proofs[1] = f.proofs[1], f.proofs[0]
			case "duplicate marker":
				identity := *f.proofs[1].Observation.Volume.Identity
				identity.MarkerID = f.proofs[0].Observation.Volume.Identity.MarkerID
				f.proofs[1] = f.sign(t, 1, f.challenges[1], LocalObservation{Volume: VolumeState{Identity: &identity}})
			case "wrong key":
				f.keys[1], _, _ = ed25519.GenerateKey(rand.Reader)
			case "reused key":
				f.keys[1] = f.keys[0]
			case "weak key":
				f.keys[1] = make(ed25519.PublicKey, 32)
			case "foreign cluster":
				f.proofs[1].ClusterID = "foreign"
			case "tampered signature":
				f.proofs[1].Signature = strings.Repeat("0", 128)
			case "reserved config":
				f.proofs[1].Observation.ConfigDigest = strings.Repeat("d", 64)
			}
			if _, err := RegisterFreshVolumes(f.cluster, f.keys, f.challenges, f.proofs); err == nil {
				t.Fatal("accepted unreserved/unbound registration")
			}
		})
	}
}

func TestVerifyRegisteredProofRetainsIdentityAcrossConfiguration(t *testing.T) {
	f := newRegistrationFixture(t)
	r := fixtureRegistration(f)
	for i := range f.proofs {
		if err := VerifyRegisteredProof(r, f.keys, f.challenges[i], f.proofs[i], nil); err != nil {
			t.Fatal("Reserved inventory rejected:", err)
		}
		configured := f.sign(t, i, f.challenges[i], configuredRegistrationObservation(f, i, InventoryProof))
		if err := VerifyRegisteredProof(r, f.keys, f.challenges[i], configured, nil); err != nil {
			t.Fatal("configured transition rejected:", err)
		}
		initialized := r
		initialized.Cluster.Phase = Initialized
		if err := VerifyRegisteredProof(initialized, f.keys, f.challenges[i], configured, nil); err != nil {
			t.Fatal("Initialized configured rejected:", err)
		}
		if VerifyRegisteredProof(initialized, f.keys, f.challenges[i], f.proofs[i], nil) == nil {
			t.Fatal("Initialized Reserved accepted")
		}
		live, _ := NewIdentityChallenge(LiveProof)
		observation := configuredRegistrationObservation(f, i, LiveProof)
		proof := f.sign(t, i, live, observation)
		endpoint := &AuthenticatedEndpoint{Member: testMember(f.cluster, i), RunID: observation.RunID, Authenticated: true}
		if err := VerifyRegisteredProof(r, f.keys, live, proof, endpoint); err != nil {
			t.Fatal("configured authenticated live rejected:", err)
		}
		for name, current := range map[string]*AuthenticatedEndpoint{"nil": nil, "old process": {Member: endpoint.Member, RunID: strings.Repeat("d", 40), Authenticated: true}, "unauthenticated": {Member: endpoint.Member, RunID: endpoint.RunID}, "wrong member": {Member: testMember(f.cluster, (i+1)%3), RunID: endpoint.RunID, Authenticated: true}} {
			t.Run(name, func(t *testing.T) {
				if VerifyRegisteredProof(r, f.keys, live, proof, current) == nil {
					t.Fatal("unbound endpoint accepted")
				}
			})
		}
	}
}

func TestVerifyRegisteredProofRejectsReplacementAndReplay(t *testing.T) {
	for _, name := range []string{"marker", "empty", "key", "digest", "invalid registration", "ordinal", "replay", "signature"} {
		t.Run(name, func(t *testing.T) {
			f := newRegistrationFixture(t)
			r := fixtureRegistration(f)
			proof := f.proofs[1]
			ch := f.challenges[1]
			switch name {
			case "marker":
				identity := *proof.Observation.Volume.Identity
				identity.MarkerID = strings.Repeat("e", 32)
				proof = f.sign(t, 1, ch, LocalObservation{Volume: VolumeState{Identity: &identity}})
			case "empty":
				proof = f.sign(t, 1, ch, LocalObservation{Volume: VolumeState{Empty: true}})
			case "key":
				f.keys[1], _, _ = ed25519.GenerateKey(rand.Reader)
			case "digest":
				r.KeyDigest = strings.Repeat("a", 64)
			case "invalid registration":
				r.MarkerIDs[1] = "bad"
			case "ordinal":
				proof.Member.Ordinal = -1
			case "replay":
				ch, _ = NewIdentityChallenge(InventoryProof)
			case "signature":
				proof.Signature = strings.Repeat("0", 128)
			}
			if VerifyRegisteredProof(r, f.keys, ch, proof, nil) == nil {
				t.Fatal("accepted replacement/replay")
			}
		})
	}
}

func newRegistrationFixture(t *testing.T) registrationFixture {
	t.Helper()
	f := registrationFixture{cluster: testCluster()}
	for i := range f.keys {
		var err error
		f.keys[i], f.private[i], err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		f.challenges[i], err = NewIdentityChallenge(InventoryProof)
		if err != nil {
			t.Fatal(err)
		}
		identity, err := NewVolumeIdentity(f.cluster, testMember(f.cluster, i))
		if err != nil {
			t.Fatal(err)
		}
		f.proofs[i] = f.sign(t, i, f.challenges[i], LocalObservation{Volume: VolumeState{Identity: &identity}})
	}
	return f
}

func (f registrationFixture) sign(t *testing.T, i int, ch IdentityChallenge, observation LocalObservation) IdentityProof {
	t.Helper()
	session, err := NewProofSession()
	if err != nil {
		t.Fatal(err)
	}
	proof, err := SignIdentityProof(f.private[i], f.cluster, testMember(f.cluster, i), session, ch, observation)
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func TestRegisterFreshVolumesRetainsPendingAndSignedIdentity(t *testing.T) {
	f := newRegistrationFixture(t)
	r, err := RegisterFreshVolumes(f.cluster, f.keys, f.challenges, f.proofs)
	if err != nil {
		t.Fatal("three fresh Reserved proofs must register:", err)
	}
	digest, _ := PublicKeySetDigest(f.keys)
	if r.Cluster != f.cluster || r.Cluster.Phase != Pending || r.KeyDigest != digest {
		t.Fatal("registration changed cluster/gate/keyset")
	}
	for i, p := range f.proofs {
		if r.MarkerIDs[i] != p.Observation.Volume.Identity.MarkerID {
			t.Fatal("lost ordinal marker")
		}
	}
}
