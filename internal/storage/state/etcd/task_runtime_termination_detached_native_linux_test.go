//go:build linux

package etcd

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"

	"github.com/goairix/sandbox/internal/kubecontract"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func commitDetachedTerminationFixtureFact(t *testing.T, h *detachedTerminationProviderFixture) {
	t.Helper()
	f := h.f
	draft, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	result, err := f.b.commitCleanupDetachedNoEffect(f.ctx, f.job, draft)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.NotNil(t, result.Entry)
}

func detachedTerminationFixturePoints(t *testing.T, h *detachedTerminationProviderFixture) []*mvccpb.KeyValue {
	t.Helper()
	f := h.f
	intent, provider, release, err := f.b.namespace.localRuntimeReleaseKeys(f.ref)
	require.NoError(t, err)
	points, err := f.b.readEnrolledPoints(f.ctx, []string{intent, provider, release}, []int{16384, 16384, 8192})
	require.NoError(t, err)
	return points
}

// This is the production metadata/provider state machine through an actual
// received TLS Pod DELETE and a real signed Terminate request. Unknown stops it
// before all physical-proof-dependent slots; the fixture emits no fake proof.
func TestDetachedRuntimeTerminationNativeUnknownNoResend(t *testing.T) {
	h := newDetachedTerminationProviderFixture(t)
	f := h.f
	commitDetachedTerminationFixtureFact(t, h)
	points := detachedTerminationFixturePoints(t, h)
	require.Nil(t, points[0])
	require.Nil(t, points[1], "only the production provider writer may create this key")
	require.Nil(t, points[2])
	intentKey, providerKey, _, err := f.b.namespace.localRuntimeReleaseKeys(f.ref)
	require.NoError(t, err)

	original := f.b.client.KV
	var commits, providerWrites, reservations atomic.Int64
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		commits.Add(1)
		reserve := len(yes) == 1 && yes[0].IsPut() && bytes.Equal(yes[0].KeyBytes(), []byte(intentKey))
		if len(yes) == 1 && yes[0].IsPut() && bytes.Equal(yes[0].KeyBytes(), []byte(providerKey)) {
			providerWrites.Add(1)
		}
		requestBytes := 0
		if reserve {
			reservations.Add(1)
			require.Len(t, cmps, 58)
			require.Empty(t, no)
			intent, err := p.DecodeTaskRuntimeTerminationIntent(yes[0].ValueBytes())
			require.NoError(t, err)
			require.EqualValues(t, 2, intent.Version)
			require.Equal(t, "reserved", intent.Phase)
			request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites([]Write{{Key: intentKey, Value: yes[0].ValueBytes()}})}
			require.Equal(t, 59, len(request.Compare)+len(request.Success)+len(request.Failure))
			requestBytes = request.Size()
			require.LessOrEqual(t, requestBytes, 128<<10)
		}
		response, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		if reserve && response != nil {
			replyBytes := (*pb.TxnResponse)(response).Size()
			require.LessOrEqual(t, replyBytes, 256<<10)
			t.Logf("actual production detached reservation compares=58 puts=1 nodes=59 request_bytes=%d reply_bytes=%d", requestBytes, replyBytes)
		}
		return response, err
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	result, err := f.b.terminateTaskRuntimeDetached(f.ctx, f.job, f.job.claim, h.destination, nil)
	require.ErrorIs(t, err, ErrOutcomeUnknown)
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.EqualValues(t, 1, reservations.Load())
	require.EqualValues(t, 5, providerWrites.Load(), "initial provider, then intent+complete for each of slots zero and one")
	require.EqualValues(t, 1, h.terminates.Load())
	require.Zero(t, h.queries.Load())
	require.EqualValues(t, 1, h.issuer.certificates.Load())
	require.EqualValues(t, 1, h.issuer.signatures.Load())
	points = detachedTerminationFixturePoints(t, h)
	require.NotNil(t, points[0])
	require.NotNil(t, points[1])
	require.Nil(t, points[2])
	intent, err := decodeLocalRuntimeTermination(points[0])
	require.NoError(t, err)
	require.EqualValues(t, 2, intent.Version)
	require.Equal(t, "reserved", intent.Phase)
	require.Empty(t, intent.TerminalProof)
	provider, err := decodeTaskRuntimeProviderCleanup(points[1])
	require.NoError(t, err)
	require.Equal(t, f.ref, provider.Task)
	require.Equal(t, intent.TaskBirth, provider.TaskBirth)
	require.True(t, localRuntimeProviderMatches(provider, intent, f.b.detachedCleanup.contractDigest))
	for i, slot := range provider.Slots {
		if i > 1 {
			require.Equal(t, "unstarted", slot.State, "Unknown cannot reach proof-dependent provider slots")
			continue
		}
		require.Equal(t, "complete", slot.State)
		require.Equal(t, intent.Current.Runtime.UID, slot.UID)
		require.Equal(t, "1", slot.ResourceVersion, "durable UID/RV identifies the original preflight")
		require.NotEmpty(t, slot.EffectID)
		require.NotNil(t, slot.Claim)
		require.Equal(t, localRuntimeClaimWire(f.job.claim), *slot.Claim)
	}
	h.mu.Lock()
	gets, deletes, patches := h.gets, h.deletes, h.patches
	pod := h.pod.DeepCopy()
	h.mu.Unlock()
	require.Equal(t, 1, deletes, "count actual TLS server DELETE receipts")
	require.Zero(t, patches, "the formal enrolled Pod was already retained")
	require.NotNil(t, pod.DeletionTimestamp)
	require.Equal(t, []string{kubecontract.EnrolledRetainFinalizer}, pod.Finalizers)
	before := []*mvccpb.KeyValue{points[0], points[1]}
	firstCommits := commits.Load()
	result, err = f.b.terminateTaskRuntimeDetached(f.ctx, f.job, f.job.claim, h.destination, nil)
	require.NoError(t, err, "same Claim returns read-only diagnosis without reopening effects")
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.NotNil(t, result.Entry)
	require.Equal(t, "reserved", result.Entry.Phase)
	points = detachedTerminationFixturePoints(t, h)
	require.Equal(t, before, points[:2], "same-Claim diagnosis preserves actual original provider and reservation MVCC")
	require.Nil(t, points[2])
	require.EqualValues(t, 1, reservations.Load())
	require.EqualValues(t, 5, providerWrites.Load())
	require.EqualValues(t, 1, h.terminates.Load())
	require.Zero(t, h.queries.Load())
	require.EqualValues(t, 1, h.issuer.certificates.Load())
	require.EqualValues(t, 1, h.issuer.signatures.Load())
	h.mu.Lock()
	afterGets, afterDeletes, afterPatches := h.gets, h.deletes, h.patches
	h.mu.Unlock()
	require.Equal(t, gets, afterGets, "same-Claim diagnosis must not reopen provider preparation")
	require.Equal(t, deletes, afterDeletes)
	require.Equal(t, patches, afterPatches)
	t.Logf("actual Commit submissions first_call_with_diagnostic_read=%d total_with_repeated_call_and_diagnostic_read=%d; transport dispatch/retries unmeasured", firstCommits, commits.Load())
	t.Logf("actual TLS receipts Pod_DELETE=%d finalizer_PATCH=%d Terminate=%d Query=%d; no physical terminal proof or slots2..6 claimed", deletes, patches, h.terminates.Load(), h.queries.Load())
}

func TestDetachedRuntimeTerminationNativeMissingFactAndCopiedJobNoEffects(t *testing.T) {
	for _, mode := range []string{"missing-fact", "copied-job"} {
		t.Run(mode, func(t *testing.T) {
			h := newDetachedTerminationProviderFixture(t)
			f := h.f
			job := f.job
			if mode == "missing-fact" {
				_, err := f.b.prepareCleanupDetachedNoEffect(f.ctx, f.job, f.drained, f.destination)
				require.NoError(t, err)
			} else {
				commitDetachedTerminationFixtureFact(t, h)
				copied := *f.job
				job = &copied
			}
			result, err := f.b.terminateTaskRuntimeDetached(f.ctx, job, f.job.claim, h.destination, nil)
			require.Error(t, err)
			require.Equal(t, OutcomeUnknown, result.Outcome)
			for _, point := range detachedTerminationFixturePoints(t, h) {
				require.Nil(t, point, "refused authority must precede all transition writes")
			}
			h.mu.Lock()
			gets, deletes, patches := h.gets, h.deletes, h.patches
			h.mu.Unlock()
			require.Zero(t, gets, "refused authority must precede provider preparation")
			require.Zero(t, deletes)
			require.Zero(t, patches)
			require.Zero(t, h.issuer.certificates.Load())
			require.Zero(t, h.issuer.signatures.Load())
			require.Zero(t, h.terminates.Load())
			require.Zero(t, h.queries.Load())
		})
	}
}
