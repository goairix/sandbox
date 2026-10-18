package etcd

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This fixture contains canonical metadata only. The backend has no client,
// signer, transport, archive store or KernelBoundary. In particular, the empty
// sealed Enrollment field is not replaced by a counterfeit runtime permission.
func cleanupLocalWireFixture(t *testing.T, maximum ...bool) (*Backend, cleanupLocalWireInput) {
	t.Helper()
	n, err := NewNamespace("/codex-test", "tasks", "cell")
	require.NoError(t, err)
	epoch, authorityID, target, workerID := "epoch", "authority", "target", "cleanup"
	maximal := len(maximum) != 0 && maximum[0]
	if maximal {
		n, err = NewNamespace("/"+strings.Repeat("p", 252), strings.Repeat("s", 128), strings.Repeat("c", 128))
		require.NoError(t, err)
		require.Len(t, n.Root(), 512)
		epoch, authorityID, target, workerID = strings.Repeat("e", 128), strings.Repeat("a", 128), strings.Repeat("t", 128), strings.Repeat("w", 128)
	}
	snapshot := localCleanupTestRecord(t)
	if maximal {
		snapshot.SandboxID = strings.Repeat("s", 128)
		snapshot.Snapshot.Version = strings.Repeat("v", 128)
	}
	workspace, err := snapshot.LocalCleanup.workspace()
	require.NoError(t, err)
	plan, err := NewTaskWorkspaceSyncPlan(TaskWorkspaceSyncPlanOptions{Workspace: workspace, Exclude: snapshot.LocalCleanup.Exclude, DeleteExtraneous: true})
	require.NoError(t, err)
	snapshotWire, err := encodeDomainRecord(snapshot)
	require.NoError(t, err)
	var snapshotCopy SandboxSnapshotRecord
	require.NoError(t, decodeDomainRecord(&mvccpb.KeyValue{Value: []byte(snapshotWire)}, &snapshotCopy))

	task, intent, link, checkpoint := taskTestRecords()
	task.Reference.Namespace, task.Reference.RestoreEpoch, task.Reference.SandboxID = n.Root(), epoch, snapshot.SandboxID
	task.Reference.Partition = workspace.Partition()
	task.WorkspaceHash, task.Snapshot = workspace.Hash(), snapshot.Snapshot
	task.CreationIntentID = "11111111-1111-4111-8111-111111111111"
	task.Runtime = RuntimeReference{ID: "pod", UID: "22222222-2222-4222-8222-222222222222", BootID: "33333333-3333-4333-8333-333333333333"}
	if maximal {
		task.Runtime.ID = strings.Repeat("p", 63) + "." + strings.Repeat("r", 62) + ".x"
	}
	intent.Task = task
	intent.Attempt.Namespace, intent.Attempt.RestoreEpoch = n.Root(), epoch
	intent.Attempt.Partition = task.Reference.Partition
	if maximal {
		intent.Attempt.RequestID = strings.Repeat("r", 128)
	}
	link.Reference, checkpoint.Reference, checkpoint.Attempt = task.Reference, task.Reference, intent.Attempt
	taskWire, err := encodeTaskRecord(task)
	require.NoError(t, err)
	intentWire, err := encodeCleanupIntentRecord(intent)
	require.NoError(t, err)
	linkWire, err := encodeTaskLinkRecord(link)
	require.NoError(t, err)
	checkpointWire, err := encodeTaskCheckpointRecord(checkpoint)
	require.NoError(t, err)

	identity := Identity{SchemaVersion: 1, Prefix: n.prefix, AuthorityID: n.scope, Cell: n.cell, ClusterID: 1, StorageID: "test-storage", RuntimeID: authorityID, RestoreEpoch: epoch}
	identityWire, err := json.Marshal(identity)
	require.NoError(t, err)
	_, err = decodeIdentity(string(identityWire), n)
	require.NoError(t, err)
	budget := TaskWorkspaceSyncBudgetOptions{BudgetID: "99999999-9999-4999-8999-999999999999", SlotsPerPartition: 64, MaxIntentBytes: 16384}
	budgetWire, err := encodeOperationMetadata(taskWorkspaceSyncRecyclingBudgetRecord{Version: 2, TaskWorkspaceSyncBudgetOptions: budget}, 4096)
	require.NoError(t, err)
	b := &Backend{namespace: n, restoreEpoch: epoch, identityValue: string(identityWire), clusterID: 1, requestTimeout: time.Second, publicationAuthorityID: authorityID, publicationTarget: target, enrolledPublication: &EnrolledPublicationOptions{ContractDigest: strings.Repeat("a", 64), MaxActive: 32}}
	b.identityKey, err = n.Key("meta", "identity")
	require.NoError(t, err)
	b.restoreKey, err = n.Key("meta", "restore_epoch")
	require.NoError(t, err)
	budgetKey, err := n.Key("meta", "workspace_sync_recycling_budget")
	require.NoError(t, err)
	b.workspaceSyncBudget = &taskWorkspaceSyncBudgetPin{profile: 2, options: budget, key: budgetKey, value: budgetWire, create: 1, mod: 1}

	taskKey, err := n.taskKey(task.Reference)
	require.NoError(t, err)
	intentKey, err := n.cleanupIntentKey(task.Reference)
	require.NoError(t, err)
	linkKey, err := n.taskLinkKey(task.Reference)
	require.NoError(t, err)
	placementKey, err := n.placementKey(task.Reference.SandboxID)
	require.NoError(t, err)
	controlKey, _, err := n.sandboxKeys(task.Reference.Partition, task.Reference.SandboxID, "read")
	require.NoError(t, err)
	workspaceKey := n.Root() + fmt.Sprintf("p/%02x/workspaces/%s/", task.Reference.Partition, task.WorkspaceHash)
	indexKey, err := n.runtimeIndexKey(task.Runtime.UID)
	require.NoError(t, err)
	keys := []string{taskKey, intentKey, linkKey, placementKey, controlKey, workspaceKey + "owner", workspaceKey + "fence", indexKey}
	values := []string{taskWire, intentWire, linkWire}
	for _, record := range []interface{ Validate() error }{
		SandboxPlacementRecord{Version: 1, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, IntentID: task.CreationIntentID, RestoreEpoch: epoch, Partition: task.Reference.Partition, Generation: task.Generation},
		SandboxControlRecord{Version: 1, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, IntentID: task.CreationIntentID, RestoreEpoch: epoch, Generation: task.Generation, DataGateEpoch: task.DataGateEpoch, Phase: PhaseDestroying, Snapshot: task.Snapshot, Runtime: &task.Runtime, ExpiresAt: task.ExpiresAt},
		WorkspaceOwnerRecord{Version: 1, WorkspaceHash: task.WorkspaceHash, SandboxID: task.Reference.SandboxID, IntentID: task.CreationIntentID, RestoreEpoch: epoch, Generation: task.Generation, Runtime: &task.Runtime},
		WorkspaceFenceRecord{Version: 1, WorkspaceHash: task.WorkspaceHash, RestoreEpoch: epoch, Generation: task.Generation},
		RuntimeIndexRecord{Version: 1, IntentID: task.CreationIntentID, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, RestoreEpoch: epoch, Generation: task.Generation, Runtime: task.Runtime},
	} {
		wire, err := encodeDomainRecord(record)
		require.NoError(t, err)
		values = append(values, wire)
	}
	checkpointKey, err := n.taskCheckpointKey(task.Reference)
	require.NoError(t, err)
	bundle := &taskClaimBundle{task: task, birth: 11, initial: intent.Attempt, checkpoint: &mvccpb.KeyValue{Key: []byte(checkpointKey), Value: []byte(checkpointWire), Version: 1, CreateRevision: 11, ModRevision: 11}}
	for i, key := range keys {
		created, modified := int64(3), int64(3)
		if i < 3 {
			created, modified = 11, 11
		} else if i == 4 {
			modified = 11
		}
		bundle.fences = append(bundle.fences, taskFence{key: key, value: values[i], create: created, mod: modified})
	}

	// These are original record bytes and legal revisions for wire construction,
	// not evidence that an etcd transaction or a physical run actually happened.
	r := cleanupSlotSchemaRecord()
	r.Task, r.InitialAttempt = task.Reference, intent.Attempt
	r.TaskDigest = workspaceSyncIntentDigest(taskWire)
	r.Slot = cleanupCandidate(r.TaskDigest, r.Selection)
	r.Incarnation = cleanupSlotTestIncarnation(r)
	r.Phase, r.Confirmed = "occupied", "reserved"
	r.Run = &cleanupExecutionRun{ID: "44444444-4444-4444-8444-444444444444", Worker: launcher.WorkspaceSyncWorkerReference{Incarnation: "55555555-5555-4555-8555-555555555555", BootID: task.Runtime.BootID, PID: 7, StartTicks: 1, UID: 1, GID: 1, PIDNamespaceDev: 1, PIDNamespaceIno: 2, MountNamespaceDev: 1, MountNamespaceIno: 3, ExecutableDev: 1, ExecutableIno: 4}}
	r.Pending = &cleanupExecutionPending{Purpose: "claim", ClaimID: taskClaimTestID}
	executionWire, err := encodeCleanupExecution(r)
	require.NoError(t, err)
	slotWire, err := encodeCleanupExecutionSlot(cleanupSlotRecordFromExecution(r))
	require.NoError(t, err)
	executionKey, err := n.cleanupExecutionKey(task.Reference)
	require.NoError(t, err)
	slotKey, err := n.cleanupExecutionSlotKey(r.Slot)
	require.NoError(t, err)
	state := &cleanupSlotState{record: r, execution: taskFence{key: executionKey, value: executionWire, create: 12, mod: 14}, slot: &taskFence{key: slotKey, value: slotWire, create: 13, mod: 13}}

	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC)
	certificate, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: "66666666-6666-4666-8666-666666666666", Role: "command_issuer", Namespace: n.Root(), AuthorityID: authorityID, Target: target, RestoreEpoch: epoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: now, NotAfter: now.Add(time.Hour)})
	require.NoError(t, err)
	certificateDigest, err := snapshotDigest(certificate)
	require.NoError(t, err)
	issuer := CommandIssuerEntry{Record: CommandIssuerRecord{Version: 1, Namespace: n.Root(), RestoreEpoch: epoch, CertificateID: "66666666-6666-4666-8666-666666666666", CertificateDigest: certificateDigest, Certificate: certificate}, Revision: 2}
	_, err = encodeCommandIssuerRecord(issuer.Record)
	require.NoError(t, err)

	attempt := intent.Attempt
	attempt.StageID = enrolledPublicationStage
	digest := strings.Repeat("a", 64)
	publication := RuntimeEnrolledPublicationRecord{RuntimePublicationRecord: RuntimePublicationRecord{Version: 2, IntentID: task.CreationIntentID, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, RestoreEpoch: epoch, Generation: task.Generation, DataGateEpoch: task.DataGateEpoch, Snapshot: task.Snapshot, ExpiresAt: task.ExpiresAt, OperationID: task.CreationIntentID, PayloadDigest: digest, ProofDigest: digest, CertificateDigest: digest, Runtime: task.Runtime, Claim: DispatchClaimReference{ClaimID: taskClaimTestID, WorkerID: "publisher", CreateRevision: 3, LeaseID: 4}, Attempt: attempt}, EnrollmentDigest: digest, ObserverCertificateDigest: digest, ActivationDigest: digest, EnrollmentRecordDigest: digest, ActivationRecordDigest: digest, NodeUID: "77777777-7777-4777-8777-777777777777", NodeBootID: "88888888-8888-4888-8888-888888888888"}
	publication.DispatchAttempt, publication.BindingAttempt, publication.MountAttemptLocator = attempt, attempt, attempt
	publication.DispatchAttempt.StageID = "runtime_dispatch"
	publication.BindingAttempt.StageID = "runtime_bind"
	publication.MountAttemptLocator.StageID = "runtime_mount"
	publicationWire, err := encodeEnrolledRecord(publication)
	require.NoError(t, err)
	var publicationCopy RuntimeEnrolledPublicationRecord
	require.NoError(t, decodeEnrolledRecord([]byte(publicationWire), &publicationCopy))
	input := cleanupLocalWireInput{bundle: bundle, state: state, snapshot: snapshotCopy, publication: &RuntimeEnrolledPublicationEntry{Record: publicationCopy, Reference: attempt.reference(digest)}, issuer: issuer, plan: plan, workerID: workerID, claimTTL: 120 * time.Second}
	return b, input
}

// The manifest is an independent test contract, not the product's own list.
// History subreads have exact names: a present group cannot hide an omitted
// native request, and arbitrary dotted suffixes are not accepted.
func cleanupLocalWireExpected() map[string][]string {
	return map[string][]string{
		"stage-begin":       {"generic-begin", "stage-grant", "stage-revoke"},
		"stage-commit":      {"close-commit", "checkpoint-commit"},
		"stage-resolve":     {"close-resolve", "checkpoint-resolve", "quiescence-resolve"},
		"quiescence-stage":  {"reserved-begin", "reserved-commit"},
		"task-checkpoint":   {"claim-create", "claim-post-fence", "checkpoint-mutation"},
		"task-renew":        {"renew-before", "renew-after", "task-grant", "task-keepalive"},
		"task-release":      {"task-revoke"},
		"close":             {"issuer-read", "issuer-register", "close-pre-guard", "close-committed-guard", "close-intent-read", "close-stage-receipt-read"},
		"close-query":       {"query-close-history", "query-close-stage-receipt", "query-close-issuer"},
		"quiescence":        {"reservation-read", "reservation-write", "quiescence-pre-guard", "quiescence-committed-guard", "quiescence-intent-read.initial", "quiescence-intent-read.prior-initial", "quiescence-intent-read.prior-complete", "quiescence-receipt-read"},
		"quiescence-query":  {"query-quiescence-history.initial", "query-quiescence-history.prior-initial", "query-quiescence-history.prior-complete", "query-quiescence-stage-receipt", "query-quiescence-issuer"},
		"dual-drain":        {"operations-page", "empty-before", "empty-after"},
		"final-sync":        {"final-sync-read", "final-sync-reserve"},
		"archive-intent":    {"archive-pins-before-issuer", "archive-pins-with-issuer", "archive-issuer-read", "archive-issuer-register", "archive-read", "archive-write"},
		"archive-send":      {"archive-guard", "archive-empty-guard", "archive-query-guard"},
		"archive-recovery":  {"intent-admit-read", "intent-admit-write", "intent-arbitrate", "recovery-claim-guard", "recovery-authority-guard", "recovery-source-guard.uncharged", "recovery-source-guard.charged", "recovery-slot-read", "recovery-slot-intent-read", "recovery-reserve", "recovery-completion-read.intent", "recovery-completion-read.checkpoint", "recovery-settle", "recovery-checkpoint-read", "recovery-checkpoint-write", "recovery-recycle"},
		"archive-recycling": {"candidate-read", "intent-read", "intent-arbitrate", "reserve", "completion-read.intent", "completion-read.checkpoint", "terminal", "gc"},
		"local-progress":    {"progress-read", "progress-initial", "progress-update", "progress-charged-update", "metadata-reserve-read.slot", "metadata-reserve-read.slot-intent", "metadata-reserve", "metadata-guard", "metadata-terminal", "metadata-settle"},
		"local-complete":    {"checkpoint-read", "completion-read", "effects-seal", "completion-publish", "completion-guard"},
		"termination":       {"authority-history-read.domain", "authority-history-read.local-completion", "authority-history-read.publication-initial", "authority-history-read.publication-complete", "authority-history-read.enrollment", "authority-history-read.archive-final", "authority-history-read.quiescence-pins", "termination-read", "provider-read", "reserve", "reserved-guard", "terminated-write", "terminated-guard", "cleanup-complete-write", "cleanup-complete-guard"},
		"provider":          cleanupLocalWireProviderNames(),
		"owner-release":     {"release-proof-read", "release-history-read.terminal-provider", "release-history-read.retained-history", "release"},
	}
}

func cleanupLocalWireProviderNames() []string {
	names := []string{"provider-initial", "provider-before-termination", "provider-after-termination"}
	for i := 0; i < 7; i++ {
		for _, phase := range []string{"intent", "unknown", "complete", "guard"} {
			names = append(names, fmt.Sprintf("step-%d-%s", i, phase))
		}
		if i >= 2 && i <= 5 {
			names = append(names, fmt.Sprintf("step-%d-skipped", i))
		}
	}
	return names
}

func cleanupLocalWireBuild(t *testing.T) (*Backend, cleanupLocalWireInput, *cleanupLocalWirePlan) {
	t.Helper()
	b, input := cleanupLocalWireFixture(t)
	plan, err := b.buildCleanupLocalWirePlan(input)
	require.NoError(t, err)
	require.NotNil(t, plan)
	require.NoError(t, validateCleanupLocalWirePlan(plan))
	return b, input, plan
}

func cleanupLocalWireFind(t *testing.T, plan *cleanupLocalWirePlan, family, variant string) cleanupWireProjection {
	t.Helper()
	for _, f := range plan.families {
		if f.name != family {
			continue
		}
		for _, projection := range f.txns {
			if projection.name == variant {
				return projection
			}
		}
	}
	t.Fatalf("missing concrete projection %s/%s", family, variant)
	return cleanupWireProjection{}
}

func TestCleanupLocalWireFamilies(t *testing.T) {
	_, _, plan := cleanupLocalWireBuild(t)
	expected := cleanupLocalWireExpected()
	require.Len(t, plan.families, 22)
	seen := make(map[string]bool)
	for _, family := range plan.families {
		require.False(t, seen[family.name], "duplicate family %s", family.name)
		seen[family.name] = true
		want, ok := expected[family.name]
		require.True(t, ok, "unexpected family %s", family.name)
		names := make(map[string]bool)
		checkName := func(name string) {
			require.False(t, names[name], "duplicate native variant %s/%s", family.name, name)
			names[name] = true
			require.Contains(t, want, name, "undeclared native variant %s/%s", family.name, name)
		}
		for _, projection := range family.txns {
			checkName(projection.name)
			require.NoError(t, validateCleanupWire(projection))
			require.NotEmpty(t, projection.request.Success, "no placeholder Txn for %s/%s", family.name, projection.name)
		}
		for _, lease := range family.leases {
			checkName(lease.name)
			require.NoError(t, validateCleanupLeaseWire(lease))
		}
		require.Len(t, names, len(want), "%s omitted a required native variant", family.name)
		if family.name == "task-release" {
			require.Empty(t, family.txns)
			require.Len(t, family.leases, 1)
			require.NotNil(t, family.leases[0].revoke)
			require.NotNil(t, family.leases[0].revoked)
		}
	}
	for name := range expected {
		require.True(t, seen[name], "missing family %s", name)
	}
	for _, variant := range []string{"query-quiescence-history.initial", "query-quiescence-history.prior-initial", "query-quiescence-history.prior-complete", "query-quiescence-stage-receipt"} {
		t.Run("missing-expanded-"+variant, func(t *testing.T) {
			_, _, bad := cleanupLocalWireBuild(t)
			removed := false
			for i := range bad.families {
				if bad.families[i].name != "quiescence-query" {
					continue
				}
				for j, projection := range bad.families[i].txns {
					if projection.name == variant {
						bad.families[i].txns = append(bad.families[i].txns[:j], bad.families[i].txns[j+1:]...)
						removed = true
						break
					}
				}
			}
			require.True(t, removed)
			require.Error(t, validateCleanupLocalWirePlan(bad), "other subreads still present must not hide omission")
		})
	}
	for _, tc := range []struct {
		variant string
		points  int
	}{
		{"query-quiescence-history.initial", 3},
		{"query-quiescence-history.prior-initial", 3},
		{"query-quiescence-history.prior-complete", 4},
		{"query-quiescence-stage-receipt", 7},
	} {
		projection := cleanupLocalWireFind(t, plan, "quiescence-query", tc.variant)
		require.Len(t, projection.request.Compare, 4)
		require.Len(t, projection.request.Success, tc.points)
		require.Len(t, projection.request.Failure, tc.points)
		for _, op := range projection.request.Success {
			require.NotNil(t, op.GetRequestRange())
			require.Empty(t, op.GetRequestRange().RangeEnd)
		}
	}

	for _, change := range []string{"missing-family", "duplicate-family", "unknown-family", "missing-variant", "duplicate-variant", "unknown-variant", "arbitrary-suffix", "empty-txn", "lease-as-txn"} {
		t.Run(change, func(t *testing.T) {
			_, _, bad := cleanupLocalWireBuild(t)
			switch change {
			case "missing-family":
				bad.families = bad.families[1:]
			case "duplicate-family":
				bad.families = append(bad.families, bad.families[0])
			case "unknown-family":
				bad.families[0].name = "unknown"
			case "missing-variant":
				bad.families[0].txns = nil
			case "duplicate-variant":
				bad.families[0].txns = append(bad.families[0].txns, bad.families[0].txns[0])
			case "unknown-variant":
				bad.families[0].txns[0].name = "unknown"
			case "arbitrary-suffix":
				bad.families[0].txns[0].name += ".unlisted"
			case "empty-txn":
				bad.families[0].txns[0].request = &pb.TxnRequest{}
			case "lease-as-txn":
				for i := range bad.families {
					if bad.families[i].name == "task-release" {
						bad.families[i].leases = nil
						bad.families[i].txns = []cleanupWireProjection{{name: "task-revoke", request: &pb.TxnRequest{}, success: &pb.TxnResponse{}, failure: &pb.TxnResponse{}}}
					}
				}
			}
			require.Error(t, validateCleanupLocalWirePlan(bad))
		})
	}
}

func TestCleanupLocalWireOrdinaryProfile(t *testing.T) {
	_, input, plan := cleanupLocalWireBuild(t)
	require.Equal(t, uint32(2), input.snapshot.Version)
	require.Equal(t, "local", input.snapshot.LocalCleanup.Provider)
	require.Equal(t, uint8(0), input.publication.Record.MountAttempt)
	require.Len(t, input.bundle.fences, 8)
	for _, family := range plan.families {
		for _, projection := range family.txns {
			nodes := len(projection.request.Compare) + len(projection.request.Success) + len(projection.request.Failure)
			t.Logf("%s/%s cmp=%d success=%d failure=%d nodes=%d request=%d successBytes=%d failureBytes=%d", family.name, projection.name, len(projection.request.Compare), len(projection.request.Success), len(projection.request.Failure), nodes, projection.request.Size(), projection.success.Size(), projection.failure.Size())
			require.LessOrEqual(t, nodes, 64)
			require.LessOrEqual(t, projection.request.Size(), 128<<10)
			require.LessOrEqual(t, projection.success.Size(), 256<<10)
			require.LessOrEqual(t, projection.failure.Size(), 256<<10)
		}
		for _, lease := range family.leases {
			require.NoError(t, validateCleanupLeaseWire(lease))
			switch {
			case lease.grant != nil:
				t.Logf("%s/%s LeaseGrant request=%d response=%d", family.name, lease.name, lease.grant.Size(), lease.granted.Size())
			case lease.keepAlive != nil:
				t.Logf("%s/%s LeaseKeepAlive request=%d response=%d", family.name, lease.name, lease.keepAlive.Size(), lease.keptAlive.Size())
			case lease.revoke != nil:
				t.Logf("%s/%s LeaseRevoke request=%d response=%d", family.name, lease.name, lease.revoke.Size(), lease.revoked.Size())
			}
		}
	}
	for _, row := range []struct {
		family, variant string
		nodes           int
	}{
		// The original Stage now commits its reservation and Q together;
		// the authenticated immutable issuer uses its exact Mod pin.
		{"quiescence-stage", "reserved-commit", 63},
		{"task-checkpoint", "claim-create", 46},
		{"task-checkpoint", "claim-post-fence", 46},
		{"local-progress", "metadata-settle", 64},
		{"local-complete", "completion-publish", 64},
		{"owner-release", "release", 64},
	} {
		projection := cleanupLocalWireFind(t, plan, row.family, row.variant)
		require.Equal(t, row.nodes, len(projection.request.Compare)+len(projection.request.Success)+len(projection.request.Failure), "%s/%s", row.family, row.variant)
	}
}

// Adjust only protobuf payload bytes until Size itself equals the requested
// boundary; field-tag and varint overhead are measured rather than guessed.
func cleanupLocalWireSizedRequest(t *testing.T, size int) cleanupWireProjection {
	t.Helper()
	projection := cleanupWireProjection{name: "boundary", request: &pb.TxnRequest{Success: []*pb.RequestOp{taskProtoPut("/boundary", "", 0)}}, success: taskProtoPutResponse(1), failure: taskProtoResponse(nil, nil)}
	for i := 0; i < 8 && projection.request.Size() != size; i++ {
		put := projection.request.Success[0].GetRequestPut()
		length := len(put.Value) + size - projection.request.Size()
		require.GreaterOrEqual(t, length, 0)
		put.Value = make([]byte, length)
	}
	require.Equal(t, size, projection.request.Size())
	return projection
}

func cleanupLocalWireSizedResponse(t *testing.T, size int) *pb.TxnResponse {
	t.Helper()
	response := taskProtoResponse([]string{"/boundary"}, []string{""})
	for i := 0; i < 8 && response.Size() != size; i++ {
		kv := response.Responses[0].GetResponseRange().Kvs[0]
		length := len(kv.Value) + size - response.Size()
		require.GreaterOrEqual(t, length, 0)
		kv.Value = make([]byte, length)
	}
	require.Equal(t, size, response.Size())
	return response
}

func TestCleanupLocalWireBounds(t *testing.T) {
	t.Run("longest-actual-profile", func(t *testing.T) {
		// Every fixture codec has already succeeded before the builder runs.
		// A valid large profile may fit; no fabricated tuple must overflow.
		b, input := cleanupLocalWireFixture(t, true)
		require.Len(t, b.namespace.Root(), 512)
		require.Len(t, b.restoreEpoch, 128)
		require.Len(t, input.workerID, 128)
		require.Len(t, input.bundle.task.Runtime.ID, 128)
		plan, err := b.buildCleanupLocalWirePlan(input)
		if err != nil {
			require.ErrorIs(t, err, ErrInvalidConfiguration)
			require.Contains(t, err.Error(), "profile")
			identified := false
			for _, variants := range cleanupLocalWireExpected() {
				for _, variant := range variants {
					if strings.Contains(err.Error(), variant) {
						identified = true
					}
				}
			}
			require.True(t, identified, "wire refusal must identify its concrete projection: %v", err)
			require.Nil(t, plan)
			t.Logf("canonical maximum profile refused before effects: %v", err)
			return
		}
		require.NoError(t, validateCleanupLocalWirePlan(plan))
		for _, family := range plan.families {
			for _, projection := range family.txns {
				require.NoError(t, validateCleanupWire(projection))
				t.Logf("maximum-profile %s/%s request=%d success=%d failure=%d nodes=%d", family.name, projection.name, projection.request.Size(), projection.success.Size(), projection.failure.Size(), len(projection.request.Compare)+len(projection.request.Success)+len(projection.request.Failure))
			}
		}
	})
	t.Run("literal-request-and-both-response-boundaries", func(t *testing.T) {
		projection := cleanupLocalWireSizedRequest(t, 128<<10)
		require.NoError(t, validateCleanupWire(projection))
		require.Error(t, validateCleanupWire(cleanupLocalWireSizedRequest(t, 128<<10+1)))
		projection.success = cleanupLocalWireSizedResponse(t, 256<<10)
		projection.failure = cleanupLocalWireSizedResponse(t, 256<<10)
		require.NoError(t, validateCleanupWire(projection))
		projection.success = cleanupLocalWireSizedResponse(t, 256<<10+1)
		require.Error(t, validateCleanupWire(projection))
		projection.success = cleanupLocalWireSizedResponse(t, 256<<10)
		projection.failure = cleanupLocalWireSizedResponse(t, 256<<10+1)
		require.Error(t, validateCleanupWire(projection))
	})
	t.Run("literal-nodes-and-no-nesting", func(t *testing.T) {
		projection := cleanupLocalWireSizedRequest(t, 128)
		projection.request.Success = nil
		for i := 0; i < 32; i++ {
			projection.request.Success = append(projection.request.Success, taskProtoRange(fmt.Sprintf("/s/%d", i)))
			projection.request.Failure = append(projection.request.Failure, taskProtoRange(fmt.Sprintf("/f/%d", i)))
		}
		require.NoError(t, validateCleanupWire(projection))
		projection.request.Failure = append(projection.request.Failure, taskProtoRange("/65"))
		require.Error(t, validateCleanupWire(projection))
		projection.request.Failure = nil
		projection.request.Success = []*pb.RequestOp{{Request: &pb.RequestOp_RequestTxn{RequestTxn: &pb.TxnRequest{Success: []*pb.RequestOp{taskProtoRange("/nested")}}}}}
		require.Error(t, validateCleanupWire(projection))
	})
	t.Run("real-lease-pairs", func(t *testing.T) {
		header := &pb.ResponseHeader{ClusterId: math.MaxUint64, MemberId: math.MaxUint64, Revision: math.MaxInt64, RaftTerm: math.MaxUint64}
		for _, projection := range []cleanupLeaseWireProjection{
			{name: "grant", grant: &pb.LeaseGrantRequest{ID: math.MaxInt64, TTL: math.MaxInt64}, granted: &pb.LeaseGrantResponse{Header: header, ID: math.MaxInt64, TTL: math.MaxInt64}},
			{name: "keepalive", keepAlive: &pb.LeaseKeepAliveRequest{ID: math.MaxInt64}, keptAlive: &pb.LeaseKeepAliveResponse{Header: header, ID: math.MaxInt64, TTL: math.MaxInt64}},
			{name: "revoke", revoke: &pb.LeaseRevokeRequest{ID: math.MaxInt64}, revoked: &pb.LeaseRevokeResponse{Header: header}},
		} {
			require.NoError(t, validateCleanupLeaseWire(projection))
		}
		for _, bad := range []cleanupLeaseWireProjection{
			{}, {name: "missing-response", grant: &pb.LeaseGrantRequest{TTL: 30}},
			{name: "wrong-response", grant: &pb.LeaseGrantRequest{TTL: 30}, revoked: &pb.LeaseRevokeResponse{Header: header}},
			{name: "two-rpcs", revoke: &pb.LeaseRevokeRequest{ID: 1}, revoked: &pb.LeaseRevokeResponse{Header: header}, keepAlive: &pb.LeaseKeepAliveRequest{ID: 1}, keptAlive: &pb.LeaseKeepAliveResponse{Header: header, ID: 1, TTL: 30}},
			{name: "grant-error", grant: &pb.LeaseGrantRequest{TTL: 30}, granted: &pb.LeaseGrantResponse{Header: header, ID: 1, TTL: 30, Error: "failed"}},
		} {
			require.Error(t, validateCleanupLeaseWire(bad))
		}
	})
	t.Run("whole-record-caps-and-seven-provider-slots", cleanupLocalWireRecordCaps)
	t.Run("real-prefix-range", func(t *testing.T) {
		b, input, plan := cleanupLocalWireBuild(t)
		prefix, err := b.taskOperationsPrefix(input.bundle.task.Reference)
		require.NoError(t, err)
		for _, variant := range []string{"operations-page", "empty-before", "empty-after"} {
			projection := cleanupLocalWireFind(t, plan, "dual-drain", variant)
			for _, branch := range [][]*pb.RequestOp{projection.request.Success, projection.request.Failure} {
				require.Len(t, branch, 1)
				rangeRequest := branch[0].GetRequestRange()
				require.NotNil(t, rangeRequest)
				require.Equal(t, prefix, string(rangeRequest.Key))
				require.Equal(t, clientv3.GetPrefixRangeEnd(prefix), string(rangeRequest.RangeEnd))
				wantLimit := int64(1)
				if variant == "operations-page" {
					wantLimit = 16
				}
				require.Equal(t, wantLimit, rangeRequest.Limit)
			}
		}
	})
}

func cleanupLocalWireRecordCaps(t *testing.T) {
	_, _, plan := cleanupLocalWireBuild(t)
	for _, tc := range []struct {
		family, variant string
		cap             int
	}{
		{"stage-commit", "close-commit", 16384},
		{"quiescence-stage", "reserved-commit", 32768},
		{"local-progress", "metadata-settle", 16384},
		{"local-complete", "completion-publish", 8192},
		{"termination", "terminated-write", 16384},
		{"provider", "provider-after-termination", 16384},
		{"owner-release", "release", 8192},
	} {
		projection := cleanupLocalWireFind(t, plan, tc.family, tc.variant)
		found := false
		for _, op := range projection.request.Success {
			if put := op.GetRequestPut(); put != nil && len(put.Value) == tc.cap {
				found = true
			}
		}
		require.True(t, found, "%s/%s must bound the entire future record at %d bytes", tc.family, tc.variant, tc.cap)
	}

	// Exercise the original schema independently of dimensional cap bytes. RV
	// permits control bytes: a 128-byte value may occupy 768 JSON bytes.
	record := localReleaseRecordFixture(t)
	require.Len(t, record.Slots, 7)
	for i := range record.Slots {
		kind := "Pod"
		if i == 2 || i == 3 {
			kind = "NetworkPolicy"
		} else if i == 4 || i == 5 {
			kind = "CiliumNetworkPolicy"
		}
		record.Slots[i] = taskRuntimeCleanupSlot{Step: taskRuntimeCleanupSteps[i], State: "complete", Kind: kind, Name: "pod", UID: record.Binding.Runtime.UID, ResourceVersion: strings.Repeat("\x00", 128), EffectID: taskClaimTestID, Claim: &taskCloseClaimWire{Task: record.Task, ClaimID: taskClaimTestID, WorkerID: "cleanup", CreateRevision: math.MaxInt64, LeaseID: math.MaxInt64}}
	}

	// The inherited fixture uses maximum routing identities. Seven repeated
	// claims plus escaped RVs exceed the whole-record cap despite valid fields.
	require.NoError(t, record.Validate())
	maximumWire, err := json.Marshal(record)
	require.NoError(t, err)
	require.Greater(t, len(maximumWire), 16384)
	_, err = encodeTaskRuntimeProviderCleanup(record)
	require.ErrorIs(t, err, ErrInvalidRecord)

	// Keep all seven maximum escaped RVs, but use an ordinary routing profile
	// consistently in the Task, provider binding and each slot's claim.
	namespace, err := NewNamespace("/codex-test", "tasks", "cell")
	require.NoError(t, err)
	record.Task.Namespace, record.Task.RestoreEpoch, record.Task.SandboxID = namespace.Root(), "epoch", "sandbox"
	record.Binding.Authority.Namespace = record.Task.Namespace
	record.Binding.Authority.RestoreEpoch = record.Task.RestoreEpoch
	record.Binding.LogicalID = record.Task.SandboxID
	for i := range record.Slots {
		record.Slots[i].Claim.Task = record.Task
	}

	require.NoError(t, record.Validate())
	wire, err := encodeTaskRuntimeProviderCleanup(record)
	require.NoError(t, err)
	require.Contains(t, wire, strings.Repeat(`\u0000`, 128))
	require.LessOrEqual(t, len(wire), 16384)
	t.Logf("seven escaped provider RVs: maximum profile=%d ordinary profile=%d cap=16384", len(maximumWire), len(wire))
	for i := 2; i <= 5; i++ {
		skipped := record
		skipped.Slots[i] = taskRuntimeCleanupSlot{Step: taskRuntimeCleanupSteps[i], State: "skipped"}
		_, err := encodeTaskRuntimeProviderCleanup(skipped)
		require.NoError(t, err)
	}
	oversized := record
	oversized.Descriptor = bytes.Repeat([]byte{'d'}, 16384)
	_, err = encodeTaskRuntimeProviderCleanup(oversized)
	require.Error(t, err, "raw descriptor and complete record have separate bounds")
}

func TestCleanupLocalWireSharedBuilders(t *testing.T) {
	b, _, plan := cleanupLocalWireBuild(t)
	for _, pair := range [][4]string{
		{"archive-recovery", "recovery-checkpoint-write", "archive-recovery", "recovery-recycle"},
		{"local-complete", "completion-read", "local-progress", "progress-read"},
		{"termination", "provider-read", "owner-release", "release-history-read.terminal-provider"},
	} {
		a := cleanupLocalWireFind(t, plan, pair[0], pair[1])
		b := cleanupLocalWireFind(t, plan, pair[2], pair[3])
		require.Equal(t, a.request, b.request)
		require.Equal(t, a.success, b.success)
		require.Equal(t, a.failure, b.failure)
	}
	for step := 0; step < 7; step++ {
		unknown := cleanupLocalWireFind(t, plan, "provider", fmt.Sprintf("step-%d-unknown", step))
		intent := cleanupLocalWireFind(t, plan, "provider", fmt.Sprintf("step-%d-intent", step))
		require.Equal(t, intent.request, unknown.request)
		require.Equal(t, intent.success, unknown.success)
		require.Equal(t, intent.failure, unknown.failure)
	}
	for _, group := range []struct {
		name   string
		points int
	}{
		{"domain", 8}, {"local-completion", 3}, {"publication-initial", 4}, {"publication-complete", 14}, {"enrollment", 3}, {"archive-final", 2}, {"quiescence-pins", 4},
	} {
		projection := cleanupLocalWireFind(t, plan, "termination", "authority-history-read."+group.name)
		require.Len(t, projection.request.Success, group.points+2)
		require.Len(t, projection.request.Failure, group.points+2)
	}
	metadataTerminal := cleanupLocalWireFind(t, plan, "local-progress", "metadata-terminal")
	require.Len(t, metadataTerminal.request.Compare, 62)
	require.Len(t, metadataTerminal.request.Success, 1)
	require.Empty(t, metadataTerminal.request.Failure)
	require.Len(t, cleanupLocalWireFind(t, plan, "local-progress", "metadata-reserve-read.slot").request.Success, 1)
	require.Len(t, cleanupLocalWireFind(t, plan, "local-progress", "metadata-reserve-read.slot-intent").request.Success, 2)

	for _, tc := range []struct {
		family, variant string
		emptyFailure    bool
	}{
		{"archive-recycling", "gc", true},
		{"local-progress", "metadata-settle", true},
		{"local-complete", "completion-publish", false},
	} {
		projection := cleanupLocalWireFind(t, plan, tc.family, tc.variant)
		cmps := make([]clientv3.Cmp, len(projection.request.Compare))
		for i, cmp := range projection.request.Compare {
			cmps[i] = clientv3.Cmp(*cmp)
		}
		shared := workspaceSyncNativeProjection(b, cmps, projection.request.Success, projection.success, tc.emptyFailure)
		require.Equal(t, projection.request, shared.request, "%s/%s diverged from actual native envelope", tc.family, tc.variant)
		require.Equal(t, projection.success, shared.success)
		require.Equal(t, projection.failure, shared.failure)
		if tc.emptyFailure {
			require.Empty(t, projection.request.Failure)
		} else {
			require.Len(t, projection.request.Failure, 3)
			require.Equal(t, b.workspaceSyncBudget.key, string(projection.request.Failure[2].GetRequestRange().Key))
		}
	}

	release := cleanupLocalWireFind(t, plan, "owner-release", "release")
	require.Len(t, release.request.Compare, 60)
	require.Len(t, release.request.Success, 4)
	require.Empty(t, release.request.Failure)
	for i := 0; i < 3; i++ {
		require.NotNil(t, release.request.Success[i].GetRequestDeleteRange())
		require.NotNil(t, release.success.Responses[i].GetResponseDeleteRange())
		require.Equal(t, int64(1), release.success.Responses[i].GetResponseDeleteRange().Deleted)
	}
	require.NotNil(t, release.request.Success[3].GetRequestPut())
	require.NotNil(t, release.success.Responses[3].GetResponsePut())
}

// Marshal each actual protobuf, not the private container (whose unexported
// fields would encode as {}). This detects caller aliasing in retained plans.
func cleanupLocalWireFingerprint(t *testing.T, plan *cleanupLocalWirePlan) []byte {
	t.Helper()
	var entries []string
	for _, family := range plan.families {
		for _, projection := range family.txns {
			request, err := projection.request.Marshal()
			require.NoError(t, err)
			success, err := projection.success.Marshal()
			require.NoError(t, err)
			failure, err := projection.failure.Marshal()
			require.NoError(t, err)
			entries = append(entries, fmt.Sprintf("%s/%s:%x:%x:%x", family.name, projection.name, request, success, failure))
		}
		for _, lease := range family.leases {
			wire, err := json.Marshal([]any{lease.grant, lease.granted, lease.keepAlive, lease.keptAlive, lease.revoke, lease.revoked})
			require.NoError(t, err)
			entries = append(entries, family.name+"/"+lease.name+":"+string(wire))
		}
	}
	sort.Strings(entries)
	return []byte(strings.Join(entries, "\n"))
}

func TestCleanupLocalWireNoEffects(t *testing.T) {
	t.Run("protected-keys", func(t *testing.T) {
		n, err := NewNamespace("/codex-test", "tasks", "cell")
		require.NoError(t, err)
		task, _, _, _ := taskTestRecords()
		execution, err := n.cleanupExecutionKey(task.Reference)
		require.NoError(t, err)
		protected := []string{execution}
		for slot := uint32(0); slot < 32; slot++ {
			key, err := n.cleanupExecutionSlotKey(slot)
			require.NoError(t, err)
			protected = append(protected, key)
		}
		for _, key := range protected {
			for _, deleteKey := range []bool{false, true} {
				write := Write{Key: key, Delete: deleteKey}
				if !deleteKey {
					write.Value = []byte(`{}`)
				}
				_, _, err := prepareMutation(n, Mutation{Writes: []Write{write}})
				require.ErrorIs(t, err, ErrInvalidMutation, "generic Stage must not mutate cleanup protocol key %s (delete=%v)", key, deleteKey)
			}
		}
		checkpoint, err := n.taskCheckpointKey(task.Reference)
		require.NoError(t, err)
		for _, key := range []string{
			n.Root() + "business/execution",
			n.Root() + "cleanup-execution-slots-extra/00",
			execution + "-extra",
			checkpoint,
		} {
			for _, deleteKey := range []bool{false, true} {
				write := Write{Key: key, Delete: deleteKey}
				if !deleteKey {
					write.Value = []byte(`{}`)
				}
				_, _, err := prepareMutation(n, Mutation{Writes: []Write{write}})
				require.NoError(t, err, "cleanup protection must not capture neighboring business key %s", key)
			}
		}
	})

	b, input, plan := cleanupLocalWireBuild(t)
	require.Nil(t, b.client)
	require.Nil(t, b.archiveStore)
	before := cleanupLocalWireFingerprint(t, plan)
	_, err := b.prepareCleanupLocalWireInventory(input, nil)
	require.ErrorIs(t, err, ErrInvalidConfiguration)
	require.Equal(t, before, cleanupLocalWireFingerprint(t, plan))

	// Mutating caller-owned backing after the pure build cannot change any
	// request, failure response or retained descriptor in the completed plan.
	input.snapshot.Payload[0] = 'x'
	input.snapshot.LocalCleanup.Exclude[0] = "changed"
	input.plan.exclude[0] = "changed"
	input.issuer.Record.Certificate[0] = 'x'
	input.bundle.fences[0].value = "changed"
	input.bundle.checkpoint.Value[0] = 'x'
	input.publication.Record.NodeUID = taskClaimTestID
	input.state.record.Pending.ClaimID = input.state.record.Run.ID
	require.Equal(t, before, cleanupLocalWireFingerprint(t, plan))
	require.NoError(t, validateCleanupLocalWirePlan(plan))

	for _, change := range []string{"missing-descriptor", "wrong-placement", "wrong-digest", "missing-bundle", "missing-publication", "joined-run", "bad-ttl"} {
		t.Run(change, func(t *testing.T) {
			b, bad := cleanupLocalWireFixture(t)
			switch change {
			case "missing-descriptor":
				bad.snapshot.LocalCleanup = nil
			case "wrong-placement":
				bad.snapshot.LocalCleanup.Partition++
			case "wrong-digest":
				bad.snapshot.Snapshot.Digest = strings.Repeat("0", 64)
			case "missing-bundle":
				bad.bundle = nil
			case "missing-publication":
				bad.publication = nil
			case "joined-run":
				bad.state.record.Run.Joined = true
			case "bad-ttl":
				bad.claimTTL = 0
			}
			plan, err := b.buildCleanupLocalWirePlan(bad)
			require.Error(t, err)
			require.Nil(t, plan)
			require.Nil(t, b.client)
		})
	}
}
