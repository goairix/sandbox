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

const MaxTaskRuntimeTerminationIntentBytes = 16384

// RecordStamp is a writer-derived pin of one fixed native record. It cannot
// supply a key or a comparator and is never sufficient authority on its own.
type RecordStamp struct {
	CreateRevision int64  `json:"create_revision"`
	ModRevision    int64  `json:"mod_revision"`
	SHA256         string `json:"sha256"`
}

type TerminationQuiescenceStage struct {
	RequestID string `json:"request_id"`
	StageID   string `json:"stage_id"`
	AttemptID string `json:"attempt_id"`
	Digest    string `json:"digest"`
}

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
}

func validateTerminationIntent(c TaskRuntimeTerminationIntent) error {
	if err := validateTaskCloseContext(c.Current); err != nil {
		return err
	}
	if c.Version != 1 || c.TaskBirth <= 0 || c.TaskBirth > c.Current.ClaimCreateRevision || !validUUID(c.CreationIntentID) || c.MountAttempt > 1 || !validHash(c.EnrollmentDigest) || !validHash(c.ObserverCertificateDigest) || !validUUID(c.NodeUID) || !validUUID(c.NodeBootID) || c.Action != "kill_pid1_and_observe_cgroup_empty" {
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
	for _, s := range []RecordStamp{a.Task, a.CleanupIntent, a.TaskLink, a.Placement, a.Control, a.Owner, a.Fence, a.RuntimeIndex, a.Quiescence, a.QuiescenceReceipt, a.QuiescenceReservation, a.FinalSync, a.Issuer, a.Archive, a.LocalComplete} {
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

func TaskRuntimeTerminationKeysFor(c TaskRuntimeTerminationIntent) (TaskRuntimeTerminationKeys, error) {
	if err := validateTerminationIntent(c); err != nil {
		return TaskRuntimeTerminationKeys{}, err
	}
	n := c.Current.Namespace
	p := n + fmt.Sprintf("p/%02x/", c.Partition)
	t := p + "tasks/" + c.Current.TaskID
	s := c.Authority.QuiescenceStage
	k := TaskRuntimeTerminationKeys{
		Identity: n + "meta/identity", Restore: n + "meta/restore_epoch",
		Intent: t + "/runtime-termination", LocalComplete: t + "/workspace-sync-local-complete", Claim: t + "/claim", Guard: t + "/guards/" + c.Current.ClaimID,
		Task: t, CleanupIntent: p + "cleanup-intents/" + c.Current.TaskID, TaskLink: p + "sandboxes/" + c.Current.SandboxID + "/cleanup-task",
		Placement: n + "indexes/sandbox/" + c.Current.SandboxID, Control: p + "controls/" + c.Current.SandboxID,
		Owner: p + "workspaces/" + c.Current.WorkspaceHash + "/owner", Fence: p + "workspaces/" + c.Current.WorkspaceHash + "/fence",
		RuntimeIndex: n + "indexes/runtime/" + terminationRuntimeIndexDigest(c.Current.Runtime.UID), Quiescence: t + "/quiesce-users",
		QuiescenceReceipt: p + "stages/" + s.RequestID + "/" + s.StageID + "/" + s.AttemptID + "/receipt", QuiescenceReservation: t + "/quiesce-users-attempt", FinalSync: t + "/final-sync-attempt",
		Issuer: n + "command-issuers/" + c.Current.IssuerCertificateID, Archive: t + "/workspace-archive-command", OperationsPrefix: p + "sandboxes/" + c.Current.SandboxID + "/operations/",
	}
	for _, key := range []string{k.Identity, k.Restore, k.Intent, k.LocalComplete, k.Claim, k.Guard, k.Task, k.CleanupIntent, k.TaskLink, k.Placement, k.Control, k.Owner, k.Fence, k.RuntimeIndex, k.Quiescence, k.QuiescenceReceipt, k.QuiescenceReservation, k.FinalSync, k.Issuer, k.Archive, k.OperationsPrefix} {
		if len(key) > 1024 || !strings.HasPrefix(key, n) {
			return TaskRuntimeTerminationKeys{}, fmt.Errorf("invalid runtime termination key bound")
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
