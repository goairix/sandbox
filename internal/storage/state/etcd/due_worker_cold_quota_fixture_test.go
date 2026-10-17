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
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This extends the closed signed-history model only at Task creation: the
// cleanup birth cohort is committed together before signing its exact stamps.
// It issues no physical capability and supplies no accepting verifier stub.
func (f *coldQuotaHistoryFixture) seedSignedCleanupColdQuotaBirth(t *testing.T, pub RuntimeEnrolledPublicationRecord) cleanupBirthTestSeed {
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
	// T/I/link/receipt/P share the real transaction birth. The same signed
	// terminal history below authenticates this exact Task and birth.
	attempt := pub.Attempt
	attempt.StageID, attempt.RequestID, attempt.AttemptID = "destroy", "cold-cleanup-birth", uuid.NewString()
	seed := cleanupBirthTestSeed{task: task, intent: CleanupIntentRecord{Version: 1, Task: task, Attempt: attempt}, stage: attempt.reference(strings.Repeat("d", 64))}
	cohort, _, err := b.cleanupBirthCohortKeys(f.task, attempt)
	require.NoError(t, err)
	copy(seed.keys[:], cohort)
	seed.wires[0] = taskWire
	seed.wires[1], err = encodeCleanupIntentRecord(seed.intent)
	require.NoError(t, err)
	seed.wires[2], err = encodeTaskLinkRecord(TaskLinkRecord{Version: 1, Reference: f.task})
	require.NoError(t, err)
	seed.wires[3], err = encodeReceipt(seed.stage, OutcomeCommitted)
	require.NoError(t, err)
	pending, err := encodeCleanupPending(cleanupPendingRecord{Version: 1, Reference: f.task, TaskDigest: workerIdentityDigest(taskWire)})
	require.NoError(t, err)
	ops := make([]clientv3.Op, 0, 5)
	for i, wire := range seed.wires {
		ops = append(ops, clientv3.OpPut(seed.keys[i], wire))
	}
	ops = append(ops, clientv3.OpPut(seed.keys[5], pending))
	response, err := b.client.Txn(context.Background()).Then(ops...).Commit()
	require.NoError(t, err)
	seed.birth = response.Header.Revision
	r.TaskBirth = f.server.point(taskKey).CreateRevision
	f.server.put(b.namespace.Root()+"quota-cleanup-model-claim-"+f.task.TaskID, "claim", 0)
	r.Current.ClaimCreateRevision = f.server.point(b.namespace.Root() + "quota-cleanup-model-claim-" + f.task.TaskID).CreateRevision

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
	return seed
}

// Configure the real 60-second profile before New; existing legacy fixtures
// retain their original defaults. The descriptor comes from actual acquisition.
func newCleanupColdPublicationFixture(t *testing.T) duePublicationFixture {
	t.Helper()
	server, o := workerTestEndpoint(t)
	o.WorkerControl.SessionTTL = 60 * time.Second
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	now := time.Now().UTC()
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "target", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
	o.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) { return ClockObservation{UTC: now}, nil })
	o.EnrolledPublication = &EnrolledPublicationOptions{ContractDigest: strings.Repeat("b", 64), MaxActive: 4}
	b, err := New(context.Background(), o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	w, err := NewWorkspaceIdentity("local", o.Identity.StorageID, "bucket", uuid.NewString()+"/")
	require.NoError(t, err)
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: w})
	require.NoError(t, err)
	in := AcquireIntentInput{Workspace: w, Principal: "due-test", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), LocalCleanupPlan: &plan, Payload: json.RawMessage(`{"html":"<>&中>"}`), Now: now, TTL: time.Hour}
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

func newCleanupColdQuotaHistoryFixture(t *testing.T) (*coldQuotaHistoryFixture, cleanupBirthTestSeed) {
	t.Helper()
	f := &coldQuotaHistoryFixture{duePublicationFixture: newCleanupColdPublicationFixture(t)}
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

	seed := f.seedSignedCleanupColdQuotaBirth(t, r)
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
	return f, seed
}
