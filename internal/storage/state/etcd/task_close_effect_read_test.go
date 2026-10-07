package etcd

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
	"testing"
)

func TestTaskCloseNativeRead(t *testing.T) {
	for _, fault := range []string{"absent", "missing-receipt", "aborted", "receipt-digest", "receipt-mod", "receipt-null", "discovery-replaced", "restore", "after-claim-release", "new-claim"} {
		t.Run(fault, func(t *testing.T) {
			f := newTaskCloseFixture(t, false)
			if fault == "absent" {
				entry, e := f.b.LoadTaskCloseData(f.ctx, f.c.reference.Task)
				require.NoError(t, e)
				require.Nil(t, entry)
				return
			}
			r, e := f.b.PrepareTaskCloseData(f.ctx, f.c)
			require.NoError(t, e)
			key, e := f.b.namespace.taskCloseDataKey(r.Reference.Task)
			require.NoError(t, e)
			_, receipt, e := f.b.stageKeys(r.Reference.Stage)
			require.NoError(t, e)
			original := f.b.client.KV
			defer func() { f.b.client.KV = original }()
			replaceReceipt := func(value string) {
				_, err := f.raw.Txn(f.ctx).Then(clientv3.OpDelete(key), clientv3.OpDelete(receipt)).Commit()
				require.NoError(t, err)
				_, err = f.raw.Txn(f.ctx).Then(clientv3.OpPut(key, f.c.closeDraft.recordValue), clientv3.OpPut(receipt, value)).Commit()
				require.NoError(t, err)
			}
			switch fault {
			case "missing-receipt":
				_, e = f.raw.Delete(f.ctx, receipt)
			case "aborted":
				v, err := encodeReceipt(r.Reference.Stage, OutcomeAborted)
				require.NoError(t, err)
				replaceReceipt(v)
			case "receipt-digest":
				ref := r.Reference.Stage
				ref.Digest = strings.Repeat("A", 64)
				v, err := encodeReceipt(ref, OutcomeCommitted)
				require.NoError(t, err)
				replaceReceipt(v)
			case "receipt-mod":
				g, err := f.raw.Get(f.ctx, receipt)
				require.NoError(t, err)
				_, e = f.raw.Put(f.ctx, receipt, string(g.Kvs[0].Value))
			case "receipt-null":
				replaceReceipt("null")
			case "discovery-replaced":
				once := false
				f.b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
					return !once && len(ops) == 3 && ops[2].IsGet() && string(ops[2].KeyBytes()) == key
				}, after: func(resp *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
					require.NoError(t, err)
					once = true
					v := f.c.closeDraft.recordValue
					rv, err := encodeReceipt(r.Reference.Stage, OutcomeCommitted)
					require.NoError(t, err)
					_, err = f.raw.Txn(f.ctx).Then(clientv3.OpDelete(key), clientv3.OpDelete(receipt)).Commit()
					require.NoError(t, err)
					_, err = f.raw.Txn(f.ctx).Then(clientv3.OpPut(key, v), clientv3.OpPut(receipt, rv)).Commit()
					require.NoError(t, err)
					return resp, nil
				}}
			case "restore":
				_, e = f.raw.Put(f.ctx, f.b.restoreKey, "other")
				defer func() {
					_, err := f.raw.Put(context.Background(), f.b.restoreKey, f.b.restoreEpoch)
					require.NoError(t, err)
				}()
			case "after-claim-release", "new-claim":
				require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.c))
				f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) { panic("history must not observe clock") })
				if fault == "new-claim" {
					next, err := f.b.ClaimTask(f.ctx, r.Reference.Task, "new-worker", 30e9)
					require.NoError(t, err)
					defer taskReleaseClaim(t, f.b, next)
					result, err := f.b.PrepareTaskCloseData(f.ctx, next)
					require.ErrorIs(t, err, ErrConflict)
					require.Nil(t, result.Prepared)
					require.Equal(t, 1, f.p.signCalls)
				}
			}
			require.NoError(t, e)
			entry, e := f.b.LoadTaskCloseData(f.ctx, r.Reference.Task)
			if fault == "after-claim-release" || fault == "new-claim" {
				require.NoError(t, e)
				require.Equal(t, r.Reference, entry.Reference)
				b, err := json.Marshal(entry.Record)
				require.NoError(t, err)
				require.NotEmpty(t, b)
			} else {
				require.Error(t, e)
				require.Nil(t, entry)
			}
		})
	}
}
