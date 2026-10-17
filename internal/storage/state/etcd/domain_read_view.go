package etcd

import (
	"bytes"
	"context"
	"fmt"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ReadWorkspaceView discovers the sandbox, then reads its four metadata points
// together. Discovery pins prevent silently following a replacement owner.
func (b *Backend) ReadWorkspaceView(ctx context.Context, workspace WorkspaceIdentity) (*SandboxReadView, error) {
	if err := b.readViewInput(ctx); err != nil {
		return nil, err
	}
	if err := b.validateWorkspaceBinding(workspace); err != nil {
		return nil, err
	}
	ownerKey, fenceKey, err := b.namespace.workspaceKeys(workspace)
	if err != nil {
		return nil, err
	}
	points, _, err := b.readViewPoints(ctx, []string{ownerKey, fenceKey}, []int{4096, 1024}, nil)
	if err != nil {
		return nil, err
	}
	owner, fence, err := b.decodeOwnerFence(points[0], points[1], workspace)
	if err != nil {
		return nil, err
	}
	if err := readViewCanonical(points[0], owner); err != nil {
		return nil, err
	}
	if err := readViewCanonical(points[1], fence); err != nil {
		return nil, err
	}
	if owner == nil {
		return nil, nil
	}
	keys, err := b.readViewKeys(workspace.Hash(), owner.SandboxID)
	if err != nil {
		return nil, err
	}
	pins := []clientv3.Cmp{
		clientv3.Compare(clientv3.ModRevision(ownerKey), "=", points[0].ModRevision),
		clientv3.Compare(clientv3.ModRevision(fenceKey), "=", points[1].ModRevision),
	}
	return b.readViewFinal(ctx, keys, pins)
}

// ReadSandboxView routes only through the discovered placement. A concurrent
// change is a conflict, rather than permission to follow another workspace.
func (b *Backend) ReadSandboxView(ctx context.Context, sandboxID string) (*SandboxReadView, error) {
	if err := b.readViewInput(ctx); err != nil {
		return nil, err
	}
	key, err := b.namespace.placementKey(sandboxID)
	if err != nil {
		return nil, err
	}
	points, _, err := b.readViewPoints(ctx, []string{key}, []int{4096}, nil)
	if err != nil {
		return nil, err
	}
	placement, err := b.decodePlacement(points[0], sandboxID)
	if err != nil {
		return nil, err
	}
	if err := readViewCanonical(points[0], placement); err != nil {
		return nil, err
	}
	if placement == nil {
		return nil, nil
	}
	keys, err := b.readViewKeys(placement.WorkspaceHash, sandboxID)
	if err != nil {
		return nil, err
	}
	return b.readViewFinal(ctx, keys, []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", points[0].ModRevision)})
}

// ConfirmSandboxView checks only that the same Backend's metadata evidence is
// still current. A successful check grants no future operation, health, or TTL
// permission. It performs exactly one read-only transaction and never retries.
func (b *Backend) ConfirmSandboxView(ctx context.Context, view *SandboxReadView) error {
	if err := b.readViewInput(ctx); err != nil {
		return err
	}
	if view == nil || view.self != view || view.origin != b || view.revision <= 0 {
		return ErrInvalidRecord
	}
	if view.identityValue != b.identityValue || view.restoreEpoch != b.restoreEpoch || view.clusterID != b.clusterID {
		return ErrIdentityMismatch
	}
	pins := make([]clientv3.Cmp, 4)
	for i, key := range view.keys {
		if !validNamespaceKey(b.namespace, key) || view.mods[i] <= 0 || view.mods[i] > view.revision {
			return ErrInvalidRecord
		}
		pins[i] = clientv3.Compare(clientv3.ModRevision(key), "=", view.mods[i])
	}
	_, _, err := b.readViewPoints(ctx, nil, nil, pins)
	return err
}

func (b *Backend) readViewInput(ctx context.Context) error {
	if ctx == nil || b == nil || b.client == nil {
		return ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	identity, err := decodeIdentity(b.identityValue, b.namespace)
	if err != nil || identity.ClusterID != b.clusterID || !validDomainSegment(b.restoreEpoch) {
		return ErrIdentityMismatch
	}
	identityKey, err := b.namespace.Key("meta", "identity")
	if err != nil || identityKey != b.identityKey {
		return ErrIdentityMismatch
	}
	restoreKey, err := b.namespace.Key("meta", "restore_epoch")
	if err != nil || restoreKey != b.restoreKey {
		return ErrIdentityMismatch
	}
	return nil
}

func (b *Backend) readViewKeys(hash, id string) ([4]string, error) {
	var keys [4]string
	if !validHexDigest(hash) || !validDomainSegment(id) {
		return keys, ErrCorruptRecord
	}
	partition := fmt.Sprintf("%02x", hashPartition(hash))
	var err error
	keys[0], err = b.namespace.Key("p", partition, "workspaces", hash, "owner")
	if err == nil {
		keys[1], err = b.namespace.Key("p", partition, "workspaces", hash, "fence")
	}
	if err == nil {
		keys[2], err = b.namespace.placementKey(id)
	}
	if err == nil {
		keys[3], _, err = b.namespace.sandboxKeys(hashPartition(hash), id, "read")
	}
	return keys, err
}

func readViewCanonical(kv *mvccpb.KeyValue, record interface{ Validate() error }) error {
	if kv == nil {
		return nil
	}
	value, err := encodeDomainRecord(record)
	if err != nil || !bytes.Equal(kv.Value, []byte(value)) {
		return ErrCorruptRecord
	}
	return nil
}

func (b *Backend) readViewFinal(ctx context.Context, keys [4]string, pins []clientv3.Cmp) (*SandboxReadView, error) {
	points, revision, err := b.readViewPoints(ctx, keys[:], []int{4096, 1024, 4096, 4096}, pins)
	if err != nil {
		return nil, err
	}
	return b.decodeReadViewPoints(keys, points, revision)
}

// Decode one already bounded native quartet. Keeping this validation separate
// lets terminal observation validate a successor in its own final transaction
// without weakening the original four-point reader or issuing another RPC.
func (b *Backend) decodeReadViewPoints(keys [4]string, points []*mvccpb.KeyValue, revision int64) (*SandboxReadView, error) {
	if len(points) != 4 || revision <= 0 {
		return nil, ErrCorruptRecord
	}
	for _, point := range points {
		if point == nil {
			return nil, ErrCorruptRecord
		}
	}
	owner, fence := new(WorkspaceOwnerRecord), new(WorkspaceFenceRecord)
	placement, control := new(SandboxPlacementRecord), new(SandboxControlRecord)
	records := []interface{ Validate() error }{owner, fence, placement, control}
	for i, record := range records {
		if err := decodeDomainRecord(points[i], record); err != nil {
			return nil, err
		}
		if err := readViewCanonical(points[i], record); err != nil {
			return nil, err
		}
	}
	for _, epoch := range []string{owner.RestoreEpoch, fence.RestoreEpoch, placement.RestoreEpoch, control.RestoreEpoch} {
		if err := b.domainEpoch(epoch); err != nil {
			return nil, err
		}
	}
	expected, err := b.readViewKeys(owner.WorkspaceHash, owner.SandboxID)
	if err != nil || expected != keys || fence.WorkspaceHash != owner.WorkspaceHash || fence.Generation != owner.Generation || placement.WorkspaceHash != owner.WorkspaceHash || control.WorkspaceHash != owner.WorkspaceHash || placement.SandboxID != owner.SandboxID || control.SandboxID != owner.SandboxID || placement.IntentID != owner.IntentID || control.IntentID != owner.IntentID || placement.Generation != owner.Generation || control.Generation != owner.Generation || control.MountAttempt != owner.MountAttempt || !readViewRuntimeEqual(owner.Runtime, control.Runtime) {
		return nil, ErrCorruptRecord
	}
	v := &SandboxReadView{origin: b, keys: keys, revision: revision,
		records:       SandboxReadState{Owner: owner, Fence: fence, Placement: placement, Control: control},
		identityValue: b.identityValue, restoreEpoch: b.restoreEpoch, clusterID: b.clusterID}
	for i, point := range points {
		v.mods[i] = point.ModRevision
	}
	v.self = v
	return v, nil
}

func readViewRuntimeEqual(a, b *RuntimeReference) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
