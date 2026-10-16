package etcd

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (b *Backend) ReserveColdPreparation(ctx context.Context, c *CreationClaim, quotaPartition uint8) (ColdPreparationResult, error) {
	result := ColdPreparationResult{Outcome: OutcomeUnknown}
	if ctx == nil {
		return result, ErrInvalidRecord
	}
	if _, _, err := b.coldProfile(); err != nil {
		return result, err
	}
	if _, err := b.creationClaimComparisons(c); err != nil {
		return result, err
	}
	owner := coldClaimOwner(c)
	for attempt := 0; attempt < 4; attempt++ {
		existing, err := b.LoadColdPreparation(ctx, c.workspace, c.reference.IntentID)
		if err != nil {
			return result, err
		}
		if existing != nil {
			if existing.Owner != owner || existing.Quota.Partition != quotaPartition {
				return result, ErrIdempotencyConflict
			}
			return ColdPreparationResult{Outcome: OutcomeCommitted, Replay: true, Record: existing}, nil
		}
		_, ready, err := b.coldReady(ctx)
		if err != nil {
			return result, err
		}
		if b.preparationInventory.QuotaSlots[quotaPartition] == 0 {
			return result, ErrPreparationCapacity
		}
		prepPrefix, _ := b.namespace.Key("preparation", "free")
		prep, err := b.coldFreeCandidate(ctx, prepPrefix+"/")
		if err != nil {
			return result, err
		}
		if prep == nil {
			return result, ErrPreparationCapacity
		}
		var quota *mvccpb.KeyValue
		// All sixteen fixed buckets must be observed empty before capacity is
		// reported. A bucket miss alone never declares the partition exhausted.
		for bucket := 0; bucket < 16; bucket++ {
			prefix, _ := b.namespace.Key("quota", fmt.Sprintf("%02x", quotaPartition), "free", fmt.Sprintf("%02x", bucket))
			quota, err = b.coldFreeCandidate(ctx, prefix+"/")
			if err != nil {
				return result, err
			}
			if quota != nil {
				break
			}
		}
		if quota == nil {
			return result, ErrPreparationCapacity
		}
		reservation, cmps, writes, err := b.coldReservationMutation(ctx, c, quotaPartition, ready, quota, prep)
		if err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}
			return result, err
		}
		key, _, _ := b.namespace.coldPreparationKeys(owner.Partition, owner.IntentID)
		if _, err = b.coldCommit(ctx, cmps, writes, key); err != nil {
			if errors.Is(err, ErrConflict) {
				continue
			}
			return result, err
		}
		return ColdPreparationResult{Outcome: OutcomeCommitted, Record: &reservation}, nil
	}
	return result, ErrConflict
}

func (b *Backend) LoadColdPreparation(ctx context.Context, w WorkspaceIdentity, intent string) (*ColdPreparationRecord, error) {
	if _, _, err := b.coldProfile(); err != nil {
		return nil, err
	}
	if err := b.validateWorkspaceBinding(w); err != nil {
		return nil, err
	}
	key, work, err := b.namespace.coldPreparationKeys(w.Partition(), intent)
	if err != nil {
		return nil, err
	}
	values, err := b.readDomain(ctx, key, work)
	if err != nil {
		return nil, err
	}
	if values[0] == nil {
		if values[1] != nil {
			return nil, ErrCorruptRecord
		}
		return nil, nil
	}
	var r ColdPreparationRecord
	var marker coldPreparationWork
	if coldDecode(values[0], &r) != nil || coldDecode(values[1], &marker) != nil || !b.validColdReservation(r, w, intent) || marker != (coldPreparationWork{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, IntentID: intent, Phase: r.Phase}) {
		return nil, ErrCorruptRecord
	}
	if r.Phase == "settled" {
		history, err := b.loadColdPublicationHistory(ctx, w, intent)
		if err != nil {
			return nil, err
		}
		if _, err := b.verifyColdSettlement(ctx, r, values[0], values[1], history); err != nil {
			return nil, err
		}
		// The original prep and quota may already be safely returned and reused.
		// The authenticated reservation is history, never current slot authority.
		return &r, nil
	}
	for _, ref := range []ColdSlotReference{r.Quota, r.Preparation} {
		slotKey, freeKey, err := b.namespace.coldSlotKeys(ref)
		if err != nil {
			return nil, err
		}
		points, err := b.readDomain(ctx, slotKey, freeKey)
		if err != nil {
			return nil, err
		}
		var slot coldSlotRecord
		if coldDecode(points[0], &slot) != nil || !b.validColdSlot(slot) || !slot.Occupied || slot.Owner != r.Owner || slot.Slot != ref || points[1] != nil {
			return nil, ErrCorruptRecord
		}
	}
	return &r, nil
}

func coldClaimOwner(c *CreationClaim) ColdSlotOwner {
	return ColdSlotOwner{IntentID: c.reference.IntentID, SandboxID: c.reference.SandboxID, WorkspaceHash: c.workspace.Hash(), RequestID: c.request.RequestID, Generation: c.reference.Generation, Partition: c.reference.Partition}
}

func (b *Backend) validColdSlot(s coldSlotRecord) bool {
	if s.Version != 1 || s.RestoreEpoch != b.restoreEpoch || s.ProfileDigest != b.preparationDigest {
		return false
	}
	ref := s.Slot
	if ref.Kind == "preparation" {
		if ref.Partition != 0 || ref.ID >= 32 {
			return false
		}
	} else if ref.Kind != "quota" || ref.ID >= b.preparationInventory.QuotaSlots[ref.Partition] {
		return false
	}
	if !s.Occupied {
		return s.Owner == (ColdSlotOwner{})
	}
	return ref.Epoch > 0 && validDomainSegment(s.Owner.IntentID) && validDomainSegment(s.Owner.SandboxID) && validDomainSegment(s.Owner.RequestID) && validHexDigest(s.Owner.WorkspaceHash) && s.Owner.Generation > 0
}

func (b *Backend) validColdReservation(r ColdPreparationRecord, w WorkspaceIdentity, intent string) bool {
	phase := r.Phase == "reserved" && r.Settlement == (StageAttemptLocator{}) || r.Phase == "settled" && r.Settlement.Validate() == nil && r.Settlement.Namespace == b.namespace.Root() && r.Settlement.RestoreEpoch == b.restoreEpoch && r.Settlement.Partition == w.Partition() && r.Settlement.RequestID == r.Owner.RequestID && r.Settlement.StageID == "cold_preparation_settle"
	return phase && r.Version == 1 && r.RestoreEpoch == b.restoreEpoch && r.ProfileDigest == b.preparationDigest && r.Owner.IntentID == intent && r.Owner.WorkspaceHash == w.Hash() && r.Owner.Partition == w.Partition() && r.Owner.Generation > 0 && validDomainSegment(r.Owner.RequestID) && validDomainSegment(r.Owner.SandboxID) && r.Quota.Kind == "quota" && r.Quota.ID < b.preparationInventory.QuotaSlots[r.Quota.Partition] && r.Quota.Epoch > 0 && r.Preparation.Kind == "preparation" && r.Preparation.Partition == 0 && r.Preparation.ID < 32 && r.Preparation.Epoch > 0
}

func (b *Backend) coldFreeCandidate(ctx context.Context, prefix string) (*mvccpb.KeyValue, error) {
	if !strings.HasSuffix(prefix, "/") || !validNamespaceKey(b.namespace, strings.TrimSuffix(prefix, "/")) {
		return nil, ErrInvalidRecord
	}
	op := clientv3.OpGet(prefix, clientv3.WithPrefix(), clientv3.WithLimit(1))
	request := &pb.TxnRequest{Compare: taskProtoComparisons(b.baseComparisons()), Success: []*pb.RequestOp{{Request: &pb.RequestOp_RequestRange{RequestRange: &pb.RangeRequest{Key: []byte(prefix), RangeEnd: []byte(clientv3.GetPrefixRangeEnd(prefix)), Limit: 1}}}}, Failure: taskProtoPoints([]string{b.identityKey, b.restoreKey})}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{taskProtoResponse([]string{prefix + "00000000"}, []string{strings.Repeat("x", 16384)}), taskProtoResponse([]string{b.identityKey, b.restoreKey}, []string{b.identityValue, b.restoreEpoch})}); err != nil {
		return nil, err
	}
	bounded, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(bounded).If(b.baseComparisons()...).Then(op).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	err = errors.Join(err, bounded.Err())
	cancel()
	if err != nil {
		return nil, err
	}
	if b.operationResponseHeader(response) != nil || taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(response)}) != nil {
		return nil, ErrCorruptRecord
	}
	if !response.Succeeded {
		if _, err := b.stageEvidencePoints(response, []string{b.identityKey, b.restoreKey}); err != nil {
			return nil, err
		}
		return nil, ErrIdentityMismatch
	}
	if len(response.Responses) != 1 || response.Responses[0] == nil || response.Responses[0].GetResponseRange() == nil {
		return nil, ErrCorruptRecord
	}
	r := response.Responses[0].GetResponseRange()
	if len(r.Kvs) > 1 || r.Count < int64(len(r.Kvs)) || len(r.Kvs) == 0 && (r.More || r.Count != 0) {
		return nil, ErrCorruptRecord
	}
	if len(r.Kvs) == 0 {
		return nil, nil
	}
	if r.Kvs[0] == nil || !strings.HasPrefix(string(r.Kvs[0].Key), prefix) || len(r.Kvs[0].Value) > 16384 {
		return nil, ErrCorruptRecord
	}
	return r.Kvs[0], nil
}

func (b *Backend) coldReservationMutation(ctx context.Context, c *CreationClaim, quotaPartition uint8, ready, quota, prep *mvccpb.KeyValue) (ColdPreparationRecord, []clientv3.Cmp, []Write, error) {
	var zero ColdPreparationRecord
	claim, err := b.creationClaimComparisons(c)
	if err != nil {
		return zero, nil, nil, err
	}
	owner := coldClaimOwner(c)
	cmps := append(b.baseComparisons(), claim...)
	cmps = append(cmps, coldComparisons(ready)...)
	var refs [2]ColdSlotReference
	var writes []Write
	for i, candidate := range []*mvccpb.KeyValue{quota, prep} {
		var free coldSlotRecord
		if coldDecode(candidate, &free) != nil || !b.validColdSlot(free) || free.Occupied || free.Slot.Epoch == math.MaxUint64 || i == 0 && (free.Slot.Kind != "quota" || free.Slot.Partition != quotaPartition) || i == 1 && free.Slot.Kind != "preparation" {
			return zero, nil, nil, ErrCorruptRecord
		}
		key, freeKey, err := b.namespace.coldSlotKeys(free.Slot)
		if err != nil || string(candidate.Key) != freeKey {
			return zero, nil, nil, ErrCorruptRecord
		}
		points, err := b.readDomain(ctx, key, freeKey)
		if err != nil {
			return zero, nil, nil, err
		}
		if points[1] == nil || points[1].ModRevision != candidate.ModRevision || string(points[1].Value) != string(candidate.Value) {
			return zero, nil, nil, ErrConflict
		}
		var slot coldSlotRecord
		if coldDecode(points[0], &slot) != nil || slot != free {
			return zero, nil, nil, ErrCorruptRecord
		}
		cmps = append(cmps, coldComparisons(points[0])...)
		cmps = append(cmps, coldComparisons(points[1])...)
		slot.Slot.Epoch++
		slot.Occupied, slot.Owner = true, owner
		refs[i] = slot.Slot
		wire, _ := coldEncode(slot)
		writes = append(writes, Write{Key: key, Value: []byte(wire)}, Write{Key: freeKey, Delete: true})
	}
	r := ColdPreparationRecord{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, Owner: owner, Quota: refs[0], Preparation: refs[1], Phase: "reserved"}
	key, work, _ := b.namespace.coldPreparationKeys(owner.Partition, owner.IntentID)
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0), clientv3.Compare(clientv3.CreateRevision(work), "=", 0))
	wire, _ := coldEncode(r)
	marker, _ := coldEncode(coldPreparationWork{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: b.preparationDigest, IntentID: owner.IntentID, Phase: "reserved"})
	writes = append(writes, Write{Key: key, Value: []byte(wire)}, Write{Key: work, Value: []byte(marker)})
	if len(cmps) != 45 || len(writes) != 6 {
		return zero, nil, nil, ErrInvalidMutation
	}
	return r, cmps, writes, nil
}
