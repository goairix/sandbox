package etcd

import (
	"bytes"
	"context"
	"errors"
	"math"
	"time"

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
	// Full wire dimensions do not yet admit physical effects or a new Run.
	// Keeping this gate before E avoids an early Joined Run with no Q/none
	// history that the existing detached cold recovery cannot yet resume.
	if err == nil {
		j.result.Reason = "detached-wire-verified"
		err = ErrInvalidConfiguration
	}
	b.archiveLife.mu.Lock()
	s := j.worker.managedSource
	if s != nil && s.job == j && s.preparing {
		j.worker.managedSource = nil
		j.worker.cache.managedSource = false
		j.worker.cache.managedSourceKeys = 0
	}
	b.archiveLife.mu.Unlock()
	b.finishCleanupJob(j, err)
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
