package etcd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
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

// Only a no-op Delete may describe the revision before the receipt write.
// Pinning the other response kinds here prevents widening that exception.
func TestStageResponseHeaderBoundaries(t *testing.T) {
	headers := []struct {
		name                    string
		header                  *pb.ResponseHeader
		wantDelete0, wantStrict bool
	}{
		{"nil", nil, true, true},
		{"equal-zero-cluster", &pb.ResponseHeader{Revision: 10}, true, true},
		{"equal-matching-cluster", &pb.ResponseHeader{ClusterId: 7, Revision: 10}, true, true},
		{"outer-minus-one-zero-cluster", &pb.ResponseHeader{Revision: 9}, true, false},
		{"outer-minus-one-matching-cluster", &pb.ResponseHeader{ClusterId: 7, Revision: 9}, true, false},
		{"outer-minus-two", &pb.ResponseHeader{Revision: 8}, false, false},
		{"future", &pb.ResponseHeader{Revision: 11}, false, false},
		{"zero", &pb.ResponseHeader{}, false, false},
		{"negative", &pb.ResponseHeader{Revision: -1}, false, false},
		{"foreign-equal", &pb.ResponseHeader{ClusterId: 8, Revision: 10}, false, false},
		{"foreign-outer-minus-one", &pb.ResponseHeader{ClusterId: 8, Revision: 9}, false, false},
	}
	for _, kind := range []string{"Delete0", "Delete1", "Put", "ReceiptPut", "Range"} {
		t.Run(kind, func(t *testing.T) {
			for _, tc := range headers {
				t.Run(tc.name, func(t *testing.T) {
					outer := &pb.ResponseHeader{ClusterId: 7, Revision: 10}
					response := &clientv3.TxnResponse{Header: outer, Succeeded: true}
					if kind == "Range" {
						b := &Backend{clusterID: 7, identityKey: "identity", restoreKey: "restore", identityValue: "id", restoreEpoch: "epoch"}
						for i, key := range []string{"identity", "restore"} {
							point := &pb.RangeResponse{Count: 1, Kvs: []*mvccpb.KeyValue{{Key: []byte(key), Value: []byte([]string{"id", "epoch"}[i]), CreateRevision: 1, ModRevision: 1}}}
							if i == 0 {
								point.Header = tc.header
							}
							response.Responses = append(response.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseRange{ResponseRange: point}})
						}
						_, err := b.stageEvidencePoints(response, []string{"identity", "restore"})
						if tc.wantStrict {
							require.NoError(t, err)
						} else {
							require.ErrorIs(t, err, ErrOutcomeUnknown)
						}
						return
					}
					writes := []Write{{Key: "business", Delete: kind != "Put"}}
					receipt := &pb.PutResponse{}
					var business *pb.ResponseOp
					if kind == "Put" {
						business = &pb.ResponseOp{Response: &pb.ResponseOp_ResponsePut{ResponsePut: &pb.PutResponse{Header: tc.header}}}
					} else {
						deleted := &pb.DeleteRangeResponse{Header: tc.header}
						if kind == "Delete1" {
							deleted.Deleted = 1
						}
						if kind == "ReceiptPut" {
							deleted.Header = &pb.ResponseHeader{Revision: 9}
							receipt.Header = tc.header
						}
						business = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: deleted}}
					}
					response.Responses = []*pb.ResponseOp{business, {Response: &pb.ResponseOp_ResponsePut{ResponsePut: receipt}}}
					want := tc.wantStrict
					if kind == "Delete0" {
						want = tc.wantDelete0
					}
					require.Equal(t, want, stageCommitResponses(response, writes))
				})
			}
		})
	}
}
