//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

type dueWorkerNativeSandbox struct {
	candidate                    DueCandidateRecord
	dueKey, dueValue, controlKey string
}

// Three controlled sandboxes use actual intent/creation/enrolled-publication
// metadata. Test-issued certificates prove no kernel or physical runtime work.
func dueWorkerNativePublish(t *testing.T, ctx context.Context, b *Backend, root ed25519.PrivateKey, now time.Time, label string, ttl time.Duration, wanted *uint8) dueWorkerNativeSandbox {
	t.Helper()
	var identity Identity
	require.NoError(t, json.Unmarshal([]byte(b.identityValue), &identity))
	var workspace WorkspaceIdentity
	for i := 0; i < 4096; i++ {
		candidate, err := NewWorkspaceIdentity("s3", identity.StorageID, "bucket", fmt.Sprintf("due-resident/%s/%s/%d/", strings.Trim(b.namespace.Root(), "/"), label, i))
		require.NoError(t, err)
		if wanted == nil || candidate.Partition() == *wanted {
			workspace = candidate
			break
		}
	}
	require.NotEmpty(t, workspace.Hash(), "bounded workspace search must find the configured partition")
	in := AcquireIntentInput{Workspace: workspace, Principal: "due-worker-native", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"native":"resident-due"}`), Now: now, TTL: ttl}
	rpc, stop := partitionWorkerNativeRequest(ctx)
	acquired, err := b.AcquireIntent(rpc, in)
	stop()
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquired.Outcome)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	claim, err := b.ClaimCreation(rpc, workspace, in.IntentID, "due-worker-native", 30*time.Second)
	stop()
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = b.ReleaseCreationClaim(cleanup, claim)
	})
	rpc, stop = partitionWorkerNativeRequest(ctx)
	dispatch, err := b.DeclareRuntimeDispatch(rpc, claim, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "target", Payload: json.RawMessage(`{"native":"resident-due"}`)})
	stop()
	require.NoError(t, err)
	nativeRuntime := RuntimeReference{ID: "pod-" + label, UID: uuid.NewString(), BootID: uuid.NewString()}
	certificate := preparationCertificate(t, b, dispatch.Entry.Record, root, "plain", nativeRuntime)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	_, err = b.BindRuntime(rpc, claim, certificate)
	stop()
	require.NoError(t, err)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	_, err = b.ConsumeRuntimeMount(rpc, claim)
	stop()
	require.NoError(t, err)
	proof, enrollment := duePublicationMaterial(t, b, claim, root, certificate, nativeRuntime, now)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	published, err := b.PublishRuntimeEnrolled(rpc, claim, proof, enrollment)
	stop()
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	require.Equal(t, "runtime_publish_enrolled_v3", published.Reference.StageID)
	candidate := dueCandidateForControl(workspace.Partition(), claim.control)
	dueKey, err := b.namespace.dueCandidateKey(candidate)
	require.NoError(t, err)
	dueValue, err := encodeDueCandidate(candidate)
	require.NoError(t, err)
	controlKey, _, err := b.namespace.sandboxKeys(candidate.Partition, candidate.SandboxID, "read")
	require.NoError(t, err)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	err = b.ReleaseCreationClaim(rpc, claim)
	stop()
	require.NoError(t, err)
	return dueWorkerNativeSandbox{candidate: candidate, dueKey: dueKey, dueValue: dueValue, controlKey: controlKey}
}

func dueWorkerNativeGet(t *testing.T, ctx context.Context, raw *clientv3.Client, key string, options ...clientv3.OpOption) *clientv3.GetResponse {
	t.Helper()
	rpc, stop := partitionWorkerNativeRequest(ctx)
	defer stop()
	response, err := raw.Get(rpc, key, options...)
	require.NoError(t, err)
	require.NotNil(t, response.Header)
	return response
}

func dueWorkerNativeCheckpoint(t *testing.T, ctx context.Context, raw *clientv3.Client, b *Backend, partition uint8) dueCheckpointRecord {
	t.Helper()
	key, err := b.namespace.dueCheckpointKey(partition)
	require.NoError(t, err)
	response := dueWorkerNativeGet(t, ctx, raw, key)
	require.Len(t, response.Kvs, 1)
	record, err := b.decodeDueCheckpoint(response.Kvs[0], response.Header.Revision)
	require.NoError(t, err)
	return record
}

func dueWorkerNativeTasks(t *testing.T, ctx context.Context, raw *clientv3.Client, b *Backend, partition uint8) []TaskRecord {
	t.Helper()
	prefix := fmt.Sprintf("%sp/%02x/tasks/", b.namespace.Root(), partition)
	response := dueWorkerNativeGet(t, ctx, raw, prefix, clientv3.WithPrefix(), clientv3.WithLimit(64))
	require.False(t, response.More)
	var tasks []TaskRecord
	for _, kv := range response.Kvs {
		if strings.Contains(strings.TrimPrefix(string(kv.Key), prefix), "/") {
			continue
		}
		var task TaskRecord
		require.NoError(t, decodeTaskRecord(kv, &task))
		require.Zero(t, kv.Lease)
		tasks = append(tasks, task)
	}
	return tasks
}

func dueWorkerNativeWait(t *testing.T, ctx context.Context, predicate func() bool) {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	for {
		if predicate() {
			return
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			t.Fatalf("resident native chain expired: %v", ctx.Err())
		case <-deadline.C:
			timer.Stop()
			t.Fatal("resident native progress did not complete within eight seconds")
		case <-timer.C:
		}
	}
}

func dueWorkerNativeOriginal(t *testing.T, b *Backend, w *DueWorker) *WorkerSession {
	t.Helper()
	id := w.Status().Session.SessionID
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	for _, s := range b.workerControl.sessions {
		if s != nil && s.reference.SessionID == id {
			return s
		}
	}
	t.Fatalf("original resident session %s missing", id)
	return nil
}

func dueWorkerNativeJoined(t *testing.T, ctx context.Context, b *Backend, raw *clientv3.Client, w *DueWorker, s *WorkerSession) {
	t.Helper()
	workerTestReceive(t, w.Done())
	workerTestReceive(t, w.workDone)
	b.archiveLife.mu.Lock()
	call, stream := s.operation, s.dueWatch
	released, cleaned := s.released, s.cleaned
	b.archiveLife.mu.Unlock()
	require.Nil(t, call)
	require.Nil(t, stream)
	require.True(t, released)
	require.True(t, cleaned)
	require.Empty(t, dueWorkerNativeGet(t, ctx, raw, s.guard.key).Kvs)
	rpc, stop := partitionWorkerNativeRequest(ctx)
	ttl, err := raw.TimeToLive(rpc, clientv3.LeaseID(s.reference.LeaseID))
	stop()
	require.NoError(t, err)
	require.Equal(t, int64(-1), ttl.TTL, "once cleanup must revoke the actual original lease")
}

// TestDueWorkerNative is the sole fresh resident acceptance chain. It owns its
// three members through the tracked launcher, and leaves all process Waits to
// that launcher's once-only cleanup after actor/client work is physically joined.
func TestDueWorkerNative(t *testing.T) {
	if os.Getenv("SANDBOX_DUE_WORKER_NATIVE") != "1" {
		t.Skip("requires the fresh isolated due-worker native collector")
	}
	require.Equal(t, "arm64", runtime.GOARCH)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	endpoints := startPartitionWorkerNativeCluster(t, ctx)
	raw, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: time.Second, Logger: zap.NewNop()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	var cluster uint64
	members := make(map[uint64]bool, 3)
	for _, endpoint := range endpoints {
		rpc, stop := partitionWorkerNativeRequest(ctx)
		status, err := raw.Status(rpc, endpoint)
		stop()
		require.NoError(t, err)
		require.Equal(t, "3.6.14", status.Version)
		if cluster == 0 {
			cluster = status.Header.ClusterId
		}
		require.Equal(t, cluster, status.Header.ClusterId)
		members[status.Header.MemberId] = true
		t.Logf("resident native member endpoint=%s member=%d cluster=%d version=%s", endpoint, status.Header.MemberId, status.Header.ClusterId, status.Version)
	}
	require.Len(t, members, 3)
	prefix, scope, cell := "/sandbox/due-worker-native", "authority", uuid.NewString()
	namespace, err := NewNamespace(prefix, scope, cell)
	require.NoError(t, err)
	identity := Identity{SchemaVersion: 1, Prefix: prefix, AuthorityID: scope, Cell: cell, ClusterID: cluster, StorageID: "resident-storage", RuntimeID: "resident-runtime", RestoreEpoch: "resident-restore"}
	identityWire, err := json.Marshal(identity)
	require.NoError(t, err)
	_, root, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Now().UTC()
	var business atomic.Value
	business.Store(now)
	options := Options{Endpoints: endpoints, Namespace: namespace, Identity: identity, AllowInsecureLoopback: true, DialTimeout: time.Second, RequestTimeout: 3 * time.Second, WorkerControl: &WorkerControlOptions{SessionTTL: 15 * time.Second}, PublicationTrust: &RuntimePublicationTrust{AuthorityID: identity.RuntimeID, Target: "target", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, Clock: testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: business.Load().(time.Time), Uncertainty: time.Millisecond}, ctx.Err()
	}), EnrolledPublication: &EnrolledPublicationOptions{ContractDigest: strings.Repeat("b", 64), MaxActive: 4}}
	identityKey, _ := namespace.Key("meta", "identity")
	restoreKey, _ := namespace.Key("meta", "restore_epoch")
	profileKey, _ := namespace.Key("meta", "worker_control_budget")
	rpc, stop := partitionWorkerNativeRequest(ctx)
	_, err = raw.Txn(rpc).Then(clientv3.OpPut(identityKey, string(identityWire)), clientv3.OpPut(restoreKey, identity.RestoreEpoch), clientv3.OpPut(profileKey, workerTestProfile(options))).Commit()
	stop()
	require.NoError(t, err)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	b, err := New(rpc, options)
	stop()
	require.NoError(t, err)
	b.client.WithLogger(zap.NewNop())
	t.Cleanup(func() { require.NoError(t, b.Close()) })

	first := dueWorkerNativePublish(t, ctx, b, root, now, "first", time.Hour, nil)
	partition := first.candidate.Partition
	late := dueWorkerNativePublish(t, ctx, b, root, now, "late", 30*time.Minute, &partition)
	unknown := dueWorkerNativePublish(t, ctx, b, root, now, "unknown", 45*time.Minute, &partition)
	// Temporarily hide only discovery rows from the initial fixed R. Their
	// real ACTIVE bundles remain unchanged and will be revalidated by admission.
	rpc, stop = partitionWorkerNativeRequest(ctx)
	_, err = raw.Txn(rpc).Then(clientv3.OpDelete(late.dueKey), clientv3.OpDelete(unknown.dueKey)).Commit()
	stop()
	require.NoError(t, err)
	business.Store(first.candidate.ExpiresAt.Add(2 * time.Second))

	// Exactly 119 inert future candidates force 118+2 native pagination with
	// the one real overdue row. No synthetic row carries sandbox authority.
	var puts []clientv3.Op
	for i := 0; i < 119; i++ {
		candidate := first.candidate
		candidate.SandboxID, candidate.IntentID = fmt.Sprintf("synthetic-%03d", i), fmt.Sprintf("synthetic-intent-%03d", i)
		candidate.ExpiresAt = business.Load().(time.Time).Add(30 * time.Second)
		key, err := namespace.dueCandidateKey(candidate)
		require.NoError(t, err)
		wire, err := encodeDueCandidate(candidate)
		require.NoError(t, err)
		puts = append(puts, clientv3.OpPut(key, wire))
	}
	for start := 0; start < len(puts); start += 32 {
		end := min(start+32, len(puts))
		rpc, stop = partitionWorkerNativeRequest(ctx)
		_, err = raw.Txn(rpc).Then(puts[start:end]...).Commit()
		stop()
		require.NoError(t, err)
	}

	var pages, stageGrants atomic.Int32
	var firstPage, secondPage atomic.Int32
	var dropCommit atomic.Bool
	originalKV, originalLease := b.client.KV, b.client.Lease
	observed := &faultKV{KV: originalKV, match: duePageRangeOps, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if err == nil && response != nil && response.Succeeded {
			index := pages.Add(1)
			rows := int32(len(response.Responses[0].GetResponseRange().Kvs))
			if index == 1 {
				firstPage.Store(rows)
			}
			if index == 2 {
				secondPage.Store(rows)
			}
		}
		return response, err
	}}
	b.client.KV = &faultKV{KV: observed, match: putsKey(unknown.controlKey), after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if err == nil && response != nil && response.Succeeded && dropCommit.Swap(false) {
			return nil, context.DeadlineExceeded // Actual etcd committed; only its client acknowledgment is lost.
		}
		return response, err
	}}
	b.client.Lease = &workerTestLease{Lease: originalLease, grant: func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
		if ttl == 30 {
			stageGrants.Add(1)
		}
		return originalLease.Grant(ctx, ttl)
	}}
	workerOptions := DueWorkerOptions{WorkerID: "resident-native", Partitions: []uint8{partition}, RangeSlots: []uint16{0}, ClaimSlots: []uint16{0}, RelistSlot: 0, StageTTL: 30 * time.Second}
	start := func() *DueWorker {
		// This is the bounded resident lifetime, not a single RPC context.
		worker, err := b.StartDueWorker(ctx, workerOptions)
		require.NoError(t, err)
		t.Cleanup(func() { _ = worker.Stop() })
		return worker
	}
	w := start()
	s := dueWorkerNativeOriginal(t, b, w)
	dueWorkerNativeWait(t, ctx, func() bool {
		return w.Status().Synchronized && w.Status().Queued == 119 && len(dueWorkerNativeTasks(t, ctx, raw, b, partition)) == 1
	})
	require.Equal(t, int32(118), firstPage.Load())
	require.Equal(t, int32(2), secondPage.Load())
	checkpoint := dueWorkerNativeCheckpoint(t, ctx, raw, b, partition)
	require.Equal(t, first.dueKey, checkpoint.Frontier)
	require.GreaterOrEqual(t, w.Status().Partitions[0].CoveredRevision, checkpoint.Revision+1)
	baselinePages := pages.Load()
	tasks := dueWorkerNativeTasks(t, ctx, raw, b, partition)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	loaded, err := b.LoadTask(rpc, tasks[0].Reference)
	stop()
	require.NoError(t, err)
	require.Equal(t, first.candidate.ExpiresAt, loaded.ExpiresAt)
	require.Empty(t, dueWorkerNativeGet(t, ctx, raw, first.dueKey).Kvs)

	// The key precedes the settled frontier. A new native Watch event must
	// admit it without another Range or any mutation of the fixed-R frontier.
	require.Less(t, late.dueKey, checkpoint.Frontier)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	latePut, err := raw.Put(rpc, late.dueKey, late.dueValue)
	stop()
	require.NoError(t, err)
	dueWorkerNativeWait(t, ctx, func() bool {
		return w.Status().Synchronized && len(dueWorkerNativeTasks(t, ctx, raw, b, partition)) == 2 && w.Status().Partitions[0].CoveredRevision >= latePut.Header.Revision
	})
	require.Equal(t, baselinePages, pages.Load())
	require.Equal(t, checkpoint.Frontier, dueWorkerNativeCheckpoint(t, ctx, raw, b, partition).Frontier)
	t.Logf("resident native tasks=2 fixed_R=%d covered=%d observed=%d late_revision=%d pages=%d queued=%d", checkpoint.Revision, w.Status().Partitions[0].CoveredRevision, w.Status().Partitions[0].ObservedRevision, latePut.Header.Revision, pages.Load(), w.Status().Queued)
	_ = w.Stop()
	dueWorkerNativeJoined(t, ctx, b, raw, w, s)

	w2 := start()
	s2 := dueWorkerNativeOriginal(t, b, w2)
	require.NotEqual(t, s.reference.SessionID, s2.reference.SessionID)
	dueWorkerNativeWait(t, ctx, func() bool { return w2.Status().Synchronized })
	dropCommit.Store(true)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	_, err = raw.Put(rpc, unknown.dueKey, unknown.dueValue)
	stop()
	require.NoError(t, err)
	dueWorkerNativeWait(t, ctx, func() bool { return !w2.Status().Running })
	dueWorkerNativeJoined(t, ctx, b, raw, w2, s2)
	require.Equal(t, DueWorkerPhase("unknown"), w2.Status().Phase)
	pending := dueWorkerNativeCheckpoint(t, ctx, raw, b, partition)
	require.NotNil(t, pending.Pending)
	require.Equal(t, unknown.candidate, pending.Pending.Candidate)
	require.Len(t, dueWorkerNativeTasks(t, ctx, raw, b, partition), 3)
	grantCount := stageGrants.Load()
	require.Equal(t, int32(3), grantCount)
	_, receiptKey, err := b.stageKeys(pending.Pending.Reference)
	require.NoError(t, err)
	receiptBefore := dueWorkerNativeGet(t, ctx, raw, receiptKey)
	require.Len(t, receiptBefore.Kvs, 1)

	// The replacement ctor resolves this exact persisted reference under new
	// original fences, before any new Stage Grant or any resumed scheduling.
	w3 := start()
	s3 := dueWorkerNativeOriginal(t, b, w3)
	require.NotEqual(t, s2.reference.SessionID, s3.reference.SessionID)
	dueWorkerNativeWait(t, ctx, func() bool { return w3.Status().Synchronized })
	recovered := dueWorkerNativeCheckpoint(t, ctx, raw, b, partition)
	require.Nil(t, recovered.Pending)
	require.Equal(t, s3.reference.SessionID, recovered.WriterSessionID)
	require.Equal(t, pending.Frontier, recovered.Frontier)
	require.Equal(t, grantCount, stageGrants.Load())
	require.Equal(t, receiptBefore.Kvs, dueWorkerNativeGet(t, ctx, raw, receiptKey).Kvs)
	require.Len(t, dueWorkerNativeTasks(t, ctx, raw, b, partition), 3)

	// Rewriting the exact leased original fence preserves its value and lease
	// but changes MVCC authority. The actor must stop, never reconstruct it.
	fence := dueWorkerNativeGet(t, ctx, raw, s3.partitions[partition].fence.key)
	require.Len(t, fence.Kvs, 1)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	_, err = raw.Put(rpc, string(fence.Kvs[0].Key), string(fence.Kvs[0].Value), clientv3.WithLease(clientv3.LeaseID(fence.Kvs[0].Lease)))
	stop()
	require.NoError(t, err)
	dueWorkerNativeWait(t, ctx, func() bool { return !w3.Status().Running })
	dueWorkerNativeJoined(t, ctx, b, raw, w3, s3)
	require.False(t, w3.Status().Synchronized)
	require.NotEmpty(t, w3.Status().LastError)
	require.Equal(t, grantCount, stageGrants.Load())
	finalTasks := dueWorkerNativeTasks(t, ctx, raw, b, partition)
	require.Len(t, finalTasks, 3)
	bySandbox := make(map[string]int, 3)
	for _, task := range finalTasks {
		bySandbox[task.Reference.SandboxID]++
	}
	for _, sandbox := range []dueWorkerNativeSandbox{first, late, unknown} {
		require.Equal(t, 1, bySandbox[sandbox.candidate.SandboxID])
		require.Empty(t, dueWorkerNativeGet(t, ctx, raw, sandbox.dueKey).Kvs)
	}
	require.NoError(t, b.Close())
	t.Logf("resident native complete: three original sessions/leases physically joined; three permanent metadata tasks; synthetic_rows=119; physical_execution=false capacity_claim=false")
}
