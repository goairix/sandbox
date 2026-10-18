//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"os"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This separate scenario loses only the ACK of an actually committed public
// refund. The default refund scenario exercises the paid writer instead. Both
// inherit the real physical proof and the same seven-phase authenticated source;
// a codec model, receipt alone or manually seeded free slot cannot enter here.
func detachedPhysicalQuotaRefundUnknown(t *testing.T, f *cleanupDetachedNoEffectNativeFixture, roots *detachedPhysicalQuotaRefundRoots, claims *WorkerBudgetShare, sourceKeys []string, before []*mvccpb.KeyValue) {
	t.Helper()
	require.Equal(t, "1", os.Getenv("SANDBOX_DETACHED_QUOTA_REFUND_UNKNOWN_PHYSICAL_NATIVE"))
	require.NotNil(t, roots.history)
	require.Equal(t, coldQuotaWorkerHistoryDone, roots.cursor.phase)
	b, origin := f.b, roots.cursor.origin
	returnKey, err := b.namespace.coldRuntimeQuotaReturnKey(origin.workspace.Partition(), origin.intent)
	require.NoError(t, err)
	journalKey, err := b.namespace.coldRuntimeQuotaAttemptKey(origin.workspace.Partition(), origin.intent)
	require.NoError(t, err)
	slotKey, freeKey, err := b.namespace.coldSlotKeys(roots.history.reservation.Quota)
	require.NoError(t, err)

	// The old worker is already stopped and the physical job has really joined.
	// Restore these public-call wrappers before returning to the parent helper's
	// session/budget defers. Paid Invoke bypasses them, so they are never used to
	// claim counts for paid transport dispatch or internal retries.
	originalKV, originalLease := b.client.KV, b.client.Lease
	leases := &creationFaultLease{Lease: originalLease}
	var submitted, grantedID, returnedRevokes atomic.Int64
	leases.grant = func(reply *clientv3.LeaseGrantResponse, err error) (*clientv3.LeaseGrantResponse, error) {
		require.NoError(t, err)
		require.NotNil(t, reply)
		require.Positive(t, int64(reply.ID))
		require.True(t, grantedID.CompareAndSwap(0, int64(reply.ID)), "the original public call has one guard Grant")
		return reply, err
	}
	leases.revoke = func(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
		require.Positive(t, grantedID.Load())
		require.Equal(t, grantedID.Load(), int64(id), "release only the original guard lease")
		reply, err := originalLease.Revoke(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, reply)
		// The shared wrapper's revokes counter counts entry, not completion.
		// Publish this separate observation only after the original RPC returns.
		returnedRevokes.Add(1)
		return reply, err
	}
	b.client.Lease = leases
	b.client.KV = &finalSyncRecordingKV{KV: originalKV, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		refund := false
		for _, operation := range yes {
			refund = refund || operation.IsPut() && string(operation.KeyBytes()) == returnKey
		}
		if !refund {
			return originalKV.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		}
		require.EqualValues(t, 1, submitted.Add(1), "an uncertain refund cannot resubmit its native Commit")
		require.Len(t, cmps, 34)
		require.Len(t, yes, 4)
		require.Len(t, no, 4)
		guardComparisons := 0
		for _, comparison := range cmps {
			wire := pb.Compare(comparison)
			if wire.Target == pb.Compare_LEASE && wire.GetLease() == grantedID.Load() {
				guardComparisons++
			}
		}
		require.Equal(t, 1, guardComparisons, "the actual Commit pins the same granted guard lease")
		writes := make([]Write, len(yes))
		for i, operation := range yes {
			require.True(t, operation.IsPut())
			require.Equal(t, clientv3.OpPut(string(operation.KeyBytes()), string(operation.ValueBytes())), operation)
			writes[i] = Write{Key: string(operation.KeyBytes()), Value: operation.ValueBytes()}
		}
		failure := make([]string, len(no))
		for i, operation := range no {
			require.True(t, operation.IsGet())
			require.Equal(t, clientv3.OpGet(string(operation.KeyBytes())), operation)
			failure[i] = string(operation.KeyBytes())
		}
		request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites(writes), Failure: taskProtoPoints(failure)}
		require.LessOrEqual(t, request.Size(), 128<<10)
		response, err := originalKV.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		require.NoError(t, err)
		require.NotNil(t, response)
		require.True(t, response.Succeeded, "lose the ACK only after the real refund and receipt commit")
		require.LessOrEqual(t, (*pb.TxnResponse)(response).Size(), 256<<10)
		t.Logf("actual detached public refund committed before ACK loss: nodes=42 request=%d reply=%d", request.Size(), (*pb.TxnResponse)(response).Size())
		return nil, context.DeadlineExceeded
	}}
	defer func() {
		b.client.KV, b.client.Lease = originalKV, originalLease
	}()

	lost, err := b.SettleColdRuntimeQuota(f.ctx, origin.workspace, origin.intent, origin.task)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, lost.Outcome)
	require.Nil(t, lost.Entry)
	require.NotEmpty(t, lost.Reference.AttemptID)
	require.EqualValues(t, 1, submitted.Load())
	require.EqualValues(t, 1, leases.grants.Load(), "one original public Stage Grant")
	require.EqualValues(t, 1, leases.revokes.Load())
	require.EqualValues(t, 1, returnedRevokes.Load(), "the original guard Revoke must return before Settle returns")
	immutable := detachedPhysicalQuotaHistoryPoints(t, f, []string{returnKey, journalKey})
	require.NotNil(t, immutable[0])
	require.NotNil(t, immutable[1])
	record, err := decodeDetachedColdRuntimeQuotaReturn(immutable[0])
	require.NoError(t, err)
	require.Equal(t, roots.cursor.releaseProof.detached.record, record.Release)
	attempt, err := b.decodeColdRuntimeQuotaAttempt(roots.history, immutable[1])
	require.NoError(t, err)
	require.Equal(t, lost.Reference, attempt.Reference)
	_, receiptKey, err := b.stageKeys(lost.Reference)
	require.NoError(t, err)
	receipt := detachedPhysicalQuotaHistoryPoints(t, f, []string{receiptKey})
	outcome, err := decodeReceipt(receipt[0], lost.Reference)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	require.Equal(t, immutable[0].CreateRevision, receipt[0].CreateRevision)
	free := detachedPhysicalQuotaHistoryPoints(t, f, []string{slotKey, freeKey})
	require.NotNil(t, free[0])
	require.NotNil(t, free[1])
	var slot coldSlotRecord
	require.NoError(t, coldDecode(free[0], &slot))
	require.False(t, slot.Occupied)
	require.Equal(t, roots.history.reservation.Quota, slot.Slot)
	require.Equal(t, free[0].Value, free[1].Value)

	// Recovery must authenticate that exact immutable return and receipt before
	// consulting a mutable slot or admitting another journal/Grant/Commit.
	replayed, err := b.SettleColdRuntimeQuota(f.ctx, origin.workspace, origin.intent, origin.task)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, replayed.Outcome)
	require.NotNil(t, replayed.Entry)
	require.Equal(t, lost.Reference, replayed.Reference)
	require.Equal(t, origin.task, replayed.Entry.Task)
	require.Equal(t, roots.history.reservation.Owner, replayed.Entry.Owner)
	require.Equal(t, roots.history.reservation.Quota, replayed.Entry.Quota)
	roots.entry = replayed.Entry
	detachedPhysicalQuotaRefundMeter(t, roots, "public-unknown-recovered")

	roots.replayRecovery, err = b.newColdQuotaWorkerRecovery(origin.owner, roots.history, origin.session, origin.partition, origin.ranges, claims)
	require.NoError(t, err)
	b.archiveLife.mu.Lock()
	claimBucket := claims.bucket
	b.archiveLife.mu.Unlock()
	for phase := 0; phase < 2; phase++ {
		detachedPhysicalQuotaHistoryRangeReady(t, f.ctx, b, origin.ranges)
		result, err := b.stepColdQuotaWorkerRecovery(origin.owner, roots.history, roots.replayRecovery, origin.session, origin.partition, origin.ranges, claims)
		require.NoError(t, err)
		if phase == 0 {
			require.Nil(t, result)
			require.Equal(t, coldQuotaRecoveryWantCommittedReceipt, roots.replayRecovery.phase)
		} else {
			require.NotNil(t, result)
			require.Equal(t, replayed.Entry, result.entry)
			require.Nil(t, result.previous)
			require.Nil(t, result.aborted)
			roots.replayResult = result
		}
		detachedPhysicalQuotaRefundMeter(t, roots, "public-unknown-paid-recovery")
	}
	b.archiveLife.mu.Lock()
	afterClaimBucket := claims.bucket
	b.archiveLife.mu.Unlock()
	require.Equal(t, claimBucket, afterClaimBucket, "committed recovery cannot move the Claim earning origin or spend credit")
	require.EqualValues(t, 1, submitted.Load())
	require.EqualValues(t, 1, leases.grants.Load())
	require.EqualValues(t, 1, leases.revokes.Load())
	require.EqualValues(t, 1, returnedRevokes.Load())
	require.Equal(t, immutable, detachedPhysicalQuotaHistoryPoints(t, f, []string{returnKey, journalKey}))
	require.Equal(t, receipt, detachedPhysicalQuotaHistoryPoints(t, f, []string{receiptKey}))
	require.Equal(t, free, detachedPhysicalQuotaHistoryPoints(t, f, []string{slotKey, freeKey}))
	for i, key := range sourceKeys {
		if key == slotKey || key == freeKey || key == returnKey || key == journalKey {
			continue
		}
		require.Equal(t, before[i], detachedPhysicalQuotaHistoryPoints(t, f, []string{key})[0], "authenticated source must survive refund recovery: %s", key)
	}
	require.Equal(t, origin, roots.cursor.origin)
	t.Log("actual detached public refund ACK lost after Commit; same-reference public and paid recovery; original guard Revoke joined; one public Grant/Commit API submission; transport dispatch/retry counts unmeasured")
}
