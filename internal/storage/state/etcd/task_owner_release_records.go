package etcd

import (
	"encoding/json"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

type localOwnerReleasedKey struct {
	Key   string        `json:"key"`
	Stamp p.RecordStamp `json:"stamp"`
}

type localOwnerReleaseRecord struct {
	Version       uint32                   `json:"version"`
	Domain        string                   `json:"domain"`
	Task          TaskReference            `json:"task"`
	TaskBirth     int64                    `json:"task_birth"`
	Claim         taskCloseClaimWire       `json:"claim"`
	Runtime       RuntimeReference         `json:"runtime"`
	WorkspaceHash string                   `json:"workspace_hash"`
	Generation    int64                    `json:"generation"`
	NodeUID       string                   `json:"node_uid"`
	NodeBootID    string                   `json:"node_boot_id"`
	Complete      p.RecordStamp            `json:"complete"`
	Termination   p.RecordStamp            `json:"termination"`
	CleanupDigest string                   `json:"cleanup_digest"`
	Deleted       [3]localOwnerReleasedKey `json:"deleted"`
}

func newLocalOwnerReleaseRecord(r p.TaskRuntimeTerminationIntent, birth, mod int64, claim taskCloseClaimWire) (localOwnerReleaseRecord, error) {
	if r.Phase != "cleanup-complete" || birth <= r.Current.ClaimCreateRevision || mod < birth || claim.Task != localRuntimeTerminationTask(r) || !validWorkspaceSyncFinalizationClaim(claim, claim.Task) || claim.CreateRevision < r.Current.ClaimCreateRevision {
		return localOwnerReleaseRecord{}, ErrInvalidRecord
	}
	wire, err := p.EncodeTaskRuntimeTerminationIntent(r)
	if err != nil {
		return localOwnerReleaseRecord{}, err
	}
	keys, err := p.TaskRuntimeTerminationKeysFor(r)
	if err != nil {
		return localOwnerReleaseRecord{}, err
	}
	return localOwnerReleaseRecord{Version: 1, Domain: "sandbox.task-owner-release.v1", Task: claim.Task, TaskBirth: r.TaskBirth, Claim: claim, Runtime: RuntimeReference(r.Current.Runtime), WorkspaceHash: r.Current.WorkspaceHash, Generation: r.Current.Generation, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID, Complete: r.Authority.LocalComplete, Termination: p.RecordStamp{CreateRevision: birth, ModRevision: mod, SHA256: workspaceSyncIntentDigest(string(wire))}, CleanupDigest: r.CleanupDigest, Deleted: [3]localOwnerReleasedKey{{keys.Owner, r.Authority.Owner}, {keys.Control, r.Authority.Control}, {keys.RuntimeIndex, r.Authority.RuntimeIndex}}}, nil
}

func encodeLocalOwnerRelease(r localOwnerReleaseRecord) (string, error) {
	if r.Version != 1 || r.Domain != "sandbox.task-owner-release.v1" || r.Task.Validate() != nil || r.TaskBirth <= 0 || r.Claim.CreateRevision <= r.TaskBirth || !validWorkspaceSyncFinalizationClaim(r.Claim, r.Task) || r.Runtime.Validate() != nil || !validHexDigest(r.WorkspaceHash) || r.Generation <= 0 || !validPreparationUUID(r.NodeUID) || !validPreparationUUID(r.NodeBootID) || !validHexDigest(r.CleanupDigest) {
		return "", ErrInvalidRecord
	}
	stamps := []p.RecordStamp{r.Complete, r.Termination}
	for _, d := range r.Deleted {
		if len(d.Key) == 0 || len(d.Key) > 1024 {
			return "", ErrInvalidRecord
		}
		stamps = append(stamps, d.Stamp)
	}
	for _, s := range stamps {
		if s.CreateRevision <= 0 || s.ModRevision < s.CreateRevision || !validHexDigest(s.SHA256) {
			return "", ErrInvalidRecord
		}
	}
	wire, err := json.Marshal(r)
	if err != nil || len(wire) > 8192 {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}

func decodeLocalOwnerRelease(kv *mvccpb.KeyValue) (localOwnerReleaseRecord, error) {
	var r localOwnerReleaseRecord
	if !enrolledImmutableKV(kv) || len(kv.Value) == 0 || len(kv.Value) > 8192 || json.Unmarshal(kv.Value, &r) != nil {
		return r, ErrCorruptRecord
	}
	wire, err := encodeLocalOwnerRelease(r)
	if err != nil || wire != string(kv.Value) || kv.CreateRevision <= r.Termination.ModRevision || kv.CreateRevision <= r.Claim.CreateRevision {
		return localOwnerReleaseRecord{}, ErrCorruptRecord
	}
	return r, nil
}
