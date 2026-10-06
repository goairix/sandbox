package etcd

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

var ErrOperationAdmissionClosed = errors.New("etcd state: operation admission closed")

// operationControlBundle owns one coherent five-point snapshot. Every original
// ModRevision and Lease is retained for a later admission transaction.
type operationControlBundle struct {
	Control   SandboxControlRecord
	Partition uint8
	Keys      []string
	KVs       []*mvccpb.KeyValue
}

// loadOperationControl discovers routing with exact points, then revalidates
// the complete authority chain in one linearizable identity-fenced snapshot.
// Discovery is never authority, and a changed route is not retried implicitly.
func (b *Backend) loadOperationControl(ctx context.Context, id string) (*operationControlBundle, error) {
	placementKey, err := b.namespace.placementKey(id)
	if err != nil {
		return nil, err
	}
	initial, err := b.readDomain(ctx, placementKey)
	if err != nil {
		return nil, err
	}
	if initial[0] == nil {
		return nil, ErrOperationAdmissionClosed
	}
	if !operationPermanentKV(initial[0]) {
		return nil, ErrCorruptRecord
	}
	placement, err := b.decodePlacement(initial[0], id)
	if err != nil {
		return nil, err
	}
	controlKey, _, err := b.namespace.sandboxKeys(placement.Partition, id, "read")
	if err != nil {
		return nil, err
	}
	discovered, err := b.readDomain(ctx, controlKey)
	if err != nil {
		return nil, err
	}
	if discovered[0] == nil {
		return nil, ErrOperationAdmissionClosed
	}
	if !operationPermanentKV(discovered[0]) {
		return nil, ErrCorruptRecord
	}
	control, err := b.decodeControl(discovered[0], placement.Partition, id, placement)
	if err != nil {
		return nil, err
	}
	if control.Phase != PhaseActive {
		return nil, ErrOperationAdmissionClosed
	}
	p := fmt.Sprintf("%02x", placement.Partition)
	ownerKey, err := b.namespace.Key("p", p, "workspaces", control.WorkspaceHash, "owner")
	if err != nil {
		return nil, err
	}
	fenceKey, err := b.namespace.Key("p", p, "workspaces", control.WorkspaceHash, "fence")
	if err != nil {
		return nil, err
	}
	indexKey, err := b.namespace.runtimeIndexKey(control.Runtime.UID)
	if err != nil {
		return nil, err
	}
	keys := []string{placementKey, controlKey, ownerKey, fenceKey, indexKey}
	values, err := b.readDomain(ctx, keys...)
	if err != nil {
		return nil, err
	}
	// Missing placement/control close admission even when stale subsidiary
	// records remain. A partially missing active chain is corruption.
	if values[0] == nil || values[1] == nil {
		return nil, ErrOperationAdmissionClosed
	}
	for _, kv := range values[:2] {
		if !operationPermanentKV(kv) {
			return nil, ErrCorruptRecord
		}
	}
	finalPlacement, err := b.decodePlacement(values[0], id)
	if err != nil {
		return nil, err
	}
	var finalControl SandboxControlRecord
	if err = decodeDomainRecord(values[1], &finalControl); err != nil {
		return nil, err
	}
	if err = b.domainEpoch(finalControl.RestoreEpoch); err != nil {
		return nil, err
	}
	if finalControl.Phase != PhaseActive {
		return nil, ErrOperationAdmissionClosed
	}
	if values[0].ModRevision != initial[0].ModRevision || values[1].ModRevision != discovered[0].ModRevision {
		return nil, ErrConflict
	}
	if _, err = b.decodeControl(values[1], placement.Partition, id, finalPlacement); err != nil {
		return nil, err
	}
	for _, kv := range values[2:] {
		if !operationPermanentKV(kv) {
			return nil, ErrCorruptRecord
		}
	}
	if values[4].CreateRevision != values[4].ModRevision {
		return nil, ErrCorruptRecord
	}
	var owner WorkspaceOwnerRecord
	var fence WorkspaceFenceRecord
	var index RuntimeIndexRecord
	if err = decodeDomainRecord(values[2], &owner); err != nil {
		return nil, err
	}
	if err = decodeDomainRecord(values[3], &fence); err != nil {
		return nil, err
	}
	if err = decodeDomainRecord(values[4], &index); err != nil {
		return nil, err
	}
	for _, epoch := range []string{owner.RestoreEpoch, fence.RestoreEpoch, index.RestoreEpoch} {
		if err = b.domainEpoch(epoch); err != nil {
			return nil, err
		}
	}
	if owner.WorkspaceHash != finalControl.WorkspaceHash || owner.SandboxID != id || owner.IntentID != finalControl.IntentID || owner.Generation != finalControl.Generation || owner.Runtime == nil || *owner.Runtime != *finalControl.Runtime || owner.MountAttempt != finalControl.MountAttempt || fence.WorkspaceHash != finalControl.WorkspaceHash || fence.Generation != finalControl.Generation || index.WorkspaceHash != finalControl.WorkspaceHash || index.SandboxID != id || index.IntentID != finalControl.IntentID || index.Generation != finalControl.Generation || index.Runtime != *finalControl.Runtime {
		return nil, ErrCorruptRecord
	}
	owned := make([]*mvccpb.KeyValue, len(values))
	for i, kv := range values {
		copy := *kv
		copy.Key = bytes.Clone(kv.Key)
		copy.Value = bytes.Clone(kv.Value)
		owned[i] = &copy
	}
	return &operationControlBundle{Control: finalControl, Partition: finalPlacement.Partition, Keys: append([]string(nil), keys...), KVs: owned}, nil
}

func operationPermanentKV(kv *mvccpb.KeyValue) bool {
	return kv != nil && kv.Lease == 0 && kv.CreateRevision > 0 && kv.ModRevision >= kv.CreateRevision
}
