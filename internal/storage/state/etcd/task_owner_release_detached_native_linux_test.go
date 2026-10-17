//go:build linux && (amd64 || arm64)

package etcd

import (
	"bytes"
	"context"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Inspect the actual client operations before dispatch, including both sides
// of any nested read-only transaction. No particular release-write shape can
// hide another Put/Delete from this diagnosis-only gate.
func assertDetachedOwnerReadOnly(t *testing.T, ops []clientv3.Op, forbidden [][]byte, counts []*atomic.Int64) {
	t.Helper()
	for _, op := range ops {
		if op.IsTxn() {
			_, yes, no := op.Txn()
			assertDetachedOwnerReadOnly(t, yes, forbidden, counts)
			assertDetachedOwnerReadOnly(t, no, forbidden, counts)
			continue
		}
		require.True(t, op.IsGet(), "diagnosis permits only Get or read-only nested transactions")
		for i, key := range forbidden {
			if bytes.Equal(op.KeyBytes(), key) {
				counts[i].Add(1)
			}
		}
	}
}

// Root's original physical parent owns bootstrap, the protected runtime,
// observer and provider. This optional child branch runs only after its genuine
// terminal proof and seven complete slots. It signs no terminal material and
// claims neither managed consumer admission nor quota/global budget integration.
func detachedPhysicalOwnerRelease(t *testing.T, f *cleanupDetachedNoEffectNativeFixture) {
	t.Helper()
	b, claim := f.b, f.job.claim
	require.NotNil(t, claim)
	terminationKey, providerKey, releaseKey, err := b.namespace.localRuntimeReleaseKeys(f.ref)
	require.NoError(t, err)
	taskKey, err := b.namespace.taskKey(f.ref)
	require.NoError(t, err)
	head, err := b.readEnrolledPoints(f.ctx, []string{terminationKey, providerKey, releaseKey, taskKey}, []int{16384, 16384, 8192, 4096})
	require.NoError(t, err)
	require.Nil(t, head[2])
	require.NotNil(t, head[3])
	intent, err := decodeLocalRuntimeTermination(head[0])
	require.NoError(t, err)
	require.Equal(t, "cleanup-complete", intent.Phase)
	require.NoError(t, b.verifyLocalRuntimeProof(intent, head[0], f.publication.Enrollment.ObserverCertificate))
	provider, err := decodeTaskRuntimeProviderCleanup(head[1])
	require.NoError(t, err)
	require.True(t, localRuntimeProviderComplete(provider))
	require.Equal(t, workspaceSyncIntentDigest(string(head[1].Value)), intent.CleanupDigest)

	// Independent test locators use the accepted key codecs, not the new
	// dispatcher. Thus scaffold RED must be reached at the release product
	// assertion, after these real producer/history preconditions have passed.
	historyKeys := detachedOwnerHistoryModelKeys(t, b, intent, head[3])
	historyCaps := []int{4096, 32768, 4096, 8192, 8192, 8192, 32768, 65536, 4096, 8192, 16384, 8192, 8192}
	historyBefore, err := b.readEnrolledPoints(f.ctx, historyKeys, historyCaps)
	require.NoError(t, err)
	require.Len(t, historyBefore, 13)
	for _, point := range historyBefore {
		require.NotNil(t, point)
	}
	expected, err := newDetachedOwnerReleaseRecord(intent, head[0].CreateRevision, head[0].ModRevision, localRuntimeClaimWire(claim))
	require.NoError(t, err)
	expectedWire, err := encodeDetachedOwnerRelease(expected)
	require.NoError(t, err)
	deleted := [3]string{expected.Deleted[0].Key, expected.Deleted[1].Key, expected.Deleted[2].Key}
	ownedBefore, err := b.readEnrolledPoints(f.ctx, deleted[:], []int{4096, 4096, 4096})
	require.NoError(t, err)
	for _, point := range ownedBefore {
		require.NotNil(t, point)
	}

	original := b.client.KV
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	defer func() {
		b.client.KV = original
		b.client.Lease = lease.Lease
	}()
	issuer := b.taskRuntimeTerminationIssuer.(*detachedTerminationFixtureIssuer)
	certificates, signatures := issuer.certificates.Load(), issuer.signatures.Load()
	var submissions, deletedIndexReads, currentExecutionReads, currentSlotReads atomic.Int64
	forbidden := [][]byte{[]byte(deleted[2]), []byte(f.job.state.execution.key), []byte(f.job.state.slot.key)}
	counts := []*atomic.Int64{&deletedIndexReads, &currentExecutionReads, &currentSlotReads}
	var readonly atomic.Bool
	b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if readonly.Load() {
			assertDetachedOwnerReadOnly(t, yes, forbidden, counts)
			assertDetachedOwnerReadOnly(t, no, forbidden, counts)
		}
		if len(yes) == 4 && yes[3].IsPut() && string(yes[3].KeyBytes()) == releaseKey {
			submissions.Add(1)
			require.False(t, readonly.Load(), "same-Claim diagnosis must never resend the release")
			require.Len(t, cmps, 59)
			require.Empty(t, no)
			for i, key := range deleted {
				require.True(t, yes[i].IsDelete())
				require.Equal(t, []byte(key), yes[i].KeyBytes())
				require.Empty(t, yes[i].RangeBytes())
				require.True(t, reflect.DeepEqual(clientv3.OpDelete(key), yes[i]), "actual delete must have no range/PrevKV/private options")
			}
			require.Equal(t, []byte(expectedWire), yes[3].ValueBytes())
			require.True(t, reflect.DeepEqual(clientv3.OpPut(releaseKey, expectedWire), yes[3]), "actual Put must retain exact value, lease0 and no PrevKV/private options")
			writes := []Write{{Key: deleted[0], Delete: true}, {Key: deleted[1], Delete: true}, {Key: deleted[2], Delete: true}, {Key: releaseKey, Value: yes[3].ValueBytes()}}
			request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites(writes)}
			require.Equal(t, 63, len(request.Compare)+len(request.Success)+len(request.Failure))
			require.LessOrEqual(t, request.Size(), 128<<10)
			response, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
			require.NoError(t, err)
			require.True(t, response.Succeeded, "drop only the ACK of a real native commit")
			require.LessOrEqual(t, (*pb.TxnResponse)(response).Size(), 256<<10)
			t.Logf("actual release nodes63 request=%d reply=%d; real committed ACK intentionally lost", request.Size(), (*pb.TxnResponse)(response).Size())
			return nil, context.DeadlineExceeded
		}
		return original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
	}}

	result, err := b.releaseTaskWorkspaceOwnerDetached(f.ctx, f.job, claim)
	require.ErrorIs(t, err, ErrOutcomeUnknown, "real proof and seven complete slots must reach the release writer before ACK loss")
	require.Equal(t, OutcomeUnknown, result.Outcome)
	require.Nil(t, result.Entry)
	require.EqualValues(t, 1, submissions.Load())
	claim.mu.Lock()
	attempt := claim.ownerReleaseAttempt
	candidate := ""
	if attempt != nil {
		candidate = attempt.detachedCandidate
	}
	claim.mu.Unlock()
	require.NotNil(t, attempt)
	require.Equal(t, expectedWire, candidate)
	select {
	case <-attempt.done:
	default:
		t.Fatal("original release owner has not actually joined")
	}
	b.archiveLife.mu.Lock()
	retainedContext, retainedCancel := attempt.ctx != nil, attempt.cancel != nil
	b.archiveLife.mu.Unlock()
	require.False(t, retainedContext)
	require.False(t, retainedCancel)

	// The three keys are really gone, not represented by copied deletion DTOs.
	after, err := b.readEnrolledPoints(f.ctx, []string{deleted[0], deleted[1], deleted[2], releaseKey}, []int{4096, 4096, 4096, 8192})
	require.NoError(t, err)
	for _, point := range after[:3] {
		require.Nil(t, point)
	}
	require.NotNil(t, after[3])
	require.Equal(t, []byte(expectedWire), after[3].Value)
	require.Zero(t, after[3].Lease)
	require.EqualValues(t, 1, after[3].Version)
	_, err = decodeLocalOwnerRelease(after[3])
	require.Error(t, err, "public v1 stays strict")

	// Lease loss grants nothing, but it cannot prohibit diagnosis of this
	// original attempted candidate. All following product reads must work
	// without runtimeIndex/current E/S or any provider/observer mutation.
	require.NoError(t, b.ReleaseTaskClaim(f.ctx, claim))
	readonly.Store(true)
	for i := 0; i < 2; i++ {
		repeated, repeatErr := b.releaseTaskWorkspaceOwnerDetached(f.ctx, f.job, claim)
		require.NoError(t, repeatErr)
		require.Equal(t, OutcomeCommitted, repeated.Outcome)
		require.NotNil(t, repeated.Entry)
		require.Equal(t, after[3].CreateRevision, repeated.Entry.CreateRevision)
	}
	for _, actualTask := range []*mvccpb.KeyValue{nil, head[3]} {
		readOwner, err := b.beginLocalRuntimeRead(f.ctx, f.ref)
		require.NoError(t, err)
		proof, err := b.readTaskOwnerReleaseProof(readOwner.ctx, f.ref, actualTask)
		readOwner.finish()
		require.NoError(t, err)
		require.NotNil(t, proof)
		require.Nil(t, proof.local)
		require.NotNil(t, proof.detached)
		require.Equal(t, after[3], proof.detached.kv)
		require.Equal(t, expected, proof.detached.record)
		require.Equal(t, intent.Current, proof.detached.termination.Current)
		require.NotNil(t, proof.detached.history)
		for i, point := range proof.detached.history.history {
			require.Equal(t, historyBefore[i], point)
		}
		paid, err := b.decodeTaskOwnerReleaseProof(f.ref, head[3], after[3], head[0], head[1], proof.detached.history.history[:])
		require.NoError(t, err)
		require.NotNil(t, paid.detached)
		require.Nil(t, paid.local)
		bad := append([]*mvccpb.KeyValue(nil), proof.detached.history.history[:]...)
		foreign := *bad[11]
		foreign.Value = append([]byte(" "), foreign.Value...)
		bad[11] = &foreign
		refused, err := b.decodeTaskOwnerReleaseProof(f.ref, head[3], after[3], head[0], head[1], bad)
		require.Error(t, err)
		require.Nil(t, refused, "a diagnostic copy is used only as a negative foreign-companion input")
	}
	readonly.Store(false)
	require.EqualValues(t, 1, submissions.Load(), "lost ACK never authorizes another native release transaction")
	require.Zero(t, deletedIndexReads.Load(), "surviving history must not reread or reconstruct the deleted runtime index")
	require.Zero(t, currentExecutionReads.Load(), "cold history cannot require a current execution")
	require.Zero(t, currentSlotReads.Load(), "cold history cannot require a current slot")
	require.Zero(t, lease.grants.Load())
	require.Equal(t, certificates, issuer.certificates.Load())
	require.Equal(t, signatures, issuer.signatures.Load())
	historyAfter, err := b.readEnrolledPoints(f.ctx, historyKeys, historyCaps)
	require.NoError(t, err)
	require.Equal(t, historyBefore, historyAfter)
	finalHead, err := b.readEnrolledPoints(f.ctx, []string{terminationKey, providerKey}, []int{16384, 16384})
	require.NoError(t, err)
	require.Equal(t, head[:2], finalHead)
	t.Log("actual proof+7 slots; one release CAS committed with lost ACK; dead original Claim readonly/cold13 survives deleted index; no managed/quota/global-admission claim")
}
