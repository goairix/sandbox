package etcd

import (
	"bytes"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"testing"
)

func TestTaskQuiesceMetadataAttemptRecord(t *testing.T) {
	base := quiesceRecordFixture(t)
	r := TaskQuiescenceAttemptRecord{Version: 1, Task: base.Task, Claim: base.Claim, DestroyingRevision: base.Context.Current.ControlRevision, CommandID: base.Context.Current.CommandID, Attempt: base.Attempt}
	wire, e := encodeTaskQuiescenceAttempt(r)
	require.NoError(t, e)
	n := namespaceFromTaskCloseTest(t, r.Task.Reference)
	key, e := n.taskQuiescenceAttemptKey(r.Task.Reference)
	require.NoError(t, e)
	for _, fault := range []string{"valid", "unknown", "claim-case", "claim-null", "claim-duplicate", "missing", "zero-command", "locator", "task", "birth", "lease", "rewrite", "key", "exact-limit", "over-limit"} {
		t.Run(fault, func(t *testing.T) {
			kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), CreateRevision: 40, ModRevision: 40}
			switch fault {
			case "unknown":
				kv.Value = append([]byte(`{"extra":0,`), kv.Value[1:]...)
			case "claim-case":
				kv.Value = bytes.Replace(kv.Value, []byte(`"claim_id"`), []byte(`"ClaimID"`), 1)
			case "claim-null":
				kv.Value = bytes.Replace(kv.Value, []byte(`"claim_id":"`+r.Claim.ClaimID+`"`), []byte(`"claim_id":null`), 1)
			case "claim-duplicate":
				kv.Value = bytes.Replace(kv.Value, []byte(`"claim_id":`), []byte(`"claim_id":"`+r.Claim.ClaimID+`","claim_id":`), 1)
			case "missing":
				kv.Value = bytes.Replace(kv.Value, []byte(`"version":1,`), nil, 1)
			case "zero-command":
				kv.Value = bytes.Replace(kv.Value, []byte(r.CommandID), []byte("00000000-0000-0000-0000-000000000000"), 1)
			case "locator":
				kv.Value = bytes.Replace(kv.Value, []byte(`"task_quiesce_users"`), []byte(`"task_close_data"`), 1)
			case "task":
				kv.Value = bytes.Replace(kv.Value, []byte(r.Task.Reference.TaskID), []byte("00000000-0000-4000-8000-000000000001"), 1)
			case "birth":
				kv.CreateRevision = r.Claim.CreateRevision
				kv.ModRevision = kv.CreateRevision
			case "lease":
				kv.Lease = 1
			case "rewrite":
				kv.ModRevision++
			case "key":
				kv.Key = append(kv.Key, 'x')
			case "exact-limit":
				kv.Value = append(kv.Value, bytes.Repeat([]byte(" "), 8192-len(kv.Value))...)
			case "over-limit":
				kv.Value = bytes.Repeat([]byte(" "), 8193)
			}
			got := r // An error must erase pre-existing diagnostic evidence.
			e := decodeTaskQuiescenceAttempt(kv, &got)
			if fault == "valid" || fault == "exact-limit" {
				require.NoError(t, e)
				require.Equal(t, r, got)
				clear(kv.Value)
				require.Equal(t, r, got)
			} else {
				require.Error(t, e)
				require.Equal(t, TaskQuiescenceAttemptRecord{}, got)
			}
		})
	}
}
