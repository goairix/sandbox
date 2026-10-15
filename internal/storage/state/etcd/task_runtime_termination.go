package etcd

import (
	"bytes"
	"context"
	"errors"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
)

func (a *taskLocalRuntimeAuthority) newTermination() p.TaskRuntimeTerminationIntent {
	c := a.command.Claims.Context.Current
	c.CommandID, c.ClaimID, c.WorkerID = uuid.NewString(), a.claim.reference.ClaimID, a.claim.reference.WorkerID
	c.ClaimCreateRevision, c.LeaseID = a.claim.reference.CreateRevision, a.claim.reference.LeaseID
	r := a.publication.Record
	return p.TaskRuntimeTerminationIntent{Version: 1, Phase: "reserved", Current: c, Partition: a.task.Reference.Partition, TaskBirth: a.claim.birth, CreationIntentID: a.task.CreationIntentID, EnrollmentDigest: r.EnrollmentDigest, ObserverCertificateDigest: r.ObserverCertificateDigest, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID, Action: "kill_pid1_and_observe_cgroup_empty", Authority: a.stamps}
}

func (a *taskLocalRuntimeAuthority) matchesTermination(r p.TaskRuntimeTerminationIntent) bool {
	if r.Authority != a.stamps || r.TaskBirth != a.claim.birth || r.CreationIntentID != a.task.CreationIntentID || r.MountAttempt != 0 || r.EnrollmentDigest != a.publication.Record.EnrollmentDigest || r.ObserverCertificateDigest != a.publication.Record.ObserverCertificateDigest || r.NodeUID != a.publication.Record.NodeUID || r.NodeBootID != a.publication.Record.NodeBootID || r.Partition != a.task.Reference.Partition || r.Current.ClaimCreateRevision > a.claim.reference.CreateRevision {
		return false
	}
	// Identity must still equal the original archive, while the termination
	// sender was chosen once by its reservation and is never rewritten later.
	original, current := a.command.Claims.Context.Current, r.Current
	current.CommandID, current.ClaimID, current.WorkerID = original.CommandID, original.ClaimID, original.WorkerID
	current.ClaimCreateRevision, current.LeaseID = original.ClaimCreateRevision, original.LeaseID
	return current == original
}

func (b *Backend) TerminateTaskRuntimeLocal(ctx context.Context, c *TaskClaim, destination *controltransport.TerminationDestination) (result TaskRuntimeTerminationResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	if !b.validTaskClaim(c) || destination == nil {
		return result, ErrInvalidRecord
	}
	c.mu.Lock()
	repeated := c.runtimeReleaseAttempt != nil
	c.mu.Unlock()
	if repeated {
		entry, err := b.LoadTaskRuntimeTermination(ctx, c.reference.Task)
		result.Entry = entry
		if entry != nil && entry.Phase == "cleanup-complete" && err == nil {
			result.Outcome = OutcomeCommitted
		}
		return result, err
	}
	o, err := b.beginLocalRuntimeRelease(ctx, c)
	if err != nil {
		return result, err
	}
	defer o.finish()
	a, err := b.loadLocalRuntimeAuthority(o.ctx, c)
	if err != nil {
		return result, err
	}
	x := &taskLocalRuntimeExecution{a: a, intent: a.newTermination()}
	points, err := b.readEnrolledPoints(o.ctx, []string{a.terminationKey, a.providerKey, a.releaseKey}, []int{16384, 16384, 8192})
	if err != nil || points[2] != nil {
		return result, errors.Join(ErrConflict, err)
	}
	x.intentKV, x.providerKV = points[0], points[1]
	if x.intentKV != nil {
		x.intent, err = decodeLocalRuntimeTermination(x.intentKV)
		if err != nil || !a.matchesTermination(x.intent) {
			return result, errors.Join(ErrCorruptRecord, err)
		}
	}
	if x.providerKV != nil {
		x.cleanup, err = decodeTaskRuntimeProviderCleanup(x.providerKV)
		if err != nil || !localRuntimeProviderMatches(x.cleanup, x.intent, b.enrolledPublication.ContractDigest) {
			return result, errors.Join(ErrCorruptRecord, err)
		}
	} else if x.intentKV != nil {
		return result, ErrCorruptRecord
	}
	if x.intentKV != nil && x.intent.Phase != "reserved" {
		if err := b.verifyLocalRuntimeProof(x.intent, x.intentKV, a.publication.Enrollment.ObserverCertificate); err != nil {
			return result, err
		}
		if x.intent.Phase == "cleanup-complete" {
			if x.intent.CleanupDigest != workspaceSyncIntentDigest(string(x.providerKV.Value)) || !localRuntimeProviderComplete(x.cleanup) {
				return result, ErrCorruptRecord
			}
			result.Outcome, result.Entry = OutcomeCommitted, localRuntimeTerminationEntry(x.intent, x.intentKV)
			return result, nil
		}
	}
	if err := x.prepareProvider(o.ctx); err != nil {
		return result, err
	}
	if x.intentKV == nil {
		if err := x.applyProvider(o.ctx, 0); err != nil {
			return result, err
		}
		if err := x.storeTermination(o.ctx); err != nil {
			return result, err
		}
		// Only this foreground owner, after a definite reserve ACK, can obtain
		// the original ticket and send once. A recovered reservation only queries.
		ticket, err := x.sign(o.ctx)
		if err != nil {
			return result, err
		}
		if err := x.applyProvider(o.ctx, 1); err != nil {
			return result, err
		}
		if err := a.guard(o.ctx, a.terminationComparisons(x.intentKV, x.providerKV)); err != nil {
			return result, err
		}
		physical, err := destination.Terminate(o.ctx, ticket, a.command.Certificate)
		if err != nil {
			return result, errors.Join(ErrOutcomeUnknown, err)
		}
		if err := x.acceptProof(o.ctx, physical); err != nil {
			return result, err
		}
	} else if x.intent.Phase == "reserved" {
		if x.cleanup.Slots[0].State != "complete" || x.cleanup.Slots[1].State != "complete" {
			return result, ErrOutcomeUnknown
		}
		query, err := localRuntimeTerminationQuery(x.intent, x.intentKV)
		if err != nil {
			return result, err
		}
		physical, err := destination.Query(o.ctx, query)
		if err != nil {
			return result, errors.Join(ErrOutcomeUnknown, err)
		}
		if err := x.acceptProof(o.ctx, physical); err != nil {
			return result, err
		}
	}
	if err := x.finishProvider(o.ctx); err != nil {
		return result, err
	}
	result.Outcome, result.Entry = OutcomeCommitted, localRuntimeTerminationEntry(x.intent, x.intentKV)
	return result, nil
}

func (x *taskLocalRuntimeExecution) sign(ctx context.Context) ([]byte, error) {
	a, b := x.a, x.a.backend
	certificate, err := b.taskRuntimeTerminationIssuer.Certificate(ctx)
	if err != nil || len(certificate) == 0 || len(certificate) > 4096 || !bytes.Equal(certificate, a.command.Certificate) {
		return nil, errors.Join(ErrIdentityMismatch, err)
	}
	certificate = bytes.Clone(certificate)
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return nil, err
	}
	issuer, err := b.enrolledVerifier.VerifyCommandIssuerCertificate(certificate, now)
	if err != nil || issuer.Digest() != x.intent.Current.IssuerCertificateDigest || issuer.CertificateID() != x.intent.Current.IssuerCertificateID {
		return nil, errors.Join(ErrIdentityMismatch, err)
	}
	a.claim.mu.Lock()
	deadline := a.claim.deadline
	a.claim.mu.Unlock()
	remaining := time.Until(deadline)
	if end, ok := ctx.Deadline(); ok && time.Until(end) < remaining {
		remaining = time.Until(end)
	}
	if remaining > 20*time.Second {
		remaining = 20 * time.Second
	}
	before, after := now.Add(-time.Second), now.Add(remaining-time.Second)
	if before.Before(issuer.NotBefore()) {
		before = issuer.NotBefore()
	}
	if !after.Before(issuer.NotAfter()) {
		after = issuer.NotAfter().Add(-time.Second)
	}
	if !now.Add(2 * time.Second).Before(after) {
		return nil, ErrGuardExpired
	}
	claims := p.TaskRuntimeTerminationTicketClaims{Version: 1, Purpose: "task_runtime_terminate", Current: x.intent.Current, Partition: x.intent.Partition, IntentCreateRevision: x.intentKV.CreateRevision, IntentModRevision: x.intentKV.ModRevision, IntentDigest: workspaceSyncIntentDigest(string(x.intentKV.Value)), EnrollmentDigest: x.intent.EnrollmentDigest, ObserverCertificateDigest: x.intent.ObserverCertificateDigest, NodeUID: x.intent.NodeUID, NodeBootID: x.intent.NodeBootID, NotBefore: before, NotAfter: after}
	wire, err := b.taskRuntimeTerminationIssuer.SignRuntimeTermination(ctx, issuer.Digest(), claims)
	if err != nil || len(wire) == 0 || len(wire) > 8192 {
		return nil, errors.Join(ErrInvalidRecord, err)
	}
	now, err = b.observePublicationClock(ctx)
	if err != nil {
		return nil, err
	}
	evidence, err := b.enrolledVerifier.VerifyTaskRuntimeTerminationTicket(wire, certificate, claims, now)
	if err != nil {
		return nil, err
	}
	return evidence.Wire(), a.local(ctx)
}

func (x *taskLocalRuntimeExecution) acceptProof(ctx context.Context, result p.NodeTerminationResult) error {
	if result.State != "terminal" || len(result.Proof) == 0 || len(result.Proof) > 4096 || !bytes.Equal(result.ObserverCertificate, x.a.publication.Enrollment.ObserverCertificate) {
		return ErrOutcomeUnknown
	}
	query, err := localRuntimeTerminationQuery(x.intent, x.intentKV)
	if err != nil {
		return err
	}
	proof, err := x.a.backend.enrolledVerifier.VerifyNodeTerminationProof(result.Proof, result.ObserverCertificate, query)
	if err != nil || proof.Current != x.intent.Current || proof.NodeUID != x.intent.NodeUID || proof.NodeBootID != x.intent.NodeBootID || proof.ObserverCertificateDigest != x.intent.ObserverCertificateDigest || proof.PodUID != x.intent.Current.Runtime.UID {
		return errors.Join(ErrCorruptRecord, err)
	}
	x.intent.Phase, x.intent.TerminalProof = "terminated", bytes.Clone(result.Proof)
	return x.storeTermination(ctx)
}
