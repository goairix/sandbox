package etcd

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"reflect"
	"strconv"
	"time"
	"unicode/utf8"
)

type taskCloseClaimWire struct {
	Task           TaskReference `json:"task"`
	ClaimID        string        `json:"claim_id"`
	WorkerID       string        `json:"worker_id"`
	CreateRevision int64         `json:"create_revision"`
	LeaseID        int64         `json:"lease_id"`
}
type taskCloseRecordWire struct {
	Version                 uint32                               `json:"version"`
	Task                    TaskRecord                           `json:"task"`
	Claim                   taskCloseClaimWire                   `json:"claim"`
	Context                 controlprotocol.TaskCloseDataContext `json:"context"`
	TicketDigest            string                               `json:"ticket_digest"`
	IssuerCertificateID     string                               `json:"issuer_certificate_id"`
	IssuerCertificateDigest string                               `json:"issuer_certificate_digest"`
	IssuerRevision          int64                                `json:"issuer_revision"`
	Ticket                  json.RawMessage                      `json:"ticket"`
	Attempt                 StageAttemptLocator                  `json:"attempt"`
}

func taskCloseWire(r TaskCloseDataRecord) taskCloseRecordWire {
	c := r.Claim
	return taskCloseRecordWire{r.Version, r.Task, taskCloseClaimWire{c.Task, c.ClaimID, c.WorkerID, c.CreateRevision, c.LeaseID}, r.Context, r.TicketDigest, r.IssuerCertificateID, r.IssuerCertificateDigest, r.IssuerRevision, bytes.Clone(r.Ticket), r.Attempt}
}
func (w taskCloseRecordWire) record() TaskCloseDataRecord {
	c := w.Claim
	return TaskCloseDataRecord{w.Version, w.Task, TaskClaimReference{Task: c.Task, ClaimID: c.ClaimID, WorkerID: c.WorkerID, CreateRevision: c.CreateRevision, LeaseID: c.LeaseID}, w.Context, w.TicketDigest, w.IssuerCertificateID, w.IssuerCertificateDigest, w.IssuerRevision, bytes.Clone(w.Ticket), w.Attempt}
}

type taskCloseTicketWire struct {
	Claims    controlprotocol.TaskCloseDataTicketClaims `json:"claims"`
	Signature []byte                                    `json:"signature"`
}

func (r TaskCloseDataRecord) Validate() error {
	c := r.Context
	ref := r.Task.Reference
	if r.Version != 1 || r.Task.Validate() != nil || r.Claim.Task != ref || !validPreparationUUID(r.Claim.ClaimID) || !validDomainSegment(r.Claim.WorkerID) || r.Claim.LeaseID <= 0 || r.Claim.CreateRevision <= c.ControlRevision || c.ControlRevision <= r.Task.ControlRevision || !validPreparationUUID(c.CommandID) || !validDomainSegment(c.AuthorityID) || !validDomainSegment(c.Target) || !validPreparationUUID(r.IssuerCertificateID) || !validHexDigest(r.IssuerCertificateDigest) || r.IssuerRevision <= 0 || !validTaskCloseAttempt(r.Attempt, ref, r.Claim.ClaimID) {
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
	expected := controlprotocol.TaskCloseDataContext{Namespace: ref.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: ref.RestoreEpoch, IssuerCertificateID: r.IssuerCertificateID, IssuerCertificateDigest: r.IssuerCertificateDigest, CommandID: c.CommandID, TaskID: ref.TaskID, TaskDigest: digest, ClaimID: r.Claim.ClaimID, WorkerID: r.Claim.WorkerID, SandboxID: ref.SandboxID, WorkspaceHash: r.Task.WorkspaceHash, Generation: r.Task.Generation, DataGateEpoch: r.Task.DataGateEpoch, ControlRevision: c.ControlRevision, ClaimCreateRevision: r.Claim.CreateRevision, LeaseID: r.Claim.LeaseID, Runtime: controlprotocol.RuntimeReference(r.Task.Runtime)}
	if c != expected || len(r.Ticket) == 0 || len(r.Ticket) > 4096 || !validHexDigest(r.TicketDigest) || !taskCloseJSON(r.Ticket) || strictPreparationMetadata(r.Ticket, reflect.TypeOf(taskCloseTicketWire{})) != nil {
		return ErrInvalidRecord
	}
	var ticket taskCloseTicketWire
	if json.Unmarshal(r.Ticket, &ticket) != nil {
		return ErrInvalidRecord
	}
	t := ticket.Claims
	if t.Version != 1 || t.Purpose != "task_close_data" || t.Context != c || len(ticket.Signature) != ed25519.SignatureSize || !validDomainExpiry(t.NotBefore) || !validDomainExpiry(t.NotAfter) || t.NotBefore.Location() != time.UTC || t.NotAfter.Location() != time.UTC || !t.NotBefore.Before(t.NotAfter) || t.NotAfter.Sub(t.NotBefore) > 30*time.Second {
		return ErrInvalidRecord
	}
	d, err := snapshotDigest(r.Ticket)
	if err != nil || d != r.TicketDigest {
		return ErrInvalidRecord
	}
	return nil
}
func encodeTaskCloseDataRecord(r TaskCloseDataRecord) (string, error) {
	if r.Validate() != nil {
		return "", ErrInvalidRecord
	}
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if e.Encode(taskCloseWire(r)) != nil {
		return "", ErrInvalidRecord
	}
	wire := bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	if len(wire) > 16384 || !taskCloseJSON(wire) {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}
func decodeTaskCloseDataRecord(kv *mvccpb.KeyValue, dst *TaskCloseDataRecord) error {
	if dst == nil || !immutableDispatchKV(kv) || len(kv.Value) > 16384 || !taskCloseJSON(kv.Value) || strictPreparationMetadata(kv.Value, reflect.TypeOf(taskCloseRecordWire{})) != nil {
		return ErrCorruptRecord
	}
	var w taskCloseRecordWire
	if json.Unmarshal(kv.Value, &w) != nil {
		return ErrCorruptRecord
	}
	r := w.record()
	if r.Validate() != nil {
		return ErrCorruptRecord
	}
	expected := fmt.Sprintf("%sp/%02x/tasks/%s/close-data", r.Task.Reference.Namespace, r.Task.Reference.Partition, r.Task.Reference.TaskID)
	if string(kv.Key) != expected {
		return ErrCorruptRecord
	}
	*dst = r
	return nil
}

// Reject UTF-16 surrogate replacement before encoding/json can normalize it.
// This local guard does not change the accepted metadata or protocol codecs.
func taskCloseJSON(w []byte) bool {
	if !utf8.Valid(w) || !json.Valid(w) {
		return false
	}
	for i := 0; i < len(w); i++ {
		if w[i] != '\\' {
			continue
		}
		i++
		if i >= len(w) {
			return false
		}
		if w[i] != 'u' {
			continue
		}
		if i+4 >= len(w) {
			return false
		}
		u, e := strconv.ParseUint(string(w[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if u >= 0xdc00 && u <= 0xdfff {
			return false
		}
		if u < 0xd800 || u > 0xdbff {
			continue
		}
		if i+6 >= len(w) || w[i+1] != '\\' || w[i+2] != 'u' {
			return false
		}
		v, e := strconv.ParseUint(string(w[i+3:i+7]), 16, 16)
		if e != nil || v < 0xdc00 || v > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
