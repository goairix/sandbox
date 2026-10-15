package etcd

import (
	"context"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func localRuntimeClaimWire(c *TaskClaim) taskCloseClaimWire {
	r := c.reference
	return taskCloseClaimWire{Task: r.Task, ClaimID: r.ClaimID, WorkerID: r.WorkerID, CreateRevision: r.CreateRevision, LeaseID: r.LeaseID}
}

func (a *taskLocalRuntimeAuthority) releaseProjection(termination *mvccpb.KeyValue, deleted [3]string, wire string) ([]clientv3.Cmp, *pb.TxnRequest, *pb.TxnResponse, error) {
	cmps := append(a.terminationComparisons(termination, nil), clientv3.Compare(clientv3.Version(a.releaseKey), "=", 0))
	writes := []Write{{Key: deleted[0], Delete: true}, {Key: deleted[1], Delete: true}, {Key: deleted[2], Delete: true}, {Key: a.releaseKey, Value: []byte(wire)}}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites(writes)}
	success := taskProtoResponse(nil, nil)
	for i := 0; i < 3; i++ {
		success.Responses = append(success.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: success.Header, Deleted: 1}}})
	}
	success.Responses = append(success.Responses, taskProtoPutResponse(1).Responses[0])
	if len(cmps) != 60 || len(request.Success) != 4 {
		return nil, nil, nil, ErrInvalidMutation
	}
	if err := taskCheckWire(a.backend.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{success, taskProtoResponse(nil, nil)}); err != nil {
		return nil, nil, nil, err
	}
	return cmps, request, success, nil
}

func (b *Backend) ReleaseTaskWorkspaceOwnerLocal(ctx context.Context, c *TaskClaim) (result TaskOwnerReleaseResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	if !b.validTaskClaim(c) {
		return result, ErrInvalidRecord
	}
	c.mu.Lock()
	repeated := c.ownerReleaseAttempt != nil
	c.mu.Unlock()
	if repeated {
		entry, err := b.LoadTaskOwnerRelease(ctx, c.reference.Task)
		result.Entry = entry
		if entry != nil && err == nil {
			result.Outcome = OutcomeCommitted
		}
		return result, err
	}
	o, err := b.beginLocalRuntimeReleasePurpose(ctx, c, "release")
	if err != nil {
		return result, err
	}
	defer o.finish()
	a, err := b.loadLocalRuntimeAuthority(o.ctx, c)
	if err != nil {
		return result, err
	}
	r, termination, _, err := b.readLocalRuntimeHistory(o.ctx, c.reference.Task)
	if err != nil || termination == nil || r.Phase != "cleanup-complete" || !a.matchesTermination(r) {
		return result, errors.Join(ErrConflict, err)
	}
	receipt, err := newLocalOwnerReleaseRecord(r, termination.CreateRevision, termination.ModRevision, localRuntimeClaimWire(c))
	if err != nil {
		return result, err
	}
	wire, err := encodeLocalOwnerRelease(receipt)
	if err != nil {
		return result, err
	}
	deleted := [3]string{receipt.Deleted[0].Key, receipt.Deleted[1].Key, receipt.Deleted[2].Key}
	cmps, _, _, err := a.releaseProjection(termination, deleted, wire)
	if err != nil {
		return result, err
	}
	if err := a.local(o.ctx); err != nil {
		return result, err
	}
	bounded, cancel := b.requestContext(o.ctx)
	response, err := b.client.Txn(bounded).If(cmps...).Then(clientv3.OpDelete(deleted[0]), clientv3.OpDelete(deleted[1]), clientv3.OpDelete(deleted[2]), clientv3.OpPut(a.releaseKey, wire)).Commit()
	err = errors.Join(err, bounded.Err())
	cancel()
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
	if response.Responses[3] == nil || response.Responses[3].GetResponsePut() == nil || response.Header.Revision <= termination.ModRevision || response.Header.Revision <= c.reference.CreateRevision {
		return result, ErrOutcomeUnknown
	}
	result.Entry = &TaskOwnerReleaseEntry{Task: c.reference.Task, CreateRevision: response.Header.Revision, SHA256: workspaceSyncIntentDigest(wire)}
	result.Outcome = OutcomeCommitted
	return result, nil
}
