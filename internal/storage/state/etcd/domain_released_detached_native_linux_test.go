//go:build linux && (amd64 || arm64)

package etcd

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type detachedTerminalAPICallContextKey uint8

// Values identify only one synchronous public API invocation by its current
// nonzero-size *testing.T. Background DueWorker calls share the client but must
// remain unmodified and must never touch a finished subtest's counters or t.
const detachedTerminalAPICallKey detachedTerminalAPICallContextKey = 1

// Root/B installs the only optional hook after the original owner-release
// fixture's zero-history-index-read assertions. This helper consumes its real
// observer proof/seven slots/native release; it creates no source or proof.
// The host transcript is never promoted to a detached positive fixture.
func detachedPhysicalReleasedRead(t *testing.T, f *cleanupDetachedNoEffectNativeFixture) {
	t.Helper()
	require.Equal(t, "1", os.Getenv("SANDBOX_DETACHED_TERMINAL_PHYSICAL_NATIVE"))
	require.Equal(t, "1", os.Getenv("SANDBOX_DETACHED_OWNER_RELEASE_PHYSICAL_NATIVE"))
	b, claim := f.b, f.job.claim
	require.NotNil(t, claim)
	claim.mu.Lock()
	lost, attempt := claim.lost, claim.ownerReleaseAttempt
	claim.mu.Unlock()
	require.True(t, lost, "terminal observation must not borrow a live TaskClaim")
	require.NotNil(t, attempt)
	select {
	case <-attempt.done:
	default:
		t.Fatal("actual release owner has not returned and joined")
	}

	// B's accepted standalone loader supplies an independent real precondition
	// outside the public API's eight-RPC count. It also proves this invocation
	// starts after the actual seal and complete signed surviving history exist.
	reader, err := b.beginLocalRuntimeRead(f.ctx, f.ref)
	require.NoError(t, err)
	proof, err := b.readTaskOwnerReleaseProof(reader.ctx, f.ref, nil)
	reader.finish()
	require.NoError(t, err)
	require.NotNil(t, proof)
	require.Nil(t, proof.local)
	require.NotNil(t, proof.detached)
	actual := proof.detached
	require.NotNil(t, actual.history)
	require.Equal(t, f.ref, actual.record.Task)
	require.Equal(t, "cleanup-complete", actual.termination.Phase)
	index, err := b.namespace.runtimeIndexKey(actual.record.Runtime.UID)
	require.NoError(t, err)
	t.Log("actual detached physical proof, seven complete provider slots, native owner release and dead original Claim verified before terminal API")

	original := b.client.KV
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	issuer := b.taskRuntimeTerminationIssuer.(*detachedTerminationFixtureIssuer)
	certificates, signatures := issuer.certificates.Load(), issuer.signatures.Load()
	defer func() {
		b.client.KV = original
		b.client.Lease = lease.Lease
	}()
	var requests []*pb.TxnRequest
	var replyBytes []int
	pointsRead := make(map[string]int)
	expectedT := t
	apiCtx := context.WithValue(f.ctx, detachedTerminalAPICallKey, expectedT)
	b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, comparisons []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if ctx.Value(detachedTerminalAPICallKey) != expectedT {
			return original.Txn(ctx).If(comparisons...).Then(yes...).Else(no...).Commit()
		}
		branches := make([][]string, 2)
		for branch, ops := range [][]clientv3.Op{yes, no} {
			for _, op := range ops {
				require.True(t, op.IsGet(), "terminal observation sends no native write or nested transaction")
				key := string(op.KeyBytes())
				require.True(t, reflect.DeepEqual(clientv3.OpGet(key), op), "every point is a current linearizable Get without options")
				branches[branch] = append(branches[branch], key)
				pointsRead[key]++
			}
		}
		request := &pb.TxnRequest{
			Compare: taskProtoComparisons(comparisons),
			Success: taskProtoPoints(branches[0]), Failure: taskProtoPoints(branches[1]),
		}
		require.LessOrEqual(t, len(request.Compare)+len(request.Success)+len(request.Failure), 64)
		require.LessOrEqual(t, request.Size(), 128<<10)
		response, err := original.Txn(ctx).If(comparisons...).Then(yes...).Else(no...).Commit()
		require.NoError(t, err)
		require.NotNil(t, response)
		require.True(t, response.Succeeded)
		require.LessOrEqual(t, (*pb.TxnResponse)(response).Size(), 256<<10)
		requests = append(requests, request)
		replyBytes = append(replyBytes, (*pb.TxnResponse)(response).Size())
		return response, nil
	}}
	result, err := b.ReadSandboxReleased(apiCtx, f.ref.SandboxID)
	require.NoError(t, err, "real detached native release must reach the public terminal consumer")
	require.NotNil(t, result)
	require.Equal(t, f.ref, result.Task)
	require.Equal(t, f.ref.SandboxID, result.SandboxID)
	require.GreaterOrEqual(t, result.Revision, actual.kv.CreateRevision)
	require.Len(t, requests, 8, "the independent precondition reads are outside this count")
	for i, cardinality := range []int{1, 4, 1, 4, 1, 2, 13, 5} {
		require.Len(t, requests[i].Success, cardinality, "RPC %d", i+1)
	}
	total := 0
	for i, size := range replyBytes {
		total += size
		t.Logf("actual detached terminal RPC%d nodes=%d request=%d reply=%d", i+1, len(requests[i].Compare)+len(requests[i].Success)+len(requests[i].Failure), requests[i].Size(), size)
	}
	require.LessOrEqual(t, total, 512<<10)
	require.Equal(t, 1, pointsRead[index], "only final absence confirmation reads the deleted runtime index")
	require.Zero(t, pointsRead[f.job.state.execution.key])
	require.Zero(t, pointsRead[f.job.state.slot.key])
	require.Zero(t, lease.grants.Load())
	require.Equal(t, certificates, issuer.certificates.Load())
	require.Equal(t, signatures, issuer.signatures.Load())

	// The actual final transaction retains every complete native source,
	// including all four companions. Task repeats are deduplicated, giving
	// twenty-one pins without a successor owner; the finite maximum is22.
	mods := make(map[string]int64)
	for _, comparison := range requests[7].Compare {
		if comparison.Target == pb.Compare_MOD {
			mods[string(comparison.Key)] = comparison.GetModRevision()
		}
	}
	require.Len(t, mods, 21)
	retained := append([]*mvccpb.KeyValue{actual.kv, actual.history.termination, actual.history.provider}, actual.history.history[:]...)
	for _, point := range retained {
		require.Equal(t, point.ModRevision, mods[string(point.Key)], "no detached source/companion may escape final pinning")
	}

	// Stop counting before independent postreads, then compare the actual
	// native bytes/envelopes, not only projected receipt or release DTOs.
	b.client.KV = original
	historyKeys := make([]string, len(actual.history.history))
	for i, point := range actual.history.history {
		historyKeys[i] = string(point.Key)
	}
	after, err := b.readEnrolledPoints(f.ctx, historyKeys, detachedRuntimeHistoryCaps())
	require.NoError(t, err)
	require.Equal(t, actual.history.history[:], after)
	headKeys := []string{string(actual.kv.Key), string(actual.history.termination.Key), string(actual.history.provider.Key)}
	head, err := b.readEnrolledPoints(f.ctx, headKeys, []int{8192, 16384, 16384})
	require.NoError(t, err)
	require.Equal(t, retained[:3], head)
	t.Logf("actual detached terminal eight-RPC replies=%d; readonly21 final pins, no live Claim/issuer/provider/quota/admission authority", total)
	detachedPhysicalReleasedCompanionRefusals(t, f, actual)
}

// Inspect only the new negative invocations. Their independent counters must
// not change the accepted positive's exact eight-RPC/21-pin assertions above.
func detachedTerminalNegativeRequest(t *testing.T, comparisons []clientv3.Cmp, yes, no []clientv3.Op) *pb.TxnRequest {
	t.Helper()
	branches := make([][]string, 2)
	for branch, ops := range [][]clientv3.Op{yes, no} {
		for _, op := range ops {
			require.True(t, op.IsGet())
			key := string(op.KeyBytes())
			require.True(t, reflect.DeepEqual(clientv3.OpGet(key), op), "negative terminal API calls remain point-read-only")
			branches[branch] = append(branches[branch], key)
		}
	}
	request := &pb.TxnRequest{
		Compare: taskProtoComparisons(comparisons),
		Success: taskProtoPoints(branches[0]), Failure: taskProtoPoints(branches[1]),
	}
	require.LessOrEqual(t, len(request.Compare)+len(request.Success)+len(request.Failure), 64)
	require.LessOrEqual(t, request.Size(), 128<<10)
	return request
}

// Produce canonical, individually codec-valid foreign tuple bytes without
// signing anything or changing native source. These bytes are negative inputs
// only; preserved signed/fact bindings must reject every substituted record.
func detachedTerminalForeignCompanion(t *testing.T, ordinal int, point *mvccpb.KeyValue, sandboxID string) []byte {
	t.Helper()
	var record interface{ Validate() error }
	switch ordinal {
	case 9:
		var publication RuntimeEnrolledPublicationRecord
		require.NoError(t, decodeEnrolledRecord(point.Value, &publication))
		publication.SandboxID = sandboxID
		record = publication
	case 10:
		var proof RuntimeEnrolledPublicationProofRecord
		require.NoError(t, decodeEnrolledRecord(point.Value, &proof))
		proof.SandboxID = sandboxID
		record = proof
	case 11:
		var enrollment RuntimeEnrollmentRecord
		require.NoError(t, decodeEnrolledRecord(point.Value, &enrollment))
		enrollment.SandboxID = sandboxID
		record = enrollment
	case 12:
		var activation RuntimeEnrollmentActivationRecord
		require.NoError(t, decodeEnrolledRecord(point.Value, &activation))
		activation.SandboxID = sandboxID
		record = activation
	default:
		t.Fatalf("unexpected publication companion ordinal %d", ordinal)
	}
	wire, err := encodeEnrolledRecord(record)
	require.NoError(t, err, "binding negative must pass its own strict codec first")
	require.NotEqual(t, point.Value, []byte(wire))
	return []byte(wire)
}

func detachedTerminalNativeSource(t *testing.T, f *cleanupDetachedNoEffectNativeFixture, actual *detachedOwnerReleaseProof) []*mvccpb.KeyValue {
	t.Helper()
	keys := make([]string, len(actual.history.history))
	for i, point := range actual.history.history {
		keys[i] = string(point.Key)
	}
	points, err := f.b.readEnrolledPoints(f.ctx, keys, detachedRuntimeHistoryCaps())
	require.NoError(t, err)
	headKeys := []string{string(actual.kv.Key), string(actual.history.termination.Key), string(actual.history.provider.Key)}
	head, err := f.b.readEnrolledPoints(f.ctx, headKeys, []int{8192, 16384, 16384})
	require.NoError(t, err)
	return append(points, head...)
}

// All sixteen refusals read the same genuine native source. Only a cloned
// local reply is malformed, so a failed case cannot corrupt the next case's
// precondition or pass merely because an earlier source was already invalid.
func detachedPhysicalReleasedCompanionRefusals(t *testing.T, f *cleanupDetachedNoEffectNativeFixture, actual *detachedOwnerReleaseProof) {
	t.Helper()
	b := f.b
	original := b.client.KV
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	defer func() {
		b.client.KV = original
		b.client.Lease = lease.Lease
	}()
	issuer := b.taskRuntimeTerminationIssuer.(*detachedTerminationFixtureIssuer)
	certificates, signatures := issuer.certificates.Load(), issuer.signatures.Load()
	before := detachedTerminalNativeSource(t, f, actual)
	foreignSandbox := "99999999-9999-4999-8999-999999999999"
	if foreignSandbox == f.ref.SandboxID {
		foreignSandbox = "88888888-8888-4888-8888-888888888888"
	}
	index, err := b.namespace.runtimeIndexKey(actual.record.Runtime.UID)
	require.NoError(t, err)
	for offset, name := range []string{"publication", "publication-proof", "enrollment", "activation"} {
		ordinal := 9 + offset
		for _, fault := range []string{"nil", "foreign", "leased", "binding"} {
			t.Run(fmt.Sprintf("terminal-companion-%s-%s", name, fault), func(t *testing.T) {
				var requests []*pb.TxnRequest
				injections := 0
				expectedT := t
				apiCtx := context.WithValue(f.ctx, detachedTerminalAPICallKey, expectedT)
				b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, comparisons []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
					if ctx.Value(detachedTerminalAPICallKey) != expectedT {
						return original.Txn(ctx).If(comparisons...).Then(yes...).Else(no...).Commit()
					}
					request := detachedTerminalNegativeRequest(t, comparisons, yes, no)
					requests = append(requests, request)
					response, err := original.Txn(ctx).If(comparisons...).Then(yes...).Else(no...).Commit()
					require.NoError(t, err)
					require.NotNil(t, response)
					require.True(t, response.Succeeded)
					require.LessOrEqual(t, (*pb.TxnResponse)(response).Size(), 256<<10)
					if len(requests) != 7 {
						return response, nil
					}
					require.Len(t, yes, 13)
					require.Equal(t, actual.history.history[ordinal].Key, yes[ordinal].KeyBytes())
					require.Len(t, response.Responses, 13)
					ranged := response.Responses[ordinal].GetResponseRange()
					require.NotNil(t, ranged)
					require.Len(t, ranged.Kvs, 1)
					require.Equal(t, actual.history.history[ordinal], ranged.Kvs[0])

					ownedRange := *ranged
					owned := cloneOwnerReleasePoint(ranged.Kvs[0])
					ownedRange.Kvs = []*mvccpb.KeyValue{owned}
					switch fault {
					case "nil":
						ownedRange.Kvs, ownedRange.Count = nil, 0
					case "foreign":
						owned.Key = []byte(string(owned.Key) + "-foreign")
					case "leased":
						owned.Lease = 1
					case "binding":
						owned.Value = detachedTerminalForeignCompanion(t, ordinal, owned, foreignSandbox)
					}
					response.Responses[ordinal] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &ownedRange}}
					injections++
					if ordinal == 12 && fault == "foreign" {
						// Diagnose the same actual mutated reply with the exact
						// dispatched keys before returning it to the API chain.
						// This pure refusal cannot replace the API assertion below.
						keys := make([]string, len(yes))
						for i, op := range yes {
							keys[i] = string(op.KeyBytes())
						}
						returned := response.Responses[ordinal].GetResponseRange().Kvs[0]
						_, parseErr := b.readViewResponse(response, keys, detachedRuntimeHistoryCaps())
						t.Logf("activation-foreign diagnostic expected=%q returned=%q response_index=%d version=%d pure_parser_error=%v", keys[ordinal], string(returned.Key), ordinal, returned.Version, parseErr)
						require.ErrorIs(t, parseErr, ErrCorruptRecord, "the actual injected reply must independently fail the original pure parser")
					}
					return response, nil
				}}
				t.Cleanup(func() { b.client.KV = original })
				result, err := b.ReadSandboxReleased(apiCtx, f.ref.SandboxID)
				t.Logf("terminal companion diagnostic companion=%s fault=%s result_nil=%t err=%v requests=%d injections=%d", name, fault, result == nil, err, len(requests), injections)
				require.ErrorIs(t, err, ErrCorruptRecord)
				require.Nil(t, result)
				require.Equal(t, 1, injections)
				require.Len(t, requests, 7, "malformed companion must not reach final RPC8")
				for i, cardinality := range []int{1, 4, 1, 4, 1, 2, 13} {
					require.Len(t, requests[i].Success, cardinality)
					for _, op := range requests[i].Success {
						key := string(op.GetRequestRange().Key)
						require.NotEqual(t, index, key)
						require.NotEqual(t, f.job.state.execution.key, key)
						require.NotEqual(t, f.job.state.slot.key, key)
					}
				}
				require.Zero(t, lease.grants.Load())
				require.Equal(t, certificates, issuer.certificates.Load())
				require.Equal(t, signatures, issuer.signatures.Load())
				t.Logf("actual native history RPC7 companion=%s fault=%s refused; no final RPC/effect", name, fault)
			})
		}
	}
	b.client.KV = original
	require.Equal(t, before, detachedTerminalNativeSource(t, f, actual), "all16 negative reply substitutions leave the entire native source/head/seal intact")
	t.Run("terminal-post-auth-activation-ABA", func(t *testing.T) {
		detachedPhysicalReleasedPostAuthABA(t, f, actual, before)
	})
}

// Only the attempt to dispatch RPC8 proves that the caller has already
// authenticated the complete RPC7 history and matched its cohort. Insert one
// real, fenced same-value write here, never inside RPC7 before pure validation.
func detachedPhysicalReleasedPostAuthABA(t *testing.T, f *cleanupDetachedNoEffectNativeFixture, actual *detachedOwnerReleaseProof, before []*mvccpb.KeyValue) {
	t.Helper()
	b := f.b
	original := b.client.KV
	defer func() { b.client.KV = original }()
	activation := actual.history.history[12]
	issuer := b.taskRuntimeTerminationIssuer.(*detachedTerminationFixtureIssuer)
	certificates, signatures := issuer.certificates.Load(), issuer.signatures.Load()
	lease := &creationFaultLease{Lease: b.client.Lease}
	b.client.Lease = lease
	defer func() { b.client.Lease = lease.Lease }()
	var requests []*pb.TxnRequest
	injections := 0
	var changedRevision int64
	expectedT := t
	apiCtx := context.WithValue(f.ctx, detachedTerminalAPICallKey, expectedT)
	b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, comparisons []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		if ctx.Value(detachedTerminalAPICallKey) != expectedT {
			return original.Txn(ctx).If(comparisons...).Then(yes...).Else(no...).Commit()
		}
		request := detachedTerminalNegativeRequest(t, comparisons, yes, no)
		requests = append(requests, request)
		if len(requests) == 8 {
			require.Len(t, yes, 5)
			mods := make(map[string]int64)
			for _, cmp := range request.Compare {
				if cmp.Target == pb.Compare_MOD {
					mods[string(cmp.Key)] = cmp.GetModRevision()
				}
			}
			require.Len(t, mods, 21)
			for _, point := range actual.history.history {
				require.Equal(t, point.ModRevision, mods[string(point.Key)])
			}
			cmps := append(b.baseComparisons(), ownTaskFence(activation).comparisons()...)
			cmps = append(cmps, clientv3.Compare(clientv3.Version(string(activation.Key)), "=", activation.Version))
			bounded, cancel := b.requestContext(f.ctx)
			written, err := f.raw.Txn(bounded).If(cmps...).Then(clientv3.OpPut(string(activation.Key), string(activation.Value))).Commit()
			rpcErr := bounded.Err()
			cancel()
			require.NoError(t, err)
			require.NoError(t, rpcErr)
			require.NotNil(t, written)
			require.NotNil(t, written.Header)
			require.True(t, written.Succeeded, "the adversarial same-value Put must really commit before final dispatch")
			require.Len(t, written.Responses, 1)
			require.NotNil(t, written.Responses[0].GetResponsePut())
			changedRevision = written.Header.Revision
			require.Greater(t, changedRevision, activation.ModRevision)
			injections++
		}
		response, err := original.Txn(ctx).If(comparisons...).Then(yes...).Else(no...).Commit()
		require.NoError(t, err)
		require.NotNil(t, response)
		require.LessOrEqual(t, (*pb.TxnResponse)(response).Size(), 256<<10)
		if len(requests) < 8 {
			require.True(t, response.Succeeded)
			return response, nil
		}
		// Request declarations alone cannot show which branch ran. The actual
		// native failure and its two real metadata replies prove that final
		// success/index reads did not execute; the original parser checks them.
		require.False(t, response.Succeeded)
		require.Len(t, response.Responses, 2)
		for i, expected := range []struct {
			key   string
			value string
		}{{b.identityKey, b.identityValue}, {b.restoreKey, b.restoreEpoch}} {
			ranged := response.Responses[i].GetResponseRange()
			require.NotNil(t, ranged)
			require.Len(t, ranged.Kvs, 1)
			require.Equal(t, expected.key, string(ranged.Kvs[0].Key))
			require.Equal(t, expected.value, string(ranged.Kvs[0].Value))
		}
		return response, nil
	}}
	result, err := b.ReadSandboxReleased(apiCtx, f.ref.SandboxID)
	require.ErrorIs(t, err, ErrConflict)
	require.Nil(t, result)
	require.Len(t, requests, 8)
	require.Equal(t, 1, injections, "one raw producer Put, separate from eight read-only API Commit calls")
	require.Zero(t, lease.grants.Load())
	require.Equal(t, certificates, issuer.certificates.Load())
	require.Equal(t, signatures, issuer.signatures.Load())
	for _, request := range requests {
		for _, op := range request.Success {
			key := string(op.GetRequestRange().Key)
			require.NotEqual(t, f.job.state.execution.key, key)
			require.NotEqual(t, f.job.state.slot.key, key)
		}
	}

	b.client.KV = original
	after := detachedTerminalNativeSource(t, f, actual)
	require.Len(t, after, len(before))
	for i, point := range after {
		if i != 12 {
			require.Equal(t, before[i], point, "only original activation MVCC may change")
			continue
		}
		require.Equal(t, activation.Key, point.Key)
		require.Equal(t, activation.Value, point.Value)
		require.Equal(t, activation.CreateRevision, point.CreateRevision)
		require.Zero(t, point.Lease)
		require.Equal(t, activation.Version+1, point.Version)
		require.Equal(t, changedRevision, point.ModRevision)
	}
	t.Logf("real post-auth activation same-value Put changed MOD %d→%d; final native CAS chose metadata Else and API returned conflict; no MVCC restoration", activation.ModRevision, changedRevision)
}
