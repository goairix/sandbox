package etcd

import (
	"bytes"
	"context"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Host observations authenticate canonical metadata only. The deliberately
// proofless release wrappers below are refusal inputs, never signed-positive
// fixtures. Native automatic composition must test successful cold deletion.
func TestCleanupPendingDeleteAdmission(t *testing.T) {
	f := newLegacyPageFixture(t)
	s := seedCleanupBirth(t, f.backfillBootstrapFixture)
	cleanupBackfillSeedMetadata(t, f, s, false, true, false, false)
	absent := cleanupBirthTestRead(t, f.backfillBootstrapFixture, s.task.Reference.TaskID)
	cleanupBackfillSeedMetadata(t, f, s, false, false, true, false)
	birth := cleanupBirthTestRead(t, f.backfillBootstrapFixture, s.task.Reference.TaskID)
	require.Nil(t, absent.pending)
	require.NotNil(t, birth.pending)
	other := newBackfillBootstrapFixture(t, 60*time.Second)
	foreign := cleanupBirthReleaseTestObservation(t, other)
	copiedBirth, copiedPartition := *birth, *f.partition
	unsealed := &cleanupBirthReleaseEvidence{origin: f.b, partition: f.partition, birth: birth}
	unsealed.self = unsealed
	copiedRelease := *unsealed
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	backfillBootstrapCredit(f.b, f.claims, 1)
	before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
	priorP := f.server.point(s.keys[5])
	for _, tc := range []struct {
		name      string
		backend   *Backend
		ctx       context.Context
		partition *WorkerPartitionClaim
		share     *WorkerBudgetShare
		birth     *cleanupBirthEvidence
		release   *cleanupBirthReleaseEvidence
	}{
		{"nil-context", f.b, nil, f.partition, f.claims, birth, unsealed},
		{"nil-backend", nil, context.Background(), f.partition, f.claims, birth, unsealed},
		{"nil-birth", f.b, context.Background(), f.partition, f.claims, nil, unsealed},
		{"copied-birth", f.b, context.Background(), f.partition, f.claims, &copiedBirth, unsealed},
		{"foreign-birth", f.b, context.Background(), f.partition, f.claims, foreign, unsealed},
		{"copied-partition", f.b, context.Background(), &copiedPartition, f.claims, birth, unsealed},
		{"range-budget", f.b, context.Background(), f.partition, f.ranges, birth, unsealed},
		{"foreign-claim", f.b, context.Background(), f.partition, other.claims, birth, unsealed},
		{"nil-release", f.b, context.Background(), f.partition, f.claims, birth, nil},
		{"copied-release", f.b, context.Background(), f.partition, f.claims, birth, &copiedRelease},
		{"unsealed-release", f.b, context.Background(), f.partition, f.claims, birth, unsealed},
		{"absent-pending", f.b, context.Background(), f.partition, f.claims, absent, nil},
		{"canceled", f.b, canceled, f.partition, f.claims, birth, unsealed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.backend.deleteCleanupPending(tc.ctx, tc.partition, tc.share, tc.birth, tc.release)
			require.Error(t, err)
		})
	}
	// Missing genuine constructor configuration can also refuse host cases;
	// these assertions prove no dispatch/charge, not isolated signed-proof joins.
	require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
	require.Equal(t, priorP, f.server.point(s.keys[5]))
	f.b.archiveLife.mu.Lock()
	tokens, owner, liveErr := f.claims.bucket.tokens, f.claims.dueOwner, f.session.live()
	f.b.archiveLife.mu.Unlock()
	require.Equal(t, uint64(1), tokens)
	require.Nil(t, owner)
	require.NoError(t, liveErr)

	// Calling the pure builder with a genuine original owner still cannot turn
	// canonical metadata or a proofless wrapper into a deletion mutation.
	owned, stop, err := f.b.beginExpiredTaskOwner(context.Background(), f.partition, f.claims)
	require.NoError(t, err)
	defer stop()
	defer owned.finish()
	mutation, err := f.b.cleanupPendingDeleteMutation(owned, birth, unsealed)
	require.Error(t, err)
	require.Empty(t, mutation.Comparisons)
	require.Empty(t, mutation.Writes)
	require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
}

func TestCleanupPendingDeleteWire(t *testing.T) {
	for _, present := range []bool{false, true} {
		name := "absent-execution"
		if present {
			name = "present-execution-accounting-only"
		}
		t.Run(name, func(t *testing.T) {
			f := newLegacyPageFixture(t)
			s := seedCleanupBirth(t, f.backfillBootstrapFixture)
			cleanupBackfillSeedMetadata(t, f, s, present, true, true, false)
			birth := cleanupBirthTestRead(t, f.backfillBootstrapFixture, s.task.Reference.TaskID)
			f.b.archiveLife.mu.Lock()
			cmps, err := f.b.workerComparisons(f.session, f.partition, f.claims)
			f.b.archiveLife.mu.Unlock()
			require.NoError(t, err)
			for _, fence := range []taskFence{birth.task, birth.intent, birth.receipt} {
				cmps = append(cmps, fence.comparisons()...)
			}
			if birth.executionFence == nil {
				cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(s.keys[4]), "=", 0))
			} else {
				cmps = append(cmps, birth.executionFence.comparisons()...)
			}
			cmps = append(cmps, birth.release.comparisons()...)
			cmps = append(cmps, birth.pending.comparisons()...)
			count := 41
			if present {
				count = 44
			}
			require.Len(t, cmps, count)

			// This manual mutation enters accounting only, never a native builder
			// or client. Present E is selecting, which cannot authorize deletion.
			mutation := Mutation{Comparisons: cmps, Writes: []Write{{Key: s.keys[5], Delete: true}}}
			before := len(backfillBootstrapRequests(f.backfillBootstrapFixture))
			require.NoError(t, f.b.preflightCleanupPendingDelete(mutation))
			owned := make([]clientv3.Cmp, len(cmps))
			for i, cmp := range cmps {
				owned[i] = copyComparison(cmp)
			}
			maximum := taskProtoComparisons(owned)
			for _, cmp := range maximum {
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
					if v.CreateRevision > 0 {
						v.CreateRevision = math.MaxInt64
					}
				case *pb.Compare_ModRevision:
					v.ModRevision = math.MaxInt64
				case *pb.Compare_Lease:
					if v.Lease > 0 {
						v.Lease = math.MaxInt64
					}
				}
			}
			key := strings.Repeat("k", 1024)
			request := &pb.TxnRequest{Compare: maximum, Success: taskProtoWrites([]Write{{Key: key, Delete: true}}), Failure: taskProtoPoints([]string{key, key})}
			require.Equal(t, count+3, len(request.Compare)+len(request.Success)+len(request.Failure))
			require.LessOrEqual(t, request.Size(), 95232)
			success := taskProtoPutResponse(1)
			success.Responses[0] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: success.Header, Deleted: math.MaxInt64}}}
			failure := taskProtoResponse([]string{key, key}, []string{strings.Repeat("i", 4096), strings.Repeat("e", 128)})
			require.LessOrEqual(t, success.Size(), 4096)
			require.LessOrEqual(t, failure.Size(), 14592)
			require.NoError(t, validateCleanupWire(cleanupWireProjection{name: "literal-pending-delete", request: request, success: success, failure: failure}))

			backfillKey, err := f.b.namespace.cleanupBackfillKey(f.partition.partition)
			require.NoError(t, err)
			for _, writes := range [][]Write{
				nil,
				{{Key: s.keys[5], Value: []byte("put")}},
				{{Key: backfillKey, Delete: true}},
				{{Key: s.keys[5], Delete: true}, {Key: backfillKey, Value: []byte("put")}},
				{{Key: s.keys[5], Delete: true, Value: []byte("unexpected")}},
			} {
				require.Error(t, f.b.preflightCleanupPendingDelete(Mutation{Comparisons: cmps, Writes: writes}))
			}
			short := mutation
			short.Comparisons = cmps[:len(cmps)-1]
			require.Error(t, f.b.preflightCleanupPendingDelete(short), "only 41/44 comparison shapes are admitted")
			for _, replacement := range []clientv3.Cmp{
				clientv3.Compare(clientv3.Value(f.b.identityKey), "=", strings.Repeat("x", 128*1024)),
				clientv3.Compare(clientv3.Value(f.b.namespace.Root()+"unknown-role"), "=", "x"),
			} {
				bad := mutation
				bad.Comparisons = append([]clientv3.Cmp(nil), cmps...)
				bad.Comparisons[0] = replacement
				require.Error(t, f.b.preflightCleanupPendingDelete(bad), "actual bytes and closed roles precede padding")
			}
			require.Len(t, backfillBootstrapRequests(f.backfillBootstrapFixture), before)
			require.Equal(t, f.b.identityValue, string(cmps[0].TargetUnion.(*pb.Compare_Value).Value), "projection must not rewrite native comparison bytes")
		})
	}
}
