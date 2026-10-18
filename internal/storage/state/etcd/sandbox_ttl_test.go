package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
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
