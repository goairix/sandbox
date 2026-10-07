package etcd

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"strings"
	"testing"
)

func quiesceRaw(t *testing.T, claims any, domain string) []byte {
	t.Helper()
	c, err := json.Marshal(claims)
	require.NoError(t, err)
	key := ed25519.NewKeyFromSeed(make([]byte, 32))
	w, err := json.Marshal(struct {
		Claims    any    `json:"claims"`
		Signature []byte `json:"signature"`
	}{claims, ed25519.Sign(key, append([]byte(domain), c...))})
	require.NoError(t, err)
	return w
}
func quiesceRecordFixture(t *testing.T) TaskQuiescenceRecord {
	old := taskCloseRecordFixture(t)
	var ticket taskCloseTicketWire
	require.NoError(t, json.Unmarshal(old.Ticket, &ticket))
	receipt := quiesceRaw(t, p.TaskDataClosedReceiptClaims{Version: 1, State: "data_closed", Context: old.Context, TicketDigest: old.TicketDigest, NotBefore: ticket.Claims.NotBefore, NotAfter: ticket.Claims.NotAfter}, "sandbox-task-data-closed-receipt:v1\x00")
	rd, err := snapshotDigest(receipt)
	require.NoError(t, err)
	current := old.Context
	current.CommandID = uuid.NewString()
	current.ClaimCreateRevision++
	current.ClaimID = uuid.NewString()
	current.WorkerID = "new-worker"
	current.LeaseID++
	ctx := p.TaskUserQuiescenceContext{Current: current, CloseDataContext: old.Context, CloseDataTicketDigest: old.TicketDigest, CloseDataReceiptDigest: rd, CloseDataIntentRevision: 20}
	tw := quiesceRaw(t, p.TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: ctx, NotBefore: ticket.Claims.NotBefore, NotAfter: ticket.Claims.NotAfter}, "sandbox-task-quiesce-users-ticket:v1\x00")
	td, err := snapshotDigest(tw)
	require.NoError(t, err)
	claim := old.Claim
	claim.ClaimID = current.ClaimID
	claim.CreateRevision = current.ClaimCreateRevision
	claim.WorkerID = current.WorkerID
	claim.LeaseID = current.LeaseID
	attempt := old.Attempt
	attempt.StageID = "task_quiesce_users"
	attempt.RequestID = claim.ClaimID
	attempt.AttemptID = uuid.NewString()
	return TaskQuiescenceRecord{Version: 1, Task: old.Task, Claim: claim, Context: ctx, TicketDigest: td, IssuerCertificateID: old.IssuerCertificateID, IssuerCertificateDigest: old.IssuerCertificateDigest, IssuerRevision: old.IssuerRevision, Ticket: tw, CloseDataReceipt: receipt, Attempt: attempt}
}
func TestTaskQuiesceMetadataRecords(t *testing.T) {
	r := quiesceRecordFixture(t)
	w, err := encodeTaskQuiescenceRecord(r)
	require.NoError(t, err)
	ns := namespaceFromTaskCloseTest(t, r.Task.Reference)
	key, err := ns.taskQuiescenceKey(r.Task.Reference)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(w), CreateRevision: 50, ModRevision: 50}
	var got TaskQuiescenceRecord
	require.NoError(t, decodeTaskQuiescenceRecord(kv, &got))
	require.Equal(t, r, got)
	clear(kv.Value)
	require.Equal(t, r.Ticket, got.Ticket)
	require.Equal(t, r.CloseDataReceipt, got.CloseDataReceipt)
	for _, kind := range []string{"task", "claim", "old-birth", "receipt-digest", "receipt-context", "purpose", "record-birth", "lease", "mod", "key", "unknown", "over-limit", "exact-limit"} {
		t.Run(kind, func(t *testing.T) {
			x := r
			v := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(w), CreateRevision: 50, ModRevision: 50}
			switch kind {
			case "task":
				x.Context.CloseDataContext.TaskID = uuid.NewString()
			case "claim":
				x.Claim.ClaimID = uuid.NewString()
			case "old-birth":
				x.Context.CloseDataIntentRevision = x.Context.CloseDataContext.ClaimCreateRevision
			case "receipt-digest":
				x.Context.CloseDataReceiptDigest = strings.Repeat("f", 64)
			case "receipt-context":
				x.CloseDataReceipt = bytes.Replace(x.CloseDataReceipt, []byte(`"data_closed"`), []byte(`"pending"`), 1)
				x.Context.CloseDataReceiptDigest, _ = snapshotDigest(x.CloseDataReceipt)
			case "purpose":
				x.Ticket = bytes.Replace(x.Ticket, []byte("task_quiesce_users"), []byte("task_close_data"), 1)
				x.TicketDigest, _ = snapshotDigest(x.Ticket)
			case "record-birth":
				v.CreateRevision = r.Context.CloseDataIntentRevision
				v.ModRevision = v.CreateRevision
			case "lease":
				v.Lease = 1
			case "mod":
				v.ModRevision++
			case "key":
				v.Key = append(v.Key, 'x')
			case "unknown":
				v.Value = append([]byte(`{"unknown":0,`), v.Value[1:]...)
			case "over-limit":
				v.Value = bytes.Repeat([]byte(" "), 32769)
			case "exact-limit":
				v.Value = append(v.Value, bytes.Repeat([]byte(" "), 32768-len(v.Value))...)
				var dst TaskQuiescenceRecord
				require.NoError(t, decodeTaskQuiescenceRecord(v, &dst))
				return
			}
			if kind == "task" || kind == "claim" || kind == "old-birth" || kind == "receipt-digest" || kind == "receipt-context" || kind == "purpose" {
				_, err := encodeTaskQuiescenceRecord(x)
				require.Error(t, err)
				v.Value, err = json.Marshal(taskQuiescenceWire(x))
				require.NoError(t, err)
			}
			var dst TaskQuiescenceRecord
			require.Error(t, decodeTaskQuiescenceRecord(v, &dst))
			require.Equal(t, TaskQuiescenceRecord{}, dst)
		})
	}
}

func TestTaskQuiesceMetadataSchema(t *testing.T) {
	r := quiesceRecordFixture(t)
	wire, err := encodeTaskQuiescenceRecord(r)
	require.NoError(t, err)
	ns := namespaceFromTaskCloseTest(t, r.Task.Reference)
	key, err := ns.taskQuiescenceKey(r.Task.Reference)
	require.NoError(t, err)
	for _, kind := range []string{"claim-case", "claim-null", "claim-duplicate", "missing-receipt", "receipt-null", "receipt-extra", "context-null", "unicode", "trailing"} {
		t.Run(kind, func(t *testing.T) {
			w := []byte(wire)
			switch kind {
			case "claim-case":
				w = bytes.Replace(w, []byte(`"claim_id"`), []byte(`"ClaimID"`), 1)
			case "claim-null":
				w = bytes.Replace(w, []byte(`"claim":{`), []byte(`"claim":null,"unused":{`), 1)
			case "claim-duplicate":
				w = bytes.Replace(w, []byte(`"claim":{`), []byte(`"claim":{"lease_id":12,`), 1)
			case "missing-receipt":
				var x map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(w, &x))
				delete(x, "close_data_receipt")
				w, err = json.Marshal(x)
				require.NoError(t, err)
			case "receipt-null":
				var x map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(w, &x))
				x["close_data_receipt"] = json.RawMessage("null")
				w, err = json.Marshal(x)
				require.NoError(t, err)
			case "receipt-extra":
				w = bytes.Replace(w, []byte(`"close_data_receipt":{"claims":{`), []byte(`"close_data_receipt":{"claims":{"remote_settled":true,`), 1)
			case "context-null":
				w = bytes.Replace(w, []byte(`"close_data_context":{`), []byte(`"close_data_context":null,"unused":{`), 1)
			case "unicode":
				w = bytes.Replace(w, []byte(`"new-worker"`), []byte(`"\ud800"`), 1)
			case "trailing":
				w = append(w, []byte("{}")...)
			}
			require.NotEqual(t, wire, string(w))
			var dst TaskQuiescenceRecord
			require.Error(t, decodeTaskQuiescenceRecord(&mvccpb.KeyValue{Key: []byte(key), Value: w, CreateRevision: 50, ModRevision: 50}, &dst))
			require.Equal(t, TaskQuiescenceRecord{}, dst)
		})
	}
}
