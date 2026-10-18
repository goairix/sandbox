//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This component starts only after the actual refund/replay assertions. A new
// original worker owns the cold job and every charged native turn. The ACK park
// controls test scheduling, not Run/Claim admission or a physical-work join.
// The fixture's existing cold option remains unchanged; verified quota binding
// is checked explicitly here even when that option is false. Automatic hint
// discovery, P deletion and managed physical admission are separate work.
func detachedPhysicalSettleOccupation(t *testing.T, f *cleanupDetachedNoEffectNativeFixture, roots *detachedPhysicalQuotaRefundRoots, sourceKeys []string) {
	t.Helper()
	require.Equal(t, "1", os.Getenv("SANDBOX_DETACHED_OCCUPATION_SETTLE_PHYSICAL_NATIVE"))
	require.NotNil(t, roots.entry)
	require.NotNil(t, roots.history.profileRelease)
	require.Nil(t, roots.history.profileRelease.local)
	release := roots.history.profileRelease.detached
	require.NotNil(t, release)
	before := detachedPhysicalQuotaHistoryPoints(t, f, sourceKeys)

	// The earlier independent paid session has finished its protocol calls.
	// Release its partition through the real API before a new worker claims it;
	// its original deferred release can join the same completed release later.
	b := f.b
	require.NoError(t, b.ReleaseWorkerPartition(f.ctx, roots.cursor.origin.partition))
	executionKey, err := b.namespace.cleanupExecutionKey(f.ref)
	require.NoError(t, err)
	require.NotNil(t, f.job.state)
	require.NotNil(t, f.job.state.slot)
	slotKey := f.job.state.slot.key
	pendingKey, err := b.namespace.cleanupPendingKey(f.ref)
	require.NoError(t, err)
	pendingBefore := detachedPhysicalQuotaHistoryPoints(t, f, []string{pendingKey})
	require.NotNil(t, pendingBefore[0])
	terminalKeys, err := b.cleanupTerminalHistoryKeys(f.job.state.record)
	require.NoError(t, err)
	taskKey, err := b.namespace.taskKey(f.ref)
	require.NoError(t, err)
	taskPoints := detachedPhysicalQuotaHistoryPoints(t, f, []string{taskKey})
	require.Len(t, taskPoints, 1)
	require.NotNil(t, taskPoints[0])
	var task TaskRecord
	require.NoError(t, decodeTaskRecord(taskPoints[0], &task))
	require.Equal(t, release.history.history[0], taskPoints[0])

	// Observe only this exact two-write settlement. Other real worker scans
	// delegate unchanged; total Txn counts would include that background work.
	type observation struct {
		request *pb.TxnRequest
		reply   *clientv3.TxnResponse
		err     error
		owner   *workerDueOwner
	}
	observed := make(chan observation, 1)
	published := make(chan error, 1)
	var settlements atomic.Int64
	var publications atomic.Int64
	var worker *DueWorker
	var job *cleanupLocalJob
	var metadataOwner *runtimeEnrolledPublicationOwner
	original := b.client.KV
	b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		terminalRead := len(yes) == len(terminalKeys)
		if terminalRead {
			for i, key := range terminalKeys {
				if !yes[i].IsGet() || string(yes[i].KeyBytes()) != key {
					terminalRead = false
					break
				}
			}
		}
		if terminalRead {
			reply, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
			if err == nil && reply != nil && reply.Succeeded && publications.Add(1) == 1 {
				// This hook executes on the original metadata actor after the
				// actual four-point RPC returns. The actor alone installs and
				// retires its shared workspace; background scans never race a
				// test goroutine assigning worker.coldQuota.
				var publishErr error
				metadataOwner, publishErr = b.registerEnrolledPublication(job.ctx)
				if publishErr == nil {
					workspace := &cleanupColdQuotaWorkspace{
						job: job, owner: metadataOwner, history: roots.history,
						ranges: worker.ranges[0], claims: worker.claims[0],
					}
					worker.coldQuota = workspace
					publishErr = worker.publishCleanupColdQuota(job, workspace, roots.entry)
					if publishErr == nil {
						publishErr = worker.validateCleanupColdQuotaVerified(job)
					}
				}
				published <- publishErr
			}
			return reply, err
		}
		if len(yes) != 2 || !yes[0].IsDelete() || string(yes[0].KeyBytes()) != slotKey || !yes[1].IsPut() || string(yes[1].KeyBytes()) != executionKey {
			return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		}
		ordinal := settlements.Add(1)
		failureKeys := make([]string, len(no))
		for i, op := range no {
			failureKeys[i] = string(op.KeyBytes())
		}
		request := &pb.TxnRequest{
			Compare: taskProtoComparisons(cmps),
			Success: taskProtoWrites([]Write{{Key: slotKey, Delete: true}, {Key: executionKey, Value: yes[1].ValueBytes()}}),
			Failure: taskProtoPoints(failureKeys),
		}
		reply, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		if ordinal == 1 {
			observed <- observation{request: request, reply: reply, err: err, owner: job.owner}
		}
		return reply, err
	}}
	parked := false
	defer func() {
		// Before the handoff this test owns the parked job; afterward the real
		// actor's stop path cancels and joins it before any owner is retired.
		if parked {
			b.finishCleanupJob(job, context.Canceled)
		}
		if worker != nil {
			// External original-session release supplies an independent bounded
			// cleanup context, then cancels/joins the real actor. A failed test
			// must not reuse that actor's already canceled work context for its
			// native lease/child cleanup or hide the primary settlement failure.
			cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
			require.NoError(t, b.ReleaseWorkerSession(cleanup, worker.session))
			stop()
			workerTestReceive(t, worker.Done())
		}
		if metadataOwner != nil {
			select {
			case <-metadataOwner.done:
			default:
				metadataOwner.finish()
			}
		}
		b.client.KV = original
	}()
	worker, err = b.StartDueWorker(f.ctx, DueWorkerOptions{
		WorkerID: "detached-occupation-settlement", Partitions: []uint8{f.ref.Partition},
		RangeSlots: []uint16{1}, ClaimSlots: []uint16{1}, RelistSlot: 0, StageTTL: 30 * time.Second,
	})
	require.NoError(t, err)
	require.NotSame(t, roots.cursor.origin.session, worker.session)
	scheduled := &cleanupDetachedNoEffectNativeFixture{cleanupSlotTestFixture: f.cleanupSlotTestFixture, worker: worker}
	job = scheduled.park(t)
	parked = true
	require.NotNil(t, job.state)
	require.Equal(t, "occupied", job.state.record.Phase)
	require.NotNil(t, job.state.record.Run)
	require.True(t, job.state.record.Run.Joined)
	require.NotNil(t, job.state.slot)
	require.Nil(t, job.claim)
	require.Empty(t, job.runID, "this cold job cannot claim the original Run as its physical work")
	prior := job.state

	// Ordinary coordinates come from actual immutable Task bytes. Only the
	// existing checked history and original postread entry supply release/refund
	// proof; no copied Task tuple or diagnostic stamp authorizes settlement.
	job.coldQuota = cleanupColdQuotaJob{
		self: job, record: task, birth: taskPoints[0].CreateRevision,
		workspace: roots.cursor.origin.workspace,
	}
	require.Nil(t, job.proof, "the actual detached proof must never become a local archive/Complete proof")

	// The actor now performs its actual charged four-point terminal read and
	// its next actual charged settlement. Unparking does not set Run.Joined,
	// create a Claim, fabricate terminal history or invoke a physical goroutine.
	b.archiveLife.mu.Lock()
	job.step = "terminal-history"
	job.started = false
	b.archiveLife.mu.Unlock()
	parked = false
	worker.wakeCleanup()
	workerTestReceive(t, job.done)
	require.NotNil(t, job.terminal, "the original terminal four-point read must authenticate before the v2 boundary")
	require.EqualValues(t, 1, publications.Load())
	require.NoError(t, workerTestReceive(t, published), "false cold option cannot skip original-job quota binding")
	t.Logf("actual detached occupation actor reached terminal history: error=%v settlement_submissions=%d", job.err, settlements.Load())
	require.NoError(t, job.err, "actual detached release and returned quota must settle the original Joined E/S occupation")
	require.Equal(t, OutcomeCommitted, job.result.Outcome)
	require.Equal(t, "complete", job.result.Reason)
	require.Equal(t, &TaskOwnerReleaseEntry{Task: f.ref, CreateRevision: release.kv.CreateRevision, SHA256: workspaceSyncIntentDigest(string(release.kv.Value))}, job.result.Release)
	require.EqualValues(t, 1, settlements.Load())
	actual := workerTestReceive(t, observed)
	require.NoError(t, actual.err)
	require.NotNil(t, actual.reply)
	require.True(t, actual.reply.Succeeded)
	require.NotNil(t, actual.owner)
	workerTestReceive(t, actual.owner.done)
	require.Len(t, actual.request.Compare, 44)
	require.Len(t, actual.request.Success, 2)
	require.Len(t, actual.request.Failure, 2)
	require.Equal(t, taskProtoPoints([]string{b.identityKey, b.restoreKey}), actual.request.Failure)
	for _, fence := range []taskFence{job.terminal.task, job.terminal.intent, job.terminal.receipt, prior.execution, *prior.slot, ownTaskFence(release.kv)} {
		for _, comparison := range taskProtoComparisons(fence.comparisons()) {
			require.Contains(t, actual.request.Compare, comparison)
		}
	}
	requestBytes, replyBytes := actual.request.Size(), (*pb.TxnResponse)(actual.reply).Size()
	require.LessOrEqual(t, requestBytes, 128<<10)
	require.LessOrEqual(t, replyBytes, 256<<10)
	t.Logf("actual detached occupation settlement: nodes=48 request_bytes=%d reply_bytes=%d; transport retries unmeasured", requestBytes, replyBytes)

	post, err := f.raw.Txn(f.ctx).Then(clientv3.OpGet(executionKey), clientv3.OpGet(slotKey), clientv3.OpGet(pendingKey)).Commit()
	require.NoError(t, err)
	require.NotNil(t, post.Header)
	require.Len(t, post.Responses, 3)
	require.Len(t, post.Responses[0].GetResponseRange().Kvs, 1)
	require.Empty(t, post.Responses[1].GetResponseRange().Kvs)
	require.Len(t, post.Responses[2].GetResponseRange().Kvs, 1)
	require.Equal(t, pendingBefore[0], post.Responses[2].GetResponseRange().Kvs[0], "A cannot delete P")
	var settled cleanupExecutionRecord
	require.NoError(t, decodeCleanupExecution(post.Responses[0].GetResponseRange().Kvs[0], post.Header.Revision, f.ref, &settled))
	expected := prior.record
	expected.Phase, expected.Confirmed, expected.Pending = "settled", "settled", nil
	expected.Release = &cleanupExecutionRelease{CreateRevision: release.kv.CreateRevision, Digest: workspaceSyncIntentDigest(string(release.kv.Value))}
	require.Equal(t, expected, settled)
	require.Nil(t, worker.coldQuota, "the shared graph must retire after the actual settlement owner joins")
	for i, key := range sourceKeys {
		if key == executionKey || key == slotKey {
			continue
		}
		require.Equal(t, before[i], detachedPhysicalQuotaHistoryPoints(t, f, []string{key})[0], key)
	}
}
