package etcd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// SandboxTTLCurrent is copied committed TTL and transport certification history.
// Activation retains root certification; neither this projection nor its bytes
// grants an operation, authorizes a Stage, or changes historical effect issuers.
// A nil current projection means the original creation credentials still apply.
type SandboxTTLCurrent struct {
	Baseline, ExpiresAt time.Time
	Timeout             int
	PreviousDigest      string
	Activation          p.TargetActivationEvidence
}

// SandboxTTLPending describes one durable, fixed renewal candidate. Its Stage
// is an arbitration identity, never a reconstructed mutation capability. A
// caller must query this candidate instead of signing a replacement on retry.
type SandboxTTLPending struct {
	Baseline, ExpiresAt time.Time
	Timeout             int
	PreviousDigest      string
	Activation          p.TargetActivationEvidence
	Operation           OperationReference
	Stage               StageReference
}

// SandboxTTLResult diagnoses the original final metadata attempt. Only a
// committed outcome reports a completed TTL change; runtime installation alone
// does not establish that control, credentials and due were committed together.
type SandboxTTLResult struct {
	Outcome             Outcome
	Stage               StageReference
	Baseline, ExpiresAt time.Time
	Timeout             int
	GuardCleanupError   error
}

// These records use two fixed business points, not a replacement operation or
// Stage protocol. Existing OperationRecord/Stage v1 bytes remain unchanged.
// PendingRevision is zero in ttl-pending and its exact immutable native birth in
// ttl-current. Persisting a locator, rather than its mutation digest, avoids a
// self-referential hash; both original Stage digests are rebuilt deterministically.
type sandboxTTLRecord struct {
	Version         uint32               `json:"version"`
	Operation       OperationRecord      `json:"operation"`
	Control         SandboxControlRecord `json:"control"`
	SourceMods      [5]int64             `json:"source_mods"`
	GuardRevision   int64                `json:"guard_revision"`
	AdmitRevision   int64                `json:"admit_revision"`
	CurrentRevision int64                `json:"current_revision"`
	DueRevision     int64                `json:"due_revision"`
	PendingRevision int64                `json:"pending_revision"`
	Baseline        time.Time            `json:"baseline"`
	Timeout         int                  `json:"timeout"`
	ExpiresAt       time.Time            `json:"expires_at"`
	PreviousDigest  string               `json:"previous_digest"`
	Activation      json.RawMessage      `json:"activation"`
	RuntimeCert     json.RawMessage      `json:"runtime_certificate"`
	IssuerCert      json.RawMessage      `json:"issuer_certificate"`
	Publish         StageAttemptLocator  `json:"publish"`
	Final           StageAttemptLocator  `json:"final"`
}

func (r sandboxTTLRecord) Validate() error {
	if r.Version != 1 || r.Operation.Validate() != nil || r.Operation.Reference.Kind != OperationMutation || r.Control.Validate() != nil || r.Control.Phase != PhaseActive || r.Control.Runtime == nil || r.Timeout < 1 || r.Timeout > 365*24*60*60 || !validDomainExpiry(r.Baseline) || r.Baseline.Location() != time.UTC || !validDomainExpiry(r.ExpiresAt) || r.ExpiresAt.Location() != time.UTC || !r.ExpiresAt.Equal(r.Baseline.Add(time.Duration(r.Timeout)*time.Second)) || !validHexDigest(r.PreviousDigest) {
		return ErrInvalidRecord
	}
	o, c := r.Operation, r.Control
	if o.Reference.SandboxID != c.SandboxID || o.WorkspaceHash != c.WorkspaceHash || o.IntentID != c.IntentID || o.Reference.RestoreEpoch != c.RestoreEpoch || o.Generation != c.Generation || o.DataGateEpoch != c.DataGateEpoch || o.Runtime != *c.Runtime || o.Snapshot != c.Snapshot || !o.ExpiresAt.Equal(c.ExpiresAt) || r.SourceMods[1] != o.ControlRevision {
		return ErrInvalidRecord
	}
	for _, mod := range r.SourceMods {
		if mod <= 0 {
			return ErrInvalidRecord
		}
	}
	if r.GuardRevision <= 0 || r.AdmitRevision <= r.GuardRevision || r.CurrentRevision < 0 || r.DueRevision <= 0 || r.PendingRevision < 0 || r.Publish.AttemptID == r.Final.AttemptID {
		return ErrInvalidRecord
	}
	for i, locator := range []StageAttemptLocator{r.Publish, r.Final} {
		stage := "ttl_prepare"
		if i == 1 {
			stage = "ttl_commit"
		}
		if locator.Validate() != nil || locator.Namespace != o.Reference.Namespace || locator.RestoreEpoch != o.Reference.RestoreEpoch || locator.Partition != o.Reference.Partition || locator.RequestID != o.Reference.RequestID || locator.StageID != stage {
			return ErrInvalidRecord
		}
	}
	// These are the existing activation/delegate wire limits. The complete
	// canonical record must additionally fit the original 64 KiB business cap.
	if len(r.Activation) == 0 || len(r.Activation) > 16384 || len(r.RuntimeCert) == 0 || len(r.RuntimeCert) > 8192 || len(r.IssuerCert) == 0 || len(r.IssuerCert) > 8192 || !json.Valid(r.Activation) || !json.Valid(r.RuntimeCert) || !json.Valid(r.IssuerCert) {
		return ErrInvalidRecord
	}
	return nil
}

func encodeSandboxTTL(r sandboxTTLRecord) (string, error) {
	if err := r.Validate(); err != nil {
		return "", err
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(r); err != nil {
		return "", ErrInvalidRecord
	}
	wire := bytes.TrimSuffix(out.Bytes(), []byte("\n"))
	if len(wire) > maxRecordBytes {
		return "", ErrInvalidRecord
	}
	return string(wire), nil
}

func decodeSandboxTTL(kv *mvccpb.KeyValue, pending bool) (sandboxTTLRecord, error) {
	var r sandboxTTLRecord
	if !operationPermanentKV(kv) || kv.Version <= 0 || len(kv.Value) > maxRecordBytes || pending && (kv.CreateRevision != kv.ModRevision || kv.Version != 1) || json.Unmarshal(kv.Value, &r) != nil || r.Validate() != nil || pending != (r.PendingRevision == 0) {
		return r, ErrCorruptRecord
	}
	// Exact canonical bytes reject unknown/missing/duplicate/null fields and
	// case aliases, including nested Control's legacy omitempty runtime tag.
	// Opaque certification objects retain their bytes for the root verifier.
	wire, err := encodeSandboxTTL(r)
	if err != nil || wire != string(kv.Value) {
		return sandboxTTLRecord{}, ErrCorruptRecord
	}
	return r, nil
}

func (b *Backend) sandboxTTLKeys(partition uint8, sandboxID string) (pending, current string, err error) {
	partitionName := fmt.Sprintf("%02x", partition)
	pending, err = b.namespace.Key("p", partitionName, "sandboxes", sandboxID, "ttl-pending")
	if err == nil {
		current, err = b.namespace.Key("p", partitionName, "sandboxes", sandboxID, "ttl-current")
	}
	return
}

func (b *Backend) sandboxTTLAuthorityKeys(r sandboxTTLRecord) ([]string, error) {
	ref, c := r.Operation.Reference, r.Control
	placement, err := b.namespace.placementKey(ref.SandboxID)
	if err != nil {
		return nil, err
	}
	control, _, err := b.namespace.sandboxKeys(ref.Partition, ref.SandboxID, "read")
	if err != nil {
		return nil, err
	}
	owner, err := b.namespace.Key("p", fmt.Sprintf("%02x", ref.Partition), "workspaces", c.WorkspaceHash, "owner")
	if err != nil {
		return nil, err
	}
	fence, err := b.namespace.Key("p", fmt.Sprintf("%02x", ref.Partition), "workspaces", c.WorkspaceHash, "fence")
	if err != nil {
		return nil, err
	}
	index, err := b.namespace.runtimeIndexKey(c.Runtime.UID)
	return []string{placement, control, owner, fence, index}, err
}

// Positive native Mod pins preserve the complete authenticated incarnation:
// changing value, attaching a lease, or deleting/recreating changes Mod. The
// original operation's four leased envelopes retain all four explicit clauses.
func (b *Backend) sandboxTTLComparisons(r sandboxTTLRecord) ([]clientv3.Cmp, error) {
	keys, err := b.sandboxTTLAuthorityKeys(r)
	if err != nil {
		return nil, err
	}
	var compares []clientv3.Cmp
	for i, key := range keys {
		compares = append(compares, clientv3.Compare(clientv3.ModRevision(key), "=", r.SourceMods[i]), clientv3.Compare(clientv3.LeaseValue(key), "=", 0))
	}
	token, guard, receipt, mutation, err := b.namespace.operationKeys(r.Operation.Reference)
	if err != nil {
		return nil, err
	}
	value, err := encodeOperationRecord(r.Operation)
	if err != nil {
		return nil, err
	}
	receiptValue, err := encodeOperationReceipt(operationReceipt{Version: 1, Reference: r.Operation.Reference, Outcome: OperationCommitted})
	if err != nil {
		return nil, err
	}
	for _, point := range []struct {
		key, value string
		revision   int64
	}{{guard, value, r.GuardRevision}, {token, value, r.AdmitRevision}, {receipt, receiptValue, r.AdmitRevision}, {mutation, value, r.AdmitRevision}} {
		compares = append(compares, operationEnvelopeComparisons(point.key, point.value, r.Operation.Reference.LeaseID, point.revision)...)
	}
	return compares, nil
}

// The same builder is used for preflight, submission and cold receipt identity.
// No guard or capability is reconstructed by a read-only use of this function.
func (b *Backend) sandboxTTLMutation(r sandboxTTLRecord, pendingRevision int64) (Mutation, error) {
	if err := r.Validate(); err != nil {
		return Mutation{}, err
	}
	r.PendingRevision = pendingRevision
	compares, err := b.sandboxTTLComparisons(r)
	if err != nil {
		return Mutation{}, err
	}
	pending, current, err := b.sandboxTTLKeys(r.Operation.Reference.Partition, r.Control.SandboxID)
	if err != nil {
		return Mutation{}, err
	}
	currentPin := clientv3.CreateRevision(current)
	if r.CurrentRevision != 0 {
		currentPin = clientv3.ModRevision(current)
	}
	compares = append(compares, clientv3.Compare(currentPin, "=", r.CurrentRevision))
	oldDue := dueCandidateForControl(r.Operation.Reference.Partition, r.Control)
	oldDueKey, err := b.namespace.dueCandidateKey(oldDue)
	if err != nil {
		return Mutation{}, err
	}
	updated := r.Control
	updated.ExpiresAt = r.ExpiresAt
	newDue := dueCandidateForControl(r.Operation.Reference.Partition, updated)
	newDueKey, err := b.namespace.dueCandidateKey(newDue)
	if err != nil {
		return Mutation{}, err
	}
	compares = append(compares, clientv3.Compare(clientv3.ModRevision(oldDueKey), "=", r.DueRevision))
	if oldDueKey != newDueKey {
		compares = append(compares, clientv3.Compare(clientv3.CreateRevision(newDueKey), "=", 0))
	}
	wire, err := encodeSandboxTTL(r)
	if err != nil {
		return Mutation{}, err
	}
	if pendingRevision == 0 {
		compares = append(compares, clientv3.Compare(clientv3.CreateRevision(pending), "=", 0))
		return Mutation{Comparisons: compares, Writes: []Write{{Key: pending, Value: []byte(wire)}}}, nil
	}
	compares = append(compares, clientv3.Compare(clientv3.ModRevision(pending), "=", pendingRevision))
	controlValue, err := encodeDomainRecord(updated)
	if err != nil {
		return Mutation{}, err
	}
	keys, err := b.sandboxTTLAuthorityKeys(r)
	if err != nil {
		return Mutation{}, err
	}
	writes := []Write{{Key: keys[1], Value: []byte(controlValue)}, {Key: current, Value: []byte(wire)}}
	// etcd forbids touching the same key twice in one transaction. An unchanged
	// expiry therefore preserves its exact due point instead of delete+put.
	if oldDueKey != newDueKey {
		dueValue, err := encodeDueCandidate(newDue)
		if err != nil {
			return Mutation{}, err
		}
		writes = append(writes, Write{Key: oldDueKey, Delete: true}, Write{Key: newDueKey, Value: []byte(dueValue)})
	}
	writes = append(writes, Write{Key: pending, Delete: true})
	return Mutation{Comparisons: compares, Writes: writes}, nil
}

// Preflight accounts both actual native branches before any Stage Grant. The
// original generic protocol reserves fourteen nodes; its actual Commit uses
// thirteen. Lease/revision placeholders maximize varints, not authority.
func (b *Backend) sandboxTTLStage(r sandboxTTLRecord, pendingRevision int64) (Mutation, StageReference, error) {
	mutation, err := b.sandboxTTLMutation(r, pendingRevision)
	if err != nil {
		return Mutation{}, StageReference{}, err
	}
	locator := r.Publish
	if pendingRevision != 0 {
		locator = r.Final
	}
	return b.preflightSandboxTTLStage(locator, mutation)
}

// A complete, authenticated no-effect rejection can retire only this pending
// point under the same operation/source fences. It uses the original signed
// candidate ID with a distinct Stage name; it never installs a replacement.
func (b *Backend) sandboxTTLRejectStage(r sandboxTTLRecord, pendingRevision int64) (Mutation, StageReference, error) {
	mutation, err := b.sandboxTTLMutation(r, pendingRevision)
	if err != nil || pendingRevision <= 0 {
		return Mutation{}, StageReference{}, ErrInvalidRecord
	}
	pending, _, err := b.sandboxTTLKeys(r.Operation.Reference.Partition, r.Control.SandboxID)
	if err != nil {
		return Mutation{}, StageReference{}, err
	}
	mutation.Writes = []Write{{Key: pending, Delete: true}}
	locator := r.Final
	locator.StageID = "ttl_reject"
	return b.preflightSandboxTTLStage(locator, mutation)
}

func (b *Backend) preflightSandboxTTLStage(locator StageAttemptLocator, mutation Mutation) (Mutation, StageReference, error) {
	owned, digest, err := prepareMutation(b.namespace, mutation)
	if err != nil {
		return Mutation{}, StageReference{}, err
	}
	ref := locator.reference(digest)
	guard, receipt, err := b.stageKeys(ref)
	if err != nil {
		return Mutation{}, StageReference{}, err
	}
	guardWire, err := json.Marshal(struct {
		StageReference
		LeaseID int64 `json:"lease_id"`
	}{ref, math.MaxInt64})
	if err != nil {
		return Mutation{}, StageReference{}, err
	}
	receiptWire, err := encodeReceipt(ref, OutcomeCommitted)
	if err != nil {
		return Mutation{}, StageReference{}, err
	}
	compares := append(b.baseComparisons(), operationEnvelopeComparisons(guard, string(guardWire), math.MaxInt64, math.MaxInt64)[:3]...)
	compares = append(compares, clientv3.Compare(clientv3.CreateRevision(receipt), "=", 0))
	compares = append(compares, owned.Comparisons...)
	writes := append(append([]Write(nil), owned.Writes...), Write{Key: receipt, Value: []byte(receiptWire)})
	points := []string{b.identityKey, b.restoreKey, receipt, guard}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(compares), Success: taskProtoWrites(writes), Failure: taskProtoPoints(points)}
	// Else may observe a foreign/corrupt record rather than our short value.
	// Retain the original whole-record cap for both native protocol points.
	reply := taskProtoResponse(points, []string{b.identityValue, b.restoreEpoch, string(bytes.Repeat([]byte{'x'}, maxRecordBytes)), string(bytes.Repeat([]byte{'x'}, maxRecordBytes))})
	if len(request.Compare)+len(request.Success)+len(request.Failure) > 64 || request.Size() > 128<<10 || reply.Size() > 256<<10 {
		return Mutation{}, StageReference{}, ErrInvalidMutation
	}
	return owned, ref, nil
}
