package etcd

import (
	"context"
	"errors"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type coldQuotaReturnHistory struct {
	reservation ColdPreparationRecord
	publication *coldPublicationHistory
	release     *localOwnerReleaseProof
	settlement  StageReference
	reservedKV  *mvccpb.KeyValue
	workKV      *mvccpb.KeyValue
	settledKV   *mvccpb.KeyValue
}

// SettleColdRuntimeQuota independently returns only the original runtime's
// occupied quota after authenticating its sealed safe-release history. A task
// reference is an address, not a termination or refund capability.
func (b *Backend) SettleColdRuntimeQuota(ctx context.Context, w WorkspaceIdentity, intent string, task TaskReference) (result ColdRuntimeQuotaReturnResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	o, err := b.registerEnrolledPublication(ctx)
	if err != nil {
		return result, err
	}
	defer o.finish()
	ctx = o.context
	if b.localRuntimeRelease.validate() != nil || b.validateTaskReference(task) != nil || b.validateWorkspaceBinding(w) != nil || task.Partition != w.Partition() {
		return result, ErrInvalidRecord
	}
	if _, _, err := b.coldProfile(); err != nil {
		return result, err
	}
	history, err := b.readColdQuotaReturnHistory(ctx, w, intent, task)
	if err != nil {
		return result, err
	}
	key, err := b.namespace.coldRuntimeQuotaReturnKey(w.Partition(), intent)
	if err != nil {
		return result, err
	}
	points, err := b.readEnrolledPoints(ctx, []string{key}, []int{16384})
	if err != nil {
		return result, err
	}
	if points[0] != nil {
		result.Reference, result.Entry, err = b.verifyColdQuotaReturn(ctx, history, points[0])
		if err == nil {
			result.Outcome = OutcomeCommitted
		}
		return result, err
	}
	_, ready, err := b.coldReady(ctx)
	if err != nil {
		return result, err
	}
	slotKey, free, err := b.namespace.coldSlotKeys(history.reservation.Quota)
	if err != nil {
		return result, err
	}
	slots, err := b.readEnrolledPoints(ctx, []string{slotKey, free}, []int{16384, 16384})
	if err != nil {
		return result, err
	}
	var slot coldSlotRecord
	if coldDecode(slots[0], &slot) != nil || !b.validColdSlot(slot) || !slot.Occupied || slot.Owner != history.reservation.Owner || slot.Slot != history.reservation.Quota || slots[1] != nil {
		return result, ErrConflict
	}
	stage, err := b.beginStageWithBuilder(ctx, w.Partition(), history.reservation.Owner.RequestID, "cold_runtime_quota_return", 10*time.Second, func(locator StageAttemptLocator) (Mutation, error) {
		mutation, err := b.coldQuotaReturnMutation(history, ready, slots[0], key, free, locator)
		if err != nil {
			return Mutation{}, err
		}
		_, digest, err := prepareMutation(b.namespace, mutation)
		if err == nil {
			// beginStageWithBuilder calls this before even its first Grant RPC.
			result.Reference = locator.reference(digest)
		}
		return mutation, err
	})
	if err != nil {
		return result, err
	}
	result.Reference = stage.Reference()
	defer func() { resultErr = errors.Join(resultErr, b.ReleaseStage(context.Background(), stage)) }()
	if _, _, err = b.coldQuotaReturnProjection(stage); err != nil {
		return result, err
	}
	result.Outcome, err = b.CommitStage(ctx, stage)
	if err != nil {
		return result, err
	}
	if result.Outcome != OutcomeCommitted {
		return result, ErrOutcomeUnknown
	}
	points, err = b.readEnrolledPoints(ctx, []string{key}, []int{16384})
	if err != nil {
		result.Outcome = OutcomeUnknown
		return result, err
	}
	ref, entry, err := b.verifyColdQuotaReturn(ctx, history, points[0])
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
	release, err := b.readLocalOwnerReleaseProof(ctx, task)
	if err != nil {
		return nil, err
	}
	if release == nil || !coldQuotaReturnOriginMatches(r, history.entry.Record, release) {
		return nil, ErrConflict
	}
	return &coldQuotaReturnHistory{reservation: r, publication: history, release: release, settlement: settlement, reservedKV: values[0], workKV: values[1], settledKV: receipts[0]}, nil
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
	r, err := decodeColdRuntimeQuotaReturn(kv)
	if err != nil || r != b.coldQuotaReturnRecord(h, r.Attempt) {
		return StageReference{}, nil, ErrCorruptRecord
	}
	key, err := b.runtimeDispatchReceiptKey(r.Attempt)
	if err != nil {
		return StageReference{}, nil, err
	}
	values, err := b.readEnrolledPoints(ctx, []string{key}, []int{4096})
	if err != nil {
		return StageReference{}, nil, err
	}
	if !enrolledImmutableKV(values[0]) || values[0].CreateRevision != kv.CreateRevision {
		return StageReference{}, nil, ErrCorruptReceipt
	}
	ref, err := decodeRuntimeDispatchReceipt(values[0], r.Attempt)
	if err != nil {
		return StageReference{}, nil, err
	}
	wire, err := encodeReceipt(ref, OutcomeCommitted)
	if err != nil || wire != string(values[0].Value) {
		return StageReference{}, nil, ErrCorruptReceipt
	}
	return ref, coldQuotaReturnEntry(r, kv), nil
}

func (b *Backend) coldQuotaReturnMutation(h *coldQuotaReturnHistory, ready, slotKV *mvccpb.KeyValue, key, free string, locator StageAttemptLocator) (Mutation, error) {
	r := b.coldQuotaReturnRecord(h, locator)
	wire, err := encodeColdRuntimeQuotaReturn(r)
	if err != nil {
		return Mutation{}, err
	}
	var slot coldSlotRecord
	if coldDecode(slotKV, &slot) != nil || !b.validColdSlot(slot) || !slot.Occupied || slot.Owner != r.Owner || slot.Slot != r.Quota {
		return Mutation{}, ErrConflict
	}
	slot.Occupied, slot.Owner = false, ColdSlotOwner{}
	freeWire, err := coldEncode(slot)
	if err != nil {
		return Mutation{}, err
	}
	var cmps []clientv3.Cmp
	for _, kv := range []*mvccpb.KeyValue{ready, h.publication.journal, h.publication.receipt, h.reservedKV, h.workKV, h.settledKV, h.release.kv, slotKV} {
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
