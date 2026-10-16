package etcd

import (
	"encoding/json"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// cleanupTerminalEvidence authenticates only the permanent birth history. It
// is neither a TaskClaim bundle nor physical authority: settlement separately
// requires the original private release proof and exact joined E/S occupation.
type cleanupTerminalEvidence struct {
	origin                *Backend
	record                cleanupExecutionSlotRecord
	task, intent, receipt taskFence
}

func (b *Backend) cleanupTerminalHistoryKeys(r cleanupExecutionRecord) ([]string, error) {
	if b.validateTaskReference(r.Task) != nil || !taskAttemptMatches(r.InitialAttempt, r.Task) || !validHexDigest(r.InitialMutationDigest) {
		return nil, ErrInvalidRecord
	}
	task, err := b.namespace.taskKey(r.Task)
	if err != nil {
		return nil, err
	}
	intent, err := b.namespace.cleanupIntentKey(r.Task)
	if err != nil {
		return nil, err
	}
	link, err := b.namespace.taskLinkKey(r.Task)
	if err != nil {
		return nil, err
	}
	_, receipt, err := b.stageKeys(r.InitialAttempt.reference(r.InitialMutationDigest))
	if err != nil {
		return nil, err
	}
	return []string{task, intent, link, receipt}, nil
}

// loadCleanupTerminalEvidence is one charged four-point read. Its receipt
// locator comes from E only for routing; the actual original intent and the
// common immutable birth must authenticate that locator before it is retained.
func (b *Backend) loadCleanupTerminalEvidence(owner *workerDueOwner, state *cleanupSlotState) (*cleanupTerminalEvidence, error) {
	if state == nil || owner == nil || owner.partition == nil || owner.partition.Reference().Partition != state.record.Task.Partition {
		return nil, ErrInvalidRecord
	}
	keys, err := b.cleanupTerminalHistoryKeys(state.record)
	if err != nil {
		return nil, err
	}
	points, revision, err := b.cleanupReadPoints(owner, keys...)
	if err != nil {
		return nil, err
	}
	return b.cleanupTerminalEvidenceFromPoints(state, points, revision)
}

// cleanupTerminalEvidenceFromPoints is a pure validator of native observations,
// in Task/intent/link/initial-receipt order. All four records remain permanent
// until the namespace restore epoch ends; no lease/death or deleted owner key
// can substitute for their exact shared birth and canonical identity.
func (b *Backend) cleanupTerminalEvidenceFromPoints(state *cleanupSlotState, points []*mvccpb.KeyValue, revision int64) (*cleanupTerminalEvidence, error) {
	if b.validateCleanupState(state) != nil || state.record.Phase != "occupied" || state.record.Run == nil || !state.record.Run.Joined || state.slot == nil || len(points) != 4 || revision < state.execution.mod {
		return nil, ErrCorruptRecord
	}
	r := state.record
	keys, err := b.cleanupTerminalHistoryKeys(r)
	if err != nil {
		return nil, err
	}
	slotKey, err := b.namespace.cleanupExecutionSlotKey(r.Slot)
	if err != nil {
		return nil, err
	}
	slotWire, err := encodeCleanupExecutionSlot(cleanupSlotRecordFromExecution(r))
	if err != nil || state.slot.key != slotKey || state.slot.value != slotWire || state.slot.lease != 0 || state.slot.create != state.slot.mod || state.slot.create <= state.execution.create || state.slot.mod > state.execution.mod {
		return nil, ErrCorruptRecord
	}
	for i, kv := range points {
		if kv == nil || string(kv.Key) != keys[i] || kv.Lease != 0 || kv.Version != 1 || kv.CreateRevision != r.TaskBirth || kv.ModRevision != r.TaskBirth || kv.ModRevision > revision || len(kv.Value) > cleanupExecutionBytes {
			return nil, ErrCorruptRecord
		}
	}
	var task TaskRecord
	var intent CleanupIntentRecord
	var link TaskLinkRecord
	if decodeTaskRecord(points[0], &task) != nil || decodeCleanupIntentRecord(points[1], &intent) != nil || decodeTaskLinkRecord(points[2], &link) != nil || task.Reference != r.Task || intent.Task != task || link.Reference != r.Task || intent.Attempt != r.InitialAttempt {
		return nil, ErrCorruptRecord
	}
	taskWire, err := encodeTaskRecord(task)
	if err != nil || taskWire != string(points[0].Value) {
		return nil, ErrCorruptRecord
	}
	intentWire, err := encodeCleanupIntentRecord(intent)
	if err != nil || intentWire != string(points[1].Value) {
		return nil, ErrCorruptRecord
	}
	linkWire, err := encodeTaskLinkRecord(link)
	if err != nil || linkWire != string(points[2].Value) {
		return nil, ErrCorruptRecord
	}

	var receipt stageReceipt
	if json.Unmarshal(points[3].Value, &receipt) != nil || !validHexDigest(receipt.Digest) || receipt.StageReference != intent.Attempt.reference(receipt.Digest) || receipt.Digest != r.InitialMutationDigest {
		return nil, ErrCorruptReceipt
	}
	outcome, err := decodeReceipt(points[3], receipt.StageReference)
	if err != nil || outcome != OutcomeCommitted {
		return nil, ErrCorruptReceipt
	}
	receiptWire, err := encodeReceipt(receipt.StageReference, outcome)
	if err != nil || receiptWire != string(points[3].Value) {
		return nil, ErrCorruptReceipt
	}
	expected := cleanupSlotRecordFromExecution(r)
	expected.TaskDigest = workerIdentityDigest(taskWire)
	expected.InitialMutationDigest = receipt.Digest
	expected.InitialReceiptDigest = workerIdentityDigest(receiptWire)
	expected.Incarnation = cleanupIncarnation(expected)
	if expected != cleanupSlotRecordFromExecution(r) {
		return nil, ErrCorruptRecord
	}
	return &cleanupTerminalEvidence{origin: b, record: expected, task: ownTaskFence(points[0]), intent: ownTaskFence(points[1]), receipt: ownTaskFence(points[3])}, nil
}

// The link is authenticated at the same immutable common birth and retained
// permanently, as in initial reservation. W/T/I/R are the original settlement
// comparisons; adding the already authenticated link would change that budget.
func (b *Backend) cleanupTerminalComparisons(owner *workerDueOwner, history *cleanupTerminalEvidence) ([]clientv3.Cmp, error) {
	if history == nil || history.origin != b || history.record.Validate() != nil || owner == nil || owner.partition == nil || owner.partition.Reference().Partition != history.record.Task.Partition {
		return nil, ErrInvalidRecord
	}
	cmps, err := b.cleanupOwnerComparisons(owner)
	if err != nil {
		return nil, err
	}
	for _, fence := range []taskFence{history.task, history.intent, history.receipt} {
		cmps = append(cmps, fence.comparisons()...)
	}
	return cmps, nil
}
