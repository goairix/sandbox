package etcd

import (
	"encoding/json"
	"fmt"
	"time"
)

// RuntimePublicationRecord permanently attributes publication to its original
// dispatch, runtime binding, consumed mount attempt, and publication claim.
type RuntimePublicationRecord struct {
	Version           uint32                 `json:"version"`
	IntentID          string                 `json:"intent_id"`
	SandboxID         string                 `json:"sandbox_id"`
	WorkspaceHash     string                 `json:"workspace_hash"`
	RestoreEpoch      string                 `json:"restore_epoch"`
	Generation        int64                  `json:"generation"`
	DataGateEpoch     int64                  `json:"data_gate_epoch"`
	Snapshot          SnapshotReference      `json:"snapshot"`
	ExpiresAt         time.Time              `json:"expires_at"`
	OperationID       string                 `json:"operation_id"`
	PayloadDigest     string                 `json:"payload_digest"`
	ProofDigest       string                 `json:"proof_digest"`
	CertificateDigest string                 `json:"certificate_digest"`
	MountOperationID  string                 `json:"mount_operation_id"`
	Runtime           RuntimeReference       `json:"runtime"`
	MountAttempt      uint8                  `json:"mount_attempt"`
	Claim             DispatchClaimReference `json:"claim"`
	Attempt           StageAttemptLocator    `json:"attempt"`
}

// RuntimePublicationProofRecord preserves the complete opaque proof JSON.
// Authenticity and its signed schema are checked by the configured authority.
type RuntimePublicationProofRecord struct {
	Version       uint32          `json:"version"`
	IntentID      string          `json:"intent_id"`
	SandboxID     string          `json:"sandbox_id"`
	WorkspaceHash string          `json:"workspace_hash"`
	RestoreEpoch  string          `json:"restore_epoch"`
	ProofDigest   string          `json:"proof_digest"`
	Generation    int64           `json:"generation"`
	Payload       json.RawMessage `json:"payload"`
}

func (r RuntimePublicationRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || r.DataGateEpoch <= 0 || r.Snapshot.Validate() != nil || !validDomainExpiry(r.ExpiresAt) || !validPreparationUUID(r.OperationID) || !validHexDigest(r.PayloadDigest) || !validHexDigest(r.ProofDigest) || !validHexDigest(r.CertificateDigest) || r.Runtime.Validate() != nil || !validPreparationClaim(r.Claim) || !validPreparationAttempt(r.Attempt, r.WorkspaceHash, r.RestoreEpoch, "runtime_publish") || !((r.MountAttempt == 0 && r.MountOperationID == "") || (r.MountAttempt == 1 && validPreparationUUID(r.MountOperationID))) {
		return ErrInvalidRecord
	}
	return nil
}
func (r RuntimePublicationProofRecord) Validate() error {
	// Count all original proof bytes before JSON compaction, including whitespace.
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || !validHexDigest(r.ProofDigest) || len(r.Payload) > 8192 {
		return ErrInvalidRecord
	}
	digest, err := snapshotDigest(r.Payload)
	if err != nil || digest != r.ProofDigest {
		return ErrInvalidRecord
	}
	return nil
}
func (n Namespace) runtimePublicationKeys(p uint8, intent string) (journal, proof string, err error) {
	if !validDomainSegment(intent) {
		return "", "", ErrInvalidRecord
	}
	journal, err = n.Key("p", fmt.Sprintf("%02x", p), "intents", intent, "runtime-publication")
	if err != nil {
		return "", "", err
	}
	proof, err = n.Key("p", fmt.Sprintf("%02x", p), "intents", intent, "publication-proof")
	return journal, proof, err
}
