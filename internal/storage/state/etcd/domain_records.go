package etcd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"
	"unicode/utf8"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

var ErrInvalidRecord = errors.New("etcd state: invalid record")
var ErrCorruptRecord = errors.New("etcd state: corrupt record")

type RuntimeReference struct {
	ID     string `json:"id"`
	UID    string `json:"uid"`
	BootID string `json:"boot_id"`
}
type SnapshotReference struct {
	Version string `json:"version"`
	Digest  string `json:"digest"`
}
type SandboxPhase string

const (
	PhasePublishing         SandboxPhase = "publishing"
	PhaseActive             SandboxPhase = "active"
	PhaseWorkspaceExclusive SandboxPhase = "workspace_exclusive"
	PhaseDestroying         SandboxPhase = "destroying"
	PhaseCleanupPending     SandboxPhase = "cleanup_pending"
)

type WorkspaceOwnerRecord struct {
	Version       uint32            `json:"version"`
	WorkspaceHash string            `json:"workspace_hash"`
	SandboxID     string            `json:"sandbox_id"`
	IntentID      string            `json:"intent_id"`
	RestoreEpoch  string            `json:"restore_epoch"`
	Generation    int64             `json:"generation"`
	Runtime       *RuntimeReference `json:"runtime,omitempty"`
	MountAttempt  uint8             `json:"mount_attempt"`
}
type WorkspaceFenceRecord struct {
	Version       uint32 `json:"version"`
	WorkspaceHash string `json:"workspace_hash"`
	RestoreEpoch  string `json:"restore_epoch"`
	Generation    int64  `json:"generation"`
}
type SandboxControlRecord struct {
	Version       uint32            `json:"version"`
	SandboxID     string            `json:"sandbox_id"`
	WorkspaceHash string            `json:"workspace_hash"`
	IntentID      string            `json:"intent_id"`
	RestoreEpoch  string            `json:"restore_epoch"`
	Generation    int64             `json:"generation"`
	DataGateEpoch int64             `json:"data_gate_epoch"`
	Phase         SandboxPhase      `json:"phase"`
	Snapshot      SnapshotReference `json:"snapshot"`
	Runtime       *RuntimeReference `json:"runtime,omitempty"`
	MountAttempt  uint8             `json:"mount_attempt"`
	ExpiresAt     time.Time         `json:"expires_at"`
}
type SandboxSnapshotRecord struct {
	Version   uint32            `json:"version"`
	SandboxID string            `json:"sandbox_id"`
	Snapshot  SnapshotReference `json:"snapshot"`
	Payload   json.RawMessage   `json:"payload"`
}
type CreationIntentRecord struct {
	Version             uint32 `json:"version"`
	IntentID            string `json:"intent_id"`
	SandboxID           string `json:"sandbox_id"`
	WorkspaceHash       string `json:"workspace_hash"`
	RequestHash         string `json:"request_hash"`
	ConfigurationDigest string `json:"configuration_digest"`
	RestoreEpoch        string `json:"restore_epoch"`
	Generation          int64  `json:"generation"`
	Phase               string `json:"phase"`
}
type CreationRequestRecord struct {
	Version             uint32 `json:"version"`
	RequestID           string `json:"request_id"`
	RequestHash         string `json:"request_hash"`
	ConfigurationDigest string `json:"configuration_digest"`
	IntentID            string `json:"intent_id"`
	SandboxID           string `json:"sandbox_id"`
	WorkspaceHash       string `json:"workspace_hash"`
	RestoreEpoch        string `json:"restore_epoch"`
	Generation          int64  `json:"generation"`
	Phase               string `json:"phase"`
}

// Validate checks opaque runtime identifiers without interpreting their format.
func (r RuntimeReference) Validate() error {
	if !validOpaque(r.ID, 128) || !validOpaque(r.UID, 128) || !validOpaque(r.BootID, 128) {
		return fmt.Errorf("%w: invalid runtime reference", ErrInvalidRecord)
	}
	return nil
}
func (s SnapshotReference) Validate() error {
	if !validDomainSegment(s.Version) || !validHexDigest(s.Digest) {
		return fmt.Errorf("%w: invalid snapshot reference", ErrInvalidRecord)
	}
	return nil
}
func validWorkspaceFields(version uint32, workspaceHash, restoreEpoch string, generation int64) bool {
	return version == 1 && validHexDigest(workspaceHash) && validDomainSegment(restoreEpoch) && generation > 0
}
func validOwnership(version uint32, workspaceHash, sandboxID, intentID, restoreEpoch string, generation int64) bool {
	return validWorkspaceFields(version, workspaceHash, restoreEpoch, generation) && validDomainSegment(sandboxID) && validDomainSegment(intentID)
}
func validRuntimeMount(runtime *RuntimeReference, mount uint8, provisional bool) bool {
	if runtime == nil {
		return provisional && mount == 0
	}
	return mount <= 1 && runtime.Validate() == nil
}
func (r WorkspaceOwnerRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || !validRuntimeMount(r.Runtime, r.MountAttempt, true) {
		return fmt.Errorf("%w: invalid workspace owner", ErrInvalidRecord)
	}
	return nil
}
func (r WorkspaceFenceRecord) Validate() error {
	if !validWorkspaceFields(r.Version, r.WorkspaceHash, r.RestoreEpoch, r.Generation) {
		return fmt.Errorf("%w: invalid workspace fence", ErrInvalidRecord)
	}
	return nil
}
func validDomainExpiry(expiry time.Time) bool {
	_, offset := expiry.Zone()
	return !expiry.IsZero() && offset == 0 && expiry.Year() >= 1 && expiry.Year() <= 9999
}
func (r SandboxControlRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || r.DataGateEpoch <= 0 || r.Snapshot.Validate() != nil || !validDomainExpiry(r.ExpiresAt) {
		return fmt.Errorf("%w: invalid sandbox control", ErrInvalidRecord)
	}
	switch r.Phase {
	case PhasePublishing:
		if r.Runtime != nil || r.MountAttempt != 0 {
			return fmt.Errorf("%w: publishing sandbox is already bound", ErrInvalidRecord)
		}
	case PhaseActive, PhaseWorkspaceExclusive:
		if !validRuntimeMount(r.Runtime, r.MountAttempt, false) {
			return fmt.Errorf("%w: sandbox runtime binding missing or invalid", ErrInvalidRecord)
		}
	case PhaseDestroying, PhaseCleanupPending:
		// An unbound cleanup intent remains durable until runtime recovery completes.
		if !validRuntimeMount(r.Runtime, r.MountAttempt, true) {
			return fmt.Errorf("%w: invalid sandbox cleanup binding", ErrInvalidRecord)
		}
	default:
		return fmt.Errorf("%w: invalid sandbox phase", ErrInvalidRecord)
	}
	return nil
}
func (r SandboxSnapshotRecord) Validate() error {
	if r.Version != 1 || !validDomainSegment(r.SandboxID) || r.Snapshot.Validate() != nil {
		return fmt.Errorf("%w: invalid sandbox snapshot", ErrInvalidRecord)
	}
	digest, err := snapshotDigest(r.Payload)
	if err != nil {
		return err
	}
	if digest != r.Snapshot.Digest {
		return fmt.Errorf("%w: snapshot digest mismatch", ErrInvalidRecord)
	}
	return nil
}
func (r CreationIntentRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || !validHexDigest(r.RequestHash) || !validHexDigest(r.ConfigurationDigest) || (r.Phase != "pending" && r.Phase != "published") {
		return fmt.Errorf("%w: invalid creation intent", ErrInvalidRecord)
	}
	return nil
}
func (r CreationRequestRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) || !validDomainSegment(r.RequestID) || !validHexDigest(r.RequestHash) || !validHexDigest(r.ConfigurationDigest) || (r.Phase != "pending" && r.Phase != "completed") {
		return fmt.Errorf("%w: invalid creation request", ErrInvalidRecord)
	}
	return nil
}
func domainRecordLimit(record interface{ Validate() error }) int {
	switch record.(type) {
	case RuntimePublicationRecord, *RuntimePublicationRecord, RuntimeBindingRecord, *RuntimeBindingRecord, RuntimeIndexRecord, *RuntimeIndexRecord, RuntimeMountIntentRecord, *RuntimeMountIntentRecord, RuntimeDispatchRecord, *RuntimeDispatchRecord, SandboxPlacementRecord, *SandboxPlacementRecord, WorkspaceOwnerRecord, *WorkspaceOwnerRecord, SandboxControlRecord, *SandboxControlRecord, CreationIntentRecord, *CreationIntentRecord, CreationRequestRecord, *CreationRequestRecord:
		return 4096
	case RuntimeBindingCertificateRecord, *RuntimeBindingCertificateRecord:
		return 8192
	case RuntimePublicationProofRecord, *RuntimePublicationProofRecord:
		return 16384
	case WorkspaceFenceRecord, *WorkspaceFenceRecord:
		return 1024
	case RuntimeDispatchInputRecord, *RuntimeDispatchInputRecord, SandboxSnapshotRecord, *SandboxSnapshotRecord:
		return 65536
	default:
		return 0
	}
}
func nilDomainRecord(record interface{ Validate() error }) bool {
	switch r := record.(type) {
	case *RuntimePublicationRecord:
		return r == nil
	case *RuntimePublicationProofRecord:
		return r == nil
	case *RuntimeBindingRecord:
		return r == nil
	case *RuntimeBindingCertificateRecord:
		return r == nil
	case *RuntimeIndexRecord:
		return r == nil
	case *RuntimeMountIntentRecord:
		return r == nil
	case *RuntimeDispatchRecord:
		return r == nil
	case *RuntimeDispatchInputRecord:
		return r == nil
	case *WorkspaceOwnerRecord:
		return r == nil
	case *WorkspaceFenceRecord:
		return r == nil
	case *SandboxControlRecord:
		return r == nil
	case *SandboxSnapshotRecord:
		return r == nil
	case *CreationIntentRecord:
		return r == nil
	case *CreationRequestRecord:
		return r == nil
	case *SandboxPlacementRecord:
		return r == nil
	default:
		return record == nil
	}
}
func encodeDomainRecord(record interface{ Validate() error }) (string, error) {
	limit := domainRecordLimit(record)
	if limit == 0 || nilDomainRecord(record) {
		return "", fmt.Errorf("%w: unsupported or nil domain record", ErrInvalidRecord)
	}
	// Own dispatch payload bytes before validation and encoding, preserving their
	// representation without retaining the caller's RawMessage backing array.
	switch r := record.(type) {
	case RuntimePublicationProofRecord:
		r.Payload = append(json.RawMessage(nil), r.Payload...)
		record = r
	case *RuntimePublicationProofRecord:
		owned := *r
		owned.Payload = append(json.RawMessage(nil), r.Payload...)
		record = owned
	case RuntimeBindingCertificateRecord:
		r.Payload = append(json.RawMessage(nil), r.Payload...)
		record = r
	case *RuntimeBindingCertificateRecord:
		owned := *r
		owned.Payload = append(json.RawMessage(nil), r.Payload...)
		record = owned
	case RuntimeDispatchInputRecord:
		r.Payload = append(json.RawMessage(nil), r.Payload...)
		record = r
	case *RuntimeDispatchInputRecord:
		owned := *r
		owned.Payload = append(json.RawMessage(nil), r.Payload...)
		record = owned
	}
	if err := record.Validate(); err != nil {
		return "", err
	}
	// Disable HTML escaping: escaped payload bytes would invalidate its digest.
	var value bytes.Buffer
	encoder := json.NewEncoder(&value)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(record); err != nil {
		return "", fmt.Errorf("%w: invalid record JSON", ErrInvalidRecord)
	}
	encoded := bytes.TrimSuffix(value.Bytes(), []byte("\n"))
	if len(encoded) > limit {
		return "", fmt.Errorf("%w: domain record exceeds size limit", ErrInvalidRecord)
	}
	return string(encoded), nil
}
func decodeDomainRecord(kv *mvccpb.KeyValue, record interface{ Validate() error }) error {
	switch destination := record.(type) {
	case *RuntimePublicationRecord:
		return decodePreparationDomain(kv, destination, 4096)
	case *RuntimePublicationProofRecord:
		return decodePreparationDomain(kv, destination, 16384)
	case *RuntimeBindingRecord:
		return decodePreparationDomain(kv, destination, 4096)
	case *RuntimeBindingCertificateRecord:
		return decodePreparationDomain(kv, destination, 8192)
	case *RuntimeIndexRecord:
		return decodePreparationDomain(kv, destination, 4096)
	case *RuntimeMountIntentRecord:
		return decodePreparationDomain(kv, destination, 4096)
	case *RuntimeDispatchRecord:
		return decodeTypedDomain(kv, destination, 4096)
	case *RuntimeDispatchInputRecord:
		return decodeTypedDomain(kv, destination, 65536)
	case *WorkspaceOwnerRecord:
		return decodeTypedDomain(kv, destination, 4096)
	case *WorkspaceFenceRecord:
		return decodeTypedDomain(kv, destination, 1024)
	case *SandboxControlRecord:
		return decodeTypedDomain(kv, destination, 4096)
	case *SandboxSnapshotRecord:
		return decodeTypedDomain(kv, destination, 65536)
	case *CreationIntentRecord:
		return decodeTypedDomain(kv, destination, 4096)
	case *CreationRequestRecord:
		return decodeTypedDomain(kv, destination, 4096)
	case *SandboxPlacementRecord:
		return decodeTypedDomain(kv, destination, 4096)
	default:
		return fmt.Errorf("%w: unsupported domain record destination", ErrCorruptRecord)
	}
}
func decodeTypedDomain[T interface{ Validate() error }](kv *mvccpb.KeyValue, destination *T, limit int) error {
	if kv == nil || kv.Lease != 0 || destination == nil || len(kv.Value) > limit || !utf8.Valid(kv.Value) {
		return fmt.Errorf("%w: invalid domain record envelope", ErrCorruptRecord)
	}
	// Omitted fields cannot inherit an old value, and failed decodes are atomic.
	var fresh T
	decoder := json.NewDecoder(bytes.NewReader(kv.Value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fresh); err != nil {
		return fmt.Errorf("%w: invalid domain JSON", ErrCorruptRecord)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%w: trailing domain JSON", ErrCorruptRecord)
	}
	if err := fresh.Validate(); err != nil {
		return fmt.Errorf("%w: invalid domain record fields", ErrCorruptRecord)
	}
	*destination = fresh
	return nil
}
func compactSnapshot(payload json.RawMessage) ([]byte, error) {
	if len(payload) > 65536 {
		return nil, fmt.Errorf("%w: snapshot JSON exceeds size limit", ErrInvalidRecord)
	}
	if !utf8.Valid(payload) {
		return nil, fmt.Errorf("%w: invalid snapshot UTF-8", ErrInvalidRecord)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, payload); err != nil {
		return nil, fmt.Errorf("%w: invalid snapshot JSON", ErrInvalidRecord)
	}
	return compact.Bytes(), nil
}
func snapshotDigest(payload json.RawMessage) (string, error) {
	compact, err := compactSnapshot(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(compact)
	return hex.EncodeToString(digest[:]), nil
}

// SandboxPlacementRecord routes ID-only reads and reserves IDs across partitions.
type SandboxPlacementRecord struct {
	Version       uint32 `json:"version"`
	SandboxID     string `json:"sandbox_id"`
	WorkspaceHash string `json:"workspace_hash"`
	IntentID      string `json:"intent_id"`
	RestoreEpoch  string `json:"restore_epoch"`
	Partition     uint8  `json:"partition"`
	Generation    int64  `json:"generation"`
}

func (r SandboxPlacementRecord) Validate() error {
	if !validOwnership(r.Version, r.WorkspaceHash, r.SandboxID, r.IntentID, r.RestoreEpoch, r.Generation) {
		return fmt.Errorf("%w: invalid sandbox placement", ErrInvalidRecord)
	}
	digest, _ := hex.DecodeString(r.WorkspaceHash)
	if r.Partition != digest[0] {
		return fmt.Errorf("%w: placement partition mismatch", ErrInvalidRecord)
	}
	return nil
}

func decodePreparationDomain[T interface{ Validate() error }](kv *mvccpb.KeyValue, dst *T, limit int) error {
	if kv == nil || len(kv.Value) > limit || !utf8.Valid(kv.Value) {
		return ErrCorruptRecord
	}
	if err := strictPreparationMetadata(kv.Value, reflect.TypeOf(dst)); err != nil {
		return err
	}
	return decodeTypedDomain(kv, dst, limit)
}
