package etcd

import (
	"context"
	"errors"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type ColdPreparationSettlementResult struct {
	Outcome   Outcome
	Reference StageReference
	Entry     *RuntimeEnrolledPublicationEntry
}

// SettleColdPreparation requires authenticated monotonic completion; the
// original publication's CreationClaim cannot authorize this independent work.
func (b *Backend) SettleColdPreparation(ctx context.Context, w WorkspaceIdentity, intent string) (result ColdPreparationSettlementResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	o, err := b.registerEnrolledPublication(ctx)
	if err != nil {
		return result, err
	}
	defer o.finish()
	ctx = o.context
	history, err := b.loadColdPublicationHistory(ctx, w, intent)
	if err != nil {
		return result, err
	}
	if history == nil {
		return result, ErrConflict
	}
	r, err := b.LoadColdPreparation(ctx, w, intent)
	if err != nil {
		return result, err
	}
	if r == nil || !coldCompletionMatches(*r, history.entry.Record) {
		return result, ErrCorruptRecord
	}
	result.Entry = history.entry
	if r.Phase == "settled" {
		key, work, _ := b.namespace.coldPreparationKeys(w.Partition(), intent)
		values, err := b.readDomain(ctx, key, work)
		if err != nil {
			return result, err
		}
		var current ColdPreparationRecord
		if coldDecode(values[0], &current) != nil || current != *r {
			return result, ErrConflict
		}
		result.Reference, err = b.verifyColdSettlement(ctx, *r, values[0], values[1], history)
		if err != nil {
			return result, err
		}
		result.Outcome = OutcomeCommitted
		return result, nil
	}
	s, err := b.coldPendingSnapshot(ctx, w, intent)
	if err != nil {
		return result, err
	}
	if s.record != *r {
		return result, ErrConflict
	}
	_, ready, err := b.coldReady(ctx)
	if err != nil {
		return result, err
	}
	stage, err := b.beginStageWithBuilder(ctx, w.Partition(), r.Owner.RequestID, "cold_preparation_settle", 10*time.Second, func(locator StageAttemptLocator) (Mutation, error) {
		mutation, err := b.coldSettlementMutation(s, ready, history, locator)
		if err != nil {
			return Mutation{}, err
		}
		_, digest, err := prepareMutation(b.namespace, mutation)
		if err != nil {
			return Mutation{}, err
		}
		result.Reference = locator.reference(digest)
		return mutation, nil
	})
	if err != nil {
		return result, err
	}
	result.Reference = stage.Reference()
	defer func() { resultErr = errors.Join(resultErr, b.ReleaseStage(context.Background(), stage)) }()
	if err := b.coldSettlementBudget(stage); err != nil {
		return result, err
	}
	result.Outcome, err = b.CommitStage(ctx, stage)
	if err != nil {
		return result, err
	}
	if result.Outcome != OutcomeCommitted {
		return result, ErrOutcomeUnknown
	}
	return result, nil
}

func coldCompletionMatches(r ColdPreparationRecord, p RuntimeEnrolledPublicationRecord) bool {
	return r.Owner.IntentID == p.IntentID && r.Owner.SandboxID == p.SandboxID && r.Owner.WorkspaceHash == p.WorkspaceHash && r.Owner.Generation == p.Generation && r.Owner.RequestID == p.Attempt.RequestID && r.Owner.Partition == p.Attempt.Partition && r.RestoreEpoch == p.RestoreEpoch
}

func (b *Backend) coldSettlementMutation(s *coldPendingState, ready *mvccpb.KeyValue, history *coldPublicationHistory, locator StageAttemptLocator) (Mutation, error) {
	if s == nil || history == nil || !coldCompletionMatches(s.record, history.entry.Record) || locator.StageID != "cold_preparation_settle" || locator.Namespace != b.namespace.Root() || locator.RestoreEpoch != b.restoreEpoch || locator.Partition != s.record.Owner.Partition || locator.RequestID != s.record.Owner.RequestID {
		return Mutation{}, ErrInvalidMutation
	}
	var prep coldSlotRecord
	if coldDecode(s.slots[1], &prep) != nil || !b.validColdSlot(prep) || prep.Owner != s.record.Owner || prep.Slot != s.record.Preparation || !prep.Occupied {
		return Mutation{}, ErrCorruptRecord
	}
	prep.Occupied, prep.Owner = false, ColdSlotOwner{}
	freeWire, err := coldEncode(prep)
	if err != nil {
		return Mutation{}, err
	}
	_, free, err := b.namespace.coldSlotKeys(prep.Slot)
	if err != nil {
		return Mutation{}, err
	}
	r := s.record
	r.Phase, r.Settlement = "settled", locator
	wire, err := coldEncode(r)
	if err != nil {
		return Mutation{}, err
	}
	marker, err := coldEncode(coldPreparationWork{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, IntentID: r.Owner.IntentID, Phase: "settled"})
	if err != nil {
		return Mutation{}, err
	}
	var cmps []clientv3.Cmp
	for _, kv := range []*mvccpb.KeyValue{ready, history.journal, history.receipt, s.reservation, s.work, s.slots[0], s.slots[1]} {
		if kv == nil {
			return Mutation{}, ErrCorruptRecord
		}
		cmps = append(cmps, coldComparisons(kv)...)
	}
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(free), "=", 0))
	writes := []Write{{Key: string(s.slots[1].Key), Value: []byte(freeWire)}, {Key: free, Value: []byte(freeWire)}, {Key: string(s.reservation.Key), Value: []byte(wire)}, {Key: string(s.work.Key), Value: []byte(marker)}}
	if len(cmps) != 22 || len(writes) != 4 {
		return Mutation{}, ErrInvalidMutation
	}
	return Mutation{Comparisons: cmps, Writes: writes}, nil
}

// Check the exact native request, including all protocol and failure nodes.
// prepareMutation additionally reserves a conservative 40-node bound.
func (b *Backend) coldSettlementBudget(s *Stage) error {
	cmps := append(b.baseComparisons(), clientv3.Compare(clientv3.Value(s.guardKey), "=", s.guardValue), clientv3.Compare(clientv3.LeaseValue(s.guardKey), "=", int64(s.leaseID)), clientv3.Compare(clientv3.CreateRevision(s.guardKey), "=", s.guardRevision), clientv3.Compare(clientv3.CreateRevision(s.receiptKey), "=", 0))
	cmps = append(cmps, s.mutation.Comparisons...)
	writes := append(append([]Write(nil), s.mutation.Writes...), Write{Key: s.receiptKey, Value: []byte(s.committedValue)})
	failure := []string{b.identityKey, b.restoreKey, s.receiptKey, s.guardKey}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites(writes), Failure: taskProtoPoints(failure)}
	if len(request.Compare) != 30 || len(request.Success) != 5 || len(request.Failure) != 4 {
		return ErrInvalidMutation
	}
	return taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoPutResponse(5), taskProtoResponse(failure, []string{b.identityValue, b.restoreEpoch, s.committedValue, s.guardValue})})
}

func (b *Backend) verifyColdSettlement(ctx context.Context, r ColdPreparationRecord, reservation, work *mvccpb.KeyValue, history *coldPublicationHistory) (StageReference, error) {
	// Preserve source-error precedence and reject invalid history before the
	// bounded receipt RPC. The pure decoder shares these same invariants.
	if err := b.validateColdSettlementSource(r, reservation, work, history); err != nil {
		return StageReference{}, err
	}

	key, err := b.runtimeDispatchReceiptKey(r.Settlement)
	if err != nil {
		return StageReference{}, err
	}
	points, err := b.readEnrolledPoints(ctx, []string{key}, []int{4096})
	if err != nil {
		return StageReference{}, err
	}
	return b.decodeColdSettlement(r, reservation, work, history, points[0])
}
