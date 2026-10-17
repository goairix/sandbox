package etcd

import (
	"encoding/json"
	"reflect"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// The cap counts the complete canonical JSON value, including the independent
// v2 release seal. It preserves the existing return-record transport allowance;
// the aggregate outer/nested maximum is verified together by codec tests.
const detachedColdRuntimeQuotaReturnBytes = 16 << 10

// This independent persisted variant carries historical attribution only. The
// v1 record and its bytes remain separate; in particular, no LocalComplete is
// invented for a detached release. A validated value is not a native proof or
// permission to grant a Stage, release a slot or perform a physical operation.
type detachedColdRuntimeQuotaReturnRecord struct {
	Version int    `json:"version"`
	Domain  string `json:"domain"`

	Namespace     string            `json:"namespace"`
	RestoreEpoch  string            `json:"restore_epoch"`
	ProfileDigest string            `json:"profile_digest"`
	Owner         ColdSlotOwner     `json:"owner"`
	Quota         ColdSlotReference `json:"quota"`
	Preparation   ColdSlotReference `json:"preparation"`

	Runtime               RuntimeReference `json:"runtime"`
	NodeUID               string           `json:"node_uid"`
	NodeBootID            string           `json:"node_boot_id"`
	DataGateEpoch         int64            `json:"data_gate_epoch"`
	Publication           StageReference   `json:"publication"`
	PublicationStamp      p.RecordStamp    `json:"publication_stamp"`
	Reservation           p.RecordStamp    `json:"reservation"`
	PreparationWork       p.RecordStamp    `json:"preparation_work"`
	PreparationSettlement StageReference   `json:"preparation_settlement"`

	Release      detachedOwnerReleaseRecord `json:"release"`
	ReleaseStamp p.RecordStamp              `json:"release_stamp"`
	Attempt      StageAttemptLocator        `json:"attempt"`
}

// Encoding validates only relationships expressible in this persisted value.
// Creation-intent attribution and authentic terminal/release history remain the
// responsibility of the future opaque proof consumer, never this pure codec.
func encodeDetachedColdRuntimeQuotaReturn(r detachedColdRuntimeQuotaReturnRecord) (string, error) {
	a, owner, release := r.Attempt, r.Owner, r.Release
	if r.Version != 2 || r.Domain != "sandbox.cold-runtime-quota-return.v2" || a.Validate() != nil || a.StageID != "cold_runtime_quota_return" {
		return "", ErrInvalidRecord
	}
	if a.Namespace != r.Namespace || a.RestoreEpoch != r.RestoreEpoch || a.Partition != owner.Partition || a.RequestID != owner.RequestID {
		return "", ErrInvalidRecord
	}
	if !validHexDigest(r.ProfileDigest) || !validDomainSegment(owner.IntentID) || !validDomainSegment(owner.SandboxID) || !validHexDigest(owner.WorkspaceHash) || owner.Generation <= 0 || r.DataGateEpoch <= 0 {
		return "", ErrInvalidRecord
	}
	if r.Runtime.Validate() != nil || !validPreparationUUID(r.NodeUID) || !validPreparationUUID(r.NodeBootID) {
		return "", ErrInvalidRecord
	}

	// The independent seal has no local Complete/archive surrogate. Its codec
	// validates exact detached delete keys; no v1 seal conversion is used.
	if _, err := encodeDetachedOwnerRelease(release); err != nil {
		return "", ErrInvalidRecord
	}
	if release.Task.Namespace != r.Namespace || release.Task.RestoreEpoch != r.RestoreEpoch || release.Task.Partition != owner.Partition || release.Task.SandboxID != owner.SandboxID {
		return "", ErrInvalidRecord
	}
	if release.WorkspaceHash != owner.WorkspaceHash || release.Generation != owner.Generation || release.Runtime != r.Runtime || release.NodeUID != r.NodeUID || release.NodeBootID != r.NodeBootID {
		return "", ErrInvalidRecord
	}

	// Quota partition is deliberately independent of the owner's partition,
	// matching the original cold-slot protocol; preparation is global slot0..31.
	if r.Quota.Kind != "quota" || r.Quota.ID >= 1000000 || r.Quota.Epoch == 0 || r.Preparation.Kind != "preparation" || r.Preparation.Partition != 0 || r.Preparation.ID >= 32 || r.Preparation.Epoch == 0 {
		return "", ErrInvalidRecord
	}
	for _, item := range []struct {
		ref   StageReference
		stage string
	}{{r.Publication, enrolledPublicationStage}, {r.PreparationSettlement, "cold_preparation_settle"}} {
		ref := item.ref
		locator := StageAttemptLocator{
			Namespace: ref.Namespace, RestoreEpoch: ref.RestoreEpoch, Partition: ref.Partition,
			RequestID: ref.RequestID, StageID: ref.StageID, AttemptID: ref.AttemptID,
		}
		stageMatches := ref.StageID == item.stage
		if item.stage == enrolledPublicationStage {
			stageMatches = supportedEnrolledPublicationStage(ref.StageID)
		}
		if locator.Validate() != nil || ref.Namespace != r.Namespace || ref.RestoreEpoch != r.RestoreEpoch || ref.Partition != owner.Partition || ref.RequestID != owner.RequestID || !stageMatches || !validHexDigest(ref.Digest) {
			return "", ErrInvalidRecord
		}
	}

	for _, stamp := range []p.RecordStamp{r.PublicationStamp, r.Reservation, r.PreparationWork, r.ReleaseStamp} {
		if !validColdQuotaStamp(stamp) {
			return "", ErrInvalidRecord
		}
	}
	if r.PublicationStamp.CreateRevision != r.PublicationStamp.ModRevision || r.ReleaseStamp.CreateRevision != r.ReleaseStamp.ModRevision || r.Reservation.ModRevision != r.PreparationWork.ModRevision {
		return "", ErrInvalidRecord
	}
	if r.Reservation.ModRevision <= r.PublicationStamp.CreateRevision || r.ReleaseStamp.CreateRevision <= release.Termination.ModRevision || r.ReleaseStamp.CreateRevision <= release.Claim.CreateRevision {
		return "", ErrInvalidRecord
	}

	wire, err := json.Marshal(r)
	if err != nil || len(wire) > detachedColdRuntimeQuotaReturnBytes {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}

// Value decoding has no MVCC envelope and can never authenticate a release or
// quota history. Typed field checking and re-encoding reject unknown, missing,
// duplicate, null or noncanonical fields throughout the independent schema.
func decodeDetachedColdRuntimeQuotaReturnValue(value []byte) (detachedColdRuntimeQuotaReturnRecord, error) {
	var record detachedColdRuntimeQuotaReturnRecord
	if len(value) > detachedColdRuntimeQuotaReturnBytes || strictPreparationMetadata(value, reflect.TypeOf(&record)) != nil || json.Unmarshal(value, &record) != nil {
		return detachedColdRuntimeQuotaReturnRecord{}, ErrCorruptRecord
	}
	wire, err := encodeDetachedColdRuntimeQuotaReturn(record)
	if err != nil || wire != string(value) {
		return detachedColdRuntimeQuotaReturnRecord{}, ErrCorruptRecord
	}
	return record, nil
}

// Native decoding additionally requires the original immutable envelope and
// temporal ordering. This structural envelope check is not source-history
// authentication; even a correctly formed KV is not an executable proof.
func decodeDetachedColdRuntimeQuotaReturn(kv *mvccpb.KeyValue) (detachedColdRuntimeQuotaReturnRecord, error) {
	if !enrolledImmutableKV(kv) {
		return detachedColdRuntimeQuotaReturnRecord{}, ErrCorruptRecord
	}
	record, err := decodeDetachedColdRuntimeQuotaReturnValue(kv.Value)
	if err != nil || kv.CreateRevision <= record.PublicationStamp.CreateRevision || kv.CreateRevision <= record.Reservation.ModRevision || kv.CreateRevision <= record.ReleaseStamp.CreateRevision {
		return detachedColdRuntimeQuotaReturnRecord{}, ErrCorruptRecord
	}
	return record, nil
}
