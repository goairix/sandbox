package etcd

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"reflect"
	"time"
)

type taskQuiescenceRecordWire struct {
	Version                 uint32                      `json:"version"`
	Task                    TaskRecord                  `json:"task"`
	Claim                   taskCloseClaimWire          `json:"claim"`
	Context                 p.TaskUserQuiescenceContext `json:"context"`
	TicketDigest            string                      `json:"ticket_digest"`
	IssuerCertificateID     string                      `json:"issuer_certificate_id"`
	IssuerCertificateDigest string                      `json:"issuer_certificate_digest"`
	IssuerRevision          int64                       `json:"issuer_revision"`
	Ticket                  json.RawMessage             `json:"ticket"`
	CloseDataReceipt        json.RawMessage             `json:"close_data_receipt"`
	Attempt                 StageAttemptLocator         `json:"attempt"`
}

func taskQuiescenceWire(r TaskQuiescenceRecord) taskQuiescenceRecordWire {
	c := r.Claim
	return taskQuiescenceRecordWire{r.Version, r.Task, taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}, r.Context, r.TicketDigest, r.IssuerCertificateID, r.IssuerCertificateDigest, r.IssuerRevision, bytes.Clone(r.Ticket), bytes.Clone(r.CloseDataReceipt), r.Attempt}
}
func (w taskQuiescenceRecordWire) record() TaskQuiescenceRecord {
	c := w.Claim
	return TaskQuiescenceRecord{Version: w.Version, Task: w.Task, Claim: TaskClaimReference{Task: c.Task, ClaimID: c.ClaimID, WorkerID: c.WorkerID, CreateRevision: c.CreateRevision, LeaseID: c.LeaseID}, Context: w.Context, TicketDigest: w.TicketDigest, IssuerCertificateID: w.IssuerCertificateID, IssuerCertificateDigest: w.IssuerCertificateDigest, IssuerRevision: w.IssuerRevision, Ticket: bytes.Clone(w.Ticket), CloseDataReceipt: bytes.Clone(w.CloseDataReceipt), Attempt: w.Attempt}
}

type taskQuiescenceTicketWire struct {
	Claims    p.TaskUserQuiescenceTicketClaims `json:"claims"`
	Signature []byte                           `json:"signature"`
}
type taskQuiescenceCloseReceiptWire struct {
	Claims    p.TaskDataClosedReceiptClaims `json:"claims"`
	Signature []byte                        `json:"signature"`
}

func quiescenceWindow(a, b time.Time) bool {
	return validDomainExpiry(a) && validDomainExpiry(b) && a.Location() == time.UTC && b.Location() == time.UTC && a.Before(b) && b.Sub(a) <= 30*time.Second
}
func taskQuiescenceLink(c p.TaskUserQuiescenceContext) bool {
	a, b := c.Current, c.CloseDataContext
	return a.Namespace == b.Namespace && a.AuthorityID == b.AuthorityID && a.Target == b.Target && a.RestoreEpoch == b.RestoreEpoch && a.TaskID == b.TaskID && a.TaskDigest == b.TaskDigest && a.SandboxID == b.SandboxID && a.WorkspaceHash == b.WorkspaceHash && a.Generation == b.Generation && a.DataGateEpoch == b.DataGateEpoch && a.ControlRevision == b.ControlRevision && a.Runtime == b.Runtime && a.ClaimCreateRevision >= b.ClaimCreateRevision && (a.ClaimCreateRevision != b.ClaimCreateRevision || (a.ClaimID == b.ClaimID && a.WorkerID == b.WorkerID && a.LeaseID == b.LeaseID)) && validPreparationUUID(b.CommandID) && validPreparationUUID(b.IssuerCertificateID) && validHexDigest(b.IssuerCertificateDigest) && validPreparationUUID(b.ClaimID) && validDomainSegment(b.WorkerID) && b.LeaseID > 0 && b.ClaimCreateRevision > b.ControlRevision && validHexDigest(c.CloseDataTicketDigest) && validHexDigest(c.CloseDataReceiptDigest) && c.CloseDataIntentRevision > b.ClaimCreateRevision
}
func (r TaskQuiescenceRecord) Validate() error {
	c := r.Context.Current
	ref := r.Task.Reference
	if r.Version != 1 || r.Task.Validate() != nil || r.Claim.Task != ref || !validPreparationUUID(r.Claim.ClaimID) || !validDomainSegment(r.Claim.WorkerID) || r.Claim.LeaseID <= 0 || r.Claim.CreateRevision <= c.ControlRevision || c.ControlRevision <= r.Task.ControlRevision || !validPreparationUUID(c.CommandID) || !validDomainSegment(c.AuthorityID) || !validDomainSegment(c.Target) || !validPreparationUUID(r.IssuerCertificateID) || !validHexDigest(r.IssuerCertificateDigest) || r.IssuerRevision <= 0 || !validTaskQuiescenceAttempt(r.Attempt, ref, r.Claim.ClaimID) || !taskQuiescenceLink(r.Context) {
		return ErrInvalidRecord
	}
	task, err := encodeTaskRecord(r.Task)
	if err != nil {
		return ErrInvalidRecord
	}
	digest, err := snapshotDigest([]byte(task))
	if err != nil {
		return ErrInvalidRecord
	}
	expected := p.TaskCloseDataContext{Namespace: ref.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: ref.RestoreEpoch, IssuerCertificateID: r.IssuerCertificateID, IssuerCertificateDigest: r.IssuerCertificateDigest, CommandID: c.CommandID, TaskID: ref.TaskID, TaskDigest: digest, ClaimID: r.Claim.ClaimID, WorkerID: r.Claim.WorkerID, SandboxID: ref.SandboxID, WorkspaceHash: r.Task.WorkspaceHash, Generation: r.Task.Generation, DataGateEpoch: r.Task.DataGateEpoch, ControlRevision: c.ControlRevision, ClaimCreateRevision: r.Claim.CreateRevision, LeaseID: r.Claim.LeaseID, Runtime: p.RuntimeReference(r.Task.Runtime)}
	if c != expected || len(r.Ticket) == 0 || len(r.Ticket) > 8192 || !validHexDigest(r.TicketDigest) || !taskCloseJSON(r.Ticket) || strictPreparationMetadata(r.Ticket, reflect.TypeOf(taskQuiescenceTicketWire{})) != nil || len(r.CloseDataReceipt) == 0 || len(r.CloseDataReceipt) > 8192 || !taskCloseJSON(r.CloseDataReceipt) || strictPreparationMetadata(r.CloseDataReceipt, reflect.TypeOf(taskQuiescenceCloseReceiptWire{})) != nil {
		return ErrInvalidRecord
	}
	var ticket taskQuiescenceTicketWire
	var receipt taskQuiescenceCloseReceiptWire
	if json.Unmarshal(r.Ticket, &ticket) != nil || json.Unmarshal(r.CloseDataReceipt, &receipt) != nil {
		return ErrInvalidRecord
	}
	t, rc := ticket.Claims, receipt.Claims
	if t.Version != 1 || t.Purpose != "task_quiesce_users" || t.Context != r.Context || len(ticket.Signature) != ed25519.SignatureSize || !quiescenceWindow(t.NotBefore, t.NotAfter) || rc.Version != 1 || rc.State != "data_closed" || rc.Context != r.Context.CloseDataContext || rc.TicketDigest != r.Context.CloseDataTicketDigest || len(receipt.Signature) != ed25519.SignatureSize || !quiescenceWindow(rc.NotBefore, rc.NotAfter) {
		return ErrInvalidRecord
	}
	td, err := snapshotDigest(r.Ticket)
	if err != nil || td != r.TicketDigest {
		return ErrInvalidRecord
	}
	rd, err := snapshotDigest(r.CloseDataReceipt)
	if err != nil || rd != r.Context.CloseDataReceiptDigest {
		return ErrInvalidRecord
	}
	return nil
}
func encodeTaskQuiescenceRecord(r TaskQuiescenceRecord) (string, error) {
	if r.Validate() != nil {
		return "", ErrInvalidRecord
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if e.Encode(taskQuiescenceWire(r)) != nil {
		return "", ErrInvalidRecord
	}
	w := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	if len(w) > 32768 || !taskCloseJSON(w) {
		return "", ErrInvalidRecord
	}
	return string(w), nil
}
func decodeTaskQuiescenceRecord(kv *mvccpb.KeyValue, dst *TaskQuiescenceRecord) error {
	if dst == nil || !immutableDispatchKV(kv) || len(kv.Value) > 32768 || !taskCloseJSON(kv.Value) || strictPreparationMetadata(kv.Value, reflect.TypeOf(taskQuiescenceRecordWire{})) != nil {
		return ErrCorruptRecord
	}
	var w taskQuiescenceRecordWire
	if json.Unmarshal(kv.Value, &w) != nil {
		return ErrCorruptRecord
	}
	r := w.record()
	if r.Validate() != nil || kv.CreateRevision <= r.Claim.CreateRevision || kv.CreateRevision <= r.IssuerRevision || kv.CreateRevision <= r.Context.CloseDataIntentRevision {
		return ErrCorruptRecord
	}
	if string(kv.Key) != fmt.Sprintf("%sp/%02x/tasks/%s/quiesce-users", r.Task.Reference.Namespace, r.Task.Reference.Partition, r.Task.Reference.TaskID) {
		return ErrCorruptRecord
	}
	*dst = r
	return nil
}
