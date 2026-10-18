package etcd

import (
	"math"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// All32 candidates have two-byte native slot keys and20-digit selections.
// JSON Slot is widest for10..31; every other coordinate has identical width.
// Select a witness in that range rather than retaining32 projection graphs.
func (d *cleanupDetachedWireBuilder) widestExecution() cleanupExecutionRecord {
	r := d.execution
	r.Selection = math.MaxUint64
	r.Slot = cleanupCandidate(r.TaskDigest, r.Selection)
	for r.Slot < 10 {
		r.Selection--
		r.Slot = cleanupCandidate(r.TaskDigest, r.Selection)
	}
	return r
}

func (d *cleanupDetachedWireBuilder) executionKeys() (string, string, string, error) {
	e, err := d.b.namespace.cleanupExecutionKey(d.execution.Task)
	if err != nil {
		return "", "", "", err
	}
	s, err := d.b.namespace.cleanupExecutionSlotKey(d.widestExecution().Slot)
	if err != nil {
		return "", "", "", err
	}
	_, receipt, err := d.b.stageKeys(d.in.bundle.initial.reference(cleanupWirePaddingDigest()))
	return e, s, receipt, err
}

func (d *cleanupDetachedWireBuilder) workerDimensions() ([]clientv3.Cmp, error) {
	b := d.b
	profile, err := b.namespace.Key("meta", "worker_control_budget")
	if err != nil {
		return nil, err
	}
	session, err := b.namespace.workerSessionKey(d.w.dimensions.runID)
	if err != nil {
		return nil, err
	}
	partition, err := b.namespace.workerPartitionKey(d.execution.Task.Partition)
	if err != nil {
		return nil, err
	}
	share, err := b.namespace.workerBudgetKey(WorkerBudgetClaim, 99)
	if err != nil {
		return nil, err
	}
	cmps := b.baseComparisons()
	for i, key := range []string{profile, session, partition, share} {
		f := cleanupWireFence(key, workerRecordBytes)
		if i > 0 {
			f.lease = math.MaxInt64
		}
		cmps = append(cmps, f.comparisons()...)
	}
	return cmps, nil
}

func (d *cleanupDetachedWireBuilder) executionProjection(variant string) (cleanupWireProjection, error) {
	b := d.b
	eKey, sKey, receipt, err := d.executionKeys()
	if err != nil {
		return cleanupWireProjection{}, err
	}
	r := d.widestExecution()
	initial, err := encodeCleanupExecution(d.execution)
	if err != nil {
		return cleanupWireProjection{}, err
	}
	selecting, err := encodeCleanupExecution(r)
	if err != nil {
		return cleanupWireProjection{}, err
	}
	r.Phase, r.Confirmed = "occupied", "reserved"
	occupied, err := encodeCleanupExecution(r)
	if err != nil {
		return cleanupWireProjection{}, err
	}
	slot, err := encodeCleanupExecutionSlot(cleanupSlotRecordFromExecution(r))
	if err != nil {
		return cleanupWireProjection{}, err
	}
	r.Run = &cleanupExecutionRun{ID: d.w.dimensions.runID, Worker: d.w.dimensions.worker}
	r.Pending = &cleanupExecutionPending{Purpose: "claim", ClaimID: d.w.claim.ClaimID}
	_, err = encodeCleanupExecution(r)
	if err != nil {
		return cleanupWireProjection{}, err
	}
	// Later progress may retain another legal Pending purpose or its maximum
	// Stage coordinates. Whole native E/S caps bound those branches, while the
	// canonical records above independently validate the detached state schema.
	e := cleanupWireFence(eKey, cleanupExecutionBytes)
	s := cleanupWireFence(sKey, cleanupExecutionBytes)
	point := func(keys, values []string) cleanupWireProjection {
		return b.cleanupProjection(variant, b.baseComparisons(), taskProtoPoints(keys), taskProtoResponse(keys, values))
	}
	switch variant {
	case "point":
		return point([]string{eKey}, []string{cleanupWirePadding(4096)}), nil
	case "observe":
		return point([]string{eKey, sKey}, []string{cleanupWirePadding(4096), cleanupWirePadding(4096)}), nil
	case "claim-observe":
		return point([]string{eKey, sKey, receipt}, []string{cleanupWirePadding(4096), cleanupWirePadding(4096), cleanupWirePadding(4096)}), nil
	case "bundle":
		keys, values := make([]string, 0, 10), make([]string, 0, 10)
		for _, f := range d.in.bundle.fences {
			keys, values = append(keys, f.key), append(values, cleanupWirePadding(4096))
		}
		keys, values = append(keys, string(d.in.bundle.checkpoint.Key), d.w.claimKey), append(values, cleanupWirePadding(4096), cleanupWirePadding(4096))
		return point(keys, values), nil
	case "terminal-history":
		return point([]string{eKey, d.w.terminationKey, d.w.providerKey, d.w.releaseKey}, []string{cleanupWirePadding(4096), cleanupWirePadding(16384), cleanupWirePadding(16384), cleanupWirePadding(8192)}), nil
	case "claim-postcheck":
		return d.w.guard(cleanupWireAppend(b.baseComparisons(), d.w.live...), false), nil
	}
	worker, err := d.workerDimensions()
	if err != nil {
		return cleanupWireProjection{}, err
	}
	tir := make([]clientv3.Cmp, 0, 12)
	for _, f := range []taskFence{d.in.bundle.fences[0], d.in.bundle.fences[1], cleanupWireFence(receipt, 4096)} {
		tir = append(tir, cleanupMaxFence(f).comparisons()...)
	}
	put := func(key, value string) []*pb.RequestOp { return []*pb.RequestOp{taskProtoPut(key, value, 0)} }
	cmps := cleanupWireAppend(worker, tir...)
	switch variant {
	case "initialize":
		return b.cleanupProjection(variant, cleanupWireAppend(cmps, clientv3.Compare(clientv3.CreateRevision(eKey), "=", 0)), put(eKey, initial), taskProtoPutResponse(1)), nil
	case "rotate":
		f := taskFence{key: eKey, value: selecting, create: math.MaxInt64, mod: math.MaxInt64}
		return b.cleanupProjection(variant, cleanupWireAppend(worker, f.comparisons()...), put(eKey, selecting), taskProtoPutResponse(1)), nil
	case "reserve":
		f := taskFence{key: eKey, value: selecting, create: math.MaxInt64, mod: math.MaxInt64}
		cmps = cleanupWireAppend(cmps, f.comparisons()...)
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(sKey), "=", 0))
		return b.cleanupProjection(variant, cmps, []*pb.RequestOp{taskProtoPut(sKey, slot, 0), taskProtoPut(eKey, occupied, 0)}, taskProtoPutResponse(2)), nil
	case "progress", "joined":
		if variant == "joined" {
			r.Run.Joined = true
		}
		_, err = encodeCleanupExecution(r)
		if err != nil {
			return cleanupWireProjection{}, err
		}
		cmps = cleanupWireAppend(worker, e.comparisons()...)
		cmps = append(cmps, s.comparisons()...)
		if variant == "joined" {
			cmps = append(b.baseComparisons(), cmps[20:]...)
		}
		return b.cleanupProjection(variant, cmps, put(eKey, cleanupWirePadding(cleanupExecutionBytes)), taskProtoPutResponse(1)), nil
	case "claim":
		mutation := taskClaimCreationMutation(d.in.bundle.fences, ownTaskFence(d.in.bundle.checkpoint), d.w.claimKey, d.w.guardKey, d.w.claimValue)
		cmps = cleanupWireAppend(b.baseComparisons(), mutation.Comparisons...)
		cmps = append(cmps, worker[4:]...)
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(eKey), "=", math.MaxInt64), clientv3.Compare(clientv3.ModRevision(sKey), "=", math.MaxInt64))
		return b.cleanupProjection(variant, cmps, []*pb.RequestOp{taskProtoPut(d.w.claimKey, d.w.claimValue, math.MaxInt64), taskProtoPut(d.w.guardKey, d.w.claimValue, math.MaxInt64)}, taskProtoPutResponse(2)), nil
	case "settle":
		cmps = append(cmps, e.comparisons()...)
		cmps = append(cmps, s.comparisons()...)
		cmps = append(cmps, cleanupWireFence(d.w.releaseKey, 8192).comparisons()...)
		r.Run.Joined, r.Confirmed, r.Phase, r.Pending = true, "settled", "settled", nil
		r.Release = &cleanupExecutionRelease{CreateRevision: math.MaxInt64 - 1, Digest: cleanupWirePaddingDigest()}
		value, err := encodeCleanupExecution(r)
		if err != nil {
			return cleanupWireProjection{}, err
		}
		response := taskProtoPutResponse(1)
		response.Responses = append([]*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseDeleteRange{ResponseDeleteRange: &pb.DeleteRangeResponse{Header: response.Header, Deleted: 1}}}}, response.Responses...)
		return b.cleanupProjection(variant, cmps, taskProtoWrites([]Write{{Key: sKey, Delete: true}, {Key: eKey, Value: []byte(value)}}), response), nil
	}
	return cleanupWireProjection{}, ErrInvalidRecord
}

func (d *cleanupDetachedWireBuilder) noEffectSource() ([]string, []string, error) {
	e, s, _, err := d.executionKeys()
	if err != nil {
		return nil, nil, err
	}
	keys, err := d.sourceKeys()
	if err != nil {
		return nil, nil, err
	}
	return []string{d.b.identityKey, d.b.restoreKey, e, s, d.w.finalKey, keys[2], keys[4], keys[5], d.factKey}, []string{d.b.identityValue, d.b.restoreEpoch, cleanupWirePadding(4096), cleanupWirePadding(4096), cleanupWirePadding(8192), cleanupWirePadding(65536), cleanupWirePadding(4096), cleanupWirePadding(8192), cleanupWirePadding(32768)}, nil
}

func (d *cleanupDetachedWireBuilder) noEffectProjection(variant string) (cleanupWireProjection, error) {
	b, w := d.b, &d.w
	keys, values, err := d.noEffectSource()
	if err != nil {
		return cleanupWireProjection{}, err
	}
	switch variant {
	case "source":
		return b.detachedNoEffectPointProjection(variant, keys, values)
	case "arbitration":
		return b.detachedNoEffectPointProjection(variant, []string{b.identityKey, b.restoreKey, d.factKey}, []string{b.identityValue, b.restoreEpoch, cleanupWirePadding(32768)})
	case "locate":
		return cleanupWirePointProjection(b.baseComparisons(), []string{b.identityKey, b.restoreKey, d.in.bundle.fences[0].key, keys[2]}, []string{b.identityValue, b.restoreEpoch, cleanupWirePadding(4096), cleanupWirePadding(4096)}), nil
	case "quiescence":
		return cleanupWirePointProjection(b.baseComparisons(), []string{b.identityKey, b.restoreKey, w.quiescenceKey, w.quiescenceReceiptKey, w.reservationKey}, []string{b.identityValue, b.restoreEpoch, cleanupWirePadding(32768), cleanupWirePadding(4096), cleanupWirePadding(8192)}), nil
	case "write":
		cmps := cleanupWireAppend(b.baseComparisons(), w.live...)
		cmps = append(cmps, cleanupWireFence(w.quiescenceKey, 32768).comparisons()...)
		cmps = append(cmps, cleanupWireFence(w.quiescenceReceiptKey, 4096).comparisons()...)
		cmps = append(cmps, taskQuiescenceReservationPin{key: w.reservationKey, revision: math.MaxInt64}.comparison())
		for _, key := range keys[2:8] {
			cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(key), "=", math.MaxInt64))
		}
		cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(d.factKey), "=", 0))
		return b.detachedNoEffectWriteProjection(cmps, d.factKey, cleanupWirePadding(32768))
	}
	return cleanupWireProjection{}, ErrInvalidRecord
}

func (d *cleanupDetachedWireBuilder) recoveryProjection(variant string) (cleanupWireProjection, error) {
	if variant == "arbitration" {
		return d.executionProjection("observe")
	}
	worker, err := d.workerDimensions()
	if err != nil {
		return cleanupWireProjection{}, err
	}
	e, s, receipt, err := d.executionKeys()
	if err != nil {
		return cleanupWireProjection{}, err
	}
	cmps := worker
	for _, f := range []taskFence{d.in.bundle.fences[0], d.in.bundle.fences[1], cleanupWireFence(receipt, 4096), cleanupWireFence(e, 4096), cleanupWireFence(s, 4096)} {
		cmps = append(cmps, cleanupMaxFence(f).comparisons()...)
	}
	keys, _, err := d.noEffectSource()
	if err != nil {
		return cleanupWireProjection{}, err
	}
	for _, key := range []string{d.w.quiescenceKey, d.w.quiescenceReceiptKey, d.w.reservationKey, d.w.finalKey, keys[5], keys[6], keys[7], d.factKey} {
		cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(key), "=", math.MaxInt64))
	}
	return d.b.cleanupProjection(variant, cmps, []*pb.RequestOp{taskProtoPut(e, cleanupWirePadding(4096), 0)}, taskProtoPutResponse(1)), nil
}
