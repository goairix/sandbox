package etcd

import "encoding/json"

// Revalidate owned canonical observations without fabricating MVCC versions or
// read revisions. This shared check creates no release authority: settled or
// unsealed history is still eligible for conservative pending publication.
func (b *Backend) validateCleanupBirthEvidence(p *WorkerPartitionClaim, birth *cleanupBirthEvidence) error {
	if !b.validWorkerBackend() || birth == nil || birth.self != birth || birth.origin != b || birth.partition != p {
		return ErrInvalidRecord
	}
	keys, digest, err := b.validateCleanupBirthMandatory(p, birth.record, birth.initial, birth.birth, birth.revision, [4]taskFence{birth.task, birth.intent, birth.link, birth.receipt})
	if err != nil {
		return err
	}
	return validateCleanupBirthMetadata(birth, keys, digest)
}

// Both evidence types revalidate the same immutable mandatory cohort. This
// helper accepts no optional metadata and grants no seal or terminal authority;
// each caller must first authenticate its own original evidence pointer.
func (b *Backend) validateCleanupBirthMandatory(p *WorkerPartitionClaim, record TaskRecord, initial StageAttemptLocator, birth, revision int64, fences [4]taskFence) ([]string, string, error) {
	b.archiveLife.mu.Lock()
	valid := b.validWorkerPartition(p)
	b.archiveLife.mu.Unlock()
	if !valid || record.Validate() != nil || record.Reference.Partition != p.partition || birth <= 0 || revision < birth || record.ControlRevision >= birth {
		return nil, "", ErrInvalidRecord
	}
	keys, caps, err := b.cleanupBirthCohortKeys(record.Reference, initial)
	if err != nil {
		return nil, "", err
	}
	for i, fence := range fences {
		if fence.key != keys[i] || fence.lease != 0 || fence.create != birth || fence.mod != birth || len(fence.value) > caps[i] {
			return nil, "", ErrInvalidRecord
		}
	}

	task, taskErr := encodeTaskRecord(record)
	intent, intentErr := encodeCleanupIntentRecord(CleanupIntentRecord{Version: 1, Task: record, Attempt: initial})
	link, linkErr := encodeTaskLinkRecord(TaskLinkRecord{Version: 1, Reference: record.Reference})
	if taskErr != nil || intentErr != nil || linkErr != nil || task != fences[0].value || intent != fences[1].value || link != fences[2].value {
		return nil, "", ErrInvalidRecord
	}
	var receipt stageReceipt
	if json.Unmarshal([]byte(fences[3].value), &receipt) != nil || !validHexDigest(receipt.Digest) || receipt.StageReference != initial.reference(receipt.Digest) || receipt.Outcome != OutcomeCommitted {
		return nil, "", ErrInvalidRecord
	}
	wire, err := encodeReceipt(receipt.StageReference, OutcomeCommitted)
	if err != nil || wire != fences[3].value {
		return nil, "", ErrInvalidRecord
	}
	return keys, receipt.Digest, nil
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
