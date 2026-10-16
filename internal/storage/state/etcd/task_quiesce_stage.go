package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"math"
	"strings"
	"time"
)

func (b *Backend) beginReservedTaskQuiescenceStage(ctx context.Context, l StageAttemptLocator, pin taskQuiescenceReservationPin, ttl time.Duration, build func(StageAttemptLocator) (Mutation, error)) (*Stage, error) {
	if ctx == nil || build == nil || ttl <= 0 || ttl > 24*time.Hour || l.Validate() != nil || l.Namespace != b.namespace.Root() || l.RestoreEpoch != b.restoreEpoch || l.StageID != "task_quiesce_users" || !validPreparationUUID(l.RequestID) || pin.revision <= 0 || !validNamespaceKey(b.namespace, pin.key) {
		return nil, ErrInvalidMutation
	}
	// Only the fixed task reservation key is permitted, never arbitrary comparisons.
	prefix := fmt.Sprintf("%sp/%02x/tasks/", b.namespace.Root(), l.Partition)
	if !strings.HasPrefix(pin.key, prefix) {
		return nil, ErrInvalidMutation
	}
	taskID := strings.TrimSuffix(strings.TrimPrefix(pin.key, prefix), "/quiesce-users-attempt")
	if !validPreparationUUID(taskID) || pin.key != prefix+taskID+"/quiesce-users-attempt" {
		return nil, ErrInvalidMutation
	}
	return b.beginStageWithLocator(ctx, l, ttl, build, &pin)
}
func taskProtoComparisons(cmps []clientv3.Cmp) []*pb.Compare {
	result := make([]*pb.Compare, len(cmps))
	for i, c := range cmps {
		v := pb.Compare(c)
		result[i] = &v
	}
	return result
}
func taskProtoPut(key, value string, lease int64) *pb.RequestOp {
	return &pb.RequestOp{Request: &pb.RequestOp_RequestPut{RequestPut: &pb.PutRequest{Key: []byte(key), Value: []byte(value), Lease: lease}}}
}
func taskProtoRange(key string) *pb.RequestOp {
	return &pb.RequestOp{Request: &pb.RequestOp_RequestRange{RequestRange: &pb.RangeRequest{Key: []byte(key)}}}
}
func taskProtoPoints(keys []string) []*pb.RequestOp {
	ops := make([]*pb.RequestOp, len(keys))
	for i, key := range keys {
		ops[i] = taskProtoRange(key)
	}
	return ops
}
func taskProtoWrites(writes []Write) []*pb.RequestOp {
	ops := make([]*pb.RequestOp, len(writes))
	for i, w := range writes {
		if w.Delete {
			ops[i] = &pb.RequestOp{Request: &pb.RequestOp_RequestDeleteRange{RequestDeleteRange: &pb.DeleteRangeRequest{Key: []byte(w.Key)}}}
		} else {
			ops[i] = taskProtoPut(w.Key, string(w.Value), 0)
		}
	}
	return ops
}

// These maximal scalars are conservative byte-accounting placeholders only.
// They never enter native requests or become receipt/claim authority.
func taskProtoResponse(keys, values []string) *pb.TxnResponse {
	h := &pb.ResponseHeader{ClusterId: math.MaxUint64, MemberId: math.MaxUint64, Revision: math.MaxInt64, RaftTerm: math.MaxUint64}
	r := &pb.TxnResponse{Header: h, Succeeded: true}
	for i, key := range keys {
		kv := &mvccpb.KeyValue{Key: []byte(key), Value: []byte(values[i]), CreateRevision: math.MaxInt64, ModRevision: math.MaxInt64, Version: math.MaxInt64, Lease: math.MaxInt64}
		r.Responses = append(r.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponseRange{ResponseRange: &pb.RangeResponse{Header: h, Kvs: []*mvccpb.KeyValue{kv}, Count: 1}}})
	}
	return r
}
func taskProtoPutResponse(count int) *pb.TxnResponse {
	h := taskProtoResponse(nil, nil).Header
	r := &pb.TxnResponse{Header: h, Succeeded: true}
	for i := 0; i < count; i++ {
		r.Responses = append(r.Responses, &pb.ResponseOp{Response: &pb.ResponseOp_ResponsePut{ResponsePut: &pb.PutResponse{Header: h}}})
	}
	return r
}
func taskCheckWire(n Namespace, requests []*pb.TxnRequest, replies []*pb.TxnResponse) error {
	for _, r := range requests {
		if len(r.Compare)+len(r.Success)+len(r.Failure) > maxStageOperations || r.Size() > maxMutationBytes {
			return ErrInvalidMutation
		}
		for _, c := range r.Compare {
			if c == nil || !validComparison(clientv3.Cmp(*c)) || !validNamespaceKey(n, string(c.Key)) && !taskWorkspaceSyncEmptyPrefixComparison(n, c) {
				return ErrInvalidMutation
			}
		}
	}
	for _, r := range replies {
		if r.Size() > maxMutationBytes {
			return ErrInvalidMutation
		}
	}
	return nil
}

// Only the entire native task operations prefix may use the trailing-slash
// range form. Point comparison validation remains unchanged.
func taskWorkspaceSyncEmptyPrefixComparison(n Namespace, c *pb.Compare) bool {
	if c == nil || c.Target != pb.Compare_VERSION || c.Result != pb.Compare_EQUAL || !validComparison(clientv3.Cmp(*c)) {
		return false
	}
	v, ok := c.TargetUnion.(*pb.Compare_Version)
	if !ok || v.Version != 0 || len(c.Key) > maxNamespaceKeyBytes {
		return false
	}
	key := string(c.Key)
	if !strings.HasPrefix(key, n.Root()) || !strings.HasSuffix(key, "/") || !validNamespaceKey(n, strings.TrimSuffix(key, "/")) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(key, n.Root()), "/")
	if len(parts) != 6 || parts[0] != "p" || len(parts[1]) != 2 || parts[2] != "sandboxes" || !validDomainSegment(parts[3]) || parts[4] != "operations" || parts[5] != "" {
		return false
	}
	for _, digit := range parts[1] {
		if !(digit >= '0' && digit <= '9' || digit >= 'a' && digit <= 'f') {
			return false
		}
	}
	return string(c.RangeEnd) == clientv3.GetPrefixRangeEnd(key) && bytes.Compare(c.RangeEnd, []byte(clientv3.GetPrefixRangeEnd(n.Root()))) <= 0
}
func (b *Backend) preflightTaskReservation(cmps []clientv3.Cmp, key, value string) error {
	projections, err := b.taskReservationWireProjections(cmps, key, value)
	if err != nil {
		return err
	}
	return checkTaskWireProjections(b.namespace, projections)
}

// Reservation writes and their exact readback share one native wire shape.
// The last comparison is the original absence check, absent from the read.
func (b *Backend) taskReservationWireProjections(cmps []clientv3.Cmp, key, value string) ([]cleanupWireProjection, error) {
	if len(cmps) == 0 || len(value) > 8192 {
		return nil, ErrInvalidMutation
	}
	keys := []string{b.identityKey, b.restoreKey, key}
	values := []string{b.identityValue, b.restoreEpoch, value}
	for i, k := range keys {
		if !validNamespaceKey(b.namespace, k) || len(values[i]) > maxRecordBytes {
			return nil, ErrInvalidMutation
		}
	}
	reads := taskProtoPoints(keys)
	write := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: []*pb.RequestOp{taskProtoPut(key, value, 0)}, Failure: reads}
	read := &pb.TxnRequest{Compare: taskProtoComparisons(cmps[:len(cmps)-1]), Success: reads, Failure: reads}
	response := taskProtoResponse(keys, values)
	return []cleanupWireProjection{{"write", write, taskProtoPutResponse(1), response}, {"read", read, response, response}}, nil
}

func (b *Backend) preflightReservedTaskStage(ref StageReference, m Mutation, guardKey, receiptKey, committed string, pin taskQuiescenceReservationPin) error {
	projections, err := b.taskStageWireProjections(ref, m, guardKey, receiptKey, committed, &pin)
	if err != nil {
		return err
	}
	return checkTaskWireProjections(b.namespace, projections)
}

// Stage projections preserve the generic14-node overhead. Only reserved
// quiescence supplies the additional original reservation Mod pin. No guard,
// receipt or dimensional future value returned here is a Stage capability.
func (b *Backend) taskStageWireProjections(ref StageReference, m Mutation, guardKey, receiptKey, committed string, pin *taskQuiescenceReservationPin) ([]cleanupWireProjection, error) {
	guard, err := json.Marshal(struct {
		StageReference
		LeaseID int64 `json:"lease_id"`
	}{ref, math.MaxInt64})
	if err != nil {
		return nil, ErrInvalidMutation
	}
	if len(guard) > maxRecordBytes || len(committed) > maxRecordBytes || len(b.identityValue) > maxRecordBytes || len(b.restoreEpoch) > maxRecordBytes {
		return nil, ErrInvalidMutation
	}
	// Account both receipt outcome envelopes explicitly.
	aborted, err := encodeReceipt(ref, OutcomeAborted)
	if err != nil || len(aborted) > maxRecordBytes {
		return nil, ErrInvalidMutation
	}
	beginCmps := append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(guardKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))
	begin := &pb.TxnRequest{Compare: taskProtoComparisons(beginCmps), Success: []*pb.RequestOp{taskProtoPut(guardKey, string(guard), math.MaxInt64)}, Failure: taskProtoPoints([]string{b.identityKey, b.restoreKey})}
	commitCmps := append(b.baseComparisons(), clientv3.Compare(clientv3.Value(guardKey), "=", string(guard)), clientv3.Compare(clientv3.LeaseValue(guardKey), "=", math.MaxInt64), clientv3.Compare(clientv3.CreateRevision(guardKey), "=", math.MaxInt64), clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))
	if pin != nil {
		commitCmps = append(commitCmps, pin.comparison())
	}
	commitCmps = append(commitCmps, m.Comparisons...)
	ops := append(taskProtoWrites(m.Writes), taskProtoPut(receiptKey, committed, 0))
	keys := []string{b.identityKey, b.restoreKey, receiptKey, guardKey}
	commit := &pb.TxnRequest{Compare: taskProtoComparisons(commitCmps), Success: ops, Failure: taskProtoPoints(keys)}
	resolveCmps := append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))
	resolve := &pb.TxnRequest{Compare: taskProtoComparisons(resolveCmps), Success: []*pb.RequestOp{taskProtoPut(receiptKey, aborted, 0)}, Failure: taskProtoPoints(keys[:3])}
	// Include every possible success envelope (Delete replies too) and full point branches.
	success := taskProtoPutResponse(len(m.Writes) + 1)
	for i, w := range m.Writes {
		if w.Delete {
			success.Responses[i] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: success.Header, Deleted: 1}}}
		}
	}
	return []cleanupWireProjection{
		{"begin", begin, taskProtoPutResponse(1), taskProtoResponse(keys[:2], []string{b.identityValue, b.restoreEpoch})},
		{"commit", commit, success, taskProtoResponse(keys, []string{b.identityValue, b.restoreEpoch, committed, string(guard)})},
		{"resolve", resolve, taskProtoPutResponse(1), taskProtoResponse(keys[:3], []string{b.identityValue, b.restoreEpoch, aborted})},
	}, nil
}
