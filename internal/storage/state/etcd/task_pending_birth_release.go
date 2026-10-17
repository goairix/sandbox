package etcd

import (
	"bytes"
	"context"
	"errors"
)

// This private result retains the original signed loader's proof and the exact
// birth observation it authenticates. It is neither a claim nor permission to
// settle execution or delete discovery metadata; later CAS must fence both.
type cleanupBirthReleaseEvidence struct {
	origin    *Backend
	self      *cleanupBirthReleaseEvidence
	partition *WorkerPartitionClaim
	birth     *cleanupBirthEvidence
	proof     *localOwnerReleaseProof
}

// A validation refusal may only attempt the final worker audit. In particular,
// a joined corruption sentinel cannot conceal transport failure, and true is
// never proof success or permission to preserve a worker without postchecks.
func cleanupBirthReleaseValidationError(err error) bool {
	var transport *enrolledPointTransportError
	if errors.As(err, &transport) || errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, ErrIdentityMismatch) || errors.Is(err, ErrGuardExpired) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return err == nil || errors.Is(err, ErrCorruptRecord) || errors.Is(err, ErrCorruptReceipt)
}

// Five RPCs at most share one original Range charge and finite phase: W guard,
// the unchanged three-read signed loader, then W guard. Neither cancellation nor
// an invalid candidate frees the owner before its actual client calls return.
func (b *Backend) readCleanupBirthRelease(ctx context.Context, p *WorkerPartitionClaim, ranges *WorkerBudgetShare, birth *cleanupBirthEvidence) (_ *cleanupBirthReleaseEvidence, resultErr error) {
	if ctx == nil || !b.validWorkerBackend() {
		return nil, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.validateCleanupBirthRelease(p, birth); err != nil {
		return nil, err
	}
	o, stop, err := b.beginCleanupBirthOwner(ctx, p, ranges, true)
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

	if err = b.preflightCleanupBirthRelease(o, birth); err != nil {
		return nil, err
	}
	_, margin, err := cleanupBirthPhaseTiming(b.requestTimeout, true)
	if err != nil {
		return nil, err
	}
	if err = b.validateCleanupBirthOwner(o, margin); err != nil {
		return nil, err
	}
	if err = b.workerGuardCheck(o.call, p, ranges); err != nil {
		return nil, err
	}
	proof, validationErr := b.readLocalOwnerReleaseProof(o.call.ctx, birth.record.Reference)
	if !cleanupBirthReleaseValidationError(validationErr) {
		return nil, validationErr
	}
	if validationErr == nil {
		validationErr = matchCleanupBirthReleaseProof(birth, proof)
	}

	// Typed invalid history remains a refused candidate, including a malformed
	// native envelope. Only this actual successful audit preserves its worker.
	if err = b.workerGuardCheck(o.call, p, ranges); err != nil {
		return nil, errors.Join(err, validationErr)
	}
	if err = o.postcheck(); err != nil {
		return nil, errors.Join(err, validationErr)
	}
	if validationErr != nil {
		guardedRefusal = true
		return nil, validationErr
	}

	// The loader's decoded record/history are privately owned. Copy its native
	// release bytes as well so no retained evidence aliases a transport buffer.
	owned := *proof
	kv := *proof.kv
	kv.Key, kv.Value = bytes.Clone(proof.kv.Key), bytes.Clone(proof.kv.Value)
	owned.kv = &kv
	result := &cleanupBirthReleaseEvidence{origin: b, partition: p, birth: birth, proof: &owned}
	result.self = result
	return result, nil
}

// Recheck the already authenticated observation without inventing a native
// read revision or an MVCC version. Canonical encodings must still match its
// owned fences; only the original birth reader can supply its origin seal.
func (b *Backend) validateCleanupBirthRelease(p *WorkerPartitionClaim, birth *cleanupBirthEvidence) error {
	if err := b.validateCleanupBirthEvidence(p, birth); err != nil {
		return err
	}
	if b.enrolledPublication == nil || !validHexDigest(b.enrolledPublication.ContractDigest) || b.enrolledVerifier == nil || b.localRuntimeRelease.validate() != nil {
		return ErrInvalidConfiguration
	}

	if birth.release == nil || birth.releaseRecord == nil {
		return ErrInvalidRecord
	}
	if birth.execution != nil && birth.execution.Release != nil {
		pin := birth.execution.Release
		if pin.CreateRevision != birth.release.create || pin.Digest != workspaceSyncIntentDigest(birth.release.value) {
			return ErrInvalidRecord
		}
	}
	return nil
}

// This join uses only the original private signed loader result. A missing or
// changed release, or a terminal proof for different Task bytes, stays refused.
func matchCleanupBirthReleaseProof(birth *cleanupBirthEvidence, proof *localOwnerReleaseProof) error {
	if proof == nil || !birth.release.matches(proof.kv) || proof.record != *birth.releaseRecord || localRuntimeTerminationTask(proof.termination) != birth.record.Reference || proof.termination.TaskBirth != birth.birth || proof.termination.Authority.Task != localRuntimeFenceStamp(birth.task) {
		return ErrCorruptRecord
	}
	if birth.execution != nil && birth.execution.Release != nil {
		pin := birth.execution.Release
		if pin.CreateRevision != proof.kv.CreateRevision || pin.Digest != workspaceSyncIntentDigest(string(proof.kv.Value)) {
			return ErrCorruptRecord
		}
	}
	return nil
}

// Before dispatch, account for all original signed-loader branches using the
// existing maximum profile. Unknown historical keys are size placeholders only;
// the unchanged loader derives and verifies actual routing from signed data.
func (b *Backend) preflightCleanupBirthRelease(owner *workerDueOwner, birth *cleanupBirthEvidence) error {
	if err := b.validateCleanupBirthOwner(owner, 0); err != nil {
		return err
	}
	if err := b.validateCleanupBirthRelease(owner.partition, birth); err != nil {
		return err
	}
	b.archiveLife.mu.Lock()
	cmps, err := b.workerComparisons(owner.call.session, owner.partition, owner.share)
	b.archiveLife.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = b.cleanupBirthWireProjections(cmps, birth.record.Reference, birth.initial, owner.call.session, true)
	return err
}
