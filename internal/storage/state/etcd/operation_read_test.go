package etcd

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func operationControlFixture(t *testing.T, mode string) (*Backend, *clientv3.Client, AcquireIntentInput, []string) {
	t.Helper()
	b, raw, in, c, proof := publicationFixture(t, mode)
	result, err := b.PublishRuntime(context.Background(), c, proof)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	placement, err := b.namespace.placementKey(in.SandboxID)
	require.NoError(t, err)
	control, _, err := b.namespace.sandboxKeys(c.workspace.Partition(), in.SandboxID, "read")
	require.NoError(t, err)
	owner, fence, err := b.namespace.workspaceKeys(c.workspace)
	require.NoError(t, err)
	index, err := b.namespace.runtimeIndexKey("uid")
	require.NoError(t, err)
	return b, raw, in, []string{placement, control, owner, fence, index}
}
func operationChangePoint(t *testing.T, raw *clientv3.Client, key, field string, value any) {
	t.Helper()
	ctx := context.Background()
	resp, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, resp.Kvs, 1)
	var wire map[string]any
	require.NoError(t, json.Unmarshal(resp.Kvs[0].Value, &wire))
	if field == "runtime.id" || field == "runtime.uid" || field == "runtime.boot_id" {
		wire["runtime"].(map[string]any)[field[8:]] = value
	} else {
		wire[field] = value
	}
	changed, err := json.Marshal(wire)
	require.NoError(t, err)
	// Recreate immutable metadata; each tamper must test decoded identity rather
	// than merely trigger the index rewrite envelope check.
	_, err = raw.Delete(ctx, key)
	require.NoError(t, err)
	_, err = raw.Put(ctx, key, string(changed))
	require.NoError(t, err)
}
func TestOperationControlCoherentBundle(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"plain", "fuse"} {
		t.Run(mode, func(t *testing.T) {
			b, _, in, keys := operationControlFixture(t, mode)
			var captured []*mvccpb.KeyValue
			reads := 0
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
				reads++
				want := keys[:1]
				if reads == 2 {
					want = keys[1:2]
				}
				if reads == 3 {
					want = keys
				}
				require.LessOrEqual(t, reads, 3)
				require.Len(t, ops, len(want))
				for i, op := range ops {
					require.True(t, op.IsGet())
					require.Empty(t, op.RangeBytes())
					require.Equal(t, want[i], string(op.KeyBytes()))
				}
				return reads == 3
			}, after: func(resp *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				for _, response := range resp.Responses {
					captured = append(captured, response.GetResponseRange().Kvs[0])
				}
				return resp, err
			}}
			got, err := b.loadOperationControl(ctx, in.SandboxID)
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Equal(t, PhaseActive, got.Control.Phase)
			require.Equal(t, in.Workspace.Partition(), got.Partition)
			require.Equal(t, keys, got.Keys)
			require.Len(t, got.KVs, 5)
			require.Equal(t, 3, reads)
			for i, kv := range got.KVs {
				require.Equal(t, keys[i], string(kv.Key))
				require.Zero(t, kv.Lease)
				require.Positive(t, kv.CreateRevision)
				require.GreaterOrEqual(t, kv.ModRevision, kv.CreateRevision)
			}
			require.Greater(t, got.KVs[1].ModRevision, got.KVs[1].CreateRevision)
			require.Greater(t, got.KVs[2].ModRevision, got.KVs[2].CreateRevision)
			captured[0].Key[0] = 'x'
			captured[0].Value[0] = 'x'
			require.Equal(t, keys[0], string(got.KVs[0].Key))
			require.Equal(t, byte('{'), got.KVs[0].Value[0])
		})
	}
	for _, phase := range []SandboxPhase{PhasePublishing, PhaseWorkspaceExclusive, PhaseDestroying, PhaseCleanupPending} {
		t.Run("closed "+string(phase), func(t *testing.T) {
			b, raw, in, keys := operationControlFixture(t, "plain")
			resp, err := raw.Get(ctx, keys[1])
			require.NoError(t, err)
			var control SandboxControlRecord
			require.NoError(t, decodeDomainRecord(resp.Kvs[0], &control))
			control.Phase = phase
			if phase == PhasePublishing {
				control.Runtime = nil
				control.MountAttempt = 0
			}
			wire, err := encodeDomainRecord(control)
			require.NoError(t, err)
			_, err = raw.Put(ctx, keys[1], wire)
			require.NoError(t, err)
			_, err = raw.Put(ctx, keys[2], "corrupt unrelated owner")
			require.NoError(t, err)
			got, err := b.loadOperationControl(ctx, in.SandboxID)
			require.Nil(t, got)
			require.ErrorIs(t, err, ErrOperationAdmissionClosed)
		})
	}
	for i := 0; i < 5; i++ {
		t.Run(fmt.Sprintf("missing %d", i), func(t *testing.T) {
			b, raw, in, keys := operationControlFixture(t, "plain")
			_, err := raw.Delete(ctx, keys[i])
			require.NoError(t, err)
			got, err := b.loadOperationControl(ctx, in.SandboxID)
			require.Nil(t, got)
			want := ErrCorruptRecord
			if i < 2 {
				want = ErrOperationAdmissionClosed
			}
			require.ErrorIs(t, err, want)
		})
		t.Run(fmt.Sprintf("tamper %d", i), func(t *testing.T) {
			b, raw, in, keys := operationControlFixture(t, "plain")
			operationChangePoint(t, raw, keys[i], "generation", 99)
			got, err := b.loadOperationControl(ctx, in.SandboxID)
			require.Nil(t, got)
			require.ErrorIs(t, err, ErrCorruptRecord)
		})
		t.Run(fmt.Sprintf("restore %d", i), func(t *testing.T) {
			b, raw, in, keys := operationControlFixture(t, "plain")
			operationChangePoint(t, raw, keys[i], "restore_epoch", "old")
			got, err := b.loadOperationControl(ctx, in.SandboxID)
			require.Nil(t, got)
			require.ErrorIs(t, err, ErrIdentityMismatch)
		})
		for _, invalid := range []string{"revision", "attribution", "malformed"} {
			t.Run(fmt.Sprintf("envelope %d %s", i, invalid), func(t *testing.T) {
				b, _, in, _ := operationControlFixture(t, "plain")
				b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) == 5 }, after: func(resp *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
					require.NoError(t, err)
					kv := resp.Responses[i].GetResponseRange().Kvs[0]
					switch invalid {
					case "revision":
						kv.CreateRevision = 0
					case "attribution":
						kv.Key = []byte("wrong")
					case "malformed":
						kv.Value = []byte("null")
					}
					return resp, err
				}}
				got, err := b.loadOperationControl(ctx, in.SandboxID)
				require.Nil(t, got)
				require.ErrorIs(t, err, ErrCorruptRecord)
			})
		}

		t.Run(fmt.Sprintf("lease %d", i), func(t *testing.T) {
			b, _, in, _ := operationControlFixture(t, "plain")
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) == 5 }, after: func(resp *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
				require.NoError(t, err)
				resp.Responses[i].GetResponseRange().Kvs[0].Lease = 17
				return resp, err
			}}
			got, err := b.loadOperationControl(ctx, in.SandboxID)
			require.Nil(t, got)
			require.ErrorIs(t, err, ErrCorruptRecord)
		})
	}
	for _, i := range []int{2, 4} {
		for _, field := range []string{"sandbox_id", "intent_id", "workspace_hash", "runtime.id", "runtime.uid", "runtime.boot_id", "mount_attempt"} {
			if i == 4 && field == "mount_attempt" {
				continue
			}
			t.Run(fmt.Sprintf("identity %d %s", i, field), func(t *testing.T) {
				b, raw, in, keys := operationControlFixture(t, "plain")
				var value any = "other"
				if field == "workspace_hash" {
					value = domainHash
				}
				if field == "mount_attempt" {
					value = 1
				}
				operationChangePoint(t, raw, keys[i], field, value)
				got, err := b.loadOperationControl(ctx, in.SandboxID)
				require.Nil(t, got)
				require.ErrorIs(t, err, ErrCorruptRecord)
			})
		}
	}
	for _, point := range []int{0, 1} {
		t.Run(fmt.Sprintf("discovery replacement %d", point), func(t *testing.T) {
			b, raw, in, keys := operationControlFixture(t, "plain")
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) == 5 }, before: func() {
				if point == 0 {
					operationChangePoint(t, raw, keys[0], "intent_id", "another-intent")
				} else {
					operationChangePoint(t, raw, keys[1], "runtime.uid", "replacement-uid")
					operationChangePoint(t, raw, keys[2], "runtime.uid", "replacement-uid")
					original, err := raw.Get(ctx, keys[4])
					require.NoError(t, err)
					var index RuntimeIndexRecord
					require.NoError(t, decodeDomainRecord(original.Kvs[0], &index))
					index.Runtime.UID = "replacement-uid"
					key, err := b.namespace.runtimeIndexKey(index.Runtime.UID)
					require.NoError(t, err)
					wire, err := encodeDomainRecord(index)
					require.NoError(t, err)
					_, err = raw.Put(ctx, key, wire)
					require.NoError(t, err)
				}
			}}
			got, err := b.loadOperationControl(ctx, in.SandboxID)
			require.Nil(t, got)
			require.ErrorIs(t, err, ErrConflict)
		})
	}
	for _, kind := range []string{"placement missing", "control missing", "phase closed", "operator restore"} {
		t.Run("final "+kind, func(t *testing.T) {
			b, raw, in, keys := operationControlFixture(t, "plain")
			b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool { return len(ops) == 5 }, before: func() {
				switch kind {
				case "placement missing":
					_, err := raw.Delete(ctx, keys[0])
					require.NoError(t, err)
				case "control missing":
					_, err := raw.Delete(ctx, keys[1])
					require.NoError(t, err)
				case "phase closed":
					operationChangePoint(t, raw, keys[1], "phase", PhaseDestroying)
					_, err := raw.Delete(ctx, keys[2])
					require.NoError(t, err)
				case "operator restore":
					_, err := raw.Put(ctx, b.restoreKey, "changed")
					require.NoError(t, err)
				}
			}}
			got, err := b.loadOperationControl(ctx, in.SandboxID)
			require.Nil(t, got)
			want := ErrOperationAdmissionClosed
			if kind == "operator restore" {
				want = ErrIdentityMismatch
			}
			require.ErrorIs(t, err, want)
		})
	}

	t.Run("index immutable", func(t *testing.T) {
		b, raw, in, keys := operationControlFixture(t, "plain")
		r, err := raw.Get(ctx, keys[4])
		require.NoError(t, err)
		_, err = raw.Put(ctx, keys[4], string(r.Kvs[0].Value))
		require.NoError(t, err)
		got, err := b.loadOperationControl(ctx, in.SandboxID)
		require.Nil(t, got)
		require.ErrorIs(t, err, ErrCorruptRecord)
	})
	t.Run("lookup identity and context", func(t *testing.T) {
		b, raw, in, _ := operationControlFixture(t, "plain")
		_, err := b.loadOperationControl(nil, in.SandboxID)
		require.ErrorIs(t, err, ErrInvalidRecord)
		_, err = b.loadOperationControl(ctx, "../bad")
		require.ErrorIs(t, err, ErrInvalidRecord)
		_, err = b.loadOperationControl(ctx, "absent")
		require.ErrorIs(t, err, ErrOperationAdmissionClosed)
		_, err = raw.Put(ctx, b.restoreKey, "changed")
		require.NoError(t, err)
		_, err = b.loadOperationControl(ctx, in.SandboxID)
		require.ErrorIs(t, err, ErrIdentityMismatch)
	})
}
