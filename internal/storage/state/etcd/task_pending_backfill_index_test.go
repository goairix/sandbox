package etcd

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This is metadata-only acceptance: P is a discovery hint. Neither branch
// manufactures a normal birth, a signed release, or physical execution.
func TestCleanupBackfillIndex(t *testing.T) {
	for _, present := range []bool{false, true} {
		name := "create-P-with-frontier"
		if present {
			name = "preserve-existing-P"
		}
		t.Run(name, func(t *testing.T) {
			f := newLegacyPageFixture(t)
			s := cleanupIndexSeed(t, f, present)
			state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
			page := f.read(t, state, nil)
			index := cleanupIndexRead(t, f, s)
			priorP, priorE := f.server.point(s.keys[5]), f.server.point(s.keys[4])
			before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
			backfillBootstrapCredit(f.b, f.claims, 1)
			next, err := f.b.advanceCleanupBackfillIndex(context.Background(), f.partition, f.claims, page, index)
			require.NoError(t, err)
			require.Equal(t, "complete", next.record.Phase)
			require.Equal(t, s.task.Reference.TaskID, next.record.Frontier)
			require.Equal(t, state.record.LegacyCutoffRevision, next.record.LegacyCutoffRevision)
			require.Equal(t, state.record.ReadViewRevision, next.record.ReadViewRevision)
			pending := f.server.point(s.keys[5])
			require.NotNil(t, pending)
			r, err := decodeCleanupPending(f.b.namespace, f.b.restoreEpoch, pending, next.fence.mod)
			require.NoError(t, err)
			require.Equal(t, s.task.Reference, r.Reference)
			require.Equal(t, workerIdentityDigest(s.wires[0]), r.TaskDigest)
			if present {
				require.Equal(t, priorP, pending)
			} else {
				require.Equal(t, next.fence.mod, pending.CreateRevision)
			}
			require.Equal(t, priorE, f.server.point(s.keys[4]))
			require.Nil(t, f.server.point(s.keys[6]))

			requests := backfillBootstrapRequests(f.backfillBootstrapFixture)[before:]
			require.Len(t, requests, 1)
			q := requests[0]
			comparisons, writes, nodes := 41, 2, 45
			if present {
				comparisons, writes, nodes = 44, 1, 47
			}
			require.Len(t, q.Compare, comparisons)
			require.Len(t, q.Success, writes)
			require.Len(t, q.Failure, 2)
			require.Equal(t, nodes, len(q.Compare)+len(q.Success)+len(q.Failure))
			f.b.archiveLife.mu.Lock()
			worker, err := f.b.workerComparisons(f.session, f.partition, f.claims)
			f.b.archiveLife.mu.Unlock()
			require.NoError(t, err)
			require.Equal(t, taskProtoComparisons(worker), q.Compare[:20])
			for _, fence := range []taskFence{state.fence, index.task, index.intent, index.link, index.receipt} {
				for _, cmp := range taskProtoComparisons(fence.comparisons()) {
					require.Contains(t, q.Compare, cmp)
				}
			}
			pendingComparisons := []clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(s.keys[5]), "=", 0)}
			if index.pending != nil {
				pendingComparisons = index.pending.comparisons()
			}
			for _, cmp := range taskProtoComparisons(pendingComparisons) {
				require.Contains(t, q.Compare, cmp)
			}
			for _, cmp := range q.Compare {
				require.NotContains(t, []string{s.keys[4], s.keys[6]}, string(cmp.Key), "optional refusal must not become invented absence")
			}
			for i, op := range q.Success {
				require.NotNil(t, op.GetRequestPut())
				key := state.fence.key
				if i < writes-1 {
					key = s.keys[5]
				}
				require.Equal(t, key, string(op.GetRequestPut().Key))
			}
			require.Equal(t, taskProtoPoints([]string{f.b.identityKey, f.b.restoreKey}), q.Failure)
		})
	}
	t.Run("two-legacy-rows-cover-only-the-first-prefix", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		first := cleanupIndexSeed(t, f, false)
		second := cleanupBackfillAdditionalBirth(t, f, first, "ffffffff-1111-4111-8111-111111111111", false)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		page := f.read(t, state, nil)
		index := cleanupIndexRead(t, f, first)
		require.True(t, page.eof)
		require.Len(t, page.rows, 2)
		backfillBootstrapCredit(f.b, f.claims, 1)
		next, err := f.b.advanceCleanupBackfillIndex(context.Background(), f.partition, f.claims, page, index)
		require.NoError(t, err)
		require.Equal(t, "list", next.record.Phase)
		require.Equal(t, first.task.Reference.TaskID, next.record.Frontier)
		require.NotNil(t, f.server.point(first.keys[5]))
		require.Nil(t, f.server.point(second.keys[5]))
		remaining := f.read(t, next, nil)
		require.Len(t, remaining.rows, 1)
		require.Equal(t, second.task.Reference.TaskID, remaining.rows[0].taskID)
	})
}

func TestCleanupBackfillIndexOwnership(t *testing.T) {
	t.Run("copied-unsealed-foreign-refuse-before-credit", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		s := cleanupIndexSeed(t, f, false)
		cleanupBackfillAdditionalBirth(t, f, s, "eeeeeeee-1111-4111-8111-111111111111", false)
		cleanupBackfillAdditionalBirth(t, f, s, "ffffffff-1111-4111-8111-111111111111", false)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		page := f.read(t, state, nil)
		continuation := f.read(t, state, page.next)
		index := cleanupIndexRead(t, f, s)
		copied, copiedPage := *index, *page
		other := newLegacyPageFixture(t)
		foreign := cleanupIndexRead(t, other, cleanupIndexSeed(t, other, false))
		_, err := f.b.cleanupBirthFromPoints(f.partition, f.server.point(s.keys[1]), cleanupBirthTestPoints(f.backfillBootstrapFixture, s), index.revision)
		var draft *cleanupBirthOptionalRefusal
		require.ErrorAs(t, err, &draft)
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		backfillBootstrapCredit(f.b, f.claims, 1)
		for _, tc := range []struct {
			page  *cleanupLegacyPage
			index *cleanupBirthIndexEvidence
		}{
			{page, nil}, {page, &copied}, {page, foreign}, {page, draft.index}, {&copiedPage, index}, {continuation, index},
		} {
			next, err := f.b.advanceCleanupBackfillIndex(context.Background(), f.partition, f.claims, tc.page, tc.index)
			require.Error(t, err)
			require.Nil(t, next)
		}
		f.b.archiveLife.mu.Lock()
		tokens := f.claims.bucket.tokens
		f.b.archiveLife.mu.Unlock()
		require.Equal(t, uint64(1), tokens)
		require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
	})
	for _, target := range []string{"T", "I", "link", "receipt", "P", "B", "W", "B-refresh", "P-insert", "canceled-failed-CAS"} {
		t.Run(target+"-conflict", func(t *testing.T) {
			f := newLegacyPageFixture(t)
			s := cleanupIndexSeed(t, f, target != "P-insert")
			state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
			page := f.read(t, state, nil)
			index := cleanupIndexRead(t, f, s)
			keys := map[string]string{"T": s.keys[0], "I": s.keys[1], "link": s.keys[2], "receipt": s.keys[3], "P": s.keys[5], "B": state.fence.key, "W": f.partition.fence.key, "canceled-failed-CAS": state.fence.key}
			switch target {
			case "B-refresh":
				observed := backfillBootstrapRead(t, f.backfillBootstrapFixture)
				backfillBootstrapCredit(f.b, f.claims, 1)
				_, err := f.b.refreshCleanupBackfill(context.Background(), f.partition, f.claims, observed)
				require.NoError(t, err)
			case "P-insert":
				cleanupBackfillSeedMetadata(t, f, s, false, false, true, false)
			default:
				key := keys[target]
				prior := f.server.point(key)
				_, err := f.b.client.Txn(context.Background()).Then(clientv3.OpDelete(key)).Commit()
				require.NoError(t, err)
				f.server.put(key, string(prior.Value), prior.Lease)
			}
			priorB, priorP := f.server.point(state.fence.key), f.server.point(s.keys[5])
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if target == "canceled-failed-CAS" {
				f.b.client.KV = &faultKV{KV: f.b.client.KV, match: putsKey(state.fence.key), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
					require.NoError(t, err)
					require.False(t, r.Succeeded)
					cancel()
					return r, nil
				}}
			}
			backfillBootstrapCredit(f.b, f.claims, 1)
			next, err := f.b.advanceCleanupBackfillIndex(ctx, f.partition, f.claims, page, index)
			require.Nil(t, next)
			if target == "canceled-failed-CAS" {
				require.ErrorIs(t, err, ErrOutcomeUnknown)
			} else {
				require.ErrorIs(t, err, ErrConflict)
			}
			require.Equal(t, priorB, f.server.point(state.fence.key))
			require.Equal(t, priorP, f.server.point(s.keys[5]))
		})
	}
	t.Run("committed-lost-ACK-requires-fresh-owner-observation", func(t *testing.T) {
		f := newLegacyPageFixture(t)
		s := cleanupIndexSeed(t, f, false)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		page := f.read(t, state, nil)
		index := cleanupIndexRead(t, f, s)
		originalKV := f.b.client.KV
		f.b.client.KV = &faultKV{KV: originalKV, match: putsKey(state.fence.key), after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
			require.NoError(t, err)
			require.True(t, r.Succeeded)
			return nil, context.DeadlineExceeded
		}}
		backfillBootstrapCredit(f.b, f.claims, 1)
		next, err := f.b.advanceCleanupBackfillIndex(context.Background(), f.partition, f.claims, page, index)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.Nil(t, next)
		committed := f.server.point(state.fence.key)
		require.Equal(t, committed.ModRevision, f.server.point(s.keys[5]).CreateRevision)
		f.b.archiveLife.mu.Lock()
		liveErr := f.session.live()
		f.b.archiveLife.mu.Unlock()
		require.Error(t, liveErr)
		f.b.client.KV = originalKV
		require.NoError(t, f.b.ReleaseWorkerSession(context.Background(), f.session))
		f.session, err = f.b.OpenWorkerSession(context.Background(), "fresh-index-owner")
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
		before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
		backfillBootstrapCredit(f.b, f.claims, 1)
		next, err = f.b.advanceCleanupBackfillIndex(context.Background(), f.partition, f.claims, page, index)
		require.Error(t, err)
		require.Nil(t, next)
		require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
	})
}

func TestCleanupBackfillIndexWire(t *testing.T) {
	for _, present := range []bool{false, true} {
		f := newLegacyPageFixture(t)
		s := cleanupIndexSeed(t, f, present)
		state := backfillBootstrapCreate(t, f.backfillBootstrapFixture)
		page := f.read(t, state, nil)
		index := cleanupIndexRead(t, f, s)
		backfillBootstrapCredit(f.b, f.claims, 1)
		owner, stop, err := f.b.beginExpiredTaskOwner(context.Background(), f.partition, f.claims)
		require.NoError(t, err)
		next, mutation, err := f.b.cleanupBackfillIndexMutation(owner, page, index)
		owner.finish()
		stop()
		require.NoError(t, err)
		require.Equal(t, "complete", next.Phase)
		require.NoError(t, f.b.preflightCleanupBackfillIndex(mutation))

		// Literal maximal dimensions size both branches independently of the
		// production padding. These values never enter an authority API or CAS.
		cmps := make([]clientv3.Cmp, len(mutation.Comparisons))
		for i, cmp := range mutation.Comparisons {
			cmps[i] = copyComparison(cmp)
		}
		maximum := taskProtoComparisons(cmps)
		for _, cmp := range maximum {
			key := string(cmp.Key)
			cmp.Key = bytes.Repeat([]byte("k"), 1024)
			switch v := cmp.TargetUnion.(type) {
			case *pb.Compare_Value:
				limit := 2048
				switch key {
				case f.b.identityKey, s.keys[0], s.keys[1]:
					limit = 4096
				case f.b.restoreKey:
					limit = 128
				}
				v.Value = bytes.Repeat([]byte("v"), limit)
			case *pb.Compare_CreateRevision:
				v.CreateRevision = math.MaxInt64
			case *pb.Compare_ModRevision:
				v.ModRevision = math.MaxInt64
			case *pb.Compare_Lease:
				v.Lease = math.MaxInt64
			}
		}
		key := strings.Repeat("k", 1024)
		writes := []*pb.RequestOp{taskProtoPut(key, strings.Repeat("b", 2048), 0)}
		if !present {
			writes = append(writes, taskProtoPut(key, strings.Repeat("p", 2048), 0))
		}
		request := &pb.TxnRequest{Compare: maximum, Success: writes, Failure: taskProtoPoints([]string{key, key})}
		require.LessOrEqual(t, request.Size(), 91136)
		require.LessOrEqual(t, len(maximum)+len(writes)+2, 47)
		failure := taskProtoResponse([]string{key, key}, []string{strings.Repeat("i", 4096), strings.Repeat("e", 128)})
		success := taskProtoPutResponse(len(writes))
		require.LessOrEqual(t, failure.Size(), 14592)
		require.LessOrEqual(t, success.Size(), 4096)
		require.NoError(t, validateCleanupWire(cleanupWireProjection{name: "literal-index-maximum", request: request, success: success, failure: failure}))

		for _, bad := range []string{"oversize", "delete", "execution-role", "release-role", "link-oversize"} {
			invalid := mutation
			invalid.Comparisons = append([]clientv3.Cmp(nil), mutation.Comparisons...)
			invalid.Writes = append([]Write(nil), mutation.Writes...)
			switch bad {
			case "oversize":
				invalid.Comparisons[0] = clientv3.Compare(clientv3.Value(f.b.identityKey), "=", strings.Repeat("x", 128*1024))
			case "delete":
				invalid.Writes[0] = Write{Key: s.keys[5], Delete: true}
			case "execution-role":
				invalid.Comparisons[0] = clientv3.Compare(clientv3.CreateRevision(s.keys[4]), "=", 0)
			case "release-role":
				invalid.Comparisons[0] = clientv3.Compare(clientv3.CreateRevision(s.keys[6]), "=", 0)
			case "link-oversize":
				invalid.Comparisons[0] = clientv3.Compare(clientv3.Value(s.keys[2]), "=", strings.Repeat("x", 2049))
			}
			require.Error(t, f.b.preflightCleanupBackfillIndex(invalid), bad)
		}
	}
}
