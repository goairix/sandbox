package nodeobserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
	"unicode"
	"unicode/utf8"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// This is a temporary, call-owned authenticated native graph. The permanent
// fact comes from the controlled Task3 create-once writer, which owns full E/S
// semantics and dual-drain receipt verification. A canonical DTO or a key's
// presence is insufficient: this reader verifies its actual immutable envelope,
// exact source stamps, signed plain publication and historical q/none bindings.
// Current Claim/Guard/TTL and the final native guard remain independent gates.
type detachedAuthoritySnapshot struct {
	record     p.DetachedNoEffectSettlement
	settlement *mvccpb.KeyValue
	source     [3]*mvccpb.KeyValue // snapshot, creation intent, publication
}

func permanentAuthorityPoint(kv *mvccpb.KeyValue) bool {
	return kv != nil && kv.Lease == 0 && kv.Version == 1 && kv.CreateRevision > 0 && kv.ModRevision == kv.CreateRevision
}

func authorityPointStamp(kv *mvccpb.KeyValue) p.RecordStamp {
	return p.RecordStamp{CreateRevision: kv.CreateRevision, ModRevision: kv.ModRevision, SHA256: digest(kv.Value)}
}

// CreationIntent is the one source whose original pending key is updated to
// published. Only its fixed native point may retain create < mod. The actual
// envelope must remain permanent and coherent, and every create/mod/hash field
// must equal the settlement's pin; source semantics are checked independently.
func detachedCreationIntentPointMatches(kv *mvccpb.KeyValue, stamp p.RecordStamp) bool {
	if kv == nil || kv.Lease != 0 || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.Version <= 0 || (kv.Version == 1) != (kv.ModRevision == kv.CreateRevision) || kv.Version > kv.ModRevision-kv.CreateRevision+1 {
		return false
	}
	return authorityPointStamp(kv) == stamp
}

func authorityHash(value string) bool {
	wire, err := hex.DecodeString(value)
	return err == nil && len(wire) == sha256.Size && hex.EncodeToString(wire) == value
}

func authoritySegment(value string) bool {
	if len(value) == 0 || len(value) > 128 || value == "." || value == ".." {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func authorityUUID(value string) bool {
	u, err := uuid.Parse(value)
	return err == nil && u.String() == value
}

// Only the fields changed by a later legal command/Claim are normalized. All
// domain, issuer, Task and runtime/gate attribution remains exactly equal.
func sameDetachedContext(old, current p.TaskCloseDataContext) bool {
	old.CommandID, old.ClaimID, old.WorkerID = current.CommandID, current.ClaimID, current.WorkerID
	old.ClaimCreateRevision, old.LeaseID = current.ClaimCreateRevision, current.LeaseID
	return old == current
}

func detachedClaimBefore(c p.DetachedNoEffectClaim, current p.TaskCloseDataContext) bool {
	if c.CreateRevision > current.ClaimCreateRevision {
		return false
	}
	return c.CreateRevision < current.ClaimCreateRevision || c.ClaimID == current.ClaimID && c.WorkerID == current.WorkerID && c.LeaseID == current.LeaseID
}

func (r *authorityReader) loadDetachedSettlement(ctx context.Context, s *authoritySnapshot) error {
	points, err := r.points(ctx, s, []string{s.keys.Settlement}, []int{p.MaxDetachedNoEffectSettlementBytes})
	if err != nil {
		return err
	}
	kv, intent := points[0], s.record
	fact, err := p.DecodeDetachedNoEffectSettlement(kv.Value)
	if err != nil || !permanentAuthorityPoint(kv) || authorityPointStamp(kv) != intent.DetachedEffects.Settlement {
		return fmt.Errorf("observer detached settlement envelope/stamp invalid: %v", err)
	}
	a := intent.Authority
	if fact.ContractDigest != intent.DetachedEffects.ContractDigest || fact.TaskBirth != intent.TaskBirth || fact.CreationIntentID != intent.CreationIntentID || fact.Partition != intent.Partition || !sameDetachedContext(fact.QuiescenceContext, intent.Current) || !detachedClaimBefore(fact.WriterClaim, intent.Current) || fact.Task != a.Task || fact.Quiescence != a.Quiescence || fact.QuiescenceReceipt != a.QuiescenceReceipt || fact.QuiescenceReservation != a.QuiescenceReservation || fact.FinalSync != a.FinalSync || fact.QuiescenceStage != a.QuiescenceStage || kv.CreateRevision <= fact.WriterClaim.CreateRevision || kv.CreateRevision <= fact.Execution.ModRevision || kv.CreateRevision < fact.EmptyRevision {
		return fmt.Errorf("observer detached settlement does not bind reserved intent/history")
	}
	for _, stamp := range []p.RecordStamp{fact.Task, fact.Snapshot, fact.Publication, fact.Quiescence, fact.QuiescenceReceipt, fact.QuiescenceReservation, fact.FinalSync, fact.Occupation} {
		if stamp.CreateRevision != stamp.ModRevision || kv.CreateRevision <= stamp.ModRevision {
			return fmt.Errorf("observer detached settlement predates immutable source")
		}
	}
	// The published intent's mutation must also precede this create-once fact;
	// its actual mutable envelope is checked at the fixed source read below.
	if kv.CreateRevision <= fact.CreationIntent.ModRevision {
		return fmt.Errorf("observer detached settlement predates published creation intent")
	}
	s.detached = &detachedAuthoritySnapshot{record: fact, settlement: kv}
	return nil
}

type detachedSnapshotRecord struct {
	Version      uint32              `json:"version"`
	SandboxID    string              `json:"sandbox_id"`
	Snapshot     p.SnapshotReference `json:"snapshot"`
	Payload      json.RawMessage     `json:"payload"`
	LocalCleanup *json.RawMessage    `json:"local_cleanup,omitempty"`
}

type detachedCreationIntent struct {
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

// This mirrors the fixed store publication schema, not a general store API.
// Digests authenticate proof/enrollment/activation bytes under the source pin;
// the root-signed publication verifier independently authenticates their graph.
type detachedPublicationRecord struct {
	Version                   uint32                  `json:"version"`
	IntentID                  string                  `json:"intent_id"`
	SandboxID                 string                  `json:"sandbox_id"`
	WorkspaceHash             string                  `json:"workspace_hash"`
	RestoreEpoch              string                  `json:"restore_epoch"`
	Generation                int64                   `json:"generation"`
	DataGateEpoch             int64                   `json:"data_gate_epoch"`
	Snapshot                  p.SnapshotReference     `json:"snapshot"`
	ExpiresAt                 time.Time               `json:"expires_at"`
	OperationID               string                  `json:"operation_id"`
	PayloadDigest             string                  `json:"payload_digest"`
	ProofDigest               string                  `json:"proof_digest"`
	CertificateDigest         string                  `json:"certificate_digest"`
	MountOperationID          string                  `json:"mount_operation_id"`
	Runtime                   p.RuntimeReference      `json:"runtime"`
	MountAttempt              uint8                   `json:"mount_attempt"`
	Claim                     p.DetachedNoEffectClaim `json:"claim"`
	Attempt                   authorityStage          `json:"attempt"`
	EnrollmentDigest          string                  `json:"enrollment_digest"`
	ObserverCertificateDigest string                  `json:"observer_certificate_digest"`
	ActivationDigest          string                  `json:"activation_digest"`
	NodeUID                   string                  `json:"node_uid"`
	NodeBootID                string                  `json:"node_boot_id"`
	EnrollmentRecordDigest    string                  `json:"enrollment_record_digest"`
	ActivationRecordDigest    string                  `json:"activation_record_digest"`
	DispatchAttempt           authorityStage          `json:"dispatch_attempt"`
	BindingAttempt            authorityStage          `json:"binding_attempt"`
	MountAttemptLocator       authorityStage          `json:"mount_attempt_locator"`
}

type detachedProofRecord struct {
	Version       uint32          `json:"version"`
	IntentID      string          `json:"intent_id"`
	SandboxID     string          `json:"sandbox_id"`
	WorkspaceHash string          `json:"workspace_hash"`
	RestoreEpoch  string          `json:"restore_epoch"`
	ProofDigest   string          `json:"proof_digest"`
	Generation    int64           `json:"generation"`
	Payload       json.RawMessage `json:"payload"`
}

type detachedEnrollmentRecord struct {
	Version             uint32             `json:"version"`
	IntentID            string             `json:"intent_id"`
	SandboxID           string             `json:"sandbox_id"`
	WorkspaceHash       string             `json:"workspace_hash"`
	RestoreEpoch        string             `json:"restore_epoch"`
	Generation          int64              `json:"generation"`
	Runtime             p.RuntimeReference `json:"runtime"`
	ObserverCertificate json.RawMessage    `json:"observer_certificate"`
	Receipt             json.RawMessage    `json:"receipt"`
}

type detachedActivationRecord struct {
	Version            uint32             `json:"version"`
	IntentID           string             `json:"intent_id"`
	SandboxID          string             `json:"sandbox_id"`
	WorkspaceHash      string             `json:"workspace_hash"`
	RestoreEpoch       string             `json:"restore_epoch"`
	Generation         int64              `json:"generation"`
	Runtime            p.RuntimeReference `json:"runtime"`
	Activation         json.RawMessage    `json:"activation"`
	RuntimeCertificate json.RawMessage    `json:"runtime_certificate"`
	IssuerCertificate  json.RawMessage    `json:"issuer_certificate"`
}

// Enrolled records use the store's json.Marshal encoding, while snapshots and
// q records preserve payload bytes with HTML escaping disabled. Both decoders
// reject unknown/missing/duplicate fields by exact canonical reconstruction.
func canonicalEnrolledNative(wire []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(canonical, wire) {
		return fmt.Errorf("noncanonical observer enrolled native record")
	}
	return nil
}

func detachedWorkspaceHash(identity []byte, requestHash string) (string, error) {
	var operator struct {
		StorageID string `json:"storage_id"`
	}
	if json.Unmarshal(identity, &operator) != nil || !authorityHash(requestHash) || len(operator.StorageID) == 0 || len(operator.StorageID) > 128 || !utf8.ValidString(operator.StorageID) {
		return "", fmt.Errorf("observer detached storage/request binding invalid")
	}
	for _, c := range operator.StorageID {
		if unicode.IsControl(c) {
			return "", fmt.Errorf("observer detached storage identity invalid")
		}
	}
	storage := sha256.Sum256([]byte(operator.StorageID))
	h := sha256.New()
	var size [8]byte
	for _, value := range []string{"sandbox.detached-identity.v1", hex.EncodeToString(storage[:]), requestHash} {
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		h.Write(size[:])
		h.Write([]byte(value))
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (r *authorityReader) authenticateDetachedSource(ctx context.Context, s *authoritySnapshot, enrolled authorityEnrollment, issuer []byte) error {
	d, c := s.detached, s.record.Current
	if r.publication == nil || enrolled.ContractDigest != d.record.ContractDigest {
		return fmt.Errorf("observer detached verified activation contract mismatch")
	}
	var task authorityTaskRecord
	if canonicalNative(s.domain[0].Value, &task) != nil || !authoritySegment(task.Snapshot.Version) {
		return fmt.Errorf("observer detached native Task snapshot locator invalid")
	}
	// Snapshot's version comes from the already authenticated native Task.
	partition := fmt.Sprintf("%sp/%02x/", c.Namespace, s.record.Partition)
	keys := []string{partition + "snapshots/" + c.SandboxID + "/" + task.Snapshot.Version, s.keys.CreationIntent, s.keys.Publication}
	points, err := r.points(ctx, s, keys, []int{65536, 4096, 8192})
	if err != nil {
		return err
	}
	for i, stamp := range []p.RecordStamp{d.record.Snapshot, d.record.CreationIntent, d.record.Publication} {
		matches := permanentAuthorityPoint(points[i]) && authorityPointStamp(points[i]) == stamp
		if i == 1 {
			matches = detachedCreationIntentPointMatches(points[i], stamp)
		}
		if !matches {
			return fmt.Errorf("observer detached native source stamp mismatch")
		}
	}
	var snapshot detachedSnapshotRecord
	var intent detachedCreationIntent
	var publication detachedPublicationRecord
	if canonicalNative(points[0].Value, &snapshot) != nil || canonicalNative(points[1].Value, &intent) != nil || canonicalEnrolledNative(points[2].Value, &publication) != nil {
		return fmt.Errorf("observer detached source is not canonical")
	}
	workspace, err := detachedWorkspaceHash(s.identity.Value, intent.RequestHash)
	if err != nil || workspace != c.WorkspaceHash || snapshot.Version != 1 || snapshot.LocalCleanup != nil || snapshot.SandboxID != c.SandboxID || snapshot.Snapshot != task.Snapshot || digest(snapshot.Payload) != snapshot.Snapshot.Digest || intent.Version != 1 || intent.Phase != "published" || intent.IntentID != s.record.CreationIntentID || intent.SandboxID != c.SandboxID || intent.WorkspaceHash != c.WorkspaceHash || intent.RestoreEpoch != c.RestoreEpoch || intent.Generation != c.Generation || !authorityHash(intent.ConfigurationDigest) {
		return fmt.Errorf("observer detached native source identity/profile mismatch")
	}
	if err := r.authenticateDetachedPublication(ctx, s, publication, enrolled); err != nil {
		return err
	}
	if err := r.authenticateDetachedQuiescence(s, task, issuer); err != nil {
		return err
	}
	copy(d.source[:], points)
	return nil
}

func (r *authorityReader) authenticateDetachedPublication(ctx context.Context, s *authoritySnapshot, publication detachedPublicationRecord, enrolled authorityEnrollment) error {
	c, p := s.record.Current, publication
	if !validDetachedPublicationMetadata(s, p) || p.IntentID != s.record.CreationIntentID || p.SandboxID != c.SandboxID || p.WorkspaceHash != c.WorkspaceHash || p.RestoreEpoch != c.RestoreEpoch || p.Generation != c.Generation || p.DataGateEpoch != c.DataGateEpoch || p.Runtime != c.Runtime || p.MountAttempt != 0 || p.MountOperationID != "" || p.EnrollmentDigest != s.record.EnrollmentDigest || p.ObserverCertificateDigest != s.record.ObserverCertificateDigest || p.NodeUID != s.record.NodeUID || p.NodeBootID != s.record.NodeBootID {
		return fmt.Errorf("observer detached native publication attribution mismatch")
	}
	var task authorityTaskRecord
	if canonicalNative(s.domain[0].Value, &task) != nil || p.Snapshot != task.Snapshot {
		return fmt.Errorf("observer detached publication snapshot mismatch")
	}
	root := s.keys.CreationIntent
	keys := []string{root + "/publication-proof", root + "/runtime-enrollment", root + "/runtime-enrollment-activation"}
	points, err := r.points(ctx, s, keys, []int{16384, 8192, 8192})
	if err != nil {
		return err
	}
	for _, point := range points {
		if !permanentAuthorityPoint(point) || point.CreateRevision != s.detached.record.Publication.CreateRevision {
			return fmt.Errorf("observer detached signed publication graph birth mismatch")
		}
	}
	var proof detachedProofRecord
	var enrollment detachedEnrollmentRecord
	var activation detachedActivationRecord
	if canonicalEnrolledNative(points[0].Value, &proof) != nil || canonicalEnrolledNative(points[1].Value, &enrollment) != nil || canonicalEnrolledNative(points[2].Value, &activation) != nil || proof.Version != 2 || enrollment.Version != 2 || activation.Version != 2 || digest(proof.Payload) != p.ProofDigest || proof.ProofDigest != p.ProofDigest || digest(points[1].Value) != p.EnrollmentRecordDigest || digest(points[2].Value) != p.ActivationRecordDigest {
		return fmt.Errorf("observer detached publication graph digest/schema mismatch")
	}
	for _, same := range []bool{
		proof.IntentID == p.IntentID && proof.SandboxID == p.SandboxID && proof.WorkspaceHash == p.WorkspaceHash && proof.RestoreEpoch == p.RestoreEpoch && proof.Generation == p.Generation,
		enrollment.IntentID == p.IntentID && enrollment.SandboxID == p.SandboxID && enrollment.WorkspaceHash == p.WorkspaceHash && enrollment.RestoreEpoch == p.RestoreEpoch && enrollment.Generation == p.Generation && enrollment.Runtime == p.Runtime,
		activation.IntentID == p.IntentID && activation.SandboxID == p.SandboxID && activation.WorkspaceHash == p.WorkspaceHash && activation.RestoreEpoch == p.RestoreEpoch && activation.Generation == p.Generation && activation.Runtime == p.Runtime,
	} {
		if !same {
			return fmt.Errorf("observer detached signed source graph identity mismatch")
		}
	}
	evidence := detachedPublicationEvidence(enrollment, activation)
	context := detachedPublicationContext(s, publication)
	verified, err := r.publication.VerifyEnrolledPublicationHistorical(proof.Payload, context, evidence, enrolled.ContractDigest)
	if err != nil || verified.Digest() != publication.ProofDigest || verified.Ready().EnrollmentDigest != enrolled.EnrollmentDigest || verified.Ready().ObserverCertificateDigest != enrolled.ObserverCertificateDigest || verified.Activation().Digest() != publication.ActivationDigest || verified.EnrollmentReceipt().NodeUID != enrolled.NodeUID || verified.EnrollmentReceipt().NodeBootID != enrolled.NodeBootID {
		return fmt.Errorf("observer detached signed plain publication invalid: %v", err)
	}
	// Historical q/close signatures below must use these exact verified runtime
	// certificate bytes. They are retained only for this call's authentication.
	return r.authenticateDetachedHistoricalTickets(s, evidence.Request.RuntimeCertificate)
}

// Store metadata outside the signed payload still has a closed schema and
// fixed stage attribution. It cannot widen the three fixed proof point keys.
func validDetachedPublicationMetadata(s *authoritySnapshot, r detachedPublicationRecord) bool {
	if !(r.Version == 2 && r.Attempt.StageID == "runtime_publish_enrolled_v2" || r.Version == 3 && r.Attempt.StageID == "runtime_publish_enrolled_v3") || !authorityUUID(r.OperationID) || !authorityUUID(r.Claim.ClaimID) || !authoritySegment(r.Claim.WorkerID) || r.Claim.CreateRevision <= 0 || r.Claim.LeaseID <= 0 || !authorityHash(r.PayloadDigest) || !authorityHash(r.ProofDigest) || !authorityHash(r.CertificateDigest) || !authorityHash(r.ActivationDigest) || !authorityHash(r.EnrollmentRecordDigest) || !authorityHash(r.ActivationRecordDigest) {
		return false
	}
	for _, expected := range []struct {
		attempt authorityStage
		stage   string
	}{{r.Attempt, r.Attempt.StageID}, {r.DispatchAttempt, "runtime_dispatch"}, {r.BindingAttempt, "runtime_bind"}, {r.MountAttemptLocator, "runtime_mount"}} {
		a := expected.attempt
		if a.Namespace != s.record.Current.Namespace || a.Partition != s.record.Partition || a.RestoreEpoch != s.record.Current.RestoreEpoch || a.StageID != expected.stage || !authorityUUID(a.RequestID) || !authorityUUID(a.AttemptID) {
			return false
		}
	}
	return true
}

func detachedPublicationEvidence(enrollment detachedEnrollmentRecord, activation detachedActivationRecord) p.EnrolledRuntimeEvidence {
	return p.EnrolledRuntimeEvidence{Request: p.NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", RuntimeCertificate: activation.RuntimeCertificate, Activation: activation.Activation, IssuerCertificate: activation.IssuerCertificate}, ObserverCertificate: enrollment.ObserverCertificate, Receipt: enrollment.Receipt}
}

func detachedPublicationContext(s *authoritySnapshot, publication detachedPublicationRecord) p.PublicationContext {
	c := s.record.Current
	return p.PublicationContext{Certificate: p.CertificateContext{Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, IntentID: publication.IntentID, SandboxID: publication.SandboxID, WorkspaceHash: publication.WorkspaceHash, Generation: publication.Generation, OperationID: publication.OperationID, PayloadDigest: publication.PayloadDigest, Snapshot: publication.Snapshot, ExpiresAt: publication.ExpiresAt}, Runtime: publication.Runtime, CertificateDigest: publication.CertificateDigest, Claim: p.ClaimReference{ClaimID: publication.Claim.ClaimID, CreateRevision: publication.Claim.CreateRevision, LeaseID: publication.Claim.LeaseID}, DataGateEpoch: publication.DataGateEpoch}
}
