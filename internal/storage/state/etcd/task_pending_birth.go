package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
)

// This immutable, private observation authenticates permanent Task birth and
// optional current metadata. Nil optional fences mean observed absence, never
// terminal success. It carries no TaskClaim, occupied slot or physical proof.
type cleanupBirthEvidence struct {
	origin    *Backend
	self      *cleanupBirthEvidence
	partition *WorkerPartitionClaim
	record    TaskRecord
	initial   StageAttemptLocator

	birth, revision int64
	task            taskFence
	intent          taskFence
	link            taskFence
	receipt         taskFence

	execution                        *cleanupExecutionRecord
	executionFence, pending, release *taskFence
	releaseRecord                    *localOwnerReleaseRecord
}

// Validate real membership and timing under the original lifecycle lock before
// credit is spent. The second check after admission closes the scheduling gap;
// that refusal is charged, but performs no native work and receives no refund.
func (b *Backend) beginCleanupBirthOwner(ctx context.Context, p *WorkerPartitionClaim, ranges *WorkerBudgetShare, terminal bool) (*workerDueOwner, context.CancelFunc, error) {
	if ctx == nil || !b.validWorkerBackend() {
		return nil, nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	phase, margin, err := cleanupBirthPhaseTiming(b.requestTimeout, terminal)
	if err != nil {
		return nil, nil, err
	}
	b.archiveLife.mu.Lock()
	err = b.cleanupBirthMembership(p, ranges, margin)
	b.archiveLife.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	bounded, stop := context.WithTimeout(ctx, phase)
	o, err := b.beginDuePage(bounded, p, ranges, nil)
	if err != nil {
		stop()
		return nil, nil, err
	}
	return o, stop, nil
}

// Requires archiveLife.mu. Original lifecycle checks precede all timing checks;
// a short remaining lease cannot manufacture a new session or extend its clock.
func (b *Backend) cleanupBirthMembership(p *WorkerPartitionClaim, ranges *WorkerBudgetShare, margin time.Duration) error {
	if !b.validWorkerPartition(p) || !b.validWorkerBudget(ranges) || ranges.kind != WorkerBudgetRange || ranges.session != p.session {
		return ErrInvalidRecord
	}
	if b.archiveLife.closed {
		return context.Canceled
	}
	if p.release != nil || p.released || p.lost || ranges.release != nil || ranges.released || ranges.lost || p.session.release != nil {
		return ErrGuardExpired
	}
	if err := p.session.live(); err != nil {
		return err
	}
	if p.session.ttlSeconds != 60 {
		return ErrInvalidConfiguration
	}
	if time.Until(p.session.deadline) <= margin {
		return ErrGuardExpired
	}
	return nil
}

// A concrete cohort can run only under the original admitted Range owner.
// Checking a diagnostic partition/session reference is insufficient.
func (b *Backend) validateCleanupBirthOwner(o *workerDueOwner, margin time.Duration) error {
	if !b.validWorkerBackend() || o == nil || o.call == nil || o.call.ctx == nil || o.partition == nil || o.share == nil || o.call.session == nil {
		return ErrInvalidRecord
	}
	if err := o.call.ctx.Err(); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if err := b.cleanupBirthMembership(o.partition, o.share, margin); err != nil {
		return err
	}
	if o.share.dueOwner != o || o.call.session != o.partition.session || o.call.session.operation != o.call {
		return ErrInvalidRecord
	}
	return nil
}

// The UUID is only a route to actual I bytes. Four finite RPCs at most share
// one original 256 KiB Range charge: W preguard, I, seven-point cohort, W
// postguard. Typed history refusal after successful guards preserves the live
// worker; transport/envelope/guard/cancellation failures lose its session.
func (b *Backend) readCleanupBirth(ctx context.Context, p *WorkerPartitionClaim, ranges *WorkerBudgetShare, taskID string) (_ *cleanupBirthEvidence, resultErr error) {
	if !validPreparationUUID(taskID) {
		return nil, ErrInvalidRecord
	}
	o, stop, err := b.beginCleanupBirthOwner(ctx, p, ranges, false)
	if err != nil {
		return nil, err
	}
	defer stop()
	defer o.finish()
	guardedRefusal := false
	defer func() {
		if resultErr != nil && o.call.started && !guardedRefusal {
			o.call.lose()
		}
	}()

	if err = b.preflightCleanupBirthRead(o, taskID); err != nil {
		return nil, err
	}
	intentKey, err := b.namespace.Key("p", fmt.Sprintf("%02x", p.partition), "cleanup-intents", taskID)
	if err != nil {
		return nil, err
	}
	_, margin, err := cleanupBirthPhaseTiming(b.requestTimeout, false)
	if err != nil {
		return nil, err
	}
	if err = b.validateCleanupBirthOwner(o, margin); err != nil {
		return nil, err
	}
	if err = b.workerGuardCheck(o.call, p, ranges); err != nil {
		return nil, err
	}
	values, err := b.readEnrolledPoints(o.call.ctx, []string{intentKey}, []int{4096})
	if err != nil {
		return nil, errors.Join(ErrOutcomeUnknown, err)
	}
	routing, validationErr := b.decodeCleanupBirthRouting(p, taskID, values[0])
	var evidence *cleanupBirthEvidence
	if validationErr == nil {
		points, revision, err := b.readCleanupBirthCohort(o, routing)
		if err != nil {
			return nil, errors.Join(ErrOutcomeUnknown, err)
		}
		evidence, validationErr = b.cleanupBirthFromPoints(p, values[0], points, revision)
	}

	// Even a typed refusal audits the original worker fences before allowing
	// another candidate to use this session. Only after both checks may an
	// optional refusal expose a sealed, separate index-only observation.
	if err = b.workerGuardCheck(o.call, p, ranges); err != nil {
		return nil, errors.Join(err, validationErr)
	}
	if err = o.postcheck(); err != nil {
		return nil, errors.Join(err, validationErr)
	}
	if validationErr != nil {
		var refusal *cleanupBirthOptionalRefusal
		if errors.As(validationErr, &refusal) && refusal.index != nil {
			refusal.index.self = refusal.index
		}
		guardedRefusal = true
		return nil, validationErr
	}
	return evidence, nil
}

// Initial discovery has an authenticated native envelope but is still only a
// route. The cohort re-reads this exact immutable I before retaining evidence.
func (b *Backend) decodeCleanupBirthRouting(p *WorkerPartitionClaim, taskID string, kv *mvccpb.KeyValue) (CleanupIntentRecord, error) {
	var r CleanupIntentRecord
	if kv == nil {
		return r, ErrConflict
	}
	if kv.Version != 1 || kv.Lease != 0 || kv.CreateRevision <= 0 || kv.ModRevision != kv.CreateRevision || decodeCleanupIntentRecord(kv, &r) != nil {
		return CleanupIntentRecord{}, ErrCorruptRecord
	}
	ref := r.Task.Reference
	if b.validateTaskReference(ref) != nil || p == nil || ref.Partition != p.partition || ref.TaskID != taskID {
		return CleanupIntentRecord{}, ErrCorruptRecord
	}
	key, keyErr := b.namespace.cleanupIntentKey(ref)
	wire, wireErr := encodeCleanupIntentRecord(r)
	if keyErr != nil || wireErr != nil || string(kv.Key) != key || string(kv.Value) != wire {
		return CleanupIntentRecord{}, ErrCorruptRecord
	}
	return r, nil
}

// This exact cohort retains its real native header revision. The shared key/cap
// builder fixes T/I/link/receipt/E/P/release; callers cannot add arbitrary reads.
// Current runtime ownership is intentionally absent from this historical join.
func (b *Backend) readCleanupBirthCohort(owner *workerDueOwner, routing CleanupIntentRecord) ([]*mvccpb.KeyValue, int64, error) {
	if err := b.validateCleanupBirthOwner(owner, 0); err != nil {
		return nil, 0, err
	}
	if routing.Validate() != nil || routing.Task.Reference.Partition != owner.partition.partition {
		return nil, 0, ErrInvalidRecord
	}
	keys, caps, err := b.cleanupBirthCohortKeys(routing.Task.Reference, routing.Attempt)
	if err != nil {
		return nil, 0, err
	}
	all := append([]string{b.identityKey, b.restoreKey}, keys...)
	allCaps := append([]int{maxIdentityJSONBytes, maxNamespaceComponentBytes}, caps...)
	projection := cleanupBirthPointsProjection("birth-cohort", b.baseComparisons(), all, allCaps)
	if err = validateCleanupWire(projection); err != nil {
		return nil, 0, err
	}

	rpc, cancel := owner.call.requestContext()
	if err = rpc.Err(); err != nil {
		cancel()
		return nil, 0, err
	}
	ops := workspaceSyncPointOps(all)
	owner.call.started = true
	response, err := b.client.Txn(rpc).If(b.baseComparisons()...).Then(ops...).Else(ops...).Commit()
	rpcErr := rpc.Err()
	cancel()
	if err != nil || rpcErr != nil {
		return nil, 0, errors.Join(ErrOutcomeUnknown, err, rpcErr)
	}

	if !enrolledResponseBound(response) {
		return nil, 0, ErrCorruptRecord
	}
	points, err := b.stageEvidencePoints(response, all)
	if err != nil {
		return nil, 0, err
	}
	if !response.Succeeded {
		return nil, 0, ErrIdentityMismatch
	}

	owned := make([]*mvccpb.KeyValue, len(keys))
	for i, kv := range points[2:] {
		if kv == nil {
			continue
		}
		if kv.Version < 1 || (kv.Version == 1) != (kv.CreateRevision == kv.ModRevision) || len(kv.Value) > caps[i] {
			return nil, 0, ErrCorruptRecord
		}
		copy := *kv
		copy.Key, copy.Value = bytes.Clone(kv.Key), bytes.Clone(kv.Value)
		owned[i] = &copy
	}

	if err = owner.postcheck(); err != nil {
		return nil, 0, err
	}
	return owned, response.Header.Revision, nil
}

// Validate all four canonical immutable birth records independently of current
// owner/control/runtime or any E/S occupation. Optional metadata is bound to
// that birth but remains an observation requiring fresh fences at later CAS.
func (b *Backend) cleanupBirthFromPoints(p *WorkerPartitionClaim, discovered *mvccpb.KeyValue, points []*mvccpb.KeyValue, revision int64) (*cleanupBirthEvidence, error) {
	if !b.validWorkerBackend() || discovered == nil || len(points) != 7 || revision <= 0 {
		return nil, ErrCorruptRecord
	}
	b.archiveLife.mu.Lock()
	valid := b.validWorkerPartition(p)
	b.archiveLife.mu.Unlock()
	if !valid {
		return nil, ErrInvalidRecord
	}
	var intent CleanupIntentRecord
	if decodeCleanupIntentRecord(discovered, &intent) != nil {
		return nil, ErrCorruptRecord
	}
	routing, err := b.decodeCleanupBirthRouting(p, intent.Task.Reference.TaskID, discovered)
	if err != nil {
		return nil, err
	}
	keys, caps, err := b.cleanupBirthCohortKeys(routing.Task.Reference, routing.Attempt)
	if err != nil || !ownTaskFence(discovered).matches(points[1]) {
		return nil, ErrCorruptRecord
	}

	birth := discovered.CreateRevision
	for i, kv := range points {
		if kv == nil {
			if i < 4 {
				return nil, ErrCorruptRecord
			}
			continue
		}
		if string(kv.Key) != keys[i] || kv.CreateRevision <= 0 || kv.ModRevision < kv.CreateRevision || kv.ModRevision > revision || kv.Version < 1 || (kv.Version == 1) != (kv.CreateRevision == kv.ModRevision) || len(kv.Value) > caps[i] {
			return nil, ErrCorruptRecord
		}
		if i < 4 && (kv.Version != 1 || kv.Lease != 0 || kv.CreateRevision != birth || kv.ModRevision != birth) {
			return nil, ErrCorruptRecord
		}
	}

	var task TaskRecord
	var link TaskLinkRecord
	if decodeTaskRecord(points[0], &task) != nil || decodeTaskLinkRecord(points[2], &link) != nil || task != routing.Task || link.Reference != task.Reference || task.ControlRevision >= birth {
		return nil, ErrCorruptRecord
	}
	taskWire, taskErr := encodeTaskRecord(task)
	linkWire, linkErr := encodeTaskLinkRecord(link)
	if taskErr != nil || linkErr != nil || taskWire != string(points[0].Value) || linkWire != string(points[2].Value) {
		return nil, ErrCorruptRecord
	}

	var receipt stageReceipt
	if json.Unmarshal(points[3].Value, &receipt) != nil || !validHexDigest(receipt.Digest) || receipt.StageReference != routing.Attempt.reference(receipt.Digest) {
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

	evidence := &cleanupBirthEvidence{
		origin: b, partition: p, record: task, initial: routing.Attempt, birth: birth, revision: revision,
		task: ownTaskFence(points[0]), intent: ownTaskFence(points[1]), link: ownTaskFence(points[2]), receipt: ownTaskFence(points[3]),
	}
	if err = b.cleanupBirthOptionalEvidence(evidence, points[4:], receipt.Digest); err != nil {
		// All mandatory records and native envelopes above are authentic. Keep
		// raw P independently: an earlier E refusal must not manufacture P
		// absence. Its canonical validity is rechecked before indexing.
		index := &cleanupBirthIndexEvidence{
			origin: b, partition: p, record: task, initial: routing.Attempt, birth: birth, revision: revision,
			task: evidence.task, intent: evidence.intent, link: evidence.link, receipt: evidence.receipt,
		}
		if points[5] != nil {
			fence := ownTaskFence(points[5])
			index.pending = &fence
		}
		return nil, &cleanupBirthOptionalRefusal{cause: err, index: index}
	}
	evidence.self = evidence
	return evidence, nil
}

// E/P/release are independent observations. In particular a canonical release
// is only preliminary: no signed termination history is read by this function.
func (b *Backend) cleanupBirthOptionalEvidence(e *cleanupBirthEvidence, points []*mvccpb.KeyValue, mutationDigest string) error {
	ref := e.record.Reference
	taskDigest := workerIdentityDigest(e.task.value)
	if kv := points[0]; kv != nil {
		var r cleanupExecutionRecord
		if decodeCleanupExecution(kv, e.revision, ref, &r) != nil || r.TaskBirth != e.birth || r.InitialAttempt != e.initial || r.TaskDigest != taskDigest || r.InitialMutationDigest != mutationDigest || r.InitialReceiptDigest != workerIdentityDigest(e.receipt.value) || r.Incarnation != cleanupIncarnation(cleanupSlotRecordFromExecution(r)) {
			return ErrCorruptRecord
		}
		fence := ownTaskFence(kv)
		e.execution, e.executionFence = &r, &fence
	}
	if kv := points[1]; kv != nil {
		r, err := decodeCleanupPending(b.namespace, b.restoreEpoch, kv, e.revision)
		if err != nil || r.Reference != ref || r.TaskDigest != taskDigest || kv.CreateRevision < e.birth {
			return ErrCorruptRecord
		}
		fence := ownTaskFence(kv)
		e.pending = &fence
	}
	if kv := points[2]; kv != nil {
		r, err := decodeLocalOwnerRelease(kv)
		if err != nil || r.Task != ref || r.TaskBirth != e.birth || r.Runtime != e.record.Runtime || r.WorkspaceHash != e.record.WorkspaceHash || r.Generation != e.record.Generation {
			return ErrCorruptRecord
		}
		fence := ownTaskFence(kv)
		e.release, e.releaseRecord = &fence, &r
	}
	return nil
}

// Before I is known, only this wire projection uses maximal routing fields.
// They are never dispatched, retained as evidence or treated as a native view.
func (b *Backend) preflightCleanupBirthRead(owner *workerDueOwner, taskID string) error {
	if err := b.validateCleanupBirthOwner(owner, 0); err != nil {
		return err
	}
	if !validPreparationUUID(taskID) {
		return ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	cmps, err := b.workerComparisons(owner.call.session, owner.partition, owner.share)
	b.archiveLife.mu.Unlock()
	if err != nil {
		return err
	}
	ref := TaskReference{Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, Partition: owner.partition.partition, TaskID: taskID, SandboxID: strings.Repeat("s", maxNamespaceComponentBytes)}
	initial := StageAttemptLocator{Namespace: ref.Namespace, RestoreEpoch: ref.RestoreEpoch, Partition: ref.Partition, RequestID: strings.Repeat("r", 128), StageID: strings.Repeat("s", 128), AttemptID: taskID}
	_, err = b.cleanupBirthWireProjections(cmps, ref, initial, owner.call.session, false)
	return err
}
