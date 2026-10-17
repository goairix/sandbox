package etcd

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// This codec-only history reuses the existing valid return record. Source KVs
// supply stamps, not runtime authority, and no client or worker is constructed.
func coldQuotaProofFixture(t *testing.T) (*Backend, *coldQuotaReturnHistory, *mvccpb.KeyValue, *mvccpb.KeyValue, coldRuntimeQuotaReturnRecord, StageReference) {
	t.Helper()
	r := quotaReturnRecordFixture(t)
	parts := strings.Split(strings.TrimSuffix(r.Namespace, "/"), "/")
	n, err := NewNamespace(strings.Join(parts[:len(parts)-2], "/"), parts[len(parts)-2], parts[len(parts)-1])
	require.NoError(t, err)
	b := &Backend{namespace: n, restoreEpoch: r.RestoreEpoch, preparationDigest: r.ProfileDigest}
	point := func(value string, create, mod int64) *mvccpb.KeyValue {
		return &mvccpb.KeyValue{Value: []byte(value), Version: 1, CreateRevision: create, ModRevision: mod}
	}
	h := &coldQuotaReturnHistory{
		reservation: ColdPreparationRecord{Owner: r.Owner, Quota: r.Quota, Preparation: r.Preparation},
		publication: &coldPublicationHistory{
			entry: &RuntimeEnrolledPublicationEntry{Record: RuntimeEnrolledPublicationRecord{
				RuntimePublicationRecord: RuntimePublicationRecord{Runtime: r.Runtime, DataGateEpoch: r.DataGateEpoch},
				NodeUID:                  r.NodeUID, NodeBootID: r.NodeBootID,
			}, Reference: r.Publication},
			journal: point("publication", 20, 20),
		},
		release:    &localOwnerReleaseProof{record: r.Release, kv: point("release", 104, 104)},
		settlement: r.PreparationSettlement,
		reservedKV: point("reservation", 10, 30),
		workKV:     point("work", 10, 30),
	}
	r = b.coldQuotaReturnRecord(h, r.Attempt)
	wire, err := encodeColdRuntimeQuotaReturn(r)
	require.NoError(t, err)
	returnKV := point(wire, 105, 105)
	ref := r.Attempt.reference(strings.Repeat("9", 64))
	receiptWire, err := encodeReceipt(ref, OutcomeCommitted)
	require.NoError(t, err)
	receiptKV := point(receiptWire, 105, 105)
	key, err := b.runtimeDispatchReceiptKey(r.Attempt)
	require.NoError(t, err)
	receiptKV.Key = []byte(key)
	return b, h, returnKV, receiptKV, r, ref
}

// Dropping the copied entry or postponing its digest until after the read would
// lose the original proof when the source buffers/history are released.
func TestColdQuotaReturnProofCopiesEntryBeforeReceipt(t *testing.T) {
	b, h, returnKV, receiptKV, r, wantRef := coldQuotaProofFixture(t)
	wantEntry := ColdRuntimeQuotaReturnEntry{
		Task: r.Release.Task, TaskBirth: r.Release.TaskBirth, Owner: r.Owner,
		Quota: r.Quota, Runtime: r.Runtime, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID,
		CreateRevision: 105, SHA256: fmt.Sprintf("%x", sha256.Sum256(returnKV.Value)),
	}
	stamp, err := b.decodeColdQuotaReturnProof(h, returnKV)
	require.NoError(t, err)
	require.Equal(t, r.Attempt, stamp.attempt)
	require.Equal(t, int64(105), stamp.birth)
	require.Equal(t, wantEntry, stamp.entry)

	clear(returnKV.Value)
	*returnKV = mvccpb.KeyValue{}
	*h = coldQuotaReturnHistory{}
	ref, entry, err := b.decodeColdQuotaReturnReceipt(stamp, receiptKV)
	require.NoError(t, err)
	require.Equal(t, wantRef, ref)
	require.Equal(t, wantEntry, *entry)

	entry.NodeUID = "caller-change"
	_, again, err := b.decodeColdQuotaReturnReceipt(stamp, receiptKV)
	require.NoError(t, err)
	require.Equal(t, wantEntry, *again, "each result owns its copied entry")
}

// Removing exact history equality would admit a canonical record bound to
// different inventory, profile or source evidence.
func TestColdQuotaReturnProofRejectsChangedHistoryBinding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*coldRuntimeQuotaReturnRecord)
	}{
		{"quota", func(r *coldRuntimeQuotaReturnRecord) { r.Quota.Epoch++ }},
		{"profile", func(r *coldRuntimeQuotaReturnRecord) { r.ProfileDigest = strings.Repeat("0", 64) }},
		{"publication", func(r *coldRuntimeQuotaReturnRecord) { r.Publication.Digest = strings.Repeat("0", 64) }},
		{"release-stamp", func(r *coldRuntimeQuotaReturnRecord) { r.ReleaseStamp.SHA256 = strings.Repeat("0", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, h, kv, _, r, _ := coldQuotaProofFixture(t)
			tc.change(&r)
			wire, err := encodeColdRuntimeQuotaReturn(r)
			require.NoError(t, err, "corruption must remain a valid standalone record")
			kv.Value = []byte(wire)
			_, err = b.decodeColdQuotaReturnProof(h, kv)
			require.ErrorIs(t, err, ErrCorruptRecord)
		})
	}
}

// Receipt attribution, immutability, original birth and canonical committed
// bytes are independent obligations; accepting any altered envelope is a bug.
func TestColdQuotaReturnReceiptRejectsUnboundEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *mvccpb.KeyValue, StageReference)
	}{
		{"wrong-key", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) { kv.Key = []byte("other/receipt") }},
		{"stale-birth", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) {
			kv.CreateRevision, kv.ModRevision = 106, 106
		}},
		{"mutable-version", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) { kv.Version++ }},
		{"mutable-revision", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) { kv.ModRevision++ }},
		{"leased", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) { kv.Lease = 1 }},
		{"malformed", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) { kv.Value = []byte("{") }},
		{"noncanonical", func(_ *testing.T, kv *mvccpb.KeyValue, _ StageReference) { kv.Value = append([]byte(" "), kv.Value...) }},
		{"aborted", func(t *testing.T, kv *mvccpb.KeyValue, ref StageReference) {
			wire, err := encodeReceipt(ref, OutcomeAborted)
			require.NoError(t, err)
			kv.Value = []byte(wire)
		}},
		{"foreign-attempt", func(t *testing.T, kv *mvccpb.KeyValue, ref StageReference) {
			ref.RequestID = "other-request"
			wire, err := encodeReceipt(ref, OutcomeCommitted)
			require.NoError(t, err)
			kv.Value = []byte(wire)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, h, returnKV, receiptKV, _, ref := coldQuotaProofFixture(t)
			stamp, err := b.decodeColdQuotaReturnProof(h, returnKV)
			require.NoError(t, err)
			tc.change(t, receiptKV, ref)
			gotRef, entry, err := b.decodeColdQuotaReturnReceipt(stamp, receiptKV)
			require.ErrorIs(t, err, ErrCorruptReceipt)
			require.Zero(t, gotRef)
			require.Nil(t, entry)
		})
	}
}
