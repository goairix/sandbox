//go:build linux && sandbox_detached_authority_test

package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/nodeobserver"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This is explicitly a native reader-component fixture. Its intent comes only
// from actual loader authority over Task3's real writer, then a guarded native
// create-only CAS. It does not execute or prove the provider/storeTermination
// reservation state machine, managed admission, PID1 or cgroup effects. No
// provider record, source, Task, E/S, Run or signed source fact is fabricated.
func TestDetachedRuntimeObserverNativeWriterFactAndGuard(t *testing.T) {
	f, fact := committedDetachedRuntimeFact(t)
	a, err := f.b.loadDetachedRuntimeAuthority(f.ctx, f.job, f.job.claim)
	require.NoError(t, err)
	intent := a.newTermination()
	wire, err := p.EncodeTaskRuntimeTerminationIntent(intent)
	require.NoError(t, err)
	require.NoError(t, a.guard(f.ctx, a.base()))
	cmps := append(a.base(), clientv3.Compare(clientv3.Version(a.terminationKey), "=", int64(0)))
	projection := taskRuntimeWriteProjection(cmps, a.terminationKey, string(wire))
	require.Len(t, projection.request.Compare, 55)
	require.Equal(t, 56, len(projection.request.Compare)+len(projection.request.Success)+len(projection.request.Failure))
	require.LessOrEqual(t, projection.request.Size(), 128<<10)
	require.LessOrEqual(t, projection.success.Size(), 256<<10)
	require.LessOrEqual(t, projection.failure.Size(), 256<<10)
	require.NoError(t, a.preflightWrite(cmps, a.terminationKey, string(wire)))
	response, err := f.b.client.Txn(f.ctx).If(cmps...).Then(clientv3.OpPut(a.terminationKey, string(wire))).Commit()
	require.NoError(t, err)
	require.NoError(t, f.b.operationResponseHeader(response))
	require.True(t, response.Succeeded)
	require.True(t, operationPutResponses(response, 1))
	require.LessOrEqual(t, (*pb.TxnResponse)(response).Size(), 256<<10)
	points, err := f.b.readEnrolledPoints(f.ctx, []string{a.terminationKey}, []int{p.MaxTaskRuntimeTerminationIntentBytes})
	require.NoError(t, err)
	require.NotNil(t, points[0])
	require.Equal(t, wire, points[0].Value)
	require.Equal(t, points[0].CreateRevision, points[0].ModRevision)
	require.EqualValues(t, 1, points[0].Version)
	require.Greater(t, points[0].CreateRevision, f.job.claim.reference.CreateRevision)

	// These operator roots and issuer key are the original fixture's inputs,
	// while enrollment/identity/clock come from its actual published state and
	// Backend. They do not come from a caller-supplied authority tuple.
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, ed25519.SeedSize))
	issuerWire := f.publication.Enrollment.Request.IssuerCertificate
	observation, err := f.b.authorityClock.Observe(f.ctx)
	require.NoError(t, err)
	issuer, err := f.b.enrolledVerifier.VerifyCommandIssuerCertificate(issuerWire, observation.UTC)
	require.NoError(t, err)
	claims := p.TaskRuntimeTerminationTicketClaims{Version: 1, Purpose: "task_runtime_terminate", Current: intent.Current, Partition: intent.Partition,
		IntentCreateRevision: points[0].CreateRevision, IntentModRevision: points[0].ModRevision, IntentDigest: workspaceSyncIntentDigest(string(points[0].Value)),
		EnrollmentDigest: intent.EnrollmentDigest, ObserverCertificateDigest: intent.ObserverCertificateDigest, NodeUID: intent.NodeUID, NodeBootID: intent.NodeBootID,
		NotBefore: observation.UTC.Add(-2 * time.Second), NotAfter: observation.UTC.Add(15 * time.Second),
	}
	ticket, err := p.SignTaskRuntimeTerminationTicket(issuerKey, issuer, claims)
	require.NoError(t, err)
	identity, err := decodeIdentity(f.b.identityValue, f.b.namespace)
	require.NoError(t, err)
	options := nodeobserver.Options{Binding: p.TrustBinding{Namespace: f.b.namespace.Root(), AuthorityID: f.b.publicationAuthorityID, Target: f.b.publicationTarget, RestoreEpoch: f.b.restoreEpoch}, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, Clock: f.b.authorityClock, EtcdIdentity: []byte(f.b.identityValue), EtcdClusterID: identity.ClusterID, RequestTimeout: f.b.requestTimeout}

	original := f.b.client.KV
	var guards, commits atomic.Int64
	f.b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, actual []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		commits.Add(1)
		guard := len(actual) == 58 && len(yes) == 1 && len(no) == 0 && yes[0].IsGet() && string(yes[0].KeyBytes()) == f.b.identityKey
		requestBytes := 0
		if guard {
			guards.Add(1)
			require.Empty(t, yes[0].RangeBytes())
			request := &pb.TxnRequest{Compare: taskProtoComparisons(actual), Success: taskProtoPoints([]string{f.b.identityKey})}
			require.Equal(t, 59, len(request.Compare)+len(request.Success)+len(request.Failure))
			requestBytes = request.Size()
			require.LessOrEqual(t, requestBytes, 128<<10)
		}
		result, err := original.Txn(ctx).If(actual...).Then(yes...).Else(no...).Commit()
		if guard && result != nil {
			replyBytes := (*pb.TxnResponse)(result).Size()
			require.LessOrEqual(t, replyBytes, 256<<10)
			t.Logf("actual observer guard: compares=58 points=1 nodes=59 request_bytes=%d reply_bytes=%d", requestBytes, replyBytes)
		}
		return result, err
	}}
	t.Cleanup(func() { f.b.client.KV = original })
	require.NoError(t, nodeobserver.ValidateDetachedTerminationNativeForTest(f.ctx, f.b.client, options, f.publication.Enrollment, ticket, issuerWire))
	require.EqualValues(t, 1, guards.Load(), "the component must complete its actual 58+1 native guard")
	t.Logf("actual successful observer bridge total Commit submissions=%d; excludes writer/intent fixture and is not a transport-dispatch upper bound", commits.Load())

	// This is a fault after the successful original read. Same-value Put is a
	// new native version/mod, so it must revoke the old fact's authority even
	// though its copied value/digest and genuine signed ticket stay unchanged.
	_, err = f.raw.Put(f.ctx, string(fact.kv.Key), string(fact.kv.Value))
	require.NoError(t, err)
	require.Error(t, nodeobserver.ValidateDetachedTerminationNativeForTest(f.ctx, f.b.client, options, f.publication.Enrollment, ticket, issuerWire))
	require.EqualValues(t, 1, guards.Load(), "revoked native fact must fail before a second live guard")
	t.Logf("actual successful bridge plus rejected ABA bridge total Commit submissions=%d; transport dispatch/retry count remains unmeasured", commits.Load())
}
