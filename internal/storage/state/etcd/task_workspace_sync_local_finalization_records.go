package etcd

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

func (n Namespace) workspaceSyncLocalKeys(task TaskReference) (string, string, error) {
	parts, err := n.taskKeyParts(task)
	if err != nil {
		return "", "", err
	}
	progress, err := n.Key(append(parts, "workspace-sync-local-progress")...)
	if err != nil {
		return "", "", err
	}
	complete, err := n.Key(append(parts, "workspace-sync-local-complete")...)
	return progress, complete, err
}

func validWorkspaceSyncFinalizationClaim(c taskCloseClaimWire, task TaskReference) bool {
	return c.Task == task && validPreparationUUID(c.ClaimID) && validDomainSegment(c.WorkerID) && c.CreateRevision > 0 && c.LeaseID > 0
}

func (p taskWorkspaceSyncLocalProgress) Validate() error {
	if p.Version != 1 || p.Task.Validate() != nil || p.TaskBirth <= 0 || !validHexDigest(p.Binding) || !validWorkspaceSyncFinalizationClaim(p.Claim, p.Task) {
		return ErrInvalidRecord
	}
	switch p.Phase {
	case "regular":
		if p.CheckpointRevision != 0 || p.CheckpointDigest != "" || p.IntentID != "" || p.IntentBirth != 0 || p.EvidenceDigest != "" || p.Ledger.Path != "" || len(p.Ledger.Frames) != 0 {
			return ErrInvalidRecord
		}
	case "metadata", "effects-done", "sealed":
		if p.CheckpointRevision <= 0 || !validHexDigest(p.CheckpointDigest) || p.IntentID != "" && !validPreparationUUID(p.IntentID) || (p.IntentID == "") != (p.IntentBirth == 0) || p.IntentBirth < 0 {
			return ErrInvalidRecord
		}
		if p.Phase != "metadata" && (!validHexDigest(p.EvidenceDigest) || p.IntentID == "" || len(p.Ledger.Frames) != 0 || p.Ledger.Path != "") || p.Phase == "metadata" && p.EvidenceDigest != "" {
			return ErrInvalidRecord
		}
	default:
		return ErrInvalidRecord
	}
	if len(p.Ledger.Frames) > 128 || len(p.Ledger.Path) > 5464 || (len(p.Ledger.Frames) == 0) != (p.Ledger.Path == "") {
		return ErrInvalidRecord
	}
	if len(p.Ledger.Frames) > 0 {
		path, err := base64.StdEncoding.Strict().DecodeString(p.Ledger.Path)
		if err != nil || len(path) > 4096 || base64.StdEncoding.EncodeToString(path) != p.Ledger.Path || !validWorkspaceSyncPath(string(path)) {
			return ErrInvalidRecord
		}
		var previous uint64
		for _, frame := range p.Ledger.Frames {
			end := frame[0]
			if end <= previous || end > uint64(len(path)) || end < uint64(len(path)) && path[end] != '/' || path[end-1] == '/' || frame[1] == 0 || frame[2] > 0777 {
				return ErrInvalidRecord
			}
			previous = end
		}
		if previous != uint64(len(path)) || strings.Count(string(path), "/") >= 128 {
			return ErrInvalidRecord
		}
	}
	return nil
}

func encodeWorkspaceSyncLocalProgress(p taskWorkspaceSyncLocalProgress) (string, error) {
	if p.Ledger.Frames == nil {
		p.Ledger.Frames = [][3]uint64{}
	}
	wire, err := encodeOperationMetadata(p, 16384)
	if err != nil {
		return "", err
	}
	p.Ledger = taskWorkspaceSyncLocalLedger{}
	projection, err := json.Marshal(p)
	if err != nil || len(projection) > 4096 {
		return "", ErrInvalidRecord
	}
	return wire, nil
}

func decodeWorkspaceSyncLocalProgress(kv *mvccpb.KeyValue) (taskWorkspaceSyncLocalProgress, error) {
	var p taskWorkspaceSyncLocalProgress
	if kv == nil || kv.Lease != 0 || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.Version <= 0 || decodePreparationDomain(kv, &p, 16384) != nil {
		return p, ErrCorruptRecord
	}
	wire, err := encodeWorkspaceSyncLocalProgress(p)
	if err != nil || wire != string(kv.Value) {
		return p, ErrCorruptRecord
	}
	return p, nil
}

func (p taskWorkspaceSyncLocalComplete) Validate() error {
	if p.Version != 1 || p.Domain != "sandbox.workspace-sync-local-complete.v1" || p.Task.Validate() != nil || p.TaskBirth <= 0 || !validWorkspaceSyncFinalizationClaim(p.Claim, p.Task) || !validHexDigest(p.Binding) || !validHexDigest(p.ProgressDigest) || !validHexDigest(p.EvidenceDigest) || p.CheckpointRevision <= 0 || !validHexDigest(p.CheckpointDigest) || !validHexDigest(p.RootDigest) {
		return ErrInvalidRecord
	}
	return nil
}

func decodeWorkspaceSyncLocalComplete(kv *mvccpb.KeyValue) (taskWorkspaceSyncLocalComplete, error) {
	var p taskWorkspaceSyncLocalComplete
	if !immutableWorkspaceSyncKV(kv) || decodePreparationDomain(kv, &p, 8192) != nil {
		return p, ErrCorruptRecord
	}
	wire, err := encodeOperationMetadata(p, 8192)
	if err != nil || wire != string(kv.Value) {
		return p, ErrCorruptRecord
	}
	return p, nil
}
