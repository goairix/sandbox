package etcd

import (
	"context"
	"errors"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type coldQuotaReturnHistory struct {
	reservation ColdPreparationRecord
	publication *coldPublicationHistory
	release     *localOwnerReleaseProof

	// Exactly one release variant is available after full authentication.
	// V1 retains its original field; only B's detached union may occupy this
	// second branch. Cursors/permits borrow one shared graph rather than clones.
	profileRelease *taskOwnerReleaseProof

	settlement StageReference
	reservedKV *mvccpb.KeyValue
	workKV     *mvccpb.KeyValue
	settledKV  *mvccpb.KeyValue
}

// SettleColdRuntimeQuota independently returns only the original runtime's
// occupied quota after authenticating its sealed safe-release history. A task
// reference is an address, not a termination or refund capability. Fresh calls
// persist the original attempt before Grant; recovery arbitrates that pending
// reference and can replace it only after authenticating its aborted receipt.
func (b *Backend) SettleColdRuntimeQuota(ctx context.Context, w WorkspaceIdentity, intent string, task TaskReference) (result ColdRuntimeQuotaReturnResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	o, err := b.registerEnrolledPublication(ctx)
	if err != nil {
		return result, err
	}
	defer o.finish()

	history, key, result, err := b.authenticateColdRuntimeQuotaReturn(o, w, intent, task)
	if err != nil || history == nil {
		return result, err
	}
	journalKey, err := b.namespace.coldRuntimeQuotaAttemptKey(w.Partition(), intent)
	if err != nil {
		return result, err
	}
	points, err := b.readEnrolledPoints(o.context, []string{journalKey}, []int{coldQuotaAttemptMaxBytes})
	if err != nil {
		return result, err
	}
	var aborted *mvccpb.KeyValue
	if points[0] != nil {
		result, aborted, err = b.resolveColdRuntimeQuotaAttempt(o, history, points[0])
		if err != nil || result.Outcome != OutcomeAborted {
			return result, err
		}
	}

	prepared, result, err := b.prepareColdRuntimeQuotaReturnHistory(o, history, key)
	if err != nil || prepared == nil {
		return result, err
	}
	// One CAS per public call: a losing candidate is burned and returned for
	// diagnostics. A later call may arbitrate the durable winner, but this call
	// never loops into another candidate or reconstructs its original Stage.
	return b.journalAndContinueColdRuntimeQuotaReturn(o, prepared, points[0], aborted)
}

// commitColdRuntimeQuotaReturn retains the quota protocol's exact projection,
// permanent transaction and post-commit verification. Cleanup revokes only the
// original guard after all reads join; uncertainty never replaces its reference.
func (b *Backend) commitColdRuntimeQuotaReturn(ctx context.Context, prepared *coldQuotaReturnPrepared, stage *Stage) (result ColdRuntimeQuotaReturnResult, resultErr error) {
	result.Outcome, result.Reference = OutcomeUnknown, prepared.reference
	defer func() { resultErr = errors.Join(resultErr, b.ReleaseStage(context.Background(), stage)) }()
	if _, _, err := b.coldQuotaReturnProjection(stage); err != nil {
		return result, err
	}

	var err error
	result.Outcome, err = b.CommitStage(ctx, stage)
	if err != nil {
		return result, err
	}
	if result.Outcome != OutcomeCommitted {
		return result, ErrOutcomeUnknown
	}

	points, err := b.readEnrolledPoints(ctx, []string{prepared.returnKey}, []int{16384})
	if err != nil {
		result.Outcome = OutcomeUnknown
		return result, err
	}
	ref, entry, err := b.verifyColdQuotaReturn(ctx, prepared.history, points[0])
	if err != nil {
		result.Outcome = OutcomeUnknown
	} else {
		result.Reference, result.Entry = ref, entry
	}
	return result, err
}

func (b *Backend) readColdQuotaReturnHistory(ctx context.Context, w WorkspaceIdentity, intent string, task TaskReference) (*coldQuotaReturnHistory, error) {
	history, err := b.loadColdPublicationHistory(ctx, w, intent)
	if err != nil {
		return nil, err
	}
	if history == nil {
		return nil, ErrConflict
	}
	key, work, err := b.namespace.coldPreparationKeys(w.Partition(), intent)
	if err != nil {
		return nil, err
	}
	values, err := b.readEnrolledPoints(ctx, []string{key, work}, []int{16384, 16384})
	if err != nil {
		return nil, err
	}
	var r ColdPreparationRecord
	if coldDecode(values[0], &r) != nil || !b.validColdReservation(r, w, intent) || r.Phase != "settled" || !coldCompletionMatches(r, history.entry.Record) {
		return nil, ErrConflict
	}
	settlement, err := b.verifyColdSettlement(ctx, r, values[0], values[1], history)
	if err != nil {
		return nil, err
	}
	receiptKey, err := b.runtimeDispatchReceiptKey(r.Settlement)
	if err != nil {
		return nil, err
	}
	receipts, err := b.readEnrolledPoints(ctx, []string{receiptKey}, []int{4096})
	if err != nil {
		return nil, err
	}
	wire, err := encodeReceipt(settlement, OutcomeCommitted)
	if err != nil || !enrolledImmutableKV(receipts[0]) || receipts[0].CreateRevision != values[0].ModRevision || string(receipts[0].Value) != wire {
		return nil, ErrCorruptReceipt
	}
	release, err := b.readTaskOwnerReleaseProof(ctx, task, nil)
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, ErrConflict
	}
	result := &coldQuotaReturnHistory{reservation: r, publication: history, settlement: settlement, reservedKV: values[0], workKV: values[1], settledKV: receipts[0]}
	if release.local != nil && release.detached == nil {
		result.release = release.local
	} else {
		result.profileRelease = release
	}
	if !coldQuotaReturnHistoryOriginMatches(result) {
		return nil, ErrConflict
	}
	return result, nil
}

func coldQuotaReturnOriginMatches(r ColdPreparationRecord, publication RuntimeEnrolledPublicationRecord, release *localOwnerReleaseProof) bool {
	if release == nil || !coldCompletionMatches(r, publication) {
		return false
	}
	t, task := release.termination, release.record.Task
	return task.Namespace == publication.Attempt.Namespace && task.RestoreEpoch == publication.RestoreEpoch && task.Partition == r.Owner.Partition && task.SandboxID == r.Owner.SandboxID && t.CreationIntentID == r.Owner.IntentID && t.Current.WorkspaceHash == r.Owner.WorkspaceHash && t.Current.Generation == r.Owner.Generation && t.Current.DataGateEpoch == publication.DataGateEpoch && RuntimeReference(t.Current.Runtime) == publication.Runtime && t.NodeUID == publication.NodeUID && t.NodeBootID == publication.NodeBootID && t.EnrollmentDigest == publication.EnrollmentDigest && t.ObserverCertificateDigest == publication.ObserverCertificateDigest && t.Phase == "cleanup-complete"
}

func (b *Backend) coldQuotaReturnRecord(h *coldQuotaReturnHistory, locator StageAttemptLocator) coldRuntimeQuotaReturnRecord {
	r, pub := h.reservation, h.publication.entry
	return coldRuntimeQuotaReturnRecord{Version: 1, Domain: "sandbox.cold-runtime-quota-return.v1", Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Owner: r.Owner, Quota: r.Quota, Preparation: r.Preparation, Runtime: pub.Record.Runtime, NodeUID: pub.Record.NodeUID, NodeBootID: pub.Record.NodeBootID, DataGateEpoch: pub.Record.DataGateEpoch, Publication: pub.Reference, PublicationStamp: localRuntimeStamp(h.publication.journal), Reservation: localRuntimeStamp(h.reservedKV), PreparationWork: localRuntimeStamp(h.workKV), PreparationSettlement: h.settlement, Release: h.release.record, ReleaseStamp: localRuntimeStamp(h.release.kv), Attempt: locator}
}

func (b *Backend) verifyColdQuotaReturn(ctx context.Context, h *coldQuotaReturnHistory, kv *mvccpb.KeyValue) (StageReference, *ColdRuntimeQuotaReturnEntry, error) {
	stamp, err := b.decodeColdQuotaReturnProof(h, kv)
	if err != nil {
		return StageReference{}, nil, err
	}
	key, err := b.runtimeDispatchReceiptKey(stamp.attempt)
	if err != nil {
		return StageReference{}, nil, err
	}
	values, err := b.readEnrolledPoints(ctx, []string{key}, []int{4096})
	if err != nil {
		return StageReference{}, nil, err
	}
	return b.decodeColdQuotaReturnReceipt(stamp, values[0])
}

func (b *Backend) coldQuotaReturnMutation(h *coldQuotaReturnHistory, ready, slotKV *mvccpb.KeyValue, key, free string, locator StageAttemptLocator) (Mutation, error) {
	wire, err := b.coldQuotaReturnWire(h, locator)
	if err != nil {
		return Mutation{}, err
	}
	var slot coldSlotRecord
	if coldDecode(slotKV, &slot) != nil || !b.validColdSlot(slot) || !slot.Occupied || slot.Owner != h.reservation.Owner || slot.Slot != h.reservation.Quota {
		return Mutation{}, ErrConflict
	}
	slot.Occupied, slot.Owner = false, ColdSlotOwner{}
	freeWire, err := coldEncode(slot)
	if err != nil {
		return Mutation{}, err
	}
	sources, err := coldQuotaReturnHistorySources(h)
	if err != nil {
		return Mutation{}, err
	}
	var cmps []clientv3.Cmp
	points := append(append([]*mvccpb.KeyValue{ready}, sources[:]...), slotKV)
	for _, kv := range points {
		if kv == nil {
			return Mutation{}, ErrCorruptRecord
		}
		cmps = append(cmps, coldComparisons(kv)...)
	}
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(free), "=", 0), clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	return Mutation{Comparisons: cmps, Writes: []Write{{Key: string(slotKV.Key), Value: []byte(freeWire)}, {Key: free, Value: []byte(freeWire)}, {Key: key, Value: []byte(wire)}}}, nil
}

func (b *Backend) coldQuotaReturnProjection(s *Stage) (*pb.TxnRequest, []*pb.TxnResponse, error) {
	cmps := append(b.baseComparisons(), clientv3.Compare(clientv3.Value(s.guardKey), "=", s.guardValue), clientv3.Compare(clientv3.LeaseValue(s.guardKey), "=", int64(s.leaseID)), clientv3.Compare(clientv3.CreateRevision(s.guardKey), "=", s.guardRevision), clientv3.Compare(clientv3.CreateRevision(s.receiptKey), "=", 0))
	cmps = append(cmps, s.mutation.Comparisons...)
	writes := append(append([]Write(nil), s.mutation.Writes...), Write{Key: s.receiptKey, Value: []byte(s.committedValue)})
	failure := []string{b.identityKey, b.restoreKey, s.receiptKey, s.guardKey}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites(writes), Failure: taskProtoPoints(failure)}
	responses := []*pb.TxnResponse{taskProtoPutResponse(4), taskProtoResponse(failure, []string{b.identityValue, b.restoreEpoch, s.committedValue, s.guardValue})}
	if len(request.Compare) != 34 || len(request.Success) != 4 || len(request.Failure) != 4 {
		return nil, nil, ErrInvalidMutation
	}
	return request, responses, taskCheckWire(b.namespace, []*pb.TxnRequest{request}, responses)
}
