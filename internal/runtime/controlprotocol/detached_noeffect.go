package controlprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// MaxDetachedNoEffectSettlementBytes bounds canonical settlement JSON to 32 KiB.
// It includes the base64 encoding of at most 4096 original execution bytes and
// the fully escaped historical context. Native request limits remain separate.
const MaxDetachedNoEffectSettlementBytes = 32 << 10

// DetachedNoEffectClaim is historical Claim attribution, never a live Claim or
// permission to replace the original attempt after its lease has disappeared.
type DetachedNoEffectClaim struct {
	ClaimID        string `json:"claim_id"`
	WorkerID       string `json:"worker_id"`
	CreateRevision int64  `json:"create_revision"`
	LeaseID        int64  `json:"lease_id"`
}

// DetachedNoEffectSettlement records one detached/plain settlement's bindings.
// Decoding authenticates no native KV, signature, original job, Kernel or Run.
// Consumers must independently authenticate those sources before using it.
type DetachedNoEffectSettlement struct {
	Version uint32 `json:"version"`
	Domain  string `json:"domain"`
	Profile string `json:"profile"`

	// QuiescenceContext is the actual historical quiescence command context.
	// Its Claim may precede both FinalSyncClaim and a later WriterClaim.
	QuiescenceContext TaskCloseDataContext `json:"quiescence_context"`
	Partition         uint8                `json:"partition"`
	TaskBirth         int64                `json:"task_birth"`
	CreationIntentID  string               `json:"creation_intent_id"`
	Incarnation       string               `json:"incarnation"`
	RunID             string               `json:"run_id"`
	Slot              uint32               `json:"slot"`
	WorkerDigest      string               `json:"worker_digest"`

	WriterClaim        DetachedNoEffectClaim `json:"writer_claim"`
	FinalSyncClaim     DetachedNoEffectClaim `json:"final_sync_claim"`
	FinalSyncAttemptID string                `json:"final_sync_attempt_id"`
	AttemptID          string                `json:"attempt_id"`
	BindingDigest      string                `json:"binding_digest"`
	ContractDigest     string                `json:"contract_digest"`
	ReceiptDigest      string                `json:"receipt_digest"`
	// EmptyRevision is the fresh whole-prefix empty Range's native revision.
	// It is an observation, not a reusable absence-of-effect permission.
	EmptyRevision int64 `json:"empty_revision"`

	Task                  RecordStamp                `json:"task"`
	Snapshot              RecordStamp                `json:"snapshot"`
	CreationIntent        RecordStamp                `json:"creation_intent"`
	Publication           RecordStamp                `json:"publication"`
	Quiescence            RecordStamp                `json:"quiescence"`
	QuiescenceReceipt     RecordStamp                `json:"quiescence_receipt"`
	QuiescenceReservation RecordStamp                `json:"quiescence_reservation"`
	FinalSync             RecordStamp                `json:"final_sync"`
	Execution             RecordStamp                `json:"execution"`
	Occupation            RecordStamp                `json:"occupation"`
	QuiescenceStage       TerminationQuiescenceStage `json:"quiescence_stage"`

	// ExecutionValue owns the exact original E bytes after Pending ACK. Its
	// schema and native birth/lease/Run semantics belong to the native reader.
	ExecutionValue []byte `json:"execution_value"`
}

// The explicit projection excludes Pending's containing E value/stamp and the
// binding digest itself. Progress can therefore persist this digest before the
// final E stamp is known, without either a hash cycle or an omitted source pin.
type detachedNoEffectBinding struct {
	Version               uint32                     `json:"version"`
	Domain                string                     `json:"domain"`
	Profile               string                     `json:"profile"`
	QuiescenceContext     TaskCloseDataContext       `json:"quiescence_context"`
	Partition             uint8                      `json:"partition"`
	TaskBirth             int64                      `json:"task_birth"`
	CreationIntentID      string                     `json:"creation_intent_id"`
	Incarnation           string                     `json:"incarnation"`
	RunID                 string                     `json:"run_id"`
	Slot                  uint32                     `json:"slot"`
	WorkerDigest          string                     `json:"worker_digest"`
	WriterClaim           DetachedNoEffectClaim      `json:"writer_claim"`
	FinalSyncClaim        DetachedNoEffectClaim      `json:"final_sync_claim"`
	FinalSyncAttemptID    string                     `json:"final_sync_attempt_id"`
	AttemptID             string                     `json:"attempt_id"`
	ContractDigest        string                     `json:"contract_digest"`
	ReceiptDigest         string                     `json:"receipt_digest"`
	EmptyRevision         int64                      `json:"empty_revision"`
	Task                  RecordStamp                `json:"task"`
	Snapshot              RecordStamp                `json:"snapshot"`
	CreationIntent        RecordStamp                `json:"creation_intent"`
	Publication           RecordStamp                `json:"publication"`
	Quiescence            RecordStamp                `json:"quiescence"`
	QuiescenceReceipt     RecordStamp                `json:"quiescence_receipt"`
	QuiescenceReservation RecordStamp                `json:"quiescence_reservation"`
	FinalSync             RecordStamp                `json:"final_sync"`
	Occupation            RecordStamp                `json:"occupation"`
	QuiescenceStage       TerminationQuiescenceStage `json:"quiescence_stage"`
}

// DetachedNoEffectBindingDigest computes pure attribution for the fixed
// domain and explicit binding projection. It excludes BindingDigest, Execution
// and ExecutionValue so an original writer can persist Pending before E's ACK.
// It deliberately does not validate or grant authority, including for invalid
// diagnostic inputs; Encode validates the completed settlement independently.
func DetachedNoEffectBindingDigest(r DetachedNoEffectSettlement) (string, error) {
	bound := detachedNoEffectBinding{
		Version: r.Version, Domain: r.Domain, Profile: r.Profile,
		QuiescenceContext: r.QuiescenceContext, Partition: r.Partition,
		TaskBirth: r.TaskBirth, CreationIntentID: r.CreationIntentID,
		Incarnation: r.Incarnation, RunID: r.RunID, Slot: r.Slot, WorkerDigest: r.WorkerDigest,
		WriterClaim: r.WriterClaim, FinalSyncClaim: r.FinalSyncClaim,
		FinalSyncAttemptID: r.FinalSyncAttemptID, AttemptID: r.AttemptID,
		ContractDigest: r.ContractDigest, ReceiptDigest: r.ReceiptDigest, EmptyRevision: r.EmptyRevision,
		Task: r.Task, Snapshot: r.Snapshot, CreationIntent: r.CreationIntent, Publication: r.Publication,
		Quiescence: r.Quiescence, QuiescenceReceipt: r.QuiescenceReceipt,
		QuiescenceReservation: r.QuiescenceReservation, FinalSync: r.FinalSync,
		Occupation: r.Occupation, QuiescenceStage: r.QuiescenceStage,
	}
	wire, err := json.Marshal(bound)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write([]byte("sandbox.detached-noeffect-binding.v1\x00"))
	h.Write(wire)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func detachedNoEffectUUID(raw string, version uuid.Version) bool {
	u, err := uuid.Parse(raw)
	return err == nil && u.String() == raw && u.Variant() == uuid.RFC4122 && u.Version() == version
}

func validDetachedNoEffectClaim(c DetachedNoEffectClaim) bool {
	return detachedNoEffectUUID(c.ClaimID, 4) && validID(c.WorkerID) && c.CreateRevision > 0 && c.LeaseID > 0
}

func validateDetachedNoEffectSettlement(r DetachedNoEffectSettlement) error {
	if r.Version != 1 || r.Domain != "sandbox.detached-noeffect.v1" || r.Profile != "detached_enrolled_plain" || validateTaskCloseContext(r.QuiescenceContext) != nil || !validUUID(r.CreationIntentID) || !detachedNoEffectUUID(r.Incarnation, 5) || !detachedNoEffectUUID(r.RunID, 4) || r.Slot >= 32 || !validHash(r.WorkerDigest) || !validHash(r.ContractDigest) || !validHash(r.ReceiptDigest) || r.EmptyRevision <= 0 {
		return fmt.Errorf("invalid detached no-effect settlement identity")
	}
	q := r.QuiescenceContext
	hash, _ := hex.DecodeString(q.WorkspaceHash)
	if len(hash) != sha256.Size || hash[0] != r.Partition || r.TaskBirth <= 0 || r.TaskBirth != q.ControlRevision || r.Task.CreateRevision != r.TaskBirth || r.Task.ModRevision != r.TaskBirth || r.Task.SHA256 != q.TaskDigest {
		return fmt.Errorf("invalid detached no-effect Task birth")
	}
	if !validDetachedNoEffectClaim(r.WriterClaim) || !validDetachedNoEffectClaim(r.FinalSyncClaim) || !detachedNoEffectUUID(r.FinalSyncAttemptID, 4) || !detachedNoEffectUUID(r.AttemptID, 4) || r.FinalSyncClaim.CreateRevision < q.ClaimCreateRevision || r.WriterClaim.CreateRevision < r.FinalSyncClaim.CreateRevision || r.WriterClaim.CreateRevision <= r.TaskBirth {
		return fmt.Errorf("invalid detached no-effect Claim history")
	}
	for _, stamp := range []RecordStamp{r.Task, r.Snapshot, r.CreationIntent, r.Publication, r.Quiescence, r.QuiescenceReceipt, r.QuiescenceReservation, r.FinalSync, r.Execution, r.Occupation} {
		if stamp.CreateRevision <= 0 || stamp.ModRevision < stamp.CreateRevision || !validHash(stamp.SHA256) {
			return fmt.Errorf("invalid detached no-effect record stamp")
		}
	}
	s := r.QuiescenceStage
	if !validID(s.RequestID) || !validID(s.StageID) || !validUUID(s.AttemptID) || !validHash(s.Digest) || r.Quiescence.CreateRevision != r.QuiescenceReceipt.CreateRevision || r.Execution.CreateRevision <= r.TaskBirth || r.Occupation.CreateRevision <= r.Execution.CreateRevision || r.Execution.ModRevision < r.Occupation.ModRevision {
		return fmt.Errorf("invalid detached no-effect execution history")
	}
	if len(r.ExecutionValue) == 0 || len(r.ExecutionValue) > 4096 || !json.Valid(r.ExecutionValue) {
		return fmt.Errorf("invalid detached no-effect execution bytes")
	}
	executionHash := sha256.Sum256(r.ExecutionValue)
	if hex.EncodeToString(executionHash[:]) != r.Execution.SHA256 {
		return fmt.Errorf("detached no-effect execution digest mismatch")
	}
	binding, err := DetachedNoEffectBindingDigest(r)
	if err != nil || !validHash(r.BindingDigest) || binding != r.BindingDigest {
		return fmt.Errorf("detached no-effect binding digest mismatch")
	}
	return nil
}

// EncodeDetachedNoEffectSettlement validates and encodes the fixed wire tuple.
// Its E bytes are bounded JSON with a matching hash; only the native reader can
// validate their execution schema, actual birth/lease and original Run owner.
func EncodeDetachedNoEffectSettlement(r DetachedNoEffectSettlement) ([]byte, error) {
	if err := validateDetachedNoEffectSettlement(r); err != nil {
		return nil, err
	}
	return encodeWire(r, MaxDetachedNoEffectSettlementBytes)
}

// DecodeDetachedNoEffectSettlement accepts only the encoder's exact canonical
// bytes, rejecting missing/duplicate/unknown/null fields and alternate encoding.
// The returned owned values remain diagnostic until native history is verified.
func DecodeDetachedNoEffectSettlement(wire []byte) (DetachedNoEffectSettlement, error) {
	var r DetachedNoEffectSettlement
	if len(wire) == 0 || len(wire) > MaxDetachedNoEffectSettlementBytes {
		return r, fmt.Errorf("invalid detached no-effect settlement size")
	}
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return DetachedNoEffectSettlement{}, err
	}
	canonical, err := EncodeDetachedNoEffectSettlement(r)
	if err != nil || !bytes.Equal(canonical, wire) {
		return DetachedNoEffectSettlement{}, fmt.Errorf("invalid or noncanonical detached no-effect settlement")
	}
	return r, nil
}
