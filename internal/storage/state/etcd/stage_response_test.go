package etcd

import (
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"strings"
	"testing"
)

func TestStageReceiptStrict(t *testing.T) {
	ref := StageReference{Namespace: "/sandbox/v1/authority/cell/", Partition: 0, RequestID: "request", StageID: "stage", AttemptID: "11234567-89ab-4cde-8012-3456789abcde", Digest: strings.Repeat("a", 64), RestoreEpoch: "epoch"}
	wire := `{"version":1,"namespace":"/sandbox/v1/authority/cell/","partition":0,"request_id":"request","stage_id":"stage","attempt_id":"11234567-89ab-4cde-8012-3456789abcde","digest":"` + strings.Repeat("a", 64) + `","restore_epoch":"epoch","outcome":"committed"}`
	key := ref.Namespace + "p/00/stages/request/stage/" + ref.AttemptID + "/receipt"
	cases := []struct {
		name   string
		change func(*mvccpb.KeyValue)
	}{
		{"leased", func(k *mvccpb.KeyValue) { k.Lease = 1 }}, {"mutated", func(k *mvccpb.KeyValue) { k.ModRevision++ }}, {"zero-revision", func(k *mvccpb.KeyValue) { k.CreateRevision = 0; k.ModRevision = 0 }}, {"wrong-key", func(k *mvccpb.KeyValue) { k.Key = []byte(key + "x") }},
		{"missing-zero-partition", func(k *mvccpb.KeyValue) { k.Value = []byte(strings.Replace(wire, `"partition":0,`, "", 1)) }},
		{"duplicate", func(k *mvccpb.KeyValue) {
			k.Value = []byte(strings.Replace(wire, `"partition":0`, `"partition":0,"partition":0`, 1))
		}},
		{"null", func(k *mvccpb.KeyValue) {
			k.Value = []byte(strings.Replace(wire, `"partition":0`, `"partition":null`, 1))
		}},
		{"unknown", func(k *mvccpb.KeyValue) {
			k.Value = []byte(strings.Replace(wire, `"partition":0`, `"partition":0,"dst":"ignored"`, 1))
		}},
		{"casing", func(k *mvccpb.KeyValue) { k.Value = []byte(strings.Replace(wire, `"partition"`, `"Partition"`, 1)) }},
		{"trailing", func(k *mvccpb.KeyValue) { k.Value = append(k.Value, []byte(`{}`)...) }},
		{"invalid-utf8", func(k *mvccpb.KeyValue) { k.Value = []byte(strings.Replace(wire, "epoch", "\xff", 1)) }},
		{"version", func(k *mvccpb.KeyValue) { k.Value = []byte(strings.Replace(wire, `"version":1`, `"version":2`, 1)) }},
		{"ref", func(k *mvccpb.KeyValue) {
			k.Value = []byte(strings.Replace(wire, `"stage_id":"stage"`, `"stage_id":"other"`, 1))
		}},
		{"outcome", func(k *mvccpb.KeyValue) { k.Value = []byte(strings.Replace(wire, "committed", "unknown", 1)) }},
		{"oversize", func(k *mvccpb.KeyValue) { k.Value = append(k.Value, []byte(strings.Repeat(" ", maxRecordBytes))...) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), CreateRevision: 5, ModRevision: 5}
			tc.change(kv)
			outcome, err := decodeReceipt(kv, ref)
			require.ErrorIs(t, err, ErrCorruptReceipt)
			require.Equal(t, OutcomeUnknown, outcome)
		})
	}
	for _, outcome := range []Outcome{OutcomeCommitted, OutcomeAborted} {
		kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(strings.Replace(wire, "committed", string(outcome), 1)), CreateRevision: 5, ModRevision: 5}
		got, err := decodeReceipt(kv, ref)
		require.NoError(t, err)
		require.Equal(t, outcome, got)
	}
}
