package etcd

import (
	"context"
	"errors"
	"time"
)

// One goroutine owns the actual physical chain. Metadata continuations always
// return to the original actor; none recreates a partition/session or bypasses
// the original Claim and effect gates. Every failure retains its occupation.
func (b *Backend) runCleanupLocal(j *cleanupLocalJob) {
	var err error
	if j.boundRecovery {
		err = b.cleanupResumeBoundArchive(j)
	} else {
		err = b.cleanupArchiveAndFinalize(j)
	}
	if err == nil {
		err = b.cleanupTerminateAndRelease(j)
	}
	// Even cancellation joins the exact Claim cleanup before declaring this
	// run joined. A late native continuation was already joined by its reply.
	if j.claim != nil {
		cleanup, stop := context.WithTimeout(context.Background(), b.requestTimeout)
		err = errors.Join(err, b.ReleaseTaskClaim(cleanup, j.claim))
		stop()
		j.claim = nil
	}
	if j.target.termination != nil {
		err = errors.Join(err, j.target.termination.Close())
	}
	j.effectsJoined = true
	// A failed recovery preflight has not created this process's Run. Never
	// join the persisted old Run merely because this goroutine has returned.
	if j.runID != "" {
		cleanup, stop := context.WithTimeout(context.Background(), b.requestTimeout)
		err = errors.Join(err, b.joinCleanupRun(cleanup, j))
		stop()
	}
	if err == nil {
		_, err = j.worker.cleanupContinuation(j, &cleanupLocalContinuation{kind: "settle"})
	}
	b.finishCleanupJob(j, err)
}

func (j *cleanupLocalJob) progress(confirmed string, pending *cleanupExecutionPending, archive *cleanupExecutionArchive) error {
	_, err := j.worker.cleanupContinuation(j, &cleanupLocalContinuation{kind: "progress", confirmed: confirmed, pending: pending, archive: archive})
	return err
}

func (j *cleanupLocalJob) pending(purpose string, stage StageReference) *cleanupExecutionPending {
	p := &cleanupExecutionPending{Purpose: purpose}
	if j.claim != nil {
		p.ClaimID, p.LeaseID = j.claim.reference.ClaimID, j.claim.reference.LeaseID
	}
	if stage.AttemptID != "" && stage.Digest != "" {
		p.StageID, p.RequestID, p.AttemptID, p.Digest = stage.StageID, stage.RequestID, stage.AttemptID, stage.Digest
	}
	return p
}

// Renewal has one original goroutine per active Claim. Its cancellation is
// joined before rollover/release. A failed or interrupted renew loses that
// Claim; it cannot silently extend work beyond the original job context.
func (b *Backend) renewCleanupClaim(j *cleanupLocalJob, claim *TaskClaim) func() error {
	ctx, cancel := context.WithCancel(j.ctx)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(b.localCleanup.ClaimTTL / 3)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
				if err := b.RenewTaskClaim(ctx, claim); err != nil {
					j.cancel()
					done <- err
					return
				}
			}
		}
	}()
	return func() error {
		cancel()
		return <-done
	}
}

func (b *Backend) cleanupArchiveAndFinalize(j *cleanupLocalJob) (resultErr error) {
	ctx, target := j.ctx, j.target
	stopRenew := b.renewCleanupClaim(j, j.claim)
	defer func() {
		if stopRenew != nil {
			resultErr = errors.Join(resultErr, stopRenew())
		}
	}()
	if err := j.progress("", j.pending("close", StageReference{}), nil); err != nil {
		return err
	}
	closeData, err := b.PrepareTaskCloseData(ctx, j.claim)
	if saveErr := j.progress("", j.pending("close", closeData.Reference.Stage), nil); err != nil || saveErr != nil {
		return errors.Join(err, saveErr)
	}
	if closeData.Outcome != OutcomeCommitted || closeData.Prepared == nil {
		return ErrOutcomeUnknown
	}
	closed, err := b.DeliverTaskCloseData(ctx, closeData.Prepared, target.destination)
	if err != nil || closed.Receipt == nil {
		return errors.Join(ErrDeliveryUnknown, err)
	}
	if err = j.progress("closed", j.pending("quiescence", StageReference{}), nil); err != nil {
		return err
	}
	quiescence, err := b.PrepareTaskQuiescence(ctx, j.claim, target.destination)
	if saveErr := j.progress("", j.pending("quiescence", quiescence.Reference.Stage), nil); err != nil || saveErr != nil {
		return errors.Join(err, saveErr)
	}
	if quiescence.Outcome != OutcomeCommitted || quiescence.Prepared == nil {
		return ErrOutcomeUnknown
	}
	delivered, err := b.DeliverTaskQuiescence(ctx, quiescence.Prepared, target.destination)
	if err != nil {
		return err
	}
	if err = b.waitCleanupQuiescence(j, delivered); err != nil {
		return err
	}
	if err = j.progress("quiescent", j.pending("final-sync", StageReference{}), nil); err != nil {
		return err
	}
	drained, err := b.PrepareAfterDualDrain(ctx, j.claim, target.destination)
	if err != nil {
		return err
	}
	if err = j.progress("drained", j.pending("final-sync", StageReference{}), nil); err != nil {
		return err
	}
	if _, err = b.ReserveTaskWorkspaceSync(ctx, j.claim, drained, target.destination, target.plan); err != nil {
		return err
	}
	if err = j.progress("", j.pending("archive", StageReference{}), nil); err != nil {
		return err
	}
	archive, err := b.PrepareTaskWorkspaceArchive(ctx, j.claim, drained, target.destination, b.localCleanup.ArchiveTimeout)
	if err != nil || archive.Outcome != OutcomeCommitted || archive.Prepared == nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	received, err := b.ReceiveTaskWorkspaceArchive(ctx, archive.Prepared)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, received.Close()) }()
	slot, err := b.BindTaskWorkspaceArchiveLocal(ctx, received, target.plan, b.localCleanup.Worker, target.root)
	if err != nil {
		return err
	}
	// Bind joins the source and target and seals this original manifest. Its
	// exact store/slot/target digest are copied diagnostics, not new authority.
	manifest := received.owner.spool.recovery.manifest
	if manifest.State != "bound" || manifest.Binding == nil || manifest.Slot != slot {
		return ErrCorruptRecord
	}
	bound := &cleanupExecutionArchive{StoreID: manifest.StoreID, Slot: slot, BindingDigest: manifest.Binding.TargetDigest}
	if err = received.Close(); err != nil {
		return err
	}
	if err = j.progress("archive-bound", nil, bound); err != nil {
		return err
	}
	renewErr := stopRenew()
	stopRenew = nil
	if renewErr != nil {
		return renewErr
	}
	cleanup, stop := context.WithTimeout(context.Background(), b.requestTimeout)
	err = b.ReleaseTaskClaim(cleanup, j.claim)
	stop()
	j.claim = nil
	if err != nil {
		return err
	}
	// The original archive claim is physically closed before a new charged
	// admission. The same permanent Run/S survives this mandatory rollover.
	j.claim, err = j.worker.requestCleanupClaim(j)
	if err != nil {
		return err
	}
	return b.cleanupFinalizeBoundArchive(j, slot)
}

// Both normal rollover and cold bound recovery arrive with the original
// managed Claim. Finalize repeats its real historical/dual-drain/source/target
// checks; a manifest preflight is never substituted for that authority.
func (b *Backend) cleanupFinalizeBoundArchive(j *cleanupLocalJob, slot uint32) (resultErr error) {
	stopRenew := b.renewCleanupClaim(j, j.claim)
	defer func() { resultErr = errors.Join(resultErr, stopRenew()) }()
	if err := j.progress("", j.pending("finalize", StageReference{}), nil); err != nil {
		return err
	}
	finalized, err := b.FinalizeTaskWorkspaceSyncLocal(j.ctx, j.claim, slot, j.target.destination, b.localCleanup.Worker)
	if err != nil || !finalized.LocalComplete {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	return j.progress("local-complete", nil, nil)
}

func (b *Backend) cleanupTerminateAndRelease(j *cleanupLocalJob) (resultErr error) {
	stopRenew := b.renewCleanupClaim(j, j.claim)
	defer func() {
		if stopRenew != nil {
			resultErr = errors.Join(resultErr, stopRenew())
		}
	}()
	if err := j.progress("", j.pending("termination", StageReference{}), nil); err != nil {
		return err
	}
	terminated, err := b.TerminateTaskRuntimeLocal(j.ctx, j.claim, j.target.termination)
	if err != nil || terminated.Outcome != OutcomeCommitted {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if err = j.progress("terminated", j.pending("owner-release", StageReference{}), nil); err != nil {
		return err
	}
	// Release deletes original claim fences. Join renewal first so it cannot
	// race that intentional deletion and misclassify the completed release.
	renewErr := stopRenew()
	stopRenew = nil
	if renewErr != nil {
		return renewErr
	}
	released, err := b.ReleaseTaskWorkspaceOwnerLocal(j.ctx, j.claim)
	if err != nil || released.Outcome != OutcomeCommitted {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	return j.progress("owner-released", nil, nil)
}
