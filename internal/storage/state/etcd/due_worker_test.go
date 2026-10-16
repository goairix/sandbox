package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The actor tests retain the actual generated KV/Lease/Watch connection. Only
// the trusted observation is movable; session deadlines and fences stay real.
type dueWorkerActorFixture struct {
	*dueExpiryFixture
	clock      atomic.Value
	watchCalls atomic.Int32
}

func newDueWorkerActorFixture(t *testing.T) *dueWorkerActorFixture {
	t.Helper()
	f := &dueWorkerActorFixture{dueExpiryFixture: newDueExpiryFixture(t)}
	require.NoError(t, f.b.ReleaseWorkerSession(context.Background(), f.session))
	f.clock.Store(f.now)
	f.b.authorityClock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: f.clock.Load().(time.Time)}, nil
	})
	f.watch(1, false, nil)
	return f
}

func dueWorkerActorOptions() DueWorkerOptions {
	return DueWorkerOptions{WorkerID: "resident-due-worker", Partitions: []uint8{0xaa}, RangeSlots: []uint16{0}, ClaimSlots: []uint16{0}, RelistSlot: 0, StageTTL: 30 * time.Second}
}

func (f *dueWorkerActorFixture) start(t *testing.T, options DueWorkerOptions) *DueWorker {
	t.Helper()
	w, err := f.b.StartDueWorker(context.Background(), options)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = w.Stop()
		workerTestReceive(t, w.Done())
	})
	return w
}

// This finite host script replays current PUTs relevant to these scenarios.
// It does not fabricate a progress certificate when an idle subscription starts
// at current+1. DELETE history and fragmented replay remain the accepted Watch
// protocol tests' responsibility; actor admissions remove their exact cache row.
func (f *dueWorkerActorFixture) watch(count int, suppress bool, stopped chan<- struct{}) {
	f.server.mu.Lock()
	f.server.watch = func(stream pb.Watch_WatchServer) error {
		f.watchCalls.Add(1)
		if stopped != nil {
			defer func() { stopped <- struct{}{} }()
		}
		creates, err := dueWatchTestCreates(stream, count, dueWatchTestHeader(f.server))
		if err != nil {
			return err
		}
		next := make([]int64, len(creates))
		for i, create := range creates {
			next[i] = create.StartRevision
		}

		for {
			request, err := stream.Recv()
			if err != nil {
				return nil
			}
			if request.GetProgressRequest() == nil {
				return fmt.Errorf("unexpected resident Watch request")
			}
			if suppress {
				continue
			}

			f.server.mu.Lock()
			header := f.server.header()
			rows := make([][]*mvccpb.KeyValue, len(creates))
			canProgress := true
			for i, create := range creates {
				canProgress = canProgress && header.Revision >= create.StartRevision
				for _, kv := range f.server.values {
					if kv.ModRevision >= next[i] && bytes.Compare(kv.Key, create.Key) >= 0 && bytes.Compare(kv.Key, create.RangeEnd) < 0 {
						rows[i] = append(rows[i], workerTestKV(kv))
					}
				}
			}
			f.server.mu.Unlock()

			for i, list := range rows {
				sort.Slice(list, func(a, b int) bool {
					if list[a].ModRevision != list[b].ModRevision {
						return list[a].ModRevision < list[b].ModRevision
					}
					return bytes.Compare(list[a].Key, list[b].Key) < 0
				})
				// One event per frame keeps this script independent of revision
				// grouping; the actual codec still observes all aggregate limits.
				for _, kv := range list {
					response := &pb.WatchResponse{Header: header, WatchId: creates[i].WatchId, Events: []*mvccpb.Event{{Type: mvccpb.PUT, Kv: kv}}}
					if err := stream.Send(response); err != nil {
						return nil
					}
					next[i] = kv.ModRevision + 1
				}
			}
			if canProgress {
				if err := stream.Send(&pb.WatchResponse{Header: header, WatchId: -1}); err != nil {
					return nil
				}
				for i := range next {
					next[i] = header.Revision + 1
				}
			}
		}
	}
	f.server.mu.Unlock()
}

func (f *dueWorkerActorFixture) tasks() []TaskRecord {
	f.server.mu.Lock()
	defer f.server.mu.Unlock()
	var tasks []TaskRecord
	for key, kv := range f.server.values {
		_, tail, found := strings.Cut(key, "/tasks/")
		if !found || strings.Contains(tail, "/") {
			continue
		}
		var task TaskRecord
		if json.Unmarshal(kv.Value, &task) == nil && task.Reference.TaskID != "" {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func (f *dueWorkerActorFixture) original(t *testing.T, w *DueWorker) *WorkerSession {
	t.Helper()
	id := w.Status().Session.SessionID
	f.b.archiveLife.mu.Lock()
	defer f.b.archiveLife.mu.Unlock()
	for _, s := range f.b.workerControl.sessions {
		if s != nil && s.reference.SessionID == id {
			return s
		}
	}
	t.Fatalf("original resident session %q is missing", id)
	return nil
}

// Seed another coherent metadata bundle, with a distinct workspace owner. This
// is model setup, not an admission stub: only the actor can create its task.
func (f *dueWorkerActorFixture) seedActive(t *testing.T, partition uint8, suffix string, expiry time.Time) DueCandidateRecord {
	t.Helper()
	runtime := RuntimeReference{ID: "pod-" + suffix, UID: uuid.NewString(), BootID: uuid.NewString()}
	hash := fmt.Sprintf("%02x", partition) + strings.ReplaceAll(uuid.NewString(), "-", "") + strings.Repeat("0", 30)
	c := SandboxControlRecord{Version: 1, SandboxID: "actor-" + suffix, WorkspaceHash: hash, IntentID: "intent-" + suffix, RestoreEpoch: f.b.restoreEpoch, Generation: 1, DataGateEpoch: 1, Phase: PhaseActive, Snapshot: f.control.Snapshot, Runtime: &runtime, ExpiresAt: expiry.UTC()}
	p := SandboxPlacementRecord{Version: 1, SandboxID: c.SandboxID, WorkspaceHash: hash, IntentID: c.IntentID, RestoreEpoch: c.RestoreEpoch, Generation: 1, Partition: partition}
	o := WorkspaceOwnerRecord{Version: 1, WorkspaceHash: hash, SandboxID: c.SandboxID, IntentID: c.IntentID, RestoreEpoch: c.RestoreEpoch, Generation: 1, Runtime: &runtime}
	wf := WorkspaceFenceRecord{Version: 1, WorkspaceHash: hash, RestoreEpoch: c.RestoreEpoch, Generation: 1}
	ri := RuntimeIndexRecord{Version: 1, IntentID: c.IntentID, SandboxID: c.SandboxID, WorkspaceHash: hash, RestoreEpoch: c.RestoreEpoch, Generation: 1, Runtime: runtime}
	placementKey, err := f.b.namespace.placementKey(c.SandboxID)
	require.NoError(t, err)
	controlKey, _, err := f.b.namespace.sandboxKeys(partition, c.SandboxID, "read")
	require.NoError(t, err)
	ownerKey, err := f.b.namespace.Key("p", fmt.Sprintf("%02x", partition), "workspaces", hash, "owner")
	require.NoError(t, err)
	fenceKey, err := f.b.namespace.Key("p", fmt.Sprintf("%02x", partition), "workspaces", hash, "fence")
	require.NoError(t, err)
	indexKey, err := f.b.namespace.runtimeIndexKey(runtime.UID)
	require.NoError(t, err)
	keys := []string{placementKey, controlKey, ownerKey, fenceKey, indexKey}
	for i, record := range []interface{ Validate() error }{p, c, o, wf, ri} {
		wire, err := encodeDomainRecord(record)
		require.NoError(t, err)
		f.server.put(keys[i], wire, 0)
	}
	candidate := dueCandidateForControl(partition, c)
	key, err := f.b.namespace.dueCandidateKey(candidate)
	require.NoError(t, err)
	wire, err := encodeDueCandidate(candidate)
	require.NoError(t, err)
	f.server.put(key, wire, 0)
	return candidate
}

func dueWorkerActorWait(t *testing.T, predicate func() bool) {
	t.Helper()
	require.Eventually(t, predicate, 8*time.Second, 10*time.Millisecond)
}

func TestDueWorkerActorActualExpiryAndStatus(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	owner := f.server.point(f.keys[2])
	w := f.start(t, dueWorkerActorOptions())
	dueWorkerActorWait(t, func() bool { return len(f.tasks()) == 1 && w.Status().Synchronized })

	tasks := f.tasks()
	task, err := f.b.LoadTask(context.Background(), tasks[0].Reference)
	require.NoError(t, err)
	require.Equal(t, f.candidate.ExpiresAt, task.ExpiresAt)
	require.Equal(t, owner, f.server.point(f.keys[2]))
	dueKey, _ := f.b.namespace.dueCandidateKey(f.candidate)
	require.Nil(t, f.server.point(dueKey))
	status := w.Status()
	require.True(t, status.Running)
	require.Len(t, status.Partitions, 1)
	require.Positive(t, status.Partitions[0].CoveredRevision)
	require.GreaterOrEqual(t, status.Partitions[0].ObservedRevision, status.Partitions[0].CoveredRevision)
	require.LessOrEqual(t, status.OwnedBytes, uint64(64<<20))
	status.Partitions[0].Frontier = "caller-owned-copy"
	require.NotEqual(t, status.Partitions[0].Frontier, w.Status().Partitions[0].Frontier)

	// Repeated finite native batches must not create a second destroy task.
	before := f.watchCalls.Load()
	dueWorkerActorWait(t, func() bool { return f.watchCalls.Load() >= before+2 })
	require.Len(t, f.tasks(), 1)
}

func TestDueWorkerActorConfigurationAndOwnedOptions(t *testing.T) {
	for _, scenario := range []string{"nil-context", "duplicate-partition", "empty-range", "duplicate-range", "range-id", "empty-claim", "duplicate-claim", "claim-id", "relist-id", "ttl-margin"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDueWorkerActorFixture(t)
			o := dueWorkerActorOptions()
			ctx := context.Background()
			switch scenario {
			case "nil-context":
				ctx = nil
			case "duplicate-partition":
				o.Partitions = []uint8{0xaa, 0xaa}
			case "empty-range":
				o.RangeSlots = nil
			case "duplicate-range":
				o.RangeSlots = []uint16{0, 0}
			case "range-id":
				o.RangeSlots = []uint16{20}
			case "empty-claim":
				o.ClaimSlots = nil
			case "duplicate-claim":
				o.ClaimSlots = []uint16{0, 0}
			case "claim-id":
				o.ClaimSlots = []uint16{100}
			case "relist-id":
				o.RelistSlot = 4
			case "ttl-margin":
				f.b.workerControl.options.SessionTTL = 3 * time.Second
				f.b.requestTimeout = time.Second
			}
			before, _, _ := f.server.counts()
			_, err := f.b.StartDueWorker(ctx, o)
			require.Error(t, err)
			after, _, _ := f.server.counts()
			require.Equal(t, before, after, "invalid startup cannot create a lease")
		})
	}

	f := newDueWorkerActorFixture(t)
	o := dueWorkerActorOptions()
	w := f.start(t, o)
	o.Partitions[0], o.RangeSlots[0], o.ClaimSlots[0] = 0xbb, 19, 99
	dueWorkerActorWait(t, func() bool { return w.Status().Synchronized })
	require.Equal(t, uint8(0xaa), w.Status().Partitions[0].Partition)
	s := f.original(t, w)
	f.b.archiveLife.mu.Lock()
	configured, unconfigured := s.partitions[0xaa], s.partitions[0xbb]
	f.b.archiveLife.mu.Unlock()
	require.NotNil(t, configured)
	require.Nil(t, unconfigured)
}

func TestDueWorkerActorOverduePrefixBeforeFullCache(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	f.seed(t, 10001, f.candidate.ExpiresAt)
	f.watch(1, true, nil)
	w := f.start(t, dueWorkerActorOptions())
	dueWorkerActorWait(t, func() bool { return len(f.tasks()) == 1 })

	// The real active row sorts before the discovery-only rows. Its task and
	// durable prefix must progress while the >10000-row view is incomplete.
	checkpoint := dueCheckpointTestRaw(t, f.duePageFixture)
	key, _ := f.b.namespace.dueCandidateKey(f.candidate)
	dueWorkerActorWait(t, func() bool {
		kv := f.server.point(dueCheckpointTestKey(f.duePageFixture))
		var record dueCheckpointRecord
		return kv != nil && json.Unmarshal(kv.Value, &record) == nil && record.Frontier >= key
	})
	require.Positive(t, checkpoint.Revision)
	require.False(t, w.Status().Synchronized)
	require.LessOrEqual(t, w.Status().OwnedBytes, uint64(64<<20))
	f.server.mu.Lock()
	var bounded bool
	for _, request := range f.server.requests {
		for _, op := range request.Success {
			if r := op.GetRequestRange(); r != nil && len(r.RangeEnd) > 0 && r.Limit == 118 {
				bounded = true
			}
		}
	}
	f.server.mu.Unlock()
	require.True(t, bounded, "actor must use the actual bounded due page")
}

func TestDueWorkerActorEarlyTailAndExpiryWithoutWrite(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	f.seed(t, 120, f.now.Add(30*time.Second))
	future := f.seedActive(t, 0xaa, "future", f.now.Add(2*time.Second))
	w := f.start(t, dueWorkerActorOptions())
	dueWorkerActorWait(t, func() bool { return w.Status().Synchronized && w.Status().Queued == 121 })
	key, _ := f.b.namespace.dueCandidateKey(f.candidate)
	require.Equal(t, key, dueCheckpointTestRaw(t, f.duePageFixture).Frontier)
	require.Equal(t, "watch", dueCheckpointTestRaw(t, f.duePageFixture).Phase, "the actual final early-tail page must durably complete list phase")
	require.LessOrEqual(t, w.Status().OwnedBytes, uint64(64<<20))

	// Moving only the trusted clock makes the existing candidate due. No
	// candidate rewrite or fake Watch heartbeat may be required to schedule it.
	f.clock.Store(f.now.Add(3 * time.Second))
	dueWorkerActorWait(t, func() bool { return len(f.tasks()) == 2 })
	futureKey, _ := f.b.namespace.dueCandidateKey(future)
	require.Nil(t, f.server.point(futureKey))
	require.Equal(t, key, dueCheckpointTestRaw(t, f.duePageFixture).Frontier, "cache settlement cannot cross the early list hole")
}

func TestDueWorkerActorLateInsertBelowFrontier(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	w := f.start(t, dueWorkerActorOptions())
	dueWorkerActorWait(t, func() bool { return len(f.tasks()) == 1 && w.Status().Synchronized })
	frontier := dueCheckpointTestRaw(t, f.duePageFixture).Frontier
	late := f.seedActive(t, 0xaa, "late", f.now.Add(-2*time.Second))
	lateKey, _ := f.b.namespace.dueCandidateKey(late)
	require.Less(t, lateKey, frontier)
	dueWorkerActorWait(t, func() bool { return len(f.tasks()) == 2 })
	require.Nil(t, f.server.point(lateKey))
	require.Equal(t, frontier, dueCheckpointTestRaw(t, f.duePageFixture).Frontier)
}

func TestDueWorkerActorMultipleOriginalShares(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	f.seedActive(t, 0xbb, "second-partition", f.now.Add(-time.Second))
	f.watch(2, false, nil)
	o := dueWorkerActorOptions()
	o.Partitions, o.RangeSlots, o.ClaimSlots = []uint8{0xaa, 0xbb}, []uint16{0, 1}, []uint16{0, 1}
	w := f.start(t, o)
	dueWorkerActorWait(t, func() bool { return len(f.tasks()) == 2 && w.Status().Synchronized })
	s := f.original(t, w)
	f.b.archiveLife.mu.Lock()
	var claims, ranges []string
	for _, share := range s.budgets {
		if share == nil {
			continue
		}
		if share.kind == WorkerBudgetClaim {
			claims = append(claims, share.fence.key)
		}
		if share.kind == WorkerBudgetRange {
			ranges = append(ranges, share.fence.key)
		}
	}
	f.b.archiveLife.mu.Unlock()
	require.Len(t, claims, 2)
	require.Len(t, ranges, 2)

	usedClaims, usedRanges := make(map[string]bool), make(map[string]bool)
	f.server.mu.Lock()
	for _, request := range f.server.requests {
		isAdmission, isRange := false, false
		for _, op := range request.Success {
			if d := op.GetRequestDeleteRange(); d != nil && strings.Contains(string(d.Key), "/due/") {
				isAdmission = true
			}
			if r := op.GetRequestRange(); r != nil && len(r.RangeEnd) > 0 {
				isRange = true
			}
		}
		for _, comparison := range request.Compare {
			for _, key := range claims {
				if isAdmission && string(comparison.Key) == key {
					usedClaims[key] = true
				}
			}
			for _, key := range ranges {
				if isRange && string(comparison.Key) == key {
					usedRanges[key] = true
				}
			}
		}
	}
	f.server.mu.Unlock()
	require.Len(t, usedClaims, 2, "earned claim turns must rotate actual original shares")
	require.Len(t, usedRanges, 2, "each partition cursor remains pinned to its original range share")
}

func TestDueWorkerActorUnknownAttemptRecovery(t *testing.T) {
	for _, scenario := range []string{"pending-ack", "grant-ack", "commit-ack"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDueWorkerActorFixture(t)
			originalKV, originalLease := f.b.client.KV, f.b.client.Lease
			var intercepted atomic.Bool
			var stageGrants atomic.Int32
			f.b.client.Lease = &workerTestLease{Lease: originalLease, grant: func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
				response, err := originalLease.Grant(ctx, ttl)
				if ttl == 30 {
					stageGrants.Add(1)
					if scenario == "grant-ack" && !intercepted.Swap(true) {
						return nil, context.DeadlineExceeded
					}
				}
				return response, err
			}}
			if scenario != "grant-ack" {
				match := putsKey(f.keys[1])
				if scenario == "pending-ack" {
					match = func(ops []clientv3.Op) bool {
						for _, op := range ops {
							if string(op.KeyBytes()) == dueCheckpointTestKey(f.duePageFixture) && bytes.Contains(op.ValueBytes(), []byte(`"pending":{`)) {
								return true
							}
						}
						return false
					}
				}
				f.b.client.KV = &faultKV{KV: originalKV, match: match, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
					if err == nil && !intercepted.Swap(true) {
						return nil, context.DeadlineExceeded
					}
					return response, err
				}}
			}

			w := f.start(t, dueWorkerActorOptions())
			workerTestReceive(t, w.Done())
			require.True(t, intercepted.Load())
			require.Equal(t, DueWorkerPhase("unknown"), w.Status().Phase)
			pending := dueCheckpointTestRaw(t, f.duePageFixture)
			require.NotNil(t, pending.Pending)
			require.Empty(t, pending.Frontier)
			if scenario == "pending-ack" {
				require.Zero(t, stageGrants.Load(), "unknown pre-Grant persistence must prevent Grant")
			}
			if scenario == "commit-ack" {
				require.Len(t, f.tasks(), 1)
			}

			// Replacement starts only after the original owner has physically
			// stopped. Its ctor must resolve the exact persisted request first.
			f.b.client.KV, f.b.client.Lease = originalKV, originalLease
			replacement := f.start(t, dueWorkerActorOptions())
			dueWorkerActorWait(t, func() bool { return replacement.Status().Synchronized && len(f.tasks()) == 1 })
			require.Nil(t, dueCheckpointTestRaw(t, f.duePageFixture).Pending)
			require.NotEqual(t, w.Status().Session.SessionID, replacement.Status().Session.SessionID)
			require.Len(t, f.tasks(), 1)
		})
	}
}

func TestDueWorkerActorWindowRollover(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	f.seedActive(t, 0xaa, "next-window", f.now.Add(61*time.Second))
	w := f.start(t, dueWorkerActorOptions())
	dueWorkerActorWait(t, func() bool { return w.Status().Synchronized && len(f.tasks()) == 1 })
	key := dueCheckpointTestKey(f.duePageFixture)
	before := f.server.point(key)
	old := dueCheckpointTestRaw(t, f.duePageFixture)
	require.Zero(t, w.Status().Queued)

	// windowAt is only the actor's local rollover schedule. The original
	// session deadline, candidate and checkpoint authority are untouched.
	f.clock.Store(f.now.Add(2 * time.Second))
	f.b.archiveLife.mu.Lock()
	w.windowAt = time.Now().Add(-time.Second)
	f.b.archiveLife.mu.Unlock()
	dueWorkerActorWait(t, func() bool {
		status := w.Status()
		return status.Synchronized && status.Queued == 1 && status.Partitions[0].Revision > old.Revision
	})
	after := f.server.point(key)
	require.Equal(t, before.CreateRevision, after.CreateRevision, "fresh view must CAS the existing permanent checkpoint")
	require.Greater(t, after.ModRevision, before.ModRevision)
	require.Greater(t, dueCheckpointTestRaw(t, f.duePageFixture).WindowEnd.UnixNano(), old.WindowEnd.UnixNano())
	f.server.mu.Lock()
	var fresh int
	for _, request := range f.server.requests {
		for _, op := range request.Success {
			if r := op.GetRequestRange(); r != nil && len(r.RangeEnd) > 0 && r.Revision == 0 {
				fresh++
			}
		}
	}
	f.server.mu.Unlock()
	require.Equal(t, 2, fresh, "rollover must actually call ReadDuePage(nil)")
}

func TestDueWorkerActorNoCreditBackoffAndRenewal(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	f.b.workerControl.options.SessionTTL = 4 * time.Second
	f.b.requestTimeout = time.Second
	w := f.start(t, dueWorkerActorOptions())
	reference := w.Status().Session
	dueWorkerActorWait(t, func() bool { return w.Status().Renewals >= 2 })
	require.Equal(t, reference, w.Status().Session, "idle renewal retains the original lease and incarnation")
	require.True(t, w.Status().Running)
	require.Less(t, f.watchCalls.Load(), int32(40), "idle diagnostic timeout must not spin")
	_, keeps, _ := f.server.counts()
	require.GreaterOrEqual(t, keeps, 2)
	require.Len(t, f.tasks(), 1)
}

func TestDueWorkerActorOriginalFenceLoss(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	w := f.start(t, dueWorkerActorOptions())
	dueWorkerActorWait(t, func() bool { return w.Status().Synchronized })
	s := f.original(t, w)
	profile := f.server.point(f.b.workerControl.profile.key)
	f.server.put(f.b.workerControl.profile.key, string(profile.Value), profile.Lease)
	workerTestReceive(t, w.Done())
	require.False(t, w.Status().Running)
	require.False(t, w.Status().Synchronized)
	require.NotEmpty(t, w.Status().LastError)
	require.Nil(t, f.server.point(s.guard.key))
}

func TestDueWorkerActorCachedAdmissionPublishesPending(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	f.seedActive(t, 0xaa, "cached-pending", f.now.Add(2*time.Second))
	block := newDueWorkerLifetimeBlock()
	var armed atomic.Bool
	original := f.b.client.Lease
	f.b.client.Lease = &workerTestLease{Lease: original, grant: func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
		if ttl == 30 && armed.Swap(false) {
			// This is the real Stage Grant after the private binding has
			// positively persisted pending on the original charged owner.
			block.hold(ctx)
		}
		return original.Grant(ctx, ttl)
	}}
	w := f.start(t, dueWorkerActorOptions())
	t.Cleanup(block.unblock)
	dueWorkerActorWait(t, func() bool { return w.Status().Synchronized && w.Status().Queued == 1 && len(f.tasks()) == 1 })
	covered := w.Status().Partitions[0].CoveredRevision
	frontier := dueCheckpointTestRaw(t, f.duePageFixture).Frontier
	armed.Store(true)
	f.clock.Store(f.now.Add(3 * time.Second))
	workerTestReceive(t, block.entered)

	permanent := dueCheckpointTestRaw(t, f.duePageFixture)
	require.NotNil(t, permanent.Pending)
	status := w.Status()
	require.False(t, status.Synchronized, "paused native admission cannot publish current synchronization")
	require.False(t, status.Partitions[0].Synchronized)
	require.NotEqual(t, DueWorkerPhase("watching"), status.Phase)
	require.Equal(t, permanent.Pending.Reference, status.Partitions[0].Pending)
	require.GreaterOrEqual(t, status.Partitions[0].CoveredRevision, covered, "historical proof survives current publication invalidation")
	require.Equal(t, 1, status.Queued)

	block.unblock()
	dueWorkerActorWait(t, func() bool { return w.Status().Synchronized && w.Status().Queued == 0 && len(f.tasks()) == 2 })
	require.Equal(t, StageReference{}, w.Status().Partitions[0].Pending)
	require.Nil(t, dueCheckpointTestRaw(t, f.duePageFixture).Pending)
	require.Equal(t, frontier, dueCheckpointTestRaw(t, f.duePageFixture).Frontier)
}

func TestDueWorkerActorSaturatedWindowExpiry(t *testing.T) {
	for _, scenario := range []string{"already-due", "future-clock-only"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDueWorkerActorFixture(t)
			// Remove only the original fixture's discovery row. This case's
			// canonical active bundle is independently seeded at the valid
			// date boundary; authority and session deadlines remain real.
			oldKey, err := f.b.namespace.dueCandidateKey(f.candidate)
			require.NoError(t, err)
			request, cancel := context.WithTimeout(context.Background(), time.Second)
			_, err = f.b.client.Txn(request).Then(clientv3.OpDelete(oldKey)).Commit()
			cancel()
			require.NoError(t, err)
			observed := time.Date(9999, 12, 31, 23, 59, 45, 0, time.UTC)
			expiry := observed.Add(-15 * time.Second)
			if scenario == "future-clock-only" {
				expiry = observed.Add(10 * time.Second)
			}
			candidate := f.seedActive(t, 0xaa, "saturated", expiry)
			f.clock.Store(observed)
			w := f.start(t, dueWorkerActorOptions())
			maximum := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
			if scenario == "future-clock-only" {
				dueWorkerActorWait(t, func() bool { return w.Status().Synchronized && w.Status().Queued == 1 })
				require.Empty(t, f.tasks())
				record := dueCheckpointTestRaw(t, f.duePageFixture)
				require.Equal(t, maximum, record.WindowEnd)
				require.Empty(t, record.Frontier)
				// No candidate/control/checkpoint write makes this input due.
				f.clock.Store(expiry.Add(time.Second))
			}

			dueWorkerActorWait(t, func() bool { return len(f.tasks()) == 1 && w.Status().Synchronized && w.Status().Queued == 0 })
			task := f.tasks()[0]
			require.Equal(t, candidate.SandboxID, task.Reference.SandboxID)
			require.Equal(t, expiry, task.ExpiresAt)
			key, err := f.b.namespace.dueCandidateKey(candidate)
			require.NoError(t, err)
			require.Nil(t, f.server.point(key))
			record := dueCheckpointTestRaw(t, f.duePageFixture)
			require.Equal(t, maximum, record.WindowEnd)
			require.Equal(t, record.Revision+1, record.ReplayFrom)
			if scenario == "already-due" {
				require.Equal(t, key, record.Frontier)
			} else {
				require.Empty(t, record.Frontier, "cache work must preserve the original early hole")
			}
		})
	}
}

func TestDueWorkerActorCachedKnownAbortRetainsCandidate(t *testing.T) {
	f := newDueWorkerActorFixture(t)
	candidate := f.seedActive(t, 0xaa, "known-abort", f.now.Add(2*time.Second))
	controlKey, _, err := f.b.namespace.sandboxKeys(candidate.Partition, candidate.SandboxID, "read")
	require.NoError(t, err)
	type resolution struct {
		reference StageReference
		outcome   Outcome
		err       error
	}
	resolved := make(chan resolution, 1)
	var resolvedOnce atomic.Bool
	var pages, grants atomic.Int32
	grantTimes := make(chan time.Time, 3)
	retryBlock := newDueWorkerLifetimeBlock()
	originalKV, originalLease := f.b.client.KV, f.b.client.Lease
	observedKV := &faultKV{KV: originalKV, match: duePageRangeOps, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		pages.Add(1)
		return response, err
	}}
	f.b.client.KV = &faultKV{KV: observedKV, match: putsKey(controlKey), before: func() {
		if resolvedOnce.Swap(true) {
			return
		}
		kv := f.server.point(dueCheckpointTestKey(f.duePageFixture))
		var record dueCheckpointRecord
		if kv == nil || json.Unmarshal(kv.Value, &record) != nil || record.Pending == nil {
			resolved <- resolution{err: fmt.Errorf("real commit reached without its durable pending")}
			return
		}
		// Exact public arbitration writes the same original receipt while
		// this actual commit is still undispatched. CommitStage must then
		// observe OutcomeAborted,nil, not a fabricated transport error.
		request, cancel := context.WithTimeout(context.Background(), time.Second)
		outcome, err := f.b.ResolveStage(request, record.Pending.Reference)
		cancel()
		resolved <- resolution{reference: record.Pending.Reference, outcome: outcome, err: err}
	}}
	f.b.client.Lease = &workerTestLease{Lease: originalLease, grant: func(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
		if ttl == 30 {
			count := grants.Add(1)
			grantTimes <- time.Now()
			if count == 3 {
				retryBlock.hold(ctx)
			}
		}
		return originalLease.Grant(ctx, ttl)
	}}
	w := f.start(t, dueWorkerActorOptions())
	t.Cleanup(retryBlock.unblock)
	dueWorkerActorWait(t, func() bool { return w.Status().Synchronized && w.Status().Queued == 1 && len(f.tasks()) == 1 })
	baselinePages := pages.Load()
	frontier := dueCheckpointTestRaw(t, f.duePageFixture).Frontier
	f.clock.Store(f.now.Add(3 * time.Second))
	aborted := workerTestReceive(t, resolved)
	require.NoError(t, aborted.err)
	require.Equal(t, OutcomeAborted, aborted.outcome)
	workerTestReceive(t, retryBlock.entered)

	// The independently charged retry is held at its real Grant, so the
	// candidate and exact new pending can be inspected before it commits.
	require.Equal(t, int32(3), grants.Load())
	_ = workerTestReceive(t, grantTimes) // Original fixture's first task.
	abortAt, retryAt := workerTestReceive(t, grantTimes), workerTestReceive(t, grantTimes)
	require.GreaterOrEqual(t, retryAt.Sub(abortAt), 800*time.Millisecond)
	require.Equal(t, baselinePages, pages.Load(), "known abort must retry from retained cache without a fresh Range")
	require.Equal(t, 1, w.Status().Queued)
	pending := dueCheckpointTestRaw(t, f.duePageFixture)
	require.NotNil(t, pending.Pending)
	require.Equal(t, candidate, pending.Pending.Candidate)
	require.NotEqual(t, aborted.reference, pending.Pending.Reference)
	dueKey, err := f.b.namespace.dueCandidateKey(candidate)
	require.NoError(t, err)
	require.NotNil(t, f.server.point(dueKey))
	require.Len(t, f.tasks(), 1)
	s := f.original(t, w)
	f.b.archiveLife.mu.Lock()
	var tokens uint64
	for _, share := range s.budgets {
		if share != nil && share.kind == WorkerBudgetClaim {
			tokens += share.bucket.tokens
		}
	}
	f.b.archiveLife.mu.Unlock()
	require.Zero(t, tokens, "retry must consume its own earned original claim token")

	retryBlock.unblock()
	dueWorkerActorWait(t, func() bool { return w.Status().Synchronized && w.Status().Queued == 0 && len(f.tasks()) == 2 })
	require.Nil(t, f.server.point(dueKey))
	require.Equal(t, baselinePages, pages.Load())
	require.Equal(t, frontier, dueCheckpointTestRaw(t, f.duePageFixture).Frontier)
	require.Equal(t, int32(3), grants.Load())
}
