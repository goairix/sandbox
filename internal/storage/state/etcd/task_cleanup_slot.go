package etcd

import (
	"encoding/json"
	"errors"
	"math"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Evidence owns the initial immutable history. Neither it nor an observed
// state is authority: every mutation also checks the original charged owner
// and atomically pins the history/row revisions required by that transition.
type cleanupSlotEvidence struct {
	origin  *Backend
	bundle  *taskClaimBundle
	receipt taskFence
	record  cleanupExecutionRecord
}

type cleanupSlotState struct {
	record    cleanupExecutionRecord
	execution taskFence
	slot      *taskFence
	// Only the fixed E/S/receipt preparation read attaches this owned proof.
	claimEvidence *cleanupSlotEvidence
}

// The owner must still be the sole registered original native call. Merely
// retaining its pointer after finish cannot extend its token or deadline.
func (b *Backend) cleanupOwnerComparisons(o *workerDueOwner) ([]clientv3.Cmp, error) {
	if b == nil || b.archiveLife == nil || o == nil || o.call == nil || o.share == nil || o.partition == nil || o.share.origin != b || o.call.session == nil {
		return nil, ErrInvalidRecord
	}
	if err := o.postcheck(); err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if o.share.dueOwner != o || o.call.session.operation != o.call || o.share.kind != WorkerBudgetClaim || o.partition.session != o.call.session || o.share.session != o.call.session {
		return nil, ErrInvalidRecord
	}
	if b.requestTimeout <= 0 || b.requestTimeout > 3*time.Second || o.call.session.ttlSeconds != 60 {
		return nil, ErrInvalidConfiguration
	}
	if time.Until(o.call.session.deadline) <= 4*b.requestTimeout+time.Second {
		return nil, ErrGuardExpired
	}
	return b.workerComparisons(o.call.session, o.partition, o.share)
}

// Each point response is checked while its native header is still available.
// The callback-free helper has one RPC and keeps original ownership through
// validation. The wider public domain reader retains its old contract.
func (b *Backend) cleanupReadPoints(o *workerDueOwner, keys ...string) ([]*mvccpb.KeyValue, int64, error) {
	if _, err := b.cleanupOwnerComparisons(o); err != nil {
		return nil, 0, err
	}
	values := make([]string, len(keys))
	for i, key := range keys {
		if !validNamespaceKey(b.namespace, key) {
			return nil, 0, ErrInvalidRecord
		}
		values[i] = cleanupWirePadding(cleanupExecutionBytes)
	}
	projection := b.cleanupProjection("point", b.baseComparisons(), taskProtoPoints(keys), taskProtoResponse(keys, values))
	if err := validateCleanupWire(projection); err != nil {
		return nil, 0, err
	}
	rpc, cancel := o.call.requestContext()
	o.call.started = true
	r, err := b.client.Txn(rpc).If(b.baseComparisons()...).Then(workspaceSyncPointOps(keys)...).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return nil, 0, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err = o.postcheck(); err != nil {
		return nil, 0, err
	}
	if err = b.operationResponseHeader(r); err != nil {
		return nil, 0, err
	}
	if (*pb.TxnResponse)(r).Size() > maxMutationBytes {
		return nil, 0, ErrCorruptRecord
	}
	if !r.Succeeded {
		if _, err = b.stageEvidencePoints(r, []string{b.identityKey, b.restoreKey}); err != nil {
			return nil, 0, err
		}
		return nil, 0, ErrIdentityMismatch
	}
	if len(r.Responses) != len(keys) {
		return nil, 0, ErrCorruptRecord
	}
	points := make([]*mvccpb.KeyValue, len(keys))
	for i, op := range r.Responses {
		if op == nil {
			return nil, 0, ErrCorruptRecord
		}
		point := op.GetResponseRange()
		if point == nil || point.More || point.Count != int64(len(point.Kvs)) || len(point.Kvs) > 1 || !operationNestedHeader(point.Header, r.Header) {
			return nil, 0, ErrCorruptRecord
		}
		if len(point.Kvs) == 0 {
			continue
		}
		kv := point.Kvs[0]
		if kv == nil || string(kv.Key) != keys[i] || kv.Version < 1 || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > r.Header.Revision || (kv.Version == 1) != (kv.CreateRevision == kv.ModRevision) || len(kv.Value) > cleanupExecutionBytes {
			return nil, 0, ErrCorruptRecord
		}
		owned := *kv
		owned.Key = append([]byte(nil), kv.Key...)
		owned.Value = append([]byte(nil), kv.Value...)
		points[i] = &owned
	}
	return points, r.Header.Revision, nil
}

func (b *Backend) loadCleanupSlotEvidence(o *workerDueOwner, bundle *taskClaimBundle) (*cleanupSlotEvidence, error) {
	if bundle == nil || len(bundle.fences) != 8 || b.validateTaskReference(bundle.task.Reference) != nil || o == nil || o.partition == nil || o.partition.Reference().Partition != bundle.task.Reference.Partition {
		return nil, ErrInvalidRecord
	}
	// The locator is from the actual intent in the coherent original bundle.
	// The full digest is learned only from this receipt and then authenticated.
	_, key, err := b.stageKeys(bundle.initial.reference(cleanupWirePaddingDigest()))
	if err != nil {
		return nil, err
	}
	points, _, err := b.cleanupReadPoints(o, key)
	if err != nil {
		return nil, err
	}
	return b.cleanupEvidenceFromReceipt(bundle, points[0])
}

func (b *Backend) cleanupEvidenceFromReceipt(bundle *taskClaimBundle, kv *mvccpb.KeyValue) (*cleanupSlotEvidence, error) {
	if kv == nil || bundle == nil || len(bundle.fences) != 8 {
		return nil, ErrCorruptReceipt
	}
	var receipt stageReceipt
	if json.Unmarshal(kv.Value, &receipt) != nil || receipt.StageReference != bundle.initial.reference(receipt.Digest) || !validHexDigest(receipt.Digest) {
		return nil, ErrCorruptReceipt
	}
	if _, _, err := b.stageKeys(receipt.StageReference); err != nil {
		return nil, ErrCorruptReceipt
	}
	outcome, err := decodeReceipt(kv, receipt.StageReference)
	if err != nil || outcome != OutcomeCommitted || kv.CreateRevision != bundle.birth || kv.ModRevision != bundle.birth || kv.Version != 1 {
		return nil, ErrCorruptReceipt
	}
	wire, err := encodeReceipt(receipt.StageReference, outcome)
	if err != nil || wire != string(kv.Value) {
		return nil, ErrCorruptReceipt
	}
	for _, fence := range bundle.fences[:3] {
		if fence.create != bundle.birth || fence.mod != bundle.birth || fence.lease != 0 {
			return nil, ErrCorruptRecord
		}
	}
	taskWire, err := encodeTaskRecord(bundle.task)
	if err != nil || taskWire != bundle.fences[0].value {
		return nil, ErrCorruptRecord
	}
	r := cleanupExecutionRecord{Version: 1, Domain: cleanupExecutionDomain, Task: bundle.task.Reference, TaskBirth: bundle.birth, InitialAttempt: bundle.initial, TaskDigest: workerIdentityDigest(taskWire), InitialMutationDigest: receipt.Digest, InitialReceiptDigest: workerIdentityDigest(wire), Phase: "selecting", Confirmed: "queued"}
	r.Slot = cleanupCandidate(r.TaskDigest, 0)
	r.Incarnation = cleanupIncarnation(cleanupSlotRecordFromExecution(r))
	return &cleanupSlotEvidence{origin: b, bundle: bundle, receipt: ownTaskFence(kv), record: r}, nil
}

func (b *Backend) readCleanupSlot(o *workerDueOwner, ref TaskReference) (*cleanupSlotState, error) {
	if b.validateTaskReference(ref) != nil || o == nil || o.partition == nil || o.partition.Reference().Partition != ref.Partition {
		return nil, ErrInvalidRecord
	}
	key, err := b.namespace.cleanupExecutionKey(ref)
	if err != nil {
		return nil, err
	}
	points, revision, err := b.cleanupReadPoints(o, key)
	if err != nil || points[0] == nil {
		return nil, err
	}
	var r cleanupExecutionRecord
	if err = decodeCleanupExecution(points[0], revision, ref, &r); err != nil {
		return nil, err
	}
	state := &cleanupSlotState{record: r, execution: ownTaskFence(points[0])}
	if r.Phase != "occupied" {
		return state, nil
	}

	// A cold Task reference needs one routing read before the exact E/S read.
	// The second read must match the first E fence; never loop or treat an old
	// candidate's absence as resolution of an uncertain reservation.
	slotKey, err := b.namespace.cleanupExecutionSlotKey(r.Slot)
	if err != nil {
		return nil, err
	}
	points, revision, err = b.cleanupReadPoints(o, key, slotKey)
	if err != nil {
		return nil, err
	}
	if !state.execution.matches(points[0]) {
		return nil, ErrConflict
	}
	return b.cleanupStateFromPoints(ref, points[0], points[1], revision)
}

func (b *Backend) cleanupStateFromPoints(ref TaskReference, execution, slot *mvccpb.KeyValue, revision int64) (*cleanupSlotState, error) {
	var e cleanupExecutionRecord
	if err := decodeCleanupExecution(execution, revision, ref, &e); err != nil {
		return nil, err
	}
	var s cleanupExecutionSlotRecord
	if e.Phase != "occupied" || decodeCleanupExecutionSlot(slot, revision, ref.Namespace, e.Slot, &s) != nil || s != cleanupSlotRecordFromExecution(e) || slot.CreateRevision <= execution.CreateRevision || slot.CreateRevision > execution.ModRevision {
		return nil, ErrCorruptRecord
	}
	ownedSlot := ownTaskFence(slot)
	return &cleanupSlotState{record: e, execution: ownTaskFence(execution), slot: &ownedSlot}, nil
}

func (b *Backend) cleanupEvidenceComparisons(o *workerDueOwner, evidence *cleanupSlotEvidence) ([]clientv3.Cmp, error) {
	if evidence == nil || evidence.origin != b || evidence.bundle == nil || len(evidence.bundle.fences) != 8 || o == nil || o.partition == nil || o.partition.Reference().Partition != evidence.record.Task.Partition {
		return nil, ErrInvalidRecord
	}
	cmps, err := b.cleanupOwnerComparisons(o)
	if err != nil {
		return nil, err
	}
	for _, fence := range []taskFence{evidence.bundle.fences[0], evidence.bundle.fences[1], evidence.receipt} {
		cmps = append(cmps, fence.comparisons()...)
	}
	return cmps, nil
}

func (b *Backend) initializeCleanupSlot(o *workerDueOwner, evidence *cleanupSlotEvidence) error {
	cmps, err := b.cleanupEvidenceComparisons(o, evidence)
	if err != nil {
		return err
	}
	if _, err = b.cleanupSlotWireInventory(o, evidence, &cleanupSlotState{record: evidence.record}); err != nil {
		return err
	}
	key, err := b.namespace.cleanupExecutionKey(evidence.record.Task)
	if err != nil {
		return err
	}
	value, err := encodeCleanupExecution(evidence.record)
	if err != nil {
		return err
	}
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	return b.cleanupWrite(o, "initialize", cmps, []Write{{Key: key, Value: []byte(value)}})
}

func (b *Backend) reserveCleanupSlot(o *workerDueOwner, evidence *cleanupSlotEvidence, state *cleanupSlotState) error {
	cmps, err := b.cleanupEvidenceComparisons(o, evidence)
	if err != nil {
		return err
	}
	if state == nil || state.record.Phase != "selecting" || state.record.Validate() != nil || state.record.Task != evidence.record.Task || state.record.Incarnation != evidence.record.Incarnation || state.execution.mod <= 0 {
		return ErrInvalidRecord
	}
	if err = b.validateCleanupState(state); err != nil {
		return err
	}
	if _, err = b.cleanupSlotWireInventory(o, evidence, state); err != nil {
		return err
	}
	key, err := b.namespace.cleanupExecutionSlotKey(state.record.Slot)
	if err != nil {
		return err
	}
	e := state.record
	e.Phase, e.Confirmed = "occupied", "reserved"
	eValue, err := encodeCleanupExecution(e)
	if err != nil {
		return err
	}
	sValue, err := encodeCleanupExecutionSlot(cleanupSlotRecordFromExecution(e))
	if err != nil {
		return err
	}
	cmps = append(cmps, state.execution.comparisons()...)
	cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	return b.cleanupWrite(o, "reserve", cmps, []Write{{Key: key, Value: []byte(sValue)}, {Key: state.execution.key, Value: []byte(eValue)}})
}

// This old-E CAS, rather than an empty observation, arbitrates a late reserve.
// Once sent, an unknown result retains the same old/new selection tuple.
func (b *Backend) rotateCleanupSlot(o *workerDueOwner, state *cleanupSlotState) error {
	cmps, err := b.cleanupOwnerComparisons(o)
	if err != nil {
		return err
	}
	if state == nil || state.record.Phase != "selecting" || state.record.Validate() != nil || state.record.Selection == math.MaxUint64 || state.execution.mod <= 0 || state.record.Task.Partition != o.partition.Reference().Partition {
		return ErrInvalidRecord
	}
	if err = b.validateCleanupState(state); err != nil {
		return err
	}
	r := state.record
	r.Selection++
	r.Slot = cleanupCandidate(r.TaskDigest, r.Selection)
	wire, err := encodeCleanupExecution(r)
	if err != nil {
		return err
	}
	cmps = append(cmps, state.execution.comparisons()...)
	return b.cleanupWrite(o, "rotate", cmps, []Write{{Key: state.execution.key, Value: []byte(wire)}})
}

// Stored bytes and the exact key must agree with the decoded state before a
// full fence can be reused. A private observed struct is not a write template.
func (b *Backend) validateCleanupState(state *cleanupSlotState) error {
	if state == nil || b.validateTaskReference(state.record.Task) != nil {
		return ErrInvalidRecord
	}
	key, err := b.namespace.cleanupExecutionKey(state.record.Task)
	if err != nil {
		return err
	}
	value, err := encodeCleanupExecution(state.record)
	if err != nil || state.execution.key != key || state.execution.value != value || state.execution.lease != 0 || state.execution.create <= state.record.TaskBirth || state.execution.mod < state.execution.create {
		return ErrInvalidRecord
	}
	return nil
}

// A mutation owns its original call until native return and strict response
// validation. Neither cancellation nor uncertain acknowledgement frees S.
func (b *Backend) cleanupWrite(o *workerDueOwner, name string, cmps []clientv3.Cmp, writes []Write) error {
	projection := b.cleanupProjection(name, cmps, taskProtoWrites(writes), taskProtoPutResponse(len(writes)))
	if err := validateCleanupWire(projection); err != nil {
		return err
	}
	if _, err := b.cleanupOwnerComparisons(o); err != nil {
		return err
	}
	ops := make([]clientv3.Op, len(writes))
	for i, write := range writes {
		if write.Delete || !validNamespaceKey(b.namespace, write.Key) || len(write.Value) > cleanupExecutionBytes {
			return ErrInvalidMutation
		}
		ops[i] = clientv3.OpPut(write.Key, string(write.Value))
	}
	rpc, cancel := o.call.requestContext()
	o.call.started = true
	r, err := b.client.Txn(rpc).If(cmps...).Then(ops...).Else(clientv3.OpGet(b.identityKey), clientv3.OpGet(b.restoreKey)).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}
	if err = b.operationResponseHeader(r); err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if (*pb.TxnResponse)(r).Size() > maxMutationBytes {
		return ErrOutcomeUnknown
	}
	if !r.Succeeded {
		if _, err = b.stageEvidencePoints(r, []string{b.identityKey, b.restoreKey}); err != nil {
			return err
		}
		return ErrConflict
	}
	if !operationPutResponses(r, len(writes)) {
		return ErrOutcomeUnknown
	}
	if err = o.postcheck(); err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	return nil
}
