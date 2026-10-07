package etcd

import (
	"context"
	"fmt"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

func (b *Backend) validateTaskReference(r TaskReference) error {
	if r.Validate() != nil {
		return ErrInvalidRecord
	}
	if b == nil || r.Namespace != b.namespace.Root() || r.RestoreEpoch != b.restoreEpoch {
		return ErrIdentityMismatch
	}
	return nil
}

// LoadTask reads owned historical diagnostics at one identity-fenced point.
// It neither joins current runtime ownership nor grants cleanup authority.
func (b *Backend) LoadTask(ctx context.Context, ref TaskReference) (*TaskRecord, error) {
	if err := b.validateTaskReference(ref); err != nil {
		return nil, err
	}
	key, err := b.namespace.taskKey(ref)
	if err != nil {
		return nil, err
	}
	values, err := b.readDomain(ctx, key)
	if err != nil {
		return nil, err
	}
	if values[0] == nil {
		return nil, nil
	}
	var record TaskRecord
	if err = decodeTaskRecord(values[0], &record); err != nil {
		return nil, err
	}
	if record.Reference != ref {
		return nil, ErrCorruptRecord
	}
	return &record, nil
}

// LoadTaskCheckpoint returns a fresh permanent metadata observation, never a
// retained claim, physical result, completion permission or authority handle.
func (b *Backend) LoadTaskCheckpoint(ctx context.Context, ref TaskReference) (*TaskCheckpointRecord, error) {
	if err := b.validateTaskReference(ref); err != nil {
		return nil, err
	}
	key, err := b.namespace.taskCheckpointKey(ref)
	if err != nil {
		return nil, err
	}
	values, err := b.readDomain(ctx, key)
	if err != nil {
		return nil, err
	}
	if values[0] == nil {
		return nil, nil
	}
	var record TaskCheckpointRecord
	if err = decodeTaskCheckpointRecord(values[0], &record); err != nil {
		return nil, err
	}
	if record.Reference != ref {
		return nil, ErrCorruptRecord
	}
	return &record, nil
}

// This coherent authority bundle is separate from the public diagnostic reads.
// Only its eight permanent records become fixed claim fences. A checkpoint is
// validated at each use and retains its original birth, not an initial ModRevision.
type taskClaimBundle struct {
	task       TaskRecord
	fences     []taskFence
	checkpoint *mvccpb.KeyValue
	birth      int64
	initial    StageAttemptLocator
}

func (b *Backend) loadTaskClaimBundle(ctx context.Context, ref TaskReference) (*taskClaimBundle, error) {
	if err := b.validateTaskReference(ref); err != nil {
		return nil, err
	}
	taskKey, err := b.namespace.taskKey(ref)
	if err != nil {
		return nil, err
	}
	discovered, err := b.readDomain(ctx, taskKey)
	if err != nil {
		return nil, err
	}
	if discovered[0] == nil {
		return nil, ErrConflict
	}
	var routing TaskRecord
	if err = decodeTaskRecord(discovered[0], &routing); err != nil {
		return nil, err
	}
	if routing.Reference != ref {
		return nil, ErrCorruptRecord
	}
	intentKey, err := b.namespace.cleanupIntentKey(ref)
	if err != nil {
		return nil, err
	}
	linkKey, err := b.namespace.taskLinkKey(ref)
	if err != nil {
		return nil, err
	}
	placementKey, err := b.namespace.placementKey(ref.SandboxID)
	if err != nil {
		return nil, err
	}
	controlKey, _, err := b.namespace.sandboxKeys(ref.Partition, ref.SandboxID, "read")
	if err != nil {
		return nil, err
	}
	p := fmt.Sprintf("%02x", ref.Partition)
	ownerKey, err := b.namespace.Key("p", p, "workspaces", routing.WorkspaceHash, "owner")
	if err != nil {
		return nil, err
	}
	fenceKey, err := b.namespace.Key("p", p, "workspaces", routing.WorkspaceHash, "fence")
	if err != nil {
		return nil, err
	}
	indexKey, err := b.namespace.runtimeIndexKey(routing.Runtime.UID)
	if err != nil {
		return nil, err
	}
	checkpointKey, err := b.namespace.taskCheckpointKey(ref)
	if err != nil {
		return nil, err
	}
	claimKey, err := b.namespace.taskClaimKey(ref)
	if err != nil {
		return nil, err
	}
	keys := []string{taskKey, intentKey, linkKey, placementKey, controlKey, ownerKey, fenceKey, indexKey, checkpointKey, claimKey}
	values, err := b.readDomain(ctx, keys...)
	if err != nil {
		return nil, err
	}
	if values[9] != nil {
		return nil, ErrConflict
	}
	if !ownTaskFence(discovered[0]).matches(values[0]) {
		return nil, ErrConflict
	}
	for _, kv := range values[:8] {
		if !operationPermanentKV(kv) {
			return nil, ErrCorruptRecord
		}
	}
	var task TaskRecord
	var intent CleanupIntentRecord
	var link TaskLinkRecord
	if err = decodeTaskRecord(values[0], &task); err != nil {
		return nil, err
	}
	if err = decodeCleanupIntentRecord(values[1], &intent); err != nil {
		return nil, err
	}
	if err = decodeTaskLinkRecord(values[2], &link); err != nil {
		return nil, err
	}
	birth := values[0].CreateRevision
	if task.Reference != ref || intent.Task != task || link.Reference != ref || values[1].CreateRevision != birth || values[2].CreateRevision != birth {
		return nil, ErrCorruptRecord
	}
	if _, err = decodeTaskCheckpointAt(values[8], ref, birth, intent.Attempt); err != nil {
		return nil, err
	}
	placement, err := b.decodePlacement(values[3], ref.SandboxID)
	if err != nil {
		return nil, err
	}
	control, err := b.decodeControl(values[4], ref.Partition, ref.SandboxID, placement)
	if err != nil {
		return nil, err
	}
	if control.Phase != PhaseDestroying || control.Runtime == nil || *control.Runtime != task.Runtime || control.WorkspaceHash != task.WorkspaceHash || control.IntentID != task.CreationIntentID || control.Generation != task.Generation || control.DataGateEpoch != task.DataGateEpoch || control.Snapshot != task.Snapshot || !control.ExpiresAt.Equal(task.ExpiresAt) || values[4].ModRevision != birth || task.ControlRevision >= birth || values[4].CreateRevision > task.ControlRevision {
		return nil, ErrCorruptRecord
	}
	var owner WorkspaceOwnerRecord
	var fence WorkspaceFenceRecord
	var index RuntimeIndexRecord
	if err = decodeDomainRecord(values[5], &owner); err != nil {
		return nil, err
	}
	if err = decodeDomainRecord(values[6], &fence); err != nil {
		return nil, err
	}
	if err = decodeDomainRecord(values[7], &index); err != nil {
		return nil, err
	}
	if values[7].CreateRevision != values[7].ModRevision || owner.RestoreEpoch != ref.RestoreEpoch || fence.RestoreEpoch != ref.RestoreEpoch || index.RestoreEpoch != ref.RestoreEpoch || owner.WorkspaceHash != task.WorkspaceHash || owner.SandboxID != ref.SandboxID || owner.IntentID != task.CreationIntentID || owner.Generation != task.Generation || owner.Runtime == nil || *owner.Runtime != task.Runtime || owner.MountAttempt != control.MountAttempt || fence.WorkspaceHash != task.WorkspaceHash || fence.Generation != task.Generation || index.WorkspaceHash != task.WorkspaceHash || index.SandboxID != ref.SandboxID || index.IntentID != task.CreationIntentID || index.Generation != task.Generation || index.Runtime != task.Runtime {
		return nil, ErrCorruptRecord
	}
	fences := make([]taskFence, 8)
	for i, kv := range values[:8] {
		fences[i] = ownTaskFence(kv)
	}
	checkpoint := *values[8]
	checkpoint.Key = append([]byte(nil), checkpoint.Key...)
	checkpoint.Value = append([]byte(nil), checkpoint.Value...)
	return &taskClaimBundle{task: task, fences: fences, checkpoint: &checkpoint, birth: birth, initial: intent.Attempt}, nil
}
