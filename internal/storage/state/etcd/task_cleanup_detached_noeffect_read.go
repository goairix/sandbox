package etcd

import (
	"context"
	"errors"
	"reflect"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

// This is temporary live/pre-release authenticated history. Its enrolled
// reader requires the original runtime index; it is not a post-owner-release
// quota/terminal reader. The original caller consumes and discards this bounded
// graph within one call, rather than attaching snapshots/proofs to 32 jobs.
type cleanupDetachedNoEffectHistory struct {
	origin *Backend
	self   *cleanupDetachedNoEffectHistory
	task   TaskRecord
	state  *cleanupSlotState

	snapshot    SandboxSnapshotRecord
	intent      CreationIntentRecord
	publication *RuntimeEnrolledPublicationEntry
	quiescence  *TaskQuiescenceEntry
	finalSync   *TaskFinalSyncAttemptEntry

	taskFence        taskFence
	quiescencePin    taskDualDrainPin
	snapshotFence    taskFence
	intentFence      taskFence
	publicationFence taskFence
	finalSyncFence   taskFence
	settlement       *cleanupDetachedNoEffectEntry
}

// Every point is an actual identity-fenced native observation. Bounds are
// checked both against a conservative protobuf projection and the real reply;
// no synthetic KV or caller DTO can supply a current Mod pin through this path.
func (b *Backend) readCleanupDetachedNoEffectPoints(ctx context.Context, name string, keys []string, caps []int) ([]*mvccpb.KeyValue, int64, error) {
	if b == nil || ctx == nil || len(keys) != len(caps) || len(keys) < 3 || len(keys) > 9 || keys[0] != b.identityKey || keys[1] != b.restoreKey {
		return nil, 0, ErrInvalidRecord
	}
	values := make([]string, len(keys))
	for i, key := range keys {
		if !validNamespaceKey(b.namespace, key) || caps[i] <= 0 || caps[i] > maxRecordBytes {
			return nil, 0, ErrInvalidRecord
		}
		values[i] = cleanupWirePadding(caps[i])
	}
	values[0], values[1] = b.identityValue, b.restoreEpoch
	responseBound := taskProtoResponse(keys, values)
	projection := cleanupWireProjection{name: "detached-noeffect-" + name, request: &pb.TxnRequest{Compare: taskProtoComparisons(b.baseComparisons()), Success: taskProtoPoints(keys), Failure: taskProtoPoints(keys)}, success: responseBound, failure: responseBound}
	// Source and arbitration dispatch use the same builders whose literal
	// 22/10-node envelopes are frozen in the native wire tests. The preliminary
	// locate and quiescence reads retain their finite 12/14-node shapes.
	if len(keys) == 3 || len(keys) == 9 {
		var err error
		projection, err = b.detachedNoEffectPointProjection(name, keys, values)
		if err != nil {
			return nil, 0, err
		}
	}
	if err := validateCleanupWire(projection); err != nil {
		return nil, 0, err
	}
	rpc, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(rpc).If(b.baseComparisons()...).Then(workspaceSyncPointOps(keys)...).Else(workspaceSyncPointOps(keys)...).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return nil, 0, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if b.operationResponseHeader(response) != nil || (*pb.TxnResponse)(response).Size() > maxMutationBytes {
		return nil, 0, ErrCorruptRecord
	}
	points, err := b.stageEvidencePoints(response, keys)
	if err != nil {
		return nil, 0, err
	}
	if !response.Succeeded {
		return nil, 0, ErrIdentityMismatch
	}
	for i, kv := range points {
		if kv != nil && (len(kv.Value) > caps[i] || kv.Version < 1 || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > response.Header.Revision || (kv.Version == 1) != (kv.CreateRevision == kv.ModRevision)) {
			return nil, 0, ErrCorruptRecord
		}
	}
	return points, response.Header.Revision, nil
}

func (b *Backend) locateCleanupDetachedNoEffect(ctx context.Context, ref TaskReference) (TaskRecord, taskFence, cleanupExecutionRecord, error) {
	fail := func(err error) (TaskRecord, taskFence, cleanupExecutionRecord, error) {
		return TaskRecord{}, taskFence{}, cleanupExecutionRecord{}, err
	}
	if err := b.validateTaskReference(ref); err != nil {
		return fail(err)
	}
	taskKey, err := b.namespace.taskKey(ref)
	if err != nil {
		return fail(err)
	}
	eKey, err := b.namespace.cleanupExecutionKey(ref)
	if err != nil {
		return fail(err)
	}
	points, revision, err := b.readCleanupDetachedNoEffectPoints(ctx, "locate", []string{b.identityKey, b.restoreKey, taskKey, eKey}, []int{maxRecordBytes, maxRecordBytes, 4096, cleanupExecutionBytes})
	if err != nil {
		return fail(err)
	}
	var task TaskRecord
	if !enrolledImmutableKV(points[2]) || decodeTaskRecord(points[2], &task) != nil || task.Reference != ref {
		return fail(ErrCorruptRecord)
	}
	var execution cleanupExecutionRecord
	if err = decodeCleanupExecution(points[3], revision, ref, &execution); err != nil || execution.TaskBirth != points[2].CreateRevision || execution.TaskDigest != workspaceSyncIntentDigest(string(points[2].Value)) {
		return fail(errors.Join(ErrCorruptRecord, err))
	}
	return task, ownTaskFence(points[2]), execution, nil
}

// The fixed five-point transport envelope supplies no signed-history authority.
// Its caller still authenticates the original quiescence, Stage receipt and
// reservation together; accepting bounded bytes alone cannot establish facts.
// The 32768-byte intent bound matches encode/decodeTaskQuiescenceRecord, while
// the receipt and reservation retain their existing 4096/8192-byte bounds.
func (b *Backend) readCleanupDetachedNoEffectQuiescencePoints(ctx context.Context, keys []string) ([]*mvccpb.KeyValue, int64, error) {
	if len(keys) != 5 {
		return nil, 0, ErrInvalidRecord
	}
	return b.readCleanupDetachedNoEffectPoints(ctx, "quiescence", keys, []int{maxRecordBytes, maxRecordBytes, 32768, 4096, 8192})
}

func (b *Backend) readCleanupDetachedNoEffectQuiescence(ctx context.Context, task TaskRecord, birth int64) (*TaskQuiescenceEntry, taskDualDrainPin, error) {
	q, err := b.loadTaskQuiescence(ctx, task.Reference, nil)
	if err != nil {
		return nil, taskDualDrainPin{}, err
	}
	if q == nil || q.Record == nil || q.Record.Task != task || q.Record.Context.Current.ControlRevision != birth || q.reservation == nil {
		return nil, taskDualDrainPin{}, ErrCorruptRecord
	}
	key, err := b.namespace.taskQuiescenceKey(task.Reference)
	if err != nil {
		return nil, taskDualDrainPin{}, err
	}
	_, receiptKey, err := b.stageKeys(q.Reference.Stage)
	if err != nil {
		return nil, taskDualDrainPin{}, err
	}
	reservationKey, err := b.namespace.taskQuiescenceAttemptKey(task.Reference)
	if err != nil {
		return nil, taskDualDrainPin{}, err
	}
	points, _, err := b.readCleanupDetachedNoEffectQuiescencePoints(ctx, []string{b.identityKey, b.restoreKey, key, receiptKey, reservationKey})
	if err != nil {
		return nil, taskDualDrainPin{}, err
	}
	wire, err := encodeTaskQuiescenceRecord(*q.Record)
	if err != nil || !enrolledImmutableKV(points[2]) || points[2].CreateRevision != q.Revision || string(points[2].Value) != wire {
		return nil, taskDualDrainPin{}, ErrCorruptRecord
	}
	stage, err := quiescenceCommittedReceipt(points[3], receiptKey, q.Revision, q.Record.Attempt)
	if err != nil || stage != q.Reference.Stage {
		return nil, taskDualDrainPin{}, errors.Join(ErrCorruptReceipt, err)
	}
	reservation, err := taskQuiescenceAttemptEntry(points[4], task.Reference)
	if err != nil || !taskQuiescenceHistoryReservation(*q.Record, q.Revision, reservation) || reservation.Revision != q.reservation.Revision {
		return nil, taskDualDrainPin{}, errors.Join(ErrCorruptRecord, err)
	}
	pin := taskDualDrainPin{reference: q.Reference, task: task, intent: ownTaskFence(points[2]), receipt: ownTaskFence(points[3]), reservation: taskQuiescenceReservationPin{key: reservationKey, revision: points[4].ModRevision}}
	return q, pin, nil
}

// readCleanupDetachedNoEffectHistory authenticates only live/pre-release
// history. It shares the original signed enrolled reader and strict E/S codec;
// a copied classification/DTO cannot replace actual source KVs. Owner/index
// deletion recovery belongs to the separate release-sealed terminal readers.
func (b *Backend) readCleanupDetachedNoEffectHistory(ctx context.Context, ref TaskReference) (*cleanupDetachedNoEffectHistory, error) {
	if b == nil || ctx == nil || b.detachedCleanup == nil {
		return nil, ErrInvalidRecord
	}
	task, taskFence, located, err := b.locateCleanupDetachedNoEffect(ctx, ref)
	if err != nil {
		return nil, err
	}
	q, qpin, err := b.readCleanupDetachedNoEffectQuiescence(ctx, task, taskFence.create)
	if err != nil {
		return nil, err
	}
	finalSync, err := b.LoadTaskFinalSyncAttempt(ctx, ref)
	if err != nil {
		return nil, err
	}
	if finalSync == nil || finalSync.Record == nil || finalSync.Record.Task != task || finalSync.Record.DestroyingRevision != taskFence.create || finalSync.Record.Plan.Mode != "none" || finalSync.Record.Quiescence != q.Reference || finalSync.Record.QuiescenceRevision != q.Revision {
		return nil, ErrCorruptRecord
	}
	read, err := b.loadRuntimePublicationEnrolledAt(ctx, runtimeHistoryLocator{hash: task.WorkspaceHash, partition: ref.Partition}, task.CreationIntentID)
	if err != nil || read == nil || read.Publication == nil {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	eKey, err := b.namespace.cleanupExecutionKey(ref)
	if err != nil {
		return nil, err
	}
	sKey, err := b.namespace.cleanupExecutionSlotKey(located.Slot)
	if err != nil {
		return nil, err
	}
	fKey, err := b.namespace.taskFinalSyncAttemptKey(ref)
	if err != nil {
		return nil, err
	}
	_, snapshotKey, err := b.namespace.sandboxKeys(ref.Partition, ref.SandboxID, task.Snapshot.Version)
	if err != nil {
		return nil, err
	}
	iKey, err := b.namespace.intentKey(ref.Partition, task.CreationIntentID)
	if err != nil {
		return nil, err
	}
	publicationKey, _, err := b.namespace.runtimePublicationKeys(ref.Partition, task.CreationIntentID)
	if err != nil {
		return nil, err
	}
	settlementKey, err := b.namespace.taskDetachedNoEffectKey(ref)
	if err != nil {
		return nil, err
	}
	keys := []string{b.identityKey, b.restoreKey, eKey, sKey, fKey, snapshotKey, iKey, publicationKey, settlementKey}
	points, revision, err := b.readCleanupDetachedNoEffectPoints(ctx, "source", keys, []int{maxRecordBytes, maxRecordBytes, 4096, 4096, 8192, maxRecordBytes, 4096, 8192, p.MaxDetachedNoEffectSettlementBytes})
	if err != nil {
		return nil, err
	}
	state, err := b.cleanupStateFromPoints(ref, points[2], points[3], revision)
	if err != nil || state == nil || state.record.TaskBirth != taskFence.create || state.record.Incarnation != located.Incarnation || state.execution.create <= taskFence.create {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	actualFinalSync, err := taskFinalSyncEntry(points[4], ref)
	if err != nil || actualFinalSync == nil || actualFinalSync.Revision != finalSync.Revision || *actualFinalSync.Record != *finalSync.Record {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	var snapshot SandboxSnapshotRecord
	var intent CreationIntentRecord
	var publication RuntimeEnrolledPublicationRecord
	if !enrolledImmutableKV(points[5]) || decodeDomainRecord(points[5], &snapshot) != nil || !operationPermanentKV(points[6]) || strictPreparationMetadata(points[6].Value, reflect.TypeOf(&intent)) != nil || decodeDomainRecord(points[6], &intent) != nil || !enrolledImmutableKV(points[7]) || decodeEnrolledRecord(points[7].Value, &publication) != nil || publication != read.Publication.Record {
		return nil, ErrCorruptRecord
	}
	snapshotWire, err := encodeDomainRecord(snapshot)
	if err != nil || snapshotWire != string(points[5].Value) {
		return nil, ErrCorruptRecord
	}
	intentWire, err := encodeDomainRecord(intent)
	if err != nil || intentWire != string(points[6].Value) {
		return nil, ErrCorruptRecord
	}
	if _, err = b.detachedCleanupSource(task, snapshot, intent, read.Publication); err != nil {
		return nil, err
	}
	entry, err := b.detachedNoEffectEntry(points[8], revision, ref)
	if err != nil {
		return nil, err
	}
	history := &cleanupDetachedNoEffectHistory{origin: b, task: task, state: state, snapshot: snapshot, intent: intent, publication: read.Publication, quiescence: q, finalSync: finalSync, taskFence: taskFence, quiescencePin: qpin, snapshotFence: ownTaskFence(points[5]), intentFence: ownTaskFence(points[6]), publicationFence: ownTaskFence(points[7]), finalSyncFence: ownTaskFence(points[4]), settlement: entry}
	history.self = history
	if entry != nil {
		if err = b.validateCleanupDetachedNoEffectFact(history, entry); err != nil {
			return nil, err
		}
	}
	return history, nil
}

// loadTaskDetachedNoEffect returns an owned authenticated live/pre-release
// historical fact. It does not mint a new Claim/Run or permit absence recovery.
func (b *Backend) loadTaskDetachedNoEffect(ctx context.Context, ref TaskReference) (*cleanupDetachedNoEffectEntry, error) {
	history, err := b.readCleanupDetachedNoEffectHistory(ctx, ref)
	if err != nil {
		return nil, err
	}
	return history.settlement, nil
}
