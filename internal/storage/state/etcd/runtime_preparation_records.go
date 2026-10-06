package etcd

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"time"
)

type RuntimeBindingRecord struct {
	Version           uint32                 `json:"version"`
	IntentID          string                 `json:"intent_id"`
	SandboxID         string                 `json:"sandbox_id"`
	WorkspaceHash     string                 `json:"workspace_hash"`
	RestoreEpoch      string                 `json:"restore_epoch"`
	Generation        int64                  `json:"generation"`
	Snapshot          SnapshotReference      `json:"snapshot"`
	ExpiresAt         time.Time              `json:"expires_at"`
	OperationID       string                 `json:"operation_id"`
	PayloadDigest     string                 `json:"payload_digest"`
	CertificateDigest string                 `json:"certificate_digest"`
	WorkspaceMode     string                 `json:"workspace_mode"`
	Runtime           RuntimeReference       `json:"runtime"`
	Claim             DispatchClaimReference `json:"claim"`
	Attempt           StageAttemptLocator    `json:"attempt"`
}
type RuntimeBindingCertificateRecord struct {
	Version           uint32          `json:"version"`
	IntentID          string          `json:"intent_id"`
	SandboxID         string          `json:"sandbox_id"`
	WorkspaceHash     string          `json:"workspace_hash"`
	RestoreEpoch      string          `json:"restore_epoch"`
	CertificateDigest string          `json:"certificate_digest"`
	Generation        int64           `json:"generation"`
	Payload           json.RawMessage `json:"payload"`
}
type RuntimeIndexRecord struct {
	Version       uint32           `json:"version"`
	IntentID      string           `json:"intent_id"`
	SandboxID     string           `json:"sandbox_id"`
	WorkspaceHash string           `json:"workspace_hash"`
	RestoreEpoch  string           `json:"restore_epoch"`
	Generation    int64            `json:"generation"`
	Runtime       RuntimeReference `json:"runtime"`
}
type RuntimeMountIntentRecord struct {
	Version             uint32                 `json:"version"`
	IntentID            string                 `json:"intent_id"`
	SandboxID           string                 `json:"sandbox_id"`
	WorkspaceHash       string                 `json:"workspace_hash"`
	RestoreEpoch        string                 `json:"restore_epoch"`
	Generation          int64                  `json:"generation"`
	Runtime             RuntimeReference       `json:"runtime"`
	CertificateDigest   string                 `json:"certificate_digest"`
	DispatchOperationID string                 `json:"dispatch_operation_id"`
	OperationID         string                 `json:"operation_id"`
	WorkspaceMode       string                 `json:"workspace_mode"`
	MountAttempt        uint8                  `json:"mount_attempt"`
	Claim               DispatchClaimReference `json:"claim"`
	Attempt             StageAttemptLocator    `json:"attempt"`
}

func validPreparationClaim(c DispatchClaimReference) bool {
	return canonicalDispatchUUID(c.ClaimID) && c.ClaimID != "00000000-0000-0000-0000-000000000000" && validDomainSegment(c.WorkerID) && c.CreateRevision > 0 && c.LeaseID > 0
}
func validPreparationAttempt(a StageAttemptLocator, hash, epoch, stage string) bool {
	return a.Validate() == nil && a.Partition == hashPartition(hash) && a.RestoreEpoch == epoch && a.StageID == stage
}
func (r RuntimeBindingRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || r.Snapshot.Validate() != nil || !validDomainExpiry(r.ExpiresAt) || !canonicalDispatchUUID(r.OperationID) || !validHexDigest(r.PayloadDigest) || !validHexDigest(r.CertificateDigest) || (r.WorkspaceMode != "plain" && r.WorkspaceMode != "fuse") || r.Runtime.Validate() != nil || !validPreparationClaim(r.Claim) || !validPreparationAttempt(r.Attempt, r.WorkspaceHash, r.RestoreEpoch, "runtime_bind") {
		return ErrInvalidRecord
	}
	return nil
}
func (r RuntimeBindingCertificateRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || !validHexDigest(r.CertificateDigest) || len(r.Payload) > 4096 {
		return ErrInvalidRecord
	}
	digest, err := snapshotDigest(r.Payload)
	if err != nil || digest != r.CertificateDigest {
		return ErrInvalidRecord
	}
	return nil
}
func (r RuntimeIndexRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || r.Runtime.Validate() != nil {
		return ErrInvalidRecord
	}
	return nil
}
func (r RuntimeMountIntentRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || r.Runtime.Validate() != nil || !validHexDigest(r.CertificateDigest) || !canonicalDispatchUUID(r.DispatchOperationID) || !canonicalDispatchUUID(r.OperationID) || !validPreparationClaim(r.Claim) || !validPreparationAttempt(r.Attempt, r.WorkspaceHash, r.RestoreEpoch, "runtime_mount") || !((r.WorkspaceMode == "plain" && r.MountAttempt == 0) || (r.WorkspaceMode == "fuse" && r.MountAttempt == 1)) {
		return ErrInvalidRecord
	}
	return nil
}
func (n Namespace) runtimeBindingKeys(p uint8, intent string) (string, string, error) {
	if !validDomainSegment(intent) {
		return "", "", ErrInvalidRecord
	}
	bk, err := n.Key("p", fmt.Sprintf("%02x", p), "intents", intent, "runtime-binding")
	if err != nil {
		return "", "", err
	}
	ck, err := n.Key("p", fmt.Sprintf("%02x", p), "intents", intent, "runtime-certificate")
	return bk, ck, err
}
func (n Namespace) runtimeMountIntentKey(p uint8, intent string) (string, error) {
	if !validDomainSegment(intent) {
		return "", ErrInvalidRecord
	}
	return n.Key("p", fmt.Sprintf("%02x", p), "intents", intent, "runtime-mount-intent")
}
func (n Namespace) runtimeIndexKey(uid string) (string, error) {
	if !validOpaque(uid, 128) {
		return "", ErrInvalidRecord
	}
	d := framedDigest("runtime-uid:v1", uid)
	return n.Key("indexes", "runtime", hex.EncodeToString(d[:]))
}

// strictPreparationMetadata checks every typed field, including nested records.
// RawMessage remains opaque: certificate authenticity belongs to the verifier.
func strictPreparationMetadata(wire []byte, typ reflect.Type) error {
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(wire), []byte("null")) {
		return ErrCorruptRecord
	}
	if typ.Kind() != reflect.Struct || typ == reflect.TypeOf(time.Time{}) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(wire))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return ErrCorruptRecord
	}
	fields := make(map[string]reflect.Type, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		fields[f.Tag.Get("json")] = f.Type
	}
	seen := make(map[string]bool, len(fields))
	for dec.More() {
		tok, err = dec.Token()
		if err != nil {
			return ErrCorruptRecord
		}
		name, ok := tok.(string)
		ft, exists := fields[name]
		if !ok || !exists || seen[name] {
			return ErrCorruptRecord
		}
		seen[name] = true
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return ErrCorruptRecord
		}
		if err := strictPreparationMetadata(raw, ft); err != nil {
			return err
		}
	}
	if _, err = dec.Token(); err != nil || len(seen) != len(fields) {
		return ErrCorruptRecord
	}
	if dec.Decode(new(any)) != io.EOF {
		return ErrCorruptRecord
	}
	return nil
}
