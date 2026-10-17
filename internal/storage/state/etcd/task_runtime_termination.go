package etcd

import (
	"bytes"
	"context"
	"errors"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"go.etcd.io/etcd/api/v3/mvccpb"
)

func (a *taskLocalRuntimeAuthority) newTermination() p.TaskRuntimeTerminationIntent {
	c := a.command.Claims.Context.Current
	c.CommandID, c.ClaimID, c.WorkerID = uuid.NewString(), a.claim.reference.ClaimID, a.claim.reference.WorkerID
	c.ClaimCreateRevision, c.LeaseID = a.claim.reference.CreateRevision, a.claim.reference.LeaseID
	r := a.publication.Record
	return p.TaskRuntimeTerminationIntent{Version: 1, Phase: "reserved", Current: c, Partition: a.task.Reference.Partition, TaskBirth: a.claim.birth, CreationIntentID: a.task.CreationIntentID, EnrollmentDigest: r.EnrollmentDigest, ObserverCertificateDigest: r.ObserverCertificateDigest, NodeUID: r.NodeUID, NodeBootID: r.NodeBootID, Action: "kill_pid1_and_observe_cgroup_empty", Authority: a.stamps}
}

func (a *taskLocalRuntimeAuthority) matchesTermination(r p.TaskRuntimeTerminationIntent) bool {
	// Common stamps do not identify the effect profile. This local origin may
	// never accept a detached tuple, including one with otherwise equal pins.
	if r.Version != 1 || r.DetachedEffects != nil {
		return false
	}
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

// TerminateTaskRuntimeLocal uses a current genuine Claim to terminate the
// enrolled runtime through its observer and finish exact provider cleanup.
// Only a newly confirmed reservation may send Terminate; retained reservations
// query their original outcome. Unknown replies do not authorize another send
// or owner release, and all original history/proof/effect guards still apply.
func (b *Backend) TerminateTaskRuntimeLocal(ctx context.Context, c *TaskClaim, destination *controltransport.TerminationDestination) (result TaskRuntimeTerminationResult, resultErr error) {
	return b.terminateTaskRuntimeLocal(ctx, c, destination, nil)
}

// A non-nil reservation narrows recovery to querying the original observer
// intent. It is checked against the actual read before any provider mutation
// or signature; an absent/recreated intent must never become a new send.
// Public callers retain the original nil-reservation behavior.
func (b *Backend) terminateTaskRuntimeLocal(ctx context.Context, c *TaskClaim, destination *controltransport.TerminationDestination, reservation *taskFence) (result TaskRuntimeTerminationResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	if !b.validTaskClaim(c) || destination == nil {
		return result, ErrInvalidRecord
	}
	c.mu.Lock()
	repeated := c.runtimeReleaseAttempt != nil
	c.mu.Unlock()
	if repeated {
		if reservation != nil {
			return result, ErrInvalidRecord
		}
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
	if reservation != nil {
		if err := matchCleanupTerminationReservation(c.reference.Task, c.birth, reservation, x.intentKV); err != nil {
			return result, err
		}
	}
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

// This pure check compares a previously authenticated reservation with one
// actual native point. It creates no proof/Claim authority. The original
// matchesTermination/authority/provider guards still run after it succeeds.
func matchCleanupTerminationReservation(task TaskReference, birth int64, expected *taskFence, actual *mvccpb.KeyValue) error {
	if task.Validate() != nil || birth <= 0 || expected == nil || actual == nil || expected.lease != 0 || expected.create <= 0 || expected.mod != expected.create || actual.Lease != 0 || actual.Version != 1 || actual.CreateRevision != actual.ModRevision || *expected != ownTaskFence(actual) {
		return ErrConflict
	}

	intent, err := decodeLocalRuntimeTermination(actual)
	if err != nil || intent.Phase != "reserved" || localRuntimeTerminationTask(intent) != task || intent.TaskBirth != birth {
		return errors.Join(ErrCorruptRecord, err)
	}
	keys, err := p.TaskRuntimeTerminationKeysFor(intent)
	if err != nil || keys.Intent != expected.key {
		return errors.Join(ErrCorruptRecord, err)
	}
	return nil
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
	before, after, err := localRuntimeTerminationWindow(now, remaining, issuer)
	if err != nil {
		return nil, err
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

// The verifier reserves one second at each authentication edge. Independently
// elapsed-accounted trusted samples can move backward within their bounded
// one-second sampling budget, so the start also leaves that second of margin.
// This does not extend the future authority: remaining is already the minimum
// of the original claim/context lifetimes, and the issuer bounds still apply.
func localRuntimeTerminationWindow(now time.Time, remaining time.Duration, issuer p.CommandIssuerIdentity) (time.Time, time.Time, error) {
	if remaining > 20*time.Second {
		remaining = 20 * time.Second
	}
	before, after := now.Add(-2*time.Second), now.Add(remaining-time.Second)
	if before.Before(issuer.NotBefore()) {
		before = issuer.NotBefore()
	}
	if !after.Before(issuer.NotAfter()) {
		after = issuer.NotAfter().Add(-time.Second)
	}
	if now.Add(-time.Second).Before(before) || !now.Add(2*time.Second).Before(after) {
		return time.Time{}, time.Time{}, ErrGuardExpired
	}
	return before, after, nil
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
