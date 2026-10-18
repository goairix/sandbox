package etcd

import (
	"math"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func cleanupDetachedLeaseProjection(name, kind string) cleanupLeaseWireProjection {
	v := cleanupLeaseWireProjection{name: name}
	header := taskProtoResponse(nil, nil).Header
	switch kind {
	case "grant":
		v.grant = &pb.LeaseGrantRequest{TTL: math.MaxInt64}
		v.granted = &pb.LeaseGrantResponse{Header: header, ID: math.MaxInt64, TTL: math.MaxInt64}
	case "keepalive":
		v.keepAlive = &pb.LeaseKeepAliveRequest{ID: math.MaxInt64}
		v.keptAlive = &pb.LeaseKeepAliveResponse{Header: header, ID: math.MaxInt64, TTL: math.MaxInt64}
	case "revoke":
		v.revoke = &pb.LeaseRevokeRequest{ID: math.MaxInt64}
		v.revoked = &pb.LeaseRevokeResponse{Header: header}
	}
	return v
}

func cleanupDetachedProjectionKeys(p cleanupWireProjection, add func([]byte) error) error {
	for _, branch := range [][]*pb.RequestOp{p.request.Success, p.request.Failure} {
		for _, op := range branch {
			var key []byte
			switch {
			case op.GetRequestRange() != nil:
				key = op.GetRequestRange().Key
			case op.GetRequestPut() != nil:
				key = op.GetRequestPut().Key
			case op.GetRequestDeleteRange() != nil:
				key = op.GetRequestDeleteRange().Key
			default:
				return ErrInvalidConfiguration
			}
			if err := add(key); err != nil {
				return err
			}
		}
	}
	for _, response := range []*pb.TxnResponse{p.success, p.failure} {
		for _, op := range response.Responses {
			if point := op.GetResponseRange(); point != nil {
				for _, kv := range point.Kvs {
					if err := add(kv.Key); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (d *cleanupDetachedWireBuilder) projection(family, variant string) (cleanupWireProjection, error) {
	switch family {
	case "source":
		return d.sourceProjection(variant)
	case "execution":
		return d.executionProjection(variant)
	case "detached-noeffect":
		return d.noEffectProjection(variant)
	case "detached-recovery":
		return d.recoveryProjection(variant)
	case "termination", "provider", "owner-release":
		return d.runtimeProjection(family, variant)
	default:
		return d.effectProjection(family, variant)
	}
}

func (d *cleanupDetachedWireBuilder) sourceProjection(variant string) (cleanupWireProjection, error) {
	keys, err := d.sourceKeys()
	if err != nil {
		return cleanupWireProjection{}, err
	}
	switch variant {
	case "snapshot-placement":
		p := cleanupWirePointProjection(d.b.baseComparisons(), keys[2:4], []string{cleanupWirePadding(65536), cleanupWirePadding(4096)})
		p.request.Failure = taskProtoPoints(keys[:2])
		p.failure = taskProtoResponse(keys[:2], []string{d.b.identityValue, d.b.restoreEpoch})
		return p, nil
	case "creation-intent":
		return d.w.enrolledRead(keys[4:5], 4096), nil
	case "publication-initial":
		return d.w.enrolledRead(keys[5:9], 8192, 16384, 8192, 8192), nil
	case "publication-complete":
		return d.w.enrolledRead(keys[5:], append([]int{8192, 16384, 8192, 8192, 4096}, enrolledHistoryCaps()...)...), nil
	}
	return cleanupWireProjection{}, ErrInvalidRecord
}

func (d *cleanupDetachedWireBuilder) effectProjection(family, variant string) (cleanupWireProjection, error) {
	b, w := d.b, &d.w
	baseLive := cleanupWireAppend(b.baseComparisons(), w.live...)
	pin := taskQuiescenceReservationPin{key: w.reservationKey, revision: math.MaxInt64 - 13}
	qGuard := cleanupWireAppend(cleanupWireAppend(b.baseComparisons(), w.quiescence...), pin.comparison())
	empty := cleanupWireAppend(baseLive, cleanupWireFence(w.quiescenceKey, 32768).comparisons()...)
	empty = append(empty, cleanupWireFence(w.quiescenceReceiptKey, 4096).comparisons()...)
	empty = append(empty, pin.comparison())
	if family == "stage-begin" || family == "stage-commit" || family == "stage-resolve" || family == "quiescence-stage" || family == "task-checkpoint" && variant == "checkpoint-mutation" {
		return d.stageProjection(family, variant)
	}
	if family == "task-checkpoint" || family == "task-renew" {
		if variant != "claim-create" {
			return w.guard(baseLive, false), nil
		}
		mutation := taskClaimCreationMutation(d.in.bundle.fences, ownTaskFence(d.in.bundle.checkpoint), w.claimKey, w.guardKey, w.claimValue)
		return b.cleanupProjection("", cleanupWireAppend(b.baseComparisons(), mutation.Comparisons...), []*pb.RequestOp{taskProtoPut(w.claimKey, w.claimValue, math.MaxInt64), taskProtoPut(w.guardKey, w.claimValue, math.MaxInt64)}, taskProtoPutResponse(2)), nil
	}
	if family == "dual-drain" {
		return d.drainProjection(variant, baseLive, empty), nil
	}
	if family == "final-sync-none" || family == "quiescence" && (variant == "reservation-write" || variant == "reservation-read") {
		key, cmps := w.finalKey, empty
		if family == "quiescence" {
			key, cmps = w.reservationKey, baseLive
		}
		if strings.HasSuffix(variant, "read") {
			return cleanupWirePointProjection(cmps, []string{b.identityKey, b.restoreKey, key}, []string{b.identityValue, b.restoreEpoch, cleanupWirePadding(8192)}), nil
		}
		return b.reservationWriteProjection(cmps, key, cleanupWirePadding(8192)), nil
	}
	issuer := func() cleanupWireProjection {
		return cleanupWirePointProjection(b.baseComparisons(), []string{b.identityKey, b.restoreKey, w.issuerKey}, []string{b.identityValue, b.restoreEpoch, w.issuerValue})
	}
	switch variant {
	case "issuer-read", "query-close-issuer", "query-quiescence-issuer":
		return issuer(), nil
	case "issuer-register":
		p := issuer()
		p.request.Compare = taskProtoComparisons(cleanupWireAppend(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(w.issuerKey), "=", 0)))
		p.request.Success = []*pb.RequestOp{taskProtoPut(w.issuerKey, w.issuerValue, 0)}
		p.success = taskProtoPutResponse(1)
		return p, nil
	case "close-pre-guard":
		return w.guard(cleanupWireAppend(b.baseComparisons(), w.close...), false), nil
	case "close-committed-guard":
		return w.guard(cleanupWireAppend(cleanupWireAppend(b.baseComparisons(), w.close...), cleanupWireFence(w.closeKey, 16384).comparisons()...), false), nil
	case "quiescence-pre-guard":
		return w.guard(qGuard, false), nil
	case "quiescence-committed-guard":
		return w.guard(cleanupWireAppend(qGuard, cleanupWireFence(w.quiescenceKey, 32768).comparisons()...), false), nil
	}
	cmps := b.baseComparisons()
	if family == "quiescence" {
		cmps = cleanupWireAppend(baseLive, pin.comparison())
	}
	keys := []string{b.identityKey, b.restoreKey, w.closeKey}
	values := []string{b.identityValue, b.restoreEpoch, cleanupWirePadding(16384)}
	if strings.HasSuffix(variant, "prior-complete") || variant == "close-stage-receipt-read" || variant == "query-close-stage-receipt" {
		keys, values = append(keys, w.closeReceiptKey), append(values, cleanupWirePadding(4096))
	} else if strings.HasSuffix(variant, ".initial") {
		keys[2], values[2] = w.quiescenceKey, cleanupWirePadding(32768)
	} else if variant == "quiescence-receipt-read" || variant == "query-quiescence-stage-receipt" {
		keys = []string{b.identityKey, b.restoreKey, w.quiescenceKey, w.quiescenceReceiptKey, w.closeKey, w.closeReceiptKey, w.reservationKey}
		values = []string{b.identityValue, b.restoreEpoch, cleanupWirePadding(32768), cleanupWirePadding(4096), cleanupWirePadding(16384), cleanupWirePadding(4096), cleanupWirePadding(8192)}
	}
	return cleanupWirePointProjection(cmps, keys, values), nil
}

// Shares the original exact reservation native shape without constructing and
// retaining its read sibling at the same time.
func (b *Backend) reservationWriteProjection(cmps []clientv3.Cmp, key, value string) cleanupWireProjection {
	keys := []string{b.identityKey, b.restoreKey, key}
	return cleanupWireProjection{request: &pb.TxnRequest{Compare: taskProtoComparisons(cleanupWireAppend(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))), Success: []*pb.RequestOp{taskProtoPut(key, value, 0)}, Failure: taskProtoPoints(keys)}, success: taskProtoPutResponse(1), failure: taskProtoResponse(keys, []string{b.identityValue, b.restoreEpoch, value})}
}

func (d *cleanupDetachedWireBuilder) stageProjection(family, variant string) (cleanupWireProjection, error) {
	w := &d.w
	ref, cmps, key, cap := w.closeStage, w.close, w.closeKey, 16384
	var pin *taskQuiescenceReservationPin
	atomicQuiescence := false
	stageVariant := "commit"
	if strings.Contains(variant, "checkpoint") || variant == "generic-begin" {
		ref.StageID = strings.Repeat("s", 128)
		key, cap = string(d.in.bundle.checkpoint.Key), 4096
		cmps = cleanupWireAppend(w.live, cleanupWireFence(key, 4096).comparisons()...)
	} else if strings.Contains(variant, "quiescence") || family == "quiescence-stage" {
		ref, cmps, key, cap = w.quiescenceStage, w.quiescence, w.quiescenceKey, 32768
		atomicQuiescence = true
	}
	if strings.HasSuffix(variant, "begin") {
		stageVariant = "begin"
	}
	if strings.HasSuffix(variant, "resolve") {
		stageVariant = "resolve"
	}
	guard, receipt, err := d.b.stageKeys(ref)
	if err != nil {
		return cleanupWireProjection{}, err
	}
	committed, err := encodeReceipt(ref, OutcomeCommitted)
	if err != nil {
		return cleanupWireProjection{}, err
	}
	if key == w.closeKey || key == w.quiescenceKey {
		cmps = cleanupWireAppend(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	}
	m := Mutation{Comparisons: cmps, Writes: []Write{{Key: key, Value: make([]byte, cap)}}}
	if atomicQuiescence {
		m = w.atomicQuiescenceMutation()
	}
	return d.b.taskStageWireProjection(stageVariant, ref, m, guard, receipt, committed, pin)
}

func (d *cleanupDetachedWireBuilder) drainProjection(variant string, live, empty []clientv3.Cmp) cleanupWireProjection {
	limit, cmps := 1, empty
	if variant == "operations-page" {
		limit, cmps = 16, live
	}
	op := taskProtoRange(d.w.prefix)
	r := op.GetRequestRange()
	r.RangeEnd, r.Limit, r.SortOrder, r.SortTarget = []byte(clientv3.GetPrefixRangeEnd(d.w.prefix)), int64(limit), pb.RangeRequest_ASCEND, pb.RangeRequest_KEY
	keys, values := make([]string, limit), make([]string, limit)
	for i := range keys {
		keys[i] = d.w.prefix + strings.Repeat("k", maxNamespaceKeyBytes-len(d.w.prefix)-2) + fmtWireIndex(i)
		values[i] = cleanupWirePadding(4096)
	}
	response := taskProtoResponse(keys, values)
	// One Range contains its bounded page, rather than one Range per row.
	page := response.Responses[0].GetResponseRange()
	for _, extra := range response.Responses[1:] {
		page.Kvs = append(page.Kvs, extra.GetResponseRange().Kvs...)
	}
	page.Count, page.More = math.MaxInt64, true
	response.Responses = response.Responses[:1]
	return cleanupWireProjection{request: &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: []*pb.RequestOp{op}, Failure: []*pb.RequestOp{op}}, success: response, failure: response}
}

func fmtWireIndex(i int) string {
	const alphabet = "0123456789abcdef"
	return string([]byte{alphabet[(i>>4)&15], alphabet[i&15]})
}
