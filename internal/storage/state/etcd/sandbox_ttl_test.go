package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This is a pure codec/transaction model, not a signed runtime or effect
// authority fixture. Real root verification, installation and final metadata
// attribution are exercised through the original HTTP native service fixture.
func sandboxTTLRecordModel(t *testing.T) (*Backend, sandboxTTLRecord) {
	t.Helper()
	o := validOptions(t)
	identity, err := json.Marshal(o.Identity)
	require.NoError(t, err)
	b := &Backend{namespace: o.Namespace, identityValue: string(identity), restoreEpoch: o.Identity.RestoreEpoch}
	b.identityKey, err = b.namespace.Key("meta", "identity")
	require.NoError(t, err)
	b.restoreKey, err = b.namespace.Key("meta", "restore_epoch")
	require.NoError(t, err)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	runtime := RuntimeReference{ID: "pod", UID: uuid.NewString(), BootID: uuid.NewString()}
	control := SandboxControlRecord{Version: 1, SandboxID: uuid.NewString(), WorkspaceHash: strings.Repeat("a", 64), IntentID: uuid.NewString(), RestoreEpoch: b.restoreEpoch, Generation: 1, DataGateEpoch: 1, Phase: PhaseActive, Snapshot: SnapshotReference{Version: "original", Digest: strings.Repeat("b", 64)}, Runtime: &runtime, ExpiresAt: now.Add(5 * time.Minute)}
	ref := OperationReference{Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, RequestID: "ttl-request", SandboxID: control.SandboxID, OperationID: uuid.NewString(), Digest: strings.Repeat("c", 64), Partition: 0xaa, Kind: OperationMutation, LeaseID: 100}
	operation := OperationRecord{Version: 1, Reference: ref, WorkspaceHash: control.WorkspaceHash, IntentID: control.IntentID, Generation: 1, DataGateEpoch: 1, ControlRevision: 11, Runtime: runtime, Snapshot: control.Snapshot, ExpiresAt: control.ExpiresAt}
	publish := StageAttemptLocator{Namespace: ref.Namespace, Partition: ref.Partition, RequestID: ref.RequestID, StageID: "ttl_prepare", AttemptID: uuid.NewString(), RestoreEpoch: ref.RestoreEpoch}
	final := publish
	final.StageID, final.AttemptID = "ttl_commit", uuid.NewString()
	r := sandboxTTLRecord{Version: 1, Operation: operation, Control: control, SourceMods: [5]int64{10, 11, 12, 13, 14}, GuardRevision: 20, AdmitRevision: 21, DueRevision: 22, Baseline: now, Timeout: 90, ExpiresAt: now.Add(90 * time.Second), PreviousDigest: strings.Repeat("d", 64), Activation: json.RawMessage(`{"model":"not-certification"}`), RuntimeCert: json.RawMessage(`{}`), IssuerCert: json.RawMessage(`{}`), Publish: publish, Final: final}
	require.NoError(t, r.Validate())
	return b, r
}

func TestSandboxTTLAtomicStagePlanAndSameDue(t *testing.T) {
	for _, same := range []bool{false, true} {
		t.Run(map[bool]string{false: "replace-due", true: "same-due"}[same], func(t *testing.T) {
			b, r := sandboxTTLRecordModel(t)
			if same {
				r.Control.ExpiresAt = r.ExpiresAt
				r.Operation.ExpiresAt = r.ExpiresAt
			}
			publish, publishRef, err := b.sandboxTTLStage(r, 0)
			require.NoError(t, err)
			require.Len(t, publish.Writes, 1)
			pending, current, err := b.sandboxTTLKeys(r.Operation.Reference.Partition, r.Control.SandboxID)
			require.NoError(t, err)
			require.Equal(t, pending, publish.Writes[0].Key)
			final, finalRef, err := b.sandboxTTLStage(r, 44)
			require.NoError(t, err)
			require.NotEqual(t, publishRef, finalRef)
			require.Equal(t, r.Final.AttemptID, finalRef.AttemptID)
			require.Equal(t, current, final.Writes[1].Key)
			require.Equal(t, Write{Key: pending, Delete: true}, final.Writes[len(final.Writes)-1])
			var updated SandboxControlRecord
			require.NoError(t, decodeDomainRecord(&mvccpb.KeyValue{Value: final.Writes[0].Value}, &updated))
			require.Equal(t, r.ExpiresAt, updated.ExpiresAt)
			require.Equal(t, r.Control.Runtime, updated.Runtime)
			require.Equal(t, r.Control.Snapshot, updated.Snapshot)
			if same {
				require.Len(t, final.Comparisons, 29)
				require.Len(t, final.Writes, 3, "unchanged due must not be deleted/recreated")
			} else {
				require.Len(t, final.Comparisons, 30)
				require.Len(t, final.Writes, 5)
				require.True(t, final.Writes[2].Delete)
				require.False(t, final.Writes[3].Delete)
			}
			seen := make(map[string]bool)
			for _, write := range final.Writes {
				require.False(t, seen[write.Key], "etcd rejects duplicate write keys")
				seen[write.Key] = true
			}
			// A current record retains the exact immutable pending birth. It
			// reconstructs both original digests without serializing its own hash.
			persisted, err := decodeSandboxTTL(&mvccpb.KeyValue{Key: []byte(current), Value: final.Writes[1].Value, CreateRevision: 50, ModRevision: 50, Version: 1}, false)
			require.NoError(t, err)
			require.EqualValues(t, 44, persisted.PendingRevision)
			_, rebuiltFinal, err := b.sandboxTTLStage(persisted, persisted.PendingRevision)
			require.NoError(t, err)
			require.Equal(t, finalRef, rebuiltFinal)
			_, rebuiltPublish, err := b.sandboxTTLStage(persisted, 0)
			require.NoError(t, err)
			require.Equal(t, publishRef, rebuiltPublish)
			rejected, rejectedRef, err := b.sandboxTTLRejectStage(r, 44)
			require.NoError(t, err)
			require.Equal(t, []Write{{Key: pending, Delete: true}}, rejected.Writes)
			require.Equal(t, "ttl_reject", rejectedRef.StageID)
		})
	}
}

func TestSandboxTTLStrictRecordsAndWireBounds(t *testing.T) {
	b, r := sandboxTTLRecordModel(t)
	wire, err := encodeSandboxTTL(r)
	require.NoError(t, err)
	operationWire, err := json.Marshal(r.Operation)
	require.NoError(t, err)
	kv := &mvccpb.KeyValue{Value: []byte(wire), CreateRevision: 44, ModRevision: 44, Version: 1}
	_, err = decodeSandboxTTL(kv, true)
	require.NoError(t, err)
	for _, value := range [][]byte{
		bytes.Replace([]byte(wire), []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		bytes.Replace([]byte(wire), append([]byte(`"operation":`), operationWire...), []byte(`"operation":null`), 1),
		append([]byte(wire), []byte(` {}`)...),
	} {
		bad := *kv
		bad.Value = value
		_, err := decodeSandboxTTL(&bad, true)
		require.Error(t, err)
	}
	for _, change := range []func(*sandboxTTLRecord){
		func(r *sandboxTTLRecord) { r.Timeout = 0 },
		func(r *sandboxTTLRecord) { r.Timeout = 365*24*60*60 + 1 },
		func(r *sandboxTTLRecord) { r.ExpiresAt = r.ExpiresAt.Add(time.Second) },
		func(r *sandboxTTLRecord) { r.SourceMods[1]++ },
		func(r *sandboxTTLRecord) { r.Operation.Reference.Kind = OperationData },
		func(r *sandboxTTLRecord) {
			r.Activation = json.RawMessage(`{"large":"` + strings.Repeat("x", 16384) + `"}`)
		},
	} {
		changed := r
		change(&changed)
		_, _, err := b.sandboxTTLStage(changed, 44)
		require.Error(t, err, "invalid input must refuse before a Stage Grant")
	}
	// Neither a lease nor an in-place rewrite is an immutable pending birth.
	for _, change := range []func(*mvccpb.KeyValue){
		func(kv *mvccpb.KeyValue) { kv.Lease = 1 },
		func(kv *mvccpb.KeyValue) {
			kv.ModRevision++
			kv.Version++
		},
	} {
		bad := *kv
		change(&bad)
		_, err := decodeSandboxTTL(&bad, true)
		require.Error(t, err)
	}
}

func TestSandboxTTLRejectsCopiedCapabilitiesBeforeRPC(t *testing.T) {
	b, r := sandboxTTLRecordModel(t)
	c := &OperationCapability{origin: b, parentCtx: context.Background(), record: r.Operation, control: r.Control}
	c.self = c
	view := &SandboxReadView{origin: b, records: SandboxReadState{Control: &r.Control}}
	view.self = view
	// Reproduce the invalid origin pointer without copying an owned mutex.
	copyOperation := &OperationCapability{self: c, origin: b, parentCtx: c.parentCtx, record: c.record, control: c.control}
	_, err := b.RenewSandboxTTL(context.Background(), copyOperation, view, r.Baseline, r.Timeout, p.TargetActivationEvidence{}, &ct.Destination{})
	require.ErrorIs(t, err, ErrInvalidRecord)
	require.Nil(t, c.ttlDraft)
	copyView := *view
	_, err = b.RenewSandboxTTL(context.Background(), c, &copyView, r.Baseline, r.Timeout, p.TargetActivationEvidence{}, &ct.Destination{})
	require.ErrorIs(t, err, ErrInvalidRecord)
	require.Nil(t, c.ttlDraft)
}

// This negative phase test supplies an already-submitted private draft solely
// to exercise arbitration. It establishes no runtime installation or successful
// TTL update: the original receipt is absent and Resolve must abort it. A zero
// Destination cannot perform transport, even if the implementation regresses.
func TestSandboxTTLUnknownFinalAttemptNeverRegrantsOrResends(t *testing.T) {
	_, options := workerTestEndpoint(t)
	b, err := New(context.Background(), options)
	require.NoError(t, err)
	defer b.Close()
	_, r := sandboxTTLRecordModel(t)
	_, ref, err := b.sandboxTTLStage(r, 44)
	require.NoError(t, err)
	_, receipt, err := b.stageKeys(ref)
	require.NoError(t, err)
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	transactions := 0
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
		transactions++
		for _, op := range ops {
			require.False(t, op.IsDelete())
			if op.IsPut() {
				require.Equal(t, receipt, string(op.KeyBytes()), "only Resolve's original aborted receipt may be written")
			}
		}
		return false
	}}
	c := &OperationCapability{origin: b, parentCtx: context.Background(), record: r.Operation, lost: true, deadline: time.Now().Add(-time.Minute)}
	c.self = c
	c.ttlDraft = &sandboxTTLDraft{record: r, finalRef: ref, finalSubmitted: true, sent: true, outcome: OutcomeUnknown, effectDeadline: time.Now().Add(-time.Minute)}
	view := &SandboxReadView{origin: b, records: SandboxReadState{Control: &r.Control}}
	view.self = view
	for i := 0; i < 2; i++ {
		result, err := b.RenewSandboxTTL(context.Background(), c, view, r.Baseline, r.Timeout, p.TargetActivationEvidence{}, &ct.Destination{})
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.Equal(t, OutcomeAborted, result.Outcome, "metadata abort cannot claim that runtime installation was undone")
		require.Equal(t, ref, result.Stage)
		require.Nil(t, c.ttlDraft.final)
	}
	require.Equal(t, 2, transactions)
	require.Zero(t, lease.grants.Load())
	require.Zero(t, lease.keeps.Load())
}

func TestSandboxTTLTrustedExpiryRejectsBeforeStageOrTransport(t *testing.T) {
	_, options := workerTestEndpoint(t)
	_, r := sandboxTTLRecordModel(t)
	clockCalls := 0
	var observedDeadline time.Time
	options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{make([]byte, ed25519.PublicKeySize)}}
	options.Clock = testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
		clockCalls++
		observedDeadline, _ = ctx.Deadline()
		return ClockObservation{UTC: r.ExpiresAt.Add(-time.Second)}, nil
	})
	b, err := New(context.Background(), options)
	require.NoError(t, err)
	defer b.Close()
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	c := &OperationCapability{origin: b, parentCtx: context.Background(), deadline: time.Now().Add(time.Minute)}
	d := &sandboxTTLDraft{record: r, effectDeadline: time.Now().Add(500 * time.Millisecond)}
	require.ErrorIs(t, b.sandboxTTLLive(context.Background(), c, d), ErrOperationAdmissionClosed, "now+one-second equals expiry and must reject before root or transport work")
	require.Equal(t, 1, clockCalls)
	require.True(t, observedDeadline.Equal(d.effectDeadline), "the actual clock call must inherit the fixed effect deadline rather than the renewed admission")
	require.Zero(t, lease.grants.Load())
	require.Nil(t, d.publish)
	require.Nil(t, d.final)
}

// Each independent authority can be the first to expire. The captured local
// deadline must use the trusted observation's immediate monotonic anchor, not a
// later wall-clock calculation or a refreshed observation on retry.
func TestSandboxTTLEffectDeadlineRetainsEarliestAuthority(t *testing.T) {
	trusted := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	anchor := time.Now()
	for selected := 0; selected < 5; selected++ {
		limits := []time.Time{anchor.Add(time.Minute), trusted.Add(time.Minute), trusted.Add(time.Minute), trusted.Add(time.Minute), trusted.Add(time.Minute)}
		if selected == 0 {
			limits[0] = anchor.Add(5 * time.Second)
		} else {
			limits[selected] = trusted.Add(5 * time.Second)
		}
		deadline := sandboxTTLEffectDeadline(anchor, trusted, limits[0], limits[1:]...)
		require.True(t, deadline.Equal(anchor.Add(4*time.Second)), "authority %d must include the one-second margin", selected)
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		actual, ok := ctx.Deadline()
		require.True(t, ok)
		require.True(t, actual.Equal(deadline))
		cancel()
	}
	deadline := sandboxTTLEffectDeadline(anchor, trusted, anchor.Add(time.Minute), time.Time{})
	require.False(t, deadline.After(anchor), "a saturated negative date difference must never wrap into fresh authority")
}

// This calls the actual public entry with a retained original draft. The
// Backend has no client, so any accidental publication, Grant or transport
// attempt would fail the test instead of masking the expired effect boundary.
func TestSandboxTTLExpiredCapturedDeadlineRejectsBeforeRPC(t *testing.T) {
	b, r := sandboxTTLRecordModel(t)
	c := &OperationCapability{origin: b, parentCtx: context.Background(), record: r.Operation, control: r.Control, deadline: time.Now().Add(time.Minute)}
	c.self = c
	c.ttlDraft = &sandboxTTLDraft{record: r, effectDeadline: time.Now().Add(-time.Second), outcome: OutcomeUnknown}
	view := &SandboxReadView{origin: b, records: SandboxReadState{Control: &r.Control}}
	view.self = view
	for i := 0; i < 2; i++ {
		_, err := b.RenewSandboxTTL(context.Background(), c, view, r.Baseline, r.Timeout, p.TargetActivationEvidence{}, &ct.Destination{})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.False(t, c.ttlDraft.publishTried)
		require.False(t, c.ttlDraft.sent)
		require.False(t, c.ttlDraft.finalTried)
	}
}

// This is only a canonical record/transaction model. Its opaque certification
// bytes cannot pass the actual reader or authorize a runtime query or recovery.
// The original publication is embedded verbatim; recovery coordinates are a
// separate envelope and must never alter the original Stage digest.
type sandboxTTLRecoveryWireModel struct {
	Version         uint32              `json:"version"`
	OriginalRaw     json.RawMessage     `json:"original_raw"`
	OriginalBirth   int64               `json:"original_birth"`
	PriorMod        int64               `json:"prior_mod"`
	PreviousFinal   *StageReference     `json:"previous_final,omitempty"`
	RecoveryFinal   StageAttemptLocator `json:"recovery_final"`
	PendingRevision int64               `json:"pending_revision"`
}

func sandboxTTLRecoveryKVModel(t *testing.T, b *Backend, original sandboxTTLRecord) (*mvccpb.KeyValue, sandboxTTLRecoveryWireModel) {
	t.Helper()
	wire, err := encodeSandboxTTL(original)
	require.NoError(t, err)
	final := original.Final
	// Canonical field framing plus a purpose domain binds one cold locator to
	// exactly this candidate and native predecessor. This is identity only.
	digest := sha256.Sum256(original.Activation)
	framing, err := json.Marshal(struct {
		Domain          string          `json:"domain"`
		CandidateDigest string          `json:"candidate_digest"`
		PriorMod        int64           `json:"prior_mod"`
		PreviousFinal   *StageReference `json:"previous_final"`
	}{"goairix/sandbox/ttl-recovery-final/v1", hex.EncodeToString(digest[:]), 44, nil})
	require.NoError(t, err)
	final.StageID, final.AttemptID = "ttl_recover", uuid.NewSHA1(uuid.NameSpaceOID, framing).String()
	envelope := sandboxTTLRecoveryWireModel{Version: 2, OriginalRaw: json.RawMessage(wire), OriginalBirth: 44, PriorMod: 44, RecoveryFinal: final}
	value, err := json.Marshal(envelope)
	require.NoError(t, err)
	pending, _, err := b.sandboxTTLKeys(original.Operation.Reference.Partition, original.Control.SandboxID)
	require.NoError(t, err)
	return &mvccpb.KeyValue{Key: []byte(pending), Value: value, CreateRevision: 44, ModRevision: 80, Version: 2}, envelope
}

func TestSandboxTTLRecoveryEnvelopePreservesOriginalPublication(t *testing.T) {
	b, original := sandboxTTLRecordModel(t)
	legacyWire, err := encodeSandboxTTL(original)
	require.NoError(t, err)
	legacy, err := decodeSandboxTTL(&mvccpb.KeyValue{Value: []byte(legacyWire), CreateRevision: 44, ModRevision: 44, Version: 1}, true)
	require.NoError(t, err)
	_, publish, err := b.sandboxTTLStage(legacy, 0)
	require.NoError(t, err)
	_, oldFinal, err := b.sandboxTTLStage(legacy, 44)
	require.NoError(t, err)
	kv, envelope := sandboxTTLRecoveryKVModel(t, b, original)
	recovered, err := decodeSandboxTTL(kv, true)
	require.NoError(t, err, "an explicit recovery envelope may advance Mod without rewriting the original publication")
	_, rebuilt, err := b.sandboxTTLStage(recovered, 0)
	require.NoError(t, err)
	require.Equal(t, publish, rebuilt)
	legacyAgain, err := encodeSandboxTTL(legacy)
	require.NoError(t, err)
	require.Equal(t, legacyWire, legacyAgain, "legacy v1 bytes and their original digest remain unchanged")

	mutation, recoveryFinal, err := b.sandboxTTLStage(recovered, kv.ModRevision)
	require.NoError(t, err)
	require.Equal(t, envelope.RecoveryFinal.AttemptID, recoveryFinal.AttemptID)
	require.Equal(t, "ttl_recover", recoveryFinal.StageID)
	require.NotEqual(t, oldFinal, recoveryFinal, "recovery must not re-grant the original Final Stage")
	_, currentKey, err := b.sandboxTTLKeys(original.Operation.Reference.Partition, original.Control.SandboxID)
	require.NoError(t, err)
	var currentEnvelope sandboxTTLRecoveryWireModel
	require.NoError(t, json.Unmarshal(mutation.Writes[1].Value, &currentEnvelope))
	require.Equal(t, envelope.OriginalRaw, currentEnvelope.OriginalRaw)
	require.Equal(t, envelope.OriginalBirth, currentEnvelope.OriginalBirth)
	require.Equal(t, kv.ModRevision, currentEnvelope.PendingRevision)
	current, err := decodeSandboxTTL(&mvccpb.KeyValue{Key: []byte(currentKey), Value: mutation.Writes[1].Value, CreateRevision: 90, ModRevision: 90, Version: 1}, false)
	require.NoError(t, err)
	_, rebuiltRecovery, err := b.sandboxTTLStage(current, currentEnvelope.PendingRevision)
	require.NoError(t, err)
	require.Equal(t, recoveryFinal, rebuiltRecovery)
	_, rebuiltPublish, err := b.sandboxTTLStage(current, 0)
	require.NoError(t, err)
	require.Equal(t, publish, rebuiltPublish)
}

func TestSandboxTTLRecoveryEnvelopeRejectsForeignCoordinates(t *testing.T) {
	b, original := sandboxTTLRecordModel(t)
	for _, change := range []func(*mvccpb.KeyValue, *sandboxTTLRecoveryWireModel){
		func(_ *mvccpb.KeyValue, r *sandboxTTLRecoveryWireModel) { r.OriginalBirth++ },
		func(_ *mvccpb.KeyValue, r *sandboxTTLRecoveryWireModel) { r.PriorMod = r.OriginalBirth - 1 },
		func(kv *mvccpb.KeyValue, r *sandboxTTLRecoveryWireModel) { r.PriorMod = kv.ModRevision },
		func(_ *mvccpb.KeyValue, r *sandboxTTLRecoveryWireModel) { r.RecoveryFinal = original.Final },
		func(_ *mvccpb.KeyValue, r *sandboxTTLRecoveryWireModel) { r.RecoveryFinal.AttemptID = uuid.NewString() },
		func(_ *mvccpb.KeyValue, r *sandboxTTLRecoveryWireModel) { r.RecoveryFinal.Partition++ },
		func(_ *mvccpb.KeyValue, r *sandboxTTLRecoveryWireModel) { r.PendingRevision = 80 },
		func(kv *mvccpb.KeyValue, _ *sandboxTTLRecoveryWireModel) { kv.Lease = 1 },
		func(kv *mvccpb.KeyValue, _ *sandboxTTLRecoveryWireModel) { kv.Version = 1 },
	} {
		kv, envelope := sandboxTTLRecoveryKVModel(t, b, original)
		change(kv, &envelope)
		var err error
		kv.Value, err = json.Marshal(envelope)
		require.NoError(t, err)
		_, err = decodeSandboxTTL(kv, true)
		require.Error(t, err)
	}
}

func TestSandboxTTLRecoveryStagePinsDeadOperationAndExactSources(t *testing.T) {
	for _, sameDue := range []bool{false, true} {
		b, original := sandboxTTLRecordModel(t)
		if sameDue {
			original.Control.ExpiresAt = original.ExpiresAt
			original.Operation.ExpiresAt = original.ExpiresAt
		}
		kv, _ := sandboxTTLRecoveryKVModel(t, b, original)
		r, err := decodeSandboxTTL(kv, true)
		require.NoError(t, err)
		mutation, ref, err := b.sandboxTTLStage(r, kv.ModRevision)
		require.NoError(t, err)
		wantComparisons, wantWrites, wantNodes := 25, 5, 43
		if sameDue {
			wantComparisons, wantWrites, wantNodes = 24, 3, 40
		}
		require.Len(t, mutation.Comparisons, wantComparisons)
		require.Len(t, mutation.Writes, wantWrites)
		sources, err := b.sandboxTTLAuthorityKeys(original)
		require.NoError(t, err)
		for i, key := range sources {
			require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.ModRevision(key), "=", original.SourceMods[i]), "Destroy or a foreign source incarnation must win its CAS")
			require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.LeaseValue(key), "=", 0))
		}
		token, oldGuard, receipt, sharedMutation, err := b.namespace.operationKeys(original.Operation.Reference)
		require.NoError(t, err)
		for _, key := range []string{token, oldGuard, receipt, sharedMutation} {
			require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.CreateRevision(key), "=", 0), "neither the old operation nor a new mutation may coexist with recovery")
		}
		_, originalFinal, err := b.sandboxTTLStage(original, kv.CreateRevision)
		require.NoError(t, err)
		finalGuard, finalReceipt, err := b.stageKeys(originalFinal)
		require.NoError(t, err)
		aborted, err := encodeReceipt(originalFinal, OutcomeAborted)
		require.NoError(t, err)
		require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.Value(finalReceipt), "=", aborted))
		require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.LeaseValue(finalReceipt), "=", 0))
		require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.CreateRevision(finalGuard), "=", 0))
		_, originalPublish, err := b.sandboxTTLStage(original, 0)
		require.NoError(t, err)
		_, publishReceipt, err := b.stageKeys(originalPublish)
		require.NoError(t, err)
		require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.ModRevision(publishReceipt), "=", kv.CreateRevision), "the original publication receipt retains its original native birth")
		require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.LeaseValue(publishReceipt), "=", 0))
		require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.CreateRevision(string(kv.Key)), "=", kv.CreateRevision))
		require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.ModRevision(string(kv.Key)), "=", kv.ModRevision))
		require.Contains(t, mutation.Comparisons, clientv3.Compare(clientv3.LeaseValue(string(kv.Key)), "=", 0))
		seen := make(map[string]bool)
		for _, write := range mutation.Writes {
			require.False(t, seen[write.Key], "same-due cannot delete and recreate the same key")
			seen[write.Key] = true
		}

		// This is the actual CommitStage envelope shape, not a count of an
		// invented DTO. Maximum-width guard coordinates cover the native lease.
		guard, recoveryReceipt, err := b.stageKeys(ref)
		require.NoError(t, err)
		guardWire, err := json.Marshal(struct {
			StageReference
			LeaseID int64 `json:"lease_id"`
		}{ref, math.MaxInt64})
		require.NoError(t, err)
		committed, err := encodeReceipt(ref, OutcomeCommitted)
		require.NoError(t, err)
		compares := append(b.baseComparisons(), operationEnvelopeComparisons(guard, string(guardWire), math.MaxInt64, math.MaxInt64)[:3]...)
		compares = append(compares, clientv3.Compare(clientv3.CreateRevision(recoveryReceipt), "=", 0))
		compares = append(compares, mutation.Comparisons...)
		writes := append(append([]Write(nil), mutation.Writes...), Write{Key: recoveryReceipt, Value: []byte(committed)})
		failureKeys := []string{b.identityKey, b.restoreKey, recoveryReceipt, guard}
		request := &pb.TxnRequest{Compare: taskProtoComparisons(compares), Success: taskProtoWrites(writes), Failure: taskProtoPoints(failureKeys)}
		require.Equal(t, wantNodes, len(request.Compare)+len(request.Success)+len(request.Failure))
		require.LessOrEqual(t, request.Size(), 128<<10)
		failure := taskProtoResponse(failureKeys, []string{b.identityValue, b.restoreEpoch, strings.Repeat("x", maxRecordBytes), strings.Repeat("x", maxRecordBytes)})
		require.LessOrEqual(t, failure.Size(), 256<<10)
		t.Logf("TTL recovery component same_due=%t nodes=%d request_pb=%d failure_pb=%d", sameDue, wantNodes, request.Size(), failure.Size())
	}
}

type sandboxTTLColdProtocolFixture struct {
	duePublicationFixture
	view        *SandboxReadView
	activation  p.TargetActivationEvidence
	original    sandboxTTLRecord
	pending     *mvccpb.KeyValue
	destination *ct.Destination
	queries     atomic.Int32
	installs    atomic.Int32
	querySeen   chan struct{}
	queryReply  chan struct{}
}

// The existing signed publication producer and actual Backend/Stage/KV paths
// establish all metadata here. The mTLS peer is a protocol model reporting its
// selected installed pair, not evidence of a real Supervisor/journal install.
// No response DTO or authorization callback is injected into the Backend.
func newSandboxTTLColdProtocolFixture(t *testing.T, blockQuery bool) *sandboxTTLColdProtocolFixture {
	t.Helper()
	f := &sandboxTTLColdProtocolFixture{duePublicationFixture: newDuePublicationFixture(t), querySeen: make(chan struct{}, 4), queryReply: make(chan struct{})}
	ctx := context.Background()
	published, err := f.b.PublishRuntimeEnrolled(ctx, f.claim, f.proof, f.enrollment)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	f.view, err = f.b.ReadSandboxView(ctx, f.claim.reference.SandboxID)
	require.NoError(t, err)
	initial, err := f.b.sandboxTTLInitialActivation(ctx, f.view)
	require.NoError(t, err)
	birth, binding := initial.Birth(), initial.Binding()
	runtimeDigest, issuerDigest := sha256.Sum256(initial.RuntimeCertificate()), sha256.Sum256(initial.IssuerCertificate())
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	wire, err := p.SignTargetActivation(root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: initial.Identity(), DataGateEpoch: initial.DataGateEpoch(), UID: birth.UID, GID: birth.GID, NetworkAllowed: birth.NetworkAllowed, ContractDigest: birth.ContractDigest, RuntimeCertificateDigest: hex.EncodeToString(runtimeDigest[:]), IssuerCertificateDigest: hex.EncodeToString(issuerDigest[:]), NotBefore: f.now.Add(-time.Second), NotAfter: f.now.Add(4 * time.Minute)})
	require.NoError(t, err)
	f.activation, err = f.b.enrolledVerifier.VerifyTargetActivation(wire, initial.RuntimeCertificate(), initial.IssuerCertificate(), birth, f.now)
	require.NoError(t, err)
	admitted, err := f.b.BeginOperation(ctx, BeginOperationInput{SandboxID: initial.Identity().SandboxID, RequestID: "ttl-cold-model", Kind: OperationMutation})
	require.NoError(t, err)
	require.Equal(t, OperationCommitted, admitted.Outcome)
	require.NotNil(t, admitted.Capability)
	c := admitted.Capability
	c.mu.Lock()
	draft, err := f.b.prepareSandboxTTL(ctx, c, f.view, f.now, 120, f.activation)
	if err == nil {
		err = f.b.publishSandboxTTL(ctx, c, draft)
	}
	c.mu.Unlock()
	require.NoError(t, err)
	f.original = draft.record
	require.NoError(t, f.b.CancelOperationCapability(ctx, c))
	pendingKey, _, err := f.b.sandboxTTLKeys(c.record.Reference.Partition, c.record.Reference.SandboxID)
	require.NoError(t, err)
	f.pending = f.server.point(pendingKey)
	require.NotNil(t, f.pending)
	require.True(t, enrolledImmutableKV(f.pending))
	token, guard, receipt, mutation, err := f.b.namespace.operationKeys(c.record.Reference)
	require.NoError(t, err)
	for _, key := range []string{token, guard, receipt, mutation} {
		require.Nil(t, f.server.point(key), "actual original Lease revoke retires all four operation envelopes")
	}

	runtimeKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
	runtimeCredential, err := p.NewManagementTLSCredential(runtimeKey, initial.RuntimeCertificate(), "runtime_receipt")
	require.NoError(t, err)
	issuerCredential, err := p.NewManagementTLSCredential(issuerKey, initial.IssuerCertificate(), "command_issuer")
	require.NoError(t, err)
	config, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: f.b.enrolledVerifier, Clock: f.b.authorityClock, Credential: runtimeCredential, PeerCertificate: initial.IssuerCertificate(), Server: true})
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	f.destination, err = ct.NewDestination(ct.DestinationOptions{Identity: initial.Identity(), RuntimeCertificate: f.activation.RuntimeCertificate(), Credential: issuerCredential, Verifier: f.b.enrolledVerifier, Clock: f.b.authorityClock, Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}})
	require.NoError(t, err)
	if !blockQuery {
		close(f.queryReply)
	}
	serverCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
				conn := tls.Server(raw, config)
				request, err := ct.ReadRuntimeRequest(conn)
				if err != nil || request.Renewal == nil {
					return
				}
				e := request.Renewal
				if e.Purpose != ct.RuntimeRenewalQueryPurpose {
					f.installs.Add(1)
					return
				}
				f.queries.Add(1)
				if e.Identity != f.activation.Identity() || e.DataGateEpoch != f.activation.DataGateEpoch() || e.PreviousDigest != f.original.PreviousDigest || e.CandidateDigest != f.activation.Digest() {
					return
				}
				select {
				case f.querySeen <- struct{}{}:
				case <-serverCtx.Done():
					return
				}
				select {
				case <-f.queryReply:
				case <-serverCtx.Done():
					return
				}
				response := ct.RuntimeRenewalResponse{Version: 1, Purpose: "runtime_activation_renewed", Identity: e.Identity, DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, PreviousDigest: e.PreviousDigest, CandidateDigest: e.CandidateDigest, Installed: true, GateState: "open"}
				if ct.WriteRuntimeRenewalResponse(conn, response) == nil {
					_ = conn.CloseWrite()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, listener.Close())
		<-done
		require.NoError(t, issuerCredential.Close())
		require.NoError(t, runtimeCredential.Close())
	})
	return f
}

func (f *sandboxTTLColdProtocolFixture) recover(ctx context.Context) (SandboxTTLResult, error) {
	return f.b.RenewSandboxTTL(ctx, nil, f.view, f.original.Baseline, f.original.Timeout, f.activation, f.destination)
}

func TestSandboxTTLColdHandlerCommitsInstalledCandidateAfterOriginalAbort(t *testing.T) {
	f := newSandboxTTLColdProtocolFixture(t, false)
	lease := &creationFaultLease{Lease: f.b.client.Lease}
	f.b.client.Lease = lease
	result, err := f.recover(context.Background())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, "ttl_recover", result.Stage.StageID)
	require.Equal(t, int32(1), f.queries.Load())
	require.Zero(t, f.installs.Load())
	require.EqualValues(t, 1, lease.grants.Load(), "only a fresh metadata Stage may be granted")
	require.Nil(t, f.server.point(string(f.pending.Key)))
	view, err := f.b.ReadSandboxView(context.Background(), f.original.Control.SandboxID)
	require.NoError(t, err)
	current, err := f.b.ReadSandboxTTLCurrent(context.Background(), view)
	require.NoError(t, err)
	require.NotNil(t, current)
	require.Equal(t, f.activation.Wire(), current.Activation.Wire())
	require.Equal(t, f.original.Baseline, current.Baseline)
	_, oldFinal, err := f.b.sandboxTTLStage(f.original, f.pending.CreateRevision)
	require.NoError(t, err)
	outcome, err := f.b.ResolveStage(context.Background(), oldFinal)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
}

func TestSandboxTTLColdHandlerLostCommitACKOnlyArbitratesSameRecovery(t *testing.T) {
	f := newSandboxTTLColdProtocolFixture(t, false)
	controlKey, _, err := f.b.namespace.sandboxKeys(f.original.Operation.Reference.Partition, f.original.Control.SandboxID, "read")
	require.NoError(t, err)
	lease := &creationFaultLease{Lease: f.b.client.Lease}
	f.b.client.Lease = lease
	dropped := 0
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(controlKey), after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if err == nil && response.Succeeded && dropped == 0 {
			dropped++
			return nil, errors.New("lost actual recovery commit acknowledgement")
		}
		return response, err
	}}
	first, err := f.recover(context.Background())
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, 1, dropped, "the original server must have committed before losing its response")
	require.Nil(t, f.server.point(string(f.pending.Key)))
	_, currentKey, err := f.b.sandboxTTLKeys(f.original.Operation.Reference.Partition, f.original.Control.SandboxID)
	require.NoError(t, err)
	committed := f.server.point(currentKey)
	require.NotNil(t, committed)
	f.view, err = f.b.ReadSandboxView(context.Background(), f.original.Control.SandboxID)
	require.NoError(t, err)
	_, originalReceipt, err := f.b.stageKeys(first.Stage)
	require.NoError(t, err)
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool {
		for _, op := range ops {
			require.False(t, op.IsDelete(), "arbitration must not resubmit any business mutation")
			if op.IsPut() {
				require.Equal(t, originalReceipt, string(op.KeyBytes()), "only the original Stage resolver may propose an aborted receipt")
			}
		}
		return false
	}}
	second, err := f.recover(context.Background())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, second.Outcome)
	require.Equal(t, first.Stage, second.Stage)
	require.Equal(t, committed, f.server.point(currentKey), "receipt arbitration cannot rewrite the committed candidate")
	require.EqualValues(t, 1, lease.grants.Load())
	require.Zero(t, f.installs.Load())
}

func TestSandboxTTLColdHandlerLostReservationACKRetainsFixedAttempt(t *testing.T) {
	f := newSandboxTTLColdProtocolFixture(t, false)
	lease := &creationFaultLease{Lease: f.b.client.Lease}
	f.b.client.Lease = lease
	reservations := 0
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(string(f.pending.Key)), after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if err == nil && response.Succeeded {
			reservations++
			if reservations == 1 {
				return nil, errors.New("lost actual pending reservation acknowledgement")
			}
		}
		return response, err
	}}
	_, err := f.recover(context.Background())
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, 1, reservations)
	require.Zero(t, lease.grants.Load(), "an unacknowledged reservation cannot return a metadata Stage owner")
	reservedKV := f.server.point(string(f.pending.Key))
	require.NotNil(t, reservedKV)
	require.Equal(t, f.pending.CreateRevision, reservedKV.CreateRevision)
	require.Greater(t, reservedKV.ModRevision, f.pending.ModRevision)
	reserved, err := decodeSandboxTTL(reservedKV, true)
	require.NoError(t, err)
	require.Equal(t, string(f.pending.Value), string(reserved.recovery.OriginalRaw))
	_, firstRef, err := f.b.sandboxTTLStage(reserved, reservedKV.ModRevision)
	require.NoError(t, err)

	// The next cold request cannot re-grant this abandoned Stage. It must
	// actually seal Aborted, then make one new metadata-only reservation.
	result, err := f.recover(context.Background())
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.NotEqual(t, firstRef, result.Stage)
	require.Equal(t, 2, reservations, "each request advances at most one fixed reservation")
	require.EqualValues(t, 1, lease.grants.Load())
	_, oldReceipt, err := f.b.stageKeys(firstRef)
	require.NoError(t, err)
	outcome, err := decodeReceipt(f.server.point(oldReceipt), firstRef)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	_, currentKey, err := f.b.sandboxTTLKeys(f.original.Operation.Reference.Partition, f.original.Control.SandboxID)
	require.NoError(t, err)
	current, err := decodeSandboxTTL(f.server.point(currentKey), false)
	require.NoError(t, err)
	require.Equal(t, string(f.pending.Value), string(current.recovery.OriginalRaw))
	require.Equal(t, &firstRef, current.recovery.PreviousFinal)
	require.Equal(t, f.pending.CreateRevision, current.recovery.OriginalBirth)
	require.Zero(t, f.installs.Load())
	oldGuard, _, err := f.b.stageKeys(firstRef)
	require.NoError(t, err)
	aborted, err := encodeReceipt(firstRef, OutcomeAborted)
	require.NoError(t, err)
	f.server.mu.Lock()
	requests := append([]*pb.TxnRequest(nil), f.server.requests...)
	f.server.mu.Unlock()
	var rotated *pb.TxnRequest
	for _, request := range requests {
		for _, op := range request.Success {
			put := op.GetRequestPut()
			if put == nil || string(put.Key) != string(f.pending.Key) {
				continue
			}
			var envelope sandboxTTLRecoveryRecord
			if json.Unmarshal(put.Value, &envelope) == nil && envelope.PreviousFinal != nil && *envelope.PreviousFinal == firstRef {
				rotated = request
			}
		}
	}
	require.NotNil(t, rotated)
	for _, compare := range taskProtoComparisons([]clientv3.Cmp{clientv3.Compare(clientv3.Value(oldReceipt), "=", aborted), clientv3.Compare(clientv3.LeaseValue(oldReceipt), "=", 0), clientv3.Compare(clientv3.CreateRevision(oldGuard), "=", 0)}) {
		require.Contains(t, rotated.Compare, compare, "the actual reservation CAS must fence the prior attempt's durable abort and absent guard")
	}
	require.Equal(t, 37, len(rotated.Compare)+len(rotated.Success)+len(rotated.Failure))
	require.LessOrEqual(t, rotated.Size(), 128<<10)
}

func TestSandboxTTLColdHandlerDestroyWinsDuringOriginalQuery(t *testing.T) {
	f := newSandboxTTLColdProtocolFixture(t, true)
	lease := &creationFaultLease{Lease: f.b.client.Lease}
	f.b.client.Lease = lease
	type answer struct {
		result SandboxTTLResult
		err    error
	}
	done := make(chan answer, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(f.queryReply) }) }
	joined := false
	t.Cleanup(func() {
		release()
		if !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("original cold handler did not join during failure cleanup")
			}
		}
	})
	go func() {
		result, err := f.recover(context.Background())
		done <- answer{result, err}
	}()
	select {
	case <-f.querySeen:
	case <-time.After(3 * time.Second):
		t.Fatal("the actual concrete TLS query did not reach its original peer")
	}
	stage, _ := taskDestroyPrepare(t, f.b, context.Background(), BeginDestroyInput{SandboxID: f.original.Control.SandboxID, RequestID: uuid.NewString(), ExpectedControlRevision: f.view.ControlRevision()})
	outcome, err := f.b.CommitStage(context.Background(), stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	require.NoError(t, f.b.ReleaseStage(context.Background(), stage))
	grantsAfterDestroy := lease.grants.Load()
	release()
	select {
	case got := <-done:
		joined = true
		require.Error(t, got.err)
		require.NotEqual(t, OutcomeCommitted, got.result.Outcome)
	case <-time.After(3 * time.Second):
		t.Fatal("the original cold handler did not return after its TLS query joined")
	}
	require.Equal(t, f.pending, f.server.point(string(f.pending.Key)), "Destroy's changed source cannot be repaired back to Active")
	require.Equal(t, grantsAfterDestroy, lease.grants.Load())
	require.Zero(t, f.installs.Load())
}
