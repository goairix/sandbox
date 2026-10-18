package etcd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
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
// PendingRevision is zero in ttl-pending. A v1 current stores the original
// pending birth; a normalized v2 current stores its recovery reservation Mod.
// Version 2 keeps the original birth separately and never alters the v1 bytes.
// Persisting locators avoids self-referential Stage mutation digests.
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

	// The normalized v1 projection never serializes recovery coordinates.
	// Original publish/Final digests therefore remain byte-for-byte stable.
	recovery *sandboxTTLRecoveryRecord
}

// Version 2 wraps, rather than rewrites, the original v1 pending value. A
// pending row gets its reserved revision from the actual KV; only current stores
// that revision, avoiding a self-referential future commit revision in the wire.
// This is a pure record. It grants neither Query nor metadata-write permission.
type sandboxTTLRecoveryRecord struct {
	Version         uint32              `json:"version"`
	OriginalRaw     json.RawMessage     `json:"original_raw"`
	OriginalBirth   int64               `json:"original_birth"`
	PriorMod        int64               `json:"prior_mod"`
	PreviousFinal   *StageReference     `json:"previous_final,omitempty"`
	RecoveryFinal   StageAttemptLocator `json:"recovery_final"`
	PendingRevision int64               `json:"pending_revision"`

	reservedMod int64
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
	if !operationPermanentKV(kv) || kv.Version <= 0 || len(kv.Value) > maxRecordBytes || json.Unmarshal(kv.Value, &r) != nil {
		return r, ErrCorruptRecord
	}
	if r.Version == 2 {
		return decodeSandboxTTLRecovery(kv, pending)
	}
	if pending && (kv.CreateRevision != kv.ModRevision || kv.Version != 1) || r.Validate() != nil || pending != (r.PendingRevision == 0) {
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

func sandboxTTLRecoveryLocator(r sandboxTTLRecord, priorMod int64, previous *StageReference) (StageAttemptLocator, error) {
	digest := sha256.Sum256(r.Activation)
	framing, err := json.Marshal(struct {
		Domain          string          `json:"domain"`
		CandidateDigest string          `json:"candidate_digest"`
		PriorMod        int64           `json:"prior_mod"`
		PreviousFinal   *StageReference `json:"previous_final"`
	}{"goairix/sandbox/ttl-recovery-final/v1", hex.EncodeToString(digest[:]), priorMod, previous})
	if err != nil {
		return StageAttemptLocator{}, ErrInvalidRecord
	}
	locator := r.Final
	locator.StageID, locator.AttemptID = "ttl_recover", uuid.NewSHA1(uuid.NameSpaceOID, framing).String()
	if locator.AttemptID == r.Final.AttemptID || locator.AttemptID == r.Publish.AttemptID {
		return StageAttemptLocator{}, ErrInvalidRecord
	}
	return locator, nil
}

func (r sandboxTTLRecoveryRecord) original() (sandboxTTLRecord, error) {
	var original sandboxTTLRecord
	if r.Version != 2 || len(r.OriginalRaw) == 0 || len(r.OriginalRaw) > maxRecordBytes || r.OriginalBirth <= 0 || r.PriorMod < r.OriginalBirth || r.PendingRevision < 0 || r.PendingRevision != 0 && r.PendingRevision <= r.PriorMod || json.Unmarshal(r.OriginalRaw, &original) != nil || original.Validate() != nil || original.PendingRevision != 0 {
		return original, ErrInvalidRecord
	}
	wire, err := encodeSandboxTTL(original)
	if err != nil || wire != string(r.OriginalRaw) {
		return sandboxTTLRecord{}, ErrInvalidRecord
	}
	if (r.PriorMod == r.OriginalBirth) != (r.PreviousFinal == nil) {
		return sandboxTTLRecord{}, ErrInvalidRecord
	}
	if previous := r.PreviousFinal; previous != nil {
		if !validHexDigest(previous.Digest) || previous.StageID != "ttl_recover" || previous.Namespace != original.Final.Namespace || previous.Partition != original.Final.Partition || previous.RestoreEpoch != original.Final.RestoreEpoch || previous.RequestID != original.Final.RequestID || previous.AttemptID == original.Final.AttemptID || previous.AttemptID == original.Publish.AttemptID {
			return sandboxTTLRecord{}, ErrInvalidRecord
		}
		if id, err := uuid.Parse(previous.AttemptID); err != nil || id.String() != previous.AttemptID || id == uuid.Nil {
			return sandboxTTLRecord{}, ErrInvalidRecord
		}
	}
	locator, err := sandboxTTLRecoveryLocator(original, r.PriorMod, r.PreviousFinal)
	if err != nil || locator != r.RecoveryFinal {
		return sandboxTTLRecord{}, ErrInvalidRecord
	}
	return original, nil
}

func encodeSandboxTTLRecovery(r sandboxTTLRecoveryRecord) (string, error) {
	if _, err := r.original(); err != nil {
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

func decodeSandboxTTLRecovery(kv *mvccpb.KeyValue, pending bool) (sandboxTTLRecord, error) {
	var envelope sandboxTTLRecoveryRecord
	if json.Unmarshal(kv.Value, &envelope) != nil {
		return sandboxTTLRecord{}, ErrCorruptRecord
	}
	original, err := envelope.original()
	if err != nil || pending != (envelope.PendingRevision == 0) {
		return sandboxTTLRecord{}, ErrCorruptRecord
	}
	if pending {
		if kv.CreateRevision != envelope.OriginalBirth || kv.ModRevision <= envelope.PriorMod || kv.Version < 2 {
			return sandboxTTLRecord{}, ErrCorruptRecord
		}
		envelope.reservedMod = kv.ModRevision
	} else {
		if envelope.PendingRevision <= envelope.PriorMod || kv.ModRevision <= envelope.PendingRevision {
			return sandboxTTLRecord{}, ErrCorruptRecord
		}
		envelope.reservedMod = envelope.PendingRevision
	}
	wire, err := encodeSandboxTTLRecovery(envelope)
	if err != nil || wire != string(kv.Value) {
		return sandboxTTLRecord{}, ErrCorruptRecord
	}
	original.PendingRevision, original.recovery = envelope.PendingRevision, &envelope
	return original, nil
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
	wire, err := encodeSandboxTTL(r)
	if err != nil {
		return Mutation{}, err
	}
	return b.sandboxTTLBusinessMutation(r, pendingRevision, compares, wire)
}

// Live and recovered commits share exactly the control/current/due mutation.
// Only the private caller supplies different, independently checked permission
// comparisons and the appropriate record wire. Same-due never duplicates a key.
func (b *Backend) sandboxTTLBusinessMutation(r sandboxTTLRecord, pendingRevision int64, compares []clientv3.Cmp, wire string) (Mutation, error) {
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
	if r.recovery != nil && pendingRevision != 0 {
		mutation, err := b.sandboxTTLRecoveryMutation(r, pendingRevision)
		if err != nil {
			return Mutation{}, StageReference{}, err
		}
		return b.preflightSandboxTTLStage(r.recovery.RecoveryFinal, mutation)
	}
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

// This builder does not grant a Stage. It prepares only the fixed candidate's
// metadata repair, excluding all expired operation authority. The runtime Query
// and actual authenticated reads must independently authorize its future caller.
// The source/control pins prevent cleanup or a new mutation from being revived.
func (b *Backend) sandboxTTLRecoveryMutation(r sandboxTTLRecord, reservedMod int64) (Mutation, error) {
	if r.recovery == nil || reservedMod <= r.recovery.PriorMod || reservedMod != r.recovery.reservedMod {
		return Mutation{}, ErrInvalidRecord
	}
	envelope := *r.recovery
	original, err := envelope.original()
	if err != nil {
		return Mutation{}, err
	}
	projection := r
	projection.PendingRevision, projection.recovery = 0, nil
	wire, err := encodeSandboxTTL(projection)
	if err != nil || wire != string(envelope.OriginalRaw) {
		return Mutation{}, ErrInvalidRecord
	}
	keys, err := b.sandboxTTLAuthorityKeys(original)
	if err != nil {
		return Mutation{}, err
	}
	var compares []clientv3.Cmp
	for i, key := range keys {
		compares = append(compares, clientv3.Compare(clientv3.ModRevision(key), "=", original.SourceMods[i]), clientv3.Compare(clientv3.LeaseValue(key), "=", 0))
	}
	token, guard, receipt, mutation, err := b.namespace.operationKeys(original.Operation.Reference)
	if err != nil {
		return Mutation{}, err
	}
	for _, key := range []string{token, guard, receipt, mutation} {
		compares = append(compares, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	}
	_, publishRef, err := b.sandboxTTLStage(original, 0)
	if err != nil {
		return Mutation{}, err
	}
	_, publishReceipt, err := b.stageKeys(publishRef)
	if err != nil {
		return Mutation{}, err
	}
	compares = append(compares, clientv3.Compare(clientv3.ModRevision(publishReceipt), "=", envelope.OriginalBirth), clientv3.Compare(clientv3.LeaseValue(publishReceipt), "=", 0))
	_, oldFinal, err := b.sandboxTTLStage(original, envelope.OriginalBirth)
	if err != nil {
		return Mutation{}, err
	}
	oldGuard, oldReceipt, err := b.stageKeys(oldFinal)
	if err != nil {
		return Mutation{}, err
	}
	aborted, err := encodeReceipt(oldFinal, OutcomeAborted)
	if err != nil {
		return Mutation{}, err
	}
	compares = append(compares, clientv3.Compare(clientv3.Value(oldReceipt), "=", aborted), clientv3.Compare(clientv3.LeaseValue(oldReceipt), "=", 0), clientv3.Compare(clientv3.CreateRevision(oldGuard), "=", 0))
	pending, _, err := b.sandboxTTLKeys(original.Operation.Reference.Partition, original.Control.SandboxID)
	if err != nil {
		return Mutation{}, err
	}
	compares = append(compares, clientv3.Compare(clientv3.CreateRevision(pending), "=", envelope.OriginalBirth), clientv3.Compare(clientv3.LeaseValue(pending), "=", 0))
	envelope.PendingRevision = reservedMod
	wire, err = encodeSandboxTTLRecovery(envelope)
	if err != nil {
		return Mutation{}, err
	}
	return b.sandboxTTLBusinessMutation(original, reservedMod, compares, wire)
}

// Reserve only one deterministic metadata attempt. The actual caller must first
// complete its concrete Query and Resolve the predecessor to Aborted. Every
// relevant absence/value is still checked atomically by the returned mutation;
// this pure preparation is not permission to Grant, install, or revive a Stage.
func (b *Backend) sandboxTTLRecoveryReservation(r sandboxTTLRecord, pendingKV *mvccpb.KeyValue, previous *StageReference) (sandboxTTLRecord, Mutation, error) {
	if !operationPermanentKV(pendingKV) {
		return sandboxTTLRecord{}, Mutation{}, ErrInvalidRecord
	}
	original := r
	original.PendingRevision, original.recovery = 0, nil
	originalRaw := pendingKV.Value
	if r.recovery != nil {
		if previous == nil {
			return sandboxTTLRecord{}, Mutation{}, ErrInvalidRecord
		}
		_, oldFinal, err := b.sandboxTTLStage(r, pendingKV.ModRevision)
		if err != nil || oldFinal != *previous {
			return sandboxTTLRecord{}, Mutation{}, ErrInvalidRecord
		}
		originalRaw = r.recovery.OriginalRaw
	} else if previous != nil || !enrolledImmutableKV(pendingKV) {
		return sandboxTTLRecord{}, Mutation{}, ErrInvalidRecord
	}
	locator, err := sandboxTTLRecoveryLocator(original, pendingKV.ModRevision, previous)
	if err != nil {
		return sandboxTTLRecord{}, Mutation{}, err
	}
	envelope := &sandboxTTLRecoveryRecord{Version: 2, OriginalRaw: bytes.Clone(originalRaw), OriginalBirth: pendingKV.CreateRevision, PriorMod: pendingKV.ModRevision, RecoveryFinal: locator, reservedMod: math.MaxInt64}
	if previous != nil {
		owned := *previous
		envelope.PreviousFinal = &owned
	}
	candidate := original
	candidate.recovery = envelope
	// The complete future Stage is measured at the maximum native revision
	// width before the reservation Put or any metadata Stage allocation.
	plan, _, err := b.sandboxTTLStage(candidate, math.MaxInt64)
	if err != nil {
		return sandboxTTLRecord{}, Mutation{}, err
	}
	pending, _, err := b.sandboxTTLKeys(original.Operation.Reference.Partition, original.Control.SandboxID)
	if err != nil || string(pendingKV.Key) != pending {
		return sandboxTTLRecord{}, Mutation{}, ErrInvalidRecord
	}
	replaced := false
	for i, compare := range plan.Comparisons {
		if string(compare.Key) == pending && compare.Target == pb.Compare_MOD {
			plan.Comparisons[i] = clientv3.Compare(clientv3.ModRevision(pending), "=", pendingKV.ModRevision)
			replaced = true
		}
	}
	if !replaced {
		return sandboxTTLRecord{}, Mutation{}, ErrInvalidMutation
	}
	if previous != nil {
		guard, receipt, err := b.stageKeys(*previous)
		if err != nil {
			return sandboxTTLRecord{}, Mutation{}, err
		}
		aborted, err := encodeReceipt(*previous, OutcomeAborted)
		if err != nil {
			return sandboxTTLRecord{}, Mutation{}, err
		}
		plan.Comparisons = append(plan.Comparisons, clientv3.Compare(clientv3.Value(receipt), "=", aborted), clientv3.Compare(clientv3.LeaseValue(receipt), "=", 0), clientv3.Compare(clientv3.CreateRevision(guard), "=", 0))
	}
	envelope.reservedMod = 0
	wire, err := encodeSandboxTTLRecovery(*envelope)
	if err != nil {
		return sandboxTTLRecord{}, Mutation{}, err
	}
	plan.Writes = []Write{{Key: pending, Value: []byte(wire)}}
	return candidate, plan, nil
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
