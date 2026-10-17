package etcd

import (
	"errors"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// resolveColdRuntimeQuotaAttemptWorker arbitrates only the exact reference in
// an already authenticated history's canonical journal. One original worker
// call pays Range 256 KiB and Claim 1 for its single native transaction. The
// original enrolled owner remains registered after that call actually returns.
//
// The returned reference/outcome are ordinary arbitration data, including an
// existing receipt read after a false worker fence. They grant no scheduling,
// replacement, quota-return or physical authority. A caller still needs a
// separately paid authenticated aborted-receipt or committed-return read.
func (b *Backend) resolveColdRuntimeQuotaAttemptWorker(o *runtimeEnrolledPublicationOwner, history *coldQuotaReturnHistory, journal *mvccpb.KeyValue, s *WorkerSession, p *WorkerPartitionClaim, ranges, claims *WorkerBudgetShare) (ref StageReference, outcome Outcome, resultErr error) {
	outcome = OutcomeUnknown
	if !b.validWorkerBackend() {
		return ref, outcome, ErrInvalidRecord
	}
	if _, err := b.coldQuotaReturnOwnerContext(o); err != nil {
		return ref, outcome, err
	}
	record, err := b.decodeColdRuntimeQuotaAttempt(history, journal)
	if err != nil {
		return ref, outcome, err
	}
	ref = record.Reference
	if p == nil || p.partition != ref.Partition || p.partition != history.reservation.Owner.Partition {
		return ref, outcome, ErrInvalidRecord
	}
	_, receiptKey, err := b.stageKeys(ref)
	if err != nil {
		return ref, outcome, err
	}
	aborted, err := encodeReceipt(ref, OutcomeAborted)
	if err != nil {
		return ref, outcome, err
	}

	// Snapshot only immutable original member fences for complete preflight.
	// Membership/liveness, trial buckets and call registration are then checked
	// atomically by paid admission; no refusal above or here spends credit.
	request, err := b.coldQuotaWorkerResolveRequest(s, p, ranges, claims, receiptKey, aborted)
	if err != nil {
		return ref, outcome, err
	}
	failureKeys := []string{b.identityKey, b.restoreKey, receiptKey}
	worstFailure := taskProtoResponse(failureKeys, []string{b.identityValue, b.restoreEpoch, strings.Repeat("x", coldQuotaAttemptMaxBytes)})
	if err := b.checkColdQuotaWorkerResolveWire(request, worstFailure); err != nil {
		return ref, outcome, err
	}
	call, worker, err := b.beginColdQuotaWorkerCall(o, s, p, ranges, claims)
	if err != nil {
		return ref, outcome, err
	}
	defer call.finish()
	defer func() {
		// Unknown native results cannot establish healthy worker authority.
		// finish runs only after the actual native return, even on cancellation.
		if resultErr != nil && call.started {
			call.lose()
		}
	}()

	request.Compare = taskProtoComparisons(append(worker, clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0)))
	if err := b.checkColdQuotaWorkerResolveWire(request, worstFailure); err != nil {
		return ref, outcome, err
	}
	native, err := b.coldQuotaWorkerResolveRPC(call, request)
	if err != nil {
		return ref, outcome, errors.Join(ErrOutcomeUnknown, err)
	}
	response := (*clientv3.TxnResponse)(native)
	if err := b.operationResponseHeader(response); err != nil || native.Size() > coldQuotaWorkerRPCResponseLimit(coldQuotaRPCResolve) {
		return ref, outcome, errors.Join(ErrOutcomeUnknown, err)
	}
	if response.Succeeded {
		if !operationPutResponses(response, 1) {
			return ref, outcome, ErrOutcomeUnknown
		}
		return ref, OutcomeAborted, nil
	}

	// Preserve public Resolve's identity/restore/original-receipt ordering and
	// decoding semantics within this worker's narrower 4096-byte receipt cap.
	// A failed W24 can never publish Then, but its existing receipt remains data.
	points, err := b.stageEvidencePoints(response, failureKeys)
	if err != nil {
		return ref, outcome, errors.Join(ErrOutcomeUnknown, err)
	}
	if points[2] == nil {
		return ref, outcome, ErrOutcomeUnknown
	}
	if len(points[2].Value) > coldQuotaAttemptMaxBytes {
		return ref, outcome, errors.Join(ErrOutcomeUnknown, ErrCorruptReceipt)
	}
	decoded, err := decodeReceipt(points[2], ref)
	if err != nil {
		return ref, outcome, errors.Join(ErrOutcomeUnknown, err)
	}
	return ref, decoded, nil
}

// coldQuotaWorkerResolveRequest snapshots W24 under its lifecycle owner. These
// original fences are immutable after acquisition; no bucket or busy state is
// read here. Paid admission supplies the final comparisons before dispatch.
func (b *Backend) coldQuotaWorkerResolveRequest(s *WorkerSession, p *WorkerPartitionClaim, ranges, claims *WorkerBudgetShare, receiptKey, aborted string) (*pb.TxnRequest, error) {
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerSession(s) || !b.validWorkerPartition(p) || !b.validWorkerBudget(ranges) || !b.validWorkerBudget(claims) || p.session != s || ranges.session != s || claims.session != s || ranges.kind != WorkerBudgetRange || claims.kind != WorkerBudgetClaim {
		return nil, ErrInvalidRecord
	}
	comparisons := append(b.baseComparisons(), b.workerControl.profile.comparisons()...)
	for _, fence := range []taskFence{s.guard, p.fence, ranges.fence, claims.fence} {
		comparisons = append(comparisons, fence.comparisons()...)
	}
	comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(receiptKey), "=", 0))
	return &pb.TxnRequest{Compare: taskProtoComparisons(comparisons), Success: []*pb.RequestOp{taskProtoPut(receiptKey, aborted, 0)}, Failure: taskProtoPoints([]string{b.identityKey, b.restoreKey, receiptKey})}, nil
}

// checkColdQuotaWorkerResolveWire bounds the full request to 128 KiB and both
// reply branches to 16 KiB, then their sum to one original 256 KiB Range charge.
// The fixed reply uses the original cold-attempt 4096-byte receipt read bound;
// larger generic stage receipts remain unknown rather than widening transport.
func (b *Backend) checkColdQuotaWorkerResolveWire(request *pb.TxnRequest, worstFailure *pb.TxnResponse) error {
	size, err := coldQuotaWorkerRPCRequestBytes(coldQuotaRPCResolve, request)
	if err != nil {
		return err
	}
	replies := []*pb.TxnResponse{taskProtoPutResponse(1), worstFailure}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, replies); err != nil {
		return err
	}
	for _, reply := range replies {
		if reply.Size() > coldQuotaWorkerRPCResponseLimit(coldQuotaRPCResolve) || reply.Size() > maxMutationBytes-size {
			return ErrInvalidMutation
		}
	}
	return nil
}
