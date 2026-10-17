package controlprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// MaxTaskRuntimeTerminationIntentBytes bounds one canonical termination intent
// to 16 KiB, including at most 4096 bytes of opaque terminal proof.
const MaxTaskRuntimeTerminationIntentBytes = 16384

// RecordStamp is a writer-derived pin of one fixed native record. It cannot
// supply a key or a comparator and is never sufficient authority on its own.
type RecordStamp struct {
	CreateRevision int64  `json:"create_revision"`
	ModRevision    int64  `json:"mod_revision"`
	SHA256         string `json:"sha256"`
}

// TerminationQuiescenceStage pins the original durable drain attempt and its
// stage digest; it does not authorize a replacement attempt.
type TerminationQuiescenceStage struct {
	RequestID string `json:"request_id"`
	StageID   string `json:"stage_id"`
	AttemptID string `json:"attempt_id"`
	Digest    string `json:"digest"`
}

// TerminationAuthorityBindings contains comparable record pins shared by both
// intent variants. Only local v1 uses Archive and LocalComplete; detached v2
// requires their exact zero values and carries separate effect bindings.
type TerminationAuthorityBindings struct {
	Task                  RecordStamp                `json:"task"`
	CleanupIntent         RecordStamp                `json:"cleanup_intent"`
	TaskLink              RecordStamp                `json:"task_link"`
	Placement             RecordStamp                `json:"placement"`
	Control               RecordStamp                `json:"control"`
	Owner                 RecordStamp                `json:"owner"`
	Fence                 RecordStamp                `json:"fence"`
	RuntimeIndex          RecordStamp                `json:"runtime_index"`
	Quiescence            RecordStamp                `json:"quiescence"`
	QuiescenceReceipt     RecordStamp                `json:"quiescence_receipt"`
	QuiescenceReservation RecordStamp                `json:"quiescence_reservation"`
	FinalSync             RecordStamp                `json:"final_sync"`
	Issuer                RecordStamp                `json:"issuer"`
	Archive               RecordStamp                `json:"archive"`
	LocalComplete         RecordStamp                `json:"local_complete"`
	QuiescenceStage       TerminationQuiescenceStage `json:"quiescence_stage"`
}

// DetachedRuntimeEffectBindings pins the detached enrolled plain profile's
// historical no-effect settlement. These copied wire values are not authority:
// a consumer must authenticate the persisted settlement, original Task/Claim/Run
// and all runtime, contract and source fences before relying on the profile.
type DetachedRuntimeEffectBindings struct {
	// Version is 1 for this tuple schema, independently of intent version 2.
	Version uint32 `json:"version"`
	// Profile is exactly detached_enrolled_plain; it does not enable a consumer.
	Profile string `json:"profile"`
	// ContractDigest is the lowercase SHA-256 pin of the enrolled plain contract.
	ContractDigest string `json:"contract_digest"`
	// Settlement may predate a renewed claim. Historical attribution belongs to
	// the native authority reader, rather than a wire revision ordering rule.
	Settlement RecordStamp `json:"settlement"`
}

// TaskRuntimeTerminationIntent pins a local v1 or detached v2 termination record.
// Accepting canonical wire does not authenticate its pins or enable a consumer.
type TaskRuntimeTerminationIntent struct {
	Version                   uint32                       `json:"version"`
	Phase                     string                       `json:"phase"`
	Current                   TaskCloseDataContext         `json:"current"`
	Partition                 uint8                        `json:"partition"`
	TaskBirth                 int64                        `json:"task_birth"`
	CreationIntentID          string                       `json:"creation_intent_id"`
	MountAttempt              uint8                        `json:"mount_attempt"`
	EnrollmentDigest          string                       `json:"enrollment_digest"`
	ObserverCertificateDigest string                       `json:"observer_certificate_digest"`
	NodeUID                   string                       `json:"node_uid"`
	NodeBootID                string                       `json:"node_boot_id"`
	Action                    string                       `json:"action"`
	Authority                 TerminationAuthorityBindings `json:"authority"`
	TerminalProof             []byte                       `json:"terminal_proof,omitempty"`
	CleanupDigest             string                       `json:"cleanup_digest,omitempty"`

	// DetachedEffects is absent in v1 to preserve its canonical bytes and is
	// mandatory in v2, where Archive and LocalComplete must remain exactly zero.
	DetachedEffects *DetachedRuntimeEffectBindings `json:"detached_effects,omitempty"`
}

func validateTerminationIntent(c TaskRuntimeTerminationIntent) error {
	if err := validateTaskCloseContext(c.Current); err != nil {
		return err
	}
	if c.TaskBirth <= 0 || c.TaskBirth > c.Current.ClaimCreateRevision || !validUUID(c.CreationIntentID) || c.MountAttempt > 1 || !validHash(c.EnrollmentDigest) || !validHash(c.ObserverCertificateDigest) || !validUUID(c.NodeUID) || !validUUID(c.NodeBootID) || c.Action != "kill_pid1_and_observe_cgroup_empty" {
		return fmt.Errorf("invalid runtime termination intent")
	}
	if len(c.Current.Namespace) > 512 {
		return fmt.Errorf("termination namespace exceeds bound")
	}
	hash, _ := hex.DecodeString(c.Current.WorkspaceHash)
	if len(hash) != sha256.Size || hash[0] != c.Partition {
		return fmt.Errorf("termination partition mismatch")
	}

	a := c.Authority
	stamps := []RecordStamp{a.Task, a.CleanupIntent, a.TaskLink, a.Placement, a.Control, a.Owner, a.Fence, a.RuntimeIndex, a.Quiescence, a.QuiescenceReceipt, a.QuiescenceReservation, a.FinalSync, a.Issuer}
	// Each version selects one complete effect profile. Zero local stamps are
	// meaningful only alongside the explicit detached settlement tuple.
	switch c.Version {
	case 1:
		if c.DetachedEffects != nil {
			return fmt.Errorf("local runtime termination cannot contain detached effects")
		}
		stamps = append(stamps, a.Archive, a.LocalComplete)
	case 2:
		d := c.DetachedEffects
		if c.MountAttempt != 0 || a.Archive != (RecordStamp{}) || a.LocalComplete != (RecordStamp{}) || d == nil {
			return fmt.Errorf("invalid detached runtime termination effect profile")
		}
		if d.Version != 1 || d.Profile != "detached_enrolled_plain" || !validHash(d.ContractDigest) {
			return fmt.Errorf("invalid detached runtime termination effect bindings")
		}
		stamps = append(stamps, d.Settlement)
	default:
		return fmt.Errorf("invalid runtime termination intent version")
	}

	for _, s := range stamps {
		if s.CreateRevision <= 0 || s.ModRevision < s.CreateRevision || !validHash(s.SHA256) {
			return fmt.Errorf("invalid runtime termination record stamp")
		}
	}

	s := a.QuiescenceStage
	if !validID(s.RequestID) || !validID(s.StageID) || !validUUID(s.AttemptID) || !validHash(s.Digest) {
		return fmt.Errorf("invalid runtime termination quiescence stage")
	}

	switch c.Phase {
	case "reserved":
		if len(c.TerminalProof) != 0 || c.CleanupDigest != "" {
			return fmt.Errorf("reserved termination cannot contain proof or cleanup")
		}
	case "terminated", "cleanup-complete":
		if len(c.TerminalProof) == 0 || len(c.TerminalProof) > 4096 {
			return fmt.Errorf("termination proof missing or exceeds bound")
		}
		if (c.Phase == "cleanup-complete" && !validHash(c.CleanupDigest)) || (c.Phase == "terminated" && c.CleanupDigest != "") {
			return fmt.Errorf("invalid termination cleanup phase")
		}
	default:
		return fmt.Errorf("invalid runtime termination phase")
	}
	return nil
}

// EncodeTaskRuntimeTerminationIntent validates a fixed record. A terminal proof
// is bounded opaque evidence here; its observer signature must be authenticated
// by the consumer before accepting a terminal phase.
func EncodeTaskRuntimeTerminationIntent(c TaskRuntimeTerminationIntent) ([]byte, error) {
	if err := validateTerminationIntent(c); err != nil {
		return nil, err
	}
	return encodeWire(c, MaxTaskRuntimeTerminationIntentBytes)
}

// DecodeTaskRuntimeTerminationIntent accepts precisely the encoder's canonical
// bytes. Byte equality also rejects duplicate/missing fields, nulls, noncanonical
// numbers, whitespace and invalid escaped Unicode without losing that evidence.
func DecodeTaskRuntimeTerminationIntent(wire []byte) (TaskRuntimeTerminationIntent, error) {
	var c TaskRuntimeTerminationIntent
	if len(wire) == 0 || len(wire) > MaxTaskRuntimeTerminationIntentBytes {
		return c, fmt.Errorf("invalid runtime termination intent size")
	}
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return TaskRuntimeTerminationIntent{}, err
	}
	canonical, err := EncodeTaskRuntimeTerminationIntent(c)
	if err != nil || !bytes.Equal(canonical, wire) {
		return TaskRuntimeTerminationIntent{}, fmt.Errorf("invalid or noncanonical runtime termination intent")
	}
	return c, nil
}

// TaskRuntimeTerminationKeys names the complete fixed point-read set. No key is
// accepted from an incoming ticket or returned as a generic comparator program.
type TaskRuntimeTerminationKeys struct {
	Identity, Restore, Intent, LocalComplete, Claim, Guard string
	Task, CleanupIntent, TaskLink, Placement, Control      string
	Owner, Fence, RuntimeIndex, Quiescence                 string
	QuiescenceReceipt, QuiescenceReservation, FinalSync    string
	Issuer, Archive, OperationsPrefix                      string

	// Detached source keys are populated only by the explicit version 2 entry.
	// Snapshot is derived later from the authenticated native Task, not a ticket.
	Settlement, CreationIntent, Publication string
}

func terminationRuntimeIndexDigest(uid string) string {
	h := sha256.New()
	var size [8]byte
	for _, s := range []string{"runtime-uid:v1", uid} {
		binary.BigEndian.PutUint64(size[:], uint64(len(s)))
		h.Write(size[:])
		h.Write([]byte(s))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// TaskRuntimeTerminationKeysFor derives the fixed local v1 native read set.
// Detached v2 remains rejected here: consumers must explicitly select its fixed
// native entry rather than treating local Archive/LocalComplete as settlement.
func TaskRuntimeTerminationKeysFor(c TaskRuntimeTerminationIntent) (TaskRuntimeTerminationKeys, error) {
	if c.Version != 1 {
		return TaskRuntimeTerminationKeys{}, fmt.Errorf("runtime termination native keys require local version 1")
	}
	if err := validateTerminationIntent(c); err != nil {
		return TaskRuntimeTerminationKeys{}, err
	}
	k := taskRuntimeTerminationCommonKeys(c)
	t := c.Current.Namespace + fmt.Sprintf("p/%02x/tasks/%s", c.Partition, c.Current.TaskID)
	k.Archive, k.LocalComplete = t+"/workspace-archive-command", t+"/workspace-sync-local-complete"
	return validateTaskRuntimeTerminationKeys(c.Current.Namespace, k)
}

// DetachedTaskRuntimeTerminationKeysFor derives only the fixed detached v2 read
// set after validating its complete effect variant. Keys and canonical wire are
// locators, never settlement, source, Claim or runtime termination authority.
func DetachedTaskRuntimeTerminationKeysFor(c TaskRuntimeTerminationIntent) (TaskRuntimeTerminationKeys, error) {
	if c.Version != 2 {
		return TaskRuntimeTerminationKeys{}, fmt.Errorf("detached runtime termination native keys require version 2")
	}
	if err := validateTerminationIntent(c); err != nil {
		return TaskRuntimeTerminationKeys{}, err
	}
	k := taskRuntimeTerminationCommonKeys(c)
	partition := c.Current.Namespace + fmt.Sprintf("p/%02x/", c.Partition)
	k.Settlement = partition + "tasks/" + c.Current.TaskID + "/detached-noeffect"
	k.CreationIntent = partition + "intents/" + c.CreationIntentID
	k.Publication = k.CreationIntent + "/runtime-publication"
	return validateTaskRuntimeTerminationKeys(c.Current.Namespace, k)
}

// Only validated variants reach this common derivation. Effect keys are added
// by their strict wrapper so an unused profile always has exact empty fields.
func taskRuntimeTerminationCommonKeys(c TaskRuntimeTerminationIntent) TaskRuntimeTerminationKeys {
	n := c.Current.Namespace
	p := n + fmt.Sprintf("p/%02x/", c.Partition)
	t := p + "tasks/" + c.Current.TaskID
	s := c.Authority.QuiescenceStage
	return TaskRuntimeTerminationKeys{
		Identity: n + "meta/identity", Restore: n + "meta/restore_epoch",
		Intent: t + "/runtime-termination", Claim: t + "/claim", Guard: t + "/guards/" + c.Current.ClaimID,
		Task: t, CleanupIntent: p + "cleanup-intents/" + c.Current.TaskID, TaskLink: p + "sandboxes/" + c.Current.SandboxID + "/cleanup-task",
		Placement: n + "indexes/sandbox/" + c.Current.SandboxID, Control: p + "controls/" + c.Current.SandboxID,
		Owner: p + "workspaces/" + c.Current.WorkspaceHash + "/owner", Fence: p + "workspaces/" + c.Current.WorkspaceHash + "/fence",
		RuntimeIndex: n + "indexes/runtime/" + terminationRuntimeIndexDigest(c.Current.Runtime.UID), Quiescence: t + "/quiesce-users",
		QuiescenceReceipt: p + "stages/" + s.RequestID + "/" + s.StageID + "/" + s.AttemptID + "/receipt", QuiescenceReservation: t + "/quiesce-users-attempt", FinalSync: t + "/final-sync-attempt",
		Issuer: n + "command-issuers/" + c.Current.IssuerCertificateID, OperationsPrefix: p + "sandboxes/" + c.Current.SandboxID + "/operations/",
	}
}

func validateTaskRuntimeTerminationKeys(n string, k TaskRuntimeTerminationKeys) (TaskRuntimeTerminationKeys, error) {
	for _, key := range []string{k.Identity, k.Restore, k.Intent, k.Claim, k.Guard, k.Task, k.CleanupIntent, k.TaskLink, k.Placement, k.Control, k.Owner, k.Fence, k.RuntimeIndex, k.Quiescence, k.QuiescenceReceipt, k.QuiescenceReservation, k.FinalSync, k.Issuer, k.OperationsPrefix} {
		if len(key) > 1024 || !strings.HasPrefix(key, n) {
			return TaskRuntimeTerminationKeys{}, fmt.Errorf("invalid runtime termination key bound")
		}
	}
	for _, key := range []string{k.Archive, k.LocalComplete, k.Settlement, k.CreationIntent, k.Publication} {
		if key != "" && (len(key) > 1024 || !strings.HasPrefix(key, n)) {
			return TaskRuntimeTerminationKeys{}, fmt.Errorf("invalid runtime termination effect key bound")
		}
	}
	return k, nil
}

// EncodeTaskRuntimeTerminationClaim constructs only the existing fixed native
// taskClaimRecord wire. It cannot reconstruct a TaskClaim capability.
func EncodeTaskRuntimeTerminationClaim(c TaskCloseDataContext, partition uint8) ([]byte, error) {
	if err := validateTaskCloseContext(c); err != nil {
		return nil, err
	}
	hash, _ := hex.DecodeString(c.WorkspaceHash)
	if len(hash) != sha256.Size || hash[0] != partition {
		return nil, fmt.Errorf("termination claim partition mismatch")
	}
	type taskReference struct {
		Namespace    string `json:"namespace"`
		RestoreEpoch string `json:"restore_epoch"`
		TaskID       string `json:"task_id"`
		SandboxID    string `json:"sandbox_id"`
		Partition    uint8  `json:"partition"`
	}
	return encodeWire(struct {
		Version  uint32        `json:"version"`
		Task     taskReference `json:"task"`
		ClaimID  string        `json:"claim_id"`
		WorkerID string        `json:"worker_id"`
		LeaseID  int64         `json:"lease_id"`
	}{1, taskReference{c.Namespace, c.RestoreEpoch, c.TaskID, c.SandboxID, partition}, c.ClaimID, c.WorkerID, c.LeaseID}, 4096)
}
