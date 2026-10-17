//go:build linux

package etcd

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This observes the actual native request, never supplies a successful reply.
// Response-loss tests retain the server's real result and change only its ACK.
func captureCleanupDetachedNoEffectWrite(t *testing.T, f *cleanupDetachedNoEffectNativeFixture, draft *cleanupDetachedNoEffectDraft, after func(*clientv3.TxnResponse, error) (*clientv3.TxnResponse, error)) *int {
	t.Helper()
	original := f.b.client.KV
	writes := new(int)
	key, err := f.b.namespace.taskDetachedNoEffectKey(f.ref)
	require.NoError(t, err)
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		writing := putsKey(key)(yes)
		if writing {
			*writes++
			require.True(t, draft.writeAttempted, "the original latch is irreversible before native send")
			require.Equal(t, 64, len(cmps)+len(yes)+len(no))
			require.Len(t, cmps, 60)
			require.Len(t, yes, 1)
			require.Len(t, no, 3)
			require.Equal(t, []byte(draft.value), yes[0].ValueBytes())
			require.Equal(t, taskEmptyComparisons(f.b, f.job.claim, f.drained.pin.intent, f.drained.pin.receipt, f.drained.pin.reservation), cmps[:53])
			projection, err := f.b.detachedNoEffectWriteProjection(cmps, key, draft.value)
			require.NoError(t, err)
			require.LessOrEqual(t, projection.request.Size(), 128<<10)
			require.LessOrEqual(t, projection.failure.Size(), 256<<10)
		}
		response, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		if writing && after != nil {
			return after(response, err)
		}
		return response, err
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	return writes
}

func TestCleanupDetachedNoEffectNativeOriginalWriter(t *testing.T) {
	f := newCleanupDetachedNoEffectNativeFixture(t)
	queries := f.queryCount.Load()
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	require.NotNil(t, draft)
	require.Greater(t, f.queryCount.Load(), queries, "prepare must revalidate the original dual-drain through real target Query")
	require.Same(t, draft, f.job.detachedNoEffect)
	pending := f.job.state.record.Pending
	require.NotNil(t, pending)
	require.Equal(t, "detached-noeffect", pending.Purpose)
	require.Equal(t, draft.record.AttemptID, pending.AttemptID)
	require.Equal(t, draft.record.BindingDigest, pending.Digest)
	require.Equal(t, f.job.claim.reference.ClaimID, pending.ClaimID)
	require.Equal(t, f.job.claim.reference.LeaseID, pending.LeaseID)
	require.Empty(t, pending.StageID)
	require.Empty(t, pending.RequestID)
	require.Equal(t, f.drained.emptyRevision, draft.record.EmptyRevision)
	require.Equal(t, []byte(f.job.state.execution.value), draft.record.ExecutionValue)
	require.Equal(t, f.job.state.execution.mod, draft.record.Execution.ModRevision)
	require.False(t, f.job.state.record.Run.Joined)
	require.Nil(t, f.job.state.record.Archive)
	writes := captureCleanupDetachedNoEffectWrite(t, f, draft, nil)

	// Equal copied fields cannot acquire authority from an original pointer.
	copyDraft := *draft
	_, err = f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, &copyDraft)
	require.Error(t, err)
	copyJob := *f.job
	_, err = f.b.commitCleanupDetachedNoEffect(f.ctx, &copyJob, draft)
	require.Error(t, err)
	require.Zero(t, *writes)

	result, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.NotNil(t, result.Entry)
	require.Equal(t, 1, *writes)
	key, err := f.b.namespace.taskDetachedNoEffectKey(f.ref)
	require.NoError(t, err)
	point, err := f.raw.Get(f.ctx, key)
	require.NoError(t, err)
	require.Len(t, point.Kvs, 1)
	kv := point.Kvs[0]
	require.Zero(t, kv.Lease)
	require.EqualValues(t, 1, kv.Version)
	require.Equal(t, kv.CreateRevision, kv.ModRevision)
	require.Greater(t, kv.CreateRevision, draft.record.Execution.ModRevision)
	require.GreaterOrEqual(t, kv.CreateRevision, f.drained.emptyRevision)
	decoded, err := p.DecodeDetachedNoEffectSettlement(kv.Value)
	require.NoError(t, err)
	require.Equal(t, draft.record, decoded)
	require.Equal(t, f.job.runID, decoded.RunID)
	require.Equal(t, f.job.state.record.Incarnation, decoded.Incarnation)
	loaded, err := f.b.loadTaskDetachedNoEffect(f.ctx, f.ref)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Equal(t, decoded, loaded.record)

	again, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, again.Outcome)
	require.Equal(t, 1, *writes, "a committed original attempt is read-only on repetition")
}

func TestCleanupDetachedNoEffectNativeLostACKRetainsAttempt(t *testing.T) {
	f := newCleanupDetachedNoEffectNativeFixture(t)
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	writes := captureCleanupDetachedNoEffectWrite(t, f, draft, func(response *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		require.NoError(t, err)
		require.True(t, response.Succeeded)
		return nil, context.DeadlineExceeded
	})
	before := *f.job.state.record.Pending
	result, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.True(t, draft.writeAttempted)
	require.Equal(t, before, *f.job.state.record.Pending)
	require.Equal(t, 1, *writes)

	// A dead Claim cannot promote a diagnostic committed fact into live
	// permission, and cannot erase the exact server-committed original record.
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.job.claim))
	result, err = f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.Error(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.NotNil(t, result.Entry)
	require.Equal(t, 1, *writes)
	f.job.claim = nil
	loaded, err := f.b.loadTaskDetachedNoEffect(f.ctx, f.ref)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	require.Equal(t, draft.record, loaded.record)
}

func TestCleanupDetachedNoEffectNativeAbsenceStaysUnknownUntilJoin(t *testing.T) {
	f := newCleanupDetachedNoEffectNativeFixture(t)
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	key, err := f.b.namespace.taskDetachedNoEffectKey(f.ref)
	require.NoError(t, err)
	original := f.b.client.KV
	writes := 0
	var delayed []clientv3.Cmp
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if putsKey(key)(yes) {
			writes++
			delayed = append([]clientv3.Cmp(nil), cmps...)
			require.True(t, draft.writeAttempted)
			// No reply proves whether the network submitted this request. The
			// original attempt must remain query-only even when KV is absent.
			return nil, context.DeadlineExceeded
		}
		return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	for i := 0; i < 2; i++ {
		result, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
		require.ErrorIs(t, err, ErrOutcomeUnknown)
		require.Equal(t, OutcomeUnknown, result.Outcome)
	}
	require.Equal(t, 1, writes)
	oldMod := f.job.state.execution.mod
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.job.claim))
	f.job.claim = nil
	f.job.effectsJoined = true
	require.NoError(t, f.b.joinCleanupRun(f.ctx, f.job))
	require.True(t, f.job.state.record.Run.Joined)
	require.Greater(t, f.job.state.execution.mod, oldMod)

	// The original delayed transaction is now fenced. A second raw probe
	// isolates E.mod from the separately dead Claim, so join's causal fence is
	// actually tested rather than accidentally relying only on lease expiry.
	late, err := original.Txn(f.ctx).If(delayed...).Then(clientv3.OpPut(key, draft.value)).Commit()
	require.NoError(t, err)
	require.False(t, late.Succeeded)
	joinFence, err := original.Txn(f.ctx).If(clientv3.Compare(clientv3.ModRevision(f.job.state.execution.key), "=", oldMod)).Then(clientv3.OpPut(key, draft.value)).Commit()
	require.NoError(t, err)
	require.False(t, joinFence.Succeeded)
	point, err := f.raw.Get(f.ctx, key)
	require.NoError(t, err)
	require.Empty(t, point.Kvs)
	require.Equal(t, 1, writes)
}

// A schema-valid Pending is not permission to replace that work. Each case
// persists its incompatible coordinates through the real actor, then checks
// that preparation fails before reserving none or changing E/attempt identity.
func TestCleanupDetachedNoEffectNativeRejectsIncompatiblePending(t *testing.T) {
	for _, purpose := range []string{"archive", "termination", "foreign-final-sync", "foreign-detached-noeffect"} {
		t.Run(purpose, func(t *testing.T) {
			f := newCleanupDetachedNoEffectNativeFixture(t)
			pending := f.job.pending(purpose, StageReference{})
			if purpose == "foreign-final-sync" || purpose == "foreign-detached-noeffect" {
				pending.Purpose = strings.TrimPrefix(purpose, "foreign-")
				pending.AttemptID, pending.Digest = uuid.NewString(), strings.Repeat("a", 64)
				if pending.Purpose == "final-sync" {
					pending.StageID, pending.RequestID = "foreign-stage", uuid.NewString()
				}
			}
			require.True(t, pending.valid(), "negative phase must independently satisfy its persisted schema")
			require.NoError(t, f.job.progress("drained", pending, nil))
			prior := f.job.state.execution
			noneKey, err := f.b.namespace.taskFinalSyncAttemptKey(f.ref)
			require.NoError(t, err)
			settlementKey, err := f.b.namespace.taskDetachedNoEffectKey(f.ref)
			require.NoError(t, err)
			var reserved atomic.Int64
			original := f.b.client.KV
			f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
				if putsKey(noneKey)(yes) || putsKey(settlementKey)(yes) {
					reserved.Add(1)
				}
				return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
			}}
			t.Cleanup(func() { f.b.client.KV = original })

			draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
			assert.ErrorIs(t, err, ErrConflict)
			assert.Nil(t, draft)
			assert.Nil(t, f.job.detachedNoEffect)
			assert.Equal(t, prior, f.job.state.execution)
			assert.Equal(t, *pending, *f.job.state.record.Pending)
			assert.Zero(t, reserved.Load(), "incompatible work must be rejected before any new reservation")
			for _, key := range []string{noneKey, settlementKey} {
				point, err := f.raw.Get(f.ctx, key)
				require.NoError(t, err)
				assert.Empty(t, point.Kvs)
			}
			actual, err := f.raw.Get(f.ctx, prior.key)
			require.NoError(t, err)
			require.Len(t, actual.Kvs, 1)
			assert.Equal(t, prior.mod, actual.Kvs[0].ModRevision)
			assert.Equal(t, prior.value, string(actual.Kvs[0].Value))
		})
	}
}

// Runtime release already takes lifecycle before Claim. This deterministic
// schedule holds lifecycle while a second goroutine owns the genuine Claim:
// the Claim-locked liveness check must not reacquire lifecycle in reverse.
// Even the failing implementation is explicitly unblocked and joined.
func TestCleanupDetachedNoEffectNativeClaimLockedLivenessAvoidsLifecycleLock(t *testing.T) {
	f := newCleanupDetachedNoEffectNativeFixture(t)
	require.NoError(t, f.b.validateCleanupDetachedNoEffectJob(f.job))
	c := f.job.claim
	entered, returned := make(chan struct{}), make(chan error, 1)
	var completed bool
	var result error
	func() {
		f.b.archiveLife.mu.Lock()
		defer f.b.archiveLife.mu.Unlock()
		go func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			close(entered)
			returned <- f.b.detachedNoEffectLive(f.ctx, f.job, c, f.drained)
		}()
		workerTestReceive(t, entered)
		select {
		case result = <-returned:
			completed = true
		case <-time.After(250 * time.Millisecond):
		}
	}()
	if !completed {
		result = workerTestReceive(t, returned)
	}
	require.NoError(t, result, "all original native/Claim/Run prerequisites remain valid")
	require.True(t, completed, "Claim-locked liveness attempted the reverse Claim -> lifecycle lock order")
}

// A different actual value is a conflict with this original uncertain attempt,
// even when the foreign value is malformed. It must never be classified as our
// committed result or cause a second write, and cannot be hidden by decoding it
// before checking exact attempt bytes.
func TestCleanupDetachedNoEffectNativeUnknownForeignRawConflicts(t *testing.T) {
	f := newCleanupDetachedNoEffectNativeFixture(t)
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	key, err := f.b.namespace.taskDetachedNoEffectKey(f.ref)
	require.NoError(t, err)
	original := f.b.client.KV
	writes := 0
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if putsKey(key)(yes) {
			writes++
			return nil, context.DeadlineExceeded
		}
		return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	first, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, first.Outcome)

	// This deliberate negative fixture is an actual foreign KV write, never
	// a fabricated successful settlement or permission for another writer.
	_, err = f.raw.Put(f.ctx, key, "foreign malformed settlement")
	require.NoError(t, err)
	result, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	assert.ErrorIs(t, err, ErrConflict)
	assert.Equal(t, OutcomeUnknown, result.Outcome)
	assert.Nil(t, result.Entry)
	assert.True(t, draft.lost)
	assert.Equal(t, 1, writes, "foreign history cannot reopen the original write opportunity")
}
