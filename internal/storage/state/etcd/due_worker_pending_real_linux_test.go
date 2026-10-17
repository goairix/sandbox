//go:build linux && (amd64 || arm64)

package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This fixture owns real metadata capabilities, but is deliberately not a
// started physical worker. Its serial actor stops at authenticated birth;
// neither a pending hint nor this harness supplies a Kernel or cleanup job.
type pendingRealFixture struct {
	b       *Backend
	raw     *clientv3.Client
	options Options
	root    ed25519.PrivateKey
	w       *DueWorker
}

func newPendingRealFixture(t *testing.T, ctx context.Context) *pendingRealFixture {
	t.Helper()
	seed, raw := integrationBackend(t)
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	options := integrationOptions(seed, raw)
	options.RequestTimeout = time.Second
	options.Clock = goodAuthorityClock()
	options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "target", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
	options.EnrolledPublication = &EnrolledPublicationOptions{ContractDigest: strings.Repeat("b", 64), MaxActive: 4}
	options.WorkerControl = &WorkerControlOptions{SessionTTL: 60 * time.Second}
	profile, err := seed.namespace.Key("meta", "worker_control_budget")
	require.NoError(t, err)
	_, err = raw.Put(ctx, profile, workerTestProfile(options))
	require.NoError(t, err)
	b, err := New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	return &pendingRealFixture{b: b, raw: raw, options: options, root: root}
}

func (f *pendingRealFixture) actor(t *testing.T, ctx context.Context, name string) *DueWorker {
	t.Helper()
	session, err := f.b.OpenWorkerSession(ctx, name)
	require.NoError(t, err)
	partition, err := f.b.ClaimWorkerPartition(ctx, session, 0xaa)
	require.NoError(t, err)
	ranges, err := f.b.AcquireWorkerBudget(ctx, session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	claims, err := f.b.AcquireWorkerBudget(ctx, session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	relist, err := f.b.AcquireWorkerBudget(ctx, session, WorkerBudgetRelist, 0)
	require.NoError(t, err)
	cache, err := newDueWorkerCache(f.b, session)
	require.NoError(t, err)
	scan, err := newDueWorkerScan(f.b, partition, ranges, claims, 30*time.Second)
	require.NoError(t, err)
	deadline, bounded := ctx.Deadline()
	require.True(t, bounded, "native actor must inherit a finite test deadline")
	owned, cancel := context.WithDeadline(session.ctx, deadline)
	t.Cleanup(cancel)
	options := dueWorkerActorOptions()
	options.AutomaticCleanup = true
	w := &DueWorker{
		backend: f.b, session: session, ctx: owned, cancel: cancel, options: options,
		ranges: []*WorkerBudgetShare{ranges}, claims: []*WorkerBudgetShare{claims},
		relist: relist, scans: []*dueWorkerScan{scan}, cache: cache,
		cleanupWake: make(chan struct{}, 1),
	}
	w.self = w
	require.NoError(t, w.initializeAutomatic())
	f.w = w
	return w
}

// Read a bucket copy and wait for original earned time. No fixture writes
// tokens, timestamps, rates or deadlines, and no wait holds a native owner.
func pendingRealCredit(t *testing.T, w *DueWorker, share *WorkerBudgetShare, amount uint32) {
	t.Helper()
	w.backend.archiveLife.mu.Lock()
	at := dueWorkerCreditAt(share.bucket, time.Now(), amount)
	w.backend.archiveLife.mu.Unlock()
	if delay := time.Until(at); delay > 0 {
		partitionWorkerNativePause(t, w.ctx, delay)
	}
}

func pendingRealTurn(t *testing.T, w *DueWorker, backfill bool) {
	t.Helper()
	if w.automatic.phase == "" {
		var selected bool
		if backfill {
			selected, _ = w.automaticSelectBackfill(time.Now())
		} else {
			selected, _ = w.automaticSelectPending(time.Now())
		}
		require.True(t, selected)
	}
	pendingRealCredit(t, w, w.ranges[0], maxMutationBytes)
	if phase := w.automatic.phase; phase == "backfill-refresh" || phase == "backfill-advance" {
		pendingRealCredit(t, w, w.claims[0], 1)
	}
	w.beginClaimLane(2)
	at, err := w.automaticTurn(w.ctx)
	w.endClaimLane()
	require.NoError(t, err)
	// An unavailable original admission returns a finite wake. Respect it;
	// later bounded turns may advance, without polling an unearned bucket.
	if delay := time.Until(at); delay > 0 {
		partitionWorkerNativePause(t, w.ctx, delay)
	}
}

// The only raw pending writes are named invalid, untrusted negative inputs.
// Their punctuation sorts before every producer UUID, crossing a whole page
// without constructing any Task, receipt, execution occupation or authority.
func (f *pendingRealFixture) badPending(t *testing.T, ctx context.Context, count int) {
	t.Helper()
	prefix := fmt.Sprintf("%sp/aa/cleanup-pending/", f.b.namespace.Root())
	for i := 0; i < count; i++ {
		_, err := f.raw.Put(ctx, fmt.Sprintf("%s!invalid-%04d", prefix, i), `{}`)
		require.NoError(t, err)
	}
}

// The original enrolled publication creates a real active sandbox record;
// PrepareDestroy/CommitStage then atomically produce its immutable Task and P.
// A test certificate is metadata provenance, never physical runtime evidence.
func (f *pendingRealFixture) destroy(t *testing.T, ctx context.Context, loseACK bool) (TaskReference, int64) {
	t.Helper()
	partition := uint8(0xaa)
	sandbox := dueWorkerNativePublish(t, ctx, f.b, f.root, time.Now().UTC(), "pending-real", time.Minute, &partition)
	control := dueWorkerNativeGet(t, ctx, f.raw, sandbox.controlKey)
	require.Len(t, control.Kvs, 1)
	stage, ref, err := f.b.PrepareDestroy(ctx, BeginDestroyInput{SandboxID: sandbox.candidate.SandboxID, RequestID: uuid.NewString(), ExpectedControlRevision: control.Kvs[0].ModRevision}, 30*time.Second)
	require.NoError(t, err)
	pendingKey, err := f.b.namespace.cleanupPendingKey(ref)
	require.NoError(t, err)
	original := f.b.client.KV
	var committed int64
	if loseACK {
		f.b.client.KV = &faultKV{KV: original, match: putsKey(pendingKey), after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			require.NoError(t, err)
			require.NotNil(t, response)
			require.True(t, response.Succeeded)
			require.Zero(t, committed, "drop exactly one original successful producer ACK")
			committed = response.Header.Revision
			return nil, errors.New("pending real fixture: committed producer ACK withheld")
		}}
	}
	outcome, commitErr := f.b.CommitStage(ctx, stage)
	f.b.client.KV = original
	if loseACK {
		require.ErrorIs(t, commitErr, ErrOutcomeUnknown)
		require.Equal(t, OutcomeUnknown, outcome)
		require.Positive(t, committed)
	} else {
		require.NoError(t, commitErr)
		require.Equal(t, OutcomeCommitted, outcome)
	}
	require.NoError(t, f.b.ReleaseStage(ctx, stage))

	keys := append(taskDestroyKeys(t, f.b, ref), pendingKey, stage.receiptKey)
	for _, key := range keys {
		response := dueWorkerNativeGet(t, ctx, f.raw, key)
		require.Len(t, response.Kvs, 1)
		kv := response.Kvs[0]
		if committed == 0 {
			committed = kv.CreateRevision
		}
		require.Equal(t, committed, kv.CreateRevision, key)
		require.Equal(t, committed, kv.ModRevision, key)
		require.EqualValues(t, 1, kv.Version, key)
		require.Zero(t, kv.Lease, key)
	}
	return ref, committed
}

func pendingRealBirth(t *testing.T, w *DueWorker, ref TaskReference, birth int64) {
	t.Helper()
	for turn := 0; turn < 8 && w.automatic.birth == nil; turn++ {
		pendingRealTurn(t, w, false)
	}
	a := w.automatic
	require.Equal(t, "pending-enqueue", a.phase)
	require.NotNil(t, a.birth)
	require.Equal(t, ref, a.birth.record.Reference)
	require.Equal(t, birth, a.birth.birth)
	require.Equal(t, a.candidate.TaskDigest, workerIdentityDigest(a.birth.task.value))
	require.NotNil(t, a.birth.pending)
	require.Nil(t, a.birth.execution)
	for _, job := range w.cleanupJobs {
		require.Nil(t, job, "birth discovery is not a queued physical job")
	}
}

func TestDueWorkerPendingRealNative(t *testing.T) {
	if os.Getenv("SANDBOX_PENDING_REAL_NATIVE") != "1" {
		t.Skip("requires the isolated pending real-etcd collector")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	endpoints := startPartitionWorkerNativeCluster(t, ctx)
	t.Setenv("TEST_ETCD_ENDPOINTS", strings.Join(endpoints, ","))

	t.Run("bad-page-valid-tail", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		f := newPendingRealFixture(t, ctx)
		ref, birth := f.destroy(t, ctx, false)
		f.badPending(t, ctx, 80)
		w := f.actor(t, ctx, "pending-real-pages")
		var starts []string
		var revisions []int64
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool {
			if len(ops) == 1 && ops[0].IsGet() && strings.Contains(string(ops[0].KeyBytes()), "/cleanup-pending/") && len(ops[0].RangeBytes()) > 0 {
				starts = append(starts, string(ops[0].KeyBytes()))
				revisions = append(revisions, ops[0].Rev())
			}
			return false
		}}
		pendingRealBirth(t, w, ref, birth)
		require.EqualValues(t, 80, w.automatic.rejected)
		require.Len(t, starts, 2)
		require.True(t, strings.HasSuffix(starts[1], "!invalid-0079\x00"))
		require.Zero(t, revisions[0])
		require.Equal(t, w.automatic.page.Revision, revisions[1])
		require.GreaterOrEqual(t, revisions[1], birth)
		t.Logf("real fixed-R pages=2 rejected=80 birth=%d view=%d; no queued job", birth, revisions[1])
	})

	t.Run("compaction-and-old-session", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		f := newPendingRealFixture(t, ctx)
		f.badPending(t, ctx, 81)
		w := f.actor(t, ctx, "pending-real-compaction")
		pendingRealTurn(t, w, false)
		old := w.automatic.partitions[0].cursor
		require.NotNil(t, old)
		r := old.cursor.revision
		advanced, err := f.raw.Put(ctx, f.b.namespace.Root()+"test-compaction-revision", "advance")
		require.NoError(t, err)
		require.Greater(t, advanced.Header.Revision, r)
		_, err = f.raw.Compact(ctx, r+1)
		require.NoError(t, err)
		original := f.b.client.KV
		compacted, calls := 0, 0
		f.b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
			calls++
			return len(ops) == 1 && ops[0].IsGet() && ops[0].Rev() == r && len(ops[0].RangeBytes()) > 0
		}, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			if errors.Is(err, rpctypes.ErrCompacted) {
				compacted++
			}
			return response, err
		}}
		pendingRealTurn(t, w, false)
		require.Equal(t, 1, compacted, "error must come from the actual compacted etcd read")
		part := &w.automatic.partitions[0]
		require.Nil(t, part.cursor)
		require.Zero(t, part.pendingRevision)
		require.Greater(t, time.Until(part.nextSweep), 29*time.Second)
		require.False(t, w.automatic.pageReady)
		require.Empty(t, w.automatic.page.Candidates)
		require.Nil(t, w.automatic.birth)
		require.Nil(t, w.automatic.release)
		f.b.archiveLife.mu.Lock()
		live := w.session.live()
		f.b.archiveLife.mu.Unlock()
		require.NoError(t, live)
		pendingRealCredit(t, w, w.ranges[0], maxMutationBytes)
		before := calls
		_, err = f.b.readCleanupPendingPage(ctx, w.scans[0].partition, w.ranges[0], old)
		require.Error(t, err)
		require.Equal(t, before, calls, "consumed cursor must refuse before RPC")

		require.NoError(t, f.b.ReleaseWorkerSession(ctx, w.session))
		before = calls
		_, err = w.automaticTurn(w.ctx)
		require.Error(t, err)
		require.Equal(t, before, calls, "old session must not send a fresh metadata RPC")
		fresh := f.actor(t, ctx, "pending-real-new-session")
		require.NotEqual(t, w.session.reference.SessionID, fresh.session.reference.SessionID)
		pendingRealCredit(t, fresh, fresh.ranges[0], maxMutationBytes)
		before = calls
		_, err = f.b.readCleanupPendingPage(ctx, fresh.scans[0].partition, fresh.ranges[0], old)
		require.Error(t, err)
		require.Equal(t, before, calls, "old cursor is not transferable to a new original session")
		t.Logf("real Compact revision=%d observed ErrCompacted=1; old capability reuse sent zero RPC", r+1)
	})

	t.Run("producer-ack-lost-replacement", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		f := newPendingRealFixture(t, ctx)
		old := f.actor(t, ctx, "pending-real-before-ack")
		ref, birth := f.destroy(t, ctx, true)
		require.NoError(t, f.b.ReleaseWorkerSession(ctx, old.session))
		require.NoError(t, f.b.Close())
		replacement, err := New(ctx, f.options)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, replacement.Close()) })
		f.b = replacement
		fresh := f.actor(t, ctx, "pending-real-after-ack")
		require.NotEqual(t, old.session.reference.SessionID, fresh.session.reference.SessionID)
		require.Nil(t, fresh.automatic.partitions[0].cursor)
		require.Nil(t, fresh.automatic.birth)
		pendingRealBirth(t, fresh, ref, birth)
		require.NotSame(t, old.cache, fresh.cache)
		t.Logf("original CommitStage ACK withheld at actual revision=%d; replacement discovered original Task birth without an explicit submission", birth)
	})

	t.Run("earned-range-due-fairness", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		f := newPendingRealFixture(t, ctx)
		w := f.actor(t, ctx, "pending-real-fairness")
		// Establish an authentic completed empty B before either lane has
		// backlog. Legacy initialization must not hand due an incidental turn.
		for turn := 0; turn < 8; turn++ {
			pendingRealTurn(t, w, true)
			state := w.automatic.partitions[0].backfill
			if state != nil && state.record.Phase == "complete" {
				break
			}
		}
		require.NotNil(t, w.automatic.partitions[0].backfill)
		require.Equal(t, "complete", w.automatic.partitions[0].backfill.record.Phase)
		partition := uint8(0xaa)
		dueWorkerNativePublish(t, ctx, f.b, f.root, time.Now().UTC(), "pending-fairness", 30*time.Second, &partition)
		f.badPending(t, ctx, 400)
		var lanes []string
		selected := ""
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool {
			if len(ops) != 1 || !ops[0].IsGet() || len(ops[0].RangeBytes()) == 0 {
				return false
			}
			selected = "due"
			if strings.Contains(string(ops[0].KeyBytes()), "/cleanup-pending/") {
				selected = "pending"
			}
			return true
		}, after: func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			if err == nil && response != nil && response.Succeeded {
				lanes = append(lanes, selected)
				if len(lanes) == 4 {
					w.cancel()
				}
			}
			return response, err
		}}
		// The original serial loop alone consumes and replenishes its share.
		// Cancellation is synchronous with native return; loop joins before
		// any assertion or Backend cleanup can read these actor-owned fields.
		require.Error(t, w.loop())
		require.Len(t, lanes, 4)
		require.Contains(t, lanes, "pending")
		require.Contains(t, lanes, "due")
		require.True(t, w.scans[0].started)
		require.Positive(t, w.automatic.rejected)
		require.Less(t, w.automatic.rejected, uint64(400), "due progressed before pending backlog drained")
		t.Logf("real normally earned single-share Range lanes=%v rejected=%d; metadata only", lanes, w.automatic.rejected)
	})
}
