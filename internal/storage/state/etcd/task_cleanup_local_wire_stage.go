package etcd

import (
	"math"
	"strings"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Future IDs use the already fixed claim/run UUID widths. They are dimensional
// identities, never persisted attempts. Revisions reserve room for the later
// close and quiescence writes while retaining maximal protobuf varint widths.
func (w *cleanupLocalWireBuilder) prepare() error {
	b, task := w.b, w.in.bundle.task
	ref := task.Reference
	w.claim = TaskClaimReference{Task: ref, ClaimID: w.dimensions.claimID, WorkerID: w.in.workerID, CreateRevision: math.MaxInt64 - 8, LeaseID: math.MaxInt64}
	var err error
	w.claimKey, err = b.namespace.taskClaimKey(ref)
	if err != nil {
		return err
	}
	w.guardKey, err = b.namespace.taskGuardKey(ref, w.claim.ClaimID)
	if err != nil {
		return err
	}
	w.claimValue, err = encodeTaskClaimRecord(taskClaimRecord{Version: 1, Task: ref, ClaimID: w.claim.ClaimID, WorkerID: w.claim.WorkerID, LeaseID: w.claim.LeaseID})
	if err != nil {
		return err
	}
	w.issuerKey, err = b.namespace.commandIssuerKey(w.in.issuer.Record.CertificateID)
	if err != nil {
		return err
	}
	w.issuerValue, err = encodeCommandIssuerRecord(w.in.issuer.Record)
	if err != nil {
		return err
	}
	w.closeKey, err = b.namespace.taskCloseDataKey(ref)
	if err != nil {
		return err
	}
	w.quiescenceKey, err = b.namespace.taskQuiescenceKey(ref)
	if err != nil {
		return err
	}
	w.reservationKey, err = b.namespace.taskQuiescenceAttemptKey(ref)
	if err != nil {
		return err
	}
	w.finalKey, err = b.namespace.taskFinalSyncAttemptKey(ref)
	if err != nil {
		return err
	}
	w.archiveKey, err = b.namespace.taskWorkspaceArchiveKey(ref)
	if err != nil {
		return err
	}
	w.intentKey, w.slotKey, err = b.workspaceSyncRecyclingKeys(TaskWorkspaceSyncIntentReference{Task: ref, BudgetID: b.workspaceSyncBudget.options.BudgetID, IntentID: w.dimensions.runID, Slot: b.workspaceSyncBudget.options.SlotsPerPartition - 1})
	if err != nil {
		return err
	}
	w.progressKey, w.completeKey, err = b.namespace.workspaceSyncLocalKeys(ref)
	if err != nil {
		return err
	}
	w.terminationKey, w.providerKey, w.releaseKey, err = b.namespace.localRuntimeReleaseKeys(ref)
	if err != nil {
		return err
	}
	w.prefix, err = b.taskOperationsPrefix(ref)
	if err != nil {
		return err
	}
	w.live = taskClaimWireComparisons(w.in.bundle.fences, w.claim, w.claimKey, w.guardKey, w.claimValue)
	w.close = taskCloseWireComparisons(w.live, w.issuerKey, w.issuerValue, w.in.issuer.Revision)
	for _, spec := range []struct {
		name string
		out  *StageReference
		key  *string
	}{{"task_close_data", &w.closeStage, &w.closeReceiptKey}, {"task_quiesce_users", &w.quiescenceStage, &w.quiescenceReceiptKey}} {
		attempt := StageAttemptLocator{Namespace: ref.Namespace, RestoreEpoch: ref.RestoreEpoch, Partition: ref.Partition, RequestID: w.claim.ClaimID, StageID: spec.name, AttemptID: w.dimensions.runID}
		*spec.out = attempt.reference(cleanupWirePaddingDigest())
		_, *spec.key, err = b.stageKeys(*spec.out)
		if err != nil {
			return err
		}
	}
	w.quiescence = cleanupWireAppend(w.close, cleanupWireFence(w.closeKey, 16384).comparisons()...)
	w.quiescence = append(w.quiescence, clientv3.Compare(clientv3.ModRevision(w.closeReceiptKey), "=", math.MaxInt64-6))
	w.archive = cleanupWireAppend(b.baseComparisons(), w.live...)
	for _, key := range []string{w.quiescenceKey, w.quiescenceReceiptKey, w.reservationKey, w.finalKey, w.issuerKey, w.archiveKey} {
		w.archive = append(w.archive, clientv3.Compare(clientv3.ModRevision(key), "=", math.MaxInt64-1))
	}
	w.sync = cleanupWireAppend(w.archive, clientv3.Compare(clientv3.Version(w.prefix), "=", 0).WithRange(clientv3.GetPrefixRangeEnd(w.prefix)))
	w.sync = append(w.sync, b.workspaceSyncBudget.comparisons()...)
	return w.effectSchemas()
}

func (w *cleanupLocalWireBuilder) stageProjection(ref StageReference, m Mutation, pin *taskQuiescenceReservationPin) ([]cleanupWireProjection, error) {
	guardKey, receiptKey, err := w.b.stageKeys(ref)
	if err != nil {
		return nil, err
	}
	committed, err := encodeReceipt(ref, OutcomeCommitted)
	if err != nil {
		return nil, err
	}
	return w.b.taskStageWireProjections(ref, m, guardKey, receiptKey, committed, pin)
}

func (w *cleanupLocalWireBuilder) stages() error {
	b := w.b
	closeMutation := Mutation{Comparisons: cleanupWireAppend(w.close, clientv3.Compare(clientv3.CreateRevision(w.closeKey), "=", 0)), Writes: []Write{{Key: w.closeKey, Value: make([]byte, 16384)}}}
	closeStage, err := w.stageProjection(w.closeStage, closeMutation, nil)
	if err != nil {
		return err
	}
	w.add("stage-commit", "close-commit", closeStage[1])
	w.add("stage-resolve", "close-resolve", closeStage[2])

	checkpoint := ownTaskFence(w.in.bundle.checkpoint)
	checkpointMutation := Mutation{Comparisons: cleanupWireAppend(w.live, cleanupWireFence(checkpoint.key, 4096).comparisons()...), Writes: []Write{{Key: checkpoint.key, Value: make([]byte, 4096)}}}
	checkpointRef := w.closeStage
	checkpointRef.StageID = strings.Repeat("s", 128)
	checkpointStage, err := w.stageProjection(checkpointRef, checkpointMutation, nil)
	if err != nil {
		return err
	}
	// Generic begin has the same shape for close and checkpoint; the longest
	// admitted checkpoint StageID bounds that actual parameterized request.
	w.add("stage-begin", "generic-begin", checkpointStage[0])
	w.add("stage-commit", "checkpoint-commit", checkpointStage[1])
	w.add("stage-resolve", "checkpoint-resolve", checkpointStage[2])
	w.add("task-checkpoint", "checkpoint-mutation", checkpointStage[1])
	create := taskClaimCreationMutation(w.in.bundle.fences, checkpoint, w.claimKey, w.guardKey, w.claimValue)
	w.add("task-checkpoint", "claim-create", b.cleanupProjection("", cleanupWireAppend(b.baseComparisons(), create.Comparisons...), []*pb.RequestOp{taskProtoPut(w.claimKey, w.claimValue, math.MaxInt64), taskProtoPut(w.guardKey, w.claimValue, math.MaxInt64)}, taskProtoPutResponse(2)))
	claimGuard := w.guard(cleanupWireAppend(b.baseComparisons(), w.live...), false)
	w.add("task-checkpoint", "claim-post-fence", claimGuard)
	w.add("task-renew", "renew-before", claimGuard)
	w.add("task-renew", "renew-after", claimGuard)

	pin := taskQuiescenceReservationPin{key: w.reservationKey, revision: math.MaxInt64 - 5}
	qStage, err := w.stageProjection(w.quiescenceStage, w.atomicQuiescenceMutation(), nil)
	if err != nil {
		return err
	}
	w.add("quiescence-stage", "reserved-begin", qStage[0])
	w.add("quiescence-stage", "reserved-commit", qStage[1])
	w.add("stage-resolve", "quiescence-resolve", qStage[2])
	reservation, err := b.taskReservationWireProjections(cleanupWireAppend(cleanupWireAppend(b.baseComparisons(), w.live...), clientv3.Compare(clientv3.CreateRevision(w.reservationKey), "=", 0)), w.reservationKey, string(make([]byte, 8192)))
	if err != nil {
		return err
	}
	w.add("quiescence", "reservation-write", reservation[0])
	w.add("quiescence", "reservation-read", reservation[1])
	w.add("close", "close-pre-guard", w.guard(cleanupWireAppend(b.baseComparisons(), w.close...), false))
	w.add("close", "close-committed-guard", w.guard(cleanupWireAppend(cleanupWireAppend(b.baseComparisons(), w.close...), cleanupWireFence(w.closeKey, 16384).comparisons()...), false))
	qGuard := cleanupWireAppend(cleanupWireAppend(b.baseComparisons(), w.quiescence...), pin.comparison())
	w.add("quiescence", "quiescence-pre-guard", w.guard(qGuard, false))
	w.add("quiescence", "quiescence-committed-guard", w.guard(cleanupWireAppend(qGuard, cleanupWireFence(w.quiescenceKey, 32768).comparisons()...), false))
	w.effectReads(pin)
	return w.drainRequests(pin)
}

// Pure future dimensions mirror the actual atomic Q producer. The positive
// issuer Mod here is only a width witness; live production obtains it from the
// original native issuer reader before constructing its own Stage mutation.
func (w *cleanupLocalWireBuilder) atomicQuiescenceMutation() Mutation {
	cmps := cleanupWireAppend(w.live, clientv3.Compare(clientv3.ModRevision(w.issuerKey), "=", math.MaxInt64))
	cmps = append(cmps, cleanupWireFence(w.closeKey, 16384).comparisons()...)
	cmps = append(cmps, clientv3.Compare(clientv3.ModRevision(w.closeReceiptKey), "=", math.MaxInt64), clientv3.Compare(clientv3.CreateRevision(w.quiescenceKey), "=", 0), clientv3.Compare(clientv3.CreateRevision(w.reservationKey), "=", 0))
	return Mutation{Comparisons: cmps, Writes: []Write{{Key: w.quiescenceKey, Value: make([]byte, 32768)}, {Key: w.reservationKey, Value: make([]byte, 8192)}}}
}

func (w *cleanupLocalWireBuilder) effectReads(pin taskQuiescenceReservationPin) {
	b := w.b
	issuer := cleanupWirePointProjection(b.baseComparisons(), []string{b.identityKey, b.restoreKey, w.issuerKey}, []string{b.identityValue, b.restoreEpoch, w.issuerValue})
	register := cleanupWireProjection{request: &pb.TxnRequest{Compare: taskProtoComparisons(cleanupWireAppend(b.baseComparisons(), clientv3.Compare(clientv3.CreateRevision(w.issuerKey), "=", 0))), Success: []*pb.RequestOp{taskProtoPut(w.issuerKey, w.issuerValue, 0)}, Failure: issuer.request.Failure}, success: taskProtoPutResponse(1), failure: issuer.failure}
	w.add("close", "issuer-read", issuer)
	w.add("close", "issuer-register", register)
	w.add("close-query", "query-close-issuer", issuer)
	w.add("quiescence-query", "query-quiescence-issuer", issuer)
	closeKeys := []string{b.identityKey, b.restoreKey, w.closeKey}
	closeValues := []string{b.identityValue, b.restoreEpoch, string(make([]byte, 16384))}
	initial := cleanupWirePointProjection(b.baseComparisons(), closeKeys, closeValues)
	complete := cleanupWirePointProjection(b.baseComparisons(), append(closeKeys, w.closeReceiptKey), append(closeValues, string(make([]byte, 4096))))
	w.add("close", "close-intent-read", initial)
	w.add("close", "close-stage-receipt-read", complete)
	w.add("close-query", "query-close-history", initial)
	w.add("close-query", "query-close-stage-receipt", complete)
	for _, source := range []struct {
		family, stem, receipt string
		cmps                  []clientv3.Cmp
	}{
		{"quiescence", "quiescence-intent-read", "quiescence-receipt-read", cleanupWireAppend(cleanupWireAppend(b.baseComparisons(), w.live...), pin.comparison())},
		{"quiescence-query", "query-quiescence-history", "query-quiescence-stage-receipt", b.baseComparisons()},
	} {
		w.add(source.family, source.stem+".initial", cleanupWirePointProjection(source.cmps, []string{b.identityKey, b.restoreKey, w.quiescenceKey}, []string{b.identityValue, b.restoreEpoch, string(make([]byte, 32768))}))
		w.add(source.family, source.stem+".prior-initial", cleanupWirePointProjection(source.cmps, closeKeys, closeValues))
		w.add(source.family, source.stem+".prior-complete", cleanupWirePointProjection(source.cmps, append(closeKeys, w.closeReceiptKey), append(closeValues, string(make([]byte, 4096)))))
		w.add(source.family, source.receipt, cleanupWirePointProjection(source.cmps, []string{b.identityKey, b.restoreKey, w.quiescenceKey, w.quiescenceReceiptKey, w.closeKey, w.closeReceiptKey, w.reservationKey}, []string{b.identityValue, b.restoreEpoch, string(make([]byte, 32768)), string(make([]byte, 4096)), string(make([]byte, 16384)), string(make([]byte, 4096)), string(make([]byte, 8192))}))
	}
}

func (w *cleanupLocalWireBuilder) drainRequests(pin taskQuiescenceReservationPin) error {
	cmps := cleanupWireAppend(w.b.baseComparisons(), w.live...)
	empty := cleanupWireAppend(cmps, cleanupWireFence(w.quiescenceKey, 32768).comparisons()...)
	empty = append(empty, cleanupWireFence(w.quiescenceReceiptKey, 4096).comparisons()...)
	empty = append(empty, pin.comparison())
	for _, spec := range []struct {
		name        string
		limit       int64
		comparisons []clientv3.Cmp
	}{{"operations-page", 16, cmps}, {"empty-before", 1, empty}, {"empty-after", 1, empty}} {
		op := &pb.RequestOp{Request: &pb.RequestOp_RequestRange{RequestRange: &pb.RangeRequest{Key: []byte(w.prefix), RangeEnd: []byte(clientv3.GetPrefixRangeEnd(w.prefix)), Limit: spec.limit, SortOrder: pb.RangeRequest_ASCEND, SortTarget: pb.RangeRequest_KEY}}}
		response := taskProtoResponse(nil, nil)
		rangeResponse := &pb.RangeResponse{Header: response.Header, Count: math.MaxInt64, More: true}
		for i := int64(0); i < spec.limit; i++ {
			rangeResponse.Kvs = append(rangeResponse.Kvs, &mvccpb.KeyValue{Key: []byte(w.prefix + strings.Repeat("k", maxNamespaceKeyBytes-len(w.prefix))), Value: make([]byte, 4096), CreateRevision: math.MaxInt64, ModRevision: math.MaxInt64, Version: math.MaxInt64, Lease: math.MaxInt64})
		}
		response.Responses = []*pb.ResponseOp{{Response: &pb.ResponseOp_ResponseRange{ResponseRange: rangeResponse}}}
		w.add("dual-drain", spec.name, cleanupWireProjection{request: &pb.TxnRequest{Compare: taskProtoComparisons(spec.comparisons), Success: []*pb.RequestOp{op}, Failure: []*pb.RequestOp{op}}, success: response, failure: response})
	}
	// final-sync uses precisely the same original dual-drain comparisons.
	reservation, err := w.b.taskReservationWireProjections(cleanupWireAppend(empty, clientv3.Compare(clientv3.CreateRevision(w.finalKey), "=", 0)), w.finalKey, string(make([]byte, 8192)))
	if err != nil {
		return err
	}
	w.add("final-sync", "final-sync-reserve", reservation[0])
	w.add("final-sync", "final-sync-read", reservation[1])
	return nil
}
