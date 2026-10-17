package etcd

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// Eight KiB includes the full fixed JSON schema: two root512 Task references,
// three opaque runtime128 values at worst six-byte JSON escaping, int64 widths
// and the exact derived delete keys (598/654/592 bytes). The bound is7643 bytes;
// allowing arbitrary1024-byte delete keys would invalidate that calculation.
const detachedOwnerReleaseBytes = 8 << 10

// This independent v2 seal contains historical attribution only. A reader
// must authenticate its original native history before it becomes a proof;
// neither these fields nor a caller-created value grant deletion authority.
type detachedOwnerReleaseRecord struct {
	Version         uint32                          `json:"version"`
	Domain          string                          `json:"domain"`
	Task            TaskReference                   `json:"task"`
	TaskBirth       int64                           `json:"task_birth"`
	Claim           taskCloseClaimWire              `json:"claim"`
	Runtime         RuntimeReference                `json:"runtime"`
	WorkspaceHash   string                          `json:"workspace_hash"`
	Generation      int64                           `json:"generation"`
	NodeUID         string                          `json:"node_uid"`
	NodeBootID      string                          `json:"node_boot_id"`
	Termination     p.RecordStamp                   `json:"termination"`
	CleanupDigest   string                          `json:"cleanup_digest"`
	DetachedEffects p.DetachedRuntimeEffectBindings `json:"detached_effects"`
	Deleted         [3]localOwnerReleasedKey        `json:"deleted"`
}

// Historical Current identifies the termination's original reservation. The
// releasing Claim may be newer, including newer than cleanup-complete itself;
// only the live caller can authenticate that Claim's current permission.
func newDetachedOwnerReleaseRecord(r p.TaskRuntimeTerminationIntent, birth, mod int64, claim taskCloseClaimWire) (detachedOwnerReleaseRecord, error) {
	if r.Version != 2 || r.DetachedEffects == nil || r.Phase != "cleanup-complete" || birth <= r.Current.ClaimCreateRevision || mod < birth || claim.Task != localRuntimeTerminationTask(r) || !validWorkspaceSyncFinalizationClaim(claim, claim.Task) || claim.CreateRevision < r.Current.ClaimCreateRevision {
		return detachedOwnerReleaseRecord{}, ErrInvalidRecord
	}
	wire, err := p.EncodeTaskRuntimeTerminationIntent(r)
	if err != nil {
		return detachedOwnerReleaseRecord{}, errors.Join(ErrInvalidRecord, err)
	}
	keys, err := p.DetachedTaskRuntimeTerminationKeysFor(r)
	if err != nil {
		return detachedOwnerReleaseRecord{}, errors.Join(ErrInvalidRecord, err)
	}

	seal := detachedOwnerReleaseRecord{
		Version: 2, Domain: "sandbox.task-owner-release.v2", Task: claim.Task, TaskBirth: r.TaskBirth, Claim: claim,
		Runtime: RuntimeReference(r.Current.Runtime), WorkspaceHash: r.Current.WorkspaceHash, Generation: r.Current.Generation, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID,
		Termination: p.RecordStamp{CreateRevision: birth, ModRevision: mod, SHA256: workspaceSyncIntentDigest(string(wire))}, CleanupDigest: r.CleanupDigest, DetachedEffects: *r.DetachedEffects,
		Deleted: [3]localOwnerReleasedKey{{keys.Owner, r.Authority.Owner}, {keys.Control, r.Authority.Control}, {keys.RuntimeIndex, r.Authority.RuntimeIndex}},
	}
	if _, err = encodeDetachedOwnerRelease(seal); err != nil {
		return detachedOwnerReleaseRecord{}, err
	}
	return seal, nil
}

func encodeDetachedOwnerRelease(r detachedOwnerReleaseRecord) (string, error) {
	if r.Version != 2 || r.Domain != "sandbox.task-owner-release.v2" || r.Task.Validate() != nil || r.TaskBirth <= 0 || !validWorkspaceSyncFinalizationClaim(r.Claim, r.Task) || r.Claim.CreateRevision <= r.TaskBirth || r.Runtime.Validate() != nil || !validHexDigest(r.WorkspaceHash) || hashPartition(r.WorkspaceHash) != r.Task.Partition || r.Generation <= 0 || !validPreparationUUID(r.NodeUID) || !validPreparationUUID(r.NodeBootID) || !validHexDigest(r.CleanupDigest) {
		return "", ErrInvalidRecord
	}
	effects := r.DetachedEffects
	if effects.Version != 1 || effects.Profile != "detached_enrolled_plain" || !validHexDigest(effects.ContractDigest) || r.Termination.CreateRevision <= r.TaskBirth {
		return "", ErrInvalidRecord
	}

	keys := detachedOwnerReleaseDeletedKeys(r)
	stamps := []p.RecordStamp{r.Termination, effects.Settlement}
	for i, deleted := range r.Deleted {
		if deleted.Key != keys[i] || len(deleted.Key) > maxNamespaceKeyBytes {
			return "", ErrInvalidRecord
		}
		stamps = append(stamps, deleted.Stamp)
	}
	for _, stamp := range stamps {
		if stamp.CreateRevision <= 0 || stamp.ModRevision < stamp.CreateRevision || !validHexDigest(stamp.SHA256) {
			return "", ErrInvalidRecord
		}
	}

	wire, err := json.Marshal(r)
	if err != nil || len(wire) > detachedOwnerReleaseBytes {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}

// Inputs have already passed their strict identity codecs. Derive each fixed
// key from those fields, never from an independently supplied deletion path.
// The UID digest is the same framed runtime-uid:v1 formula as runtimeIndexKey.
func detachedOwnerReleaseDeletedKeys(r detachedOwnerReleaseRecord) [3]string {
	partition := r.Task.Namespace + fmt.Sprintf("p/%02x/", r.Task.Partition)
	uid := framedDigest("runtime-uid:v1", r.Runtime.UID)
	return [3]string{
		partition + "workspaces/" + r.WorkspaceHash + "/owner",
		partition + "controls/" + r.Task.SandboxID,
		r.Task.Namespace + "indexes/runtime/" + hex.EncodeToString(uid[:]),
	}
}

func decodeDetachedOwnerRelease(kv *mvccpb.KeyValue) (detachedOwnerReleaseRecord, error) {
	var r detachedOwnerReleaseRecord
	if !enrolledImmutableKV(kv) || len(kv.Value) == 0 || len(kv.Value) > detachedOwnerReleaseBytes || json.Unmarshal(kv.Value, &r) != nil {
		return detachedOwnerReleaseRecord{}, ErrCorruptRecord
	}
	wire, err := encodeDetachedOwnerRelease(r)
	key := r.Task.Namespace + fmt.Sprintf("p/%02x/tasks/%s/owner-release", r.Task.Partition, r.Task.TaskID)
	if err != nil || wire != string(kv.Value) || string(kv.Key) != key || kv.CreateRevision <= r.Termination.ModRevision || kv.CreateRevision <= r.Claim.CreateRevision {
		return detachedOwnerReleaseRecord{}, errors.Join(ErrCorruptRecord, err)
	}
	return r, nil
}

// Matching historical bytes is necessary attribution, not signature/native
// authentication or permission to release with the historical sender's Claim.
func matchDetachedOwnerReleaseHistory(r detachedOwnerReleaseRecord, termination *mvccpb.KeyValue) error {
	if _, err := encodeDetachedOwnerRelease(r); err != nil {
		return errors.Join(ErrCorruptRecord, err)
	}
	intent, err := decodeLocalRuntimeTermination(termination)
	if err != nil {
		return errors.Join(ErrCorruptRecord, err)
	}
	expected, err := newDetachedOwnerReleaseRecord(intent, termination.CreateRevision, termination.ModRevision, r.Claim)
	if err != nil || expected != r {
		return errors.Join(ErrCorruptRecord, err)
	}
	key := r.Task.Namespace + fmt.Sprintf("p/%02x/tasks/%s/runtime-termination", r.Task.Partition, r.Task.TaskID)
	if string(termination.Key) != key {
		return ErrCorruptRecord
	}
	return nil
}
