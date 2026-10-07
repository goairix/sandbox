package etcd

import (
	"encoding/json"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"strings"
	"testing"
)

func finalSyncRecordFixture(t *testing.T) TaskFinalSyncAttemptRecord {
	q := quiesceRecordFixture(t)
	return TaskFinalSyncAttemptRecord{Version: 1, Task: q.Task, Claim: q.Claim, DestroyingRevision: q.Context.Current.ControlRevision, Quiescence: TaskQuiescenceReference{Task: q.Task.Reference, CommandID: q.Context.Current.CommandID, Stage: q.Attempt.reference(strings.Repeat("a", 64))}, QuiescenceRevision: 50, ReceiptDigest: strings.Repeat("b", 64), Plan: TaskFinalSyncPlan{Mode: "sync", Digest: strings.Repeat("c", 64)}, AttemptID: uuid.NewString()}
}

func TestTaskFinalSyncRecords(t *testing.T) {
	r := finalSyncRecordFixture(t)
	w, err := encodeTaskFinalSyncAttempt(r)
	require.NoError(t, err)
	ns := namespaceFromTaskCloseTest(t, r.Task.Reference)
	key, err := ns.taskFinalSyncAttemptKey(r.Task.Reference)
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(key, "/tasks/"+r.Task.Reference.TaskID+"/final-sync-attempt"))
	kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(w), CreateRevision: 51, ModRevision: 51, Version: 1}
	var got TaskFinalSyncAttemptRecord
	require.NoError(t, decodeTaskFinalSyncAttempt(kv, &got))
	require.Equal(t, r, got)
	for _, fault := range []string{"mode", "digest", "attempt", "claim", "destroy", "quiescence-task", "quiescence-birth", "quiescence-command", "receipt-digest", "key", "native-birth", "lease", "mod", "version", "unknown", "duplicate", "missing", "null", "whitespace", "reordered", "unicode", "over-limit", "trailing"} {
		t.Run(fault, func(t *testing.T) {
			x := r
			v := *kv
			v.Value = []byte(w)
			switch fault {
			case "mode":
				x.Plan.Mode = "Sync"
			case "digest":
				x.Plan.Digest = strings.Repeat("A", 64)
			case "attempt":
				x.AttemptID = "bad"
			case "claim":
				x.Claim.Task.TaskID = uuid.NewString()
			case "destroy":
				x.DestroyingRevision = x.Task.ControlRevision
			case "quiescence-task":
				x.Quiescence.Task.TaskID = uuid.NewString()
			case "quiescence-birth":
				x.QuiescenceRevision = x.DestroyingRevision
			case "quiescence-command":
				x.Quiescence.CommandID = "bad"
			case "receipt-digest":
				x.ReceiptDigest = "bad"
			case "key":
				v.Key = append([]byte(key), 'x')
			case "native-birth":
				v.CreateRevision = 50
				v.ModRevision = 50
			case "lease":
				v.Lease = 1
			case "mod":
				v.ModRevision++
			case "version":
				v.Version = 2
			case "unknown":
				v.Value = append([]byte(`{"remote_settled":true,`), v.Value[1:]...)
			case "duplicate":
				v.Value = append([]byte(`{"version":1,`), v.Value[1:]...)
			case "missing":
				var m map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(v.Value, &m))
				delete(m, "plan")
				v.Value, err = json.Marshal(m)
				require.NoError(t, err)
			case "null":
				v.Value = []byte(strings.Replace(w, `"plan":{`, `"plan":null,"unused":{`, 1))
			case "whitespace":
				v.Value = append(v.Value, ' ')
			case "reordered":
				var m map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(v.Value, &m))
				v.Value, err = json.Marshal(m)
				require.NoError(t, err)
			case "unicode":
				v.Value = []byte(strings.Replace(w, r.Claim.WorkerID, `\ud800`, 1))
			case "over-limit":
				v.Value = []byte(strings.Repeat(" ", 8193))
			case "trailing":
				v.Value = append(v.Value, []byte("{}")...)
			}
			if x != r {
				_, err = encodeTaskFinalSyncAttempt(x)
				require.Error(t, err)
				v.Value, err = json.Marshal(taskFinalSyncWire(x))
				require.NoError(t, err)
			}
			got = r
			require.Error(t, decodeTaskFinalSyncAttempt(&v, &got))
			require.Equal(t, TaskFinalSyncAttemptRecord{}, got)
		})
	}
	for _, mode := range []string{"sync", "fuse", "none"} {
		x := r
		x.Plan.Mode = mode
		_, err := encodeTaskFinalSyncAttempt(x)
		require.NoError(t, err)
	}
	require.Error(t, decodeTaskFinalSyncAttempt(kv, nil))
}
