package etcd

import (
	"context"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// LoadCreationIntent reads permanent intent metadata; it does not infer runtime state.
func (b *Backend) LoadCreationIntent(ctx context.Context, w WorkspaceIdentity, intentID string) (*CreationIntentRecord, error) {
	if err := b.validateWorkspaceBinding(w); err != nil {
		return nil, err
	}
	key, err := b.namespace.intentKey(w.Partition(), intentID)
	if err != nil {
		return nil, err
	}
	records, err := b.readDomain(ctx, key)
	if err != nil {
		return nil, err
	}
	return b.decodeCreationIntent(records[0], w, intentID)
}
func (b *Backend) decodeCreationIntent(kv *mvccpb.KeyValue, w WorkspaceIdentity, id string) (*CreationIntentRecord, error) {
	if kv == nil {
		return nil, nil
	}
	if kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision {
		return nil, ErrCorruptRecord
	}
	record := new(CreationIntentRecord)
	if err := decodeDomainRecord(kv, record); err != nil {
		return nil, err
	}
	if err := b.domainEpoch(record.RestoreEpoch); err != nil {
		return nil, err
	}
	if record.IntentID != id || record.WorkspaceHash != w.Hash() || hashPartition(record.WorkspaceHash) != w.Partition() {
		return nil, ErrCorruptRecord
	}
	return record, nil
}

// creationBundle holds a single linearizable snapshot of all six permanent records.
type creationBundle struct {
	intent  *CreationIntentRecord
	request *CreationRequestRecord
	control *SandboxControlRecord
	records []*mvccpb.KeyValue
	keys    []string
	claim   *mvccpb.KeyValue
}

func (b *Backend) readCreationBundle(ctx context.Context, w WorkspaceIdentity, id, claimKey string) (*creationBundle, error) {
	initial, err := b.LoadCreationIntent(ctx, w, id)
	if err != nil {
		return nil, err
	}
	if initial == nil {
		return nil, ErrConflict
	}
	ik, err := b.namespace.intentKey(w.Partition(), id)
	if err != nil {
		return nil, err
	}
	ok, fk, err := b.namespace.workspaceKeys(w)
	if err != nil {
		return nil, err
	}
	ck, _, err := b.namespace.sandboxKeys(w.Partition(), initial.SandboxID, "read")
	if err != nil {
		return nil, err
	}
	rk, err := b.namespace.requestKey(initial.RequestHash)
	if err != nil {
		return nil, err
	}
	pk, err := b.namespace.placementKey(initial.SandboxID)
	if err != nil {
		return nil, err
	}
	keys := []string{ik, ok, fk, ck, rk, pk, claimKey}
	records, err := b.readDomain(ctx, keys...)
	if err != nil {
		return nil, err
	}
	for _, kv := range records[:6] {
		if kv == nil || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.Lease != 0 {
			return nil, ErrCorruptRecord
		}
	}
	intent, err := b.decodeCreationIntent(records[0], w, id)
	if err != nil {
		return nil, err
	}
	owner, fence, err := b.decodeOwnerFence(records[1], records[2], w)
	if err != nil {
		return nil, err
	}
	placement, err := b.decodePlacement(records[5], initial.SandboxID)
	if err != nil {
		return nil, err
	}
	control, err := b.decodeControl(records[3], w.Partition(), initial.SandboxID, placement)
	if err != nil {
		return nil, err
	}
	request, err := b.decodeRequest(records[4], initial.RequestHash)
	if err != nil {
		return nil, err
	}
	if intent.SandboxID != initial.SandboxID || intent.RequestHash != initial.RequestHash || owner.SandboxID != intent.SandboxID || owner.IntentID != id || owner.Generation != intent.Generation || fence.Generation != intent.Generation || control.WorkspaceHash != w.Hash() || control.IntentID != id || control.Generation != intent.Generation || request.WorkspaceHash != w.Hash() || request.SandboxID != intent.SandboxID || request.IntentID != id || request.Generation != intent.Generation || request.ConfigurationDigest != intent.ConfigurationDigest {
		return nil, ErrCorruptRecord
	}
	if (owner.Runtime == nil) != (control.Runtime == nil) || (owner.Runtime != nil && *owner.Runtime != *control.Runtime) || owner.MountAttempt != control.MountAttempt {
		return nil, ErrCorruptRecord
	}
	if intent.Phase == "published" && request.Phase == "completed" {
		if owner.Runtime == nil {
			return nil, ErrCorruptRecord
		}
		return nil, ErrConflict
	}
	if intent.Phase != "pending" || request.Phase != "pending" {
		return nil, ErrCorruptRecord
	}
	if control.Phase == PhaseDestroying || control.Phase == PhaseCleanupPending {
		return nil, ErrConflict
	}
	if control.Phase != PhasePublishing {
		return nil, ErrCorruptRecord
	}
	if owner.Runtime != nil || owner.MountAttempt != 0 || control.Runtime != nil || control.MountAttempt != 0 {
		return nil, ErrCorruptRecord
	}
	return &creationBundle{request: request, intent: intent, control: control, records: records[:6], keys: keys[:6], claim: records[6]}, nil
}
