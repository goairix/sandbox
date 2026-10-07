package etcd

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"reflect"
	"strings"
	"testing"
	"time"
)

func taskCloseRecordFixture(t *testing.T) TaskCloseDataRecord {
	t.Helper()
	task, _, _, _ := taskTestRecords()
	now := time.Now().UTC()
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	key := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789012"))
	cid := uuid.NewString()
	binding := controlprotocol.TrustBinding{Namespace: task.Reference.Namespace, AuthorityID: "runtime", Target: "kubernetes", RestoreEpoch: task.Reference.RestoreEpoch}
	v, e := controlprotocol.NewManagementVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, e)
	cert, e := controlprotocol.SignCommandIssuerCertificate(root, controlprotocol.CommandIssuerCertificateClaims{Version: 1, CertificateID: cid, Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: key.Public().(ed25519.PublicKey), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, e)
	issuer, e := v.VerifyCommandIssuerCertificate(cert, now)
	require.NoError(t, e)
	claim := TaskClaimReference{Task: task.Reference, ClaimID: uuid.NewString(), WorkerID: "worker", CreateRevision: task.ControlRevision + 2, LeaseID: 12}
	wire, e := encodeTaskRecord(task)
	require.NoError(t, e)
	digest, e := snapshotDigest([]byte(wire))
	require.NoError(t, e)
	c := controlprotocol.TaskCloseDataContext{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IssuerCertificateID: cid, IssuerCertificateDigest: issuer.Digest(), CommandID: uuid.NewString(), TaskID: task.Reference.TaskID, TaskDigest: digest, ClaimID: claim.ClaimID, WorkerID: claim.WorkerID, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, Generation: task.Generation, DataGateEpoch: task.DataGateEpoch, ControlRevision: task.ControlRevision + 1, ClaimCreateRevision: claim.CreateRevision, LeaseID: claim.LeaseID, Runtime: controlprotocol.RuntimeReference(task.Runtime)}
	ticket, e := controlprotocol.SignTaskCloseDataTicket(key, issuer, controlprotocol.TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: c, NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(20 * time.Second)})
	require.NoError(t, e)
	td, e := snapshotDigest(ticket)
	require.NoError(t, e)
	return TaskCloseDataRecord{Version: 1, Task: task, Claim: claim, Context: c, TicketDigest: td, IssuerCertificateID: cid, IssuerCertificateDigest: issuer.Digest(), IssuerRevision: 2, Ticket: ticket, Attempt: StageAttemptLocator{Namespace: binding.Namespace, RestoreEpoch: binding.RestoreEpoch, Partition: task.Reference.Partition, RequestID: claim.ClaimID, StageID: "task_close_data", AttemptID: uuid.NewString()}}
}
func TestTaskCloseRecords(t *testing.T) {
	r := taskCloseRecordFixture(t)
	wire, e := encodeTaskCloseDataRecord(r)
	require.NoError(t, e)
	ns := namespaceFromTaskCloseTest(t, r.Task.Reference)
	k, e := ns.taskCloseDataKey(r.Task.Reference)
	require.NoError(t, e)
	kv := &mvccpb.KeyValue{Key: []byte(k), Value: []byte(wire), CreateRevision: 50, ModRevision: 50}
	var got TaskCloseDataRecord
	require.NoError(t, decodeTaskCloseDataRecord(kv, &got))
	require.Equal(t, r, got)
	clear(kv.Value)
	require.Equal(t, r.Ticket, got.Ticket)
	for _, fault := range []string{"version", "claim", "birth", "digest", "issuer", "attempt", "ticket", "unknown", "lease", "mod", "key", "size"} {
		t.Run(fault, func(t *testing.T) {
			x := r
			x.Ticket = bytes.Clone(r.Ticket)
			v := &mvccpb.KeyValue{Key: []byte(k), Value: []byte(wire), CreateRevision: 50, ModRevision: 50}
			switch fault {
			case "version":
				x.Version = 2
			case "claim":
				x.Claim.ClaimID = uuid.NewString()
			case "birth":
				x.Context.ControlRevision = x.Claim.CreateRevision
			case "digest":
				x.Context.TaskDigest = strings.Repeat("f", 64)
			case "issuer":
				x.IssuerCertificateDigest = strings.Repeat("f", 64)
			case "attempt":
				x.Attempt.RequestID = uuid.NewString()
			case "ticket":
				x.Ticket = bytes.Replace(x.Ticket, []byte("task_close_data"), []byte("task_close_fake"), 1)
				x.TicketDigest, _ = snapshotDigest(x.Ticket)
			case "unknown":
				v.Value = append([]byte(`{"extra":0,`), v.Value[1:]...)
			case "lease":
				v.Lease = 1
			case "mod":
				v.ModRevision++
			case "key":
				v.Key = []byte(k + "x")
			case "size":
				v.Value = bytes.Repeat([]byte(" "), 16385)
			}
			if !reflect.DeepEqual(x, r) {
				v.Value, _ = json.Marshal(taskCloseWire(x))
				_, e := encodeTaskCloseDataRecord(x)
				require.Error(t, e)
			}
			var dst TaskCloseDataRecord
			require.Error(t, decodeTaskCloseDataRecord(v, &dst))
			require.Equal(t, TaskCloseDataRecord{}, dst)
		})
	}
}
func namespaceFromTaskCloseTest(t *testing.T, r TaskReference) Namespace {
	t.Helper()
	p := strings.Split(strings.TrimSuffix(r.Namespace, "/"), "/")
	n, e := NewNamespace(strings.Join(p[:len(p)-2], "/"), p[len(p)-2], p[len(p)-1])
	require.NoError(t, e)
	return n
}

func TestTaskCloseSchema(t *testing.T) {
	r := taskCloseRecordFixture(t)
	wire, e := encodeTaskCloseDataRecord(r)
	require.NoError(t, e)
	n := namespaceFromTaskCloseTest(t, r.Task.Reference)
	key, e := n.taskCloseDataKey(r.Task.Reference)
	require.NoError(t, e)
	reject := func(t *testing.T, w []byte) {
		t.Helper()
		var dst TaskCloseDataRecord
		require.Error(t, decodeTaskCloseDataRecord(&mvccpb.KeyValue{Key: []byte(key), Value: w, CreateRevision: 50, ModRevision: 50}, &dst))
		require.Equal(t, TaskCloseDataRecord{}, dst)
	}
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(wire), &object))
	var claim map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(object["claim"], &claim))
	require.Len(t, claim, 5)
	for _, f := range []string{"task", "claim_id", "worker_id", "create_revision", "lease_id"} {
		require.Contains(t, claim, f)
	}
	for _, fault := range []string{"CamelCase", "duplicate", "missing", "null", "unknown", "nested-task-null", "trailing", "unicode", "exact-limit", "over-limit"} {
		t.Run(fault, func(t *testing.T) {
			w := []byte(wire)
			switch fault {
			case "CamelCase":
				w = bytes.Replace(w, []byte(`"claim_id"`), []byte(`"ClaimID"`), 1)
			case "duplicate":
				w = bytes.Replace(w, []byte(`"claim":{`), []byte(`"claim":{"worker_id":"worker",`), 1)
			case "missing":
				w = bytes.Replace(w, []byte(`"worker_id":"worker",`), nil, 1)
			case "null":
				w = bytes.Replace(w, []byte(`"worker_id":"worker"`), []byte(`"worker_id":null`), 1)
			case "unknown":
				w = bytes.Replace(w, []byte(`"claim":{`), []byte(`"claim":{"extra":0,`), 1)
			case "nested-task-null":
				copy := taskCloseWire(r)
				b, e := json.Marshal(copy)
				require.NoError(t, e)
				var m map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(b, &m))
				var c map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(m["claim"], &c))
				c["task"] = json.RawMessage("null")
				m["claim"], _ = json.Marshal(c)
				w, _ = json.Marshal(m)
			case "trailing":
				w = append(w, []byte("{}")...)
			case "unicode":
				w = bytes.Replace(w, []byte(`"worker_id":"worker"`), []byte(`"worker_id":"\ud800"`), 1)
			case "exact-limit":
				w = append(w, bytes.Repeat([]byte(" "), 16384-len(w))...)
				var dst TaskCloseDataRecord
				require.NoError(t, decodeTaskCloseDataRecord(&mvccpb.KeyValue{Key: []byte(key), Value: w, CreateRevision: 50, ModRevision: 50}, &dst))
				return
			case "over-limit":
				w = append(w, bytes.Repeat([]byte(" "), 16385-len(w))...)
			}
			reject(t, w)
		})
	}
	// Public diagnostic struct JSON deliberately does not define persisted Claim.
	public, e := json.Marshal(r)
	require.NoError(t, e)
	reject(t, public)
}

func TestTaskCloseRecordBirth(t *testing.T) {
	r := taskCloseRecordFixture(t)
	n := namespaceFromTaskCloseTest(t, r.Task.Reference)
	key, e := n.taskCloseDataKey(r.Task.Reference)
	require.NoError(t, e)
	for _, fault := range []string{"claim-at-intent", "issuer-at-intent", "issuer-after-intent"} {
		t.Run(fault, func(t *testing.T) {
			x := r
			revision := r.Claim.CreateRevision
			if fault != "claim-at-intent" {
				revision = 50
				x.IssuerRevision = revision
				if fault == "issuer-after-intent" {
					x.IssuerRevision++
				}
			}
			wire, e := encodeTaskCloseDataRecord(x)
			require.NoError(t, e)
			var dst TaskCloseDataRecord
			require.Error(t, decodeTaskCloseDataRecord(&mvccpb.KeyValue{Key: []byte(key), Value: []byte(wire), CreateRevision: revision, ModRevision: revision}, &dst))
			require.Equal(t, TaskCloseDataRecord{}, dst)
		})
	}
}
