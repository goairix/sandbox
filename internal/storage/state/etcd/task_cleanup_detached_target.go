package etcd

import (
	"bytes"
	"context"
	"errors"
	"math"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/launcher"
)

// Only the serial workspace retains decoded history. The registry's target
// contains compact dimensions and a diagnostic report, never a pb plan/source
// clone, transport, local filesystem root or archive authority.
type cleanupDetachedSource struct {
	snapshot    SandboxSnapshotRecord
	intent      CreationIntentRecord
	workspace   WorkspaceIdentity
	publication *RuntimeEnrolledPublicationEntry
}

type cleanupDetachedTarget struct {
	worker launcher.WorkspaceSyncWorkerReference
	report cleanupDetachedWireReport
}

func (b *Backend) cleanupDetachedPreparationProfile() error {
	// Startup still requires the original full-local deployment configuration.
	// Its archive dependencies are not invoked or borrowed by this profile.
	if b.cleanupPhysicalProfile() != nil || b.taskIssuer == nil || b.taskQuiescenceIssuer == nil || b.taskRuntimeTerminationIssuer == nil || b.taskVerifier == nil || b.detachedCleanup == nil {
		return ErrInvalidConfiguration
	}
	return nil
}

// Only the actor can transfer source ownership, after owner.finish/stop. A
// pending discovery CAS must finish before invalidating its cursor/coverage.
func (w *DueWorker) beginCleanupDetachedPreparation(j *cleanupLocalJob) error {
	b := w.backend
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	s := w.managedSource
	if s == nil || s.job != j || s.preparing || s.source == nil || j.started || j.owner != nil || j.step != "detached-target" || j.ctx.Err() != nil || w.session.operation != nil {
		return ErrInvalidRecord
	}
	registered := false
	for _, original := range w.cleanupJobs {
		registered = registered || original == j
	}
	if !registered {
		return ErrInvalidRecord
	}
	for _, scan := range w.scans {
		if scan.checkpoint != nil && scan.checkpoint.record.Pending != nil {
			return ErrConflict
		}
	}
	// Source19 + actual bundle/common/current-projection union is checked by
	// every emission against84. Reserve that finite key allowance before the
	// preparation goroutine can materialize any future request.
	w.cache.managedSourceKeys = 84
	keys, limit := w.cache.limitsLocked(w.cache.automatic, w.cache.coldQuota)
	if w.cache.count > keys || w.cache.bytes > limit {
		w.cache.invalidateLocked("detached preflight workspace")
		for _, scan := range w.scans {
			scan.resetView()
		}
		w.windowAt = time.Time{}
	}
	w.pauseDiscoveryLocked("replaying")
	s.preparing, j.started = true, true
	return nil
}

func (b *Backend) runCleanupDetachedPreparation(j *cleanupLocalJob) {
	err := b.prepareCleanupDetachedTarget(j)
	if err == nil {
		j.result.Reason = "detached-wire-verified"
		// Source classification and pure preflight remain usable without a
		// refund consumer. Physical admission requires that consumer before
		// E/Run/Grant: killing and releasing first would otherwise strand the
		// occupation with no verified quota return available for settlement.
		if !b.cleanupColdQuotaEnabled() {
			err = ErrInvalidConfiguration
		} else {
			var reply cleanupLocalReply
			reply, err = j.worker.cleanupContinuation(j, &cleanupLocalContinuation{kind: "admit"})
			j.claim = reply.claim
		}
	}
	if err == nil && j.earlyRecovery {
		err = b.resumeCleanupDetached(j)
	}
	if err == nil {
		err = b.cleanupDetachedExecute(j)
	}
	// The same original registry owner keeps the source reservation through
	// transport, renewal, Claim and native Run joins. Cancellation cannot
	// refund a blocked callback or an accepted actor continuation early.
	b.finishCleanupPhysical(j, err)
}

// Callbacks and Kernel checks belong to this same registered preparation
// goroutine, outside the native actor. Cancellation remains joined even if a
// configured callback returns late; neither callback nor a report authorizes E.
func (b *Backend) prepareCleanupDetachedTarget(j *cleanupLocalJob) error {
	if j == nil || j.worker == nil || j.worker.backend != b || j.worker.self != j.worker || j.detachedTarget == nil || j.bundle == nil || j.bundle.task.Reference != j.task {
		return ErrInvalidRecord
	}
	b.archiveLife.mu.Lock()
	s := j.worker.managedSource
	registered := false
	for _, original := range j.worker.cleanupJobs {
		registered = registered || original == j
	}
	valid := registered && s != nil && s.job == j && s.preparing && s.source != nil && j.started && j.owner == nil && j.request == nil && j.step == "detached-target"
	b.archiveLife.mu.Unlock()
	if !valid {
		return ErrInvalidRecord
	}
	if err := errors.Join(j.ctx.Err(), b.cleanupDetachedPreparationProfile()); err != nil {
		return err
	}
	if err := b.localCleanup.Worker.ValidateCurrent(); err != nil {
		return err
	}
	worker, err := b.localCleanup.Worker.WorkspaceSyncWorkerReference()
	if err != nil {
		return err
	}
	issuer, err := b.cleanupDetachedDimensionIssuer(j.ctx)
	if err != nil {
		return err
	}
	source := s.source
	report, err := b.visitCleanupDetachedWire(cleanupDetachedWireInput{bundle: j.bundle, snapshot: source.snapshot, intent: source.intent, workspace: source.workspace, publication: source.publication, issuer: issuer, workerID: j.worker.options.WorkerID, claimTTL: b.localCleanup.ClaimTTL}, cleanupLocalWireDimensions{runID: "11111111-1111-4111-8111-111111111111", claimID: "22222222-2222-4222-8222-222222222222", worker: worker}, func(cleanupDetachedWireEmission) error { return nil })
	if err != nil {
		return err
	}
	if err = j.ctx.Err(); err != nil {
		return err
	}
	j.detachedTarget.worker, j.detachedTarget.report = worker, report
	j.target = &cleanupLocalTarget{snapshot: source.snapshot, publication: source.publication, detachedWorkspace: source.workspace, worker: worker}
	return nil
}

// A compact inventory is rebuilt from the original serial source and current
// E/S on the charged actor. No complete protobuf tree survives the visitor.
// The independent private inventory binds the actual pending Claim and Kernel;
// a diagnostic report or a public ClaimTask cannot enter this pipeline.
func (b *Backend) prepareCleanupDetachedInventory(j *cleanupLocalJob) (*cleanupLocalWireInventory, error) {
	s := j.worker.managedSource
	if s == nil || s.job != j || !s.preparing || s.source == nil || j.owner == nil || j.state == nil || j.state.slot == nil || j.state.record.Run == nil || j.state.record.Run.Joined || j.state.record.Pending == nil || j.state.record.Pending.Purpose != "claim" || j.target == nil || j.detachedTarget == nil {
		return nil, ErrInvalidRecord
	}
	worker, err := b.localCleanup.Worker.WorkspaceSyncWorkerReference()
	if err != nil || worker != j.state.record.Run.Worker {
		return nil, errors.Join(ErrInvalidRecord, err)
	}
	report, err := b.visitCleanupDetachedWire(b.cleanupDetachedInput(j), cleanupLocalWireDimensions{runID: j.state.record.Run.ID, claimID: j.state.record.Pending.ClaimID, worker: worker}, func(cleanupDetachedWireEmission) error { return nil })
	if err != nil {
		return nil, err
	}
	i := &cleanupLocalWireInventory{origin: b, task: j.task, incarnation: j.state.record.Incarnation, runID: j.state.record.Run.ID, claimID: j.state.record.Pending.ClaimID, executionMod: j.state.execution.mod, slotMod: j.state.slot.mod, kernel: b.localCleanup.Worker, detachedJob: j, detachedReport: report}
	i.self = i
	return i, nil
}

func (b *Backend) cleanupDetachedInput(j *cleanupLocalJob) cleanupDetachedWireInput {
	source := j.worker.managedSource.source
	return cleanupDetachedWireInput{bundle: j.bundle, snapshot: source.snapshot, intent: source.intent, workspace: source.workspace, publication: source.publication, issuer: j.target.issuer, workerID: j.worker.options.WorkerID, claimTTL: b.localCleanup.ClaimTTL}
}

// The original managed Claim owns every effect. Detached cleanup follows the
// existing close/Q/drain protocol but never creates an archive or local plan.
// Renewal is joined before owner release intentionally deletes its fences.
func (b *Backend) cleanupDetachedExecute(j *cleanupLocalJob) (resultErr error) {
	stopRenew := b.renewCleanupClaim(j, j.claim)
	defer func() {
		if stopRenew != nil {
			resultErr = errors.Join(resultErr, stopRenew())
		}
	}()
	factKey, err := b.namespace.taskDetachedNoEffectKey(j.task)
	if err != nil {
		return err
	}
	// A fresh Task has no Q/none history yet. Probe only the fixed fact point;
	// present bytes must pass the full authenticating reader before skipping
	// runtime drain. Absence itself grants no effect authority.
	points, _, err := b.readCleanupDetachedNoEffectPoints(j.ctx, "arbitration", []string{b.identityKey, b.restoreKey, factKey}, []int{maxRecordBytes, maxRecordBytes, p.MaxDetachedNoEffectSettlementBytes})
	if err != nil {
		return err
	}
	if points[2] == nil {
		if err = b.cleanupDetachedSettleEffects(j); err != nil {
			return err
		}
	} else {
		fact, err := b.loadTaskDetachedNoEffect(j.ctx, j.task)
		if err != nil || fact == nil {
			return errors.Join(ErrConflict, err)
		}
	}
	if err = j.progress("drained", j.pending("termination", StageReference{}), nil); err != nil {
		return err
	}
	terminated, err := b.terminateTaskRuntimeDetached(j.ctx, j, j.claim, j.target.termination, nil)
	if err != nil || terminated.Outcome != OutcomeCommitted {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if err = j.progress("terminated", j.pending("owner-release", StageReference{}), nil); err != nil {
		return err
	}
	renewErr := stopRenew()
	stopRenew = nil
	if renewErr != nil {
		return renewErr
	}
	released, err := b.releaseTaskWorkspaceOwnerDetached(j.ctx, j, j.claim)
	if err != nil || released.Outcome != OutcomeCommitted {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	return j.progress("owner-released", nil, nil)
}

func (b *Backend) cleanupDetachedSettleEffects(j *cleanupLocalJob) error {
	if j.earlyRecovery && j.early != nil {
		if err := b.recheckCleanupEarlyHistory(j); err != nil {
			return err
		}
	}
	if j.early == nil || j.early.close == nil {
		if err := b.cleanupCloseData(j); err != nil {
			return err
		}
	}
	if j.early == nil || j.early.quiescence == nil {
		if err := b.cleanupQuiesceUsers(j); err != nil {
			return err
		}
	}
	drained, err := b.PrepareAfterDualDrain(j.ctx, j.claim, j.target.destination)
	if err != nil {
		return err
	}
	if err = j.progress("drained", j.pending("final-sync", StageReference{}), nil); err != nil {
		return err
	}
	draft, err := b.prepareCleanupDetachedNoEffect(j.ctx, j, drained, j.target.destination)
	if err != nil {
		return err
	}
	settled, err := b.commitCleanupDetachedNoEffect(j.ctx, j, draft)
	if err != nil || settled.Outcome != OutcomeCommitted {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	return nil
}

// Validate exactly the three detached signing roles, with no registration Put.
// Revision is explicitly a future-width dimension; later native registration
// must supply its original actual KV before an inventory can authorize Grant.
func (b *Backend) cleanupDetachedDimensionIssuer(ctx context.Context) (CommandIssuerEntry, error) {
	bounded, cancel := b.requestContext(ctx)
	defer cancel()
	var certificate []byte
	for _, issuer := range []interface {
		Certificate(context.Context) ([]byte, error)
	}{b.taskIssuer, b.taskQuiescenceIssuer, b.taskRuntimeTerminationIssuer} {
		wire, err := issuer.Certificate(bounded)
		if err != nil || len(wire) == 0 || len(wire) > 4096 || !b.localCleanup.Credential.MatchesCertificate(wire) || certificate != nil && !bytes.Equal(certificate, wire) {
			return CommandIssuerEntry{}, errors.Join(ErrInvalidConfiguration, err)
		}
		certificate = bytes.Clone(wire)
	}
	now, err := b.observePublicationClock(bounded)
	if err != nil {
		return CommandIssuerEntry{}, err
	}
	identity, err := b.taskVerifier.VerifyCommandIssuerCertificate(certificate, now)
	if err != nil {
		return CommandIssuerEntry{}, err
	}
	if err = bounded.Err(); err != nil {
		return CommandIssuerEntry{}, err
	}
	return CommandIssuerEntry{Revision: math.MaxInt64 - 30, Record: CommandIssuerRecord{Version: 1, Namespace: b.namespace.Root(), RestoreEpoch: b.restoreEpoch, CertificateID: identity.CertificateID(), CertificateDigest: identity.Digest(), Certificate: identity.Wire()}}, nil
}
