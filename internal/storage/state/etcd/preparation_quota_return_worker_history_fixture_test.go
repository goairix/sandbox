package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This host fixture uses the actual public signed publication and the existing
// signed-release model. The settled reservation is explicit canonical metadata;
// neither it nor the test-issued terminal signature asserts physical cleanup.
type coldQuotaHistoryFixture struct {
	duePublicationFixture
	workspace   WorkspaceIdentity
	task        TaskReference
	termination p.TaskRuntimeTerminationIntent
	keys        p.TaskRuntimeTerminationKeys
	releaseKey  string
	historyKeys []string
	owner       *runtimeEnrolledPublicationOwner
	session     *WorkerSession
	part        *WorkerPartitionClaim
	ranges      *WorkerBudgetShare
	claims      *WorkerBudgetShare
}

func newColdQuotaHistoryFixture(t *testing.T) *coldQuotaHistoryFixture {
	t.Helper()
	f := &coldQuotaHistoryFixture{duePublicationFixture: newDuePublicationFixture(t)}
	b, ctx := f.b, context.Background()
	f.workspace = f.claim.workspace
	publication, err := b.PublishRuntimeEnrolled(ctx, f.claim, f.proof, f.enrollment)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, publication.Outcome)
	r := publication.Entry.Record

	// Only the cold profile's codec constraints matter to this history reader.
	// Real reserve/settle mutation admission has its independent test coverage.
	b.preparationInventory = &PreparationInventoryOptions{}
	b.preparationInventory.QuotaSlots[7] = 1
	b.preparationDigest, b.preparationTotal = strings.Repeat("b", 64), 33
	b.localRuntimeRelease = &LocalRuntimeReleaseOptions{MaxActive: 1, Timeout: time.Second, Provider: &k.TaskRuntimeCleanupClient{}}
	settlement := r.Attempt
	settlement.StageID, settlement.AttemptID = "cold_preparation_settle", uuid.NewString()
	reservation := ColdPreparationRecord{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Owner: coldClaimOwner(f.claim), Quota: ColdSlotReference{Kind: "quota", Partition: 7, Epoch: 1}, Preparation: ColdSlotReference{Kind: "preparation", Epoch: 1}, Phase: "settled", Settlement: settlement}
	reservedKey, workKey, err := b.namespace.coldPreparationKeys(f.workspace.Partition(), r.IntentID)
	require.NoError(t, err)
	reservedWire, err := coldEncode(reservation)
	require.NoError(t, err)
	workWire, err := coldEncode(coldPreparationWork{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, IntentID: r.IntentID, Phase: "settled"})
	require.NoError(t, err)
	receiptKey, err := b.runtimeDispatchReceiptKey(settlement)
	require.NoError(t, err)
	receiptWire, err := encodeReceipt(settlement.reference(strings.Repeat("c", 64)), OutcomeCommitted)
	require.NoError(t, err)
	_, err = b.client.Txn(ctx).Then(clientv3.OpPut(reservedKey, reservedWire), clientv3.OpPut(workKey, workWire), clientv3.OpPut(receiptKey, receiptWire)).Commit()
	require.NoError(t, err)

	f.seedSignedRelease(t, r)
	f.owner, err = b.registerEnrolledPublication(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		select {
		case <-f.owner.done:
		default:
			f.owner.finish()
		}
	})
	f.session, err = b.OpenWorkerSession(ctx, "quota-history")
	require.NoError(t, err)
	f.part, err = b.ClaimWorkerPartition(ctx, f.session, f.task.Partition)
	require.NoError(t, err)
	f.ranges, err = b.AcquireWorkerBudget(ctx, f.session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	f.claims, err = b.AcquireWorkerBudget(ctx, f.session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	f.fund()
	return f
}

// The signing pattern is the portable domain_released_proof_test model. Here
// the Task and observer are bound to the actual published enrollment, so the
// final quota origin join also executes its production cryptographic checks.
func (f *coldQuotaHistoryFixture) seedSignedRelease(t *testing.T, pub RuntimeEnrolledPublicationRecord) {
	t.Helper()
	b := f.b
	r := localReleaseTerminationFixture(t)
	r.Current.Namespace, r.Current.RestoreEpoch = b.namespace.Root(), b.restoreEpoch
	r.Current.AuthorityID, r.Current.Target = b.publicationAuthorityID, b.publicationTarget
	r.Current.TaskID, r.Current.SandboxID = uuid.NewString(), pub.SandboxID
	r.Current.WorkspaceHash, r.Current.Generation = pub.WorkspaceHash, pub.Generation
	r.Current.DataGateEpoch, r.Current.ControlRevision = pub.DataGateEpoch, f.server.point(publicationControlKey(t, f)).ModRevision
	r.Current.Runtime = p.RuntimeReference(pub.Runtime)
	r.Partition, r.CreationIntentID = pub.Attempt.Partition, pub.IntentID
	r.NodeUID, r.NodeBootID = pub.NodeUID, pub.NodeBootID
	r.EnrollmentDigest, r.ObserverCertificateDigest = pub.EnrollmentDigest, pub.ObserverCertificateDigest
	f.task = localRuntimeTerminationTask(r)
	task := TaskRecord{Version: 1, Reference: f.task, Kind: "cleanup", WorkspaceHash: pub.WorkspaceHash, CreationIntentID: pub.IntentID, Generation: pub.Generation, DataGateEpoch: pub.DataGateEpoch, ControlRevision: r.Current.ControlRevision, Runtime: pub.Runtime, Snapshot: pub.Snapshot, ExpiresAt: pub.ExpiresAt}
	taskWire, err := encodeTaskRecord(task)
	require.NoError(t, err)
	r.Current.TaskDigest = workspaceSyncIntentDigest(taskWire)
	taskKey, err := b.namespace.taskKey(f.task)
	require.NoError(t, err)
	f.server.put(taskKey, taskWire, 0)
	r.TaskBirth = f.server.point(taskKey).CreateRevision
	f.server.put(b.namespace.Root()+"quota-history-model-claim", "claim", 0)
	r.Current.ClaimCreateRevision = f.server.point(b.namespace.Root() + "quota-history-model-claim").CreateRevision

	// Task close context requires the actual claim birth to follow the current
	// control revision. Derive protocol keys only after both births are known.
	keys, err := p.TaskRuntimeTerminationKeysFor(r)
	require.NoError(t, err)
	f.keys = keys
	claim := taskCloseClaimWire{Task: f.task, ClaimID: r.Current.ClaimID, WorkerID: r.Current.WorkerID, CreateRevision: r.Current.ClaimCreateRevision, LeaseID: r.Current.LeaseID}
	digest := strings.Repeat("d", 64)
	complete := taskWorkspaceSyncLocalComplete{Version: 1, Domain: "sandbox.workspace-sync-local-complete.v1", Task: f.task, TaskBirth: r.TaskBirth, Binding: digest, Claim: claim, ProgressDigest: digest, EvidenceDigest: digest, CheckpointRevision: r.TaskBirth, CheckpointDigest: digest, RootDigest: digest}
	completeWire, err := encodeOperationMetadata(complete, 8192)
	require.NoError(t, err)
	f.server.put(keys.LocalComplete, completeWire, 0)
	for _, key := range []string{keys.Quiescence, keys.QuiescenceReceipt, keys.QuiescenceReservation, keys.FinalSync, keys.Issuer, keys.Archive} {
		f.server.put(key, `{"model":"signed-stamp-only"}`, 0)
	}
	for _, binding := range []struct {
		key   string
		stamp *p.RecordStamp
	}{{keys.Task, &r.Authority.Task}, {keys.LocalComplete, &r.Authority.LocalComplete}, {keys.Quiescence, &r.Authority.Quiescence}, {keys.QuiescenceReceipt, &r.Authority.QuiescenceReceipt}, {keys.QuiescenceReservation, &r.Authority.QuiescenceReservation}, {keys.FinalSync, &r.Authority.FinalSync}, {keys.Issuer, &r.Authority.Issuer}, {keys.Archive, &r.Authority.Archive}} {
		*binding.stamp = localRuntimeStamp(f.server.point(binding.key))
	}
	reserved, err := p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	f.server.put(keys.Intent, string(reserved), 0)
	intent := f.server.point(keys.Intent)
	observerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{6}, 32))
	observer, err := b.enrolledVerifier.VerifyNodeObserverCertificate(f.enrollment.ObserverCertificate, pub.NodeUID, pub.NodeBootID, f.now)
	require.NoError(t, err)
	proof, err := p.SignNodeTerminationProof(observerKey, observer, p.NodeTerminationProofClaims{Version: 1, Purpose: "task_runtime_terminated", Current: r.Current, Partition: r.Partition, IntentCreateRevision: intent.CreateRevision, IntentDigest: workspaceSyncIntentDigest(string(reserved)), EnrollmentDigest: r.EnrollmentDigest, ObserverCertificateDigest: observer.Digest(), TicketDigest: digest, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID, PodUID: pub.Runtime.UID, PodResourceVersion: "model-rv", ProviderDeletingAt: f.now.Add(-time.Second), PID1Dead: true, CgroupEmpty: true, ObservedAt: f.now})
	require.NoError(t, err)
	provider := taskRuntimeProviderCleanupRecord{Version: 1, Task: f.task, TaskBirth: r.TaskBirth, Binding: localRuntimeProviderBinding(r, b.enrolledPublication.ContractDigest), Descriptor: []byte(`{"model":"cleanup"}`)}
	for i, step := range taskRuntimeCleanupSteps {
		provider.Slots[i] = taskRuntimeCleanupSlot{Step: step, State: "skipped"}
		if i == 0 || i == 1 || i == 6 {
			provider.Slots[i] = taskRuntimeCleanupSlot{Step: step, State: "complete", Kind: "Pod", Name: "model-pod", UID: pub.Runtime.UID, ResourceVersion: "model-rv", EffectID: r.Current.CommandID, Claim: &claim}
		}
	}
	providerWire, err := encodeTaskRuntimeProviderCleanup(provider)
	require.NoError(t, err)
	_, providerKey, releaseKey, err := b.namespace.localRuntimeReleaseKeys(f.task)
	require.NoError(t, err)
	f.server.put(providerKey, providerWire, 0)
	r.Phase, r.TerminalProof, r.CleanupDigest = "cleanup-complete", proof, workspaceSyncIntentDigest(providerWire)
	terminal, err := p.EncodeTaskRuntimeTerminationIntent(r)
	require.NoError(t, err)
	f.server.put(keys.Intent, string(terminal), 0)
	intent = f.server.point(keys.Intent)
	release, err := newLocalOwnerReleaseRecord(r, intent.CreateRevision, intent.ModRevision, claim)
	require.NoError(t, err)
	releaseWire, err := encodeLocalOwnerRelease(release)
	require.NoError(t, err)
	f.server.put(releaseKey, releaseWire, 0)
	f.releaseKey, f.termination = releaseKey, r
	f.historyKeys, err = b.localRuntimeHistoryKeys(r, f.task)
	require.NoError(t, err)
}

func publicationControlKey(t *testing.T, f *coldQuotaHistoryFixture) string {
	t.Helper()
	key, _, err := f.b.namespace.sandboxKeys(f.claim.reference.Partition, f.claim.reference.SandboxID, "read")
	require.NoError(t, err)
	return key
}

func (f *coldQuotaHistoryFixture) fund() {
	f.b.archiveLife.mu.Lock()
	defer f.b.archiveLife.mu.Unlock()
	f.ranges.bucket.last, f.ranges.bucket.tokens = time.Now().Add(time.Hour), 262144
	f.claims.bucket.last, f.claims.bucket.tokens = time.Now().Add(time.Hour), 1
}

// This independent public-loader baseline must pass before cursor RED is run.
// It prevents a broken or unsigned fixture from masquerading as feature failure.
func TestColdQuotaWorkerHistorySignedFixturePublicBaseline(t *testing.T) {
	f := newColdQuotaHistoryFixture(t)
	history, err := f.b.readColdQuotaReturnHistory(f.owner.context, f.workspace, f.claim.reference.IntentID, f.task)
	require.NoError(t, err)
	require.NotNil(t, history)
	require.Equal(t, f.task, history.release.record.Task)
	require.Equal(t, f.termination, history.release.termination)
	require.True(t, coldQuotaReturnOriginMatches(history.reservation, history.publication.entry.Record, history.release))
	for _, kv := range []*mvccpb.KeyValue{history.publication.journal, history.publication.receipt, history.reservedKV, history.workKV, history.settledKV, history.release.kv} {
		require.NotNil(t, kv)
	}
}
