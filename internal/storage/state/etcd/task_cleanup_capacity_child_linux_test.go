//go:build linux && (amd64 || arm64)

package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// Native scale is opt-in and covers only the View/Snapshot/scheduling typed
// metadata subset. Pilot mode validates the harness for 60 seconds at 10k;
// tier evidence requires one hour of idle observation plus maintenance and
// member recovery. These rows neither model complete B_live nor authorize
// Pod/FUSE/runtime side effects or describe N actually usable sandboxes.
func TestMetadataScaleNative(t *testing.T) {
	if os.Getenv("SANDBOX_METADATA_SCALE_NATIVE") != "1" {
		t.Skip("explicit owned disk-backed three-member metadata scale fixture required")
	}
	root := os.Getenv("SANDBOX_METADATA_SCALE_DATA")
	require.True(t, filepath.IsAbs(root), "runner must provide an absolute owned disk-backed data directory")
	var disk syscall.Statfs_t
	require.NoError(t, syscall.Statfs(root, &disk))
	require.NotEqual(t, int64(unix.TMPFS_MAGIC), int64(disk.Type), "100k model data must not live in memory-backed tmpfs")
	require.GreaterOrEqual(t, uint64(disk.Bavail)*uint64(disk.Bsize), uint64(20<<30), "reserve20GiB free before creating the disposable cluster")
	data, err := os.MkdirTemp(root, "metadata-scale-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(data)) })
	pilot := os.Getenv("SANDBOX_METADATA_SCALE_PILOT") == "1"
	duration := time.Hour
	tiers := []int{10000, 100000}
	if pilot {
		duration, tiers = time.Minute, []int{10000}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(tiers))*duration+time.Hour)
	t.Cleanup(cancel)
	endpoints, children := startPartitionWorkerNativeClusterWithData(t, ctx, data, 4<<30)
	raw, err := clientv3.New(clientv3.Config{Endpoints: endpoints, DialTimeout: time.Second, Logger: zap.NewNop()})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	bounded, stop := partitionWorkerNativeRequest(ctx)
	members, err := raw.MemberList(bounded)
	stop()
	require.NoError(t, err)
	require.Len(t, members.Members, 3)
	namespace, err := NewNamespace("/codex-scale", "metadata", uuid.NewString())
	require.NoError(t, err)
	identity := Identity{SchemaVersion: 1, Prefix: namespace.prefix, AuthorityID: namespace.scope, Cell: namespace.cell, ClusterID: members.Header.ClusterId, StorageID: "test-storage", RuntimeID: "test-runtime", RestoreEpoch: "test-epoch"}
	identityWire, err := json.Marshal(identity)
	require.NoError(t, err)
	identityKey, err := namespace.Key("meta", "identity")
	require.NoError(t, err)
	restoreKey, err := namespace.Key("meta", "restore_epoch")
	require.NoError(t, err)
	bounded, stop = partitionWorkerNativeRequest(ctx)
	provisioned, err := raw.Txn(bounded).If(clientv3.Compare(clientv3.CreateRevision(identityKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(restoreKey), "=", 0)).Then(clientv3.OpPut(identityKey, string(identityWire)), clientv3.OpPut(restoreKey, identity.RestoreEpoch)).Commit()
	stop()
	require.NoError(t, err)
	require.True(t, provisioned.Succeeded)
	// The original operation admission clock requires its matching immutable
	// publication trust. This fixture root signs no model rows or effects.
	publicationRoot := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x73}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	options := Options{Endpoints: endpoints, Namespace: namespace, Identity: identity, AllowInsecureLoopback: true, RequestTimeout: 2 * time.Second, Clock: goodAuthorityClock(), PublicationTrust: &RuntimePublicationTrust{AuthorityID: identity.RuntimeID, Target: "kubernetes", Roots: []ed25519.PublicKey{publicationRoot}}, WorkerControl: &WorkerControlOptions{SessionTTL: time.Minute}, SnapshotCacheBytes: 256 << 10}
	// This whole cluster is disposable. Do not issue an unbounded 900k-key
	// namespace delete at teardown: join every original member, then remove
	// only the owned data directory registered before those process owners.
	base, err := New(ctx, Options{Endpoints: endpoints, Namespace: namespace, Identity: identity, AllowInsecureLoopback: true})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	profileKey, err := namespace.Key("meta", "worker_control_budget")
	require.NoError(t, err)
	bounded, stop = partitionWorkerNativeRequest(ctx)
	profile, err := raw.Txn(bounded).If(base.baseComparisons()...).Then(clientv3.OpPut(profileKey, workerTestProfile(options))).Commit()
	stop()
	require.NoError(t, err)
	require.True(t, profile.Succeeded)
	seed, err := New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, seed.Close()) })
	expiry := time.Now().UTC().Add(30 * 24 * time.Hour)
	previous := 0
	var baseline metadataScaleObservation
	for _, count := range tiers {
		seedStart := time.Now()
		digest := sha256.New()
		var seedBytes, requestBytes uint64
		for start := previous; start < count; start += 3 {
			var writes []Write
			for ordinal := start; ordinal < start+3 && ordinal < count; ordinal++ {
				model, _, err := metadataScaleModel(seed.namespace, ordinal, expiry)
				require.NoError(t, err)
				for _, write := range model {
					_, _ = fmt.Fprintf(digest, "%s\x00%s\x00", write.Key, write.Value)
					seedBytes += uint64(len(write.Key) + len(write.Value))
				}
				writes = append(writes, model...)
			}
			request, err := metadataScaleSeedRequest(seed, writes)
			require.NoError(t, err)
			requestBytes += uint64(request.Size())
			cmps := seed.baseComparisons()
			ops := make([]clientv3.Op, 0, len(writes))
			for _, write := range writes {
				cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(write.Key), "=", 0))
				ops = append(ops, clientv3.OpPut(write.Key, string(write.Value)))
			}
			bounded, stop := partitionWorkerNativeRequest(ctx)
			response, err := raw.Txn(bounded).If(cmps...).Then(ops...).Else(clientv3.OpGet(seed.identityKey), clientv3.OpGet(seed.restoreKey)).Commit()
			stop()
			require.NoError(t, err)
			require.True(t, response.Succeeded)
			require.NoError(t, seed.operationResponseHeader(response))
			require.Len(t, response.Responses, len(writes))
		}
		t.Logf("METADATA_SCALE seed N=%d added=%d subset_keys=%d B_model_subset_added_key_value_bytes=%d request_protobuf_bytes=%d seed_sha256=%x elapsed=%s", count, count-previous, 9*(count-previous), seedBytes, requestBytes, digest.Sum(nil), time.Since(seedStart))
		before := metadataScaleControlDigest(t, ctx, raw, seed.namespace, count)
		observation := metadataScaleIdle(t, ctx, raw, options, count, duration, pilot, children, endpoints)
		require.Equal(t, before, metadataScaleControlDigest(t, ctx, raw, seed.namespace, count), "idle/maintenance must not rewrite any model control or its native envelope")
		if previous != 0 {
			require.LessOrEqual(t, observation.transactions, baseline.transactions*2+1024, "10xN must not produce10x short-period worker transactions; long audit/source reads are reported separately")
			require.LessOrEqual(t, observation.keeps, baseline.keeps*2+8, "worker session renewal must not become per-sandbox renewal")
		}
		baseline, previous = observation, count
	}
	if pilot {
		t.Log("METADATA_SCALE PILOT ONLY:10k/60s harness; no1h tier, maintenance/fault,100k, Pod/FUSE or HTTP SLO acceptance")
	} else {
		t.Log("METADATA_SCALE View/Snapshot/scheduling typed subset10k/100k tiers finished; not completeB_live/cellcapacity,1M/multi-cell/24h, actualN sandboxes/Pods/FUSE, creation throughput or HTTP SLO evidence")
	}
}

type metadataScaleObservation struct {
	transactions, ranges, mutations, controlWrites, responseBytes, grants, keeps int64
}

// This observer never changes a result or a request. Unary response bytes are
// exact protobuf bytes; Watch/network totals come from the real server metrics.
func metadataScaleIdle(t *testing.T, ctx context.Context, raw *clientv3.Client, options Options, count int, duration time.Duration, pilot bool, children []*partitionWorkerNativeProcess, endpoints []string) metadataScaleObservation {
	t.Helper()
	b, err := New(ctx, options)
	require.NoError(t, err)
	defer b.Close()
	var transactions, ranges, mutations, controlWrites, responseBytes atomic.Int64
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
		transactions.Add(1)
		for _, op := range ops {
			if op.IsGet() {
				ranges.Add(1)
			} else {
				mutations.Add(1)
				if strings.Contains(string(op.KeyBytes()), "/controls/") {
					controlWrites.Add(1)
				}
			}
		}
		return true
	}, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if response != nil {
			responseBytes.Add(int64((*pb.TxnResponse)(response).Size()))
		}
		return response, err
	}}
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	partitions := make([]uint8, 256)
	for i := range partitions {
		partitions[i] = uint8(i)
	}
	claims := make([]uint16, 20)
	for i := range claims {
		claims[i] = uint16(i)
	}
	// Twenty fixed original shares fund 256 initial checkpoint turns without
	// mutating token buckets. Both N tiers receive exactly this same allocation.
	w, err := b.StartDueWorker(ctx, DueWorkerOptions{WorkerID: fmt.Sprintf("metadata-scale-%d", count), Partitions: partitions, RangeSlots: []uint16{0}, ClaimSlots: claims, RelistSlot: 0, StageTTL: 30 * time.Second})
	require.NoError(t, err)
	defer func() { require.NoError(t, w.Stop()); <-w.Done() }()
	start := time.Now()
	for !w.Status().Synchronized {
		require.Less(t, time.Since(start), 3*time.Minute, "initial real bounded discovery failed to synchronize")
		partitionWorkerNativePause(t, ctx, 100*time.Millisecond)
	}
	metadataScaleSample(t, ctx, b, options, count)
	t.Logf("METADATA_SCALE startup/sample N=%d elapsed=%s txns=%d grants=%d keeps=%d", count, time.Since(start), transactions.Load(), lease.grants.Load(), lease.keeps.Load())
	metadataScaleMetrics(t, ctx, raw, endpoints, count, "idle-before")
	runtime.GC() // outside the measured idle interval, after seed temporaries returned
	prior := metadataScaleObservation{transactions: transactions.Load(), ranges: ranges.Load(), mutations: mutations.Load(), controlWrites: controlWrites.Load(), responseBytes: responseBytes.Load(), grants: lease.grants.Load(), keeps: lease.keeps.Load()}
	// All model expirations are outside the current short scheduling window.
	// This isolates ordinary idle cost; it does not exercise a full N-row audit
	// every window or claim throughput under N simultaneous expirations.
	start = time.Now()
	maintained := false
	for time.Since(start) < duration {
		partitionWorkerNativePause(t, ctx, time.Second)
		status := w.Status()
		require.True(t, status.Running, "%+v", status)
		require.Empty(t, status.LastError)
		require.LessOrEqual(t, status.OwnedBytes, uint64(64<<20))
		require.LessOrEqual(t, status.Queued, 10000)
		if !pilot && !maintained && time.Since(start) >= duration/2 {
			maintenanceStart := time.Now()
			maintenanceBefore := metadataScaleObservation{transactions.Load(), ranges.Load(), mutations.Load(), controlWrites.Load(), responseBytes.Load(), lease.grants.Load(), lease.keeps.Load()}
			metadataScaleMaintenance(t, ctx, raw, endpoints, children, b, options, count)
			t.Logf("METADATA_SCALE maintenance-only N=%d elapsed=%s txns=%d ranges=%d mutations=%d control_writes=%d response_pb=%d grants=%d keeps=%d", count, time.Since(maintenanceStart), transactions.Load()-maintenanceBefore.transactions, ranges.Load()-maintenanceBefore.ranges, mutations.Load()-maintenanceBefore.mutations, controlWrites.Load()-maintenanceBefore.controlWrites, responseBytes.Load()-maintenanceBefore.responseBytes, lease.grants.Load()-maintenanceBefore.grants, lease.keeps.Load()-maintenanceBefore.keeps)
			prior.transactions += transactions.Load() - maintenanceBefore.transactions
			prior.ranges += ranges.Load() - maintenanceBefore.ranges
			prior.mutations += mutations.Load() - maintenanceBefore.mutations
			prior.controlWrites += controlWrites.Load() - maintenanceBefore.controlWrites
			prior.responseBytes += responseBytes.Load() - maintenanceBefore.responseBytes
			prior.grants += lease.grants.Load() - maintenanceBefore.grants
			prior.keeps += lease.keeps.Load() - maintenanceBefore.keeps
			// Maintenance/fault work is reported separately. It cannot shorten
			// the required one-hour idle observation or count as idle operations.
			start = start.Add(time.Since(maintenanceStart))
			maintained = true
		}
	}
	result := metadataScaleObservation{transactions.Load() - prior.transactions, ranges.Load() - prior.ranges, mutations.Load() - prior.mutations, controlWrites.Load() - prior.controlWrites, responseBytes.Load() - prior.responseBytes, lease.grants.Load() - prior.grants, lease.keeps.Load() - prior.keeps}
	require.Zero(t, result.controlWrites)
	require.Zero(t, result.grants, "idle cannot grant sandbox leases")
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	rss, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)
	for _, line := range strings.Split(string(rss), "\n") {
		if strings.HasPrefix(line, "VmRSS:") || strings.HasPrefix(line, "VmHWM:") {
			t.Logf("METADATA_SCALE N=%d process %s", count, line)
		}
	}
	status := w.Status()
	t.Logf("METADATA_SCALE idle N=%d elapsed=%s transaction/range/mutation/control_write/response_pb/grant/keep=%+v heap_alloc=%d heap_sys=%d total_alloc=%d goroutines=%d worker_phase=%s worker_owned=%d worker_queued=%d worker_renewals=%d", count, time.Since(start), result, memory.HeapAlloc, memory.HeapSys, memory.TotalAlloc, runtime.NumGoroutine(), status.Phase, status.OwnedBytes, status.Queued, status.Renewals)
	metadataScaleMetrics(t, ctx, raw, endpoints, count, "idle-after")
	return result
}

// Verification scans are outside idle accounting. Retain at most 118 controls
// at one fixed revision, never an N-row map or a complete snapshot mirror.
func metadataScaleControlDigest(t *testing.T, ctx context.Context, raw *clientv3.Client, namespace Namespace, count int) string {
	t.Helper()
	hash := sha256.New()
	rows, revision := 0, int64(0)
	for partition := 0; partition < 256; partition++ {
		prefix := fmt.Sprintf("%sp/%02x/controls/", namespace.Root(), partition)
		cursor, end := prefix, clientv3.GetPrefixRangeEnd(prefix)
		for {
			bounded, stop := partitionWorkerNativeRequest(ctx)
			response, err := raw.Get(bounded, cursor, clientv3.WithRange(end), clientv3.WithLimit(118), clientv3.WithRev(revision), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
			stop()
			require.NoError(t, err)
			require.LessOrEqual(t, (*pb.RangeResponse)(response).Size(), 256<<10)
			if revision == 0 {
				revision = response.Header.Revision
			}
			for _, kv := range response.Kvs {
				require.Zero(t, kv.Lease)
				require.EqualValues(t, 1, kv.Version)
				require.Equal(t, kv.CreateRevision, kv.ModRevision)
				_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%d/%d/%d/%d\x00", kv.Key, kv.Value, kv.CreateRevision, kv.ModRevision, kv.Version, kv.Lease)
				rows++
			}
			if !response.More {
				break
			}
			require.NotEmpty(t, response.Kvs)
			cursor = string(response.Kvs[len(response.Kvs)-1].Key) + "\x00"
		}
	}
	require.Equal(t, count, rows)
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func metadataScaleSample(t *testing.T, ctx context.Context, b *Backend, options Options, count int) {
	t.Helper()
	identity, err := decodeIdentity(b.identityValue, b.namespace)
	require.NoError(t, err)
	identity.RestoreEpoch = b.restoreEpoch
	// An independent actual API Backend has its own bounded cache. It receives
	// neither the worker's source objects nor a complete N-row mirror, and its
	// observation wrapper cannot race the running worker's original connection.
	apiOptions := options
	apiOptions.Endpoints, apiOptions.Namespace, apiOptions.Identity = b.client.Endpoints(), b.namespace, identity
	apiOptions.Clock, apiOptions.WorkerControl = b.authorityClock, nil
	api, err := New(ctx, apiOptions)
	require.NoError(t, err)
	defer api.Close()
	lease := &creationFaultLease{Lease: api.client.Lease}
	api.client.Lease = lease
	txns, responseBytes := 0, 0
	api.client.KV = &faultKV{KV: api.client.KV, match: func([]clientv3.Op) bool {
		txns++
		return true
	}, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		if response != nil {
			responseBytes += (*pb.TxnResponse)(response).Size()
		}
		return response, err
	}}
	start := time.Now()
	var first string
	for _, ordinal := range []int{0, count / 3, count - 1} {
		_, id, err := metadataScaleModel(b.namespace, ordinal, time.Now().UTC().Add(time.Hour))
		require.NoError(t, err)
		view, err := api.ReadSandboxView(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, view)
		read, err := api.ReadSandboxSnapshot(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, read)
		require.NoError(t, api.ConfirmSandboxView(ctx, read.View))
		require.Equal(t, id, read.Snapshot.SandboxID)
		first = id
	}
	// These are two original metadata operation admissions/renewals, not real
	// Exec, target terminal receipts or throughput claims. Cancellation revokes
	// each original lease before the idle interval and on every failure path.
	for i := 0; i < 2; i++ {
		bounded, stop := context.WithTimeout(ctx, 10*time.Second)
		result, err := api.BeginOperation(bounded, BeginOperationInput{SandboxID: first, RequestID: uuid.NewString(), Kind: OperationData})
		require.NoError(t, err)
		require.NotNil(t, result.Capability)
		func() {
			defer func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				require.NoError(t, api.CancelOperationCapability(cleanup, result.Capability))
			}()
			require.NoError(t, api.RenewOperation(bounded, result.Capability))
			outcome, err := api.ResolveOperation(bounded, result.Reference)
			require.NoError(t, err)
			require.Equal(t, OperationCommitted, outcome)
		}()
		stop()
	}
	require.EqualValues(t, 2, lease.grants.Load())
	require.EqualValues(t, 2, lease.keeps.Load())
	require.EqualValues(t, 2, lease.revokes.Load())
	t.Logf("METADATA_SCALE fixed_API_sample N=%d elapsed=%s txns=%d response_pb_bytes=%d grants=%d keeps=%d revokes=%d snapshot_budget=262144", count, time.Since(start), txns, responseBytes, lease.grants.Load(), lease.keeps.Load(), lease.revokes.Load())
}

// Server counters include the real Watch streams and all native clients;
// diagnostic Status/HTTP scrape calls are identified, never hidden in a claim
// of exact worker-only network bytes. Only bounded selected metric lines remain.
func metadataScaleMetrics(t *testing.T, ctx context.Context, raw *clientv3.Client, endpoints []string, count int, phase string) {
	t.Helper()
	client := &http.Client{Timeout: 3 * time.Second}
	defer client.CloseIdleConnections()
	for _, endpoint := range endpoints {
		bounded, stop := partitionWorkerNativeRequest(ctx)
		status, err := raw.Status(bounded, endpoint)
		stop()
		require.NoError(t, err)
		t.Logf("METADATA_SCALE %s N=%d endpoint=%s member=%x leader=%x db_total=%d db_in_use=%d raft_index=%d raft_applied=%d", phase, count, endpoint, status.Header.MemberId, status.Leader, status.DbSize, status.DbSizeInUse, status.RaftIndex, status.RaftAppliedIndex)
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/metrics", nil)
		require.NoError(t, err)
		response, err := client.Do(request)
		require.NoError(t, err)
		wire, readErr := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
		closeErr := response.Body.Close()
		require.NoError(t, readErr)
		require.NoError(t, closeErr)
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.LessOrEqual(t, len(wire), 2<<20)
		lines := 0
		for _, line := range strings.Split(string(wire), "\n") {
			selected := false
			for _, prefix := range []string{"etcd_network_client_grpc_", "grpc_server_started_total", "etcd_server_proposals_", "etcd_disk_wal_fsync_duration_seconds_sum", "etcd_disk_wal_fsync_duration_seconds_count", "etcd_disk_backend_commit_duration_seconds_sum", "etcd_disk_backend_commit_duration_seconds_count", "etcd_debugging_lease_"} {
				selected = selected || strings.HasPrefix(line, prefix)
			}
			if selected {
				fields := strings.Fields(line)
				require.GreaterOrEqual(t, len(fields), 2)
				_, err := strconv.ParseFloat(fields[len(fields)-1], 64)
				require.NoError(t, err)
				require.Less(t, lines, 128, "bounded fixed metric inventory")
				t.Logf("METADATA_SCALE metric %s %s %s", phase, endpoint, line)
				lines++
			}
		}
		require.Positive(t, lines)
	}
}

// Real maintenance runs serially, follower before leader. A single stopped
// member is physically waited and restarted with its original data/arguments;
// the remaining quorum must serve the same API/operation sample during fault.
func metadataScaleMaintenance(t *testing.T, ctx context.Context, raw *clientv3.Client, endpoints []string, children []*partitionWorkerNativeProcess, b *Backend, options Options, count int) {
	t.Helper()
	bounded, stop := partitionWorkerNativeRequest(ctx)
	point, err := raw.Get(bounded, b.identityKey)
	stop()
	require.NoError(t, err)
	bounded, stop = context.WithTimeout(ctx, time.Minute)
	_, err = raw.Compact(bounded, point.Header.Revision, clientv3.WithCompactPhysical())
	stop()
	require.NoError(t, err)
	order := make([]int, 0, 3)
	leader := -1
	for i, endpoint := range endpoints {
		bounded, stop = partitionWorkerNativeRequest(ctx)
		status, err := raw.Status(bounded, endpoint)
		stop()
		require.NoError(t, err)
		if status.Header.MemberId == status.Leader {
			leader = i
		} else {
			order = append(order, i)
		}
	}
	require.Len(t, order, 2)
	require.GreaterOrEqual(t, leader, 0)
	order = append(order, leader)
	for _, i := range order {
		bounded, stop = context.WithTimeout(ctx, 2*time.Minute)
		_, err = raw.Defragment(bounded, endpoints[i])
		stop()
		require.NoError(t, err)
		bounded, stop = partitionWorkerNativeRequest(ctx)
		_, err = raw.Status(bounded, endpoints[i])
		stop()
		require.NoError(t, err)
	}
	i := order[0]
	bounded, stop = partitionWorkerNativeRequest(ctx)
	original, err := raw.Status(bounded, endpoints[i])
	stop()
	require.NoError(t, err)
	children[i].stop()
	metadataScaleSample(t, ctx, b, options, count)
	command := exec.Command(children[i].cmd.Path, children[i].cmd.Args[1:]...)
	children[i] = startPartitionWorkerNativeProcess(t, command)
	bounded, stop = partitionWorkerNativeRequest(ctx)
	reference, err := raw.Status(bounded, endpoints[leader])
	stop()
	require.NoError(t, err)
	deadline := time.Now().Add(30 * time.Second)
	for {
		bounded, stop = partitionWorkerNativeRequest(ctx)
		status, err := raw.Status(bounded, endpoints[i])
		stop()
		if err == nil && status.Header != nil && status.Header.ClusterId == original.Header.ClusterId && status.Header.MemberId == original.Header.MemberId && status.Leader != 0 && status.RaftAppliedIndex >= reference.RaftAppliedIndex {
			break
		}
		require.True(t, time.Now().Before(deadline), "restarted original member did not recover: %v", err)
		partitionWorkerNativePause(t, ctx, 100*time.Millisecond)
	}
	t.Logf("METADATA_SCALE maintenance N=%d compact_revision=%d defrag_order=%v fault_member=%d originalWait=true restart_original_data=true", count, point.Header.Revision, order, i)
}

// This private test manifest carries bytes, never live capabilities or a
// callback. Its parent owns the genuine cell and signing root. RestoreEpoch
// must be reconstructed from Binding because Identity deliberately omits it
// from its persisted JSON representation.
type cleanupCapacityChildInput struct {
	Binding       p.TrustBinding
	ConfigBinding p.TrustBinding
	Identity      Identity
	Root          ed25519.PrivateKey
	RootPublic    ed25519.PublicKey
	PhysicalTask  TaskReference
	Contract      string
	Directory     string
	Nonce         string
}

// ACKs are bounded observations of this one fixed 33-Task experiment. They
// cannot be passed to any admission, Run, physical effect or settlement API.
type cleanupCapacityChildACK struct {
	Nonce, Phase           string
	Slot                   uint32
	Rows, Turns, Conflicts int
}

func cleanupCapacityChildRead(t *testing.T, path string) cleanupCapacityChildInput {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular())
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	owner, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.Zero(t, owner.Uid)
	require.LessOrEqual(t, info.Size(), int64(65536))
	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()
	wire, err := io.ReadAll(io.LimitReader(file, 65537))
	require.NoError(t, err)
	defer clear(wire)
	require.LessOrEqual(t, len(wire), 65536)
	var input cleanupCapacityChildInput
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&input))
	require.Equal(t, io.EOF, decoder.Decode(new(any)))
	require.Len(t, input.Root, ed25519.PrivateKeySize)
	require.Len(t, input.RootPublic, ed25519.PublicKeySize)
	require.True(t, bytes.Equal(input.Root.Public().(ed25519.PublicKey), input.RootPublic))
	require.Equal(t, input.Binding, input.ConfigBinding)
	require.Equal(t, "kubernetes", input.Binding.Target)
	require.Equal(t, input.Binding.AuthorityID, input.Identity.RuntimeID)
	input.Identity.RestoreEpoch = input.Binding.RestoreEpoch
	require.NoError(t, input.PhysicalTask.Validate())
	require.Equal(t, input.Binding.Namespace, input.PhysicalTask.Namespace)
	require.Equal(t, input.Binding.RestoreEpoch, input.PhysicalTask.RestoreEpoch)
	require.NotEqual(t, uint8(0xaa), input.PhysicalTask.Partition)
	require.Len(t, input.Nonce, 36)
	require.True(t, validHexDigest(input.Contract))
	return input
}

// Only the three fixed bytes 1=CHECK33, 2=RETRY33, 3=STOP are accepted in that
// order. One reader and one buffered command bound communication independently
// of Task count. Closing the owned pipe unblocks and joins that reader before
// session cleanup; the sole foreground goroutine owns every native turn/renew.
func TestCleanupPersistentCapacityChild(t *testing.T) {
	path := os.Getenv("CLEANUP_CAPACITY_CHILD_INPUT")
	if path == "" {
		return
	}
	require.Zero(t, os.Geteuid())
	input := cleanupCapacityChildRead(t, path)
	defer clear(input.Root)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	require.NoError(t, syscall.SetNonblock(3, true))
	commands := os.NewFile(3, "capacity-commands")
	replies := os.NewFile(4, "capacity-replies")
	require.NotNil(t, commands)
	require.NotNil(t, replies)
	defer replies.Close()
	queue := make(chan byte, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			var command [1]byte
			_, err := io.ReadFull(commands, command[:])
			if err != nil {
				command[0] = 0
			}
			select {
			case queue <- command[0]:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	var stopReader sync.Once
	joinReader := func() {
		stopReader.Do(func() {
			cancel()
			_ = commands.Close()
			<-readerDone
		})
	}
	defer joinReader()

	ca, err := os.ReadFile(input.Directory + "/ca.pem")
	require.NoError(t, err)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca))
	certificate, err := tls.LoadX509KeyPair(input.Directory+"/server.pem", input.Directory+"/server-key.pem")
	require.NoError(t, err)
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{certificate}}
	// The input is root-only, but every subsequent metadata call runs as the
	// ordinary zero-cap UID. This role never bootstraps a physical Kernel.
	for capability := uintptr(0); capability <= 40; capability++ {
		_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, capability, 0, 0, 0, 0)
		require.Zero(t, errno)
	}
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_SETGROUPS, 0, 0, 0)
	require.Zero(t, errno)
	_, _, errno = syscall.AllThreadsSyscall(unix.SYS_SETRESGID, 1000, 1000, 1000)
	require.Zero(t, errno)
	_, _, errno = syscall.AllThreadsSyscall(unix.SYS_SETRESUID, 1000, 1000, 1000)
	require.Zero(t, errno)
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig, DialTimeout: 2 * time.Second, Logger: zap.NewNop()})
	require.NoError(t, err)
	defer raw.Close()
	namespace, err := NewNamespace(input.Identity.Prefix, input.Identity.AuthorityID, input.Identity.Cell)
	require.NoError(t, err)
	require.Equal(t, input.Binding.Namespace, namespace.Root())
	options := Options{
		Endpoints: raw.Endpoints(), Namespace: namespace, Identity: input.Identity, TLS: tlsConfig,
		RequestTimeout: 2 * time.Second, Clock: goodAuthorityClock(),
		PublicationTrust: &RuntimePublicationTrust{AuthorityID: input.Binding.AuthorityID, Target: input.Binding.Target, Roots: []ed25519.PublicKey{input.RootPublic}},
		WorkerControl:    &WorkerControlOptions{SessionTTL: 60 * time.Second},
	}
	b, err := New(ctx, options)
	require.NoError(t, err)
	defer b.Close()
	// This separate Backend only authenticates the genuine physical release
	// history. Ordinary metadata publication above must retain enrolled=nil.
	proofOptions := options
	proofOptions.EnrolledPublication = &EnrolledPublicationOptions{ContractDigest: input.Contract, MaxActive: 32}
	proofReader, err := New(ctx, proofOptions)
	require.NoError(t, err)
	defer proofReader.Close()
	session, err := b.OpenWorkerSession(ctx, "capacity-metadata-child")
	require.NoError(t, err)
	part, err := b.ClaimWorkerPartition(ctx, session, 0xaa)
	require.NoError(t, err)
	peer := &cleanupCapacityPeer{b: b, session: session, part: part}
	for slot := 32; slot < 100; slot++ {
		share, err := b.AcquireWorkerBudget(ctx, session, WorkerBudgetClaim, uint16(slot))
		require.NoError(t, err)
		peer.shares = append(peer.shares, share)
	}
	peer.observeNative(false)
	var closeSession sync.Once
	cleanup := func() {
		closeSession.Do(func() {
			joinReader()
			cleanupCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
			defer stop()
			require.NoError(t, b.ReleaseWorkerSession(cleanupCtx, session))
			require.NoError(t, b.Close())
			require.NoError(t, proofReader.Close())
		})
	}
	defer cleanup()

	var tasks [32]*cleanupCapacityTaskState
	ordinal := 0
	for candidate := 0; candidate < 65536 && ordinal < len(tasks); candidate++ {
		workspace, err := NewWorkspaceIdentity("s3", input.Identity.StorageID, "bucket", fmt.Sprintf("capacity/%d/", candidate))
		require.NoError(t, err)
		if workspace.Partition() != 0xaa {
			continue
		}
		task := &cleanupCapacityTaskState{ref: cleanupCapacityTask(t, ctx, b, input.Root, workspace, ordinal)}
		require.NoError(t, peer.prepare(ctx, task))
		if ordinal < 31 {
			require.NoError(t, peer.occupy(ctx, task, false))
			require.Nil(t, task.occupied.record.Run)
			require.Equal(t, "reserved", task.occupied.record.Confirmed)
		}
		tasks[ordinal] = task
		ordinal++
	}
	require.Equal(t, len(tasks), ordinal)
	extra := tasks[31]
	metadata := cleanupCapacityRows(t, ctx, raw, namespace.Root())
	require.Len(t, metadata, 63, "31 real S/E plus only the original selecting E33")
	delete(metadata, extra.selected.execution.key)
	require.Len(t, metadata, 62)
	stable := func(rows map[string]*mvccpb.KeyValue) {
		for key, before := range metadata {
			require.Equal(t, before, rows[key], "31 metadata occupations remain exact: %s", key)
		}
	}
	ack := func(phase string, slot uint32) {
		observation, stop := context.WithTimeout(context.Background(), 2*time.Second)
		defer stop()
		wire, err := json.Marshal(cleanupCapacityChildACK{Nonce: input.Nonce, Phase: phase, Slot: slot, Rows: len(cleanupCapacityRows(t, observation, raw, namespace.Root())), Turns: peer.turns, Conflicts: peer.conflicts})
		require.NoError(t, err)
		require.LessOrEqual(t, len(wire)+1, 4096)
		_, err = replies.Write(append(wire, '\n'))
		require.NoError(t, err)
	}
	ack("READY31", 0)
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	var full map[string]*mvccpb.KeyValue
	var physical cleanupExecutionRecord
	physicalKey, err := namespace.cleanupExecutionKey(input.PhysicalTask)
	require.NoError(t, err)
	for expected := byte(1); expected <= 3; {
		select {
		case <-ctx.Done():
			t.Fatal("metadata child deadline before fixed protocol completed")
		case <-ticker.C:
			require.NoError(t, b.RenewWorkerSession(ctx, session))
		case command := <-queue:
			require.Equal(t, expected, command, "fixed CHECK33/RETRY33/STOP order; EOF is failure")
			switch command {
			case 1:
				full = cleanupCapacityRows(t, ctx, raw, namespace.Root())
				require.Len(t, full, 65)
				stable(full)
				kv := full[physicalKey]
				require.NotNil(t, kv)
				require.NoError(t, decodeCleanupExecution(kv, kv.ModRevision, input.PhysicalTask, &physical))
				require.Equal(t, "occupied", physical.Phase)
				require.Equal(t, "archive-bound", physical.Confirmed)
				require.NotNil(t, physical.Run)
				require.True(t, physical.Run.Joined)
				require.NotNil(t, physical.Archive)
				delete(full, extra.selected.execution.key)
				require.Len(t, full, 64)
				before := peer.conflicts
				require.NoError(t, peer.occupy(ctx, extra, true))
				require.Equal(t, 32, peer.conflicts-before)
				require.Nil(t, extra.occupied)
				after := cleanupCapacityRows(t, ctx, raw, namespace.Root())
				require.Len(t, after, 65)
				for key, before := range full {
					require.Equal(t, before, after[key], "natural recovery raced CHECK33: %s", key)
				}
				require.True(t, extra.selected.execution.matches(after[extra.selected.execution.key]))
				ack("CHECKED33", physical.Slot)
			case 2:
				before := cleanupCapacityRows(t, ctx, raw, namespace.Root())
				stable(before)
				kv := before[physicalKey]
				require.NotNil(t, kv)
				var settled cleanupExecutionRecord
				require.NoError(t, decodeCleanupExecution(kv, kv.ModRevision, input.PhysicalTask, &settled))
				require.Equal(t, "settled", settled.Phase)
				require.Equal(t, physical.Incarnation, settled.Incarnation)
				require.NotNil(t, settled.Run)
				require.True(t, settled.Run.Joined)
				require.NotNil(t, settled.Release)
				proof, err := proofReader.readLocalOwnerReleaseProof(ctx, input.PhysicalTask)
				require.NoError(t, err)
				require.NotNil(t, proof)
				require.Equal(t, physical.TaskBirth, proof.record.TaskBirth)
				require.Equal(t, settled.Release.CreateRevision, proof.kv.CreateRevision)
				require.Equal(t, settled.Release.Digest, workspaceSyncIntentDigest(string(proof.kv.Value)))
				slot, err := namespace.cleanupExecutionSlotKey(physical.Slot)
				require.NoError(t, err)
				require.Nil(t, before[slot], "only the genuine physical settlement may free this S")
				require.NoError(t, peer.occupy(ctx, extra, false))
				require.Equal(t, physical.Slot, extra.occupied.record.Slot)
				require.Nil(t, extra.occupied.record.Run)
				stable(cleanupCapacityRows(t, ctx, raw, namespace.Root()))
				ack("RETRIED33", physical.Slot)
			case 3:
				stable(cleanupCapacityRows(t, ctx, raw, namespace.Root()))
				require.Zero(t, peer.claimPuts.Load(), "metadata occupations never obtain physical TaskClaims")
				cleanup()
				ack("STOPPED", physical.Slot)
				return
			}
			expected++
		}
	}
}
