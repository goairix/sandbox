package etcd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// This codec-only fixture reuses the quota-return history stamps and valid
// settlement reference. It provides no client, runtime authority or death proof.
func coldSettlementDecodeFixture(t *testing.T) (*Backend, *coldQuotaReturnHistory, *mvccpb.KeyValue, StageReference) {
	t.Helper()
	b, h, _, _, r, _ := coldQuotaProofFixture(t)
	ref := r.PreparationSettlement
	h.reservation.Version = 1
	h.reservation.RestoreEpoch = b.restoreEpoch
	h.reservation.ProfileDigest = b.preparationDigest
	h.reservation.Phase = "settled"
	h.reservation.Settlement = StageAttemptLocator{
		Namespace: ref.Namespace, RestoreEpoch: ref.RestoreEpoch, Partition: ref.Partition,
		RequestID: ref.RequestID, StageID: ref.StageID, AttemptID: ref.AttemptID,
	}
	p := &h.publication.entry.Record
	p.IntentID, p.SandboxID, p.WorkspaceHash = r.Owner.IntentID, r.Owner.SandboxID, r.Owner.WorkspaceHash
	p.Generation, p.RestoreEpoch = r.Owner.Generation, r.RestoreEpoch
	p.Attempt = h.reservation.Settlement
	marker, err := coldEncode(coldPreparationWork{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, IntentID: r.Owner.IntentID, Phase: "settled"})
	require.NoError(t, err)
	h.workKV.Value = []byte(marker)

	wire, err := encodeReceipt(ref, OutcomeCommitted)
	require.NoError(t, err)
	key, err := b.runtimeDispatchReceiptKey(h.reservation.Settlement)
	require.NoError(t, err)
	receipt := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), Version: 1, CreateRevision: 30, ModRevision: 30}
	return b, h, receipt, ref
}

// A valid already-paid receipt must yield the original reference without any
// client. Returning a new attempt or requiring another RPC breaks worker reuse.
func TestColdSettlementDecodeUsesProvidedReceipt(t *testing.T) {
	b, h, receipt, want := coldSettlementDecodeFixture(t)
	got, err := b.decodeColdSettlement(h.reservation, h.reservedKV, h.workKV, h.publication, receipt)
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// Every original source invariant must reject before receipt validation and
// before the public wrapper sends its bounded receipt read. The host model
// records real metadata RPCs; it does not authenticate native etcd history.
func TestColdSettlementDecodeRejectsSourceBeforeReceiptRead(t *testing.T) {
	model, server := workerTestBackend(t)
	for _, tc := range []struct {
		name   string
		change func(*coldQuotaReturnHistory)
	}{
		{"phase", func(h *coldQuotaReturnHistory) { h.reservation.Phase = "pending" }},
		{"missing-history", func(h *coldQuotaReturnHistory) { h.publication = nil }},
		{"intent", func(h *coldQuotaReturnHistory) { h.reservation.Owner.IntentID = "other-intent" }},
		{"sandbox", func(h *coldQuotaReturnHistory) { h.reservation.Owner.SandboxID = "other-sandbox" }},
		{"workspace", func(h *coldQuotaReturnHistory) { h.reservation.Owner.WorkspaceHash = strings.Repeat("0", 64) }},
		{"generation", func(h *coldQuotaReturnHistory) { h.reservation.Owner.Generation++ }},
		{"request", func(h *coldQuotaReturnHistory) { h.reservation.Owner.RequestID = "other-request" }},
		{"partition", func(h *coldQuotaReturnHistory) { h.reservation.Owner.Partition++ }},
		{"restore-epoch", func(h *coldQuotaReturnHistory) { h.reservation.RestoreEpoch = "other-epoch" }},
		{"missing-reservation", func(h *coldQuotaReturnHistory) { h.reservedKV = nil }},
		{"missing-work", func(h *coldQuotaReturnHistory) { h.workKV = nil }},
		{"different-modification", func(h *coldQuotaReturnHistory) { h.workKV.ModRevision++ }},
		{"same-publication-birth", func(h *coldQuotaReturnHistory) {
			h.reservedKV.ModRevision, h.workKV.ModRevision = 20, 20
		}},
		{"before-publication-birth", func(h *coldQuotaReturnHistory) {
			h.reservedKV.ModRevision, h.workKV.ModRevision = 19, 19
		}},
		{"wrong-work-marker", func(h *coldQuotaReturnHistory) {
			h.workKV.Value = []byte(strings.Replace(string(h.workKV.Value), `"phase":"settled"`, `"phase":"pending"`, 1))
		}},
		{"noncanonical-work-marker", func(h *coldQuotaReturnHistory) {
			h.workKV.Value = append([]byte(" "), h.workKV.Value...)
		}},
		{"leased-work-marker", func(h *coldQuotaReturnHistory) { h.workKV.Lease = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, h, _, _ := coldSettlementDecodeFixture(t)
			tc.change(h)
			got, err := b.decodeColdSettlement(h.reservation, h.reservedKV, h.workKV, h.publication, nil)
			require.ErrorIs(t, err, ErrCorruptRecord)
			require.Zero(t, got)

			// An invalid locator would fail key derivation if that step moved
			// ahead of source validation; the source error must still win.
			h.reservation.Settlement.RequestID = "invalid/request"
			b.client = model.client
			server.mu.Lock()
			before := len(server.requests)
			server.mu.Unlock()
			got, err = b.verifyColdSettlement(context.Background(), h.reservation, h.reservedKV, h.workKV, h.publication)
			require.ErrorIs(t, err, ErrCorruptRecord)
			require.Zero(t, got)
			server.mu.Lock()
			after := len(server.requests)
			server.mu.Unlock()
			require.Equal(t, before, after, "invalid sources cannot send a receipt RPC")
		})
	}
}

// Pure decoding cannot borrow point-reader attribution. The key, original
// birth, immutable envelope and canonical committed bytes each bind the proof.
func TestColdSettlementDecodeRejectsUnboundReceipt(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *mvccpb.KeyValue, StageReference) *mvccpb.KeyValue
	}{
		{"missing", func(_ *testing.T, _ *mvccpb.KeyValue, _ StageReference) *mvccpb.KeyValue { return nil }},
		{"wrong-key", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) *mvccpb.KeyValue {
			kv.Key = []byte("other/receipt")
			return kv
		}},
		{"different-birth", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) *mvccpb.KeyValue {
			kv.CreateRevision, kv.ModRevision = 31, 31
			return kv
		}},
		{"mutable-version", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) *mvccpb.KeyValue {
			kv.Version++
			return kv
		}},
		{"mutable-revision", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) *mvccpb.KeyValue {
			kv.ModRevision++
			return kv
		}},
		{"leased", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) *mvccpb.KeyValue {
			kv.Lease = 1
			return kv
		}},
		{"malformed", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) *mvccpb.KeyValue {
			kv.Value = []byte("{")
			return kv
		}},
		{"noncanonical", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) *mvccpb.KeyValue {
			kv.Value = append([]byte(" "), kv.Value...)
			return kv
		}},
		{"aborted", func(t *testing.T, kv *mvccpb.KeyValue, ref StageReference) *mvccpb.KeyValue {
			wire, err := encodeReceipt(ref, OutcomeAborted)
			require.NoError(t, err)
			kv.Value = []byte(wire)
			return kv
		}},
		{"foreign-attempt", func(t *testing.T, kv *mvccpb.KeyValue, ref StageReference) *mvccpb.KeyValue {
			ref.AttemptID = "00000000-0000-0000-0000-000000000001"
			wire, err := encodeReceipt(ref, OutcomeCommitted)
			require.NoError(t, err)
			kv.Value = []byte(wire)
			return kv
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, h, receipt, ref := coldSettlementDecodeFixture(t)
			receipt = tc.change(t, receipt, ref)
			got, err := b.decodeColdSettlement(h.reservation, h.reservedKV, h.workKV, h.publication, receipt)
			require.ErrorIs(t, err, ErrCorruptReceipt)
			require.Zero(t, got)
		})
	}
}
