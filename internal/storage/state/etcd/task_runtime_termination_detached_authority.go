package etcd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// This private live capability belongs to one original registered physical
// continuation. The authenticated fact may belong to an earlier legal writer
// Run/Claim; it never supplies today's job, Kernel or Claim ownership. No full
// E/S/source graph is retained after construction and no archive DTO is created.
type taskDetachedRuntimeAuthority struct {
	job                *cleanupLocalJob
	runID, incarnation string
	cutoff             time.Time
	current            p.TaskCloseDataContext
	certificate        []byte
	effects            p.DetachedRuntimeEffectBindings
	settlement         taskFence
}

// Run changes are owned by the original metadata actor and require the old
// physical continuation's actual join. Checking the registry/job/Kernel plus
// the captured Run on every local gate preserves that ownership; a persisted
// Run DTO or copied Claim holder cannot reconstruct the original continuation.
func (b *Backend) validateDetachedRuntimeOwner(j *cleanupLocalJob, c *TaskClaim) error {
	if err := b.validateCleanupDetachedNoEffectJob(j); err != nil {
		return err
	}
	if !b.validTaskClaim(c) || j.claim != c || c.parentCtx != j.ctx || j.effectsJoined || j.pendingProgress != nil || j.state.slot == nil || j.state.record.Phase != "occupied" || j.state.record.Run.Joined || j.state.record.Archive != nil || j.state.record.Release != nil || b.validateCleanupState(j.state) != nil {
		return ErrInvalidRecord
	}
	return nil
}

func (b *Backend) loadDetachedRuntimeAuthority(ctx context.Context, j *cleanupLocalJob, c *TaskClaim) (*taskLocalRuntimeAuthority, error) {
	if ctx == nil || b == nil || b.detachedCleanup == nil || b.enrolledPublication == nil || b.enrolledVerifier == nil {
		return nil, ErrInvalidRecord
	}
	if err := b.validateDetachedRuntimeOwner(j, c); err != nil {
		return nil, err
	}
	// Capture the original Claim cutoff without holding its mutex through the
	// registry lock or any native call. A later renewal cannot lengthen this
	// particular construction/operation's captured authority window.
	c.mu.Lock()
	err := c.live(ctx)
	deadline := c.deadline
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(30 * time.Second) // Original runtime-release maximum.
	if deadline.Before(cutoff) {
		cutoff = deadline
	}
	if caller, ok := ctx.Deadline(); ok && caller.Before(cutoff) {
		cutoff = caller
	}
	owned, cancel := context.WithDeadline(ctx, cutoff)
	defer cancel()

	task, fences, cmps, err := b.loadRuntimeAuthorityDomain(owned, c)
	if err != nil {
		return nil, err
	}
	history, err := b.readCleanupDetachedNoEffectHistory(owned, task.Reference)
	if err != nil {
		return nil, err
	}
	if history == nil || history.self != history || history.origin != b || history.task != task || history.taskFence != fences[0] || history.state == nil || history.state.slot == nil || history.state.execution != j.state.execution || *history.state.slot != *j.state.slot || history.settlement == nil || history.settlement.self != history.settlement || history.settlement.origin != b {
		return nil, ErrConflict
	}
	writer := history.settlement.record.WriterClaim
	if writer.CreateRevision > c.reference.CreateRevision || writer.CreateRevision == c.reference.CreateRevision && writer != detachedNoEffectClaim(c.reference) {
		return nil, ErrIdentityMismatch
	}
	supplement, err := b.loadDetachedRuntimeSupplement(owned, history)
	if err != nil {
		return nil, err
	}
	if err := b.verifyDetachedRuntimeHistory(history, supplement.certificate); err != nil {
		return nil, err
	}

	fact := history.settlement
	detached := &taskDetachedRuntimeAuthority{job: j, runID: j.runID, incarnation: j.state.record.Incarnation, cutoff: cutoff,
		current: history.quiescence.Record.Context.Current, certificate: supplement.certificate,
		effects:    p.DetachedRuntimeEffectBindings{Version: 1, Profile: "detached_enrolled_plain", ContractDigest: fact.record.ContractDigest, Settlement: localRuntimeStamp(fact.kv)},
		settlement: ownTaskFence(fact.kv),
	}
	a := &taskLocalRuntimeAuthority{backend: b, claim: c, task: task, publication: history.publication, enrollment: supplement.enrollment, detached: detached}
	a.stamps = p.TerminationAuthorityBindings{Task: localRuntimeFenceStamp(fences[0]), CleanupIntent: localRuntimeFenceStamp(fences[1]), TaskLink: localRuntimeFenceStamp(fences[2]), Placement: localRuntimeFenceStamp(fences[3]), Control: localRuntimeFenceStamp(fences[4]), Owner: localRuntimeFenceStamp(fences[5]), Fence: localRuntimeFenceStamp(fences[6]), RuntimeIndex: localRuntimeFenceStamp(fences[7]),
		Quiescence: localRuntimeFenceStamp(history.quiescencePin.intent), QuiescenceReceipt: localRuntimeFenceStamp(history.quiescencePin.receipt), QuiescenceReservation: localRuntimeFenceStamp(supplement.reservation), FinalSync: localRuntimeFenceStamp(history.finalSyncFence), Issuer: localRuntimeFenceStamp(supplement.issuer), QuiescenceStage: fact.record.QuiescenceStage,
	}
	// These are actual accepted native observations, not caller-selected pins.
	// Prefix and permanent fact Mod are appended only by the profile builder.
	a.comparisons = append(b.baseComparisons(), cmps...)
	for _, fence := range []taskFence{history.quiescencePin.intent, history.quiescencePin.receipt, supplement.reservation, history.finalSyncFence, supplement.issuer, history.snapshotFence, history.intentFence, history.publicationFence} {
		a.comparisons = append(a.comparisons, clientv3.Compare(clientv3.ModRevision(fence.key), "=", fence.mod))
	}
	if len(a.comparisons) != 52 {
		return nil, ErrInvalidRecord
	}
	a.prefix, err = b.taskOperationsPrefix(task.Reference)
	if err != nil {
		return nil, err
	}
	a.terminationKey, a.providerKey, a.releaseKey, err = b.namespace.localRuntimeReleaseKeys(task.Reference)
	if err != nil {
		return nil, err
	}
	if err := a.guard(owned, a.base()); err != nil {
		return nil, err
	}
	return a, nil
}

type detachedRuntimeSupplement struct {
	reservation, issuer taskFence
	certificate         []byte
	enrollment          [2]taskFence
}

// The four-point supplement is one fixed identity-fenced native transaction:
// base4 + Then6 + Else6 =16 nodes. Reservation must be the actual original KV;
// hashing a re-encoded diagnostic reservation would lose its native envelope.
func (b *Backend) loadDetachedRuntimeSupplement(ctx context.Context, h *cleanupDetachedNoEffectHistory) (detachedRuntimeSupplement, error) {
	fail := func(err error) (detachedRuntimeSupplement, error) { return detachedRuntimeSupplement{}, err }
	q := h.quiescence
	issuerKey, err := b.namespace.commandIssuerKey(q.Record.Context.Current.IssuerCertificateID)
	if err != nil {
		return fail(err)
	}
	ek, ak, err := b.namespace.runtimeEnrollmentKeys(h.task.Reference.Partition, h.task.CreationIntentID)
	if err != nil {
		return fail(err)
	}
	points, err := b.readEnrolledPoints(ctx, []string{h.quiescencePin.reservation.key, issuerKey, ek, ak}, []int{8192, 8192, 8192, 8192})
	if err != nil {
		return fail(err)
	}
	reservation, err := taskQuiescenceAttemptEntry(points[0], h.task.Reference)
	if err != nil || reservation == nil || reservation.Record == nil || !taskQuiescenceHistoryReservation(*q.Record, q.Revision, reservation) || reservation.Revision != h.quiescencePin.reservation.revision || localRuntimeStamp(points[0]) != h.settlement.record.QuiescenceReservation {
		return fail(errors.Join(ErrCorruptRecord, err))
	}
	canonical, err := encodeTaskQuiescenceAttempt(*reservation.Record)
	if err != nil || canonical != string(points[0].Value) {
		return fail(errors.Join(ErrCorruptRecord, err))
	}
	issuer, err := b.decodeCommandIssuer(points[1])
	if err != nil || issuer == nil || issuer.Revision != q.Record.IssuerRevision || issuer.Record.CertificateID != q.Record.Context.Current.IssuerCertificateID || issuer.Record.CertificateDigest != q.Record.Context.Current.IssuerCertificateDigest {
		return fail(errors.Join(ErrCorruptRecord, err))
	}
	var enrollment RuntimeEnrollmentRecord
	var activation RuntimeEnrollmentActivationRecord
	publication := h.publication.Record
	if !enrolledImmutableKV(points[2]) || !enrolledImmutableKV(points[3]) || points[2].CreateRevision != h.publicationFence.create || points[3].CreateRevision != h.publicationFence.create || decodeEnrolledRecord(points[2].Value, &enrollment) != nil || decodeEnrolledRecord(points[3].Value, &activation) != nil || workspaceSyncIntentDigest(string(points[2].Value)) != publication.EnrollmentRecordDigest || workspaceSyncIntentDigest(string(points[3].Value)) != publication.ActivationRecordDigest {
		return fail(ErrCorruptRecord)
	}
	return detachedRuntimeSupplement{reservation: ownTaskFence(points[0]), issuer: ownTaskFence(points[1]), certificate: bytes.Clone(issuer.Record.Certificate), enrollment: [2]taskFence{ownTaskFence(points[2]), ownTaskFence(points[3])}}, nil
}

// Authenticate the stored q/close signatures at their own signed historical
// intervals. Task3 already authenticated the actual users-quiesced receipt by
// fresh Query before committing this fact; its digest remains bound to the
// original immutable none reservation. None alone never grants effect absence.
func (b *Backend) verifyDetachedRuntimeHistory(h *cleanupDetachedNoEffectHistory, issuer []byte) error {
	q := h.quiescence.Record
	var ticket taskQuiescenceTicketWire
	if json.Unmarshal(q.Ticket, &ticket) != nil {
		return ErrCorruptRecord
	}
	at := ticket.Claims.NotBefore.Add(ticket.Claims.NotAfter.Sub(ticket.Claims.NotBefore) / 2)
	verified, err := b.enrolledVerifier.VerifyTaskUserQuiescenceTicket(q.Ticket, issuer, q.Context, at)
	if err != nil || verified.Digest() != q.TicketDigest {
		return fmt.Errorf("detached termination historical quiescence: %w", errors.Join(ErrCorruptRecord, err))
	}
	var receipt taskQuiescenceCloseReceiptWire
	if json.Unmarshal(q.CloseDataReceipt, &receipt) != nil {
		return ErrCorruptRecord
	}
	c := receipt.Claims
	at = c.NotBefore.Add(c.NotAfter.Sub(c.NotBefore) / 2)
	closed, err := b.enrolledVerifier.VerifyTaskDataClosedReceipt(q.CloseDataReceipt, h.publication.Enrollment.Request.RuntimeCertificate, q.Context.CloseDataContext, q.Context.CloseDataTicketDigest, c.NotBefore, c.NotAfter, at)
	if err != nil || !bytes.Equal(closed.Wire(), q.CloseDataReceipt) {
		return fmt.Errorf("detached termination historical close: %w", errors.Join(ErrCorruptRecord, err))
	}
	return nil
}
