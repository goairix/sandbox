package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This sample reads actual committed values from an isolated synthetic creation.
// It measures retained raw values, not MVCC/WAL/replicas or throughput/capacity.
func TestRuntimePublicationCapacitySample(t *testing.T) {
	for _, size := range []int{16, 4096, 8192} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			base, raw := integrationBackend(t)
			ctx := context.Background()
			issuer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
			root := issuer.Public().(ed25519.PublicKey)
			o := integrationOptions(base, raw)
			o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{root}}
			o.Clock = goodAuthorityClock()
			b, err := New(ctx, o)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			// Mutating caller configuration cannot replace the backend's pinned authority.
			for i := range root {
				root[i] = 0
			}
			o.PublicationTrust.Target = "caller-changed"
			in := acquisitionInput(t, "publication-capacity")
			acquire, err := b.AcquireIntent(ctx, in)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, acquire.Outcome)
			c, err := b.ClaimCreation(ctx, in.Workspace, in.IntentID, "capacity-worker", 30*time.Second)
			require.NoError(t, err)
			defer b.ReleaseCreationClaim(ctx, c)
			payload := json.RawMessage(`{"synthetic":"` + strings.Repeat("x", size-len(`{"synthetic":""}`)) + `"}`)
			require.Len(t, payload, size)
			dispatch, err := b.DeclareRuntimeDispatch(ctx, c, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "kubernetes", Payload: payload})
			require.NoError(t, err)
			certificate := preparationCertificate(t, b, dispatch.Entry.Record, issuer, "fuse", RuntimeReference{ID: "id", UID: "uid", BootID: "boot"})
			bind, err := b.BindRuntime(ctx, c, certificate)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, bind.Outcome)
			mount, err := b.ConsumeRuntimeMount(ctx, c)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, mount.Outcome)
			proof := publicationProof(t, b, c, certificate, nil)
			result, err := b.PublishRuntime(ctx, c, proof)
			require.NoError(t, err)
			require.Equal(t, OutcomeCommitted, result.Outcome)
			bundle, err := b.loadRuntimePreparation(ctx, in.Workspace, in.IntentID, "")
			require.NoError(t, err)
			categories := map[string]string{}
			for _, kv := range bundle.DispatchKVs {
				categories[string(kv.Key)] = "dispatch"
			}
			for _, kv := range bundle.BindingKVs {
				categories[string(kv.Key)] = "binding including reserved index"
			}
			for _, kv := range bundle.MountKVs {
				categories[string(kv.Key)] = "mount"
			}
			jk, pk, err := b.namespace.runtimePublicationKeys(in.Workspace.Partition(), in.IntentID)
			require.NoError(t, err)
			_, prk, err := b.stageKeys(result.Reference)
			require.NoError(t, err)
			for _, k := range []string{jk, pk, prk} {
				categories[k] = "publication"
			}
			owner, fence, err := b.namespace.workspaceKeys(in.Workspace)
			require.NoError(t, err)
			control, snapshot, err := b.namespace.sandboxKeys(in.Workspace.Partition(), in.SandboxID, in.SnapshotVersion)
			require.NoError(t, err)
			intent, err := b.namespace.intentKey(in.Workspace.Partition(), in.IntentID)
			require.NoError(t, err)
			request, err := b.namespace.requestKey(c.request.RequestHash)
			require.NoError(t, err)
			placement, err := b.namespace.placementKey(in.SandboxID)
			require.NoError(t, err)
			for _, k := range []string{owner, fence, control, intent, request, placement} {
				categories[k] = "original domains except snapshot"
			}
			categories[snapshot] = "fixed fixture snapshot"
			_, ark, err := b.stageKeys(acquire.Reference)
			require.NoError(t, err)
			categories[ark] = "Acquire receipt"
			values, err := raw.Get(ctx, b.namespace.Root(), clientv3.WithPrefix())
			require.NoError(t, err)
			totals := map[string]int{}
			counts := map[string]int{}
			retained := 0
			for _, kv := range values.Kvs {
				key := string(kv.Key)
				if kv.Lease != 0 || strings.HasPrefix(key, b.namespace.Root()+"meta/") {
					continue
				}
				category, ok := categories[key]
				require.True(t, ok, "unclassified permanent key %s", key)
				totals[category] += len(kv.Value)
				counts[category]++
				retained += len(kv.Value)
			}
			require.Len(t, categories, 20)
			require.Equal(t, 3, counts["dispatch"])
			require.Equal(t, 4, counts["binding including reserved index"])
			require.Equal(t, 2, counts["mount"])
			require.Equal(t, 3, counts["publication"])
			require.Equal(t, 6, counts["original domains except snapshot"])
			require.Equal(t, 1, counts["fixed fixture snapshot"])
			require.Equal(t, 1, counts["Acquire receipt"])
			newValues := totals["dispatch"] + totals["binding including reserved index"] + totals["mount"] + totals["publication"]
			for _, category := range []string{"dispatch", "binding including reserved index", "mount", "publication", "original domains except snapshot", "fixed fixture snapshot", "Acquire receipt"} {
				t.Logf("%s: records=%d raw_value_bytes=%d", category, counts[category], totals[category])
			}
			t.Logf("SYNTHETIC dispatch_input=%d bytes; fixed fixture snapshot_payload=%d bytes; certificate_wire=%d and proof_wire=%d already included in retained values; configured root=%d bytes (outside etcd)", len(payload), len(in.Payload), len(certificate), len(proof), ed25519.PublicKeySize)
			t.Logf("new journal/index/receipt records=12 subtotal=%d raw bytes; full retained records=20 subtotal=%d raw bytes (7 original domains + Acquire receipt included); full including one configured root=%d bytes. Keys/MVCC/WAL/history/replicas excluded; no throughput or N capacity claim.", newValues, retained, retained+ed25519.PublicKeySize)
		})
	}
}
