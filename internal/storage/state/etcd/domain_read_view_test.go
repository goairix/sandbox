package etcd

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
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
	n, err := NewNamespace("/"+strings.Repeat("p", 253), strings.Repeat("s", 128), strings.Repeat("c", 127))
	require.NoError(t, err)
	require.Len(t, n.Root(), maxNamespaceRootBytes)
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

func TestCoherentViewConfirmNative(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	in := acquisitionInput(t, "confirm")
	_, err := b.AcquireIntent(ctx, in)
	require.NoError(t, err)
	view, err := b.ReadSandboxView(ctx, in.SandboxID)
	require.NoError(t, err)
	before, err := raw.Get(ctx, view.keys[3])
	require.NoError(t, err)
	base := b.client.KV
	calls := 0
	b.client.KV = &faultKV{KV: base, match: func(ops []clientv3.Op) bool {
		calls++
		require.Empty(t, ops, "successful confirmation does not read payloads or write")
		return false
	}}
	require.NoError(t, b.ConfirmSandboxView(ctx, view))
	require.Equal(t, 1, calls)
	after, err := raw.Get(ctx, view.keys[3])
	require.NoError(t, err)
	require.Equal(t, before.Header.Revision, after.Header.Revision)
	require.Equal(t, before.Kvs, after.Kvs)
	b.client.KV = base

	for i, key := range view.keys {
		t.Run("point-"+string(rune('0'+i)), func(t *testing.T) {
			view, err := b.ReadSandboxView(ctx, in.SandboxID)
			require.NoError(t, err)
			got, err := raw.Get(ctx, key)
			require.NoError(t, err)
			_, err = raw.Put(ctx, key, string(got.Kvs[0].Value))
			require.NoError(t, err)
			require.ErrorIs(t, b.ConfirmSandboxView(ctx, view), ErrConflict)
		})
	}
	t.Run("delete-recreate-ABA", func(t *testing.T) {
		view, err := b.ReadSandboxView(ctx, in.SandboxID)
		require.NoError(t, err)
		got, err := raw.Get(ctx, view.keys[3])
		require.NoError(t, err)
		_, err = raw.Delete(ctx, view.keys[3])
		require.NoError(t, err)
		_, err = raw.Put(ctx, view.keys[3], string(got.Kvs[0].Value))
		require.NoError(t, err)
		require.ErrorIs(t, b.ConfirmSandboxView(ctx, view), ErrConflict)
	})
	for _, mutation := range []string{"ttl", "phase", "runtime", "data-gate"} {
		t.Run(mutation, func(t *testing.T) {
			view, err := b.ReadSandboxView(ctx, in.SandboxID)
			require.NoError(t, err)
			control := view.Records().Control
			original, err := encodeDomainRecord(control)
			require.NoError(t, err)
			switch mutation {
			case "ttl":
				control.ExpiresAt = control.ExpiresAt.Add(time.Minute)
			case "phase":
				control.Phase = PhaseDestroying
			case "runtime":
				control.Phase = PhaseCleanupPending
				control.Runtime = &RuntimeReference{ID: "runtime", UID: "uid", BootID: "boot"}
			case "data-gate":
				control.DataGateEpoch++
			}
			value, err := encodeDomainRecord(control)
			require.NoError(t, err)
			_, err = raw.Put(ctx, view.keys[3], value)
			require.NoError(t, err)
			require.ErrorIs(t, b.ConfirmSandboxView(ctx, view), ErrConflict)
			_, err = raw.Put(ctx, view.keys[3], original)
			require.NoError(t, err)
		})
	}
	view, err = b.ReadSandboxView(ctx, in.SandboxID)
	require.NoError(t, err)
	clone := *view
	foreign := *b
	require.ErrorIs(t, b.ConfirmSandboxView(ctx, nil), ErrInvalidRecord)
	require.ErrorIs(t, b.ConfirmSandboxView(ctx, &SandboxReadView{}), ErrInvalidRecord)
	require.ErrorIs(t, b.ConfirmSandboxView(ctx, &clone), ErrInvalidRecord)
	require.ErrorIs(t, foreign.ConfirmSandboxView(ctx, view), ErrInvalidRecord)
	require.ErrorIs(t, b.ConfirmSandboxView(nil, view), ErrInvalidRecord)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, b.ConfirmSandboxView(cancelled, view), context.Canceled)
	_, err = raw.Put(ctx, b.restoreKey, "changed-epoch")
	require.NoError(t, err)
	require.ErrorIs(t, b.ConfirmSandboxView(ctx, view), ErrIdentityMismatch)
}

func TestCoherentViewReadFaultsNative(t *testing.T) {
	b, _ := integrationBackend(t)
	ctx := context.Background()
	in := acquisitionInput(t, "read-faults")
	_, err := b.AcquireIntent(ctx, in)
	require.NoError(t, err)
	tests := []struct {
		name   string
		mutate func(*clientv3.TxnResponse)
		want   error
	}{
		{"cluster", func(r *clientv3.TxnResponse) { r.Header.ClusterId++ }, ErrIdentityMismatch},
		{"revision", func(r *clientv3.TxnResponse) { r.Header.Revision = 0 }, ErrCorruptRecord},
		{"top-unknown", func(r *clientv3.TxnResponse) { r.XXX_unrecognized = []byte{8, 1} }, ErrCorruptRecord},
		{"header-unknown", func(r *clientv3.TxnResponse) { r.Header.XXX_unrecognized = []byte{8, 1} }, ErrCorruptRecord},
		{"count", func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Count++ }, ErrCorruptRecord},
		{"more", func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().More = true }, ErrCorruptRecord},
		{"foreign-key", func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].Key = []byte("/foreign/owner") }, ErrCorruptRecord},
		{"wrong-point", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0].Key = r.Responses[1].GetResponseRange().Kvs[0].Key
		}, ErrCorruptRecord},
		{"lease", func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].Lease = 1 }, ErrCorruptRecord},
		{"create", func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].CreateRevision = 0 }, ErrCorruptRecord},
		{"future-mod", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0].ModRevision = r.Header.Revision + 1
		}, ErrCorruptRecord},
		{"version", func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0].Version = 0 }, ErrCorruptRecord},
		{"impossible-version", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0].Version = r.Header.Revision + 1
		}, ErrCorruptRecord},
		{"nil-kv", func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().Kvs[0] = nil }, ErrCorruptRecord},
		{"two-kvs", func(r *clientv3.TxnResponse) {
			rangeResponse := r.Responses[0].GetResponseRange()
			rangeResponse.Kvs = append(rangeResponse.Kvs, rangeResponse.Kvs[0])
			rangeResponse.Count = 2
		}, ErrCorruptRecord},
		{"missing-response", func(r *clientv3.TxnResponse) { r.Responses = r.Responses[:3] }, ErrCorruptRecord},
		{"nil-response", func(r *clientv3.TxnResponse) { r.Responses[0] = nil }, ErrCorruptRecord},
		{"recursive-txn", func(r *clientv3.TxnResponse) {
			r.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseTxn{ResponseTxn: (*pb.TxnResponse)(r)}}
		}, ErrCorruptRecord},
		{"nested-revision", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Header = &pb.ResponseHeader{Revision: r.Header.Revision + 1}
		}, ErrCorruptRecord},
		{"nested-cluster", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Header = &pb.ResponseHeader{Revision: r.Header.Revision, ClusterId: r.Header.ClusterId + 1}
		}, ErrCorruptRecord},
		{"nested-member", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Header = &pb.ResponseHeader{Revision: r.Header.Revision, MemberId: r.Header.MemberId + 1}
		}, ErrCorruptRecord},
		{"range-unknown", func(r *clientv3.TxnResponse) { r.Responses[0].GetResponseRange().XXX_unrecognized = []byte{8, 1} }, ErrCorruptRecord},
		{"kv-unknown", func(r *clientv3.TxnResponse) {
			r.Responses[0].GetResponseRange().Kvs[0].XXX_unrecognized = []byte{8, 1}
		}, ErrCorruptRecord},
	}
	base := b.client.KV
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b.client.KV = &faultKV{KV: base, match: func(ops []clientv3.Op) bool { return len(ops) == 4 }, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				test.mutate(r)
				return r, nil
			}}
			view, err := b.ReadSandboxView(ctx, in.SandboxID)
			require.Nil(t, view)
			require.ErrorIs(t, err, test.want)
		})
	}
	b.client.KV = base
}

func TestCoherentViewRecordLinksNative(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	in := acquisitionInput(t, "record-links")
	_, err := b.AcquireIntent(ctx, in)
	require.NoError(t, err)
	view, err := b.ReadSandboxView(ctx, in.SandboxID)
	require.NoError(t, err)
	state := view.Records()
	ownerValue, err := encodeDomainRecord(state.Owner)
	require.NoError(t, err)
	for _, field := range []string{"sandbox", "intent", "generation", "workspace", "restore", "runtime", "canonical", "unknown", "duplicate", "oversize"} {
		t.Run(field, func(t *testing.T) {
			owner := *state.Owner
			want := ErrCorruptRecord
			switch field {
			case "sandbox":
				owner.SandboxID = "another-sandbox"
			case "intent":
				owner.IntentID = "another-intent"
			case "generation":
				owner.Generation++
			case "workspace":
				owner.WorkspaceHash = strings.Repeat("a", 64)
			case "restore":
				owner.RestoreEpoch = "another-restore"
				want = ErrIdentityMismatch
			case "runtime":
				owner.Runtime = &RuntimeReference{ID: "runtime", UID: "uid", BootID: "boot"}
			}
			value, err := encodeDomainRecord(owner)
			require.NoError(t, err)
			switch field {
			case "canonical":
				value = " " + value
			case "unknown":
				value = strings.TrimSuffix(value, "}") + `,"extra":1}`
			case "duplicate":
				value = strings.TrimSuffix(value, "}") + `,"version":1}`
			case "oversize":
				value += strings.Repeat(" ", 4096)
			}
			_, err = raw.Put(ctx, view.keys[0], value)
			require.NoError(t, err)
			got, err := b.ReadSandboxView(ctx, in.SandboxID)
			require.Nil(t, got)
			require.ErrorIs(t, err, want)
			_, err = raw.Put(ctx, view.keys[0], ownerValue)
			require.NoError(t, err)
		})
	}
	// Control's mount attempt is independently valid, but must match owner.
	runtime := &RuntimeReference{ID: "runtime", UID: "uid", BootID: "boot"}
	state.Owner.Runtime, state.Control.Runtime = runtime, runtime
	state.Control.Phase, state.Control.MountAttempt = PhaseActive, 1
	ownerValue, err = encodeDomainRecord(state.Owner)
	require.NoError(t, err)
	controlValue, err := encodeDomainRecord(state.Control)
	require.NoError(t, err)
	_, err = raw.Txn(ctx).Then(clientv3.OpPut(view.keys[0], ownerValue), clientv3.OpPut(view.keys[3], controlValue)).Commit()
	require.NoError(t, err)
	got, err := b.ReadWorkspaceView(ctx, in.Workspace)
	require.Nil(t, got)
	require.ErrorIs(t, err, ErrCorruptRecord)
}

func TestCoherentViewInputAndOwnedCopies(t *testing.T) {
	ctx := context.Background()
	var b *Backend
	_, err := b.ReadWorkspaceView(ctx, WorkspaceIdentity{})
	require.ErrorIs(t, err, ErrInvalidRecord)
	_, err = b.ReadSandboxView(ctx, "id")
	require.ErrorIs(t, err, ErrInvalidRecord)
	var nilView *SandboxReadView
	require.Zero(t, nilView.Revision())
	require.Zero(t, nilView.ControlRevision())
	require.Equal(t, SandboxReadState{}, nilView.Records())
	zero := new(SandboxReadView)
	require.Zero(t, zero.Revision())
	require.Equal(t, SandboxReadState{}, zero.Records())
	for i := 0; i < 32; i++ {
		runtime := &RuntimeReference{ID: fmt.Sprintf("runtime-%d", i), UID: "uid", BootID: "boot"}
		v := &SandboxReadView{records: SandboxReadState{Owner: &WorkspaceOwnerRecord{Runtime: runtime}, Fence: &WorkspaceFenceRecord{}, Placement: &SandboxPlacementRecord{}, Control: &SandboxControlRecord{Runtime: runtime}}}
		v.self = v
		copy := v.Records()
		copy.Owner.Runtime.ID = "changed"
		copy.Control.Runtime.UID = "changed"
		require.Equal(t, runtime.ID, v.Records().Owner.Runtime.ID)
		require.Equal(t, "uid", v.Records().Control.Runtime.UID)
	}
	// Host-only envelope regression: a valid point with a foreign key fails.
	n, err := NewNamespace("/test", "state", "view")
	require.NoError(t, err)
	b = &Backend{namespace: n}
	r := &clientv3.TxnResponse{Header: &pb.ResponseHeader{Revision: 1}, Responses: []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &pb.RangeResponse{Kvs: []*mvccpb.KeyValue{{Key: []byte("/foreign"), Value: []byte("x"), CreateRevision: 1, ModRevision: 1, Version: 1}}, Count: 1}}}}}
	_, err = b.readViewResponse(r, []string{n.Root() + "point"}, []int{4096})
	require.ErrorIs(t, err, ErrCorruptRecord)
}

func TestCoherentViewConfirmFaultsNative(t *testing.T) {
	b, raw := integrationBackend(t)
	ctx := context.Background()
	in := acquisitionInput(t, "confirm-faults")
	_, err := b.AcquireIntent(ctx, in)
	require.NoError(t, err)
	base := b.client.KV
	tests := []struct {
		name  string
		fail  bool
		after func(*clientv3.TxnResponse) (*clientv3.TxnResponse, error)
		want  error
	}{
		{"RPC-error", false, func(r *clientv3.TxnResponse) (*clientv3.TxnResponse, error) { return r, context.DeadlineExceeded }, context.DeadlineExceeded},
		{"nil", false, func(r *clientv3.TxnResponse) (*clientv3.TxnResponse, error) { return nil, nil }, ErrIdentityMismatch},
		{"unexpected-payload", false, func(r *clientv3.TxnResponse) (*clientv3.TxnResponse, error) {
			r.Responses = []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &pb.RangeResponse{}}}}
			return r, nil
		}, ErrCorruptRecord},
		{"cluster", false, func(r *clientv3.TxnResponse) (*clientv3.TxnResponse, error) { r.Header.ClusterId++; return r, nil }, ErrIdentityMismatch},
		{"failed-count", true, func(r *clientv3.TxnResponse) (*clientv3.TxnResponse, error) {
			r.Responses[0].GetResponseRange().Count++
			return r, nil
		}, ErrIdentityMismatch},
		{"failed-key", true, func(r *clientv3.TxnResponse) (*clientv3.TxnResponse, error) {
			r.Responses[0].GetResponseRange().Kvs[0].Key = []byte(b.namespace.Root() + "wrong-identity")
			return r, nil
		}, ErrIdentityMismatch},
		{"failed-lease", true, func(r *clientv3.TxnResponse) (*clientv3.TxnResponse, error) {
			r.Responses[0].GetResponseRange().Kvs[0].Lease = 9
			return r, nil
		}, ErrIdentityMismatch},
		{"failed-version", true, func(r *clientv3.TxnResponse) (*clientv3.TxnResponse, error) {
			r.Responses[1].GetResponseRange().Kvs[0].Version = 0
			return r, nil
		}, ErrIdentityMismatch},
		{"failed-restore", true, func(r *clientv3.TxnResponse) (*clientv3.TxnResponse, error) {
			r.Responses[1].GetResponseRange().Kvs[0].Value = []byte("another-restore")
			return r, nil
		}, ErrIdentityMismatch},
		{"failed-nested-header", true, func(r *clientv3.TxnResponse) (*clientv3.TxnResponse, error) {
			r.Responses[0].GetResponseRange().Header = &pb.ResponseHeader{Revision: r.Header.Revision + 1}
			return r, nil
		}, ErrIdentityMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b.client.KV = base
			view, err := b.ReadSandboxView(ctx, in.SandboxID)
			require.NoError(t, err)
			controlValue, err := encodeDomainRecord(view.Records().Control)
			require.NoError(t, err)
			fault := &faultKV{KV: base, match: func(ops []clientv3.Op) bool { return len(ops) == 0 }, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				require.Equal(t, !test.fail, r.Succeeded)
				return test.after(r)
			}}
			if test.fail {
				fault.before = func() {
					_, err := raw.Put(ctx, view.keys[3], controlValue)
					require.NoError(t, err)
				}
			}
			b.client.KV = fault
			require.ErrorIs(t, b.ConfirmSandboxView(ctx, view), test.want)
		})
	}
	b.client.KV = base
	wrongStorage, err := NewWorkspaceIdentity("s3", "other-storage", "bucket", "team/project/")
	require.NoError(t, err)
	calls := 0
	b.client.KV = &faultKV{KV: base, match: func(ops []clientv3.Op) bool { calls++; return false }}
	_, err = b.ReadWorkspaceView(ctx, wrongStorage)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	_, err = b.ReadSandboxView(nil, in.SandboxID)
	require.ErrorIs(t, err, ErrInvalidRecord)
	_, err = b.ReadSandboxView(ctx, "../foreign")
	require.ErrorIs(t, err, ErrInvalidRecord)
	require.Zero(t, calls)
}
