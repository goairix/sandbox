//go:build linux

package etcd

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func detachedTerminationQueryHistory(t *testing.T, h *detachedTerminationProviderFixture) []*mvccpb.KeyValue {
	t.Helper()
	f := h.f
	intent, provider, _, err := f.b.namespace.localRuntimeReleaseKeys(f.ref)
	require.NoError(t, err)
	fact, err := f.b.namespace.taskDetachedNoEffectKey(f.ref)
	require.NoError(t, err)
	none, err := f.b.namespace.taskFinalSyncAttemptKey(f.ref)
	require.NoError(t, err)
	points, err := f.b.readEnrolledPoints(f.ctx, []string{intent, provider, fact, none}, []int{16384, 16384, p.MaxDetachedNoEffectSettlementBytes, 8192})
	require.NoError(t, err)
	for _, point := range points {
		require.NotNil(t, point)
	}
	return points
}

func waitDetachedTerminationPeerReturned(t *testing.T, h *detachedTerminationProviderFixture) {
	t.Helper()
	select {
	case <-h.peerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("original component peer did not close its actual TLS connection")
	}
}

// This follows the actual metadata recovery gate, not managed admission or a
// physical PID1/cgroup recovery. The first real production call owns its native
// provider writes; only after that original owner and TLS peer return may this
// original job join Run and consume the accepted actor's new-Run CAS.
func TestDetachedRuntimeTerminationNativeJoinedNewClaimRetainedQuery(t *testing.T) {
	h := newDetachedTerminationProviderFixture(t)
	f := h.f
	commitDetachedTerminationFixtureFact(t, h)
	oldClaim, oldRun, oldEmpty := f.job.claim, f.job.runID, f.drained.emptyRevision
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, oldClaim))
	stale, err := f.b.loadDetachedRuntimeAuthority(f.ctx, f.job, oldClaim)
	require.NoError(t, err)
	result, err := f.b.terminateTaskRuntimeDetached(f.ctx, f.job, oldClaim, h.destination, nil)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	waitDetachedTerminationPeerReturned(t, h)
	oldClaim.mu.Lock()
	owner := oldClaim.runtimeReleaseAttempt
	oldClaim.mu.Unlock()
	require.NotNil(t, owner)
	select {
	case <-owner.done:
	default:
		t.Fatal("original runtime-release owner returned without actually joining")
	}
	require.Nil(t, f.job.owner)
	before := detachedTerminationQueryHistory(t, h)
	intent, err := decodeLocalRuntimeTermination(before[0])
	require.NoError(t, err)
	require.Equal(t, "reserved", intent.Phase)
	query, err := localRuntimeTerminationQuery(intent, before[0])
	require.NoError(t, err)
	reservation := ownTaskFence(before[0])
	fact, err := p.DecodeDetachedNoEffectSettlement(before[2].Value)
	require.NoError(t, err)
	require.Equal(t, oldRun, fact.RunID)
	require.Equal(t, detachedNoEffectClaim(oldClaim.reference), fact.WriterClaim)

	// All calls of this original physical continuation have now returned.
	// Lease release is not used as a join substitute: the original join writer
	// must still advance native E, then the accepted gate owns exactly one CAS.
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, oldClaim))
	f.job.claim = nil
	f.job.effectsJoined = true
	require.NoError(t, f.b.joinCleanupRun(f.ctx, f.job))
	joined := f.job.state.execution
	require.True(t, f.job.state.record.Run.Joined)
	gate := prepareCleanupDetachedRecoveryForTest(t, f)
	require.Same(t, gate, f.job.detachedRecovery)
	original := f.b.client.KV
	var runWrites atomic.Int64
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if putsKey(joined.key)(yes) {
			runWrites.Add(1)
			require.Equal(t, 51, len(cmps)+len(yes)+len(no))
			require.Contains(t, cmps, clientv3.Compare(clientv3.ModRevision(string(before[2].Key)), "=", before[2].ModRevision))
		}
		return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	_, err = f.worker.cleanupContinuation(f.job, &cleanupLocalContinuation{kind: "resume-detached-noeffect"})
	f.b.client.KV = original
	require.NoError(t, err)
	require.EqualValues(t, 1, runWrites.Load())
	require.NotEqual(t, oldRun, f.job.runID)
	require.Equal(t, f.job.runID, f.job.state.record.Run.ID)
	require.False(t, f.job.state.record.Run.Joined)
	require.Greater(t, f.job.state.execution.mod, joined.mod)
	require.Nil(t, f.job.detachedRecovery)
	require.Nil(t, f.job.detachedNoEffect)
	require.False(t, f.job.effectsJoined)
	queriesBeforeDrain := f.queryCount.Load()
	f.job.claim, err = f.b.ClaimTask(f.job.ctx, f.ref, "retained-query-component", 30*time.Second)
	require.NoError(t, err)
	current := f.job.claim
	require.NotEqual(t, oldClaim.reference, current.reference)
	require.Greater(t, current.reference.CreateRevision, oldClaim.reference.CreateRevision)
	f.drained, err = f.b.PrepareAfterDualDrain(f.ctx, current, f.destination)
	require.NoError(t, err)
	require.Greater(t, f.queryCount.Load(), queriesBeforeDrain)
	require.Greater(t, f.drained.emptyRevision, oldEmpty)
	require.GreaterOrEqual(t, f.drained.emptyRevision, current.reference.CreateRevision)
	require.NoError(t, f.job.progress("drained", f.job.pending("final-sync", StageReference{}), nil))
	require.Equal(t, before, detachedTerminationQueryHistory(t, h), "new Run/Claim/drain cannot rewrite old fact, none or reservation/provider")

	fresh, err := f.b.loadDetachedRuntimeAuthority(f.ctx, f.job, current)
	require.NoError(t, err)
	require.True(t, fresh.matchesTermination(intent))
	foreign := intent
	foreign.Version, foreign.DetachedEffects = 1, nil
	require.False(t, fresh.matchesTermination(foreign))
	foreign = intent
	bindings := *intent.DetachedEffects
	foreign.DetachedEffects = &bindings
	foreign.DetachedEffects.Settlement.ModRevision++
	require.False(t, fresh.matchesTermination(foreign))
	foreign = intent
	bindings = *intent.DetachedEffects
	foreign.DetachedEffects = &bindings
	foreign.DetachedEffects.ContractDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if foreign.DetachedEffects.ContractDigest == intent.DetachedEffects.ContractDigest {
		foreign.DetachedEffects.ContractDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	}
	require.False(t, fresh.matchesTermination(foreign))
	foreign = intent
	foreign.Current.ClaimCreateRevision = current.reference.CreateRevision + 1
	require.False(t, fresh.matchesTermination(foreign))
	require.NotEqual(t, fact.WriterClaim, detachedNoEffectClaim(current.reference))
	current.mu.Lock()
	claimComparisons := current.comparisons()
	current.mu.Unlock()
	var commits, puts, guards atomic.Int64
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		commits.Add(1)
		for _, branch := range [][]clientv3.Op{yes, no} {
			for _, operation := range branch {
				if operation.IsPut() {
					puts.Add(1)
				}
			}
		}
		guard := len(cmps) == 54 && len(yes) == 1 && len(no) == 0 && yes[0].IsGet() && string(yes[0].KeyBytes()) == f.b.identityKey
		requestBytes := 0
		if guard {
			guards.Add(1)
			require.Subset(t, cmps, claimComparisons, "each live guard must retain the genuine NEW Claim/guard Lease comparisons")
			request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints([]string{f.b.identityKey})}
			requestBytes = request.Size()
			require.Equal(t, 55, len(request.Compare)+len(request.Success)+len(request.Failure))
			require.LessOrEqual(t, requestBytes, 128<<10)
		}
		response, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		if guard && response != nil {
			replyBytes := (*pb.TxnResponse)(response).Size()
			require.LessOrEqual(t, replyBytes, 256<<10)
			t.Logf("actual retained Query current-Claim guard nodes=55 request_bytes=%d reply_bytes=%d", requestBytes, replyBytes)
		}
		return response, err
	}}
	require.ErrorIs(t, stale.guard(f.ctx, stale.base()), ErrInvalidRecord)
	require.Zero(t, commits.Load(), "old authority must refuse locally before a new native dispatch")
	h.allowQuery.Store(true)
	result, err = f.b.terminateTaskRuntimeDetached(f.ctx, f.job, current, h.destination, &reservation)
	operationCommits := commits.Load()
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	waitDetachedTerminationPeerReturned(t, h)
	require.EqualValues(t, 2, guards.Load(), "actual loader and provider-preparation guards both use the fresh Claim")
	require.Zero(t, puts.Load(), "retained Query allocates no candidate, provider, fact or none Put")
	after := detachedTerminationQueryHistory(t, h)
	require.Equal(t, before, after)
	retained, err := decodeLocalRuntimeTermination(after[0])
	require.NoError(t, err)
	require.Equal(t, intent.Current, retained.Current)
	retainedQuery, err := localRuntimeTerminationQuery(retained, after[0])
	require.NoError(t, err)
	require.Equal(t, query, retainedQuery)
	require.EqualValues(t, 1, h.issuer.certificates.Load())
	require.EqualValues(t, 1, h.issuer.signatures.Load())
	require.EqualValues(t, 1, h.terminates.Load())
	require.EqualValues(t, 1, h.queries.Load())
	h.mu.Lock()
	deletes, patches := h.deletes, h.patches
	h.mu.Unlock()
	require.Equal(t, 1, deletes)
	require.Zero(t, patches)
	t.Logf("actual oldRun=%s newRun=%s oldClaim=%s newClaim=%s actor_Run_Puts=%d operation_Puts=%d Commit_submissions=%d; transport retry dispatch unmeasured", oldRun, f.job.runID, oldClaim.reference.ClaimID, current.reference.ClaimID, runWrites.Load(), puts.Load(), operationCommits)
	t.Log("actual retained observer Query=1; total Terminate=1 Sign=1 Pod_DELETE=1, no terminal proof/PID1/cgroup/slots2..6 claim")
}
