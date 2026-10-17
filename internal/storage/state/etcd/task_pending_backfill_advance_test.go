package etcd

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Additional Tasks are born in actual model transactions, with canonical
// T/I/link/receipt at a common revision. They are never synthetic birth seals.
func cleanupBackfillAdditionalBirth(t *testing.T, f *legacyPageFixture, seed cleanupBirthTestSeed, id string, indexed bool) cleanupBirthTestSeed {
	t.Helper()
	s := seed
	s.task.Reference.TaskID = id
	s.task.Reference.SandboxID = "backfill-" + id
	s.intent.Task = s.task
	s.intent.Attempt.AttemptID = id
	s.stage = s.intent.Attempt.reference(s.stage.Digest)
	keys, _, err := f.b.cleanupBirthCohortKeys(s.task.Reference, s.intent.Attempt)
	require.NoError(t, err)
	copy(s.keys[:], keys)
	s.wires[0], err = encodeTaskRecord(s.task)
	require.NoError(t, err)
	s.wires[1], err = encodeCleanupIntentRecord(s.intent)
	require.NoError(t, err)
	s.wires[2], err = encodeTaskLinkRecord(TaskLinkRecord{Version: 1, Reference: s.task.Reference})
	require.NoError(t, err)
	s.wires[3], err = encodeReceipt(s.stage, OutcomeCommitted)
	require.NoError(t, err)
	var ops []clientv3.Op
	for i, value := range s.wires {
		ops = append(ops, clientv3.OpPut(s.keys[i], value))
	}
	if indexed {
		pending, err := encodeCleanupPending(cleanupPendingRecord{Version: 1, Reference: s.task.Reference, TaskDigest: workerIdentityDigest(s.wires[0])})
		require.NoError(t, err)
		ops = append(ops, clientv3.OpPut(s.keys[5], pending))
	}
	r, err := f.b.client.Txn(context.Background()).Then(ops...).Commit()
	require.NoError(t, err)
	s.birth = r.Header.Revision
	return s
}

func cleanupBackfillSeedMetadata(t *testing.T, f *legacyPageFixture, s cleanupBirthTestSeed, execution, release, pending, settled bool) {
	t.Helper()
	if release {
		f.server.put(f.b.namespace.Root()+"backfill-metadata-revision", "advance", 0)
		f.server.put(s.keys[6], cleanupBirthTestRelease(t, s), 0)
	}
	if execution {
		r := cleanupBirthTestExecution(s)
		if settled {
			r.Phase, r.Confirmed = "settled", "settled"
			r.Release = &cleanupExecutionRelease{CreateRevision: s.birth + 2, Digest: strings.Repeat("a", 64)}
		}
		wire, err := encodeCleanupExecution(r)
		require.NoError(t, err)
		f.server.put(s.keys[4], wire, 0)
	}
	if pending {
		wire, err := encodeCleanupPending(cleanupPendingRecord{Version: 1, Reference: s.task.Reference, TaskDigest: workerIdentityDigest(s.wires[0])})
		require.NoError(t, err)
		f.server.put(s.keys[5], wire, 0)
	}
}

func cleanupBackfillAdvanceRead(t *testing.T, f *legacyPageFixture, state *cleanupBackfillState, s cleanupBirthTestSeed) (*cleanupLegacyPage, *cleanupBirthEvidence) {
	t.Helper()
	page := f.read(t, state, nil)
	birth := cleanupBirthTestRead(t, f.backfillBootstrapFixture, s.task.Reference.TaskID)
	return page, birth
}

func TestCleanupBackfillAdvance(t *testing.T) {
	for mask := 0; mask < 8; mask++ {
		t.Run(fmt.Sprintf("atomic-pending-e%d-r%d-p%d", mask&1, (mask>>1)&1, (mask>>2)&1), func(t *testing.T) {
			f := newLegacyPageFixture(t)
			s := seedCleanupBirth(t, f.backfillBootstrapFixture)
			e, r, p := mask&1 != 0, mask&2 != 0, mask&4 != 0
			cleanupBackfillSeedMetadata(t, f, s, e, r, p, false)
			state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
			page, birth := cleanupBackfillAdvanceRead(t, f, state, s)
			priorP := f.server.point(s.keys[5])
			before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
			backfillBootstrapCredit(f.b, f.claims, 1)
			next, err := f.b.advanceCleanupBackfill(context.Background(), f.partition, f.claims, page, birth, nil)
			require.NoError(t, err)
			require.NotNil(t, next)
			require.Same(t, next, next.self)
			require.Same(t, f.partition, next.partition)
			require.Equal(t, "complete", next.record.Phase, "nonempty EOF is covered by this same CAS")
			require.Equal(t, s.task.Reference.TaskID, next.record.Frontier)
			require.Equal(t, state.record.LegacyCutoffRevision, next.record.LegacyCutoffRevision)
			require.Equal(t, state.record.ReadViewRevision, next.record.ReadViewRevision)
			require.Equal(t, ownTaskFence(f.server.point(state.fence.key)), next.fence)
			require.Equal(t, state.fence.create, next.fence.create)
			pending := f.server.point(s.keys[5])
			require.NotNil(t, pending)
			decoded, err := decodeCleanupPending(f.b.namespace, f.b.restoreEpoch, pending, next.fence.mod)
			require.NoError(t, err)
			require.Equal(t, workerIdentityDigest(s.wires[0]), decoded.TaskDigest)
			if p {
				require.Equal(t, priorP, pending, "existing immutable P must not be rewritten")
			} else {
				require.Equal(t, next.fence.mod, pending.CreateRevision, "P and frontier commit atomically")
			}
			requests := backfillBootstrapRequests(f.backfillBootstrapFixture)[before:]
			require.Len(t, requests, 1)
			q := requests[0]
			require.LessOrEqual(t, q.Size(), 128*1024)
			comparisons := 39 + 3*int(mask&1) + 3*int((mask>>1)&1) + 3*int((mask>>2)&1)
			writes := 2
			if p {
				writes = 1
			}
			require.Len(t, q.Compare, comparisons)
			require.Len(t, q.Success, writes)
			require.Len(t, q.Failure, 2)
			require.Equal(t, state.fence.key, string(q.Success[writes-1].GetRequestPut().Key))
			if !p {
				require.Equal(t, s.keys[5], string(q.Success[0].GetRequestPut().Key))
			}
			f.b.archiveLife.mu.Lock()
			worker, err := f.b.workerComparisons(f.session, f.partition, f.claims)
			f.b.archiveLife.mu.Unlock()
			require.NoError(t, err)
			require.Equal(t, taskProtoComparisons(worker), q.Compare[:20])
			for _, fence := range []taskFence{state.fence, birth.task, birth.intent, birth.receipt} {
				for _, cmp := range taskProtoComparisons(fence.comparisons()) {
					require.Contains(t, q.Compare, cmp)
				}
			}
			for i, fence := range []*taskFence{birth.executionFence, birth.release, birth.pending} {
				key := []string{s.keys[4], s.keys[6], s.keys[5]}[i]
				cmps := []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(key), "=", 0)}
				if fence != nil {
					cmps = fence.comparisons()
				}
				for _, cmp := range taskProtoComparisons(cmps) {
					require.Contains(t, q.Compare, cmp)
				}
			}
			require.Equal(t, taskProtoPoints([]string{f.b.identityKey, f.b.restoreKey}), q.Failure)
		})
	}
	t.Run("settled-without-authenticated-release-remains-indexed", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		s := seedCleanupBirth(t, f.backfillBootstrapFixture)
		cleanupBackfillSeedMetadata(t, f, s, true, false, false, true)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		page, birth := cleanupBackfillAdvanceRead(t, f, state, s)
		backfillBootstrapCredit(f.b, f.claims, 1)
		next, err := f.b.advanceCleanupBackfill(context.Background(), f.partition, f.claims, page, birth, nil)
		require.NoError(t, err)
		require.Equal(t, "complete", next.record.Phase)
		require.NotNil(t, f.server.point(s.keys[5]))
		require.Equal(t, *birth.executionFence, ownTaskFence(f.server.point(s.keys[4])))
	})
	t.Run("two-legacy-eof-needs-two-fresh-prefixes", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		first := seedCleanupBirth(t, f.backfillBootstrapFixture)
		second := cleanupBackfillAdditionalBirth(t, f, first, "ffffffff-1111-4111-8111-111111111111", false)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		page, birth := cleanupBackfillAdvanceRead(t, f, state, first)
		require.True(t, page.eof)
		require.Len(t, page.rows, 2)
		backfillBootstrapCredit(f.b, f.claims, 1)
		next, err := f.b.advanceCleanupBackfill(context.Background(), f.partition, f.claims, page, birth, nil)
		require.NoError(t, err)
		require.Equal(t, "list", next.record.Phase)
		require.Equal(t, first.task.Reference.TaskID, next.record.Frontier)
		require.Nil(t, f.server.point(second.keys[5]))
		page, birth = cleanupBackfillAdvanceRead(t, f, next, second)
		require.Len(t, page.rows, 1)
		backfillBootstrapCredit(f.b, f.claims, 1)
		last, err := f.b.advanceCleanupBackfill(context.Background(), f.partition, f.claims, page, birth, nil)
		require.NoError(t, err)
		require.Equal(t, "complete", last.record.Phase)
		require.Equal(t, second.task.Reference.TaskID, last.record.Frontier)
	})
	for _, scenario := range []string{"empty", "post-cutoff-only", "post-cutoff-prefix"} {
		t.Run(scenario+"-eof", func(t *testing.T) {
			f := newLegacyPageFixture(t)
			var seed cleanupBirthTestSeed
			if scenario == "post-cutoff-prefix" {
				seed = seedCleanupBirth(t, f.backfillBootstrapFixture)
			}
			state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
			if scenario == "post-cutoff-only" {
				task, intent, _, _ := taskTestRecords()
				task.Reference.Namespace, task.Reference.RestoreEpoch, task.Reference.Partition = f.b.namespace.Root(), f.b.restoreEpoch, 0xaa
				task.WorkspaceHash = "aa" + strings.Repeat("1", 62)
				intent.Task = task
				intent.Attempt.Namespace, intent.Attempt.RestoreEpoch, intent.Attempt.Partition = task.Reference.Namespace, task.Reference.RestoreEpoch, task.Reference.Partition
				seed = cleanupBirthTestSeed{task: task, intent: intent, stage: intent.Attempt.reference(strings.Repeat("d", 64))}
				seed = cleanupBackfillAdditionalBirth(t, f, seed, taskTestID, true)
			}
			if scenario == "post-cutoff-prefix" {
				cleanupBackfillAdditionalBirth(t, f, seed, "00000001-1111-4111-8111-111111111111", true)
			}
			if scenario != "empty" {
				observed := backfillBootstrapRead(t, f.backfillBootstrapFixture)
				backfillBootstrapCredit(f.b, f.claims, 1)
				var err error
				state, err = f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
				require.NoError(t, err)
			}
			page := f.read(t, state, nil)
			var birth *cleanupBirthEvidence
			if scenario == "post-cutoff-prefix" {
				birth = cleanupBirthTestRead(t, f.backfillBootstrapFixture, seed.task.Reference.TaskID)
			}
			backfillBootstrapCredit(f.b, f.claims, 1)
			next, err := f.b.advanceCleanupBackfill(context.Background(), f.partition, f.claims, page, birth, nil)
			require.NoError(t, err)
			require.Equal(t, "complete", next.record.Phase)
			if scenario == "empty" {
				require.Empty(t, next.record.Frontier)
			} else {
				require.Equal(t, seed.task.Reference.TaskID, next.record.Frontier)
				require.NotNil(t, f.server.point(seed.keys[5]))
			}
		})
	}
}

func TestCleanupBackfillAdvanceConflict(t *testing.T) {
	for _, target := range []string{"B-refresh", "B-ABA", "E-update", "E-ABA", "P-update", "P-ABA", "release-update", "release-ABA", "canceled-failed-CAS"} {
		t.Run(target, func(t *testing.T) {
			f := newLegacyPageFixture(t)
			s := seedCleanupBirth(t, f.backfillBootstrapFixture)
			cleanupBackfillSeedMetadata(t, f, s, true, true, true, false)
			state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
			page, birth := cleanupBackfillAdvanceRead(t, f, state, s)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			key := state.fence.key
			switch {
			case strings.HasPrefix(target, "E"):
				key = s.keys[4]
			case strings.HasPrefix(target, "P"):
				key = s.keys[5]
			case strings.HasPrefix(target, "release"):
				key = s.keys[6]
			}
			if target == "B-refresh" {
				observed := backfillBootstrapRead(t, f.backfillBootstrapFixture)
				backfillBootstrapCredit(f.b, f.claims, 1)
				_, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
				require.NoError(t, err)
			} else {
				kv := f.server.point(key)
				if strings.HasSuffix(target, "ABA") {
					_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(key)).Commit()
					require.NoError(t, err)
				}
				f.server.put(key, string(kv.Value), kv.Lease)
			}
			priorB, priorP := f.server.point(state.fence.key), f.server.point(s.keys[5])
			if target == "canceled-failed-CAS" {
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(state.fence.key), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
					require.NoError(t, err)
					require.False(t, r.Succeeded)
					cancel()
					return r, nil
				}}
			}
			backfillBootstrapCredit(f.b, f.claims, 1)
			next, err := f.b.advanceCleanupBackfill(ctx, f.partition, f.claims, page, birth, nil)
			require.Error(t, err)
			require.Nil(t, next)
			if target == "canceled-failed-CAS" {
				require.ErrorIs(t, err, ErrOutcomeUnknown)
				f.b.archiveLife.mu.Lock()
				liveErr := f.session.live()
				f.b.archiveLife.mu.Unlock()
				require.Error(t, liveErr)
			} else {
				require.ErrorIs(t, err, ErrConflict)
			}
			require.Equal(t, priorB, f.server.point(state.fence.key))
			require.Equal(t, priorP, f.server.point(s.keys[5]), "a failed frontier CAS cannot independently alter P")
		})
	}
	t.Run("unchanged-predecessor-retries-only-with-fresh-evidence", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		s := seedCleanupBirth(t, f.backfillBootstrapFixture)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		page, birth := cleanupBackfillAdvanceRead(t, f, state, s)
		cleanupBackfillSeedMetadata(t, f, s, true, false, false, false)
		backfillBootstrapCredit(f.b, f.claims, 1)
		next, err := f.b.advanceCleanupBackfill(context.Background(), f.partition, f.claims, page, birth, nil)
		require.ErrorIs(t, err, ErrConflict)
		require.Nil(t, next)
		require.Equal(t, state.fence, ownTaskFence(f.server.point(state.fence.key)))
		require.Nil(t, f.server.point(s.keys[5]))

		observed := backfillBootstrapRead(t, f.backfillBootstrapFixture)
		page, birth = cleanupBackfillAdvanceRead(t, f, observed.state, s)
		require.NotNil(t, birth.execution)
		backfillBootstrapCredit(f.b, f.claims, 1)
		next, err = f.b.advanceCleanupBackfill(context.Background(), f.partition, f.claims, page, birth, nil)
		require.NoError(t, err)
		require.Equal(t, "complete", next.record.Phase)
		require.NotNil(t, f.server.point(s.keys[5]))
	})
	t.Run("unknown-committed-ack-reloads-coverage-under-fresh-owner", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		s := seedCleanupBirth(t, f.backfillBootstrapFixture)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		page, birth := cleanupBackfillAdvanceRead(t, f, state, s)
		originalKV := f.b.client.KV
		f.b.client.KV = &faultKV{KV: originalKV, match: putsKey(state.fence.key), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			require.NoError(t, err)
			require.True(t, r.Succeeded)
			return nil, context.DeadlineExceeded
		}}
		backfillBootstrapCredit(f.b, f.claims, 1)
		next, err := f.b.advanceCleanupBackfill(context.Background(), f.partition, f.claims, page, birth, nil)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.Nil(t, next)
		committed := f.server.point(state.fence.key)
		require.Equal(t, committed.ModRevision, f.server.point(s.keys[5]).CreateRevision)
		f.b.client.KV = originalKV
		require.NoError(t, f.b.ReleaseWorkerSession(context.Background(), f.session))
		f.session, err = f.b.OpenWorkerSession(context.Background(), "fresh-backfill-owner")
		require.NoError(t, err)
		f.partition, err = f.b.ClaimWorkerPartition(context.Background(), f.session, 0xaa)
		require.NoError(t, err)
		f.ranges, err = f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetRange, 0)
		require.NoError(t, err)
		f.claims, err = f.b.AcquireWorkerBudget(context.Background(), f.session, WorkerBudgetClaim, 0)
		require.NoError(t, err)
		observed := backfillBootstrapRead(t, f.backfillBootstrapFixture)
		require.Equal(t, ownTaskFence(committed), observed.state.fence)
		require.Equal(t, "complete", observed.state.record.Phase)
		require.NotSame(t, state.partition, observed.state.partition)
		backfillBootstrapCredit(f.b, f.claims, 1)
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		next, err = f.b.advanceCleanupBackfill(context.Background(), f.partition, f.claims, page, birth, nil)
		require.Error(t, err)
		require.Nil(t, next)
		require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before, "old pointer lineage cannot be adopted after uncertain ACK")
	})
}

func TestCleanupBackfillAdvanceOwnership(t *testing.T) {
	t.Run("copied-foreign-and-continuation-refuse-before-credit", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		s := seedCleanupBirth(t, f.backfillBootstrapFixture)
		cleanupBackfillAdditionalBirth(t, f, s, "eeeeeeee-1111-4111-8111-111111111111", false)
		cleanupBackfillAdditionalBirth(t, f, s, "ffffffff-1111-4111-8111-111111111111", false)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		page, birth := cleanupBackfillAdvanceRead(t, f, state, s)
		continuation := f.read(t, state, page.next)
		other := newLegacyPageFixture(t)
		foreignSeed := seedCleanupBirth(t, other.backfillBootstrapFixture)
		foreignState := backfillBootstrapCreate(t, other.backfillBootstrapFixture)
		foreignPage, foreignBirth := cleanupBackfillAdvanceRead(t, other, foreignState, foreignSeed)
		copiedPage, copiedBirth := *page, *birth
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		backfillBootstrapCredit(f.b, f.claims, 1)
		for _, tc := range []struct {
			page  *cleanupLegacyPage
			birth *cleanupBirthEvidence
		}{
			{nil, birth}, {&copiedPage, birth}, {foreignPage, birth},
			{continuation, birth}, {page, nil}, {page, &copiedBirth}, {page, foreignBirth},
		} {
			next, err := f.b.advanceCleanupBackfill(context.Background(), f.partition, f.claims, tc.page, tc.birth, nil)
			require.Error(t, err)
			require.Nil(t, next)
		}
		f.b.archiveLife.mu.Lock()
		tokens := f.claims.bucket.tokens
		f.b.archiveLife.mu.Unlock()
		require.Equal(t, uint64(1), tokens)
		require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
	})
	t.Run("Close-joins-actual-CAS-return", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		s := seedCleanupBirth(t, f.backfillBootstrapFixture)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		page, birth := cleanupBackfillAdvanceRead(t, f, state, s)
		entered, release := make(chan struct{}, 1), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		t.Cleanup(unblock)
		f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(state.fence.key), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			entered <- struct{}{}
			<-release
			return r, err
		}}
		backfillBootstrapCredit(f.b, f.claims, 1)
		done, closed := make(chan error, 1), make(chan error, 1)
		go func() {
			_, err := f.b.advanceCleanupBackfill(context.Background(), f.partition, f.claims, page, birth, nil)
			done <- err
		}()
		workerTestReceive(t, entered)
		f.b.archiveLife.mu.Lock()
		callContext := f.claims.dueOwner.call.ctx
		f.b.archiveLife.mu.Unlock()
		go func() { closed <- f.b.Close() }()
		workerTestReceive(t, callContext.Done())
		f.b.archiveLife.mu.Lock()
		held := f.claims.dueOwner != nil
		f.b.archiveLife.mu.Unlock()
		require.True(t, held)
		select {
		case err := <-closed:
			t.Fatalf("Close returned before original CAS: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		unblock()
		require.ErrorIs(t, workerTestReceive(t, done), ErrOutcomeUnknown)
		require.NoError(t, workerTestReceive(t, closed))
	})
}

func TestCleanupBackfillAdvanceWire(t *testing.T) {
	f := newLegacyPageFixture(t)
	s := seedCleanupBirth(t, f.backfillBootstrapFixture)
	cleanupBackfillSeedMetadata(t, f, s, true, true, true, false)
	state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
	page, birth := cleanupBackfillAdvanceRead(t, f, state, s)
	backfillBootstrapCredit(f.b, f.claims, 1)
	owner, stop, err := f.b.beginExpiredTaskOwner(context.Background(), f.partition, f.claims)
	require.NoError(t, err)
	defer stop()
	defer owner.finish()
	next, mutation, err := f.b.cleanupBackfillAdvanceMutation(owner, page, birth, nil)
	require.NoError(t, err)
	require.Equal(t, "complete", next.Phase)
	require.Len(t, mutation.Comparisons, 48)
	require.Len(t, mutation.Writes, 1)
	require.NoError(t, f.b.preflightCleanupBackfillAdvance(mutation))

	// Literal maximal protobuf accounting only. The synthetic dimensions never
	// enter a builder, native CAS or proof API; deletion authority is still
	// deferred to a real signed-positive native composition test.
	ownedComparisons := make([]clientv3.Cmp, len(mutation.Comparisons))
	for i, cmp := range mutation.Comparisons {
		ownedComparisons[i] = copyComparison(cmp)
	}
	cmps := taskProtoComparisons(ownedComparisons)
	for _, cmp := range cmps {
		key := string(cmp.Key)
		cmp.Key = bytes.Repeat([]byte("k"), 1024)
		switch v := cmp.TargetUnion.(type) {
		case *pb.Compare_Value:
			limit := 2048
			switch key {
			case f.b.identityKey, s.keys[0], s.keys[1], s.keys[4]:
				limit = 4096
			case f.b.restoreKey:
				limit = 128
			case s.keys[6]:
				limit = 8192
			}
			v.Value = bytes.Repeat([]byte("v"), limit)
		case *pb.Compare_CreateRevision:
			v.CreateRevision = math.MaxInt64
		case *pb.Compare_ModRevision:
			v.ModRevision = math.MaxInt64
		case *pb.Compare_Lease:
			if v.Lease > 0 {
				v.Lease = math.MaxInt64
			}
		}
	}
	key := strings.Repeat("k", 1024)
	request := &pb.TxnRequest{Compare: cmps, Success: []*pb.RequestOp{{Request: &pb.RequestOp_RequestDeleteRange{RequestDeleteRange: &pb.DeleteRangeRequest{Key: []byte(key)}}}, taskProtoPut(key, strings.Repeat("b", 2048), 0)}, Failure: taskProtoPoints([]string{key, key})}
	require.Equal(t, 52, len(request.Compare)+len(request.Success)+len(request.Failure))
	require.LessOrEqual(t, request.Size(), 107136)
	require.LessOrEqual(t, request.Size(), 128*1024)
	failure := taskProtoResponse([]string{key, key}, []string{strings.Repeat("i", 4096), strings.Repeat("e", 128)})
	require.LessOrEqual(t, failure.Size(), 14592)
	require.LessOrEqual(t, failure.Size(), 256*1024)
	success := taskProtoPutResponse(2)
	success.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: success.Header, Deleted: math.MaxInt64}}}
	require.LessOrEqual(t, success.Size(), 4096)
	require.NoError(t, validateCleanupWire(cleanupWireProjection{name: "literal-backfill-terminal-maximum", request: request, success: success, failure: failure}))
	oversize := mutation
	oversize.Comparisons = append([]clientv3.Cmp(nil), mutation.Comparisons...)
	oversize.Comparisons[0] = clientv3.Compare(clientv3.Value(f.b.identityKey), "=", strings.Repeat("x", 128*1024))
	require.Error(t, f.b.preflightCleanupBackfillAdvance(oversize), "actual oversize must fail before padding")
}
