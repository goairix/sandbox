package redisbootstrap

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
)

// BootstrapRegistration retains immutable namespace member identities, not election state.
type BootstrapRegistration struct {
	Cluster   ClusterState `json:"cluster"`
	KeyDigest string       `json:"keyDigest"`
	MarkerIDs [3]string    `json:"markerIDs"`
}

var errBootstrapRegistration = errors.New("invalid bootstrap registration")

// Validate checks the retained cluster, ordered key digest and unique markers.
func (r BootstrapRegistration) Validate() error {
	if r.Cluster.Validate() != nil || !lowerHex(r.KeyDigest, 32) {
		return errBootstrapRegistration
	}
	for i, marker := range r.MarkerIDs {
		if !lowerHex(marker, 16) {
			return errBootstrapRegistration
		}
		for j := 0; j < i; j++ {
			if marker == r.MarkerIDs[j] {
				return errBootstrapRegistration
			}
		}
	}
	return nil
}

// ParseBootstrapRegistration strictly decodes a bounded registration document.
func ParseBootstrapRegistration(data []byte) (BootstrapRegistration, error) {
	var r BootstrapRegistration
	if checkJSON(data) != nil {
		return r, errBootstrapRegistration
	}
	fields, err := exactKeys(data, "cluster", "keyDigest", "markerIDs")
	if err != nil {
		return r, errBootstrapRegistration
	}
	cluster, err := ParseClusterState(fields["cluster"])
	if err != nil {
		return r, errBootstrapRegistration
	}
	var markers []string
	if json.Unmarshal(fields["markerIDs"], &markers) != nil || len(markers) != 3 || json.Unmarshal(fields["keyDigest"], &r.KeyDigest) != nil {
		return BootstrapRegistration{}, errBootstrapRegistration
	}
	r.Cluster = cluster
	copy(r.MarkerIDs[:], markers)
	if r.Validate() != nil {
		return BootstrapRegistration{}, errBootstrapRegistration
	}
	return r, nil
}

func registrationInventoryChallenges(challenges [3]IdentityChallenge) error {
	for i, ch := range challenges {
		if validateChallenge(ch) != nil || ch.Purpose != InventoryProof {
			return errBootstrapRegistration
		}
		for j := 0; j < i; j++ {
			if ch.Nonce == challenges[j].Nonce {
				return errBootstrapRegistration
			}
		}
	}
	return nil
}

// RegisterFreshVolumes grants identity registration only to three Reserved Pending
// members. Each caller-generated inventory nonce must be fresh and used once;
// this verifier cannot establish its generation time or independently inspect PVCs.
// The signed observations must come from trusted local observation providers.
func RegisterFreshVolumes(c ClusterState, keys [3]ed25519.PublicKey, challenges [3]IdentityChallenge, proofs [3]IdentityProof) (BootstrapRegistration, error) {
	if c.Validate() != nil || c.Phase != Pending || registrationInventoryChallenges(challenges) != nil {
		return BootstrapRegistration{}, errBootstrapRegistration
	}
	digest, err := PublicKeySetDigest(keys)
	if err != nil {
		return BootstrapRegistration{}, errBootstrapRegistration
	}
	r := BootstrapRegistration{Cluster: c, KeyDigest: digest}
	for i, proof := range proofs {
		member := Member{DNS: c.Members[i], Ordinal: i}
		observation := proof.Observation
		if observation.Volume.Empty || observation.Volume.Identity == nil || observation.Volume.Identity.InitialConfig != Reserved || VerifyIdentityProof(keys[i], c, member, challenges[i], proof, nil, nil) != nil {
			return BootstrapRegistration{}, errBootstrapRegistration
		}
		r.MarkerIDs[i] = observation.Volume.Identity.MarkerID
	}
	if r.Validate() != nil {
		return BootstrapRegistration{}, errBootstrapRegistration
	}
	return r, nil
}

// VerifyRegisteredProof binds a fresh proof to the original ordinal marker and
// key set. Pending inventory allows Reserved -> Configured; Initialized inventory
// requires Configured. Live always requires Configured and a current endpoint
// independently authenticated by the caller. The caller must generate a new
// one-use challenge for this check. This is not INFO, topology or readiness proof.
func VerifyRegisteredProof(r BootstrapRegistration, keys [3]ed25519.PublicKey, ch IdentityChallenge, proof IdentityProof, current *AuthenticatedEndpoint) error {
	if r.Validate() != nil {
		return errBootstrapRegistration
	}
	digest, err := PublicKeySetDigest(keys)
	if err != nil || digest != r.KeyDigest || proof.Member.Validate(r.Cluster) != nil {
		return errBootstrapRegistration
	}
	member := proof.Member
	state := Reserved
	if r.Cluster.Phase == Initialized || ch.Purpose == LiveProof {
		state = Configured
	}
	expected := VolumeIdentity{ClusterID: r.Cluster.ClusterID, MarkerID: r.MarkerIDs[member.Ordinal], Member: member, InitialConfig: state}
	if VerifyIdentityProof(keys[member.Ordinal], r.Cluster, member, ch, proof, &expected, current) != nil {
		return errBootstrapRegistration
	}
	return nil
}
