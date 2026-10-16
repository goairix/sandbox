//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
)

// One fresh finite three-member chain. Synthetic pagination records below
// are discovery-only test data and never establish active sandbox authority.
func TestDueDiscoveryNative(t *testing.T) {
	if os.Getenv("SANDBOX_DUE_DISCOVERY_NATIVE") != "1" {
		t.Skip("requires the fresh owned three-member due fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	endpoints := startPartitionWorkerNativeCluster(t, ctx)
	raw, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: time.Second, Logger: zap.NewNop()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	var cluster uint64
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
		t.Logf("due native member endpoint=%s member_id=%d cluster_id=%d leader=%d version=%s", endpoint, status.Header.MemberId, status.Header.ClusterId, status.Leader, status.Version)
	}
	prefix, scope, cell := "/sandbox/due-discovery-native", "authority", uuid.NewString()
	n, err := NewNamespace(prefix, scope, cell)
	require.NoError(t, err)
	identity := Identity{SchemaVersion: 1, Prefix: prefix, AuthorityID: scope, Cell: cell, ClusterID: cluster, StorageID: "due-storage", RuntimeID: "due-runtime", RestoreEpoch: "due-restore"}
	identityWire, err := json.Marshal(identity)
	require.NoError(t, err)
	_, root, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	now := time.Now().UTC()
	var business atomic.Value
	business.Store(now)
	o := Options{Endpoints: endpoints, Namespace: n, Identity: identity, AllowInsecureLoopback: true, DialTimeout: time.Second, RequestTimeout: 3 * time.Second, WorkerControl: &WorkerControlOptions{SessionTTL: 15 * time.Second}, PublicationTrust: &RuntimePublicationTrust{AuthorityID: identity.RuntimeID, Target: "target", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, Clock: testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: business.Load().(time.Time), Uncertainty: time.Millisecond}, ctx.Err()
	}), EnrolledPublication: &EnrolledPublicationOptions{ContractDigest: strings.Repeat("b", 64), MaxActive: 4}}
	identityKey, _ := n.Key("meta", "identity")
	restoreKey, _ := n.Key("meta", "restore_epoch")
	profileKey, _ := n.Key("meta", "worker_control_budget")
	for _, value := range []struct{ key, value string }{{identityKey, string(identityWire)}, {restoreKey, identity.RestoreEpoch}, {profileKey, workerTestProfile(o)}} {
		rpc, stop := partitionWorkerNativeRequest(ctx)
		_, err := raw.Put(rpc, value.key, value.value)
		stop()
		require.NoError(t, err)
	}
	b, err := New(ctx, o)
	require.NoError(t, err)
	b.client.WithLogger(zap.NewNop())
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	w, err := NewWorkspaceIdentity("s3", identity.StorageID, "bucket", cell+"/")
	require.NoError(t, err)
	in := AcquireIntentInput{Workspace: w, Principal: "due-native", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"native":"due"}`), Now: now, TTL: time.Hour}
	acquired, err := b.AcquireIntent(ctx, in)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquired.Outcome)
	creation, err := b.ClaimCreation(ctx, w, in.IntentID, "due-publisher", 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		require.NoError(t, b.ReleaseCreationClaim(cleanup, creation))
	})
	dispatch, err := b.DeclareRuntimeDispatch(ctx, creation, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "target", Payload: json.RawMessage(`{"native":"due"}`)})
	require.NoError(t, err)
	runtime := RuntimeReference{ID: "pod", UID: uuid.NewString(), BootID: uuid.NewString()}
	certificate := preparationCertificate(t, b, dispatch.Entry.Record, root, "plain", runtime)
	_, err = b.BindRuntime(ctx, creation, certificate)
	require.NoError(t, err)
	_, err = b.ConsumeRuntimeMount(ctx, creation)
	require.NoError(t, err)
	proof, enrollment := duePublicationMaterial(t, b, creation, root, certificate, runtime, now)
	published, err := b.PublishRuntimeEnrolled(ctx, creation, proof, enrollment)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	require.Equal(t, "runtime_publish_enrolled_v3", published.Reference.StageID)
	candidate := dueCandidateForControl(w.Partition(), creation.control)
	dueKey, err := n.dueCandidateKey(candidate)
	require.NoError(t, err)
	controlKey, _, err := n.sandboxKeys(w.Partition(), in.SandboxID, "read")
	require.NoError(t, err)
	get := func(key string) *clientv3.GetResponse {
		rpc, stop := partitionWorkerNativeRequest(ctx)
		defer stop()
		r, err := raw.Get(rpc, key)
		require.NoError(t, err)
		return r
	}
	due, control := get(dueKey), get(controlKey)
	require.Len(t, due.Kvs, 1)
	require.Len(t, control.Kvs, 1)
	require.Equal(t, control.Kvs[0].ModRevision, due.Kvs[0].CreateRevision)
	require.Zero(t, due.Kvs[0].Lease)
	journal, _, err := n.runtimePublicationKeys(w.Partition(), in.IntentID)
	require.NoError(t, err)
	require.Equal(t, due.Kvs[0].CreateRevision, get(journal).Kvs[0].CreateRevision)
	t.Logf("due native atomic publication revision=%d index_key_bytes=%d index_value_bytes=%d", due.Kvs[0].CreateRevision, len(due.Kvs[0].Key), len(due.Kvs[0].Value))
	openWorker := func(worker string) (*WorkerSession, *WorkerPartitionClaim, *WorkerBudgetShare, *WorkerBudgetShare) {
		s, err := b.OpenWorkerSession(ctx, worker)
		require.NoError(t, err)
		p, err := b.ClaimWorkerPartition(ctx, s, w.Partition())
		require.NoError(t, err)
		ranges, err := b.AcquireWorkerBudget(ctx, s, WorkerBudgetRange, 0)
		require.NoError(t, err)
		claims, err := b.AcquireWorkerBudget(ctx, s, WorkerBudgetClaim, 0)
		require.NoError(t, err)
		return s, p, ranges, claims
	}
	s, p, ranges, claims := openWorker("due-original")
	dueNativeSample(t, ctx, raw, n, "one-active-sandbox-before-synthetic")
	business.Store(candidate.ExpiresAt.Add(2 * time.Second))
	partitionWorkerNativePause(t, ctx, 1100*time.Millisecond)
	originalKV := b.client.KV
	b.client.KV = &faultKV{KV: originalKV, match: duePageRangeOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if err == nil && r != nil && r.Succeeded {
			rr := r.Responses[0].GetResponseRange()
			t.Logf("due native Range outer_revision=%d nested=%+v count=%d returned=%d more=%t", r.Header.Revision, rr.Header, rr.Count, len(rr.Kvs), rr.More)
		}
		return r, err
	}}
	page, err := b.ReadDuePage(ctx, p, ranges, nil)
	require.NoError(t, err)
	require.Equal(t, []DueCandidateRecord{candidate}, page.Candidates)
	// Normal metadata competition does not retire a healthy worker. Resolve
	// the refused original attempt before explicitly starting another one.
	b.client.KV = &faultKV{KV: originalKV, match: putsKey(controlKey), before: func() {
		r := get(controlKey)
		rpc, stop := partitionWorkerNativeRequest(ctx)
		_, err := raw.Put(rpc, controlKey, string(r.Kvs[0].Value))
		stop()
		require.NoError(t, err)
	}}
	refused, err := b.AdmitExpiredTask(ctx, p, claims, candidate, uuid.NewString(), 30*time.Second)
	require.ErrorIs(t, err, ErrConflict)
	require.Equal(t, OutcomeUnknown, refused.Outcome)
	b.client.KV = originalKV
	require.NoError(t, b.RenewWorkerSession(ctx, s))
	outcome, err := b.ResolveStage(ctx, refused.Reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	partitionWorkerNativePause(t, ctx, 1100*time.Millisecond)
	// Rewriting this exact original leased partition fence invalidates the
	// actual admission transaction. No cached candidate repairs the claim.
	b.client.KV = &faultKV{KV: originalKV, match: putsKey(controlKey), before: func() {
		r := get(p.fence.key)
		rpc, stop := partitionWorkerNativeRequest(ctx)
		_, err := raw.Put(rpc, p.fence.key, string(r.Kvs[0].Value), clientv3.WithLease(clientv3.LeaseID(r.Kvs[0].Lease)))
		stop()
		require.NoError(t, err)
	}}
	denied, err := b.AdmitExpiredTask(ctx, p, claims, candidate, uuid.NewString(), 30*time.Second)
	require.Error(t, err)
	require.Equal(t, OutcomeUnknown, denied.Outcome)
	require.Len(t, get(dueKey).Kvs, 1)
	b.archiveLife.mu.Lock()
	lost := s.lost
	b.archiveLife.mu.Unlock()
	require.True(t, lost)
	b.client.KV = originalKV
	require.NoError(t, b.ReleaseWorkerSession(ctx, s))
	outcome, err = b.ResolveStage(ctx, denied.Reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeAborted, outcome)
	s, p, ranges, claims = openWorker("due-replacement")
	partitionWorkerNativePause(t, ctx, 1100*time.Millisecond)
	expired, err := b.AdmitExpiredTask(ctx, p, claims, candidate, uuid.NewString(), 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, expired.GuardCleanupError)
	require.Equal(t, OutcomeCommitted, expired.Outcome)
	require.Empty(t, get(dueKey).Kvs)
	current := get(controlKey)
	var destroying SandboxControlRecord
	require.NoError(t, decodeDomainRecord(current.Kvs[0], &destroying))
	require.Equal(t, PhaseDestroying, destroying.Phase)
	for _, key := range taskDestroyKeys(t, b, expired.Task) {
		r := get(key)
		require.Len(t, r.Kvs, 1)
		require.Equal(t, current.Kvs[0].ModRevision, r.Kvs[0].CreateRevision)
		require.Zero(t, r.Kvs[0].Lease)
	}
	task, err := b.LoadTask(ctx, expired.Task)
	require.NoError(t, err)
	require.Equal(t, candidate.ExpiresAt, task.ExpiresAt)
	t.Logf("due native expired metadata task=%s publication_revision=%d task_birth=%d original_attempt=%s", expired.Task.TaskID, due.Kvs[0].CreateRevision, current.Kvs[0].ModRevision, expired.Reference.AttemptID)
	dueNativeSample(t, ctx, raw, n, "one-metadata-task-sandbox-before-synthetic")
	// Page more than118 inert candidates with actual native fixed-revision
	// headers/count semantics, then change current MVCC while continuing R.
	require.NoError(t, b.RenewWorkerSession(ctx, s))
	var synthetic []DueCandidateRecord
	var puts []clientv3.Op
	for i := 0; i < 125; i++ {
		r := candidate
		r.SandboxID = fmt.Sprintf("synthetic-%03d", i)
		r.IntentID = fmt.Sprintf("synthetic-intent-%03d", i)
		r.ExpiresAt = business.Load().(time.Time).Add(-time.Second)
		key, err := n.dueCandidateKey(r)
		require.NoError(t, err)
		wire, err := encodeDueCandidate(r)
		require.NoError(t, err)
		synthetic = append(synthetic, r)
		puts = append(puts, clientv3.OpPut(key, wire))
	}
	for start := 0; start < len(puts); start += 32 {
		end := start + 32
		if end > len(puts) {
			end = len(puts)
		}
		rpc, stop := partitionWorkerNativeRequest(ctx)
		r, err := raw.Txn(rpc).Then(puts[start:end]...).Commit()
		stop()
		require.NoError(t, err)
		require.True(t, r.Succeeded)
	}
	partitionWorkerNativePause(t, ctx, 300*time.Millisecond)
	b.client.KV = &faultKV{KV: originalKV, match: duePageRangeOps, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if err == nil && r != nil && r.Succeeded {
			rr := r.Responses[0].GetResponseRange()
			t.Logf("due native fixed-view outer=%d nested=%+v count=%d returned=%d more=%t", r.Header.Revision, rr.Header, rr.Count, len(rr.Kvs), rr.More)
		}
		return r, err
	}}
	first, err := b.ReadDuePage(ctx, p, ranges, nil)
	require.NoError(t, err)
	require.Equal(t, synthetic[:118], first.Candidates)
	require.NotNil(t, first.Next)
	additional := synthetic[124]
	additional.SandboxID = "synthetic-999"
	key, _ := n.dueCandidateKey(additional)
	wire, _ := encodeDueCandidate(additional)
	rpc, stop := partitionWorkerNativeRequest(ctx)
	_, err = raw.Put(rpc, key, wire)
	stop()
	require.NoError(t, err)
	key, _ = n.dueCandidateKey(synthetic[124])
	wire, _ = encodeDueCandidate(synthetic[124])
	rpc, stop = partitionWorkerNativeRequest(ctx)
	_, err = raw.Put(rpc, key, wire)
	stop()
	require.NoError(t, err)
	partitionWorkerNativePause(t, ctx, 300*time.Millisecond)
	last, err := b.ReadDuePage(ctx, p, ranges, first.Next)
	require.NoError(t, err)
	require.Equal(t, first.Revision, last.Revision)
	require.Equal(t, synthetic[118:], last.Candidates)
	require.Nil(t, last.Next)
	partitionWorkerNativePause(t, ctx, 300*time.Millisecond)
	compactView, err := b.ReadDuePage(ctx, p, ranges, nil)
	require.NoError(t, err)
	require.NotNil(t, compactView.Next)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	marker, err := raw.Put(rpc, n.Root()+"fixture-compaction-marker", "test-only")
	stop()
	require.NoError(t, err)
	rpc, stop = partitionWorkerNativeRequest(ctx)
	_, err = raw.Compact(rpc, marker.Header.Revision, clientv3.WithCompactPhysical())
	stop()
	require.NoError(t, err)
	partitionWorkerNativePause(t, ctx, 300*time.Millisecond)
	invalid, err := b.ReadDuePage(ctx, p, ranges, compactView.Next)
	require.True(t, errors.Is(err, rpctypes.ErrCompacted), "actual compaction invalidates fixed view: %v", err)
	require.Empty(t, invalid.Candidates)
	require.Nil(t, invalid.Next)
	t.Logf("due native expected compaction error=%v", err)
	t.Logf("due native fixed_revision=%d returned_pages=118+7 concurrent_extra_excluded=1 compacted_view=%d", first.Revision, compactView.Revision)
}

func dueNativeSample(t *testing.T, ctx context.Context, raw *clientv3.Client, n Namespace, label string) {
	t.Helper()
	rpc, stop := partitionWorkerNativeRequest(ctx)
	defer stop()
	r, err := raw.Get(rpc, n.Root(), clientv3.WithPrefix(), clientv3.WithLimit(128))
	require.NoError(t, err)
	require.False(t, r.More)
	require.Equal(t, int64(len(r.Kvs)), r.Count)
	require.LessOrEqual(t, len(r.Kvs), 128)
	native := pb.RangeResponse(*r)
	require.LessOrEqual(t, native.Size(), maxMutationBytes)
	var permanent, leased, keyBytes, valueBytes int
	for _, kv := range r.Kvs {
		if kv.Lease == 0 {
			permanent++
		} else {
			leased++
		}
		keyBytes += len(kv.Key)
		valueBytes += len(kv.Value)
	}
	t.Logf("due native byte sample=%s revision=%d latest_keys=%d permanent_keys=%d leased_keys=%d key_bytes=%d value_bytes=%d logical_total_bytes=%d includes_namespace_fixed_profiles_history_and_one_controlled_sandbox=true synthetic_rows=0 production_average=false capacity_claim=false", label, r.Header.Revision, len(r.Kvs), permanent, leased, keyBytes, valueBytes, keyBytes+valueBytes)
}
