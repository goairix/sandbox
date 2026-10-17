package etcd

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This pure fixture sizes the same destroy mutation used by real producers.
// Synthetic maximum worker values are wire inputs only, never worker authority.
func cleanupPendingWireFixture(t *testing.T, due bool) (*Backend, Mutation, StageReference) {
	t.Helper()
	b, source := cleanupLocalWireFixture(t, true)
	task := source.bundle.task
	control := SandboxControlRecord{Version: 1, SandboxID: task.Reference.SandboxID, WorkspaceHash: task.WorkspaceHash, IntentID: strings.Repeat("i", 128), RestoreEpoch: task.Reference.RestoreEpoch, Generation: math.MaxInt64, DataGateEpoch: math.MaxInt64, Phase: PhaseActive, Snapshot: task.Snapshot, Runtime: &RuntimeReference{ID: strings.Repeat("r", 128), UID: strings.Repeat("u", 128), BootID: strings.Repeat("b", 128)}, ExpiresAt: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)}
	bundle := &operationControlBundle{Control: control, Partition: task.Reference.Partition}
	for _, fence := range source.bundle.fences[3:] {
		bundle.Keys = append(bundle.Keys, fence.key)
		bundle.KVs = append(bundle.KVs, &mvccpb.KeyValue{Key: []byte(fence.key), Value: []byte(fence.value), Version: 1, CreateRevision: math.MaxInt64, ModRevision: math.MaxInt64})
	}
	attempt := source.bundle.initial
	attempt.StageID = "destroy"
	mutation, err := buildTaskDestroyMutation(b.namespace, bundle, task.Reference, attempt)
	require.NoError(t, err)
	if due {
		for i := 0; i < 4; i++ {
			key, err := b.namespace.Key("p", fmt.Sprintf("%02x", task.Reference.Partition), "wire-worker", fmt.Sprint(i))
			require.NoError(t, err)
			fence := taskFence{key: key, value: strings.Repeat("w", 2048), create: math.MaxInt64, mod: math.MaxInt64, lease: math.MaxInt64}
			mutation.Comparisons = append(mutation.Comparisons, fence.comparisons()...)
		}
		candidate := dueCandidateForControl(task.Reference.Partition, control)
		key, err := b.namespace.dueCandidateKey(candidate)
		require.NoError(t, err)
		value, err := encodeDueCandidate(candidate)
		require.NoError(t, err)
		mutation.Comparisons = append(mutation.Comparisons, clientv3.Compare(clientv3.Value(key), "=", value), clientv3.Compare(clientv3.ModRevision(key), "=", int64(math.MaxInt64)))
		mutation.Writes = append(mutation.Writes, Write{Key: key, Delete: true})
	}
	owned, digest, err := prepareMutation(b.namespace, mutation)
	require.NoError(t, err)
	return b, owned, attempt.reference(digest)
}

func TestCleanupPendingProducerWire(t *testing.T) {
	for _, due := range []bool{false, true} {
		t.Run(fmt.Sprintf("maximum-due-%t", due), func(t *testing.T) {
			b, mutation, ref := cleanupPendingWireFixture(t, due)
			projections, err := b.taskDestroyWireProjections(ref, mutation)
			require.NoError(t, err)
			require.Len(t, projections, 3)
			compare, success := 33, 7
			if due {
				compare, success = 51, 8
			}
			for i, shape := range [][3]int{{6, 1, 2}, {compare, success, 4}, {5, 1, 3}} {
				projection := projections[i]
				require.Len(t, projection.request.Compare, shape[0])
				require.Len(t, projection.request.Success, shape[1])
				require.Len(t, projection.request.Failure, shape[2])
				require.LessOrEqual(t, projection.request.Size(), 128*1024)
				require.LessOrEqual(t, projection.success.Size(), 256*1024)
				require.LessOrEqual(t, projection.failure.Size(), 256*1024)
				require.Equal(t, int64(math.MaxInt64), projection.success.Header.Revision)
				require.Equal(t, int64(math.MaxInt64), projection.failure.Header.Revision)
			}
			begin, commit := projections[0].request, projections[1].request
			require.Equal(t, int64(math.MaxInt64), begin.Success[0].GetRequestPut().Lease)
			require.Equal(t, int64(math.MaxInt64), commit.Compare[5].GetLease())
			require.Equal(t, int64(math.MaxInt64), commit.Compare[6].GetCreateRevision())
			if due {
				require.Equal(t, 63, len(commit.Compare)+len(commit.Success)+len(commit.Failure))
				require.NotNil(t, commit.Success[6].GetRequestDeleteRange())
				require.EqualValues(t, 1, projections[1].success.Responses[6].GetResponseDeleteRange().Deleted)
			}
			require.NoError(t, b.preflightTaskDestroy(ref, mutation))
		})
	}

	for _, due := range []bool{false, true} {
		t.Run(fmt.Sprintf("actual-producer-due-%t", due), func(t *testing.T) {
			f := newDueExpiryFixture(t)
			if due {
				f.earnClaim()
				result, err := f.admit()
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, result.Outcome)
			} else {
				control := f.server.point(f.keys[1])
				stage, _, err := f.b.PrepareDestroy(context.Background(), BeginDestroyInput{SandboxID: f.control.SandboxID, RequestID: "pending-manual", ExpectedControlRevision: control.ModRevision}, 30*time.Second)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, f.b.ReleaseStage(context.Background(), stage)) })
				outcome, err := f.b.CommitStage(context.Background(), stage)
				require.NoError(t, err)
				require.Equal(t, OutcomeCommitted, outcome)
			}
			var commit *pb.TxnRequest
			f.server.mu.Lock()
			for _, request := range f.server.requests {
				for _, op := range request.Success {
					if put := op.GetRequestPut(); put != nil && string(put.Key) == f.keys[1] {
						commit = request
					}
				}
			}
			f.server.mu.Unlock()
			require.NotNil(t, commit)
			compare, success := 33, 7
			if due {
				compare, success = 51, 8
				require.NotNil(t, commit.Success[6].GetRequestDeleteRange())
			}
			require.Len(t, commit.Compare, compare)
			require.Len(t, commit.Success, success)
			require.Len(t, commit.Failure, 4)
			require.LessOrEqual(t, commit.Size(), 128*1024)
			// P is the sixth business write in either actual producer; it is
			// covered by an absence compare in the very same native request.
			pending := commit.Success[5].GetRequestPut()
			require.NotNil(t, pending)
			require.Contains(t, string(pending.Key), "/cleanup-pending/")
			absence := clientv3.Compare(clientv3.CreateRevision(string(pending.Key)), "=", 0)
			require.Contains(t, commit.Compare, (*pb.Compare)(&absence))
		})
	}

	t.Run("narrow-preflight-refuses-generic-admissible-oversize", func(t *testing.T) {
		b, mutation, ref := cleanupPendingWireFixture(t, false)
		// These synthetic compare values fit generic Stage's record/node/JSON
		// limits but exceed this producer's128KiB wire contract. This is a
		// pure validator test, not a claim that valid domain rows are this big.
		for _, i := range []int{0, 4} {
			mutation.Comparisons[i] = clientv3.Compare(clientv3.Value(string(mutation.Comparisons[i].Key)), "=", strings.Repeat("x", 64*1024))
		}
		owned, digest, err := prepareMutation(b.namespace, mutation)
		require.NoError(t, err)
		ref.Digest = digest
		require.Error(t, b.preflightTaskDestroy(ref, owned))
		require.Nil(t, b.client, "pure preflight runs without a Grant-capable client")
	})
}
