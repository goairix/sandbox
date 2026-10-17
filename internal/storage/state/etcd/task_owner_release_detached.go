package etcd

import (
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

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
