package redisbootstrap

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

// ReadinessAttestation is a proposed local-PVC/process identity proof only.
// It does not prove topology, replication readiness or authorization to initialize.
type ReadinessAttestation struct {
	Version   int    `json:"version"`
	ClusterID string `json:"clusterID"`
	Member    Member `json:"member"`
	MarkerID  string `json:"markerID"`
	RunID     string `json:"runID"`
	Signature string `json:"signature"`
}

// AuthenticatedEndpoint is caller-acquired, fresh authenticated INFO server evidence.
// The caller must connect to this exact fixed member and prevent endpoint substitution.
// Authenticated is an assertion by that trusted caller, not authentication performed here.
type AuthenticatedEndpoint struct {
	Member        Member
	RunID         string
	Authenticated bool
}

var runIDPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

func attestationMAC(key []byte, proof ReadinessAttestation) ([]byte, error) {
	if len(key) < 32 {
		return nil, errors.New("attestation MAC key needs at least 256 bits")
	}
	proof.Signature = ""
	data, err := json.Marshal(proof)
	if err != nil {
		return nil, fmt.Errorf("encode attestation: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	if _, err := mac.Write([]byte("sandbox/redisbootstrap/pvc-readiness/v1\x00")); err != nil {
		return nil, err
	}
	if _, err := mac.Write(data); err != nil {
		return nil, err
	}
	return mac.Sum(nil), nil
}

// SignAttestation signs a locally validated Configured PVC identity and live run_id.
// The trusted local signer must itself read/validate the PVC and authenticate its
// own Redis process. Supplying arbitrary request fields to this function in a
// remote signing service would destroy its identity guarantee.
func SignAttestation(key []byte, identity VolumeIdentity, c ClusterState, runID string) (ReadinessAttestation, error) {
	var proof ReadinessAttestation
	if err := identity.Validate(c); err != nil {
		return proof, err
	}
	if identity.InitialConfig != Configured {
		return proof, errors.New("incomplete local configuration cannot attest readiness")
	}
	if !runIDPattern.MatchString(runID) {
		return proof, errors.New("invalid Redis server run_id")
	}
	proof = ReadinessAttestation{Version: 1, ClusterID: identity.ClusterID, Member: identity.Member, MarkerID: identity.MarkerID, RunID: runID}
	signature, err := attestationMAC(key, proof)
	if err != nil {
		return ReadinessAttestation{}, err
	}
	proof.Signature = hex.EncodeToString(signature)
	return proof, nil
}

// VerifyAttestation verifies MAC and expected PVC identity, then binds the proof
// to a freshly authenticated current endpoint's run_id. An old same-DNS process
// proof cannot attest a replacement Redis process with a different run_id.
// Same-process replay is not topology freshness; transport/challenge, trusted
// marker registration and signing-key isolation are not implemented here.
func VerifyAttestation(key []byte, proof ReadinessAttestation, c ClusterState, expected VolumeIdentity, current AuthenticatedEndpoint) error {
	if err := expected.Validate(c); err != nil {
		return err
	}
	if expected.InitialConfig != Configured {
		return errors.New("expected PVC configuration is incomplete")
	}
	if err := current.Member.Validate(c); err != nil {
		return err
	}
	if proof.Version != 1 || proof.ClusterID != c.ClusterID || proof.Member != expected.Member || proof.MarkerID != expected.MarkerID {
		return errors.New("attestation does not match expected PVC identity")
	}
	if !current.Authenticated || current.Member != proof.Member || !runIDPattern.MatchString(current.RunID) || proof.RunID != current.RunID {
		return errors.New("attestation does not bind current authenticated Redis process")
	}
	signature, err := hex.DecodeString(proof.Signature)
	if err != nil || len(signature) != sha256.Size || hex.EncodeToString(signature) != proof.Signature {
		return errors.New("invalid attestation signature encoding")
	}
	expectedMAC, err := attestationMAC(key, proof)
	if err != nil {
		return err
	}
	if !hmac.Equal(signature, expectedMAC) {
		return errors.New("invalid attestation MAC")
	}
	return nil
}
