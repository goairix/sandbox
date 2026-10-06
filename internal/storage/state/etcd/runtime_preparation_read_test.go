package etcd

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"testing"
)

func preparationMetadataFixture(t *testing.T) (*Backend, *clientv3.Client, WorkspaceIdentity, RuntimeBindingRecord, []string) {
	t.Helper()
	b, raw := integrationBackend(t)
	w, d, input, dk, ik, drk := dispatchTestRecords(t, b)
	receipt := stageReceipt{Version: 1, StageReference: d.Attempt.reference(domainHash), Outcome: OutcomeCommitted}
	_, err := raw.Txn(context.Background()).Then(clientv3.OpPut(dk, dispatchTestWire(t, d)), clientv3.OpPut(ik, dispatchTestWire(t, input)), clientv3.OpPut(drk, dispatchTestWire(t, receipt))).Commit()
	require.NoError(t, err)
	payload := json.RawMessage(`{"opaque":"metadata"}`)
	digest, err := snapshotDigest(payload)
	require.NoError(t, err)
	attempt := d.Attempt
	attempt.StageID = "runtime_bind"
	r := RuntimeBindingRecord{Version: 1, IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, RestoreEpoch: d.RestoreEpoch, Generation: d.Generation, Snapshot: d.Snapshot, ExpiresAt: d.ExpiresAt, OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, CertificateDigest: digest, WorkspaceMode: "fuse", Runtime: RuntimeReference{ID: "id", UID: "uid", BootID: "boot"}, Claim: d.Claim, Attempt: attempt}
	cert := RuntimeBindingCertificateRecord{Version: 1, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, Generation: r.Generation, CertificateDigest: digest, Payload: payload}
	index := RuntimeIndexRecord{Version: 1, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, Generation: r.Generation, Runtime: r.Runtime}
	bk, ck, err := b.namespace.runtimeBindingKeys(w.Partition(), r.IntentID)
	require.NoError(t, err)
	xk, err := b.namespace.runtimeIndexKey(r.Runtime.UID)
	require.NoError(t, err)
	rk, err := b.runtimeDispatchReceiptKey(attempt)
	require.NoError(t, err)
	receipt.StageReference = attempt.reference(domainHash)
	_, err = raw.Txn(context.Background()).Then(clientv3.OpPut(bk, dispatchTestWire(t, r)), clientv3.OpPut(ck, dispatchTestWire(t, cert)), clientv3.OpPut(xk, dispatchTestWire(t, index)), clientv3.OpPut(rk, dispatchTestWire(t, receipt))).Commit()
	require.NoError(t, err)
	return b, raw, w, r, []string{bk, ck, xk, rk}
}
func TestRuntimePreparationMetadataLoader(t *testing.T) {
	b, _, w, r, _ := preparationMetadataFixture(t)
	entry, err := b.LoadRuntimeBinding(context.Background(), w, r.IntentID)
	require.NoError(t, err)
	require.NotNil(t, entry, "durable binding must be recoverable without authority configuration")
	require.Equal(t, r, entry.Record)
	entry.Certificate[0] = '!'
	again, err := b.LoadRuntimeBinding(context.Background(), w, r.IntentID)
	require.NoError(t, err)
	require.Equal(t, `{"opaque":"metadata"}`, string(again.Certificate))
}
func TestRuntimePreparationMetadataCorruption(t *testing.T) {
	for _, fault := range []string{"missing index", "changed index", "missing receipt", "wrong receipt", "duplicate", "null nested", "missing field"} {
		t.Run(fault, func(t *testing.T) {
			b, raw, w, r, keys := preparationMetadataFixture(t)
			ctx := context.Background()
			var err error
			switch fault {
			case "missing index":
				_, err = raw.Delete(ctx, keys[2])
			case "changed index":
				got, e := raw.Get(ctx, keys[2])
				require.NoError(t, e)
				_, err = raw.Put(ctx, keys[2], string(got.Kvs[0].Value))
			case "missing receipt":
				_, err = raw.Delete(ctx, keys[3])
			case "wrong receipt":
				receipt := stageReceipt{Version: 1, StageReference: r.Attempt.reference(domainHash), Outcome: OutcomeAborted}
				_, err = raw.Put(ctx, keys[3], dispatchTestWire(t, receipt))
			default:
				got, e := raw.Get(ctx, keys[0])
				require.NoError(t, e)
				wire := string(got.Kvs[0].Value)
				if fault == "duplicate" {
					wire = strings.Replace(wire, `"version":1`, `"version":1,"version":1`, 1)
				}
				if fault == "null nested" {
					wire = strings.Replace(wire, `"runtime":{"id":"id","uid":"uid","boot_id":"boot"}`, `"runtime":null`, 1)
				}
				if fault == "missing field" {
					wire = strings.Replace(wire, `"workspace_mode":"fuse",`, "", 1)
				}
				// Delete/recreate the complete quartet to preserve immutability while corrupting schema.
				gets, e := b.readDomain(ctx, keys...)
				require.NoError(t, e)
				var deletes, puts []clientv3.Op
				for i, k := range keys {
					deletes = append(deletes, clientv3.OpDelete(k))
					v := string(gets[i].Value)
					if i == 0 {
						v = wire
					}
					puts = append(puts, clientv3.OpPut(k, v))
				}
				_, e = raw.Txn(ctx).Then(deletes...).Commit()
				require.NoError(t, e)
				_, err = raw.Txn(ctx).Then(puts...).Commit()
			}
			require.NoError(t, err)
			entry, err := b.LoadRuntimeBinding(ctx, w, r.IntentID)
			require.Error(t, err)
			require.Nil(t, entry)
		})
	}
}

func TestRuntimePreparationMountMetadata(t *testing.T) {
	b, raw, w, r, _ := preparationMetadataFixture(t)
	ctx := context.Background()
	a := r.Attempt
	a.StageID = "runtime_mount"
	m := RuntimeMountIntentRecord{Version: 1, IntentID: r.IntentID, SandboxID: r.SandboxID, WorkspaceHash: r.WorkspaceHash, RestoreEpoch: r.RestoreEpoch, Generation: r.Generation, Runtime: r.Runtime, CertificateDigest: r.CertificateDigest, DispatchOperationID: r.OperationID, OperationID: "11234567-89ab-4cde-8012-3456789abcde", WorkspaceMode: "fuse", MountAttempt: 1, Claim: r.Claim, Attempt: a}
	mk, err := b.namespace.runtimeMountIntentKey(w.Partition(), r.IntentID)
	require.NoError(t, err)
	rk, err := b.runtimeDispatchReceiptKey(a)
	require.NoError(t, err)
	receipt := stageReceipt{Version: 1, StageReference: a.reference(domainHash), Outcome: OutcomeCommitted}
	_, err = raw.Txn(ctx).Then(clientv3.OpPut(mk, dispatchTestWire(t, m)), clientv3.OpPut(rk, dispatchTestWire(t, receipt))).Commit()
	require.NoError(t, err)
	entry, err := b.LoadRuntimeMountIntent(ctx, w, r.IntentID)
	require.NoError(t, err)
	require.NotNil(t, entry)
	require.Equal(t, m, entry.Record)
	// A permanent no-mount decision must not be inferred by changing the mode.
	m.WorkspaceMode = "plain"
	m.MountAttempt = 0
	_, err = raw.Delete(ctx, mk)
	require.NoError(t, err)
	_, err = raw.Delete(ctx, rk)
	require.NoError(t, err)
	_, err = raw.Txn(ctx).Then(clientv3.OpPut(mk, dispatchTestWire(t, m)), clientv3.OpPut(rk, dispatchTestWire(t, receipt))).Commit()
	require.NoError(t, err)
	_, err = b.LoadRuntimeMountIntent(ctx, w, r.IntentID)
	require.ErrorIs(t, err, ErrCorruptRecord)
}

func TestRuntimePreparationRejectsDuplicateReceipt(t *testing.T) {
	b, raw, w, r, keys := preparationMetadataFixture(t)
	ctx := context.Background()
	values, err := b.readDomain(ctx, keys...)
	require.NoError(t, err)
	var deletes, puts []clientv3.Op
	for i, key := range keys {
		deletes = append(deletes, clientv3.OpDelete(key))
		wire := string(values[i].Value)
		if i == 3 {
			wire = strings.Replace(wire, `"version":1`, `"version":1,"version":1`, 1)
		}
		puts = append(puts, clientv3.OpPut(key, wire))
	}
	_, err = raw.Txn(ctx).Then(deletes...).Commit()
	require.NoError(t, err)
	_, err = raw.Txn(ctx).Then(puts...).Commit()
	require.NoError(t, err)
	entry, err := b.LoadRuntimeBinding(ctx, w, r.IntentID)
	require.ErrorIs(t, err, ErrCorruptReceipt)
	require.Nil(t, entry)
}

func TestRuntimePreparationRejectsZeroOperationUUID(t *testing.T) {
	_, _, _, r, _ := preparationMetadataFixture(t)
	r.OperationID = "00000000-0000-0000-0000-000000000000"
	_, err := encodeDomainRecord(r)
	require.ErrorIs(t, err, ErrInvalidRecord)
}
