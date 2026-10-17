package etcd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
)

// This serial host harness registers real jobs in the existing actor and uses
// actual signed publication/release history. It never starts physical work or
// treats the signed model as evidence about a real sandbox's death.
type cleanupColdQuotaFixture struct {
	*coldQuotaHistoryFixture
	w         *DueWorker
	job       *cleanupLocalJob
	history   *coldQuotaReturnHistory
	slotKey   string
	freeKey   string
	returnKey string
	birthSeed cleanupBirthTestSeed
}

func newCleanupColdQuotaFixture(t *testing.T) *cleanupColdQuotaFixture {
	t.Helper()
	historyFixture, seed := newCleanupColdQuotaHistoryFixture(t)
	f := &cleanupColdQuotaFixture{coldQuotaHistoryFixture: historyFixture, birthSeed: seed}
	b := f.b
	history, err := b.readColdQuotaReturnHistory(f.owner.context, f.workspace, f.claim.reference.IntentID, f.task)
	require.NoError(t, err)
	f.history = history
	// The baseline owner belongs to the fixture, not the new coordinator. Free
	// its registration so MaxActive=1 exercises the coordinator's real lifetime.
	f.owner.finish()
	b.localCleanup = &LocalCleanupOptions{MaxActive: 32, ColdRuntimeQuotaReturn: true}
	ctx, cancel := context.WithCancel(f.session.ctx)
	f.w = &DueWorker{backend: b, session: f.session, ctx: ctx, cancel: cancel,
		options: DueWorkerOptions{AutomaticCleanup: true},
		ranges:  []*WorkerBudgetShare{f.ranges}, claims: []*WorkerBudgetShare{f.claims},
		cleanupWake: make(chan struct{}, 1)}
	f.w.self = f.w
	scan := &dueWorkerScan{backend: b, partition: f.part, ranges: f.ranges, claims: f.claims}
	f.w.scans = []*dueWorkerScan{scan}
	jobctx, jobcancel := context.WithCancel(ctx)
	taskKey, err := b.namespace.taskKey(f.task)
	require.NoError(t, err)
	taskKV := f.server.point(taskKey)
	var task TaskRecord
	require.NoError(t, decodeTaskRecord(taskKV, &task))
	f.job = &cleanupLocalJob{worker: f.w, task: f.task, partition: f.part,
		ctx: jobctx, cancel: jobcancel, stopCaller: func() {}, done: make(chan struct{}),
		step: "cold-quota-history", proof: history.release,
		bundle: &taskClaimBundle{task: task, birth: taskKV.CreateRevision, fences: []taskFence{ownTaskFence(taskKV)}},
		target: &cleanupLocalTarget{plan: TaskWorkspaceSyncPlan{workspace: f.workspace}}}
	f.w.cleanupJobs[0] = f.job
	require.NoError(t, f.w.captureCleanupColdQuotaOrigin(f.job, history))
	readyKey, err := b.namespace.Key("preparation", "inventory", "ready")
	require.NoError(t, err)
	readyWire, err := coldEncode(preparationInventoryRecord{Version: 1, RestoreEpoch: b.restoreEpoch,
		ProfileDigest: b.preparationDigest, Next: 33, Total: 33, Ready: true})
	require.NoError(t, err)
	f.slotKey, f.freeKey, err = b.namespace.coldSlotKeys(history.reservation.Quota)
	require.NoError(t, err)
	slotWire, err := coldEncode(coldSlotRecord{Version: 1, RestoreEpoch: b.restoreEpoch,
		ProfileDigest: b.preparationDigest, Slot: history.reservation.Quota, Occupied: true, Owner: history.reservation.Owner})
	require.NoError(t, err)
	_, err = b.client.Txn(context.Background()).Then(clientv3.OpPut(readyKey, readyWire), clientv3.OpPut(f.slotKey, slotWire)).Commit()
	require.NoError(t, err)
	f.returnKey, err = b.namespace.coldRuntimeQuotaReturnKey(f.task.Partition, task.CreationIntentID)
	require.NoError(t, err)
	f.server.mu.Lock()
	f.server.requests = nil
	f.server.mu.Unlock()
	t.Cleanup(func() {
		cancel()
		// No actor goroutine is started. The serial cleanup entry still must
		// release its owned enrolled registration after actual calls return.
		f.w.stopCleanupJobs()
	})
	return f
}

func (f *cleanupColdQuotaFixture) turn(t *testing.T) {
	t.Helper()
	f.fund()
	_, err := f.w.cleanupTurn()
	require.NoError(t, err)
	require.NoError(t, f.job.err)
}

func (f *cleanupColdQuotaFixture) requests() []*pb.TxnRequest {
	return coldQuotaHistoryRequests(f.coldQuotaHistoryFixture)
}

func (f *cleanupColdQuotaFixture) verify(t *testing.T) {
	t.Helper()
	for turn := 0; turn < 32; turn++ {
		if f.w.validateCleanupColdQuotaVerified(f.job) == nil {
			return
		}
		f.turn(t)
	}
	t.Fatal("actual cleanupTurn never published verified quota")
}

// Charging the ordinary cleanup Claim before entering a cold metadata phase
// either deadlocks session.operation or consumes a Claim for a Range-only read.
func TestDueWorkerColdQuotaActorPaidPhases(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	groups := coldQuotaHistoryGroups(t, f.coldQuotaHistoryFixture)
	originalSlot := f.server.point(f.slotKey)
	grants, _, revokes := f.server.counts()
	for phase, group := range groups {
		f.fund()
		f.b.archiveLife.mu.Lock()
		claimBefore := f.claims.bucket
		f.claims.bucket.tokens = 0
		claimBefore.tokens = 0
		f.b.archiveLife.mu.Unlock()
		_, err := f.w.cleanupTurn()
		require.NoError(t, err)
		require.NoError(t, f.job.err)
		requests := f.requests()
		require.Len(t, requests, phase+1)
		require.Equal(t, taskProtoPoints(append([]string{f.b.identityKey, f.b.restoreKey}, group...)), requests[phase].Success)
		require.Len(t, requests[phase].Compare, 20)
		f.b.archiveLife.mu.Lock()
		claimAfter, operation := f.claims.bucket, f.session.operation
		f.b.archiveLife.mu.Unlock()
		require.Equal(t, claimBefore, claimAfter)
		require.Nil(t, operation, "each metadata call has actually returned and finished")
		workspace := f.w.coldQuota
		require.NotNil(t, workspace)
		_, err = f.w.cleanupTurn()
		require.NoError(t, err)
		require.Same(t, workspace, f.w.coldQuota, "credit refusal preserves the original workspace and phase")
		require.Len(t, f.requests(), phase+1)
	}
	f.verify(t)
	slotKV, freeKV, returnKV := f.server.point(f.slotKey), f.server.point(f.freeKey), f.server.point(f.returnKey)
	require.NotNil(t, slotKV)
	require.NotNil(t, freeKV)
	require.NotNil(t, returnKV)
	var slot coldSlotRecord
	require.NoError(t, coldDecode(slotKV, &slot))
	require.False(t, slot.Occupied)
	require.Equal(t, ColdSlotOwner{}, slot.Owner)
	require.Equal(t, f.history.reservation.Quota, slot.Slot)
	require.Equal(t, originalSlot.CreateRevision, slotKV.CreateRevision)
	require.Equal(t, originalSlot.Version+1, slotKV.Version)
	require.Equal(t, slotKV.Value, freeKV.Value)
	require.Equal(t, slotKV.ModRevision, freeKV.CreateRevision)
	require.Equal(t, slotKV.ModRevision, returnKV.CreateRevision)
	require.Equal(t, freeKV.CreateRevision, freeKV.ModRevision)
	gotGrants, _, gotRevokes := f.server.counts()
	require.Equal(t, grants+1, gotGrants, "only quota Stage may Grant in this metadata-only job")
	require.Equal(t, revokes+1, gotRevokes, "the original Stage cleanup joined before proof publication")
	require.Nil(t, f.w.coldQuota, "completion drops the heavy borrowed graph")
	require.False(t, f.job.started, "quota-only jobs never start the physical goroutine")
	require.Empty(t, f.job.runID)
}

// A validated return record alone is insufficient: stopping before its own
// receipt read must leave the original occupied E/S settlement gate closed.
func TestDueWorkerColdQuotaRequiresFinalPaidReceipt(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	for turn := 0; turn < 32 && f.server.point(f.returnKey) == nil; turn++ {
		f.turn(t)
	}
	require.NotNil(t, f.server.point(f.returnKey))
	require.Error(t, f.w.validateCleanupColdQuotaVerified(f.job))
	f.turn(t) // Q1: validate the return record, then yield.
	require.Error(t, f.w.validateCleanupColdQuotaVerified(f.job))
	require.NotNil(t, f.w.coldQuota)
	f.b.archiveLife.mu.Lock()
	f.ranges.bucket.tokens = 0
	f.b.archiveLife.mu.Unlock()
	before := len(f.requests())
	_, err := f.w.cleanupTurn()
	require.NoError(t, err)
	require.Len(t, f.requests(), before)
	require.Error(t, f.w.validateCleanupColdQuotaVerified(f.job))
	f.turn(t) // Q2 authenticates the committed original receipt.
	require.NoError(t, f.w.validateCleanupColdQuotaVerified(f.job))
}

// A committed replay has no rights over today's mutable quota slot. It must
// authenticate immutable history and return/receipt, then skip fresh Stage.
func TestDueWorkerColdQuotaActorCommittedReplay(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	first, err := f.b.SettleColdRuntimeQuota(context.Background(), f.workspace, f.claim.reference.IntentID, f.task)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, first.Outcome)
	f.server.put(f.slotKey, "belongs to a later quota owner", 0)
	f.server.mu.Lock()
	f.server.requests = nil
	f.server.mu.Unlock()
	grants, _, revokes := f.server.counts()
	f.verify(t)
	gotGrants, _, gotRevokes := f.server.counts()
	require.Equal(t, grants, gotGrants)
	require.Equal(t, revokes, gotRevokes)
	require.Equal(t, "belongs to a later quota owner", string(f.server.point(f.slotKey).Value))
	for _, request := range f.requests() {
		for _, op := range request.Success {
			if get := op.GetRequestRange(); get != nil {
				require.NotEqual(t, f.slotKey, string(get.Key), "replay must precede every mutable-slot read")
			}
		}
	}
}

// The proof source must remain bound to the registered original job after the
// shared workspace is released. A copied public entry or copied job is data.
func TestDueWorkerColdQuotaVerifiedGateOriginalMembership(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	f.verify(t)
	copyJob := *f.job
	require.Error(t, f.w.validateCleanupColdQuotaVerified(&copyJob))
	f.w.cleanupJobs[0] = &copyJob
	require.Error(t, f.w.validateCleanupColdQuotaVerified(&copyJob))
	require.Error(t, f.w.validateCleanupColdQuotaVerified(f.job))
	f.w.cleanupJobs[0] = f.job
	require.NoError(t, f.w.validateCleanupColdQuotaVerified(f.job))
	f.job.bundle.birth++
	require.Error(t, f.w.validateCleanupColdQuotaVerified(f.job))
}

// Discovery's pointer/ref cannot bypass positive origin checks by preserving
// every other coordinate. All refusals precede quota journals and any Stage.
func TestDueWorkerColdQuotaPositiveOriginalOrigin(t *testing.T) {
	for _, field := range []string{"task", "birth", "owner", "runtime", "workspace", "generation", "gate", "membership"} {
		t.Run(field, func(t *testing.T) {
			f := newCleanupColdQuotaFixture(t)
			switch field {
			case "task":
				f.job.task.TaskID = "33333333-3333-4333-8333-333333333333"
			case "birth":
				f.job.bundle.birth++
			case "owner":
				f.job.bundle.task.CreationIntentID = "other-intent"
			case "runtime":
				f.job.bundle.task.Runtime.UID = "33333333-3333-4333-8333-333333333333"
			case "workspace":
				f.job.bundle.task.WorkspaceHash = strings.Repeat("0", 64)
			case "generation":
				f.job.bundle.task.Generation++
			case "gate":
				f.job.bundle.task.DataGateEpoch++
			case "membership":
				copyJob := *f.job
				f.job = &copyJob
			}
			before := len(f.requests())
			err := f.w.captureCleanupColdQuotaOrigin(f.job, f.history)
			require.Error(t, err)
			require.Len(t, f.requests(), before)
			require.NotNil(t, f.server.point(f.slotKey))
			require.Nil(t, f.server.point(f.returnKey))
		})
	}
}

// Unknown journal acknowledgement must lose the original worker session and
// leave exactly its durable attempted reference; no live permit may be forged.
func TestDueWorkerColdQuotaUnknownJournalKeepsDurableAttempt(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	journalKey, err := f.b.namespace.coldRuntimeQuotaAttemptKey(f.task.Partition, f.claim.reference.IntentID)
	require.NoError(t, err)
	f.server.txnReply = func(request *pb.TxnRequest, response *pb.TxnResponse) {
		for _, op := range request.Success {
			if put := op.GetRequestPut(); put != nil && string(put.Key) == journalKey {
				response.Header = nil
			}
		}
	}
	grants, _, _ := f.server.counts()
	for turn := 0; turn < 32 && f.server.point(journalKey) == nil; turn++ {
		f.fund()
		_, err = f.w.cleanupTurn()
		require.NoError(t, err)
	}
	journal := f.server.point(journalKey)
	require.NotNil(t, journal)
	f.b.archiveLife.mu.Lock()
	liveErr := f.session.live()
	f.b.archiveLife.mu.Unlock()
	require.Error(t, liveErr)
	require.Error(t, f.w.validateCleanupColdQuotaVerified(f.job))
	gotGrants, _, _ := f.server.counts()
	require.Equal(t, grants, gotGrants)
	require.NotNil(t, f.server.point(f.slotKey))
	require.Nil(t, f.server.point(f.returnKey))
	require.True(t, errors.Is(f.job.err, ErrOutcomeUnknown) || errors.Is(f.job.err, ErrGuardExpired))
}

// Explicit cold mode must reject an incomplete profile before the first startup
// RPC. Disabled historical modes retain their existing configuration behavior.
func TestDueWorkerColdQuotaStartupRequiresExplicitCompleteProfile(t *testing.T) {
	f := newColdQuotaHistoryFixture(t)
	f.b.localCleanup = &LocalCleanupOptions{MaxActive: 32, ColdRuntimeQuotaReturn: true}
	before := len(coldQuotaHistoryRequests(f))
	for _, automatic := range []bool{false, true} {
		worker, err := f.b.StartDueWorker(context.Background(), DueWorkerOptions{WorkerID: "cold-incomplete",
			Partitions: []uint8{f.task.Partition}, RangeSlots: []uint16{1}, ClaimSlots: []uint16{1}, RelistSlot: 0,
			AutomaticCleanup: automatic})
		require.ErrorIs(t, err, ErrInvalidConfiguration)
		require.Nil(t, worker)
	}
	require.Len(t, coldQuotaHistoryRequests(f), before)
}

// Closing or canceling while a native response is withheld cannot release the
// shared workspace/enrolled owner or publish job.done before actual return.
func TestDueWorkerColdQuotaCancellationJoinsNativeReturn(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	original := f.b.client
	transport, err := clientv3.New(clientv3.Config{Endpoints: original.Endpoints(), DialTimeout: time.Second,
		DialOptions: []grpc.DialOption{grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, request, reply any, connection *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
			err := invoke(ctx, method, request, reply, connection, options...)
			if method == "/etcdserverpb.KV/Txn" {
				close(entered)
				<-release
			}
			return err
		})}})
	require.NoError(t, err)
	f.b.client = transport
	returned := make(chan struct{})
	started := false
	t.Cleanup(func() {
		unblock()
		if started {
			<-returned
		}
		f.b.client = original
		require.NoError(t, transport.Close())
	})
	f.fund()
	started = true
	go func() {
		_, _ = f.w.cleanupTurn()
		close(returned)
	}()
	workerTestReceive(t, entered)
	f.job.cancel()
	select {
	case <-returned:
		t.Fatal("cancellation detached the native response")
	case <-time.After(20 * time.Millisecond):
	}
	require.NotNil(t, f.w.coldQuota)
	select {
	case <-f.job.done:
		t.Fatal("completion published before native return")
	default:
	}
	unblock()
	workerTestReceive(t, returned)
	require.Nil(t, f.w.coldQuota)
	workerTestReceive(t, f.job.done)
	require.Error(t, f.w.validateCleanupColdQuotaVerified(f.job))
}

// Retention accounting must inspect capacity and the actual owned metadata
// graph, rather than reporting the fixed reservation as its measured size.
func TestDueWorkerColdQuotaReachableBytesAndKeyOverrun(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	for phase := 0; phase < 4; phase++ {
		f.turn(t)
	}
	used, keys, err := f.w.cleanupColdQuotaOwnedBytes()
	require.NoError(t, err)
	require.Greater(t, used, uint64(0))
	require.LessOrEqual(t, used, uint64(4<<20))
	require.LessOrEqual(t, keys, 64)
	workspace := f.w.coldQuota
	require.NotNil(t, workspace)
	original := workspace.history.reservedKV.Value
	workspace.history.reservedKV.Value = make([]byte, 1, 5<<20)
	_, _, err = f.w.cleanupColdQuotaOwnedBytes()
	require.Error(t, err, "slice capacity remains reachable even when its length is one")
	workspace.history.reservedKV.Value = original
	require.NoError(t, func() error { _, _, err := f.w.cleanupColdQuotaOwnedBytes(); return err }())
	for turn := 0; turn < 20 && workspace.prepared == nil; turn++ {
		f.turn(t)
	}
	require.NotNil(t, workspace.prepared)
	originalComparisons := workspace.prepared.mutation.Comparisons
	for i := 0; i < 65; i++ {
		workspace.prepared.mutation.Comparisons = append(workspace.prepared.mutation.Comparisons,
			clientv3.Compare(clientv3.CreateRevision(f.b.namespace.Root()+strings.Repeat("k", i+1)), "=", 0))
	}
	_, _, err = f.w.cleanupColdQuotaOwnedBytes()
	require.Error(t, err, "bounded bytes alone do not permit more than 64 owned source keys")
	workspace.prepared.mutation.Comparisons = originalComparisons
}

// The existing 32 registry is the only waiting room. A workspace owner cannot
// be copied onto every waiting job, and waiting never dispatches native work.
func TestDueWorkerColdQuotaSingleWorkspaceAndOriginalRegistry(t *testing.T) {
	f := newCleanupColdQuotaFixture(t)
	f.turn(t)
	workspace := f.w.coldQuota
	require.NotNil(t, workspace)
	for i := 1; i < 32; i++ {
		task := f.task
		task.TaskID = fmt.Sprintf("%08x-1111-4111-8111-111111111111", i)
		job, err := f.w.enqueueCleanupLocal(f.w.ctx, task)
		require.NoError(t, err)
		require.NotNil(t, job)
	}
	duplicate, full := f.w.automaticRegistry(f.task)
	require.True(t, duplicate)
	require.True(t, full)
	other := f.task
	other.TaskID = "ffffffff-1111-4111-8111-111111111111"
	_, err := f.w.enqueueCleanupLocal(f.w.ctx, other)
	require.ErrorIs(t, err, ErrConflict)
	before := len(f.requests())
	for _, job := range f.w.cleanupJobs {
		if job == f.job {
			continue
		}
		err = f.w.beginCleanupColdQuota(job)
		require.ErrorIs(t, err, ErrConflict, "busy shared workspace yields without creating another owner")
		require.False(t, job.started)
	}
	require.Same(t, workspace, f.w.coldQuota)
	require.Len(t, f.requests(), before)
	used, keys, err := f.w.cleanupColdQuotaOwnedBytes()
	require.NoError(t, err)
	require.LessOrEqual(t, used, uint64(4<<20), "all 32 compact additions and one workspace share this reservation")
	require.LessOrEqual(t, keys, 64)
}
