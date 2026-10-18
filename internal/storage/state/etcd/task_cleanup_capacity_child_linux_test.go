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
// member recovery. The separate startup pilot retains 10k-to-100k checkpoint
// history without the idle interval and is diagnostic evidence only.
// These rows neither model complete B_live nor authorize
// Pod/FUSE/runtime side effects or describe N actually usable sandboxes.
func TestMetadataScaleNative(t *testing.T) {
	if os.Getenv("SANDBOX_METADATA_SCALE_NATIVE") != "1" {
		t.Skip("explicit owned disk-backed three-member metadata scale fixture required")
	}
	pilot := os.Getenv("SANDBOX_METADATA_SCALE_PILOT") == "1"
	faultPilot := os.Getenv("SANDBOX_METADATA_SCALE_FAULT_PILOT") == "1"
	startupPilot := os.Getenv("SANDBOX_METADATA_SCALE_STARTUP_PILOT") == "1"
	duration, tiers, err := metadataScaleRunPlan(pilot, faultPilot, startupPilot)
	require.NoError(t, err)
	root := os.Getenv("SANDBOX_METADATA_SCALE_DATA")
	require.True(t, filepath.IsAbs(root), "runner must provide an absolute owned disk-backed data directory")
	var disk syscall.Statfs_t
	require.NoError(t, syscall.Statfs(root, &disk))
	require.NotEqual(t, int64(unix.TMPFS_MAGIC), int64(disk.Type), "100k model data must not live in memory-backed tmpfs")
	require.GreaterOrEqual(t, uint64(disk.Bavail)*uint64(disk.Bsize), uint64(20<<30), "reserve20GiB free before creating the disposable cluster")
	data, err := os.MkdirTemp(root, "metadata-scale-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(data)) })
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
		if startupPilot {
			metadataScaleResourceDiagnostics(t, count, "seeded", children)
		}
		before := metadataScaleControlDigest(t, ctx, raw, seed.namespace, count)
		observation := metadataScaleIdle(t, ctx, raw, options, count, duration, pilot, faultPilot, startupPilot, children, endpoints)
		require.Equal(t, before, metadataScaleControlDigest(t, ctx, raw, seed.namespace, count), "idle/maintenance must not rewrite any model control or its native envelope")
		if previous != 0 && !startupPilot {
			require.LessOrEqual(t, observation.transactions, baseline.transactions*2+1024, "10xN must not produce10x short-period worker transactions; long audit/source reads are reported separately")
			require.LessOrEqual(t, observation.keeps, baseline.keeps*2+8, "worker session renewal must not become per-sandbox renewal")
		}
		baseline, previous = observation, count
	}
	if startupPilot {
		t.Log("METADATA_SCALE STARTUP PILOT ONLY:10k sync/Stop then100k with original checkpoints and3min sync bound; no1h idle, maintenance/fault or full capacity acceptance")
	} else if faultPilot {
		t.Log("METADATA_SCALE FAULT PILOT ONLY:10k/60s idle plus real maintenance/member restart harness; not1h/100k capacity or degraded startup acceptance")
	} else if pilot {
		t.Log("METADATA_SCALE PILOT ONLY:10k/60s harness; no1h tier, maintenance/fault,100k, Pod/FUSE or HTTP SLO acceptance")
	} else {
		t.Log("METADATA_SCALE View/Snapshot/scheduling typed subset10k/100k tiers finished; not completeB_live/cellcapacity,1M/multi-cell/24h, actualN sandboxes/Pods/FUSE, creation throughput or HTTP SLO evidence")
	}
}

type metadataScaleObservation struct {
	transactions, ranges, mutations, controlWrites, responseBytes, grants, keeps int64
}

// Report a copied, fixed-size worker snapshot without iterating native model
// rows or touching the actor's private scan state. Revision gaps distinguish
// unfinished Range/checkpoint work from an old Watch replay baseline.
func metadataScaleStartupStatus(t *testing.T, count int, elapsed time.Duration, status DueWorkerStatus) {
	t.Helper()
	synchronized, checkpointed := 0, 0
	var minimumReplay, maximumReplay, minimumCovered, maximumCovered int64
	var largestGap DueWorkerPartitionStatus
	for i, partition := range status.Partitions {
		if partition.Synchronized {
			synchronized++
		}
		if partition.Revision > 0 {
			checkpointed++
		}
		if i == 0 {
			minimumReplay, minimumCovered = partition.ReplayFrom, partition.CoveredRevision
		}
		minimumReplay, maximumReplay = min(minimumReplay, partition.ReplayFrom), max(maximumReplay, partition.ReplayFrom)
		minimumCovered, maximumCovered = min(minimumCovered, partition.CoveredRevision), max(maximumCovered, partition.CoveredRevision)
		if i == 0 || partition.ObservedRevision-partition.CoveredRevision > largestGap.ObservedRevision-largestGap.CoveredRevision {
			largestGap = partition
		}
	}
	t.Logf("METADATA_SCALE startup progress N=%d elapsed=%s running=%t synchronized=%t phase=%s partitions=%d synced=%d checkpointed=%d replay_from=%d..%d covered=%d..%d renewals=%d owned=%d queued=%d error=%q largest_gap=%+v", count, elapsed, status.Running, status.Synchronized, status.Phase, len(status.Partitions), synchronized, checkpointed, minimumReplay, maximumReplay, minimumCovered, maximumCovered, status.Renewals, status.OwnedBytes, status.Queued, status.LastError, largestGap)
}

// The original process buffers have no live-reader lock, so only read fixed
// kernel files here. Each resource class has a 64 KiB aggregate payload cap;
// diagnostic failures are logged and never replace the measured worker error.
func metadataScaleResourceDiagnostics(t *testing.T, count int, phase string, children []*partitionWorkerNativeProcess) {
	t.Helper()
	cgroups := []string{
		"/sys/fs/cgroup/memory.current", "/sys/fs/cgroup/memory.swap.current",
		"/sys/fs/cgroup/memory.events", "/sys/fs/cgroup/memory.stat", "/sys/fs/cgroup/memory.pressure",
		"/sys/fs/cgroup/cpu.stat", "/sys/fs/cgroup/cpu.pressure", "/sys/fs/cgroup/io.stat", "/sys/fs/cgroup/io.pressure",
	}
	processes := []string{"/proc/self/status", "/proc/self/io"}
	for _, child := range children {
		for _, name := range []string{"status", "stat", "io"} {
			processes = append(processes, fmt.Sprintf("/proc/%d/%s", child.cmd.Process.Pid, name))
		}
	}
	for _, class := range []struct {
		name  string
		paths []string
	}{{"cgroup", cgroups}, {"process", processes}} {
		remaining := 64 << 10
		for _, path := range class.paths {
			if remaining == 0 {
				t.Logf("METADATA_SCALE diagnostic N=%d phase=%s class=%s payload_cap_reached=true", count, phase, class.name)
				break
			}
			file, err := os.Open(path)
			if err != nil {
				t.Logf("METADATA_SCALE diagnostic N=%d phase=%s class=%s path=%s open_error=%v", count, phase, class.name, path, err)
				continue
			}
			wire, readErr := io.ReadAll(io.LimitReader(file, int64(remaining)+1))
			closeErr := file.Close()
			truncated := len(wire) > remaining
			if truncated {
				wire = wire[:remaining]
			}
			remaining -= len(wire)
			t.Logf("METADATA_SCALE diagnostic N=%d phase=%s class=%s path=%s truncated=%t read_error=%v close_error=%v data=%q", count, phase, class.name, path, truncated, readErr, closeErr, wire)
		}
	}
}

// Maintenance diagnosis below wraps only this original worker's Backend, not
// the raw/admin client or the separate fixed-API sample Backend.
type metadataScaleMaintenanceDiagnostics struct {
	mu                          sync.Mutex
	t                           *testing.T
	parent                      context.Context
	worker                      *DueWorker
	active                      bool
	phase                       string
	sequence                    uint64
	defragSequence              uint64
	defrags                     [3]metadataScaleDefragInterval
	firstFailure                *metadataScaleMaintenanceRPCFailure
	normalBytes, errorBytes     int
	normalDropped, errorDropped int
}

// Each lane retains at most 64 KiB of payload, reserving a separate lane for
// phase boundaries and errors after ordinary RPC traffic fills its allowance.
// A line is at most 4 KiB; the summary explicitly counts any dropped lines.
// These logs are observations only; no context, timeout or result is replaced.
func (d *metadataScaleMaintenanceDiagnostics) log(failure bool, format string, arguments ...any) {
	line := fmt.Sprintf(format, arguments...)
	if len(line) > 4096 {
		line = line[:4085] + "[truncated]"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	used := &d.normalBytes
	if failure {
		used = &d.errorBytes
	}
	if *used+len(line) > 64<<10 {
		if failure {
			d.errorDropped++
		} else {
			d.normalDropped++
		}
		return
	}
	*used += len(line)
	d.t.Logf("METADATA_SCALE maintenance_diagnostic owner_scope=original-due-worker %s", line)
}

func (d *metadataScaleMaintenanceDiagnostics) bind(w *DueWorker) {
	d.mu.Lock()
	d.worker = w
	d.mu.Unlock()
}

// These markers enclose only the actual Defragment invocation, excluding the
// surrounding admin Status calls and all formatted phase/log descriptions.
func (d *metadataScaleMaintenanceDiagnostics) beginDefrag() uint64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.defragSequence++
	return d.defragSequence
}

func (d *metadataScaleMaintenanceDiagnostics) endDefrag(epoch uint64, member int, started, ended time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if epoch == 0 || epoch > uint64(len(d.defrags)) {
		return
	}
	// Both actual call anchors precede observer locking. Keeping all three
	// ordered intervals allows a dispatched RPC to cross adjacent members,
	// without treating the unmeasured gaps or publication delay as Defrag.
	d.defrags[epoch-1] = metadataScaleDefragInterval{epoch: epoch, member: member, started: started, ended: ended}
	if d.firstFailure != nil {
		d.firstFailure.defrags = d.defrags
	}
}

func (d *metadataScaleMaintenanceDiagnostics) permitsLoss(err error, original WorkerSessionReference) bool {
	if d.parent.Err() != nil {
		return false
	}
	if metadataScaleOnlyMemberStopTransport(err) {
		return true
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return metadataScaleObservedDefragDeadline(err, original, d.firstFailure)
}

func (d *metadataScaleMaintenanceDiagnostics) state(event string, elapsed time.Duration) {
	d.mu.Lock()
	w, phase := d.worker, d.phase
	d.mu.Unlock()
	if w == nil {
		return
	}
	status := w.Status()
	w.backend.archiveLife.mu.Lock()
	lost, released := w.session.lost, w.session.released
	deadline := w.session.deadline
	operation, watch := w.session.operation != nil, w.session.dueWatch != nil
	w.backend.archiveLife.mu.Unlock()
	parentLeaves, parentTruncated := metadataScaleDiagnosticErrorLeaves(d.parent.Err())
	d.log(true, "event=%s phase=%s at=%s elapsed=%s running=%t synchronized=%t worker_phase=%s session=%s lease=%d lost=%t released=%t session_deadline=%s operation=%t watch=%t last_error=%q parent_leaves=%+v parent_truncated=%t", event, phase, time.Now().UTC().Format(time.RFC3339Nano), elapsed, status.Running, status.Synchronized, status.Phase, status.Session.SessionID, status.Session.LeaseID, lost, released, deadline.UTC().Format(time.RFC3339Nano), operation, watch, status.LastError, parentLeaves, parentTruncated)
}

// Only the existing serial fixture calls runPhase. The defer records failure
// exits too; Require's Goexit must not erase which real maintenance call ran.
func (d *metadataScaleMaintenanceDiagnostics) runPhase(name string, work func()) {
	d.mu.Lock()
	d.active, d.phase = true, name
	d.mu.Unlock()
	started := time.Now()
	d.state("phase-start", 0)
	defer func() {
		d.state("phase-end", time.Since(started))
		d.mu.Lock()
		d.phase = "after-" + name
		d.mu.Unlock()
	}()
	work()
}

func (d *metadataScaleMaintenanceDiagnostics) request(ctx context.Context, method, shape string) func(error) {
	d.mu.Lock()
	active, phase := d.active, d.phase
	w := d.worker
	d.sequence++
	sequence := d.sequence
	d.mu.Unlock()
	observation := metadataScaleMaintenanceRPCFailure{method: method}
	if w != nil {
		w.backend.archiveLife.mu.Lock()
		observation.session, observation.sessionDeadline = w.session.reference, w.session.deadline
		w.backend.archiveLife.mu.Unlock()
	}
	// Complete observer/lifecycle locking before sampling the actual call
	// boundary. Logging is deferred until return, so it cannot consume the
	// original deadline and then be mistaken for dispatched native work.
	deadline, hasDeadline := ctx.Deadline()
	observation.deadline = deadline
	observation.parentBefore, observation.sendContextErr = d.parent.Err(), ctx.Err()
	started := time.Now()
	observation.sent = started
	return func(err error) {
		// Observe before the original caller cancels its RPC context. Both its
		// error and the returned error survive independently as typed leaves.
		observation.completed, observation.returned = true, time.Now()
		observation.requestErr, observation.rpcErr, observation.parentAfter = err, ctx.Err(), d.parent.Err()
		d.mu.Lock()
		observation.defrags = d.defrags
		if err != nil || observation.rpcErr != nil {
			d.firstFailure = metadataScaleKeepFirstFailure(d.firstFailure, observation)
		}
		returnedPhase := d.phase
		d.mu.Unlock()
		if !active {
			return
		}
		before, beforeTruncated := metadataScaleDiagnosticErrorLeaves(observation.sendContextErr)
		leaves, truncated := metadataScaleDiagnosticErrorLeaves(err)
		ctxLeaves, ctxTruncated := metadataScaleDiagnosticErrorLeaves(observation.rpcErr)
		parentLeaves, parentTruncated := metadataScaleDiagnosticErrorLeaves(observation.parentAfter)
		d.log(false, "event=rpc-send sequence=%d method=%s shape=%s phase=%s at=%s has_deadline=%t deadline=%s ctx_leaves=%+v ctx_truncated=%t emission=after-rpc-return", sequence, method, shape, phase, started.UTC().Format(time.RFC3339Nano), hasDeadline, deadline.UTC().Format(time.RFC3339Nano), before, beforeTruncated)
		d.log(err != nil || observation.rpcErr != nil, "event=rpc-return sequence=%d method=%s shape=%s sent_phase=%s returned_phase=%s sent=%s returned=%s elapsed=%s deadline=%s error_leaves=%+v error_truncated=%t ctx_leaves=%+v ctx_truncated=%t parent_leaves=%+v parent_truncated=%t", sequence, method, shape, phase, returnedPhase, started.UTC().Format(time.RFC3339Nano), observation.returned.UTC().Format(time.RFC3339Nano), observation.returned.Sub(started), deadline.UTC().Format(time.RFC3339Nano), leaves, truncated, ctxLeaves, ctxTruncated, parentLeaves, parentTruncated)
	}
}

type metadataScaleDiagnosticKV struct {
	clientv3.KV
	diagnostics *metadataScaleMaintenanceDiagnostics
}

func (kv *metadataScaleDiagnosticKV) Get(ctx context.Context, key string, options ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	finish := kv.diagnostics.request(ctx, "Range", metadataScaleOperationShape([]clientv3.Op{clientv3.OpGet(key, options...)}))
	response, err := kv.KV.Get(ctx, key, options...)
	finish(err)
	return response, err
}

func (kv *metadataScaleDiagnosticKV) Txn(ctx context.Context) clientv3.Txn {
	return &metadataScaleDiagnosticTxn{Txn: kv.KV.Txn(ctx), ctx: ctx, diagnostics: kv.diagnostics}
}

type metadataScaleDiagnosticTxn struct {
	clientv3.Txn
	ctx         context.Context
	diagnostics *metadataScaleMaintenanceDiagnostics
	comparisons int
	shape       string
}

func (txn *metadataScaleDiagnosticTxn) If(comparisons ...clientv3.Cmp) clientv3.Txn {
	txn.comparisons = len(comparisons)
	txn.Txn = txn.Txn.If(comparisons...)
	return txn
}

func (txn *metadataScaleDiagnosticTxn) Then(operations ...clientv3.Op) clientv3.Txn {
	txn.shape = metadataScaleOperationShape(operations)
	txn.Txn = txn.Txn.Then(operations...)
	return txn
}

func (txn *metadataScaleDiagnosticTxn) Else(operations ...clientv3.Op) clientv3.Txn {
	txn.Txn = txn.Txn.Else(operations...)
	return txn
}

func (txn *metadataScaleDiagnosticTxn) Commit() (*clientv3.TxnResponse, error) {
	finish := txn.diagnostics.request(txn.ctx, "Txn", fmt.Sprintf("comparisons=%d %s", txn.comparisons, txn.shape))
	response, err := txn.Txn.Commit()
	finish(err)
	return response, err
}

// Shape retains at most four key locators, never values or full source graphs.
func metadataScaleOperationShape(operations []clientv3.Op) string {
	shape := fmt.Sprintf("operations=%d", len(operations))
	for _, op := range operations[:min(len(operations), 4)] {
		key, kind := string(op.KeyBytes()), "other"
		if len(key) > 128 {
			key = key[:128] + "[truncated]"
		}
		if op.IsGet() {
			kind = "Range"
		} else if op.IsPut() {
			kind = "Put"
		} else if op.IsDelete() {
			kind = "Delete"
		}
		shape += fmt.Sprintf(" %s(%q,range=%t)", kind, key, len(op.RangeBytes()) != 0)
	}
	return shape
}

// KeepAlive and once-only cleanup share the same concrete worker client. These
// two observations distinguish renewal from guard/discovery timeouts without
// changing the existing lease counters or intercepting maintenance/admin RPCs.
type metadataScaleDiagnosticLease struct {
	clientv3.Lease
	diagnostics *metadataScaleMaintenanceDiagnostics
}

func (lease *metadataScaleDiagnosticLease) KeepAliveOnce(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error) {
	finish := lease.diagnostics.request(ctx, "KeepAliveOnce", fmt.Sprintf("lease=%d", id))
	response, err := lease.Lease.KeepAliveOnce(ctx, id)
	finish(err)
	return response, err
}

func (lease *metadataScaleDiagnosticLease) Revoke(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
	finish := lease.diagnostics.request(ctx, "Revoke", fmt.Sprintf("lease=%d", id))
	response, err := lease.Lease.Revoke(ctx, id)
	finish(err)
	return response, err
}

// This observer never changes a result or a request. Unary response bytes are
// exact protobuf bytes; Watch/network totals come from the real server metrics.
func metadataScaleIdle(t *testing.T, ctx context.Context, raw *clientv3.Client, options Options, count int, duration time.Duration, pilot, faultPilot, startupPilot bool, children []*partitionWorkerNativeProcess, endpoints []string) metadataScaleObservation {
	t.Helper()
	b, err := New(ctx, options)
	require.NoError(t, err)
	defer b.Close()
	// Register before the worker's Stop defer. Failure diagnostics run only
	// after that original owner joined; member.stop retains its sole Cmd.Wait.
	// The existing member output buffers must never be read while writers live.
	defer func() {
		if !t.Failed() {
			return
		}
		metadataScaleResourceDiagnostics(t, count, "failure-before-member-stop", children)
		for _, child := range children {
			child.stop()
			for name, output := range map[string]*partitionWorkerNativeOutput{"stdout": &child.stdout, "stderr": &child.stderr} {
				wire := output.buffer.Bytes()
				if len(wire) > 8<<10 {
					wire = wire[len(wire)-(8<<10):]
				}
				// The original buffer retains its first 64 KiB. If it dropped
				// output, this is only that retained prefix's tail, not a claim
				// that we recovered later bytes from the actual member process.
				t.Logf("METADATA_SCALE joined member N=%d pid=%d %s_retained_prefix_tail=%q dropped=%d", count, child.cmd.Process.Pid, name, wire, output.dropped)
			}
		}
	}()
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
	diagnostics := &metadataScaleMaintenanceDiagnostics{t: t, parent: ctx}
	b.client.Lease = lease
	if !startupPilot && (!pilot || faultPilot) {
		b.client.KV = &metadataScaleDiagnosticKV{KV: b.client.KV, diagnostics: diagnostics}
		b.client.Lease = &metadataScaleDiagnosticLease{Lease: lease, diagnostics: diagnostics}
	}
	defer func() {
		diagnostics.state("fixture-return", 0)
		diagnostics.mu.Lock()
		defer diagnostics.mu.Unlock()
		t.Logf("METADATA_SCALE maintenance_diagnostic payload_bytes=%d phase_error_payload_bytes=%d normal_dropped=%d phase_error_dropped=%d hard_payload_cap=131072", diagnostics.normalBytes, diagnostics.errorBytes, diagnostics.normalDropped, diagnostics.errorDropped)
	}()
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
	workerOptions := DueWorkerOptions{WorkerID: fmt.Sprintf("metadata-scale-%d", count), Partitions: partitions, RangeSlots: []uint16{0}, ClaimSlots: claims, RelistSlot: 0, StageTTL: 30 * time.Second}
	w, err := b.StartDueWorker(ctx, workerOptions)
	require.NoError(t, err)
	diagnostics.bind(w)
	originalWorker := w
	var originalStopError error
	defer func() {
		if w != originalWorker {
			require.NoError(t, w.Stop())
			<-w.Done()
		}
		stopError := originalWorker.Stop()
		<-originalWorker.Done()
		if originalStopError == nil {
			require.NoError(t, stopError)
		} else {
			require.True(t, diagnostics.permitsLoss(stopError, originalWorker.session.Reference()))
			require.EqualError(t, stopError, originalStopError.Error(), "shutdown must retain the original measured fault result")
		}
	}()
	start := time.Now()
	nextDiagnostic := start
	for {
		status := w.Status()
		if !status.Running {
			terminalErr := w.Stop()
			t.Logf("METADATA_SCALE startup stopped N=%d elapsed=%s status=%+v terminal_error=%v txns=%d ranges=%d mutations=%d response_pb=%d grants=%d keeps=%d", count, time.Since(start), status, terminalErr, transactions.Load(), ranges.Load(), mutations.Load(), responseBytes.Load(), lease.grants.Load(), lease.keeps.Load())
			require.True(t, status.Running, "original startup owner terminated; terminal error remains a failure: %v", terminalErr)
		}
		require.LessOrEqual(t, status.OwnedBytes, uint64(64<<20))
		require.LessOrEqual(t, status.Queued, 10000)
		if status.Synchronized {
			require.Empty(t, status.LastError)
			break
		}
		if !time.Now().Before(nextDiagnostic) || time.Since(start) >= 3*time.Minute {
			metadataScaleStartupStatus(t, count, time.Since(start), status)
			t.Logf("METADATA_SCALE startup RPC N=%d txns=%d ranges=%d mutations=%d response_pb=%d grants=%d keeps=%d", count, transactions.Load(), ranges.Load(), mutations.Load(), responseBytes.Load(), lease.grants.Load(), lease.keeps.Load())
			nextDiagnostic = time.Now().Add(5 * time.Second)
		}
		require.Less(t, time.Since(start), 3*time.Minute, "initial real bounded discovery failed to synchronize; phase=%s running=%t error=%s", status.Phase, status.Running, status.LastError)
		partitionWorkerNativePause(t, ctx, 100*time.Millisecond)
	}
	if startupPilot {
		metadataScaleStartupStatus(t, count, time.Since(start), w.Status())
		result := metadataScaleObservation{transactions.Load(), ranges.Load(), mutations.Load(), controlWrites.Load(), responseBytes.Load(), lease.grants.Load(), lease.keeps.Load()}
		require.Zero(t, result.controlWrites)
		t.Logf("METADATA_SCALE startup-only N=%d elapsed=%s original_checkpoint_retained=true observation=%+v; original Stop/join follows before the next seed tier", count, time.Since(start), result)
		return result
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
		if (!pilot || faultPilot) && !maintained && time.Since(start) >= duration/2 {
			maintenanceStart := time.Now()
			maintenanceBefore := metadataScaleObservation{transactions.Load(), ranges.Load(), mutations.Load(), controlWrites.Load(), responseBytes.Load(), lease.grants.Load(), lease.keeps.Load()}
			metadataScaleMaintenance(t, ctx, raw, endpoints, children, b, options, count, diagnostics)
			diagnostics.mu.Lock()
			diagnostics.phase = "worker-recovery"
			diagnostics.mu.Unlock()
			diagnostics.state("recovery-start", 0)
			w = metadataScaleRecoverWorker(t, ctx, raw, b, w, workerOptions, &originalStopError, diagnostics)
			diagnostics.bind(w)
			diagnostics.state("recovery-end", 0)
			diagnostics.mu.Lock()
			diagnostics.active = false
			diagnostics.mu.Unlock()
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
	require.Equal(t, !pilot || faultPilot, maintained, "full tiers and the explicit fault pilot must execute real maintenance and member recovery")
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

// Member failure or an attributed Defrag RPC timeout may conservatively lose a
// dispatched original guard even when quorum service remains available. Never
// revive that session: join its owner, observe actual native retirement, then
// use the unchanged acquisition path. This is all maintenance accounting.
func metadataScaleRecoverWorker(t *testing.T, ctx context.Context, raw *clientv3.Client, b *Backend, w *DueWorker, options DueWorkerOptions, originalStopError *error, diagnostics *metadataScaleMaintenanceDiagnostics) *DueWorker {
	t.Helper()
	recovery, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	for {
		current := w.Status()
		b.archiveLife.mu.Lock()
		lost := w.session.lost || w.session.released
		b.archiveLife.mu.Unlock()
		if lost {
			break
		}
		require.True(t, current.Running, "%+v", current)
		require.Empty(t, current.LastError, "an unattributed maintenance failure cannot be hidden; replacement still requires exact original-owner transport or Defrag evidence")
		if current.Synchronized {
			t.Logf("METADATA_SCALE maintenance worker survived same_session=%s same_lease=%d", current.Session.SessionID, current.Session.LeaseID)
			return w
		}
		partitionWorkerNativePause(t, recovery, 100*time.Millisecond)
	}
	select {
	case <-w.Done():
	case <-recovery.Done():
		t.Fatal("lost original worker did not physically join within recovery bound")
	}
	stopError := w.Stop()
	*originalStopError = stopError
	require.True(t, diagnostics.permitsLoss(stopError, w.session.Reference()), "only original Unknown+member-stop transport or the attributed ordered-Defrag RPC deadline permits this measured replacement: %v", stopError)
	oldSession := w.session
	oldReference := oldSession.Reference()
	require.ErrorIs(t, b.RenewWorkerSession(recovery, oldSession), ErrInvalidRecord, "the retired original capability must remain unusable")
	b.archiveLife.mu.Lock()
	retired := oldSession.lost && oldSession.released
	joined := oldSession.operation == nil && oldSession.dueWatch == nil
	oldGuard := oldSession.guard.key
	b.archiveLife.mu.Unlock()
	require.True(t, retired)
	require.True(t, joined, "the original native and Watch owners must be physically retired")

	// A lost response to the original revoke is not a native absence proof.
	// Bounded linearizable reads must observe both the actual guard absence and
	// original lease disappearance before a replacement can acquire its slots.
	retirement, stop := context.WithTimeout(recovery, time.Duration(oldSession.ttlSeconds)*time.Second+5*time.Second)
	defer stop()
	for {
		rpc, finish := partitionWorkerNativeRequest(retirement)
		guard, err := raw.Get(rpc, oldGuard)
		finish()
		require.NoError(t, err)
		require.NotNil(t, guard.Header)
		require.Equal(t, b.clusterID, guard.Header.ClusterId)
		require.Greater(t, guard.Header.Revision, int64(0))
		require.False(t, guard.More)
		require.Equal(t, int64(len(guard.Kvs)), guard.Count)
		require.LessOrEqual(t, len(guard.Kvs), 1)
		rpc, finish = partitionWorkerNativeRequest(retirement)
		lease, err := raw.TimeToLive(rpc, clientv3.LeaseID(oldReference.LeaseID))
		finish()
		require.NoError(t, err)
		require.NotNil(t, lease.ResponseHeader)
		require.Equal(t, b.clusterID, lease.ClusterId)
		require.Greater(t, lease.Revision, int64(0))
		require.EqualValues(t, oldReference.LeaseID, lease.ID)
		if len(guard.Kvs) == 0 && lease.TTL == -1 {
			break
		}
		partitionWorkerNativePause(t, retirement, 100*time.Millisecond)
	}

	// The replacement retains the original experiment lifetime, not this
	// bounded wait context which is canceled when maintenance returns.
	replacement, err := b.StartDueWorker(ctx, options)
	require.NoError(t, err)

	// Register immediately: failed synchronization must still join this new
	// actual actor. The enclosing caller also stops it before Backend.Close.
	t.Cleanup(func() {
		require.NoError(t, replacement.Stop())
		<-replacement.Done()
	})
	newReference := replacement.session.Reference()
	require.NotEqual(t, oldReference.SessionID, newReference.SessionID)
	require.NotEqual(t, oldReference.LeaseID, newReference.LeaseID)
	for {
		current := replacement.Status()
		require.True(t, current.Running, "%+v", current)
		require.Empty(t, current.LastError)
		if current.Synchronized {
			break
		}
		partitionWorkerNativePause(t, recovery, 100*time.Millisecond)
	}
	require.Equal(t, oldReference, oldSession.Reference(), "a new owner must not mutate or revive the retired original")
	t.Logf("METADATA_SCALE maintenance worker replacement original_session=%s original_lease=%d original_stop=%v original_done=true original_guard_absent=true original_lease_gone=true new_session=%s new_lease=%d synchronized=true", oldReference.SessionID, oldReference.LeaseID, stopError, newReference.SessionID, newReference.LeaseID)
	return replacement
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
	api := metadataScaleSampleBackend(t, ctx, b, options)
	defer api.Close()
	metadataScaleSampleWithBackend(t, ctx, api, count)
}

// Startup remains fail closed: New authenticates all configured endpoints while
// all members are available. Fault sampling reuses this original client; it
// does not claim that a new Backend can start with an unavailable endpoint.
func metadataScaleSampleBackend(t *testing.T, ctx context.Context, b *Backend, options Options) *Backend {
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
	return api
}

// One sample owns its observation wrappers and original operation leases. No
// worker connection or endpoint configuration is replaced during member loss.
func metadataScaleSampleWithBackend(t *testing.T, ctx context.Context, api *Backend, count int) {
	t.Helper()
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
		_, id, err := metadataScaleModel(api.namespace, ordinal, time.Now().UTC().Add(time.Hour))
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
func metadataScaleMaintenance(t *testing.T, ctx context.Context, raw *clientv3.Client, endpoints []string, children []*partitionWorkerNativeProcess, b *Backend, options Options, count int, diagnostics *metadataScaleMaintenanceDiagnostics) {
	t.Helper()
	diagnostics.mu.Lock()
	diagnostics.active, diagnostics.phase = true, "maintenance-entry"
	diagnostics.mu.Unlock()
	started := time.Now()
	diagnostics.state("maintenance-start", 0)
	defer func() { diagnostics.state("maintenance-return", time.Since(started)) }()

	bounded, stop := partitionWorkerNativeRequest(ctx)
	point, err := raw.Get(bounded, b.identityKey)
	stop()
	require.NoError(t, err)
	diagnostics.runPhase("compact", func() {
		bounded, stop = context.WithTimeout(ctx, time.Minute)
		_, err = raw.Compact(bounded, point.Header.Revision, clientv3.WithCompactPhysical())
		stop()
		require.NoError(t, err)
	})
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
		diagnostics.runPhase(fmt.Sprintf("defrag-member-%d", i), func() {
			bounded, stop = context.WithTimeout(ctx, 2*time.Minute)
			func() {
				epoch := diagnostics.beginDefrag()
				started := time.Now()
				_, err = raw.Defragment(bounded, endpoints[i])
				ended := time.Now()
				diagnostics.endDefrag(epoch, i, started, ended)
			}()
			stop()
			require.NoError(t, err)
			bounded, stop = partitionWorkerNativeRequest(ctx)
			_, err = raw.Status(bounded, endpoints[i])
			stop()
			require.NoError(t, err)
		})
	}
	i := order[0]
	bounded, stop = partitionWorkerNativeRequest(ctx)
	original, err := raw.Status(bounded, endpoints[i])
	stop()
	require.NoError(t, err)
	// Complete the original strict startup validation before stopping a member.
	// The tested availability property is quorum service on an existing client,
	// not successful all-endpoint validation during a degraded fresh startup.
	var api *Backend
	diagnostics.runPhase("fixed-api-startup", func() {
		api = metadataScaleSampleBackend(t, ctx, b, options)
	})
	defer api.Close()
	diagnostics.runPhase(fmt.Sprintf("kill-member-%d", i), func() {
		children[i].stop()
	})
	diagnostics.runPhase("fixed-api-degraded", func() {
		metadataScaleSampleWithBackend(t, ctx, api, count)
	})
	diagnostics.runPhase(fmt.Sprintf("restart-member-%d", i), func() {
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
	})
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
