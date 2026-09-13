package redisbootstrap

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"

	"filippo.io/edwards25519"
)

// ProofPurpose separates initial PVC inventory from authenticated live-process proof.
type ProofPurpose string

const (
	InventoryProof ProofPurpose = "inventory"
	LiveProof      ProofPurpose = "live"
)

// IdentityChallenge is the entire remotely supplied signing request.
type IdentityChallenge struct {
	Version int          `json:"version"`
	Purpose ProofPurpose `json:"purpose"`
	Nonce   string       `json:"nonce"`
}

// LocalObservation is trusted local evidence, not remotely supplied identity.
// Its provider must coherently read/validate the private PVC and, for live,
// authenticate the local Redis INFO server run_id. This library does neither.
type LocalObservation struct {
	Volume       VolumeState               `json:"volume"`
	Snapshot     *PersistentConfigSnapshot `json:"snapshot"`
	ConfigDigest string                    `json:"configDigest"`
	RunID        string                    `json:"runID"`
}

// IdentityProof binds a fixed local signer session and observation to a challenge.
// Session is signed, not a timestamp; the caller's fresh nonce prevents replay.
type IdentityProof struct {
	Version     int              `json:"version"`
	Purpose     ProofPurpose     `json:"purpose"`
	Nonce       string           `json:"nonce"`
	Session     string           `json:"session"`
	ClusterID   string           `json:"clusterID"`
	Member      Member           `json:"member"`
	Observation LocalObservation `json:"observation"`
	Signature   string           `json:"signature"`
}

var errIdentityProof = errors.New("invalid identity proof")

func lowerHex(s string, size int) bool {
	decoded, err := hex.DecodeString(s)
	return err == nil && len(decoded) == size && hex.EncodeToString(decoded) == s
}
func validPurpose(p ProofPurpose) bool { return p == InventoryProof || p == LiveProof }
func validateChallenge(ch IdentityChallenge) error {
	if ch.Version != 1 || !validPurpose(ch.Purpose) || !lowerHex(ch.Nonce, 32) {
		return errIdentityProof
	}
	return nil
}

// NewIdentityChallenge generates a cryptographically random one-use nonce.
func NewIdentityChallenge(p ProofPurpose) (IdentityChallenge, error) {
	if !validPurpose(p) {
		return IdentityChallenge{}, errIdentityProof
	}
	nonce, err := NewProofSession()
	return IdentityChallenge{Version: 1, Purpose: p, Nonce: nonce}, err
}

// NewProofSession generates an independent random signer-process session.
func NewProofSession() (string, error) {
	var data [32]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", errors.New("identity randomness unavailable")
	}
	return hex.EncodeToString(data[:]), nil
}

func validateObservation(c ClusterState, m Member, purpose ProofPurpose, o LocalObservation) error {
	v := o.Volume
	if purpose == InventoryProof && o.RunID != "" {
		return errIdentityProof
	}
	if v.Empty {
		if purpose != InventoryProof || v.Identity != nil || v.Persisted != nil || o.Snapshot != nil || o.ConfigDigest != "" || o.RunID != "" {
			return errIdentityProof
		}
		return nil
	}
	if v.Identity == nil || v.Identity.Validate(c) != nil || v.Identity.Member != m {
		return errIdentityProof
	}
	if v.Identity.InitialConfig == Reserved {
		if purpose != InventoryProof || v.Persisted != nil || o.Snapshot != nil || o.ConfigDigest != "" || o.RunID != "" {
			return errIdentityProof
		}
		return nil
	}
	if v.Persisted == nil || o.Snapshot == nil || !lowerHex(o.ConfigDigest, 32) || !runIDPattern.MatchString(o.Snapshot.SentinelID) {
		return errIdentityProof
	}
	p := *v.Persisted
	if p.Member != m || o.Snapshot.State != p || o.Snapshot.CurrentEpoch < p.SentinelEpoch || !containsDNS(c, p.PrimaryDNS) || (p.Role != Primary && p.Role != Replica) || (p.Role == Primary && p.PrimaryDNS != m.DNS) || (p.Role == Replica && p.PrimaryDNS == m.DNS) {
		return errIdentityProof
	}
	if purpose == LiveProof && !runIDPattern.MatchString(o.RunID) {
		return errIdentityProof
	}
	return nil
}

func proofSigningBytes(proof IdentityProof) []byte {
	proof.Signature = ""
	// This concrete wire struct contains only strings, integers, booleans and
	// fixed structs/pointers, with no custom Marshaler or cyclic references.
	// json.Marshal cannot fail for this type.
	data, _ := json.Marshal(proof)
	return append([]byte("sandbox/redisbootstrap/identity-proof/v1\x00"), data...)
}

// inverseProofCofactor is 8^-1 modulo the prime-order subgroup's scalar order.
// Eight has a fixed canonical scalar encoding, so SetCanonicalBytes cannot
// fail. The maintained group library performs all curve/scalar operations.
var inverseProofCofactor = func() *edwards25519.Scalar {
	encoded := [32]byte{8}
	eight, _ := new(edwards25519.Scalar).SetCanonicalBytes(encoded[:])
	return new(edwards25519.Scalar).Invert(eight)
}()

// validProofPublicKey requires a canonical, nonidentity prime-subgroup point.
// Length/nonzero checks alone permit identity-key Ed25519 signature forgery.
// Cofactor multiplication alone also misses mixed-torsion points. Projecting
// P into the prime subgroup as [8^-1]([8]P) and requiring equality rejects both
// pure small-order and mixed-torsion keys without a custom curve implementation.
func validProofPublicKey(key ed25519.PublicKey) bool {
	if len(key) != ed25519.PublicKeySize {
		return false
	}
	point, err := new(edwards25519.Point).SetBytes(key)
	if err != nil || !bytes.Equal(point.Bytes(), key) || point.Equal(edwards25519.NewIdentityPoint()) == 1 {
		return false
	}
	cofactored := new(edwards25519.Point).MultByCofactor(point)
	projected := new(edwards25519.Point).ScalarMult(inverseProofCofactor, cofactored)
	return projected.Equal(point) == 1
}

func validProofPrivateKey(key ed25519.PrivateKey) bool {
	if len(key) != ed25519.PrivateKeySize {
		return false
	}
	// Go's 64-byte private-key format is seed || public key. Regeneration proves
	// these halves agree; compare the complete key without secret-dependent timing.
	derived := ed25519.NewKeyFromSeed(key[:ed25519.SeedSize])
	return subtle.ConstantTimeCompare(derived, key) == 1
}

// SignIdentityProof signs only trusted local observations. It does not read PVC
// files or authenticate INFO, and grants no election or readiness authority.
func SignIdentityProof(key ed25519.PrivateKey, c ClusterState, m Member, session string, ch IdentityChallenge, o LocalObservation) (IdentityProof, error) {
	if !validProofPrivateKey(key) || m.Validate(c) != nil || !lowerHex(session, 32) || validateChallenge(ch) != nil || validateObservation(c, m, ch.Purpose, o) != nil {
		return IdentityProof{}, errIdentityProof
	}
	proof := IdentityProof{Version: 1, Purpose: ch.Purpose, Nonce: ch.Nonce, Session: session, ClusterID: c.ClusterID, Member: m, Observation: o}
	proof.Signature = hex.EncodeToString(ed25519.Sign(key, proofSigningBytes(proof)))
	return proof, nil
}

// VerifyIdentityProof verifies a fixed public key and fresh challenge. First
// inventory registration may omit expected; subsequent inventory must retain
// its marker, including Reserved -> Configured. Live requires both a Configured
// expected marker and independently freshly authenticated exact endpoint.
func VerifyIdentityProof(key ed25519.PublicKey, c ClusterState, m Member, ch IdentityChallenge, proof IdentityProof, expected *VolumeIdentity, current *AuthenticatedEndpoint) error {
	if !validProofPublicKey(key) || m.Validate(c) != nil || validateChallenge(ch) != nil || proof.Version != ch.Version || proof.Purpose != ch.Purpose || proof.Nonce != ch.Nonce || proof.ClusterID != c.ClusterID || proof.Member != m || !lowerHex(proof.Session, 32) || validateObservation(c, m, ch.Purpose, proof.Observation) != nil || !lowerHex(proof.Signature, 64) {
		return errIdentityProof
	}
	if expected != nil {
		actual := proof.Observation.Volume.Identity
		if expected.Validate(c) != nil || expected.Member != m || actual == nil || actual.ClusterID != expected.ClusterID || actual.Member != expected.Member || actual.MarkerID != expected.MarkerID || (expected.InitialConfig == Configured && actual.InitialConfig != Configured) {
			return errIdentityProof
		}
	}
	if ch.Purpose == LiveProof && (expected == nil || expected.InitialConfig != Configured || current == nil || !current.Authenticated || current.Member != m || !runIDPattern.MatchString(current.RunID) || current.RunID != proof.Observation.RunID) {
		return errIdentityProof
	}
	signature, _ := hex.DecodeString(proof.Signature)
	if !ed25519.Verify(key, proofSigningBytes(proof), signature) {
		return errIdentityProof
	}
	return nil
}

// PublicKeySetDigest hashes the fixed ordinal order, rejecting missing,
// noncanonical, non-prime-subgroup, identity or reused member keys. It is not
// namespace registration itself.
func PublicKeySetDigest(keys [3]ed25519.PublicKey) (string, error) {
	h := sha256.New()
	for i, key := range keys {
		if !validProofPublicKey(key) {
			return "", errIdentityProof
		}
		for j := 0; j < i; j++ {
			if string(key) == string(keys[j]) {
				return "", errIdentityProof
			}
		}
		// SHA-256's hash.Hash Write always consumes all bytes and returns nil.
		_, _ = h.Write(key)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
