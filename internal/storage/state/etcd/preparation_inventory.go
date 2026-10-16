package etcd

import (
	"context"
	"errors"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// InitializePreparationInventory is an explicit operator step. Application
// constructors never call it. Each call initializes at most eight fixed slots.
func (b *Backend) InitializePreparationInventory(ctx context.Context) (PreparationInventoryProgress, error) {
	digest, total, err := b.coldProfile()
	if err != nil || ctx == nil {
		return PreparationInventoryProgress{}, errors.Join(ErrInvalidRecord, err)
	}
	progressKey, _ := b.namespace.Key("preparation", "inventory", "progress")
	readyKey, _ := b.namespace.Key("preparation", "inventory", "ready")
	values, err := b.readDomain(ctx, progressKey, readyKey)
	if err != nil {
		return PreparationInventoryProgress{}, err
	}
	if values[1] != nil {
		return b.ReadPreparationInventory(ctx)
	}
	r := preparationInventoryRecord{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: digest, Total: total}
	cmps := append(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(readyKey), "=", 0))
	var writes []Write
	if values[0] == nil {
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(progressKey), "=", 0))
	} else {
		if coldDecode(values[0], &r) != nil || !b.validColdInventory(r) || r.Ready {
			return PreparationInventoryProgress{}, ErrCorruptRecord
		}
		cmps = append(cmps, coldComparisons(values[0])...)
		end := min(r.Next+8, total)
		for ordinal := r.Next; ordinal < end; ordinal++ {
			ref := b.coldInventorySlot(ordinal)
			slot := coldSlotRecord{Version: 1, RestoreEpoch: b.restoreEpoch, ProfileDigest: digest, Slot: ref}
			key, free, err := b.namespace.coldSlotKeys(ref)
			if err != nil {
				return PreparationInventoryProgress{}, err
			}
			wire, _ := coldEncode(slot)
			for _, k := range []string{key, free} {
				cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(k), "=", 0))
				writes = append(writes, Write{Key: k, Value: []byte(wire)})
			}
		}
		if r.Next == total {
			r.Ready = true
			wire, _ := coldEncode(r)
			writes = append(writes, Write{Key: readyKey, Value: []byte(wire)})
		}
		r.Next = end
	}
	wire, _ := coldEncode(r)
	writes = append(writes, Write{Key: progressKey, Value: []byte(wire)})
	_, err = b.coldCommit(ctx, cmps, writes, progressKey)
	if err != nil {
		return PreparationInventoryProgress{}, err
	}
	return PreparationInventoryProgress{Ready: r.Ready, Next: r.Next, Total: r.Total}, nil
}

func (b *Backend) ReadPreparationInventory(ctx context.Context) (PreparationInventoryProgress, error) {
	_, kv, err := b.coldReady(ctx)
	if err != nil {
		return PreparationInventoryProgress{}, err
	}
	var r preparationInventoryRecord
	if err := coldDecode(kv, &r); err != nil {
		return PreparationInventoryProgress{}, err
	}
	return PreparationInventoryProgress{Ready: true, Next: r.Next, Total: r.Total}, nil
}

func (b *Backend) validColdInventory(r preparationInventoryRecord) bool {
	return r.Version == 1 && r.RestoreEpoch == b.restoreEpoch && r.ProfileDigest == b.preparationDigest && r.Total == b.preparationTotal && r.Next <= r.Total && (!r.Ready || r.Next == r.Total)
}

func (b *Backend) coldReady(ctx context.Context) (string, *mvccpb.KeyValue, error) {
	if _, _, err := b.coldProfile(); err != nil {
		return "", nil, err
	}
	key, _ := b.namespace.Key("preparation", "inventory", "ready")
	values, err := b.readDomain(ctx, key)
	if err != nil {
		return "", nil, err
	}
	if values[0] == nil {
		return "", nil, ErrPreparationNotReady
	}
	var r preparationInventoryRecord
	if coldDecode(values[0], &r) != nil || !enrolledImmutableKV(values[0]) || !b.validColdInventory(r) || !r.Ready {
		return "", nil, ErrCorruptRecord
	}
	return key, values[0], nil
}

// Called only by the operator's bounded batch, never by allocation or Resume.
func (b *Backend) coldInventorySlot(ordinal uint64) ColdSlotReference {
	if ordinal < 32 {
		return ColdSlotReference{Kind: "preparation", ID: uint32(ordinal)}
	}
	ordinal -= 32
	for p, count := range b.preparationInventory.QuotaSlots {
		if ordinal < uint64(count) {
			return ColdSlotReference{Kind: "quota", Partition: uint8(p), ID: uint32(ordinal)}
		}
		ordinal -= uint64(count)
	}
	return ColdSlotReference{}
}

func coldComparisons(kv *mvccpb.KeyValue) []clientv3.Cmp {
	key := string(kv.Key)
	return []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", kv.ModRevision), clientv3.Compare(clientv3.Value(key), "=", string(kv.Value)), clientv3.Compare(clientv3.LeaseValue(key), "=", 0)}
}

// coldCommit is private to the closed preparation domain. It retains every
// identity/claim fence supplied by its producer and never attaches a Lease.
func (b *Backend) coldCommit(ctx context.Context, cmps []clientv3.Cmp, writes []Write, arbitration string) (int64, error) {
	failureKeys := []string{b.identityKey, b.restoreKey, arbitration}
	request := &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites(writes), Failure: taskProtoPoints(failureKeys)}
	maxFailure := taskProtoResponse(failureKeys, []string{b.identityValue, b.restoreEpoch, strings.Repeat("x", 16384)})
	maxSuccess := taskProtoPutResponse(len(writes))
	for i, w := range writes {
		if !validNamespaceKey(b.namespace, w.Key) || len(w.Value) > 16384 {
			return 0, ErrInvalidMutation
		}
		if w.Delete {
			maxSuccess.Responses[i] = &pb.ResponseOp{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: maxSuccess.Header, Deleted: 1}}}
		}
	}
	if err := taskCheckWire(b.namespace, []*pb.TxnRequest{request}, []*pb.TxnResponse{maxSuccess, maxFailure}); err != nil {
		return 0, err
	}
	ops := make([]clientv3.Op, 0, len(writes))
	for _, w := range writes {
		if w.Delete {
			ops = append(ops, clientv3.OpDelete(w.Key))
		} else {
			ops = append(ops, clientv3.OpPut(w.Key, string(w.Value)))
		}
	}
	bounded, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(bounded).If(cmps...).Then(ops...).Else(clientv3.OpGet(failureKeys[0]), clientv3.OpGet(failureKeys[1]), clientv3.OpGet(failureKeys[2])).Commit()
	err = errors.Join(err, bounded.Err())
	cancel()
	if err != nil {
		return 0, errors.Join(ErrOutcomeUnknown, err)
	}
	if b.operationResponseHeader(response) != nil || taskCheckWire(b.namespace, nil, []*pb.TxnResponse{(*pb.TxnResponse)(response)}) != nil {
		return 0, ErrOutcomeUnknown
	}
	if !response.Succeeded {
		if _, err := b.stageEvidencePoints(response, failureKeys); err != nil {
			return 0, err
		}
		return 0, ErrConflict
	}
	if len(response.Responses) != len(writes) {
		return 0, ErrOutcomeUnknown
	}
	for i, op := range response.Responses {
		if op == nil {
			return 0, ErrOutcomeUnknown
		}
		if writes[i].Delete {
			if op.GetResponseDeleteRange() == nil || op.GetResponseDeleteRange().Deleted != 1 {
				return 0, ErrOutcomeUnknown
			}
		} else if op.GetResponsePut() == nil {
			return 0, ErrOutcomeUnknown
		}
	}
	return response.Header.Revision, nil
}
