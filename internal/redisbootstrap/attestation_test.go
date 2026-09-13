package redisbootstrap

import (
	"strings"
	"testing"
)

func TestAttestationBindsCurrentAuthenticatedProcess(t *testing.T) {
	for _, name := range []string{"valid", "short key", "wrong key", "cluster", "marker", "dns", "ordinal", "run id", "stale replacement", "unauthenticated endpoint", "wrong endpoint", "signature", "version", "reserved marker"} {
		t.Run(name, func(t *testing.T) {
			c := testCluster()
			identity := testIdentity(c, 0)
			key := []byte(strings.Repeat("k", 32))
			runID := strings.Repeat("a", 40)
			proof, err := SignAttestation(key, identity, c, runID)
			if err != nil {
				t.Fatal(err)
			}
			endpoint := AuthenticatedEndpoint{Member: testMember(c, 0), RunID: runID, Authenticated: true}
			switch name {
			case "short key":
				key = key[:8]
			case "wrong key":
				key = []byte(strings.Repeat("x", 32))
			case "cluster":
				proof.ClusterID = "other"
			case "marker":
				proof.MarkerID = strings.Repeat("f", 32)
			case "dns":
				proof.Member.DNS = c.Members[1]
			case "ordinal":
				proof.Member.Ordinal = 1
			case "run id":
				proof.RunID = "bad"
			case "stale replacement":
				endpoint.RunID = strings.Repeat("b", 40)
			case "unauthenticated endpoint":
				endpoint.Authenticated = false
			case "wrong endpoint":
				endpoint.Member = testMember(c, 1)
			case "signature":
				proof.Signature = strings.Repeat("0", 64)
			case "version":
				proof.Version = 2
			case "reserved marker":
				identity.InitialConfig = Reserved
			}
			err = VerifyAttestation(key, proof, c, identity, endpoint)
			if (err == nil) != (name == "valid") {
				t.Fatalf("Verify=%v", err)
			}
		})
	}
}

func TestAttestationSigningRejectsInvalidInputs(t *testing.T) {
	c := testCluster()
	v := testIdentity(c, 0)
	key := []byte(strings.Repeat("k", 32))
	for _, name := range []string{"short key", "empty run id", "uppercase run id", "invalid identity", "reserved"} {
		t.Run(name, func(t *testing.T) {
			vv := v
			kk := key
			runID := strings.Repeat("a", 40)
			switch name {
			case "short key":
				kk = kk[:1]
			case "empty run id":
				runID = ""
			case "uppercase run id":
				runID = strings.Repeat("A", 40)
			case "invalid identity":
				vv.Member.Ordinal = 3
			case "reserved":
				vv.InitialConfig = Reserved
			}
			if _, err := SignAttestation(kk, vv, c, runID); err == nil {
				t.Fatal("invalid signing input accepted")
			}
		})
	}
}

func TestBusinessWritersPhaseGate(t *testing.T) {
	c := testCluster()
	if BusinessWritersAllowed(nil) || BusinessWritersAllowed(&c) {
		t.Fatal("Pending allowed writers")
	}
	c.Phase = Initialized
	if !BusinessWritersAllowed(&c) {
		t.Fatal("valid Initialized blocked")
	}
	c.Members[1] = c.Members[0]
	if BusinessWritersAllowed(&c) {
		t.Fatal("invalid Initialized allowed writers")
	}
}
