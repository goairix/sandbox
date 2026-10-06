package etcd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// readDomain takes one linearizable, identity-fenced snapshot of point keys.
// Read evidence never grants permission to operate a runtime.
func (b *Backend) readDomain(ctx context.Context, keys ...string) ([]*mvccpb.KeyValue, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is nil", ErrInvalidRecord)
	}
	ops := make([]clientv3.Op, len(keys))
	for i, key := range keys {
		if !validNamespaceKey(b.namespace, key) {
			return nil, ErrInvalidRecord
		}
		ops[i] = clientv3.OpGet(key)
	}
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	response, err := b.client.Txn(bounded).If(b.baseComparisons()...).Then(ops...).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	if err != nil {
		return nil, fmt.Errorf("etcd state: read domain: %w", err)
	}
	if response == nil || response.Header == nil || response.Header.ClusterId != b.clusterID {
		return nil, ErrIdentityMismatch
	}
	if !response.Succeeded {
		if len(response.Responses) != 2 {
			return nil, ErrIdentityMismatch
		}
		for i, key := range []string{b.identityKey, b.restoreKey} {
			if response.Responses[i] == nil {
				return nil, ErrIdentityMismatch
			}
			metadata := response.Responses[i].GetResponseRange()
			if metadata == nil || len(metadata.Kvs) != 1 || metadata.Kvs[0] == nil || string(metadata.Kvs[0].Key) != key {
				return nil, ErrIdentityMismatch
			}
		}
		if err := b.validateResponseIdentity(response); err != nil {
			return nil, err
		}
		// All comparisons concern operator metadata; a failed condition cannot
		// supply authoritative business records even with malformed transport data.
		return nil, ErrIdentityMismatch
	}
	if len(response.Responses) != len(keys) {
		return nil, ErrCorruptRecord
	}
	result := make([]*mvccpb.KeyValue, len(keys))
	for i, response := range response.Responses {
		if response == nil {
			return nil, ErrCorruptRecord
		}
		r := response.GetResponseRange()
		if r == nil || len(r.Kvs) > 1 || r.More || r.Count != int64(len(r.Kvs)) {
			return nil, ErrCorruptRecord
		}
		if len(r.Kvs) == 1 {
			if r.Kvs[0] == nil || string(r.Kvs[0].Key) != keys[i] {
				return nil, ErrCorruptRecord
			}
			result[i] = r.Kvs[0]
		}
	}
	return result, nil
}

func (b *Backend) validateWorkspaceBinding(w WorkspaceIdentity) error {
	if err := w.validate(); err != nil {
		return err
	}
	identity, err := decodeIdentity(b.identityValue, b.namespace)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(identity.StorageID))
	if hex.EncodeToString(digest[:]) != w.storageIdentityHash {
		return ErrIdentityMismatch
	}
	return nil
}
func (b *Backend) domainEpoch(epoch string) error {
	if epoch != b.restoreEpoch {
		return ErrIdentityMismatch
	}
	return nil
}
func hashPartition(hash string) uint8 { digest, _ := hex.DecodeString(hash); return digest[0] }
func (b *Backend) decodeOwnerFence(ownerKV, fenceKV *mvccpb.KeyValue, w WorkspaceIdentity) (*WorkspaceOwnerRecord, *WorkspaceFenceRecord, error) {
	var owner *WorkspaceOwnerRecord
	var fence *WorkspaceFenceRecord
	if fenceKV != nil {
		fence = new(WorkspaceFenceRecord)
		if err := decodeDomainRecord(fenceKV, fence); err != nil {
			return nil, nil, err
		}
		if err := b.domainEpoch(fence.RestoreEpoch); err != nil {
			return nil, nil, err
		}
		if fence.WorkspaceHash != w.Hash() {
			return nil, nil, ErrCorruptRecord
		}
	}
	if ownerKV != nil {
		owner = new(WorkspaceOwnerRecord)
		if err := decodeDomainRecord(ownerKV, owner); err != nil {
			return nil, nil, err
		}
		if err := b.domainEpoch(owner.RestoreEpoch); err != nil {
			return nil, nil, err
		}
		if owner.WorkspaceHash != w.Hash() || fence == nil || owner.Generation != fence.Generation {
			return nil, nil, ErrCorruptRecord
		}
	}
	return owner, fence, nil
}
func (b *Backend) decodeRequest(kv *mvccpb.KeyValue, hash string) (*CreationRequestRecord, error) {
	if kv == nil {
		return nil, nil
	}
	record := new(CreationRequestRecord)
	if err := decodeDomainRecord(kv, record); err != nil {
		return nil, err
	}
	if err := b.domainEpoch(record.RestoreEpoch); err != nil {
		return nil, err
	}
	if record.RequestHash != hash {
		return nil, ErrCorruptRecord
	}
	return record, nil
}
func (b *Backend) decodePlacement(kv *mvccpb.KeyValue, id string) (*SandboxPlacementRecord, error) {
	if kv == nil {
		return nil, nil
	}
	record := new(SandboxPlacementRecord)
	if err := decodeDomainRecord(kv, record); err != nil {
		return nil, err
	}
	if err := b.domainEpoch(record.RestoreEpoch); err != nil {
		return nil, err
	}
	if record.SandboxID != id {
		return nil, ErrCorruptRecord
	}
	return record, nil
}
func (b *Backend) decodeControl(kv *mvccpb.KeyValue, p uint8, id string, placement *SandboxPlacementRecord) (*SandboxControlRecord, error) {
	if kv == nil {
		return nil, nil
	}
	record := new(SandboxControlRecord)
	if err := decodeDomainRecord(kv, record); err != nil {
		return nil, err
	}
	if err := b.domainEpoch(record.RestoreEpoch); err != nil {
		return nil, err
	}
	if record.SandboxID != id || hashPartition(record.WorkspaceHash) != p || placement == nil || placement.Partition != p || placement.WorkspaceHash != record.WorkspaceHash || placement.IntentID != record.IntentID || placement.Generation != record.Generation || placement.RestoreEpoch != record.RestoreEpoch {
		return nil, ErrCorruptRecord
	}
	return record, nil
}
func decodeSnapshot(kv *mvccpb.KeyValue, p uint8, id string, ref SnapshotReference, placement *SandboxPlacementRecord) (*SandboxSnapshotRecord, error) {
	if kv == nil {
		return nil, nil
	}
	record := new(SandboxSnapshotRecord)
	if err := decodeDomainRecord(kv, record); err != nil {
		return nil, err
	}
	if record.SandboxID != id || record.Snapshot != ref || placement == nil || placement.Partition != p {
		return nil, ErrCorruptRecord
	}
	return record, nil
}

func (b *Backend) LoadRequest(ctx context.Context, principal, key string) (*CreationRequestRecord, error) {
	hash, err := requestKeyHash(principal, key)
	if err != nil {
		return nil, err
	}
	k, err := b.namespace.requestKey(hash)
	if err != nil {
		return nil, err
	}
	records, err := b.readDomain(ctx, k)
	if err != nil {
		return nil, err
	}
	return b.decodeRequest(records[0], hash)
}
func (b *Backend) LoadOwner(ctx context.Context, w WorkspaceIdentity) (*WorkspaceOwnerRecord, error) {
	if err := b.validateWorkspaceBinding(w); err != nil {
		return nil, err
	}
	owner, fence, err := b.namespace.workspaceKeys(w)
	if err != nil {
		return nil, err
	}
	records, err := b.readDomain(ctx, owner, fence)
	if err != nil {
		return nil, err
	}
	record, _, err := b.decodeOwnerFence(records[0], records[1], w)
	return record, err
}
func (b *Backend) LoadPlacement(ctx context.Context, id string) (*SandboxPlacementRecord, error) {
	k, err := b.namespace.placementKey(id)
	if err != nil {
		return nil, err
	}
	records, err := b.readDomain(ctx, k)
	if err != nil {
		return nil, err
	}
	return b.decodePlacement(records[0], id)
}
func (b *Backend) LoadControl(ctx context.Context, partition uint8, id string) (*SandboxControlRecord, error) {
	control, _, err := b.namespace.sandboxKeys(partition, id, "read")
	if err != nil {
		return nil, err
	}
	pk, err := b.namespace.placementKey(id)
	if err != nil {
		return nil, err
	}
	records, err := b.readDomain(ctx, control, pk)
	if err != nil {
		return nil, err
	}
	placement, err := b.decodePlacement(records[1], id)
	if err != nil {
		return nil, err
	}
	return b.decodeControl(records[0], partition, id, placement)
}
func (b *Backend) LoadSnapshot(ctx context.Context, partition uint8, id string, ref SnapshotReference) (*SandboxSnapshotRecord, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	_, snapshot, err := b.namespace.sandboxKeys(partition, id, ref.Version)
	if err != nil {
		return nil, err
	}
	pk, err := b.namespace.placementKey(id)
	if err != nil {
		return nil, err
	}
	records, err := b.readDomain(ctx, snapshot, pk)
	if err != nil {
		return nil, err
	}
	placement, err := b.decodePlacement(records[1], id)
	if err != nil {
		return nil, err
	}
	return decodeSnapshot(records[0], partition, id, ref, placement)
}
