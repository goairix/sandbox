//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestColdRuntimeQuotaReturnNativeFixture(t *testing.T) {
	if os.Getenv("SANDBOX_COLD_QUOTA_NATIVE") != "1" {
		t.Skip("requires exact owned cold quota return fixture")
	}
	coldCreateNativeFixture(t, "^TestColdRuntimeQuotaReturnNative$")
}

func TestColdRuntimeQuotaReturnNativeFullchain(t *testing.T) {
	if os.Getenv("SANDBOX_COLD_QUOTA_NATIVE") != "1" {
		t.Skip("requires exact owned cold quota return fullchain")
	}
	for _, fault := range []string{"", "ack-release"} {
		name := "quota-guard-commit-ACK"
		if fault != "" {
			name = "release-ACK-quota-postread"
		}
		if !t.Run(name, func(t *testing.T) { runLocalRuntimeReleaseNativeMode(t, fault, true) }) {
			return
		}
	}
}

func coldQuotaReturnNativeProfile(o *state.Options) {
	o.PreparationInventory = &state.PreparationInventoryOptions{}
	o.PreparationInventory.QuotaSlots[7], o.PreparationInventory.QuotaSlots[8] = 1, 1
}

// This fault controller only drops actual native ACKs or corrupts a retained
// metadata reply. It cannot manufacture observer success or provider effects.
type coldQuotaReturnNativeFault struct {
	mu            sync.Mutex
	armed         string
	committed     bool
	postReadDrops int
	drops         map[string]int
	attempts      []state.StageReference
	reads         []string
	tamperKey     string
	tamperField   string
	tamperCount   int
}

func (q *coldQuotaReturnNativeFault) arm(mode string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.armed, q.committed = mode, false
}

func (q *coldQuotaReturnNativeFault) dropGrant(err error) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if err != nil || q.armed != "quota-grant" {
		return false
	}
	if q.drops == nil {
		q.drops = make(map[string]int)
	}
	q.drops["quota-grant"]++
	q.armed = ""
	return true
}

func (q *coldQuotaReturnNativeFault) before(r *pb.TxnRequest) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, op := range r.Success {
		if point := op.GetRequestRange(); point != nil {
			q.reads = append(q.reads, string(point.Key))
			if q.armed == "post-read" && q.committed && strings.HasSuffix(string(point.Key), "/cold-runtime-quota-return") {
				q.postReadDrops++
				return status.Error(codes.PermissionDenied, "fixture rejected postcommit metadata read")
			}
		}
	}
	return nil
}

func (q *coldQuotaReturnNativeFault) after(r *pb.TxnRequest, response *pb.TxnResponse, err error, label string) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, op := range r.Success {
		if put := op.GetRequestPut(); put != nil {
			if strings.HasSuffix(string(put.Key), "/cold-runtime-quota-return") {
				label = "quota-commit"
			} else if strings.HasSuffix(string(put.Key), "/guard") {
				var guard struct{ state.StageReference }
				if json.Unmarshal(put.Value, &guard) == nil && guard.StageID == "cold_runtime_quota_return" {
					label = "quota-guard"
					q.attempts = append(q.attempts, guard.StageReference)
				}
			}
		}
	}
	if err != nil || response == nil || !response.Succeeded {
		return label, false
	}
	if label == "quota-commit" {
		q.committed = true
	}
	if q.tamperKey != "" {
		for _, op := range response.Responses {
			if point := op.GetResponseRange(); point != nil {
				for _, kv := range point.Kvs {
					if string(kv.Key) != q.tamperKey {
						continue
					}
					q.tamperCount++
					if q.tamperField == "stamp" {
						kv.ModRevision++
					} else {
						var object map[string]json.RawMessage
						if json.Unmarshal(kv.Value, &object) == nil {
							object[q.tamperField], _ = json.Marshal(uuid.NewString())
							kv.Value, _ = json.Marshal(object)
						}
					}
				}
			}
		}
	}
	if q.armed == label {
		if q.drops == nil {
			q.drops = make(map[string]int)
		}
		q.drops[label]++
		q.armed = ""
		return label, true
	}
	return label, false
}

func coldQuotaReturnAfterRealRelease(t *testing.T, ctx context.Context, cold *state.Backend, options state.Options, tlsConfig *tls.Config, w state.WorkspaceIdentity, task state.TaskReference, setup localReleaseNativeSetup, next state.AcquireIntentInput) {
	t.Helper()
	original, err := cold.LoadColdPreparation(ctx, w, setup.IntentID)
	require.NoError(t, err)
	require.Equal(t, "settled", original.Phase)
	require.Equal(t, task.SandboxID, original.Owner.SandboxID)
	// Reuse the original preparation before returning quota; the next workspace
	// owner now exists and must survive all old-runtime recovery paths.
	claim, err := cold.ClaimCreation(ctx, w, next.IntentID, "quota-prep-reuse", time.Minute)
	require.NoError(t, err)
	prepReuse, err := cold.ReserveColdPreparation(ctx, claim, 8)
	require.NoError(t, err)
	require.Equal(t, original.Preparation.ID, prepReuse.Record.Preparation.ID)
	require.Equal(t, original.Preparation.Epoch+1, prepReuse.Record.Preparation.Epoch)
	require.NoError(t, cold.ReleaseCreationClaim(ctx, claim))
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig.Clone(), DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	ownerKey, err := options.Namespace.Key("p", fmt.Sprintf("%02x", w.Partition()), "workspaces", w.Hash(), "owner")
	require.NoError(t, err)
	ownerBefore := coldQuotaReadOne(t, ctx, raw, ownerKey)
	endpoint, proxy := newLocalReleaseNativeProxy(t, tlsConfig, "cold-quota")
	options.Endpoints = []string{endpoint}
	// LocalRuntimeRelease requires recovery ownership even for metadata calls.
	// Each Backend owns its own empty store; the original cold Backend retains
	// the real archive store and all termination evidence stays in etcd.
	firstRecovery := *options.TaskWorkspaceArchiveRecovery
	firstRecovery.Directory = t.TempDir()
	require.NoError(t, os.Chmod(firstRecovery.Directory, 0700))
	options.TaskWorkspaceArchiveRecovery = &firstRecovery
	first, err := state.New(ctx, options)
	require.NoError(t, err)
	defer func() { require.NoError(t, first.Close()) }()
	secondRecovery := firstRecovery
	secondRecovery.Directory = t.TempDir()
	require.NoError(t, os.Chmod(secondRecovery.Directory, 0700))
	options.TaskWorkspaceArchiveRecovery = &secondRecovery
	second, err := state.New(ctx, options)
	require.NoError(t, err)
	defer func() { require.NoError(t, second.Close()) }()
	intentPrefix := fmt.Sprintf("p/%02x/intents/%s/", w.Partition(), setup.IntentID)
	taskPrefix := fmt.Sprintf("p/%02x/tasks/%s/", task.Partition, task.TaskID)
	settlementReceipt := coldQuotaReceiptKey(t, options.Namespace, original.Settlement)
	// Corrupt actual retained metadata responses, never a death-success DTO.
	faults := []struct{ key, field string }{
		{options.Namespace.Root() + intentPrefix + "runtime-publication", "stamp"},
		{options.Namespace.Root() + intentPrefix + "runtime-publication", "node_uid"},
		{options.Namespace.Root() + intentPrefix + "runtime-publication", "node_boot_id"},
		{options.Namespace.Root() + intentPrefix + "cold-preparation", "stamp"},
		{options.Namespace.Root() + intentPrefix + "cold-work", "stamp"},
		{options.Namespace.Root() + intentPrefix + "runtime-enrollment", "stamp"},
		{settlementReceipt, "stamp"},
		{options.Namespace.Root() + taskPrefix + "owner-release", "stamp"},
		{options.Namespace.Root() + taskPrefix + "runtime-termination", "stamp"},
		{options.Namespace.Root() + taskPrefix + "runtime-provider-cleanup", "version"},
	}
	for _, fault := range faults {
		proxy.quota.mu.Lock()
		proxy.quota.tamperKey, proxy.quota.tamperField = fault.key, fault.field
		before := proxy.quota.tamperCount
		proxy.quota.mu.Unlock()
		result, err := first.SettleColdRuntimeQuota(ctx, w, setup.IntentID, task)
		require.Error(t, err)
		require.NotEqual(t, state.OutcomeCommitted, result.Outcome)
		proxy.quota.mu.Lock()
		after := proxy.quota.tamperCount
		proxy.quota.tamperKey = ""
		proxy.quota.mu.Unlock()
		require.Greater(t, after, before)
		coldQuotaAssertHeld(t, ctx, raw, options.Namespace, *original)
	}
	var returned state.ColdRuntimeQuotaReturnResult
	if setup.Fault == "" {
		proxy.quota.arm("quota-grant")
		grantUnknown, err := first.SettleColdRuntimeQuota(ctx, w, setup.IntentID, task)
		require.ErrorIs(t, err, state.ErrOutcomeUnknown)
		require.Equal(t, state.OutcomeUnknown, grantUnknown.Outcome)
		require.NotEqual(t, state.StageReference{}, grantUnknown.Reference, "exact ref must precede first actual Grant")
		outcome, err := second.ResolveStage(ctx, grantUnknown.Reference)
		require.NoError(t, err)
		require.Equal(t, state.OutcomeAborted, outcome)
		coldQuotaAssertHeld(t, ctx, raw, options.Namespace, *original)
		proxy.quota.arm("quota-guard")
		unknown, err := first.SettleColdRuntimeQuota(ctx, w, setup.IntentID, task)
		require.ErrorIs(t, err, state.ErrOutcomeUnknown)
		require.Equal(t, state.OutcomeUnknown, unknown.Outcome)
		require.NotEqual(t, state.StageReference{}, unknown.Reference)
		outcome, err = second.ResolveStage(ctx, unknown.Reference)
		require.NoError(t, err)
		require.Equal(t, state.OutcomeAborted, outcome)
		coldQuotaAssertHeld(t, ctx, raw, options.Namespace, *original)
		proxy.quota.arm("quota-commit")
		unknown, err = first.SettleColdRuntimeQuota(ctx, w, setup.IntentID, task)
		require.ErrorIs(t, err, state.ErrOutcomeUnknown)
		require.Equal(t, state.OutcomeUnknown, unknown.Outcome)
		require.NotEqual(t, state.StageReference{}, unknown.Reference)
		outcome, err = second.ResolveStage(ctx, unknown.Reference)
		require.NoError(t, err)
		require.Equal(t, state.OutcomeCommitted, outcome)
		returned, err = second.SettleColdRuntimeQuota(ctx, w, setup.IntentID, task)
		require.NoError(t, err)
		require.Equal(t, unknown.Reference, returned.Reference)
	} else {
		proxy.quota.arm("post-read")
		unknown, err := first.SettleColdRuntimeQuota(ctx, w, setup.IntentID, task)
		require.Error(t, err)
		require.Equal(t, state.OutcomeUnknown, unknown.Outcome)
		require.NotEqual(t, state.StageReference{}, unknown.Reference, "postcommit read failure must preserve exact original StageReference")
		proxy.quota.mu.Lock()
		postReadDrops := proxy.quota.postReadDrops
		attempts := append([]state.StageReference(nil), proxy.quota.attempts...)
		proxy.quota.mu.Unlock()
		require.Positive(t, postReadDrops)
		require.NotEmpty(t, attempts)
		require.Equal(t, attempts[len(attempts)-1], unknown.Reference)
		proxy.quota.arm("")
		outcome, err := second.ResolveStage(ctx, unknown.Reference)
		require.NoError(t, err)
		require.Equal(t, state.OutcomeCommitted, outcome)
		returned, err = second.SettleColdRuntimeQuota(ctx, w, setup.IntentID, task)
		require.NoError(t, err)
		require.Equal(t, unknown.Reference, returned.Reference)
	}
	require.Equal(t, state.OutcomeCommitted, returned.Outcome)
	require.Equal(t, original.Quota, returned.Entry.Quota)
	require.Equal(t, original.Owner, returned.Entry.Owner)
	require.Equal(t, task, returned.Entry.Task)
	require.Equal(t, state.RuntimeReference(setup.Runtime), returned.Entry.Runtime)
	require.Equal(t, setup.NodeUID, returned.Entry.NodeUID)
	require.Equal(t, setup.NodeBootID, returned.Entry.NodeBootID)
	// Reserve the returned original quota through the real allocator, then
	// authenticate the old return without reading or rewriting this new epoch.
	reuseW, err := state.NewWorkspaceIdentity("local", setup.Identity.StorageID, "bucket", "quota-reuse/"+uuid.NewString()+"/")
	require.NoError(t, err)
	reuse := state.AcquireIntentInput{Workspace: reuseW, Principal: "fixture", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"reuse":true}`), Now: time.Now().UTC(), TTL: time.Hour}
	acquired, err := second.AcquireIntent(ctx, reuse)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, acquired.Outcome)
	reuseClaim, err := second.ClaimCreation(ctx, reuseW, reuse.IntentID, "quota-reused", time.Minute)
	require.NoError(t, err)
	reused, err := second.ReserveColdPreparation(ctx, reuseClaim, original.Quota.Partition)
	require.NoError(t, err)
	require.Equal(t, original.Quota.ID, reused.Record.Quota.ID)
	require.Equal(t, original.Quota.Epoch+1, reused.Record.Quota.Epoch)
	slotKey, freeKey := coldQuotaSlotKeys(t, options.Namespace, original.Quota)
	slotBefore := coldQuotaReadOne(t, ctx, raw, slotKey)
	proxy.quota.mu.Lock()
	proxy.quota.reads = nil
	proxy.quota.mu.Unlock()
	replayed, err := first.SettleColdRuntimeQuota(ctx, w, setup.IntentID, task)
	require.NoError(t, err)
	require.Equal(t, returned, replayed)
	proxy.quota.mu.Lock()
	reads := append([]string(nil), proxy.quota.reads...)
	proxy.quota.mu.Unlock()
	require.NotContains(t, reads, slotKey)
	require.NotContains(t, reads, freeKey)
	prepSlot, err := options.Namespace.Key("preparation", "slots", fmt.Sprintf("%02d", original.Preparation.ID))
	require.NoError(t, err)
	prepFree, err := options.Namespace.Key("preparation", "free", fmt.Sprintf("%02d", original.Preparation.ID))
	require.NoError(t, err)
	require.NotContains(t, reads, prepSlot)
	require.NotContains(t, reads, prepFree)
	returnReceipt := coldQuotaReceiptKey(t, options.Namespace, state.StageAttemptLocator{Namespace: returned.Reference.Namespace, RestoreEpoch: returned.Reference.RestoreEpoch, Partition: returned.Reference.Partition, RequestID: returned.Reference.RequestID, StageID: returned.Reference.StageID, AttemptID: returned.Reference.AttemptID})
	for _, key := range []string{options.Namespace.Root() + intentPrefix + "cold-runtime-quota-return", returnReceipt} {
		proxy.quota.mu.Lock()
		proxy.quota.tamperKey, proxy.quota.tamperField = key, "stamp"
		before := proxy.quota.tamperCount
		proxy.quota.mu.Unlock()
		corrupt, err := second.SettleColdRuntimeQuota(ctx, w, setup.IntentID, task)
		require.Error(t, err)
		require.NotEqual(t, state.OutcomeCommitted, corrupt.Outcome)
		proxy.quota.mu.Lock()
		after := proxy.quota.tamperCount
		proxy.quota.tamperKey = ""
		proxy.quota.mu.Unlock()
		require.Greater(t, after, before)
	}
	require.Equal(t, slotBefore, coldQuotaReadOne(t, ctx, raw, slotKey))
	require.Equal(t, ownerBefore, coldQuotaReadOne(t, ctx, raw, ownerKey))
	prepAfter, err := second.LoadColdPreparation(ctx, w, next.IntentID)
	require.NoError(t, err)
	require.Equal(t, prepReuse.Record, prepAfter)
	require.NoError(t, second.ReleaseCreationClaim(ctx, reuseClaim))
	proxy.mu.Lock()
	transactions := append([]localReleaseNativeTransaction(nil), proxy.transactions...)
	proxy.mu.Unlock()
	commits := 0
	for _, transaction := range transactions {
		if transaction.Label == "quota-commit" {
			commits++
			require.True(t, transaction.Succeeded)
			require.Equal(t, 42, transaction.Nodes)
			require.Equal(t, 34, transaction.Comparisons)
			require.Equal(t, 4, transaction.Success)
			require.Equal(t, 4, transaction.Failure)
			t.Logf("actual authenticated quota return nodes=42 compares=34 success=4 failure=4 request=%d response=%d dropped=%t", transaction.RequestBytes, transaction.ResponseBytes, transaction.Dropped)
		}
	}
	require.Equal(t, 1, commits)
	proxy.quota.mu.Lock()
	grantDrops, guardDrops, commitDrops, postReadDrops := proxy.quota.drops["quota-grant"], proxy.quota.drops["quota-guard"], proxy.quota.drops["quota-commit"], proxy.quota.postReadDrops
	proxy.quota.mu.Unlock()
	if setup.Fault == "" {
		require.Equal(t, 1, grantDrops)
		require.Equal(t, 1, guardDrops)
		require.Equal(t, 1, commitDrops)
	} else {
		require.Positive(t, postReadDrops)
	}
	t.Log("real physical observer proof/provider cleanup/original64 release authenticated; quota exactly once; two Backend ACK recovery; prep/new workspace owner/quota epoch reuse unchanged")
}

func coldQuotaReadOne(t *testing.T, ctx context.Context, raw *clientv3.Client, key string) *mvccpb.KeyValue {
	t.Helper()
	response, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, response.Kvs, 1)
	return response.Kvs[0]
}

func coldQuotaReceiptKey(t *testing.T, ns state.Namespace, attempt state.StageAttemptLocator) string {
	t.Helper()
	key, err := ns.Key("p", fmt.Sprintf("%02x", attempt.Partition), "stages", attempt.RequestID, attempt.StageID, attempt.AttemptID, "receipt")
	require.NoError(t, err)
	return key
}

func coldQuotaSlotKeys(t *testing.T, ns state.Namespace, ref state.ColdSlotReference) (string, string) {
	t.Helper()
	key, err := ns.Key("quota", fmt.Sprintf("%02x", ref.Partition), "slots", fmt.Sprintf("%08d", ref.ID))
	require.NoError(t, err)
	free, err := ns.Key("quota", fmt.Sprintf("%02x", ref.Partition), "free", fmt.Sprintf("%02x", ref.ID%16), fmt.Sprintf("%08d", ref.ID))
	require.NoError(t, err)
	return key, free
}

func coldQuotaAssertHeld(t *testing.T, ctx context.Context, raw *clientv3.Client, ns state.Namespace, r state.ColdPreparationRecord) {
	t.Helper()
	key, free := coldQuotaSlotKeys(t, ns, r.Quota)
	kv := coldQuotaReadOne(t, ctx, raw, key)
	var slot struct {
		Occupied bool                    `json:"occupied"`
		Owner    state.ColdSlotOwner     `json:"owner"`
		Slot     state.ColdSlotReference `json:"slot"`
	}
	require.NoError(t, json.Unmarshal(kv.Value, &slot))
	require.True(t, slot.Occupied)
	require.Equal(t, r.Owner, slot.Owner)
	require.Equal(t, r.Quota, slot.Slot)
	index, err := raw.Get(ctx, free)
	require.NoError(t, err)
	require.Empty(t, index.Kvs)
}
