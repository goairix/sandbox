package etcd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestCoherentViewReadNative(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	in := acquisitionInput(t, "coherent-read")
	_, err := b.AcquireIntent(ctx, in)
	require.NoError(t, err)
	ownerKey, fenceKey, err := b.namespace.workspaceKeys(in.Workspace)
	require.NoError(t, err)
	placementKey, err := b.namespace.placementKey(in.SandboxID)
	require.NoError(t, err)
	controlKey, _, err := b.namespace.sandboxKeys(in.Workspace.Partition(), in.SandboxID, "read")
	require.NoError(t, err)
	// Give control its own revision: snapshot revision and control MOD differ.
	control, err := b.LoadControl(ctx, in.Workspace.Partition(), in.SandboxID)
	require.NoError(t, err)
	control.Phase = PhaseCleanupPending
	control.Runtime = &RuntimeReference{ID: "runtime", UID: "uid", BootID: "boot"}
	owner, err := b.LoadOwner(ctx, in.Workspace)
	require.NoError(t, err)
	owner.Runtime = control.Runtime
	ownerValue, err := encodeDomainRecord(owner)
	require.NoError(t, err)
	controlValue, err := encodeDomainRecord(control)
	require.NoError(t, err)
	written, err := raw.Txn(ctx).Then(clientv3.OpPut(ownerKey, ownerValue), clientv3.OpPut(controlKey, controlValue)).Commit()
	require.NoError(t, err)
	_, err = raw.Put(ctx, b.namespace.Root()+"unrelated", "advance-snapshot")
	require.NoError(t, err)
	for _, entry := range []string{"workspace", "sandbox"} {
		t.Run(entry, func(t *testing.T) {
			calls := 0
			base := b.client.KV
			b.client.KV = &faultKV{KV: base, match: func(ops []clientv3.Op) bool {
				calls++
				for _, op := range ops {
					require.True(t, op.IsGet())
				}
				return false
			}}
			defer func() { b.client.KV = base }()
			var view *SandboxReadView
			if entry == "workspace" {
				view, err = b.ReadWorkspaceView(ctx, in.Workspace)
			} else {
				view, err = b.ReadSandboxView(ctx, in.SandboxID)
			}
			require.NoError(t, err)
			require.NotNil(t, view)
			require.Equal(t, 2, calls)
			require.Greater(t, view.Revision(), written.Header.Revision)
			require.Equal(t, written.Header.Revision, view.ControlRevision())
			require.Equal(t, [4]string{ownerKey, fenceKey, placementKey, controlKey}, view.keys)
			got := view.Records()
			require.Equal(t, owner, got.Owner)
			require.Equal(t, control, got.Control)
			require.Equal(t, in.Workspace.Hash(), got.Fence.WorkspaceHash)
			require.Equal(t, in.SandboxID, got.Placement.SandboxID)
			got.Owner.Runtime.ID = "mutated"
			got.Control.Runtime.UID = "mutated"
			got.Fence.Generation++
			got.Placement.SandboxID = "mutated"
			require.Equal(t, owner, view.Records().Owner)
			require.Equal(t, control, view.Records().Control)
			require.Equal(t, int64(1), view.Records().Fence.Generation)
			require.Equal(t, in.SandboxID, view.Records().Placement.SandboxID)
		})
	}
}

func TestCoherentViewDiscoveryNative(t *testing.T) {
	for _, entry := range []string{"workspace", "sandbox"} {
		t.Run(entry, func(t *testing.T) {
			b, raw := integrationBackend(t)
			ctx := context.Background()
			in := acquisitionInput(t, "discovery-"+entry)
			_, err := b.AcquireIntent(ctx, in)
			require.NoError(t, err)
			ownerKey, _, err := b.namespace.workspaceKeys(in.Workspace)
			require.NoError(t, err)
			placementKey, err := b.namespace.placementKey(in.SandboxID)
			require.NoError(t, err)
			calls := 0
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { calls++; return len(ops) == 4 }, before: func() {
				key := ownerKey
				if entry == "sandbox" {
					key = placementKey
				}
				// Same-value rewrites are changes too; discovery is a MOD pin.
				got, err := raw.Get(ctx, key)
				require.NoError(t, err)
				_, err = raw.Put(ctx, key, string(got.Kvs[0].Value))
				require.NoError(t, err)
			}}
			var view *SandboxReadView
			if entry == "workspace" {
				view, err = b.ReadWorkspaceView(ctx, in.Workspace)
			} else {
				view, err = b.ReadSandboxView(ctx, in.SandboxID)
			}
			require.Nil(t, view)
			require.ErrorIs(t, err, ErrConflict)
			require.Equal(t, 2, calls)
		})
	}
}

func TestCoherentViewAbsenceNative(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	in := acquisitionInput(t, "absence")
	view, err := b.ReadWorkspaceView(ctx, in.Workspace)
	require.NoError(t, err)
	require.Nil(t, view)
	view, err = b.ReadSandboxView(ctx, in.SandboxID)
	require.NoError(t, err)
	require.Nil(t, view)
	_, err = b.AcquireIntent(ctx, in)
	require.NoError(t, err)
	ownerKey, _, err := b.namespace.workspaceKeys(in.Workspace)
	require.NoError(t, err)
	_, err = raw.Delete(ctx, ownerKey)
	require.NoError(t, err)
	// A permanent fence alone is a valid absence for the workspace entry.
	view, err = b.ReadWorkspaceView(ctx, in.Workspace)
	require.NoError(t, err)
	require.Nil(t, view)
	// Placement still references this graph: ID entry must reject the hole.
	view, err = b.ReadSandboxView(ctx, in.SandboxID)
	require.Nil(t, view)
	require.ErrorIs(t, err, ErrCorruptRecord)
}

func TestCoherentViewWireBudget(t *testing.T) {
	n, err := NewNamespace("/"+strings.Repeat("p", 250), strings.Repeat("s", 128), strings.Repeat("c", 127))
	require.NoError(t, err)
	identityKey, err := n.Key("meta", "identity")
	require.NoError(t, err)
	restoreKey, err := n.Key("meta", "restore_epoch")
	require.NoError(t, err)
	b := &Backend{namespace: n, identityKey: identityKey, restoreKey: restoreKey, identityValue: strings.Repeat("x", maxIdentityJSONBytes), restoreEpoch: strings.Repeat("r", 128)}
	keys, err := b.readViewKeys(strings.Repeat("f", 64), strings.Repeat("i", 128))
	require.NoError(t, err)
	pins := make([]clientv3.Cmp, 4)
	values := make([]string, 4)
	limits := []int{4096, 1024, 4096, 4096}
	for i, key := range keys {
		pins[i] = clientv3.Compare(clientv3.ModRevision(key), "=", int64(1<<63-1))
		values[i] = strings.Repeat("x", limits[i])
	}
	require.NoError(t, b.readViewPreflight(keys[:], limits, pins))
	request := &pb.TxnRequest{Compare: taskProtoComparisons(append(b.baseComparisons(), pins...)), Success: taskProtoPoints(keys[:]), Failure: taskProtoPoints([]string{identityKey, restoreKey})}
	reply := taskProtoResponse(keys[:], values)
	require.Equal(t, 14, len(request.Compare)+len(request.Success)+len(request.Failure))
	require.Less(t, request.Size(), maxMutationBytes)
	require.Less(t, reply.Size(), maxMutationBytes)
	t.Logf("maximum namespace: nodes=%d request=%d domain-reply=%d payload=%d", 14, request.Size(), reply.Size(), 13312)
	require.ErrorIs(t, b.readViewPreflight(append(keys[:], identityKey), append(limits, 4096), pins), ErrInvalidRecord)
	require.ErrorIs(t, b.readViewPreflight(keys[:], []int{65536, 1024, 4096, 4096}, pins), ErrInvalidRecord)
}
