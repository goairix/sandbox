package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Run the real serial loop with one naturally replenishing Range share. The
// pending backlog consists of rejected rows, so each page immediately exposes
// another refill opportunity. Four actual page RPCs must include both lanes;
// Claim arbitration alone cannot prevent automatic-first Range starvation.
func TestDueWorkerAutomaticRangeFairness(t *testing.T) {
	server, options := workerTestEndpoint(t)
	options.WorkerControl.SessionTTL = 60 * time.Second
	now := time.Now().UTC()
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	options.PublicationTrust = &RuntimePublicationTrust{AuthorityID: options.Identity.RuntimeID, Target: "target", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
	options.Clock = testAuthorityClock(func(context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: now}, nil
	})
	b, err := New(context.Background(), options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	session, err := b.OpenWorkerSession(context.Background(), "range-fairness")
	require.NoError(t, err)
	partition, err := b.ClaimWorkerPartition(context.Background(), session, 0xaa)
	require.NoError(t, err)
	ranges, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRange, 0)
	require.NoError(t, err)
	claims, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	relist, err := b.AcquireWorkerBudget(context.Background(), session, WorkerBudgetRelist, 0)
	require.NoError(t, err)
	cache, err := newDueWorkerCache(b, session)
	require.NoError(t, err)
	scan, err := newDueWorkerScan(b, partition, ranges, claims, 30*time.Second)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(session.ctx, 5*time.Second)
	defer cancel()
	actorOptions := dueWorkerActorOptions()
	actorOptions.AutomaticCleanup = true
	w := &DueWorker{backend: b, session: session, ctx: ctx, cancel: cancel, options: actorOptions, ranges: []*WorkerBudgetShare{ranges}, claims: []*WorkerBudgetShare{claims}, relist: relist, scans: []*dueWorkerScan{scan}, cache: cache, cleanupWake: make(chan struct{}, 1)}
	w.self = w
	require.NoError(t, w.initializeAutomatic())

	// Obtain a genuinely completed empty B through the original protocol so
	// legacy bootstrap phases cannot accidentally give due its first token.
	f := &backfillBootstrapFixture{b: b, server: server, session: session, partition: partition, ranges: ranges, claims: claims}
	state := backfillBootstrapCreate(t, f)
	backfillBootstrapCredit(b, ranges, 262144)
	page, err := b.readCleanupLegacyPage(ctx, partition, ranges, state, nil)
	require.NoError(t, err)
	backfillBootstrapCredit(b, claims, 1)
	state, err = b.advanceCleanupBackfill(ctx, partition, claims, page, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "complete", state.record.Phase)
	w.automatic.partitions[0].backfill = state

	pages := &duePageFixture{b: b, server: server, now: now, snapshots: make(map[int64][]*mvccpb.KeyValue)}
	pages.seed(t, 240, now.Add(30*time.Second))
	for _, hint := range pendingPageSeed(t, pages, 0, 400) {
		key, err := b.namespace.cleanupPendingKey(hint.Reference)
		require.NoError(t, err)
		server.put(key, `{}`, 0)
	}
	server.mu.Lock()
	server.txnReply = pages.rangeReply
	server.mu.Unlock()

	// Restore normal earned-time replenishment after fixture preparation.
	// No loop turn adds credit, changes its rate, refunds, or consumes early.
	b.archiveLife.mu.Lock()
	ranges.bucket.last, ranges.bucket.tokens, ranges.bucket.remainder = time.Now(), 262144, 0
	b.archiveLife.mu.Unlock()
	var lanes []string
	var selected string
	b.client.KV = &faultKV{KV: b.client.KV, match: func(ops []clientv3.Op) bool {
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
				cancel()
			}
		}
		return response, err
	}}

	// This is metadata-only loop execution, not positive physical admission.
	// Cancellation joins the fourth RPC; earlier successful pages must already
	// have made progress without waiting for all 400 pending rows to drain.
	require.Error(t, w.loop())
	require.Len(t, lanes, 4)
	require.Contains(t, lanes, "pending")
	require.Contains(t, lanes, "due", "single Range share was monopolized: %v", lanes)
	require.True(t, scan.started, "due coverage must be published before the final canceled call")
	require.Positive(t, w.automatic.rejected)
}

// This serial metadata harness owns genuine session/partition/budget objects
// and the original native model connection. It is not a started automatic
// worker: no physical profile, configuration injection or fake Kernel exists.
// Positive StartDueWorker and physical jobs belong to the native composition.
func newDueAutomaticMetadataFixture(t *testing.T) (*legacyPageFixture, *DueWorker) {
	t.Helper()
	f := newLegacyPageFixture(t)
	cache, err := newDueWorkerCache(f.b, f.session)
	require.NoError(t, err)
	scan, err := newDueWorkerScan(f.b, f.partition, f.ranges, f.claims, 30*time.Second)
	require.NoError(t, err)
	relist, err := f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRelist, 0)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(f.session.ctx)
	t.Cleanup(cancel)
	options := dueWorkerActorOptions()
	options.AutomaticCleanup = true
	w := &DueWorker{
		backend:     f.b,
		session:     f.session,
		ctx:         ctx,
		cancel:      cancel,
		options:     options,
		ranges:      []*WorkerBudgetShare{f.ranges},
		claims:      []*WorkerBudgetShare{f.claims},
		relist:      relist,
		scans:       []*dueWorkerScan{scan},
		cache:       cache,
		cleanupWake: make(chan struct{}, 1),
	}
	w.self = w
	require.NoError(t, w.initializeAutomatic())
	return f, w
}

func dueAutomaticMetadataTurn(t *testing.T, f *legacyPageFixture, w *DueWorker) {
	t.Helper()
	backfillBootstrapCredit(f.b, f.ranges, 262144)
	backfillBootstrapCredit(f.b, f.claims, 1)
	w.claimLane = 2
	_, err := w.automaticTurn(w.ctx)
	require.NoError(t, err)
	w.publishStatus("listing", nil)
}

func TestDueWorkerAutomaticAdmission(t *testing.T) {
	for _, ttl := range []time.Duration{15 * time.Second, 60 * time.Second} {
		t.Run(ttl.String()+"-missing-physical-profile", func(t *testing.T) {
			f := newBackfillBootstrapFixture(t, ttl)
			options := dueWorkerActorOptions()
			options.AutomaticCleanup = true
			before := len(backfillBootstrapRequests(f))
			worker, err := f.b.StartDueWorker(context.Background(), options)
			require.ErrorIs(t, err, ErrInvalidConfiguration)
			require.Nil(t, worker)
			require.Len(t, backfillBootstrapRequests(f), before, "profile refusal must precede all worker startup RPCs")
		})
	}
	t.Run("default-remains-explicit", func(t *testing.T) {
		f := newDueWorkerActorFixture(t)
		options := dueWorkerActorOptions()
		require.False(t, options.AutomaticCleanup)
		w := f.start(t, options)
		require.False(t, w.Status().Cleanup.Enabled)
		require.Nil(t, w.automatic)
	})
}

func TestDueWorkerAutomaticDiscovery(t *testing.T) {
	t.Run("legacy-is-indexed-and-completed-by-finite-turns", func(t *testing.T) {
		f, w := newDueAutomaticMetadataFixture(t)
		s := seedCleanupBirth(t, f.backfillBootstrapFixture)
		key, err := f.b.namespace.cleanupBackfillKey(f.partition.partition)
		require.NoError(t, err)
		complete := false
		for i := 0; i < 40; i++ {
			dueAutomaticMetadataTurn(t, f, w)
			kv := f.server.point(key)
			if kv == nil {
				continue
			}
			r, err := decodeCleanupBackfill(f.b.namespace, f.b.restoreEpoch, workerIdentityDigest(f.b.identityValue), 0xaa, kv, kv.ModRevision)
			require.NoError(t, err)
			if r.Phase == "complete" {
				complete = true
				break
			}
		}
		require.True(t, complete)
		require.NotNil(t, f.server.point(s.keys[5]))
		require.Nil(t, f.server.point(s.keys[4]), "metadata discovery cannot pretend to run physical cleanup")
		require.True(t, w.Status().Cleanup.Enabled)
		require.Len(t, w.Status().Cleanup.Partitions, 1)
	})
	t.Run("rejected-full-page-does-not-hide-valid-tail", func(t *testing.T) {
		f, w := newDueAutomaticMetadataFixture(t)
		view := &duePageFixture{b: f.b, server: f.server}
		rows := pendingPageSeed(t, view, 0, 80)
		for _, row := range rows {
			key, err := f.b.namespace.cleanupPendingKey(row.Reference)
			require.NoError(t, err)
			f.server.put(key, `{}`, 0)
		}
		s := seedCleanupBirth(t, f.backfillBootstrapFixture)
		cleanupBackfillSeedMetadata(t, f, s, false, false, true, false)
		secondPage := false
		var nextSweep time.Time
		for i := 0; i < 50; i++ {
			dueAutomaticMetadataTurn(t, f, w)
			for _, request := range backfillBootstrapRequests(f.backfillBootstrapFixture) {
				for _, op := range request.Success {
					q := op.GetRequestRange()
					if q != nil && strings.Contains(string(q.Key), "/cleanup-pending/") && strings.HasSuffix(string(q.Key), "\x00") {
						secondPage = true
					}
				}
			}
			status := w.Status().Cleanup
			if len(status.Partitions) == 1 && !status.Partitions[0].NextSweep.IsZero() && status.Rejected >= 80 {
				nextSweep = status.Partitions[0].NextSweep
				break
			}
		}
		require.True(t, secondPage, "the original raw-key continuation must cross all rejected rows")
		require.Greater(t, time.Until(nextSweep), 29*time.Second)
		require.NotNil(t, f.server.point(s.keys[5]), "unsupported host execution remains retryable on the next sweep")
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		for i := 0; i < 5; i++ {
			dueAutomaticMetadataTurn(t, f, w)
		}
		for _, request := range backfillBootstrapRequests(f.backfillBootstrapFixture)[before:] {
			for _, op := range request.Success {
				q := op.GetRequestRange()
				if q != nil && len(q.RangeEnd) > 0 {
					require.False(t, strings.Contains(string(q.Key), "/cleanup-pending/"), "a completed sweep cannot hot-retry")
				}
			}
		}
	})
}

// Exercise the real readyClaim and original consuming owner with one shared
// token. These are lane/grant tests, not a claim that host physical jobs ran.
func TestDueWorkerAutomaticFairness(t *testing.T) {
	f, w := newDueAutomaticMetadataFixture(t)
	grants := make([]int, 0, 12)
	for round := 0; round < 12; round++ {
		backfillBootstrapCredit(f.b, f.claims, 1)
		before := len(grants)
		for _, lane := range []int{1, 2, 0} {
			w.claimLane = lane
			share, _ := w.readyClaim(time.Now())
			if share == nil {
				continue
			}
			owner, stop, err := f.b.beginExpiredTaskOwner(w.ctx, f.partition, share)
			require.NoError(t, err)
			require.Same(t, f.claims, share)
			require.Same(t, f.session, owner.call.session)
			require.NoError(t, owner.postcheck())
			owner.finish()
			stop()
			grants = append(grants, lane)
		}
		require.Len(t, grants, before+1, "one earned token grants exactly one lane")
	}
	for i := 3; i+3 <= len(grants); i++ {
		seen := [3]bool{}
		for _, lane := range grants[i : i+3] {
			seen[lane] = true
		}
		require.Equal(t, [3]bool{true, true, true}, seen, "every waiting lane must progress within three grants: %v", grants)
	}
}

func TestDueWorkerAutomaticBounds(t *testing.T) {
	t.Run("owned-page-and-birth-overlap", func(t *testing.T) {
		f, w := newDueAutomaticMetadataFixture(t)
		pendingPageSeed(t, &duePageFixture{b: f.b, server: f.server}, 0, 80)
		s := seedCleanupBirth(t, f.backfillBootstrapFixture)
		backfillBootstrapCredit(f.b, f.ranges, 262144)
		page, err := f.b.readCleanupPendingPage(w.ctx, f.partition, f.ranges, nil)
		require.NoError(t, err)
		require.Len(t, page.Candidates, 80)
		birth := cleanupBirthTestRead(t, f.backfillBootstrapFixture, s.task.Reference.TaskID)
		w.automatic.page, w.automatic.birth = page, birth
		retained, err := w.automaticRetainedBytes()
		require.NoError(t, err)
		require.Positive(t, retained)
		require.LessOrEqual(t, retained, uint64(4<<20))
		require.LessOrEqual(t, unsafe.Sizeof(*w.automatic), uintptr(64<<10))
		require.Equal(t, 3670016, 256*8192+80*4096+32*4096+(1<<20)+(64<<10))
		require.Equal(t, 369, 256+80+32+1)

		// Invalid retained bodies are accounting-only inputs. They must fail
		// before a later actor phase could publish or dispatch them.
		w.automatic.page.Candidates = append(w.automatic.page.Candidates, page.Candidates[0])
		_, err = w.automaticRetainedBytes()
		require.Error(t, err)
		w.automatic.page.Candidates = w.automatic.page.Candidates[:80]
		w.automatic.page.Candidates[0].Reference.SandboxID = strings.Repeat("x", 4<<20)
		_, err = w.automaticRetainedBytes()
		require.Error(t, err)
	})
	t.Run("automatic-cache-reserves-369-identities", func(t *testing.T) {
		f, w := newDueAutomaticMetadataFixture(t)
		f.b.archiveLife.mu.Lock()
		defer f.b.archiveLife.mu.Unlock()
		for i := 0; i < 9631; i++ {
			err := w.cache.put(fmt.Sprintf("%spure-accounting/%05d", f.b.namespace.Root(), i), "v", DueCandidateRecord{})
			require.NoError(t, err)
		}
		require.ErrorIs(t, w.cache.put(f.b.namespace.Root()+"pure-accounting/extra", "v", DueCandidateRecord{}), ErrConflict)
		require.Equal(t, 9631, w.cache.count)
	})
	t.Run("automatic-cache-reserves-four-mebibytes", func(t *testing.T) {
		f, w := newDueAutomaticMetadataFixture(t)
		f.b.archiveLife.mu.Lock()
		defer f.b.archiveLife.mu.Unlock()
		require.ErrorIs(t, w.cache.put(f.b.namespace.Root()+"pure-accounting/large", strings.Repeat("x", 12<<20), DueCandidateRecord{}), ErrConflict)
		require.Zero(t, w.cache.count, "charge before cloning into the shared due cache")
	})
}

func TestDueWorkerAutomaticClose(t *testing.T) {
	f, w := newDueAutomaticMetadataFixture(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	f.b.client.KV = &faultKV{KV: f.b.client.KV, match: func(ops []clientv3.Op) bool {
		return len(ops) > 0 && ops[0].IsGet()
	}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		entered <- struct{}{}
		<-release
		return r, err
	}}
	backfillBootstrapCredit(f.b, f.ranges, 262144)
	done, closed := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := w.automaticTurn(w.ctx)
		done <- err
	}()
	workerTestReceive(t, entered)
	go func() { closed <- f.b.Close() }()
	workerTestReceive(t, f.session.ctx.Done())
	select {
	case err := <-closed:
		t.Fatalf("Close detached the actual automatic metadata call: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	require.Error(t, workerTestReceive(t, done))
	require.NoError(t, workerTestReceive(t, closed))
}
