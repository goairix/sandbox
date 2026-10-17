package etcd

import (
	"bytes"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// readColdQuotaWorkerPoints pays one original Range charge for finite metadata
// points under W20. Keys/caps are ordinary data, never quota or physical-death
// authority. The caller retains its registered enrolled owner for subsequent
// phases; only this read's original worker call is finished here, after the
// actual native return. Typed business-history parsing belongs to the caller.
// The caller keeps keys and caps unchanged until this method returns.
func (b *Backend) readColdQuotaWorkerPoints(o *runtimeEnrolledPublicationOwner, s *WorkerSession, p *WorkerPartitionClaim, ranges *WorkerBudgetShare, keys []string, caps []int) (_ []*mvccpb.KeyValue, resultErr error) {
	if !b.validWorkerBackend() || len(keys) != len(caps) || len(keys) == 0 || len(keys) > 14 {
		return nil, ErrInvalidRecord
	}

	all := append([]string{b.identityKey, b.restoreKey}, keys...)
	values := []string{b.identityValue, b.restoreEpoch}
	for i, key := range all {
		if !validNamespaceKey(b.namespace, key) {
			return nil, ErrInvalidRecord
		}
		if i >= 2 {
			if caps[i-2] <= 0 || caps[i-2] > maxRecordBytes {
				return nil, ErrInvalidRecord
			}
			values = append(values, string(bytes.Repeat([]byte{'x'}, caps[i-2])))
		}
	}
	request, err := b.coldQuotaWorkerMetadataRequest(s, p, ranges, all)
	if err != nil {
		return nil, err
	}
	worstReply := taskProtoResponse(all, values)
	if err := b.checkColdQuotaWorkerMetadataWire(request, worstReply); err != nil {
		return nil, err
	}

	call, comparisons, err := b.beginColdQuotaWorkerMetadataCall(o, s, p, ranges)
	if err != nil {
		return nil, err
	}
	defer call.finish()
	defer func() {
		// A malformed or missing ACK cannot establish healthy authority.
		// Local preflight/admission refusals above have neither charged nor
		// started native work and never invalidate the original session.
		if resultErr != nil && call.started {
			call.lose()
		}
	}()

	request.Compare = taskProtoComparisons(comparisons)
	// Send only the final paid admission's W20. Recheck its full frame so a
	// future change to fence mutability cannot bypass the earlier preflight.
	// An impossible changed-frame refusal keeps the already paid credit spent.
	if err := b.checkColdQuotaWorkerMetadataWire(request, worstReply); err != nil {
		return nil, err
	}

	response, err := b.coldQuotaWorkerMetadataRPC(call, request)
	if err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	native := (*clientv3.TxnResponse)(response)
	if !enrolledResponseBound(native) {
		return nil, errors.Join(ErrOutcomeUnknown, ErrCorruptRecord)
	}
	points, err := b.stageEvidencePoints(native, all)
	if err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	if !response.Succeeded {
		// Identity evidence is valid, but a failed W20 may be any worker
		// fence. It cannot be interpreted as a safe business-only refusal.
		return nil, errors.Join(ErrOutcomeUnknown, ErrGuardExpired)
	}
	for i, kv := range points[2:] {
		if kv != nil && len(kv.Value) > caps[i] {
			return nil, errors.Join(ErrOutcomeUnknown, ErrCorruptRecord)
		}
	}

	return points[2:], nil
}

// checkColdQuotaWorkerMetadataWire bounds the complete pure-point request and
// conservative reply. taskCheckWire checks individual messages; this cohort
// additionally limits their sum to one original 256 KiB Range allowance.
func (b *Backend) checkColdQuotaWorkerMetadataWire(request *pb.TxnRequest, worstReply *pb.TxnResponse) error {
	requestBytes, err := coldQuotaWorkerRPCRequestBytes(coldQuotaRPCMetadata, request)
	if err != nil {
		return err
	}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{worstReply}); err != nil {
		return err
	}
	if worstReply.Size() > maxMutationBytes-requestBytes {
		return ErrInvalidMutation
	}
	return nil
}

// coldQuotaWorkerMetadataRequest snapshots only original member fences for
// wire preflight, under their owning lifecycle lock. These fences are immutable
// after successful acquisition. Full owner/liveness/busy checks and atomic
// payment remain solely in beginColdQuotaWorkerTurn, after the complete wire
// preflight; no bucket or Claim state is read by this preparation step.
func (b *Backend) coldQuotaWorkerMetadataRequest(s *WorkerSession, p *WorkerPartitionClaim, ranges *WorkerBudgetShare, keys []string) (*pb.TxnRequest, error) {
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if !b.validWorkerSession(s) || !b.validWorkerPartition(p) || !b.validWorkerBudget(ranges) || p.session != s || ranges.session != s || ranges.kind != WorkerBudgetRange {
		return nil, ErrInvalidRecord
	}
	comparisons := append(b.baseComparisons(), b.workerControl.profile.comparisons()...)
	for _, fence := range []taskFence{s.guard, p.fence, ranges.fence} {
		comparisons = append(comparisons, fence.comparisons()...)
	}
	points := taskProtoPoints(keys)
	return &pb.TxnRequest{Compare: taskProtoComparisons(comparisons), Success: points, Failure: points}, nil
}
