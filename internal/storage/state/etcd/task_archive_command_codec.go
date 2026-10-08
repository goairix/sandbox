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

type taskArchiveCommandRecord struct {
	Version           uint32                             `json:"version"`
	FinalSync         taskFinalSyncRecordWire            `json:"final_sync"`
	FinalSyncRevision int64                              `json:"final_sync_revision"`
	Claims            p.TaskWorkspaceArchiveTicketClaims `json:"claims"`
	IssuerRevision    int64                              `json:"issuer_revision"`
	Certificate       json.RawMessage                    `json:"certificate"`
	Ticket            json.RawMessage                    `json:"ticket"`
	QuiescenceReceipt json.RawMessage                    `json:"quiescence_receipt"`
}

type taskArchiveTicketWire struct {
	Claims    p.TaskWorkspaceArchiveTicketClaims `json:"claims"`
	Signature []byte                             `json:"signature"`
}
type taskArchiveQuiescenceReceiptWire struct {
	Claims    p.TaskUserQuiescenceReceiptClaims `json:"claims"`
	Signature []byte                            `json:"signature"`
}
type taskArchiveCertificateWire struct {
	Claims    p.CommandIssuerCertificateClaims `json:"claims"`
	Signature []byte                           `json:"signature"`
}

func taskArchiveStrict(w []byte, limit int, typ reflect.Type) bool {
	return len(w) > 0 && len(w) <= limit && taskCloseJSON(w) && strictPreparationMetadata(w, typ) == nil
}
func (r taskArchiveCommandRecord) validate() error {
	f, a := r.FinalSync.record(), r.Claims
	c := a.Context.Current
	if r.Version != 1 || f.Validate() != nil || a.Validate() != nil || r.IssuerRevision <= 0 || r.FinalSyncRevision <= f.Claim.CreateRevision || r.FinalSyncRevision <= f.QuiescenceRevision || a.Context.FinalSyncRevision != r.FinalSyncRevision || a.Context.QuiescenceIntentRevision != f.QuiescenceRevision || a.Context.PlanDigest != f.Plan.Digest || a.Context.QuiescenceReceiptDigest != f.ReceiptDigest {
		return ErrInvalidRecord
	}
	task, err := encodeTaskRecord(f.Task)
	if err != nil {
		return ErrInvalidRecord
	}
	digest := digestTaskQuiescenceProof([]byte(task))
	expected := p.TaskCloseDataContext{Namespace: f.Task.Reference.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: f.Task.Reference.RestoreEpoch, IssuerCertificateID: c.IssuerCertificateID, IssuerCertificateDigest: c.IssuerCertificateDigest, CommandID: f.AttemptID, TaskID: f.Task.Reference.TaskID, TaskDigest: digest, ClaimID: f.Claim.ClaimID, WorkerID: f.Claim.WorkerID, SandboxID: f.Task.Reference.SandboxID, WorkspaceHash: f.Task.WorkspaceHash, Generation: f.Task.Generation, DataGateEpoch: f.Task.DataGateEpoch, ControlRevision: f.DestroyingRevision, ClaimCreateRevision: f.Claim.CreateRevision, LeaseID: f.Claim.LeaseID, Runtime: p.RuntimeReference(f.Task.Runtime)}
	if c != expected || a.Context.QuiescenceContext.Current.CommandID != f.Quiescence.CommandID || !taskArchiveStrict(r.Ticket, 8192, reflect.TypeOf(taskArchiveTicketWire{})) || !taskArchiveStrict(r.Certificate, 4096, reflect.TypeOf(taskArchiveCertificateWire{})) || !taskArchiveStrict(r.QuiescenceReceipt, 12288, reflect.TypeOf(taskArchiveQuiescenceReceiptWire{})) {
		return ErrInvalidRecord
	}
	var ticket taskArchiveTicketWire
	var receipt taskArchiveQuiescenceReceiptWire
	var cert taskArchiveCertificateWire
	if json.Unmarshal(r.Ticket, &ticket) != nil || json.Unmarshal(r.QuiescenceReceipt, &receipt) != nil || json.Unmarshal(r.Certificate, &cert) != nil {
		return ErrInvalidRecord
	}
	q, ic := receipt.Claims, cert.Claims
	if !validDomainExpiry(ic.NotBefore) || !validDomainExpiry(ic.NotAfter) || ic.NotBefore.Location() != time.UTC || ic.NotAfter.Location() != time.UTC || !ic.NotBefore.Before(ic.NotAfter) {
		return ErrInvalidRecord
	}
	if ticket.Claims != a || len(ticket.Signature) != ed25519.SignatureSize || q.Version != 1 || q.State != "users_quiesced" || !quiescenceWindow(q.NotBefore, q.NotAfter) || !validHexDigest(q.ExecutionSetDigest) || q.RegisteredCount > 64 || uint64(q.RegisteredCount) != uint64(q.NeverSpawnedCount)+uint64(q.LocalTerminalCount) || q.Context != a.Context.QuiescenceContext || q.TicketDigest != a.Context.QuiescenceTicketDigest || len(receipt.Signature) != ed25519.SignatureSize || digestTaskQuiescenceProof(r.QuiescenceReceipt) != f.ReceiptDigest || ic.Version != 1 || ic.Role != "command_issuer" || ic.CertificateID != c.IssuerCertificateID || digestTaskQuiescenceProof(r.Certificate) != c.IssuerCertificateDigest || ic.Namespace != c.Namespace || ic.AuthorityID != c.AuthorityID || ic.Target != c.Target || ic.RestoreEpoch != c.RestoreEpoch || len(ic.PublicKey) != ed25519.PublicKeySize || len(cert.Signature) != ed25519.SignatureSize || ic.NotBefore.After(a.NotBefore) || ic.NotAfter.Before(a.SessionNotAfter) {
		return ErrInvalidRecord
	}
	for _, pair := range []struct {
		wire  []byte
		typed any
	}{{r.Ticket, ticket}, {r.Certificate, cert}, {r.QuiescenceReceipt, receipt}} {
		canonical, err := json.Marshal(pair.typed)
		if err != nil || !bytes.Equal(pair.wire, canonical) {
			return ErrInvalidRecord
		}
	}
	return nil
}
func encodeTaskArchiveCommand(r taskArchiveCommandRecord) (string, error) {
	if r.validate() != nil {
		return "", ErrInvalidRecord
	}
	var out bytes.Buffer
	e := json.NewEncoder(&out)
	e.SetEscapeHTML(false)
	if e.Encode(r) != nil {
		return "", ErrInvalidRecord
	}
	w := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	if len(w) > maxRecordBytes || !taskCloseJSON(w) {
		return "", ErrInvalidRecord
	}
	return string(w), nil
}
func decodeTaskArchiveCommand(kv *mvccpb.KeyValue, dst *taskArchiveCommandRecord) error {
	if dst == nil {
		return ErrCorruptRecord
	}
	*dst = taskArchiveCommandRecord{}
	if !immutableDispatchKV(kv) || kv.Version != 1 || !taskArchiveStrict(kv.Value, maxRecordBytes, reflect.TypeOf(taskArchiveCommandRecord{})) {
		return ErrCorruptRecord
	}
	var r taskArchiveCommandRecord
	if json.Unmarshal(kv.Value, &r) != nil {
		return ErrCorruptRecord
	}
	w, err := encodeTaskArchiveCommand(r)
	f := r.FinalSync.record()
	if err != nil || !bytes.Equal(kv.Value, []byte(w)) || kv.CreateRevision <= r.FinalSyncRevision || kv.CreateRevision <= r.IssuerRevision || kv.CreateRevision <= f.Claim.CreateRevision || kv.CreateRevision <= f.QuiescenceRevision || string(kv.Key) != fmt.Sprintf("%sp/%02x/tasks/%s/workspace-archive-command", f.Task.Reference.Namespace, f.Task.Reference.Partition, f.Task.Reference.TaskID) {
		return ErrCorruptRecord
	}
	*dst = r
	return nil
}
func (n Namespace) taskWorkspaceArchiveKey(r TaskReference) (string, error) {
	parts, err := n.taskKeyParts(r)
	if err != nil {
		return "", err
	}
	return n.Key(append(parts, "workspace-archive-command")...)
}
