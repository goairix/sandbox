package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/kubecontract"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const duePublicationStageForTest = "runtime_publish_enrolled_v3"

type duePublicationFixture struct {
	b          *Backend
	server     *workerTestServer
	claim      *CreationClaim
	proof      []byte
	enrollment p.EnrolledRuntimeEvidence
	now        time.Time
}

// All metadata goes through the real client and bounded gRPC model. This
// fixture has no Docker prerequisite and cannot silently skip the RED.
func newDuePublicationFixture(t *testing.T) duePublicationFixture {
	t.Helper()
	return newDuePublicationFixtureForSource(t, "s3", nil)
}

// The optional source profile changes only construction inputs. Original due
// tests retain the same s3 identity, arbitrary contract and payload-only snapshot.
func newDuePublicationFixtureForSource(t *testing.T, profile string, detached *DetachedCleanupOptions, configure ...func(*Options)) duePublicationFixture {
	t.Helper()
	server, o := workerTestEndpoint(t)
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	now := time.Now().UTC()
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "target", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
	o.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: now}, nil })
	o.EnrolledPublication = &EnrolledPublicationOptions{ContractDigest: strings.Repeat("b", 64), MaxActive: 4}
	if detached != nil {
		contract, err := kubecontract.EnrolledPlainContractDigest(detached.PinnedPlainImage)
		require.NoError(t, err)
		o.EnrolledPublication.ContractDigest = contract
		o.DetachedCleanup = detached
	}
	// Actor-specific controls are selected before New owns/validates options.
	// Existing publication/classification tests retain their original defaults.
	for _, apply := range configure {
		apply(&o)
	}
	b, err := New(context.Background(), o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	idempotencyKey := uuid.NewString()
	var w WorkspaceIdentity
	if profile == "detached" {
		w, err = b.DetachedIdentity("due-test", idempotencyKey)
	} else {
		w, err = NewWorkspaceIdentity(profile, o.Identity.StorageID, "bucket", uuid.NewString()+"/")
	}
	require.NoError(t, err)
	in := AcquireIntentInput{Workspace: w, Principal: "due-test", IdempotencyKey: idempotencyKey, RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"html":"<>&中>"}`), Now: now, TTL: time.Hour}
	if profile == "local" {
		plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w})
		require.NoError(t, err)
		in.LocalCleanupPlan = &plan
	}
	acquired, err := b.AcquireIntent(context.Background(), in)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquired.Outcome)
	c, err := b.ClaimCreation(context.Background(), w, in.IntentID, "due-test", 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.ReleaseCreationClaim(context.Background(), c)) })
	dispatch, err := b.DeclareRuntimeDispatch(context.Background(), c, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "target", Payload: json.RawMessage(`{"html":"<>&中>"}`)})
	require.NoError(t, err)
	runtime := RuntimeReference{ID: "pod", UID: uuid.NewString(), BootID: uuid.NewString()}
	certificate := preparationCertificate(t, b, dispatch.Entry.Record, root, "plain", runtime)
	_, err = b.BindRuntime(context.Background(), c, certificate)
	require.NoError(t, err)
	_, err = b.ConsumeRuntimeMount(context.Background(), c)
	require.NoError(t, err)
	proof, enrollment := duePublicationMaterial(t, b, c, root, certificate, runtime, now)
	return duePublicationFixture{b: b, server: server, claim: c, proof: proof, enrollment: enrollment, now: now}
}

// This is test-issued cryptographic enrollment, not a kernel observation or
// runtime execution claim. The native due test independently owns its members.
func duePublicationMaterial(t *testing.T, b *Backend, c *CreationClaim, root ed25519.PrivateKey, certificate []byte, runtime RuntimeReference, now time.Time) ([]byte, p.EnrolledRuntimeEvidence) {
	t.Helper()
	runtimeKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
	observerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{6}, 32))
	nb, na := now.Add(-30*time.Second), now.Add(5*time.Minute)
	binding := p.TrustBinding{Namespace: b.namespace.Root(), AuthorityID: b.publicationAuthorityID, Target: b.publicationTarget, RestoreEpoch: b.restoreEpoch}
	verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	identity := p.RuntimeIdentityContext{SandboxID: c.reference.SandboxID, WorkspaceHash: c.workspace.Hash(), Generation: c.reference.Generation, Runtime: p.RuntimeReference(runtime)}
	runtimeCertificate, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: runtimeKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, Runtime: identity.Runtime})
	require.NoError(t, err)
	issuerCertificate, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	rd, _ := snapshotDigest(runtimeCertificate)
	id, _ := snapshotDigest(issuerCertificate)
	activation, err := p.SignTargetActivation(root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: identity, DataGateEpoch: c.control.DataGateEpoch, UID: 1000, GID: 1000, ContractDigest: b.enrolledPublication.ContractDigest, RuntimeCertificateDigest: rd, IssuerCertificateDigest: id, NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	node, boot := uuid.NewString(), uuid.NewString()
	observerCertificate, err := p.SignNodeObserverCertificate(root, p.NodeObserverCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "node_observer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, NodeUID: node, NodeBootID: boot, PublicKey: observerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	observer, err := verifier.VerifyNodeObserverCertificate(observerCertificate, node, boot, now)
	require.NoError(t, err)
	ad, _ := snapshotDigest(activation)
	receipt, err := p.SignNodeEnrollmentReceipt(observerKey, observer, p.NodeEnrollmentReceiptClaims{Version: 1, Purpose: "runtime_node_enrolled", EnrollmentDigest: strings.Repeat("e", 64), ObserverCertificateDigest: observer.Digest(), NodeUID: node, NodeBootID: boot, ActivationDigest: ad, Runtime: identity.Runtime, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, CreationIntentID: c.reference.IntentID, ObservedAt: now})
	require.NoError(t, err)
	enrollment := p.EnrolledRuntimeEvidence{Request: p.NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", RuntimeCertificate: runtimeCertificate, Activation: activation, IssuerCertificate: issuerCertificate}, ObserverCertificate: observerCertificate, Receipt: receipt.Wire()}
	cd, _ := snapshotDigest(certificate)
	proof, err := p.SignEnrolledReadyReceipt(runtimeKey, certificate, p.EnrolledReadyReceiptClaims{ReadyReceiptClaims: p.ReadyReceiptClaims{Version: 2, CertificateDigest: cd, Claim: p.ClaimReference{ClaimID: c.reference.ClaimID, CreateRevision: c.reference.CreateRevision, LeaseID: c.reference.LeaseID}, DataGateEpoch: c.control.DataGateEpoch, GateState: "open", ObservedAt: now, ValidUntil: now.Add(5 * time.Second)}, EnrollmentDigest: receipt.Claims.EnrollmentDigest, ObserverCertificateDigest: observer.Digest(), ActivationDigest: ad})
	require.NoError(t, err)
	return proof, enrollment
}

func duePublicationTestKey(f duePublicationFixture) string {
	c := f.claim.control
	return fmt.Sprintf("%sp/%02x/due/%s/%s/%019d/%s", f.b.namespace.Root(), f.claim.reference.Partition, c.ExpiresAt.UTC().Format("20060102T150405.000000000Z"), c.SandboxID, c.Generation, c.IntentID)
}

func TestDuePublicationAtomic(t *testing.T) {
	f := newDuePublicationFixture(t)
	result, err := f.b.PublishRuntimeEnrolled(context.Background(), f.claim, f.proof, f.enrollment)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.Equal(t, duePublicationStageForTest, result.Reference.StageID)
	require.Equal(t, uint32(3), result.Entry.Record.Version)
	controlKey, _, err := f.b.namespace.sandboxKeys(f.claim.reference.Partition, f.claim.reference.SandboxID, "read")
	require.NoError(t, err)
	control, candidate := f.server.point(controlKey), f.server.point(duePublicationTestKey(f))
	require.NotNil(t, candidate, "the actual publication must write the discovery candidate")
	require.Equal(t, control.ModRevision, candidate.CreateRevision)
	require.Equal(t, candidate.CreateRevision, candidate.ModRevision)
	require.Zero(t, candidate.Lease)
	require.LessOrEqual(t, len(candidate.Value), 1024)
	var wire struct {
		Version       uint32    `json:"version"`
		Partition     uint8     `json:"partition"`
		SandboxID     string    `json:"sandbox_id"`
		WorkspaceHash string    `json:"workspace_hash"`
		RestoreEpoch  string    `json:"restore_epoch"`
		Generation    int64     `json:"generation"`
		IntentID      string    `json:"intent_id"`
		ExpiresAt     time.Time `json:"expires_at"`
	}
	require.NoError(t, json.Unmarshal(candidate.Value, &wire))
	require.Equal(t, uint32(1), wire.Version)
	require.Equal(t, f.claim.reference.Partition, wire.Partition)
	require.Equal(t, f.claim.control.SandboxID, wire.SandboxID)
	require.Equal(t, f.claim.control.WorkspaceHash, wire.WorkspaceHash)
	require.Equal(t, f.claim.control.RestoreEpoch, wire.RestoreEpoch)
	require.Equal(t, f.claim.control.Generation, wire.Generation)
	require.Equal(t, f.claim.control.IntentID, wire.IntentID)
	require.Equal(t, f.claim.control.ExpiresAt, wire.ExpiresAt)
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	var committed *pb.TxnRequest
	for _, request := range f.server.requests {
		for _, op := range request.Success {
			if put := op.GetRequestPut(); put != nil && string(put.Key) == duePublicationTestKey(f) {
				committed = request
			}
		}
	}
	require.NotNil(t, committed)
	require.Len(t, committed.Compare, 54, "all original authority comparisons survive")
	require.Len(t, committed.Success, 10)
	require.Empty(t, committed.Failure, "v3 alone spends the redundant compact Else on the index")
	require.Equal(t, 64, len(committed.Compare)+len(committed.Success)+len(committed.Failure))
	require.LessOrEqual(t, committed.Size(), 262144)
}

func TestDuePublicationFailureAtomic(t *testing.T) {
	f := newDuePublicationFixture(t)
	controlKey, _, err := f.b.namespace.sandboxKeys(f.claim.reference.Partition, f.claim.reference.SandboxID, "read")
	require.NoError(t, err)
	before := f.server.point(controlKey)
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(controlKey), before: func() { f.server.put(controlKey, string(before.Value), 0) }}
	result, err := f.b.PublishRuntimeEnrolled(context.Background(), f.claim, f.proof, f.enrollment)
	require.NoError(t, err, "original live guard without a receipt remains unknown after read-bearing arbitration")
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.Equal(t, duePublicationStageForTest, result.Reference.StageID)
	require.Nil(t, f.server.point(duePublicationTestKey(f)))
	current := f.server.point(controlKey)
	require.Equal(t, before.Value, current.Value, "failed publication cannot activate control")
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	var failed, arbitration bool
	for _, request := range f.server.requests {
		if len(request.Compare) == 54 {
			failed = true
			require.Empty(t, request.Failure)
		}
		if failed && len(request.Compare) == 4 && len(request.Success) == 4 && len(request.Failure) == 4 {
			arbitration = true
		}
	}
	require.True(t, failed)
	require.True(t, arbitration, "empty v3 failure must reach current-base read-bearing arbitration")
}

func TestDuePublicationLostReplyReplay(t *testing.T) {
	f := newDuePublicationFixture(t)
	controlKey, _, err := f.b.namespace.sandboxKeys(f.claim.reference.Partition, f.claim.reference.SandboxID, "read")
	require.NoError(t, err)
	original := f.b.client.KV
	f.b.client.KV = &faultKV{KV: original, match: putsKey(controlKey), after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, response.Succeeded)
		return nil, context.DeadlineExceeded
	}}
	first, err := f.b.PublishRuntimeEnrolled(context.Background(), f.claim, f.proof, f.enrollment)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, first.Outcome)
	require.Equal(t, duePublicationStageForTest, first.Reference.StageID)
	candidate := f.server.point(duePublicationTestKey(f))
	require.NotNil(t, candidate)
	grants, _, _ := f.server.counts()
	f.b.client.KV = original
	replay, err := f.b.PublishRuntimeEnrolled(context.Background(), f.claim, f.proof, f.enrollment)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, replay.Outcome)
	require.Equal(t, first.Reference, replay.Reference)
	require.Equal(t, PublishReplay, replay.Disposition)
	require.Equal(t, candidate, f.server.point(duePublicationTestKey(f)))
	after, _, _ := f.server.counts()
	require.Equal(t, grants, after, "replay must not allocate or reapply")
}

func TestDuePublicationVersionPairs(t *testing.T) {
	f := newDuePublicationFixture(t)
	result, err := f.b.PublishRuntimeEnrolled(context.Background(), f.claim, f.proof, f.enrollment)
	require.NoError(t, err)
	r := result.Entry.Record
	for _, pair := range []struct {
		version uint32
		stage   string
	}{{2, "runtime_publish_enrolled_v2"}, {3, duePublicationStageForTest}} {
		t.Run(pair.stage, func(t *testing.T) {
			copy := r
			copy.Version, copy.Attempt.StageID = pair.version, pair.stage
			wire, err := encodeEnrolledRecord(copy)
			require.NoError(t, err)
			var decoded RuntimeEnrolledPublicationRecord
			require.NoError(t, decodeEnrolledRecord([]byte(wire), &decoded))
			require.Equal(t, copy, decoded)
			copy.Version = 5 - pair.version
			require.Error(t, copy.Validate(), "accepted stage/version cannot silently change meaning")
		})
	}
	for _, stage := range []string{"runtime_publish_enrolled_v2", duePublicationStageForTest} {
		ref := result.Reference
		ref.StageID = stage
		_, receipt, err := f.b.stageKeys(ref)
		require.NoError(t, err)
		wire, err := encodeReceipt(ref, OutcomeCommitted)
		require.NoError(t, err)
		if stage == "runtime_publish_enrolled_v2" {
			f.server.put(receipt, wire, 0)
		}
		outcome, err := f.b.ReadRuntimeEnrolledPublicationAttempt(context.Background(), ref)
		require.NoError(t, err)
		require.Equal(t, OutcomeCommitted, outcome)
	}
}

func TestDuePublicationV3Maximum(t *testing.T) {
	n, err := NewNamespace("/"+strings.Repeat("p", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
	require.NoError(t, err)
	b := &Backend{namespace: n, restoreEpoch: strings.Repeat("e", 128), identityValue: strings.Repeat("i", 2048)}
	b.identityKey, _ = n.Key("meta", "identity")
	b.restoreKey, _ = n.Key("meta", "restore_epoch")
	l := StageAttemptLocator{Namespace: n.Root(), RequestID: strings.Repeat("r", 128), StageID: duePublicationStageForTest, AttemptID: "11111111-1111-4111-8111-111111111111", RestoreEpoch: b.restoreEpoch}
	m := Mutation{}
	for i := 0; i < 46; i++ {
		m.Comparisons = append(m.Comparisons, clientv3.Compare(clientv3.CreateRevision(n.Root()+fmt.Sprintf("cmp-%d", i)), "=", math.MaxInt64))
	}
	for i := 0; i < 9; i++ {
		m.Writes = append(m.Writes, Write{Key: n.Root() + fmt.Sprintf("write-%d", i), Value: bytes.Repeat([]byte(`\u003c`), 1000)})
	}
	require.NoError(t, b.preflightEnrolledMaximum(l.reference(strings.Repeat("a", 64)), m))
	m.Comparisons = m.Comparisons[1:]
	require.Error(t, b.preflightEnrolledMaximum(l.reference(strings.Repeat("a", 64)), m))
}

func TestDuePublicationQuotaReturnVersions(t *testing.T) {
	for _, stage := range []string{"runtime_publish_enrolled_v2", duePublicationStageForTest} {
		r := quotaReturnRecordFixture(t)
		r.Publication.StageID = stage
		wire, err := encodeColdRuntimeQuotaReturn(r)
		require.NoError(t, err)
		require.Contains(t, wire, stage)
	}
	r := quotaReturnRecordFixture(t)
	r.Publication.StageID = "runtime_publish_enrolled_v4"
	_, err := encodeColdRuntimeQuotaReturn(r)
	require.Error(t, err)
}
