package etcd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

func (r TaskWorkspaceSyncIntentReference) Validate() error {
	if r.Task.Validate() != nil || !validPreparationUUID(r.BudgetID) || !validPreparationUUID(r.IntentID) || r.Slot >= 64 || r.Revision < 0 {
		return ErrInvalidRecord
	}
	return nil
}

func (r TaskWorkspaceSyncIntentRecord) Validate() error {
	w := WorkspaceIdentity{provider: r.Provider, storageIdentityHash: r.StorageIdentityHash, bucket: r.Bucket, prefix: r.Prefix, hash: r.WorkspaceHash, partition: r.Reference.Task.Partition}
	c := r.Claim
	if r.Version != 1 || r.Reference.Validate() != nil || r.Reference.Revision != 0 || r.BudgetRevision <= 0 || r.Kind != "put_object" || validateWorkspaceSyncIdentity(w) != nil || c.Task != r.Reference.Task || !validPreparationUUID(c.ClaimID) || !validDomainSegment(c.WorkerID) || c.CreateRevision <= 0 || c.LeaseID <= 0 || !validPreparationUUID(r.FinalSyncAttemptID) || !validPreparationUUID(r.CommandID) || !validHexDigest(r.TicketDigest) || !validHexDigest(r.ArchiveSHA256) || !validHexDigest(r.PlanDigest) || !validHexDigest(r.BodySHA256) || !validWorkspaceSyncPath(r.RelativePath) || r.Mode < 0 || r.Mode > 0777 || r.Size < 0 || r.Offset < 0 {
		return ErrInvalidRecord
	}
	return nil
}

func (r taskWorkspaceSyncSlotRecord) Validate() error {
	if r.Version != 1 || !validPreparationUUID(r.BudgetID) || !validPreparationUUID(r.IntentID) || !validHexDigest(r.IntentDigest) || r.Slot >= 64 {
		return ErrInvalidRecord
	}
	return nil
}

func workspaceSyncIntentDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (w taskWorkspaceSyncIntentWire) record() TaskWorkspaceSyncIntentRecord {
	c := w.Claim
	return TaskWorkspaceSyncIntentRecord{w.taskWorkspaceSyncIntentAttribution, TaskClaimReference{Task: c.Task, ClaimID: c.ClaimID, WorkerID: c.WorkerID, CreateRevision: c.CreateRevision, LeaseID: c.LeaseID}}
}

func (w taskWorkspaceSyncIntentWire) Validate() error { return w.record().Validate() }

func encodeWorkspaceSyncIntent(r TaskWorkspaceSyncIntentRecord, limit int) (string, error) {
	c := r.Claim
	return encodeOperationMetadata(taskWorkspaceSyncIntentWire{r.taskWorkspaceSyncIntentAttribution, taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}}, limit)
}

func (b *Backend) workspaceSyncIntentKeys(r TaskWorkspaceSyncIntentReference) (string, string, error) {
	if r.Validate() != nil {
		return "", "", ErrInvalidRecord
	}
	if r.Task.Namespace != b.namespace.Root() || r.Task.RestoreEpoch != b.restoreEpoch {
		return "", "", ErrIdentityMismatch
	}
	i, err := b.namespace.Key("p", fmt.Sprintf("%02x", r.Task.Partition), "workspace-sync-intents", r.IntentID)
	if err != nil {
		return "", "", err
	}
	s, err := b.namespace.Key("p", fmt.Sprintf("%02x", r.Task.Partition), "workspace-sync-slots", fmt.Sprintf("%02d", r.Slot))
	return i, s, err
}

func decodeWorkspaceSyncIntent(kv *mvccpb.KeyValue, limit int) (TaskWorkspaceSyncIntentRecord, error) {
	var r TaskWorkspaceSyncIntentRecord
	var wire taskWorkspaceSyncIntentWire
	if !immutableWorkspaceSyncKV(kv) || decodePreparationDomain(kv, &wire, limit) != nil {
		return r, ErrCorruptRecord
	}
	r = wire.record()
	value, err := encodeWorkspaceSyncIntent(r, limit)
	if err != nil || value != string(kv.Value) {
		return r, ErrCorruptRecord
	}
	return r, nil
}

func decodeWorkspaceSyncSlot(kv *mvccpb.KeyValue) (taskWorkspaceSyncSlotRecord, error) {
	var r taskWorkspaceSyncSlotRecord
	if !immutableWorkspaceSyncKV(kv) || decodePreparationDomain(kv, &r, 512) != nil {
		return r, ErrCorruptRecord
	}
	value, err := encodeOperationMetadata(r, 512)
	if err != nil || value != string(kv.Value) {
		return r, ErrCorruptRecord
	}
	return r, nil
}
