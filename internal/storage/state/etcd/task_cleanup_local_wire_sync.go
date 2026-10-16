package etcd

import (
	"math"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

func (w *cleanupLocalWireBuilder) archiveRequests() error {
	b := w.b
	fresh := cleanupWireAppend(cleanupWireAppend(b.baseComparisons(), w.live...), clientv3.Compare(clientv3.ModRevision(w.reservationKey), "=", math.MaxInt64-5))
	keys := []string{b.identityKey, b.restoreKey, w.finalKey, w.quiescenceKey, w.quiescenceReceiptKey, w.reservationKey}
	values := []string{b.identityValue, b.restoreEpoch, string(make([]byte, 8192)), string(make([]byte, 32768)), string(make([]byte, 4096)), string(make([]byte, 8192))}
	w.add("archive-intent", "archive-pins-before-issuer", cleanupWirePointProjection(fresh, keys, values))
	w.add("archive-intent", "archive-pins-with-issuer", cleanupWirePointProjection(fresh, append(keys, w.issuerKey), append(values, w.issuerValue)))
	// The archive does not yet exist at admission: its eventual Mod pin is
	// added only by the post-commit authority. All earlier history pins remain.
	projections, err := b.taskArchiveWireProjections(w.archive[:len(w.archive)-1], w.archiveKey, string(make([]byte, maxRecordBytes)))
	if err != nil {
		return err
	}
	w.add("archive-intent", "archive-write", projections[0])
	w.add("archive-intent", "archive-read", projections[1])
	issuer, err := b.taskArchiveWireProjections(w.archive[:len(w.archive)-2], w.issuerKey, w.issuerValue)
	if err != nil {
		return err
	}
	w.add("archive-intent", "archive-issuer-register", issuer[0])
	// The successful registration installs its original issuer Mod before readback.
	issuerRead := cleanupWirePointProjection(w.archive[:len(w.archive)-1], []string{b.identityKey, b.restoreKey, w.issuerKey}, []string{b.identityValue, b.restoreEpoch, w.issuerValue})
	w.add("archive-intent", "archive-issuer-read", issuerRead)
	w.add("archive-send", "archive-guard", w.guard(w.archive, false))
	empty := cleanupWireAppend(w.archive, clientv3.Compare(clientv3.Version(w.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(w.prefix)))
	op := taskProtoRange(w.prefix)
	op.GetRequestRange().RangeEnd = []byte(clientv3.GetPrefixRangeEnd(w.prefix))
	op.GetRequestRange().Limit = 1
	op.GetRequestRange().SortOrder = pb.RangeRequest_ASCEND
	op.GetRequestRange().SortTarget = pb.RangeRequest_KEY
	rangeReply := taskProtoResponse([]string{w.prefix + strings.Repeat("k", maxNamespaceKeyBytes-len(w.prefix))}, []string{string(make([]byte, maxRecordBytes))})
	w.add("archive-send", "archive-empty-guard", cleanupWireProjection{request: &pb.TxnRequest{Compare: taskProtoComparisons(w.archive), Success: []*pb.RequestOp{op}, Failure: []*pb.RequestOp{op}}, success: rangeReply, failure: rangeReply})
	w.add("archive-send", "archive-query-guard", w.guard(empty, true))
	return nil
}

// These two/three-field pins are the original recycling protocol's pins, not
// full task fences. The retained history and whole-prefix/budget pins precede
// them; changing this cardinality would exceed the native64-node transitions.
func cleanupWireMutablePins(key, value string) []clientv3.Cmp {
	return []clientv3.Cmp{clientv3.Compare(clientv3.ModRevision(key), "=", math.MaxInt64-1), clientv3.Compare(clientv3.Value(key), "=", value)}
}

func cleanupWireImmutablePins(key, value string) []clientv3.Cmp {
	return append([]clientv3.Cmp{clientv3.Compare(clientv3.CreateRevision(key), "=", math.MaxInt64-1)}, cleanupWireMutablePins(key, value)...)
}

func (w *cleanupLocalWireBuilder) syncRequests() error {
	b := w.b
	intent := string(make([]byte, b.workspaceSyncBudget.options.MaxIntentBytes))
	slot, progress := string(make([]byte, 512)), string(make([]byte, 16384))
	checkpointKey := string(w.in.bundle.checkpoint.Key)
	checkpoint := string(make([]byte, 4096))
	intentPins := cleanupWireImmutablePins(w.intentKey, intent)
	slotPins := cleanupWireMutablePins(w.slotKey, slot)
	charged := cleanupWireAppend(w.sync, intentPins...)
	terminal := cleanupWireAppend(charged, cleanupWireImmutablePins(w.slotKey, slot)...)
	reserve := cleanupWireAppend(w.sync, clientv3.Compare(clientv3.Version(w.intentKey), "=", 0))
	reserve = append(reserve, slotPins...)
	keys := []string{b.identityKey, b.restoreKey, b.workspaceSyncBudget.key, w.intentKey, w.slotKey}
	admit, err := b.workspaceSyncIntentWireProjection(reserve, w.intentKey, intent, w.slotKey, slot, keys)
	if err != nil {
		return err
	}
	intentRead := w.syncRead(cleanupWireAppend(b.baseComparisons(), b.workspaceSyncBudget.comparisons()...), []string{w.intentKey, w.slotKey}, len(intent), 512)
	candidate := w.syncRead(w.sync, []string{w.intentKey, w.slotKey}, len(intent), 512)
	for _, family := range []string{"archive-recovery", "archive-recycling"} {
		name := "intent-read"
		if family == "archive-recovery" {
			name = "intent-admit-read"
		}
		w.add(family, name, intentRead)
		w.add(family, "intent-arbitrate", intentRead)
	}
	w.add("archive-recovery", "intent-admit-write", admit)
	w.add("archive-recycling", "candidate-read", candidate)
	w.add("archive-recycling", "reserve", admit)
	claimGuard := cleanupWireAppend(cleanupWireAppend(b.baseComparisons(), w.live...), clientv3.Compare(clientv3.Version(w.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(w.prefix)))
	claimGuard = append(claimGuard, b.workspaceSyncBudget.comparisons()...)
	w.add("archive-recovery", "recovery-claim-guard", workspaceSyncNativeProjection(b, claimGuard, taskProtoPoints([]string{b.identityKey}), taskProtoResponse([]string{b.identityKey}, []string{b.identityValue}), false))
	w.add("archive-recovery", "recovery-authority-guard", w.guard(w.sync[:len(w.sync)-3], true))
	for _, source := range []struct {
		name string
		cmps []clientv3.Cmp
	}{{"uncharged", w.sync}, {"charged", terminal}} {
		w.add("archive-recovery", "recovery-source-guard."+source.name, workspaceSyncNativeProjection(b, source.cmps, taskProtoPoints([]string{b.identityKey}), taskProtoResponse([]string{b.identityKey}, []string{b.identityValue}), false))
	}
	w.add("archive-recovery", "recovery-slot-read", w.syncRead(w.sync, []string{w.slotKey}, 512))
	w.add("archive-recovery", "recovery-slot-intent-read", w.syncRead(cleanupWireAppend(w.sync, slotPins...), []string{w.slotKey, w.intentKey}, 512, len(intent)))
	w.add("archive-recovery", "recovery-reserve", workspaceSyncNativeProjection(b, reserve, []*pb.RequestOp{taskProtoPut(w.intentKey, intent, 0), taskProtoPut(w.slotKey, slot, 0)}, taskProtoPutResponse(2), false))
	completionRead := w.syncRead(w.sync, []string{w.slotKey, w.intentKey}, 512, len(intent))
	w.add("archive-recovery", "recovery-completion-read.intent", completionRead)
	w.add("archive-recycling", "completion-read.intent", completionRead)
	completionCheckpointRead := w.syncRead(w.sync, []string{w.slotKey, w.intentKey, checkpointKey}, 512, len(intent), 4096)
	w.add("archive-recovery", "recovery-completion-read.checkpoint", completionCheckpointRead)
	w.add("archive-recycling", "completion-read.checkpoint", completionCheckpointRead)
	settle := workspaceSyncNativeProjection(b, terminal, []*pb.RequestOp{taskProtoPut(w.slotKey, slot, 0)}, taskProtoPutResponse(1), false)
	w.add("archive-recovery", "recovery-settle", settle)
	w.add("archive-recycling", "terminal", settle)
	checkpointRead := w.syncRead(w.sync, []string{checkpointKey}, 4096)
	w.add("archive-recovery", "recovery-checkpoint-read", checkpointRead)
	w.add("local-complete", "checkpoint-read", checkpointRead)
	gcComparisons := cleanupWireAppend(cleanupWireAppend(charged, slotPins...), cleanupWireMutablePins(checkpointKey, checkpoint)...)
	gc := workspaceSyncNativeProjection(b, gcComparisons, taskProtoWrites([]Write{{Key: w.slotKey, Value: []byte(slot)}, {Key: w.intentKey, Delete: true}, {Key: checkpointKey, Value: []byte(checkpoint)}}), recyclingGCReply(), true)
	w.add("archive-recovery", "recovery-recycle", gc)
	// captureCheckpoint only constructs data. Its write is the full recycle
	// transaction; this diagnostic alias does not represent an extra RPC.
	w.add("archive-recovery", "recovery-checkpoint-write", gc)
	w.add("archive-recycling", "gc", gc)

	progressRead := w.syncRead(w.sync, []string{w.progressKey, w.completeKey}, 16384, 8192)
	w.add("local-progress", "progress-read", progressRead)
	// readCompletionOwned consumes loadProgress's two-point result directly.
	w.add("local-complete", "completion-read", progressRead)
	progressPins := cleanupWireMutablePins(w.progressKey, progress)
	for _, item := range []struct {
		name  string
		cmps  []clientv3.Cmp
		empty bool
	}{
		{"progress-initial", cleanupWireAppend(w.sync, clientv3.Compare(clientv3.Version(w.progressKey), "=", 0)), false},
		{"progress-update", cleanupWireAppend(w.sync, progressPins...), false},
		{"progress-charged-update", cleanupWireAppend(cleanupWireAppend(charged, slotPins...), progressPins...), true},
	} {
		w.add("local-progress", item.name, workspaceSyncNativeProjection(b, item.cmps, []*pb.RequestOp{taskProtoPut(w.progressKey, progress, 0)}, taskProtoPutResponse(1), item.empty))
	}
	w.add("local-progress", "metadata-reserve-read.slot", w.syncRead(w.sync, []string{w.slotKey}, 512))
	w.add("local-progress", "metadata-reserve-read.slot-intent", completionRead)
	w.add("local-progress", "metadata-reserve", workspaceSyncNativeProjection(b, cleanupWireAppend(reserve, progressPins...), []*pb.RequestOp{taskProtoPut(w.intentKey, intent, 0), taskProtoPut(w.slotKey, slot, 0)}, taskProtoPutResponse(2), false))
	metadata := cleanupWireAppend(cleanupWireAppend(charged, slotPins...), progressPins...)
	w.add("local-progress", "metadata-guard", workspaceSyncNativeProjection(b, metadata, taskProtoPoints([]string{b.identityKey}), taskProtoResponse([]string{b.identityKey}, []string{b.identityValue}), true))
	w.add("local-progress", "metadata-terminal", workspaceSyncNativeProjection(b, cleanupWireAppend(terminal, progressPins...), []*pb.RequestOp{taskProtoPut(w.slotKey, slot, 0)}, taskProtoPutResponse(1), true))
	local, err := b.taskLocalFinalizationWireProjections(w.sync, w.claim, w.in.bundle.birth, w.in.plan.digest, w.in.plan.digest, w.in.state.record.Run.ID, w.progressKey, w.completeKey, checkpointKey)
	if err != nil {
		return err
	}
	w.add("local-progress", "metadata-settle", local[0])
	w.add("local-complete", "completion-publish", local[1])
	guard := workspaceSyncNativeProjection(b, w.sync, taskProtoPoints([]string{b.identityKey}), taskProtoResponse([]string{b.identityKey}, []string{b.identityValue}), false)
	w.add("local-complete", "effects-seal", guard)
	w.add("local-complete", "completion-guard", guard)
	return nil
}
