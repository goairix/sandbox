package etcd

import (
	"context"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Only a current genuine registered continuation can begin a write. Historical
// termination Current and settlement WriterClaim remain their original senders;
// neither supplies today's Claim/Run/Kernel permission. A consumed original
// Claim attempt selects diagnosis only, even after its lease is intentionally
// released. The diagnosis result is metadata, not renewed effect authority.
func (b *Backend) releaseTaskWorkspaceOwnerDetached(ctx context.Context, j *cleanupLocalJob, c *TaskClaim) (result TaskOwnerReleaseResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	if ctx == nil || !b.validTaskClaim(c) {
		return result, ErrInvalidRecord
	}
	c.mu.Lock()
	prior := c.ownerReleaseAttempt
	c.mu.Unlock()
	if prior != nil {
		return b.diagnoseDetachedOwnerRelease(ctx, c, prior)
	}
	if err := b.validateDetachedRuntimeOwner(j, c); err != nil {
		return result, err
	}
	o, err := b.beginLocalRuntimeReleasePurpose(ctx, c, "release")
	if err != nil {
		return result, err
	}
	defer o.finish()
	a, err := b.loadDetachedRuntimeAuthority(o.ctx, j, c)
	if err != nil {
		return result, err
	}
	execution, cancel := context.WithDeadline(o.ctx, a.detached.cutoff)
	defer cancel()
	x, err := b.loadRuntimeTerminationExecution(execution, a, nil)
	if err != nil || x == nil || x.intentKV == nil || x.intent.Phase != "cleanup-complete" {
		return result, errors.Join(ErrConflict, err)
	}
	record, err := newDetachedOwnerReleaseRecord(x.intent, x.intentKV.CreateRevision, x.intentKV.ModRevision, localRuntimeClaimWire(c))
	if err != nil {
		return result, err
	}
	wire, err := encodeDetachedOwnerRelease(record)
	if err != nil {
		return result, err
	}
	deleted := [3]string{record.Deleted[0].Key, record.Deleted[1].Key, record.Deleted[2].Key}
	cmps, _, _, err := a.detachedReleaseProjection(x.intentKV, deleted, wire)
	if err != nil {
		return result, err
	}
	if err = a.local(execution); err != nil {
		return result, err
	}

	// No lifecycle lock is acquired while holding c.mu. Registry/Kernel were
	// checked by a.local above; this last local check consumes one immutable
	// bounded candidate immediately before native send. Unknown keeps it and
	// the original purpose latch forever, rather than creating another attempt.
	c.mu.Lock()
	if c.ownerReleaseAttempt != o || o.detachedCandidate != "" {
		c.mu.Unlock()
		return result, ErrConflict
	}
	if err = c.live(execution); err != nil {
		c.mu.Unlock()
		return result, err
	}
	o.detachedCandidate = wire
	c.mu.Unlock()
	bounded, stop := b.requestContext(execution)
	response, err := b.client.Txn(bounded).If(cmps...).Then(clientv3.OpDelete(deleted[0]), clientv3.OpDelete(deleted[1]), clientv3.OpDelete(deleted[2]), clientv3.OpPut(a.releaseKey, wire)).Commit()
	err = errors.Join(err, bounded.Err())
	stop()
	if err != nil {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	if b.operationResponseHeader(response) != nil || taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(response)}) != nil {
		return result, ErrOutcomeUnknown
	}
	if !response.Succeeded {
		return result, ErrConflict
	}
	if len(response.Responses) != 4 {
		return result, ErrOutcomeUnknown
	}
	for _, op := range response.Responses[:3] {
		if op == nil || op.GetResponseDeleteRange() == nil || op.GetResponseDeleteRange().Deleted != 1 {
			return result, ErrOutcomeUnknown
		}
	}
	if response.Responses[3] == nil || response.Responses[3].GetResponsePut() == nil || response.Header.Revision <= x.intentKV.ModRevision || response.Header.Revision <= c.reference.CreateRevision {
		return result, ErrOutcomeUnknown
	}
	result.Outcome = OutcomeCommitted
	result.Entry = &TaskOwnerReleaseEntry{Task: c.reference.Task, CreateRevision: response.Header.Revision, SHA256: workspaceSyncIntentDigest(wire)}
	return result, nil
}

// The original Claim pointer and finished purpose owner may read their own
// candidate without a live Claim. They cannot begin another native mutation,
// even if a prior preflight failed or the actual seal remains absent. A cold
// caller without this marker uses the separate private historical reader.
func (b *Backend) diagnoseDetachedOwnerRelease(ctx context.Context, c *TaskClaim, original *taskLocalRuntimeReleaseOwner) (result TaskOwnerReleaseResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	if ctx == nil || !b.validTaskClaim(c) || original == nil {
		return result, ErrInvalidRecord
	}
	c.mu.Lock()
	valid := c.ownerReleaseAttempt == original && original.backend == b && original.claim == c && original.task == c.reference.Task
	candidate := original.detachedCandidate
	c.mu.Unlock()
	if !valid || candidate == "" {
		return result, ErrOutcomeUnknown
	}
	select {
	case <-original.done:
	default:
		return result, ErrOutcomeUnknown
	}
	o, err := b.beginLocalRuntimeRead(ctx, c.reference.Task)
	if err != nil {
		return result, err
	}
	defer o.finish()
	proof, err := b.readTaskOwnerReleaseProof(o.ctx, c.reference.Task, nil)
	if err != nil {
		if errors.Is(err, ErrCorruptRecord) || errors.Is(err, ErrCorruptReceipt) {
			return result, errors.Join(ErrConflict, err)
		}
		return result, err
	}
	if proof == nil {
		return result, ErrOutcomeUnknown
	}
	if proof.local != nil || proof.detached == nil || string(proof.detached.kv.Value) != candidate || proof.detached.record.Claim != localRuntimeClaimWire(c) {
		return result, ErrConflict
	}
	result.Outcome = OutcomeCommitted
	result.Entry = &TaskOwnerReleaseEntry{Task: c.reference.Task, CreateRevision: proof.detached.kv.CreateRevision, SHA256: workspaceSyncIntentDigest(candidate)}
	return result, nil
}

// This is only the v2 wire projection, not an authority constructor or native
// dispatch. Its original live caller supplies authenticated pins and checks
// the Claim/job cutoff separately. Keep the public local v1 60/64 unchanged:
// base4+Claim40+source8+empty1+settlement1+termination4+absent1 =59, then four
// writes =63. No Else branch, bookkeeping write or provider replay is added.
func (a *taskLocalRuntimeAuthority) detachedReleaseProjection(termination *mvccpb.KeyValue, deleted [3]string, wire string) ([]clientv3.Cmp, *pb.TxnRequest, *pb.TxnResponse, error) {
	if a == nil || a.backend == nil || a.detached == nil || len(a.comparisons) != 52 || !validLocalRuntimeMutableKV(termination) || string(termination.Key) != a.terminationKey || len(termination.Value) == 0 || len(termination.Value) > 16384 || len(wire) == 0 || len(wire) > detachedOwnerReleaseBytes || a.detached.settlement.mod <= 0 {
		return nil, nil, nil, ErrInvalidMutation
	}
	for _, key := range []string{a.terminationKey, a.releaseKey, a.detached.settlement.key, deleted[0], deleted[1], deleted[2]} {
		if !validNamespaceKey(a.backend.namespace, key) {
			return nil, nil, nil, ErrInvalidMutation
		}
	}
	cmps := append(a.terminationComparisons(termination, nil), clientv3.Compare(clientv3.Version(a.releaseKey), "=", 0))
	if len(cmps) != 59 {
		return nil, nil, nil, ErrInvalidMutation
	}
	writes := []Write{{Key: deleted[0], Delete: true}, {Key: deleted[1], Delete: true}, {Key: deleted[2], Delete: true}, {Key: a.releaseKey, Value: []byte(wire)}}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites(writes)}
	success := taskProtoResponse(nil, nil)
	for i := 0; i < 3; i++ {
		success.Responses = append(success.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: success.Header, Deleted: 1}}})
	}
	success.Responses = append(success.Responses, taskProtoPutResponse(1).Responses[0])
	projection := cleanupWireProjection{name: "detached-owner-release", request: request, success: success, failure: taskProtoResponse(nil, nil)}
	if err := validateCleanupWire(projection); err != nil {
		return nil, nil, nil, err
	}
	return cmps, request, success, nil
}
