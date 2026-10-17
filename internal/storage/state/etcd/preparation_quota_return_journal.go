package etcd

import (
	"errors"
	"strings"
	"sync/atomic"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// journalAndContinueColdRuntimeQuotaReturn owns one journal admission attempt.
// Only its trusted successful ACK permits the original continuation to Grant.
// Every error burns this capability, including an unknown durable CAS result;
// a duplicate racing caller must not burn the admitted submitter's capability.
// The registering caller owns finish through continuation and ReleaseStage.
func (b *Backend) journalAndContinueColdRuntimeQuotaReturn(o *runtimeEnrolledPublicationOwner, prepared *coldQuotaReturnPrepared, previous, aborted *mvccpb.KeyValue) (ColdRuntimeQuotaReturnResult, error) {
	result := ColdRuntimeQuotaReturnResult{Outcome: OutcomeUnknown}
	if prepared == nil {
		return result, ErrInvalidRecord
	}
	result.Reference = prepared.reference
	if !atomic.CompareAndSwapUint32(&prepared.journalAttempted, 0, 1) {
		return result, ErrConflict
	}
	admitted := false
	defer func() {
		if !admitted {
			atomic.StoreUint32(&prepared.consumed, 1)
		}
	}()
	if prepared.self != prepared || prepared.backend != b || prepared.owner != o || prepared.history == nil || prepared.history.release == nil || prepared.history.release.kv == nil || prepared.returnKey == "" || atomic.LoadUint32(&prepared.consumed) != 0 {
		return result, ErrInvalidRecord
	}
	ctx, err := b.coldQuotaReturnOwnerContext(o)
	if err != nil {
		return result, err
	}
	_, digest, err := prepareMutation(b.namespace, prepared.mutation)
	if err != nil || digest != prepared.reference.Digest {
		return result, ErrInvalidMutation
	}
	key, err := b.namespace.coldRuntimeQuotaAttemptKey(prepared.history.reservation.Owner.Partition, prepared.history.reservation.Owner.IntentID)
	if err != nil {
		return result, err
	}
	returnKey, err := b.namespace.coldRuntimeQuotaReturnKey(prepared.history.reservation.Owner.Partition, prepared.history.reservation.Owner.IntentID)
	if err != nil || returnKey != prepared.returnKey {
		return result, ErrInvalidRecord
	}
	wire, err := encodeColdRuntimeQuotaAttempt(b.coldQuotaAttemptRecord(prepared))
	if err != nil {
		return result, err
	}
	comparisons, err := b.coldQuotaAttemptComparisons(prepared, key, previous, aborted)
	if err != nil {
		return result, err
	}
	failureKeys := []string{b.identityKey, b.restoreKey, key}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(comparisons), Success: taskProtoWrites([]Write{{Key: key, Value: []byte(wire)}}), Failure: taskProtoPoints(failureKeys)}
	maxFailure := taskProtoResponse(failureKeys, []string{b.identityValue, b.restoreEpoch, strings.Repeat("x", coldQuotaAttemptMaxBytes)})
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoPutResponse(1), maxFailure}); err != nil {
		return result, err
	}

	bounded, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(bounded).If(comparisons...).Then(clientv3.OpPut(key, wire)).Else(clientv3.OpGet(failureKeys[0]), clientv3.OpGet(failureKeys[1]), clientv3.OpGet(failureKeys[2])).Commit()
	err = errors.Join(err, bounded.Err())
	cancel()
	if err != nil {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	if err := b.operationResponseHeader(response); err != nil || !enrolledResponseBound(response) {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	if !response.Succeeded {
		points, err := b.stageEvidencePoints(response, failureKeys)
		if err != nil {
			return result, err
		}
		if points[2] != nil && len(points[2].Value) > coldQuotaAttemptMaxBytes {
			return result, ErrCorruptRecord
		}
		return result, ErrConflict
	}
	if !operationPutResponses(response, 1) {
		return result, ErrOutcomeUnknown
	}

	// Mark admission before entering the existing once-only continuation. It
	// inherits the original owner's cancellation and consumes itself even when
	// that cancellation wins immediately after the journal acknowledgement.
	admitted = true
	return b.continueColdRuntimeQuotaReturn(o, prepared)
}

// Six authenticated snapshots pin the attribution's original publication,
// settled reservation and release. Ready/slot remain in the frozen refund
// mutation: this CAS never refunds or changes those domain records itself.
func (b *Backend) coldQuotaAttemptComparisons(prepared *coldQuotaReturnPrepared, key string, previous, aborted *mvccpb.KeyValue) ([]clientv3.Cmp, error) {
	h := prepared.history
	if h.publication == nil {
		return nil, ErrInvalidRecord
	}
	comparisons := b.baseComparisons()
	for _, kv := range []*mvccpb.KeyValue{h.publication.journal, h.publication.receipt, h.reservedKV, h.workKV, h.settledKV, h.release.kv} {
		if !operationPermanentKV(kv) || kv.Version <= 0 {
			return nil, ErrCorruptRecord
		}
		comparisons = append(comparisons, ownTaskFence(kv).comparisons()...)
	}
	comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(prepared.returnKey), "=", 0))
	if previous == nil {
		if aborted != nil {
			return nil, ErrCorruptReceipt
		}
		comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
		if len(comparisons) != 30 {
			return nil, ErrInvalidMutation
		}
		return comparisons, nil
	}
	old, err := b.decodeColdRuntimeQuotaAttempt(h, previous)
	if err != nil || old.Reference == prepared.reference {
		return nil, ErrCorruptRecord
	}
	if err := coldQuotaAttemptAbortedReceipt(previous, aborted, old.Reference); err != nil {
		return nil, err
	}
	comparisons = append(comparisons, ownTaskFence(previous).comparisons()...)
	comparisons = append(comparisons, ownTaskFence(aborted).comparisons()...)
	if len(comparisons) != 37 {
		return nil, ErrInvalidMutation
	}
	return comparisons, nil
}

// A plain absence or Resolve's unverified response cannot authorize replacing
// an attempt. The original immutable, canonical aborted receipt must follow
// this journal snapshot and remain pinned by the replacement transaction.
func coldQuotaAttemptAbortedReceipt(journal, receipt *mvccpb.KeyValue, ref StageReference) error {
	if !enrolledImmutableKV(receipt) || len(receipt.Value) > coldQuotaAttemptMaxBytes || receipt.CreateRevision <= journal.ModRevision {
		return ErrCorruptReceipt
	}
	outcome, err := decodeReceipt(receipt, ref)
	wire, wireErr := encodeReceipt(ref, OutcomeAborted)
	if err != nil || wireErr != nil || outcome != OutcomeAborted || wire != string(receipt.Value) {
		return ErrCorruptReceipt
	}
	return nil
}

// resolveColdRuntimeQuotaAttempt never reconstructs the stored Stage. Resolve
// and a delayed original continuation compete through that same receipt.
// Unknown retains the original reference; committed needs the original return
// proof; only a reread authenticated aborted receipt permits fresh preparation.
func (b *Backend) resolveColdRuntimeQuotaAttempt(o *runtimeEnrolledPublicationOwner, history *coldQuotaReturnHistory, journal *mvccpb.KeyValue) (ColdRuntimeQuotaReturnResult, *mvccpb.KeyValue, error) {
	result := ColdRuntimeQuotaReturnResult{Outcome: OutcomeUnknown}
	ctx, err := b.coldQuotaReturnOwnerContext(o)
	if err != nil {
		return result, nil, err
	}
	record, err := b.decodeColdRuntimeQuotaAttempt(history, journal)
	if err != nil {
		return result, nil, err
	}
	result.Reference = record.Reference
	outcome, err := b.ResolveStage(ctx, record.Reference)
	if err != nil {
		return result, nil, err
	}
	if outcome == OutcomeAborted {
		_, receiptKey, err := b.stageKeys(record.Reference)
		if err != nil {
			return result, nil, err
		}
		points, err := b.readEnrolledPoints(ctx, []string{receiptKey}, []int{coldQuotaAttemptMaxBytes})
		if err != nil {
			return result, nil, err
		}
		if err := coldQuotaAttemptAbortedReceipt(journal, points[0], record.Reference); err != nil {
			return result, nil, err
		}
		result.Outcome = OutcomeAborted
		return result, points[0], nil
	}
	if outcome != OutcomeCommitted {
		return result, nil, ErrOutcomeUnknown
	}
	key, err := b.namespace.coldRuntimeQuotaReturnKey(record.Owner.Partition, record.Owner.IntentID)
	if err != nil {
		return result, nil, err
	}
	points, err := b.readEnrolledPoints(ctx, []string{key}, []int{16384})
	if err != nil {
		return result, nil, err
	}
	ref, entry, err := b.verifyColdQuotaReturn(ctx, history, points[0])
	if err != nil || ref != record.Reference {
		return result, nil, errors.Join(ErrCorruptRecord, err)
	}
	result.Outcome, result.Entry = OutcomeCommitted, entry
	return result, nil, nil
}
