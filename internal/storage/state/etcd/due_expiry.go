package etcd

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// ExpiredTaskResult describes only the original metadata attempt. Neither
// reference authorizes runtime cleanup, task execution or owner/quota release.
type ExpiredTaskResult struct {
	Outcome Outcome
	// Reference and Task retain the exact attempted identities on an unknown
	// result; they do not authorize a replacement attempt or task execution.
	Reference         StageReference
	Task              TaskReference
	GuardCleanupError error
}

// observeExpiryLowerBound deliberately does not use publication's UTC+elapsed
// value. Uncertainty is subtracted; elapsed only rejects stale observations.
func (b *Backend) observeExpiryLowerBound(ctx context.Context) (time.Time, error) {
	if b.publicationVerifier == nil || b.authorityClock == nil {
		return time.Time{}, ErrInvalidConfiguration
	}
	rpc, cancel := b.requestContext(ctx)
	defer cancel()
	started := time.Now()
	observation, err := b.authorityClock.Observe(rpc)
	elapsed := time.Since(started)
	if err != nil || rpc.Err() != nil {
		return time.Time{}, errors.Join(err, rpc.Err())
	}
	if observation.Uncertainty < 0 || observation.Uncertainty > time.Second || elapsed < 0 || elapsed > time.Second-observation.Uncertainty || !validDomainExpiry(observation.UTC) || observation.UTC.Location() != time.UTC {
		return time.Time{}, ErrInvalidRecord
	}
	lower := observation.UTC.Add(-observation.Uncertainty)
	if !validDomainExpiry(lower) || lower.Location() != time.UTC {
		return time.Time{}, ErrInvalidRecord
	}
	return lower, nil
}

// AdmitExpiredTask creates the existing metadata-only cleanup task and
// consumes its exact due candidate in the same transaction. It never returns
// a generic Stage that could bypass the original clock/worker rechecks.
func (b *Backend) AdmitExpiredTask(ctx context.Context, p *WorkerPartitionClaim, share *WorkerBudgetShare, candidate DueCandidateRecord, requestID string, ttl time.Duration) (result ExpiredTaskResult, resultErr error) {
	return b.admitExpiredTask(ctx, p, share, candidate, requestID, ttl, nil)
}

// The resident consumer persists its exact pending reference using this
// admission's original charged call before any Stage Grant. Public admission
// retains its existing signature and has no caller-supplied persistence hook.
func (b *Backend) admitExpiredTask(ctx context.Context, p *WorkerPartitionClaim, share *WorkerBudgetShare, candidate DueCandidateRecord, requestID string, ttl time.Duration, pending *dueWorkerAdmission) (result ExpiredTaskResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	if ctx == nil || !b.validWorkerBackend() || candidate.Validate() != nil || !validDomainSegment(requestID) || candidate.RestoreEpoch != b.restoreEpoch {
		return result, ErrInvalidRecord
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		return result, ErrInvalidMutation
	}
	if b.publicationVerifier == nil || b.authorityClock == nil {
		return result, ErrInvalidConfiguration
	}
	if pending != nil && (pending.origin != b || pending.partition != p || pending.claims != share || pending.checkpoint == nil || pending.checkpoint.record.Pending != nil) {
		return result, ErrInvalidRecord
	}
	o, stopDeadline, err := b.beginExpiredTaskOwner(ctx, p, share)
	if err != nil {
		return result, err
	}
	defer stopDeadline()
	defer o.finish()
	call := o.call
	terminalNativeFailure := false
	defer func() {
		if resultErr != nil && (terminalNativeFailure || call.ctx.Err() != nil) {
			call.lose()
		}
	}()
	if candidate.Partition != p.partition {
		return result, ErrInvalidRecord
	}
	firstBound, err := b.observeExpiryLowerBound(call.ctx)
	if err != nil {
		return result, err
	}
	if firstBound.Before(candidate.ExpiresAt) {
		if pending != nil {
			pending.early = true
		}
		return result, ErrOperationAdmissionClosed
	}
	// These generic read helpers use this owner's fixed original lease
	// deadline, not only the per-request timeout. Reads create no authority.
	call.started = true
	bundle, err := b.loadOperationControl(call.ctx, candidate.SandboxID)
	if err != nil {
		terminalNativeFailure = !errors.Is(err, ErrConflict) && !errors.Is(err, ErrOperationAdmissionClosed)
		if pending != nil && errors.Is(err, ErrOperationAdmissionClosed) {
			pending.rejected = true
		}
		return result, err
	}
	if bundle.Partition != p.partition || dueCandidateForControl(bundle.Partition, bundle.Control) != candidate {
		if pending != nil {
			pending.rejected = true
		}
		return result, ErrConflict
	}
	dueKey, err := b.namespace.dueCandidateKey(candidate)
	if err != nil {
		return result, err
	}
	points, err := b.readEnrolledPoints(call.ctx, []string{dueKey}, []int{dueRecordBytes})
	if err != nil {
		terminalNativeFailure = true
		return result, err
	}
	if points[0] == nil {
		if pending != nil {
			pending.rejected = true
		}
		return result, ErrConflict
	}
	actual, err := decodeDueCandidate(b.namespace, points[0], points[0].ModRevision)
	if err != nil {
		terminalNativeFailure = true
		return result, err
	}
	if actual != candidate {
		if pending != nil {
			pending.rejected = true
		}
		return result, ErrConflict
	}
	locator := StageAttemptLocator{Namespace: b.namespace.Root(), Partition: p.partition, RequestID: requestID, StageID: "destroy", AttemptID: uuid.NewString(), RestoreEpoch: b.restoreEpoch}
	result.Task = TaskReference{Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, TaskID: uuid.NewString(), SandboxID: candidate.SandboxID, Partition: p.partition}
	mutation, err := buildTaskDestroyMutation(b.namespace, bundle, result.Task, locator)
	if err != nil {
		return result, err
	}
	b.archiveLife.mu.Lock()
	worker, err := b.workerComparisons(p.session, p, share)
	b.archiveLife.mu.Unlock()
	if err != nil {
		terminalNativeFailure = true
		return result, err
	}
	// Generic Stage already supplies the original four base comparisons.
	mutation.Comparisons = append(mutation.Comparisons, worker[4:]...)
	// The canonical permanent due row was decoded above with a positive Mod.
	// Exact Mod pins its complete observed MVCC incarnation: deletion/recreate,
	// same-value replacement and lease attachment all change it. Retain exact
	// Value as well, while leaving every worker and five-authority fence intact.
	// The two saved nodes fund atomic pending absence+Put within the original64.
	mutation.Comparisons = append(mutation.Comparisons,
		clientv3.Compare(clientv3.Value(dueKey), "=", string(points[0].Value)),
		clientv3.Compare(clientv3.ModRevision(dueKey), "=", points[0].ModRevision),
	)
	mutation.Writes = append(mutation.Writes, Write{Key: dueKey, Delete: true})
	owned, digest, err := prepareMutation(b.namespace, mutation)
	if err != nil {
		return result, err
	}
	result.Reference = locator.reference(digest) // retained even on an unknown Grant.
	if err = b.preflightExpiredTask(result.Reference, owned); err != nil {
		return result, err
	}
	if pending != nil {
		if err = pending.persist(call, candidate, result); err != nil {
			terminalNativeFailure = true
			return result, err
		}
	}
	stage, err := b.beginStageWithLocator(call.ctx, locator, ttl, func(StageAttemptLocator) (Mutation, error) { return owned, nil }, nil)
	if err != nil {
		terminalNativeFailure = !errors.Is(err, ErrConflict) || errors.Is(err, ErrOutcomeUnknown)
		var failure *stageBeginFailure
		if errors.As(err, &failure) {
			result.GuardCleanupError = failure.cleanup
		}
		return result, err
	}
	defer func() {
		result.GuardCleanupError = errors.Join(result.GuardCleanupError, b.revokeStageLease(stage.leaseID))
	}()
	if stage.reference != result.Reference {
		return result, ErrInvalidMutation
	}
	secondBound, err := b.observeExpiryLowerBound(call.ctx)
	if err != nil {
		return result, err
	}
	if secondBound.Before(firstBound) || secondBound.Before(candidate.ExpiresAt) {
		if pending != nil {
			pending.early = true
		}
		return result, ErrOperationAdmissionClosed
	}
	if err = o.postcheck(); err != nil {
		terminalNativeFailure = true
		return result, err
	}
	// The original profile/session/partition/share and all five authoritative
	// records plus exact due KV are still fenced inside this actual commit.
	result.Outcome, err = b.CommitStage(call.ctx, stage)
	if err != nil {
		if errors.Is(err, ErrConflict) && !errors.Is(err, ErrOutcomeUnknown) {
			// A metadata CAS can lose normally. Before preserving this local
			// worker, prove its original twenty fences in a read-bearing RPC
			// under the same charged owner and fixed original deadline.
			if guardErr := b.workerGuardCheck(call, p, share); guardErr != nil {
				terminalNativeFailure = true
				return result, errors.Join(err, guardErr)
			}
		} else {
			terminalNativeFailure = true
		}
		return result, err
	}
	if err = o.postcheck(); err != nil {
		terminalNativeFailure = true
		result.Outcome = OutcomeUnknown
		return result, err
	}
	return result, nil
}

// The generic reservation remains fourteen:43 input comparisons+7 business
// operations+14=64. Actual commit is51 comparisons+8 success+4 failure=63.
func (b *Backend) preflightExpiredTask(ref StageReference, mutation Mutation) error {
	if ref.StageID != "destroy" || len(mutation.Comparisons) != 43 || len(mutation.Writes) != 7 || !mutation.Writes[6].Delete {
		return ErrInvalidMutation
	}
	return b.preflightTaskDestroy(ref, mutation)
}
