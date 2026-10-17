package etcd

import (
	"math"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// taskDestroyWireProjections models the original Stage begin/commit and
// ResolveStage requests for both producers. Business writes come from the same
// copied mutation that dispatch uses; maximal lease/revision/header scalars are
// size placeholders only and never become a Stage or execution capability.
func (b *Backend) taskDestroyWireProjections(ref StageReference, mutation Mutation) ([]cleanupWireProjection, error) {
	if b == nil || ref.StageID != "destroy" {
		return nil, ErrInvalidMutation
	}
	due := len(mutation.Writes) == 7
	comparisons, writes := 25, 6
	if due {
		comparisons, writes = 43, 7
	}
	if len(mutation.Comparisons) != comparisons || len(mutation.Writes) != writes {
		return nil, ErrInvalidMutation
	}
	for i, write := range mutation.Writes {
		if write.Delete != (due && i == 6) {
			return nil, ErrInvalidMutation
		}
	}
	owned, digest, err := prepareMutation(b.namespace, mutation)
	if err != nil || digest != ref.Digest {
		return nil, ErrInvalidMutation
	}
	guardKey, receiptKey, err := b.stageKeys(ref)
	if err != nil {
		return nil, err
	}
	guard := enrolledGuard(ref, math.MaxInt64)
	committed, err := encodeReceipt(ref, OutcomeCommitted)
	if err != nil {
		return nil, err
	}
	aborted, err := encodeReceipt(ref, OutcomeAborted)
	if err != nil {
		return nil, err
	}
	// Bounded StageReference fields keep these actual canonical records within
	// the producer arithmetic, independently of generic Stage's wider codec cap.
	if len(guard) > 2048 || len(committed) > 2048 || len(aborted) > 2048 {
		return nil, ErrInvalidMutation
	}

	identityKeys := []string{b.identityKey, b.restoreKey}
	identityValues := []string{b.identityValue, b.restoreEpoch}
	begin := &pb.TxnRequest{
		Compare: taskProtoComparisons(append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(guardKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))),
		Success: []*pb.RequestOp{taskProtoPut(guardKey, guard, math.MaxInt64)},
		Failure: taskProtoPoints(identityKeys),
	}
	cmps := append(b.baseComparisons(),
		clientv3.Compare(clientv3.Value(guardKey), "=", guard),
		clientv3.Compare(clientv3.LeaseValue(guardKey), "=", int64(math.MaxInt64)),
		clientv3.Compare(clientv3.CreateRevision(guardKey), "=", int64(math.MaxInt64)),
		clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0),
	)
	cmps = append(cmps, owned.Comparisons...)
	commitKeys := []string{b.identityKey, b.restoreKey, receiptKey, guardKey}
	commit := &pb.TxnRequest{
		Compare: taskProtoComparisons(cmps),
		Success: append(taskProtoWrites(owned.Writes), taskProtoPut(receiptKey, committed, 0)),
		Failure: taskProtoPoints(commitKeys),
	}
	success := taskProtoPutResponse(writes + 1)
	if due {
		success.Responses[6] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: success.Header, Deleted: 1}}}
	}

	// ResolveStage arbitrates with the same immutable receipt absence fence
	// as commit. Its successful branch writes aborted; an existing receipt is
	// returned on failure. Committed is the larger of the two legal wire values.
	resolveKeys := []string{b.identityKey, b.restoreKey, receiptKey}
	resolve := &pb.TxnRequest{
		Compare: taskProtoComparisons(append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))),
		Success: []*pb.RequestOp{taskProtoPut(receiptKey, aborted, 0)},
		Failure: taskProtoPoints(resolveKeys),
	}
	return []cleanupWireProjection{
		{"destroy-begin", begin, taskProtoPutResponse(1), taskProtoResponse(identityKeys, identityValues)},
		{"destroy-commit", commit, success, taskProtoResponse(commitKeys, []string{b.identityValue, b.restoreEpoch, committed, guard})},
		{"destroy-resolve", resolve, taskProtoPutResponse(1), taskProtoResponse(resolveKeys, []string{b.identityValue, b.restoreEpoch, committed})},
	}, nil
}

// Both producers call this before the original Stage allocates a Lease. The
// narrower128KiB request contract leaves generic Stage's256KiB limit unchanged.
func (b *Backend) preflightTaskDestroy(ref StageReference, mutation Mutation) error {
	projections, err := b.taskDestroyWireProjections(ref, mutation)
	if err != nil {
		return err
	}
	for _, projection := range projections {
		if err := validateCleanupWire(projection); err != nil {
			return err
		}
	}
	return nil
}
