package etcd

import "encoding/json"

// Revalidate owned canonical observations without fabricating MVCC versions or
// read revisions. This shared check creates no release authority: settled or
// unsealed history is still eligible for conservative pending publication.
func (b *Backend) validateCleanupBirthEvidence(p *WorkerPartitionClaim, birth *cleanupBirthEvidence) error {
	if !b.validWorkerBackend() || birth == nil || birth.self != birth || birth.origin != b || birth.partition != p {
		return ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	valid := b.validWorkerPartition(p)
	b.archiveLife.mu.Unlock()
	if !valid || birth.record.Validate() != nil || birth.record.Reference.Partition != p.partition || birth.birth <= 0 || birth.revision < birth.birth || birth.record.ControlRevision >= birth.birth {
		return ErrInvalidRecord
	}
	keys, caps, err := b.cleanupBirthCohortKeys(birth.record.Reference, birth.initial)
	if err != nil {
		return err
	}
	for i, fence := range []taskFence{birth.task, birth.intent, birth.link, birth.receipt} {
		if fence.key != keys[i] || fence.lease != 0 || fence.create != birth.birth || fence.mod != birth.birth || len(fence.value) > caps[i] {
			return ErrInvalidRecord
		}
	}

	task, taskErr := encodeTaskRecord(birth.record)
	intent, intentErr := encodeCleanupIntentRecord(CleanupIntentRecord{Version: 1, Task: birth.record, Attempt: birth.initial})
	link, linkErr := encodeTaskLinkRecord(TaskLinkRecord{Version: 1, Reference: birth.record.Reference})
	if taskErr != nil || intentErr != nil || linkErr != nil || task != birth.task.value || intent != birth.intent.value || link != birth.link.value {
		return ErrInvalidRecord
	}
	var receipt stageReceipt
	if json.Unmarshal([]byte(birth.receipt.value), &receipt) != nil || !validHexDigest(receipt.Digest) || receipt.StageReference != birth.initial.reference(receipt.Digest) || receipt.Outcome != OutcomeCommitted {
		return ErrInvalidRecord
	}
	wire, err := encodeReceipt(receipt.StageReference, OutcomeCommitted)
	if err != nil || wire != birth.receipt.value {
		return ErrInvalidRecord
	}
	return validateCleanupBirthMetadata(birth, keys, receipt.Digest)
}

// Optional records retain exact bytes and native revision bounds. A preliminary
// release may be absent even for settled E; only the signed release validator
// joins E's release pin to an actual proof before any pending deletion.
func validateCleanupBirthMetadata(birth *cleanupBirthEvidence, keys []string, mutationDigest string) error {
	ref := birth.record.Reference
	digest := workerIdentityDigest(birth.task.value)
	if (birth.execution == nil) != (birth.executionFence == nil) {
		return ErrInvalidRecord
	}
	if r := birth.execution; r != nil {
		f := birth.executionFence
		wire, err := encodeCleanupExecution(*r)
		if err != nil || f.key != keys[4] || f.lease != 0 || f.create <= birth.birth || f.mod < f.create || f.mod > birth.revision || wire != f.value {
			return ErrInvalidRecord
		}
		if r.Task != ref || r.TaskBirth != birth.birth || r.InitialAttempt != birth.initial || r.TaskDigest != digest || r.InitialMutationDigest != mutationDigest || r.InitialReceiptDigest != workerIdentityDigest(birth.receipt.value) || r.Incarnation != cleanupIncarnation(cleanupSlotRecordFromExecution(*r)) {
			return ErrInvalidRecord
		}
	}
	if f := birth.pending; f != nil {
		wire, err := encodeCleanupPending(cleanupPendingRecord{Version: 1, Reference: ref, TaskDigest: digest})
		if err != nil || f.key != keys[5] || f.lease != 0 || f.create < birth.birth || f.mod != f.create || f.mod > birth.revision || wire != f.value {
			return ErrInvalidRecord
		}
	}

	if (birth.release == nil) != (birth.releaseRecord == nil) {
		return ErrInvalidRecord
	}
	if birth.release == nil {
		return nil
	}
	r, f := birth.releaseRecord, birth.release
	wire, err := encodeLocalOwnerRelease(*r)
	if err != nil || f.key != keys[6] || f.lease != 0 || f.create <= birth.birth || f.mod != f.create || f.mod > birth.revision || f.create <= r.Termination.ModRevision || f.create <= r.Claim.CreateRevision || wire != f.value {
		return ErrInvalidRecord
	}
	if r.Task != ref || r.TaskBirth != birth.birth || r.Runtime != birth.record.Runtime || r.WorkspaceHash != birth.record.WorkspaceHash || r.Generation != birth.record.Generation {
		return ErrInvalidRecord
	}
	return nil
}
