//go:build linux

package etcd

import (
	"context"
	"sync/atomic"
	"testing"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This consumes Task3's accepted actual native writer and original signed
// acquisition/publication fixture. That fixture controls metadata scheduling;
// it proves neither managed inventory/admission nor PID1/cgroup termination.
// No source, Task, E/S, Claim, Run or settlement is fabricated for a positive.
func committedDetachedRuntimeFact(t *testing.T) (*cleanupDetachedNoEffectNativeFixture, *cleanupDetachedNoEffectEntry) {
	t.Helper()
	f := newCleanupDetachedNoEffectNativeFixture(t)
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	result, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.NotNil(t, result.Entry)
	return f, result.Entry
}

func TestDetachedRuntimeAuthorityNativeOriginalFact(t *testing.T) {
	f, fact := committedDetachedRuntimeFact(t)
	// Count actual Commit submissions from this loader onward, preserving all
	// real replies. This does not count transport interceptor dispatch/retries.
	original := f.b.client.KV
	var guards, commits atomic.Int64
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		commits.Add(1)
		guard := len(cmps) == 54 && len(yes) == 1 && len(no) == 0 && yes[0].IsGet() && string(yes[0].KeyBytes()) == f.b.identityKey
		requestBytes := 0
		if guard {
			guards.Add(1)
			require.Empty(t, yes[0].RangeBytes())
			request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints([]string{f.b.identityKey})}
			require.Equal(t, 55, len(request.Compare)+len(request.Success)+len(request.Failure))
			requestBytes = request.Size()
			require.LessOrEqual(t, requestBytes, 128<<10)
		}
		response, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		if guard && response != nil {
			replyBytes := (*pb.TxnResponse)(response).Size()
			require.LessOrEqual(t, replyBytes, 256<<10)
			t.Logf("actual native guard: compares=54 points=1 nodes=55 request_bytes=%d reply_bytes=%d", requestBytes, replyBytes)
		}
		return response, err
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	a, err := f.b.loadDetachedRuntimeAuthority(f.ctx, f.job, f.job.claim)
	require.NoError(t, err)
	require.NotNil(t, a)
	loaderCommits := commits.Load()
	t.Logf("actual native loader Commit submissions=%d; excludes fixture/writer and is not a transport-dispatch upper bound", loaderCommits)
	intent := a.newTermination()
	_, err = p.EncodeTaskRuntimeTerminationIntent(intent)
	require.NoError(t, err, "an actual no-effect writer must yield a complete canonical detached variant")
	require.EqualValues(t, 2, intent.Version)
	require.Zero(t, intent.MountAttempt)
	require.Equal(t, p.RecordStamp{}, intent.Authority.Archive)
	require.Equal(t, p.RecordStamp{}, intent.Authority.LocalComplete)
	require.NotNil(t, intent.DetachedEffects)
	require.Equal(t, fact.record.ContractDigest, intent.DetachedEffects.ContractDigest)
	require.Equal(t, p.RecordStamp{CreateRevision: fact.kv.CreateRevision, ModRevision: fact.kv.ModRevision, SHA256: workspaceSyncIntentDigest(string(fact.kv.Value))}, intent.DetachedEffects.Settlement)
	require.Equal(t, f.job.claim.reference.ClaimID, intent.Current.ClaimID)
	require.Equal(t, f.job.claim.reference.WorkerID, intent.Current.WorkerID)
	require.Equal(t, f.job.claim.reference.CreateRevision, intent.Current.ClaimCreateRevision)
	require.Equal(t, f.job.claim.reference.LeaseID, intent.Current.LeaseID)
	require.Equal(t, fact.record.QuiescenceContext.TaskDigest, intent.Current.TaskDigest)
	require.Equal(t, fact.record.QuiescenceContext.Runtime, intent.Current.Runtime)
	require.Equal(t, fact.record.QuiescenceStage, intent.Authority.QuiescenceStage)
	require.Equal(t, fact.record.QuiescenceReservation, intent.Authority.QuiescenceReservation)
	require.True(t, a.matchesTermination(intent))

	// Every tuple field participates in the variant comparison, including when
	// the common comparable authority stamps and original Current stay equal.
	changed := intent
	effects := *intent.DetachedEffects
	changed.DetachedEffects = &effects
	changed.DetachedEffects.Settlement.ModRevision++
	require.False(t, a.matchesTermination(changed))
	changed = intent
	effects = *intent.DetachedEffects
	changed.DetachedEffects = &effects
	changed.DetachedEffects.ContractDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if changed.DetachedEffects.ContractDigest == intent.DetachedEffects.ContractDigest {
		changed.DetachedEffects.ContractDigest = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	}
	require.False(t, a.matchesTermination(changed))
	changed = intent
	changed.Version, changed.DetachedEffects = 1, nil
	require.False(t, a.matchesTermination(changed), "detached origin cannot be reinterpreted through the local profile")

	// Both the loader's final guard and this later original guard must reach
	// native storage; neither synthetic projections nor candidate counts pass.
	require.NoError(t, a.guard(f.ctx, a.base()))
	require.EqualValues(t, 2, guards.Load())
	require.Equal(t, loaderCommits+1, commits.Load())
	t.Logf("actual loader plus repeated guard total Commit submissions=%d; transport dispatch/retry count remains unmeasured", commits.Load())
}

// A local sub-operation may be canceled after its initial live check without
// consuming the parent Claim. The existing native fixture supplies an actual
// Claim and its parent; no complete local/archive authority is fabricated.
func TestLocalRuntimeAuthorityDomainCanceledChildDoesNotLoseClaim(t *testing.T) {
	f := newCleanupDetachedNoEffectNativeFixture(t)
	b, c := f.b, f.job.claim
	parent := c.parentCtx
	a := &taskLocalRuntimeAuthority{backend: b, claim: c}
	require.NoError(t, a.local(parent))

	child, cancel := context.WithCancel(parent)
	cancel()
	task, fences, comparisons, err := b.loadRuntimeAuthorityDomain(child, c)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, task)
	require.Nil(t, fences)
	require.Nil(t, comparisons)
	c.mu.Lock()
	lost := c.lost
	c.mu.Unlock()
	require.False(t, lost, "canceling this read must preserve the original local parent Claim")
	require.NoError(t, a.local(parent))
}

// Detached construction deliberately owns a stricter current-Claim window.
// Keep its existing c.live cancellation policy separate from the shared local
// read's non-consuming policy; this is a negative gate before native reads.
func TestDetachedRuntimeAuthorityNativeCanceledChildLosesClaim(t *testing.T) {
	f := newCleanupDetachedNoEffectNativeFixture(t)
	child, cancel := context.WithCancel(f.ctx)
	cancel()
	a, err := f.b.loadDetachedRuntimeAuthority(child, f.job, f.job.claim)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, a)
	f.job.claim.mu.Lock()
	lost := f.job.claim.lost
	f.job.claim.mu.Unlock()
	require.True(t, lost, "detached entry must retain its original current-Claim cancellation policy")
}

func TestDetachedRuntimeAuthorityNativeMissingFactAndCopiedOwner(t *testing.T) {
	f := newCleanupDetachedNoEffectNativeFixture(t)
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	a, err := f.b.loadDetachedRuntimeAuthority(f.ctx, f.job, f.job.claim)
	require.Error(t, err, "the original none reservation still cannot stand in for a missing settlement")
	require.Nil(t, a)
	result, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)

	copyJob := *f.job
	a, err = f.b.loadDetachedRuntimeAuthority(f.ctx, &copyJob, f.job.claim)
	require.Error(t, err)
	require.Nil(t, a)
	// Copy only immutable attribution, never the mutex. The original self
	// pointer intentionally remains original, so this holder is not a Claim.
	originalClaim := f.job.claim
	copyClaim := &TaskClaim{self: originalClaim.self, origin: originalClaim.origin, parentCtx: originalClaim.parentCtx, reference: originalClaim.reference, birth: originalClaim.birth}
	a, err = f.b.loadDetachedRuntimeAuthority(f.ctx, f.job, copyClaim)
	require.Error(t, err)
	require.Nil(t, a)
	a, err = f.b.loadDetachedRuntimeAuthority(f.ctx, f.job, f.job.claim)
	require.NoError(t, err, "refusing copied owners must not consume the original capability")
	require.NotNil(t, a)
}

func TestDetachedRuntimeAuthorityNativeRejectsFactAndSourceABA(t *testing.T) {
	for _, source := range []string{"settlement", "creation-intent", "quiescence-reservation", "publication"} {
		t.Run(source, func(t *testing.T) {
			f, fact := committedDetachedRuntimeFact(t)
			a, err := f.b.loadDetachedRuntimeAuthority(f.ctx, f.job, f.job.claim)
			require.NoError(t, err)
			require.NoError(t, a.guard(f.ctx, a.base()))
			key := string(fact.kv.Key)
			switch source {
			case "creation-intent":
				key, err = f.b.namespace.intentKey(f.ref.Partition, fact.record.CreationIntentID)
			case "quiescence-reservation":
				key, err = f.b.namespace.taskQuiescenceAttemptKey(f.ref)
			case "publication":
				key, _, err = f.b.namespace.runtimePublicationKeys(f.ref.Partition, fact.record.CreationIntentID)
			}
			require.NoError(t, err)
			before, err := f.raw.Get(f.ctx, key)
			require.NoError(t, err)
			require.Len(t, before.Kvs, 1)
			// Fault injection begins only after genuine writer+loader+guard
			// success. Same-value Put preserves copied fields/digest while
			// changing the actual MVCC history, and cannot retain authority.
			_, err = f.raw.Put(f.ctx, key, string(before.Kvs[0].Value))
			require.NoError(t, err)
			require.Error(t, a.guard(f.ctx, a.base()))
			reloaded, err := f.b.loadDetachedRuntimeAuthority(f.ctx, f.job, f.job.claim)
			require.Error(t, err)
			require.Nil(t, reloaded)
		})
	}
}
