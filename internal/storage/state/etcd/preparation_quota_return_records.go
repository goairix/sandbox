package etcd

import (
	"fmt"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

type ColdRuntimeQuotaReturnResult struct {
	Outcome   Outcome
	Reference StageReference
	Entry     *ColdRuntimeQuotaReturnEntry
}

// ColdRuntimeQuotaReturnEntry is copied historical metadata, never authority
// for runtime termination, provider cleanup, or another quota mutation.
type ColdRuntimeQuotaReturnEntry struct {
	Task           TaskReference
	TaskBirth      int64
	Owner          ColdSlotOwner
	Quota          ColdSlotReference
	Runtime        RuntimeReference
	NodeUID        string
	NodeBootID     string
	CreateRevision int64
	SHA256         string
}

type coldRuntimeQuotaReturnRecord struct {
	Version               int                     `json:"version"`
	Domain                string                  `json:"domain"`
	Namespace             string                  `json:"namespace"`
	RestoreEpoch          string                  `json:"restore_epoch"`
	ProfileDigest         string                  `json:"profile_digest"`
	Owner                 ColdSlotOwner           `json:"owner"`
	Quota                 ColdSlotReference       `json:"quota"`
	Preparation           ColdSlotReference       `json:"preparation"`
	Runtime               RuntimeReference        `json:"runtime"`
	NodeUID               string                  `json:"node_uid"`
	NodeBootID            string                  `json:"node_boot_id"`
	DataGateEpoch         int64                   `json:"data_gate_epoch"`
	Publication           StageReference          `json:"publication"`
	PublicationStamp      p.RecordStamp           `json:"publication_stamp"`
	Reservation           p.RecordStamp           `json:"reservation"`
	PreparationWork       p.RecordStamp           `json:"preparation_work"`
	PreparationSettlement StageReference          `json:"preparation_settlement"`
	Release               localOwnerReleaseRecord `json:"release"`
	ReleaseStamp          p.RecordStamp           `json:"release_stamp"`
	Attempt               StageAttemptLocator     `json:"attempt"`
}

func (n Namespace) coldRuntimeQuotaReturnKey(partition uint8, intent string) (string, error) {
	return n.Key("p", fmt.Sprintf("%02x", partition), "intents", intent, "cold-runtime-quota-return")
}

func validColdQuotaStamp(s p.RecordStamp) bool {
	return s.CreateRevision > 0 && s.ModRevision >= s.CreateRevision && validHexDigest(s.SHA256)
}

func encodeColdRuntimeQuotaReturn(r coldRuntimeQuotaReturnRecord) (string, error) {
	a, owner, release := r.Attempt, r.Owner, r.Release
	if r.Version != 1 || r.Domain != "sandbox.cold-runtime-quota-return.v1" || a.Validate() != nil || a.StageID != "cold_runtime_quota_return" || a.Namespace != r.Namespace || a.RestoreEpoch != r.RestoreEpoch || a.Partition != owner.Partition || a.RequestID != owner.RequestID || !validHexDigest(r.ProfileDigest) || !validDomainSegment(owner.IntentID) || !validDomainSegment(owner.SandboxID) || !validHexDigest(owner.WorkspaceHash) || owner.Generation <= 0 || r.DataGateEpoch <= 0 || r.Runtime.Validate() != nil || !validPreparationUUID(r.NodeUID) || !validPreparationUUID(r.NodeBootID) {
		return "", ErrInvalidRecord
	}
	if _, err := encodeLocalOwnerRelease(release); err != nil || release.Task.Namespace != r.Namespace || release.Task.RestoreEpoch != r.RestoreEpoch || release.Task.Partition != owner.Partition || release.Task.SandboxID != owner.SandboxID || release.WorkspaceHash != owner.WorkspaceHash || release.Generation != owner.Generation || release.Runtime != r.Runtime || release.NodeUID != r.NodeUID || release.NodeBootID != r.NodeBootID {
		return "", ErrInvalidRecord
	}
	if r.Quota.Kind != "quota" || r.Quota.ID >= 1000000 || r.Quota.Epoch == 0 || r.Preparation.Kind != "preparation" || r.Preparation.Partition != 0 || r.Preparation.ID >= 32 || r.Preparation.Epoch == 0 {
		return "", ErrInvalidRecord
	}
	for _, item := range []struct {
		ref   StageReference
		stage string
	}{{r.Publication, enrolledPublicationStage}, {r.PreparationSettlement, "cold_preparation_settle"}} {
		ref := item.ref
		locator := StageAttemptLocator{Namespace: ref.Namespace, RestoreEpoch: ref.RestoreEpoch, Partition: ref.Partition, RequestID: ref.RequestID, StageID: ref.StageID, AttemptID: ref.AttemptID}
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
	if r.PublicationStamp.CreateRevision != r.PublicationStamp.ModRevision || r.ReleaseStamp.CreateRevision != r.ReleaseStamp.ModRevision || r.Reservation.ModRevision != r.PreparationWork.ModRevision || r.Reservation.ModRevision <= r.PublicationStamp.CreateRevision || r.ReleaseStamp.CreateRevision <= release.Termination.ModRevision || r.ReleaseStamp.CreateRevision <= release.Claim.CreateRevision {
		return "", ErrInvalidRecord
	}
	return coldEncode(r)
}

func decodeColdRuntimeQuotaReturn(kv *mvccpb.KeyValue) (coldRuntimeQuotaReturnRecord, error) {
	var r coldRuntimeQuotaReturnRecord
	if !enrolledImmutableKV(kv) || coldDecode(kv, &r) != nil {
		return r, ErrCorruptRecord
	}
	wire, err := encodeColdRuntimeQuotaReturn(r)
	if err != nil || wire != string(kv.Value) || kv.CreateRevision <= r.PublicationStamp.CreateRevision || kv.CreateRevision <= r.Reservation.ModRevision || kv.CreateRevision <= r.ReleaseStamp.CreateRevision {
		return r, ErrCorruptRecord
	}
	return r, nil
}

func coldQuotaReturnEntry(r coldRuntimeQuotaReturnRecord, kv *mvccpb.KeyValue) *ColdRuntimeQuotaReturnEntry {
	return &ColdRuntimeQuotaReturnEntry{Task: r.Release.Task, TaskBirth: r.Release.TaskBirth, Owner: r.Owner, Quota: r.Quota, Runtime: r.Runtime, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID, CreateRevision: kv.CreateRevision, SHA256: workspaceSyncIntentDigest(string(kv.Value))}
}
