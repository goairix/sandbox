package etcd

// Index evidence authenticates only an immutable birth and raw P observation.
// It deliberately has no E/release fields and cannot stand in for normal birth
// or physical cleanup authority. Only readCleanupBirth's actual postguard seals
// an optional-refusal draft; pure decoding never publishes a usable seal.
type cleanupBirthIndexEvidence struct {
	origin    *Backend
	self      *cleanupBirthIndexEvidence
	partition *WorkerPartitionClaim
	record    TaskRecord
	initial   StageAttemptLocator

	birth, revision int64
	task            taskFence
	intent          taskFence
	link            taskFence
	receipt         taskFence
	pending         *taskFence
}

// The strict read remains an error. This separate payload can only authorize
// conservative discovery indexing, never omission, deletion or execution.
type cleanupBirthOptionalRefusal struct {
	cause error
	index *cleanupBirthIndexEvidence
}

func (e *cleanupBirthOptionalRefusal) Error() string {
	return e.cause.Error()
}

func (e *cleanupBirthOptionalRefusal) Unwrap() error {
	return e.cause
}

// The seal proves guarded observation, not P validity. Recheck raw P here so
// an early E refusal cannot conceal a malformed, leased or rewritten P.
func (b *Backend) validateCleanupBirthIndexEvidence(p *WorkerPartitionClaim, index *cleanupBirthIndexEvidence) error {
	if !b.validWorkerBackend() || index == nil || index.self != index || index.origin != b || index.partition != p {
		return ErrInvalidRecord
	}
	keys, _, err := b.validateCleanupBirthMandatory(p, index.record, index.initial, index.birth, index.revision, [4]taskFence{index.task, index.intent, index.link, index.receipt})
	if err != nil {
		return err
	}
	if f := index.pending; f != nil {
		wire, err := encodeCleanupPending(cleanupPendingRecord{Version: 1, Reference: index.record.Reference, TaskDigest: workerIdentityDigest(index.task.value)})
		if err != nil || f.key != keys[5] || f.lease != 0 || f.create < index.birth || f.mod != f.create || f.mod > index.revision || wire != f.value {
			return ErrInvalidRecord
		}
	}
	return nil
}
