package etcd

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func operationRecordFixture() OperationRecord {
	return OperationRecord{Version: 1, Reference: OperationReference{Namespace: "/test/scope/cell/", RestoreEpoch: "epoch", RequestID: "request", SandboxID: "sandbox", OperationID: "01234567-89ab-4cde-8012-3456789abcde", Digest: strings.Repeat("a", 64), Partition: 0xaa, Kind: OperationData, LeaseID: 31}, WorkspaceHash: strings.Repeat("a", 64), IntentID: "intent", Generation: 1, DataGateEpoch: 1, ControlRevision: 8, Runtime: RuntimeReference{ID: "id", UID: "uid", BootID: "boot"}, Snapshot: SnapshotReference{Version: "v1", Digest: strings.Repeat("b", 64)}, ExpiresAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
}
func operationTestKV(t *testing.T, v any) *mvccpb.KeyValue {
	t.Helper()
	wire, err := json.Marshal(v)
	require.NoError(t, err)
	return &mvccpb.KeyValue{Key: []byte("key"), Value: wire, Lease: 31, CreateRevision: 10, ModRevision: 10}
}
func TestOperationRecordStrictCodec(t *testing.T) {
	r := operationRecordFixture()
	kv := operationTestKV(t, r)
	t.Run("leased roundtrip and copies", func(t *testing.T) {
		wire, err := encodeOperationRecord(r)
		require.NoError(t, err)
		require.JSONEq(t, string(kv.Value), wire)
		var got OperationRecord
		require.NoError(t, decodeOperationRecord(kv, &got))
		require.Equal(t, r, got)
		saved := got
		kv.Value[0] = 'x'
		require.Equal(t, saved, got)
		kv.Value[0] = '{'
		require.ErrorIs(t, decodeDomainRecord(kv, &got), ErrCorruptRecord)
		require.ErrorIs(t, decodeOperationRecord(kv, nil), ErrCorruptRecord)
	})
	cases := map[string]func(*mvccpb.KeyValue){
		"lease zero": func(k *mvccpb.KeyValue) { k.Lease = 0 }, "other lease": func(k *mvccpb.KeyValue) { k.Lease = 32 }, "negative lease": func(k *mvccpb.KeyValue) { k.Lease = -1 },
		"rewritten": func(k *mvccpb.KeyValue) { k.ModRevision++ }, "zero revision": func(k *mvccpb.KeyValue) { k.CreateRevision = 0; k.ModRevision = 0 }, "negative revision": func(k *mvccpb.KeyValue) { k.CreateRevision = -1; k.ModRevision = -1 },
		"null": func(k *mvccpb.KeyValue) { k.Value = []byte("null") }, "duplicate": func(k *mvccpb.KeyValue) {
			k.Value = []byte(strings.Replace(string(k.Value), `"version":1`, `"version":1,"version":1`, 1))
		},
		"missing": func(k *mvccpb.KeyValue) { k.Value = []byte(strings.Replace(string(k.Value), `"version":1,`, "", 1)) }, "unknown": func(k *mvccpb.KeyValue) {
			k.Value = []byte(strings.Replace(string(k.Value), `"version":1`, `"version":1,"other":1`, 1))
		},
		"nested null": func(k *mvccpb.KeyValue) {
			k.Value = []byte(strings.Replace(string(k.Value), `"lease_id":31`, `"lease_id":null`, 1))
		}, "nested duplicate": func(k *mvccpb.KeyValue) {
			k.Value = []byte(strings.Replace(string(k.Value), `"lease_id":31`, `"lease_id":31,"lease_id":31`, 1))
		},
		"trailing": func(k *mvccpb.KeyValue) { k.Value = append(k.Value, []byte(" {}")...) }, "oversize": func(k *mvccpb.KeyValue) { k.Value = append(k.Value, []byte(strings.Repeat(" ", 4097))...) }, "utf8": func(k *mvccpb.KeyValue) { k.Value = append(k.Value, 0xff) },
		"nil UUID": func(k *mvccpb.KeyValue) {
			k.Value = []byte(strings.Replace(string(k.Value), r.Reference.OperationID, "00000000-0000-0000-0000-000000000000", 1))
		}, "wire lease zero": func(k *mvccpb.KeyValue) {
			k.Value = []byte(strings.Replace(string(k.Value), `"lease_id":31`, `"lease_id":0`, 1))
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			bad := operationTestKV(t, r)
			change(bad)
			old := operationRecordFixture()
			old.IntentID = "old"
			saved := old
			require.ErrorIs(t, decodeOperationRecord(bad, &old), ErrCorruptRecord)
			require.Equal(t, saved, old)
		})
	}
	t.Run("record size boundary", func(t *testing.T) {
		bounded := operationTestKV(t, r)
		bounded.Value = append(bounded.Value, []byte(strings.Repeat(" ", 4096-len(bounded.Value)))...)
		var got OperationRecord
		require.NoError(t, decodeOperationRecord(bounded, &got))
		bounded.Value = append(bounded.Value, ' ')
		require.ErrorIs(t, decodeOperationRecord(bounded, &got), ErrCorruptRecord)
	})

	t.Run("invalid outbound metadata", func(t *testing.T) {
		for _, change := range []func(*OperationRecord){func(r *OperationRecord) { r.Reference.LeaseID = 0 }, func(r *OperationRecord) { r.Reference.Kind = "other" }, func(r *OperationRecord) { r.Reference.RequestID = strings.Repeat("a", 129) }, func(r *OperationRecord) { r.Reference.OperationID = "../escape" }, func(r *OperationRecord) { r.Reference.Partition++ }, func(r *OperationRecord) { r.ControlRevision = 0 }, func(r *OperationRecord) { r.Reference.Namespace = "/other/" }} {
			bad := r
			change(&bad)
			_, err := encodeOperationRecord(bad)
			require.Error(t, err)
		}
	})
	t.Run("key identity", func(t *testing.T) {
		n, err := NewNamespace("/test", "scope", "cell")
		require.NoError(t, err)
		token, guard, receipt, mutation, err := n.operationKeys(r.Reference)
		require.NoError(t, err)
		require.Equal(t, "/test/scope/cell/p/aa/sandboxes/sandbox/operations/01234567-89ab-4cde-8012-3456789abcde", token)
		require.Equal(t, "/test/scope/cell/p/aa/operation-attempts/01234567-89ab-4cde-8012-3456789abcde/guard", guard)
		require.Equal(t, "/test/scope/cell/p/aa/operation-attempts/01234567-89ab-4cde-8012-3456789abcde/receipt", receipt)
		require.Equal(t, "/test/scope/cell/p/aa/sandboxes/sandbox/mutation", mutation)
		other := r.Reference
		other.RequestID = "another-request"
		tk, gk, rk, mk, err := n.operationKeys(other)
		require.NoError(t, err)
		require.Equal(t, []string{token, guard, receipt, mutation}, []string{tk, gk, rk, mk})
		bad := r.Reference
		bad.Namespace = "/" + strings.Repeat("a", 512) + "/scope/cell/"
		_, _, _, _, err = n.operationKeys(bad)
		require.Error(t, err)

		for _, change := range []func(*OperationReference){func(r *OperationReference) { r.RequestID = "../../escape" }, func(r *OperationReference) { r.SandboxID = "../escape" }, func(r *OperationReference) { r.Namespace = "/else/scope/cell/" }, func(r *OperationReference) { r.LeaseID = 0 }} {
			bad := r.Reference
			change(&bad)
			_, _, _, _, err = n.operationKeys(bad)
			require.Error(t, err)
		}
	})
	t.Run("receipt schema and completion", func(t *testing.T) {
		receipt := operationReceipt{Version: 1, Reference: r.Reference, Outcome: OperationCommitted}
		rk := operationTestKV(t, receipt)
		wire, err := encodeOperationReceipt(receipt)
		require.NoError(t, err)
		require.JSONEq(t, string(rk.Value), wire)
		var got operationReceipt
		require.NoError(t, decodeOperationReceipt(rk, &got))
		require.Equal(t, receipt, got)
		require.NoError(t, validateOperationCompletion(kv, rk))
		rk.ModRevision++
		rk.CreateRevision++
		require.ErrorIs(t, validateOperationCompletion(kv, rk), ErrCorruptReceipt)
		for name, change := range cases {
			t.Run(name, func(t *testing.T) {
				bad := operationTestKV(t, receipt)
				change(bad)
				old := receipt
				old.Outcome = OperationAborted
				saved := old
				require.ErrorIs(t, decodeOperationReceipt(bad, &old), ErrCorruptReceipt)
				require.Equal(t, saved, old)
			})
		}
		bounded := operationTestKV(t, receipt)
		bounded.Value = append(bounded.Value, []byte(strings.Repeat(" ", 2048-len(bounded.Value)))...)
		require.NoError(t, decodeOperationReceipt(bounded, &got))
		bounded.Value = append(bounded.Value, ' ')
		require.ErrorIs(t, decodeOperationReceipt(bounded, &got), ErrCorruptReceipt)
		require.ErrorIs(t, decodeOperationReceipt(rk, nil), ErrCorruptReceipt)
		mismatch := receipt
		mismatch.Reference.RequestID = "other"
		require.ErrorIs(t, validateOperationCompletion(kv, operationTestKV(t, mismatch)), ErrCorruptReceipt)

		for _, outcome := range []OperationOutcome{OperationUnknown, OperationExpired, "other"} {
			bad := receipt
			bad.Outcome = outcome
			_, err := encodeOperationReceipt(bad)
			require.Error(t, err)
		}
	})
}
