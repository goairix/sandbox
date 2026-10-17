package etcd

import (
	"fmt"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// The pending attribution contains no mutation or permission. Four KiB bounds
// every read and CAS failure reply independently of runtime-history size.
const coldQuotaAttemptMaxBytes = 4096

// coldRuntimeQuotaAttemptRecord version 1 binds one original release and quota
// owner to an exact pre-Grant StageReference. It remains pending after commit:
// only the immutable return and its receipt prove completion. Replacing an
// aborted attempt preserves the key's original create revision; history GC is
// a separate maintenance concern, not part of this settlement protocol.
type coldRuntimeQuotaAttemptRecord struct {
	Version       int               `json:"version"`
	Domain        string            `json:"domain"`
	Namespace     string            `json:"namespace"`
	RestoreEpoch  string            `json:"restore_epoch"`
	ProfileDigest string            `json:"profile_digest"`
	Owner         ColdSlotOwner     `json:"owner"`
	Quota         ColdSlotReference `json:"quota"`
	Task          TaskReference     `json:"task"`
	TaskBirth     int64             `json:"task_birth"`
	ReleaseStamp  p.RecordStamp     `json:"release_stamp"`
	Reference     StageReference    `json:"reference"`
}

func (n Namespace) coldRuntimeQuotaAttemptKey(partition uint8, intent string) (string, error) {
	return n.Key("p", fmt.Sprintf("%02x", partition), "intents", intent, "cold-runtime-quota-attempt")
}

func (b *Backend) coldQuotaAttemptRecord(prepared *coldQuotaReturnPrepared) coldRuntimeQuotaAttemptRecord {
	h := prepared.history
	return coldRuntimeQuotaAttemptRecord{Version: 1, Domain: "sandbox.cold-runtime-quota-attempt.v1", Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Owner: h.reservation.Owner, Quota: h.reservation.Quota, Task: h.release.record.Task, TaskBirth: h.release.record.TaskBirth, ReleaseStamp: localRuntimeStamp(h.release.kv), Reference: prepared.reference}
}

func encodeColdRuntimeQuotaAttempt(r coldRuntimeQuotaAttemptRecord) (string, error) {
	ref, owner := r.Reference, r.Owner
	locator := StageAttemptLocator{Namespace: ref.Namespace, Partition: ref.Partition, RequestID: ref.RequestID, StageID: ref.StageID, AttemptID: ref.AttemptID, RestoreEpoch: ref.RestoreEpoch}
	if r.Version != 1 || r.Domain != "sandbox.cold-runtime-quota-attempt.v1" || locator.Validate() != nil || ref.StageID != "cold_runtime_quota_return" || !validHexDigest(ref.Digest) || ref.Namespace != r.Namespace || ref.RestoreEpoch != r.RestoreEpoch || ref.Partition != owner.Partition || ref.RequestID != owner.RequestID || !validHexDigest(r.ProfileDigest) {
		return "", ErrInvalidRecord
	}
	if !validDomainSegment(owner.IntentID) || !validDomainSegment(owner.SandboxID) || !validHexDigest(owner.WorkspaceHash) || owner.Generation <= 0 || r.Quota.Kind != "quota" || r.Quota.ID >= 1000000 || r.Quota.Epoch == 0 {
		return "", ErrInvalidRecord
	}
	if r.Task.Validate() != nil || r.Task.Namespace != r.Namespace || r.Task.RestoreEpoch != r.RestoreEpoch || r.Task.Partition != owner.Partition || r.Task.SandboxID != owner.SandboxID || r.TaskBirth <= 0 || !validColdQuotaStamp(r.ReleaseStamp) || r.ReleaseStamp.CreateRevision != r.ReleaseStamp.ModRevision || r.TaskBirth >= r.ReleaseStamp.CreateRevision {
		return "", ErrInvalidRecord
	}
	wire, err := coldEncode(r)
	if err != nil || len(wire) > coldQuotaAttemptMaxBytes {
		return "", ErrInvalidRecord
	}
	return wire, nil
}

// decodeColdRuntimeQuotaAttempt accepts mutable permanent KV envelopes, but
// requires canonical version-1 bytes and every attribution field from already
// authenticated history. The retained reference only selects arbitration; it
// cannot recreate a prepared mutation or authorize another continuation.
func (b *Backend) decodeColdRuntimeQuotaAttempt(h *coldQuotaReturnHistory, kv *mvccpb.KeyValue) (coldRuntimeQuotaAttemptRecord, error) {
	var r coldRuntimeQuotaAttemptRecord
	if b == nil || h == nil || h.release == nil || h.release.kv == nil || kv == nil || kv.Version <= 0 || len(kv.Value) > coldQuotaAttemptMaxBytes || coldDecode(kv, &r) != nil {
		return r, ErrCorruptRecord
	}
	wire, err := encodeColdRuntimeQuotaAttempt(r)
	key, keyErr := b.namespace.coldRuntimeQuotaAttemptKey(h.reservation.Owner.Partition, h.reservation.Owner.IntentID)
	expected := b.coldQuotaAttemptRecord(&coldQuotaReturnPrepared{history: h, reference: r.Reference})
	if err != nil || keyErr != nil || wire != string(kv.Value) || string(kv.Key) != key || r != expected || kv.CreateRevision <= r.ReleaseStamp.ModRevision || (kv.Version == 1) != (kv.CreateRevision == kv.ModRevision) {
		return r, ErrCorruptRecord
	}
	return r, nil
}
