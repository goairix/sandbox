//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The caller has completed the original physical observer/provider release.
// All quota calls use complete copies of that zero-cap worker's Backend options;
// only the endpoint and each Backend's private recovery directory change.
func coldQuotaJournalAfterRealRelease(t *testing.T, ctx context.Context, cold *state.Backend, options state.Options, tlsConfig *tls.Config, workspace state.WorkspaceIdentity, task state.TaskReference, setup localReleaseNativeSetup, next state.AcquireIntentInput) {
	t.Helper()
	original, err := cold.LoadColdPreparation(ctx, workspace, setup.IntentID)
	require.NoError(t, err)
	require.NotNil(t, original)
	require.Equal(t, "settled", original.Phase)
	require.Equal(t, task.SandboxID, original.Owner.SandboxID)

	// An actual new workspace owner and preparation incarnation already exist
	// when refund begins; retained death evidence must never reclaim either.
	claim, err := cold.ClaimCreation(ctx, workspace, next.IntentID, "journal-preparation-reuse", time.Minute)
	require.NoError(t, err)
	preparation, err := cold.ReserveColdPreparation(ctx, claim, 8)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, preparation.Outcome)
	require.Equal(t, original.Preparation.ID, preparation.Record.Preparation.ID)
	require.Equal(t, original.Preparation.Epoch+1, preparation.Record.Preparation.Epoch)
	require.NoError(t, cold.ReleaseCreationClaim(ctx, claim))

	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig.Clone(), DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	ownerKey, err := options.Namespace.Key("p", fmt.Sprintf("%02x", workspace.Partition()), "workspaces", workspace.Hash(), "owner")
	require.NoError(t, err)
	ownerBefore := coldQuotaReadOne(t, ctx, raw, ownerKey)
	journalKey, err := options.Namespace.Key("p", fmt.Sprintf("%02x", workspace.Partition()), "intents", setup.IntentID, "cold-runtime-quota-attempt")
	require.NoError(t, err)
	returnKey, err := options.Namespace.Key("p", fmt.Sprintf("%02x", workspace.Partition()), "intents", setup.IntentID, "cold-runtime-quota-return")
	require.NoError(t, err)
	endpoint, proxy := newLocalReleaseNativeProxy(t, tlsConfig, setup.Fault)
	options.Endpoints = []string{endpoint}
	first := coldQuotaJournalNativeBackend(t, ctx, options)
	observation := proxy.journal
	observation.mu.Lock()
	observation.journalKey, observation.returnKey = journalKey, returnKey
	observation.watchGrants = true
	observation.mu.Unlock()

	var previous, aborted *mvccpb.KeyValue
	returner := first
	if setup.Fault == "quota-journal-ACK" {
		unknown, err := first.SettleColdRuntimeQuota(ctx, workspace, setup.IntentID, task)
		require.ErrorIs(t, err, state.ErrOutcomeUnknown)
		require.Equal(t, state.OutcomeUnknown, unknown.Outcome)
		require.NotEqual(t, state.StageReference{}, unknown.Reference)
		observation.mu.Lock()
		grantCount, dropped := len(observation.grants), observation.dropped
		observation.mu.Unlock()
		require.True(t, dropped)
		require.Zero(t, grantCount, "lost journal ACK must never reach actual Grant")
		previous = coldQuotaReadOne(t, ctx, raw, journalKey)
		coldQuotaJournalNativeReference(t, previous, unknown.Reference)
		require.Equal(t, int64(1), previous.Version)
		coldQuotaAssertHeld(t, ctx, raw, options.Namespace, *original)
		missing, err := raw.Get(ctx, returnKey)
		require.NoError(t, err)
		require.Empty(t, missing.Kvs)

		// Close joins the original public owner. The fresh Backend's public
		// Settle must arbitrate the retained ref before replacing the journal;
		// the fixture does not resolve or reconstruct the lost continuation.
		require.NoError(t, first.Close())
		returner = coldQuotaJournalNativeBackend(t, ctx, options)
	}

	returned, err := returner.SettleColdRuntimeQuota(ctx, workspace, setup.IntentID, task)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, returned.Outcome)
	require.NotNil(t, returned.Entry)
	require.Equal(t, original.Quota, returned.Entry.Quota)
	require.Equal(t, original.Owner, returned.Entry.Owner)
	require.Equal(t, task, returned.Entry.Task)
	require.Equal(t, state.RuntimeReference(setup.Runtime), returned.Entry.Runtime)
	require.Equal(t, setup.NodeUID, returned.Entry.NodeUID)
	require.Equal(t, setup.NodeBootID, returned.Entry.NodeBootID)
	current := coldQuotaReadOne(t, ctx, raw, journalKey)
	coldQuotaJournalNativeReference(t, current, returned.Reference)
	if previous != nil {
		oldRef, err := coldQuotaJournalNativeRecord(previous)
		require.NoError(t, err)
		require.NotEqual(t, oldRef, returned.Reference)
		aborted = coldQuotaJournalNativeReceipt(t, ctx, raw, options.Namespace, oldRef, state.OutcomeAborted)
		require.Greater(t, aborted.CreateRevision, previous.ModRevision)
		require.Greater(t, current.ModRevision, aborted.CreateRevision)
	}
	returnKV := coldQuotaReadOne(t, ctx, raw, returnKey)
	require.Zero(t, returnKV.Lease)
	require.Equal(t, int64(1), returnKV.Version)
	require.Equal(t, returnKV.CreateRevision, returnKV.ModRevision)
	receipt := coldQuotaJournalNativeReceipt(t, ctx, raw, options.Namespace, returned.Reference, state.OutcomeCommitted)
	require.Equal(t, returnKV.CreateRevision, receipt.CreateRevision)
	require.Equal(t, returned.Entry.CreateRevision, returnKV.CreateRevision)
	require.Greater(t, returnKV.CreateRevision, current.ModRevision)

	// The quota slot, free index, exact return and receipt are one actual etcd
	// transaction. Compare their revisions before the allocator reuses the slot.
	slotKey, freeKey := coldQuotaSlotKeys(t, options.Namespace, original.Quota)
	slot := coldQuotaReadOne(t, ctx, raw, slotKey)
	free := coldQuotaReadOne(t, ctx, raw, freeKey)
	var released struct {
		Occupied bool                    `json:"occupied"`
		Owner    state.ColdSlotOwner     `json:"owner"`
		Slot     state.ColdSlotReference `json:"slot"`
	}
	require.NoError(t, json.Unmarshal(slot.Value, &released))
	require.False(t, released.Occupied)
	require.Equal(t, state.ColdSlotOwner{}, released.Owner)
	require.Equal(t, original.Quota, released.Slot)
	require.Zero(t, slot.Lease)
	require.Equal(t, slot.Value, free.Value)
	require.Equal(t, returnKV.CreateRevision, slot.ModRevision)
	require.Equal(t, returnKV.CreateRevision, free.CreateRevision)
	coldQuotaJournalNativeTransactions(t, proxy, current, previous, aborted, returned.Reference)

	observation.mu.Lock()
	observation.watchGrants = false
	observation.mu.Unlock()
	coldQuotaJournalNativeReuse(t, ctx, returner, raw, options.Namespace, options.Identity.StorageID, workspace, task, setup.IntentID, original.Quota, returned, proxy, journalKey)
	require.Equal(t, ownerBefore, coldQuotaReadOne(t, ctx, raw, ownerKey))
	preparationAfter, err := returner.LoadColdPreparation(ctx, workspace, next.IntentID)
	require.NoError(t, err)
	require.Equal(t, preparation.Record, preparationAfter)
	require.Equal(t, current, coldQuotaReadOne(t, ctx, raw, journalKey))
	require.Equal(t, returnKV, coldQuotaReadOne(t, ctx, raw, returnKey))
	require.NoError(t, returner.Close())
	t.Logf("%s: real physical release; durable journal before Grant; cold recovery; one exact refund; actual quota epoch reuse and immutable public replay", setup.Fault)
}

func coldQuotaJournalNativeBackend(t *testing.T, ctx context.Context, options state.Options) *state.Backend {
	t.Helper()
	require.NotNil(t, options.TaskWorkspaceArchiveRecovery)
	require.NotNil(t, options.PreparationInventory)
	// Each complete Backend retains the original provider/issuers and owns a
	// separate recovery store; the live worker's old spool may still be busy.
	recovery := *options.TaskWorkspaceArchiveRecovery
	recovery.Directory = t.TempDir()
	require.NoError(t, os.Chmod(recovery.Directory, 0700))
	options.TaskWorkspaceArchiveRecovery = &recovery
	backend, err := state.New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	return backend
}

func coldQuotaJournalNativeReference(t *testing.T, kv *mvccpb.KeyValue, expected state.StageReference) {
	t.Helper()
	require.Zero(t, kv.Lease)
	require.Positive(t, kv.CreateRevision)
	require.GreaterOrEqual(t, kv.ModRevision, kv.CreateRevision)
	require.Positive(t, kv.Version)
	ref, err := coldQuotaJournalNativeRecord(kv)
	require.NoError(t, err)
	require.Equal(t, expected, ref)
}

func coldQuotaJournalNativeReceipt(t *testing.T, ctx context.Context, raw *clientv3.Client, namespace state.Namespace, ref state.StageReference, outcome state.Outcome) *mvccpb.KeyValue {
	t.Helper()
	key := coldQuotaReceiptKey(t, namespace, state.StageAttemptLocator{Namespace: ref.Namespace, RestoreEpoch: ref.RestoreEpoch, Partition: ref.Partition, RequestID: ref.RequestID, StageID: ref.StageID, AttemptID: ref.AttemptID})
	kv := coldQuotaReadOne(t, ctx, raw, key)
	var receipt struct {
		Version uint32 `json:"version"`
		state.StageReference
		Outcome state.Outcome `json:"outcome"`
	}
	require.NoError(t, json.Unmarshal(kv.Value, &receipt))
	require.Equal(t, uint32(1), receipt.Version)
	require.Equal(t, ref, receipt.StageReference)
	require.Equal(t, outcome, receipt.Outcome)
	require.Zero(t, kv.Lease)
	require.Equal(t, int64(1), kv.Version)
	require.Equal(t, kv.CreateRevision, kv.ModRevision)
	return kv
}

func coldQuotaJournalNativeTransactions(t *testing.T, proxy *localReleaseNativeProxy, current, previous, aborted *mvccpb.KeyValue, ref state.StageReference) {
	t.Helper()
	observation := proxy.journal
	observation.mu.Lock()
	requests := append([]*pb.TxnRequest(nil), observation.journalRequests...)
	grants := append([]coldQuotaJournalNativeGrant(nil), observation.grants...)
	observation.mu.Unlock()
	wantJournals := 1
	if previous != nil {
		wantJournals = 2
		require.Equal(t, int64(2), current.Version)
		require.Equal(t, previous.CreateRevision, current.CreateRevision)
	}
	require.Len(t, requests, wantJournals)
	require.Len(t, grants, 1, "only the acknowledged successful candidate may actually Grant")
	for index, request := range requests {
		comparisons := 30
		if index == 1 {
			comparisons = 37
			coldQuotaJournalNativeFullFence(t, request, previous)
			coldQuotaJournalNativeFullFence(t, request, aborted)
		}
		require.Len(t, request.Compare, comparisons)
		require.Len(t, request.Success, 1)
		require.Len(t, request.Failure, 3)
		require.Equal(t, current.Key, request.Success[0].GetRequestPut().Key)
		require.Zero(t, request.Success[0].GetRequestPut().Lease)
		t.Logf("actual journal CAS compares=%d success=1 failure=3 nodes=%d request_bytes=%d", comparisons, comparisons+4, request.Size())
	}
	for _, grant := range grants {
		require.NoError(t, grant.err)
		require.Equal(t, ref, grant.candidate)
		require.NotNil(t, grant.response)
		require.Len(t, grant.response.Kvs, 1)
		require.Equal(t, current, grant.response.Kvs[0], "original candidate journal must already be durable before actual Grant")
		coldQuotaJournalNativeReference(t, grant.response.Kvs[0], ref)
	}

	proxy.mu.Lock()
	transactions := append([]localReleaseNativeTransaction(nil), proxy.transactions...)
	proxy.mu.Unlock()
	journals, refunds := 0, 0
	for _, transaction := range transactions {
		if transaction.Label != "quota-journal" && transaction.Label != "quota-commit" {
			continue
		}
		require.True(t, transaction.Succeeded)
		require.LessOrEqual(t, transaction.RequestBytes, 262144)
		require.LessOrEqual(t, transaction.ResponseBytes, 262144)
		if transaction.Label == "quota-journal" {
			journals++
			require.Equal(t, previous != nil && journals == 1, transaction.Dropped)
		} else {
			refunds++
			require.Equal(t, 42, transaction.Nodes)
			require.Equal(t, 34, transaction.Comparisons)
			require.Equal(t, 4, transaction.Success)
			require.Equal(t, 4, transaction.Failure)
			require.False(t, transaction.Dropped)
		}
	}
	require.Equal(t, wantJournals, journals)
	require.Equal(t, 1, refunds)
}

// A replacement pins the exact old journal and original aborted receipt with
// the protocol's four permanent-record comparisons. Version is codec evidence,
// never an additional transaction comparison or test-only product requirement.
func coldQuotaJournalNativeFullFence(t *testing.T, request *pb.TxnRequest, kv *mvccpb.KeyValue) {
	t.Helper()
	require.NotNil(t, kv)
	targets := make(map[pb.Compare_CompareTarget]int)
	for _, comparison := range request.Compare {
		if string(comparison.Key) != string(kv.Key) {
			continue
		}
		require.Empty(t, comparison.RangeEnd)
		require.Equal(t, pb.Compare_EQUAL, comparison.Result)
		targets[comparison.Target]++
		switch comparison.Target {
		case pb.Compare_VALUE:
			require.Equal(t, kv.Value, comparison.GetValue())
		case pb.Compare_CREATE:
			require.Equal(t, kv.CreateRevision, comparison.GetCreateRevision())
		case pb.Compare_MOD:
			require.Equal(t, kv.ModRevision, comparison.GetModRevision())
		case pb.Compare_LEASE:
			require.Equal(t, kv.Lease, comparison.GetLease())
		default:
			t.Fatal("unexpected permanent-record fence target")
		}
	}
	require.Equal(t, map[pb.Compare_CompareTarget]int{pb.Compare_VALUE: 1, pb.Compare_CREATE: 1, pb.Compare_MOD: 1, pb.Compare_LEASE: 1}, targets)
}

func coldQuotaJournalNativeReuse(t *testing.T, ctx context.Context, backend *state.Backend, raw *clientv3.Client, namespace state.Namespace, storageID string, workspace state.WorkspaceIdentity, task state.TaskReference, intent string, quota state.ColdSlotReference, returned state.ColdRuntimeQuotaReturnResult, proxy *localReleaseNativeProxy, journalKey string) {
	t.Helper()
	reuseWorkspace, err := state.NewWorkspaceIdentity("local", storageID, "bucket", "journal-quota-reuse/"+uuid.NewString()+"/")
	require.NoError(t, err)
	reuse := state.AcquireIntentInput{Workspace: reuseWorkspace, Principal: "fixture", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"journal_reuse":true}`), Now: time.Now().UTC(), TTL: time.Hour}
	acquired, err := backend.AcquireIntent(ctx, reuse)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, acquired.Outcome)
	claim, err := backend.ClaimCreation(ctx, reuseWorkspace, reuse.IntentID, "journal-quota-reused", time.Minute)
	require.NoError(t, err)
	reused, err := backend.ReserveColdPreparation(ctx, claim, quota.Partition)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, reused.Outcome)
	require.Equal(t, quota.ID, reused.Record.Quota.ID)
	require.Equal(t, quota.Epoch+1, reused.Record.Quota.Epoch)

	slotKey, freeKey := coldQuotaSlotKeys(t, namespace, quota)
	slotBefore := coldQuotaReadOne(t, ctx, raw, slotKey)
	ownerKey, err := namespace.Key("p", fmt.Sprintf("%02x", reuseWorkspace.Partition()), "workspaces", reuseWorkspace.Hash(), "owner")
	require.NoError(t, err)
	ownerBefore := coldQuotaReadOne(t, ctx, raw, ownerKey)
	proxy.journal.mu.Lock()
	proxy.journal.reads = nil
	proxy.journal.mu.Unlock()
	proxy.mu.Lock()
	transactionCount := len(proxy.transactions)
	proxy.mu.Unlock()

	replayed, err := backend.SettleColdRuntimeQuota(ctx, workspace, intent, task)
	require.NoError(t, err)
	require.Equal(t, returned, replayed)
	proxy.journal.mu.Lock()
	reads := append([]*pb.RangeRequest(nil), proxy.journal.reads...)
	proxy.journal.mu.Unlock()
	for _, point := range reads {
		for _, key := range []string{slotKey, freeKey, journalKey} {
			coversKey := string(point.Key) == key || len(point.RangeEnd) != 0 && string(point.Key) <= key && (string(point.RangeEnd) == "\x00" || key < string(point.RangeEnd))
			require.False(t, coversKey, "old public replay read mutable key %q through [%q,%q)", key, point.Key, point.RangeEnd)
		}
	}
	proxy.mu.Lock()
	replayTransactions := append([]localReleaseNativeTransaction(nil), proxy.transactions[transactionCount:]...)
	proxy.mu.Unlock()
	for _, transaction := range replayTransactions {
		require.NotEqual(t, "quota-journal", transaction.Label)
		require.NotEqual(t, "quota-commit", transaction.Label)
	}
	require.Equal(t, slotBefore, coldQuotaReadOne(t, ctx, raw, slotKey))
	require.Equal(t, ownerBefore, coldQuotaReadOne(t, ctx, raw, ownerKey))
	freeAfter, err := raw.Get(ctx, freeKey)
	require.NoError(t, err)
	require.Empty(t, freeAfter.Kvs)
	require.NoError(t, backend.ReleaseCreationClaim(ctx, claim))
}
