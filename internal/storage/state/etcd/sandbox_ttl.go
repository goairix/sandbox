package etcd

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The original OperationCapability.mu owns the draft across actual synchronous
// transport return. A failed send permanently selects query-only arbitration;
// cancellation cannot erase the fixed native candidate or allocate another one.
type sandboxTTLDraft struct {
	record     sandboxTTLRecord
	activation p.TargetActivationEvidence
	previous   p.TargetActivationEvidence
	// effectDeadline is captured once from the original admission and trusted
	// business intervals. Later clock observations or lease renewal cannot
	// extend it. Receipt arbitration remains possible after this deadline.
	effectDeadline    time.Time
	publish           *Stage
	publishRef        StageReference
	publishTried      bool
	publishSubmitted  bool
	pendingRevision   int64
	final             *Stage
	finalRef          StageReference
	finalTried        bool
	finalSubmitted    bool
	sent              bool
	rejected          bool
	outcome           Outcome
	guardCleanupError error
}

// RenewSandboxTTL fixes one native candidate before contacting the exact runtime
// through a concrete authenticated Destination. It never accepts an ACK DTO as
// commit permission. A retry must provide that candidate's pinned Destination;
// it queries instead of resending installation. A nil operation is allowed only
// to diagnose an existing native pending candidate, using Query/Resolve; a dead
// Stage is never re-granted and this cold path cannot complete a new mutation.
// Baseline is the caller's fixed controlled UTC observation, not a retry's now.
func (b *Backend) RenewSandboxTTL(ctx context.Context, c *OperationCapability, view *SandboxReadView, baseline time.Time, seconds int, next p.TargetActivationEvidence, destination *ct.Destination) (result SandboxTTLResult, resultErr error) {
	result.Outcome = OutcomeUnknown
	if b == nil || ctx == nil || destination == nil || view == nil || view.self != view || view.origin != b || view.records.Control == nil {
		return result, ErrInvalidRecord
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if c == nil {
		return b.querySandboxTTLPending(ctx, view, destination)
	}
	if c.self != c || c.origin != b || c.parentCtx == nil || c.record.Reference.Kind != OperationMutation {
		return result, ErrInvalidRecord
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	bounded, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.parentCtx, cancel)
	defer func() {
		stop()
		cancel()
	}()
	if c.ttlDraft == nil {
		if err := c.admissionLive(); err != nil {
			return result, err
		}
		prepareCtx, finish := context.WithDeadline(bounded, c.deadline)
		d, err := b.prepareSandboxTTL(prepareCtx, c, view, baseline, seconds, next)
		finish()
		if err != nil {
			return result, err
		}
		c.ttlDraft = d
	}
	d := c.ttlDraft
	if d.record.Timeout != seconds || !d.record.Baseline.Equal(baseline) || d.activation.Digest() != next.Digest() || view.records.Control.SandboxID != d.record.Control.SandboxID {
		return result, ErrConflict
	}
	defer func() {
		result.Stage, result.Outcome = d.finalRef, d.outcome
		result.Baseline, result.ExpiresAt, result.Timeout = d.record.Baseline, d.record.ExpiresAt, d.record.Timeout
		result.GuardCleanupError = errors.Join(result.GuardCleanupError, d.guardCleanupError)
		if resultErr != nil && d.sent && !d.rejected {
			resultErr = errors.Join(ErrOutcomeUnknown, resultErr)
		}
	}()
	if d.finalSubmitted {
		// Resolve the original final receipt; do not rerun transport or compare
		// stale pre-update control fences after a possibly committed result.
		outcome, err := b.ResolveStage(ctx, d.finalRef)
		d.outcome = outcome
		if err != nil || outcome != OutcomeCommitted {
			return result, errors.Join(ErrOutcomeUnknown, err)
		}
		if d.rejected {
			d.outcome = OutcomeAborted
			return result, ErrConflict
		}
		return result, nil
	}
	// The effect context cannot outlive either the original parent admission
	// or the captured monotonic deadline. A sent candidate can still be queried
	// using the caller's context, but that diagnostic never renews write power.
	effectCtx, finishEffect := context.WithDeadline(bounded, d.effectDeadline)
	defer finishEffect()
	if d.rejected {
		return result, b.rejectSandboxTTL(effectCtx, c, d)
	}
	if !d.sent {
		if err := effectCtx.Err(); err != nil {
			return result, err
		}
		if err := c.admissionLive(); err != nil {
			return result, err
		}
		if err := b.publishSandboxTTL(effectCtx, c, d); err != nil {
			return result, err
		}
		if err := b.sandboxTTLLive(effectCtx, c, d); err != nil {
			return result, err
		}
	}
	if err := b.sandboxTTLDestination(d.record, destination); err != nil {
		return result, err
	}
	expectedCertificate := d.previous.RuntimeCertificate()
	if d.sent {
		expectedCertificate = d.activation.RuntimeCertificate()
	}
	if !bytes.Equal(destination.RuntimeCertificate(), expectedCertificate) {
		return result, ErrInvalidRecord
	}
	var response ct.RuntimeRenewalResponse
	var err error
	installing := !d.sent
	if d.sent {
		response, err = destination.QueryRuntimeActivationRenewal(ctx, d.activation.DataGateEpoch(), d.record.PreviousDigest, d.activation.Digest())
	} else {
		if err := b.fenceSandboxTTL(effectCtx, d); err != nil {
			return result, err
		}
		if err := effectCtx.Err(); err != nil {
			return result, err
		}
		// This latch precedes the actual native send and is never reset. A
		// missing ACK cannot authorize another installation, signing or Grant.
		d.sent = true
		response, err = destination.RenewRuntimeActivation(effectCtx, d.record.PreviousDigest, d.activation)
	}
	if err != nil {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	if response.Installed == response.Rejected || response.Identity != d.activation.Identity() || response.DataGateEpoch != d.activation.DataGateEpoch() || response.PreviousDigest != d.record.PreviousDigest || response.CandidateDigest != d.activation.Digest() {
		return result, ErrOutcomeUnknown
	}
	if response.Rejected {
		// Only the Backend's first actual Install can establish this no-effect
		// fact. A query response or arbitrary caller DTO cannot retire pending.
		if !installing {
			return result, ErrOutcomeUnknown
		}
		d.rejected = true
		return result, b.rejectSandboxTTL(effectCtx, c, d)
	}
	if response.GateState != "open" {
		return result, ErrOutcomeUnknown
	}
	if err := b.sandboxTTLLive(effectCtx, c, d); err != nil {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	if d.final == nil {
		if d.finalTried {
			outcome, err := b.ResolveStage(ctx, d.finalRef)
			d.outcome = outcome
			if outcome != OutcomeCommitted || err != nil {
				return result, errors.Join(ErrOutcomeUnknown, err)
			}
			return result, nil
		}
		mutation, ref, err := b.sandboxTTLStage(d.record, d.pendingRevision)
		if err != nil {
			return result, err
		}
		d.finalRef, d.finalTried = ref, true
		d.final, err = b.beginStageWithLocator(effectCtx, d.record.Final, time.Until(d.effectDeadline), func(StageAttemptLocator) (Mutation, error) { return mutation, nil }, nil)
		if err != nil {
			return result, errors.Join(ErrOutcomeUnknown, err)
		}
		if d.final.reference != ref {
			return result, ErrInvalidMutation
		}
	}
	if err := b.sandboxTTLLive(effectCtx, c, d); err != nil {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	d.finalSubmitted = true
	d.outcome, err = b.CommitStage(effectCtx, d.final)
	if err != nil || d.outcome != OutcomeCommitted {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	// Committed metadata wins, but a lost original lifetime is still diagnosed
	// as uncertain to this caller. It never restores that operation's authority.
	if err := b.sandboxTTLLive(effectCtx, c, d); err != nil {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	cleanup, finish := b.requestContext(context.Background())
	result.GuardCleanupError = b.ReleaseStage(cleanup, d.final)
	finish()
	return result, nil
}

func (b *Backend) prepareSandboxTTL(ctx context.Context, c *OperationCapability, view *SandboxReadView, baseline time.Time, seconds int, next p.TargetActivationEvidence) (*sandboxTTLDraft, error) {
	if seconds < 1 || seconds > 365*24*60*60 || !validDomainExpiry(baseline) || baseline.Location() != time.UTC || c.record.Reference.SandboxID != view.records.Control.SandboxID || c.record.ControlRevision != view.ControlRevision() || !reflect.DeepEqual(c.control, *view.records.Control) || c.control.Phase != PhaseActive {
		return nil, ErrInvalidRecord
	}
	if err := b.ConfirmSandboxView(ctx, view); err != nil {
		return nil, err
	}
	pending, err := b.ReadSandboxTTLPending(ctx, view)
	if err != nil {
		return nil, err
	}
	if pending != nil {
		return nil, ErrOutcomeUnknown
	}
	initial, err := b.sandboxTTLInitialActivation(ctx, view)
	if err != nil {
		return nil, err
	}
	prior := initial
	current, currentKV, err := b.readSandboxTTLRecord(ctx, view, false)
	if err != nil {
		return nil, err
	}
	if currentKV != nil {
		prior, err = b.authenticateSandboxTTL(ctx, view, current)
		if err != nil {
			return nil, err
		}
	}
	// Capture first: any delay in the trusted observation may shorten this
	// local interval, but cannot move a business cutoff into the future.
	anchor := time.Now()
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return nil, err
	}
	expires := baseline.Add(time.Duration(seconds) * time.Second)
	deadline := sandboxTTLEffectDeadline(anchor, now, c.deadline, c.control.ExpiresAt, prior.NotAfter(), next.NotAfter(), expires)
	ctx, finish := context.WithDeadline(ctx, deadline)
	defer finish()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if baseline.After(now) || !now.Add(time.Second).Before(c.control.ExpiresAt) {
		return nil, ErrOperationAdmissionClosed
	}
	verified, err := b.enrolledVerifier.VerifyTargetActivation(next.Wire(), next.RuntimeCertificate(), next.IssuerCertificate(), initial.Birth(), now)
	if err != nil {
		return nil, err
	}
	if verified.Identity() != initial.Identity() || verified.DataGateEpoch() != initial.DataGateEpoch() || verified.ActivationID() == prior.ActivationID() || !verified.NotBefore().After(prior.NotBefore()) {
		return nil, ErrInvalidRecord
	}
	// The exact old transport interval must be live before any installation.
	if _, err := b.enrolledVerifier.VerifyTargetActivation(prior.Wire(), prior.RuntimeCertificate(), prior.IssuerCertificate(), initial.Birth(), now); err != nil {
		return nil, err
	}
	if !validDomainExpiry(expires) || !now.Add(time.Second).Before(expires) {
		return nil, ErrOperationAdmissionClosed
	}
	r := sandboxTTLRecord{Version: 1, Operation: c.record, Control: c.control, GuardRevision: c.guardRevision, AdmitRevision: c.admitRevision, Baseline: baseline, Timeout: seconds, ExpiresAt: expires, PreviousDigest: prior.Digest(), Activation: verified.Wire(), RuntimeCert: verified.RuntimeCertificate(), IssuerCert: verified.IssuerCertificate()}
	r.Publish = StageAttemptLocator{Namespace: b.namespace.Root(), Partition: c.record.Reference.Partition, RestoreEpoch: b.restoreEpoch, RequestID: c.record.Reference.RequestID, StageID: "ttl_prepare", AttemptID: uuid.NewString()}
	r.Final = r.Publish
	r.Final.StageID, r.Final.AttemptID = "ttl_commit", verified.ActivationID()
	if currentKV != nil {
		r.CurrentRevision = currentKV.ModRevision
	}
	keys, err := b.sandboxTTLAuthorityKeys(r)
	if err != nil {
		return nil, err
	}
	for i, fence := range c.fences {
		if i >= len(r.SourceMods) {
			return nil, ErrInvalidRecord
		}
		if fence.Key != keys[i] {
			return nil, ErrInvalidRecord
		}
		r.SourceMods[i] = fence.ModRevision
	}
	if len(c.fences) != len(r.SourceMods) {
		return nil, ErrInvalidRecord
	}
	due := dueCandidateForControl(c.record.Reference.Partition, c.control)
	dueKey, err := b.namespace.dueCandidateKey(due)
	if err != nil {
		return nil, err
	}
	points, err := b.readEnrolledPoints(ctx, []string{dueKey}, []int{dueRecordBytes})
	if err != nil || len(points) != 1 || points[0] == nil {
		return nil, errors.Join(ErrConflict, err)
	}
	actual, err := decodeDueCandidate(b.namespace, points[0], points[0].ModRevision)
	if err != nil || actual != due {
		return nil, errors.Join(ErrCorruptRecord, err)
	}
	r.DueRevision = points[0].ModRevision
	_, ref, err := b.sandboxTTLStage(r, 0)
	if err != nil {
		return nil, err
	}
	// Reserve the worst native revision width for the final record before the
	// first candidate Stage Grant. The real final ref is derived after birth.
	if _, _, err := b.sandboxTTLStage(r, math.MaxInt64); err != nil {
		return nil, err
	}
	if _, _, err := b.sandboxTTLRejectStage(r, math.MaxInt64); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &sandboxTTLDraft{record: r, activation: verified, previous: prior, effectDeadline: deadline, publishRef: ref, outcome: OutcomeUnknown}, nil
}

// The trusted observation is translated immediately onto one local monotonic
// anchor. Subtracting one second from every bound leaves dispatch/join margin;
// taking the minimum once prevents later observations from restoring authority.
func sandboxTTLEffectDeadline(anchor, trusted, admission time.Time, limits ...time.Time) time.Time {
	deadline := admission.Add(-time.Second)
	for _, limit := range limits {
		// Sub saturates for very distant dates. Handle already-expired limits
		// before subtracting the margin, so a negative duration cannot wrap.
		remaining := limit.Sub(trusted)
		candidate := anchor
		if remaining > time.Second {
			candidate = anchor.Add(remaining - time.Second)
		}
		if candidate.Before(deadline) {
			deadline = candidate
		}
	}
	return deadline
}

// This branch is reachable only after the original Install returned a complete
// authenticated Rejected response before touching the target journal. It does
// not interpret query absence or an RPC error as permission to erase pending.
func (b *Backend) rejectSandboxTTL(ctx context.Context, c *OperationCapability, d *sandboxTTLDraft) error {
	if !d.rejected || !d.sent || d.pendingRevision <= 0 {
		return ErrOutcomeUnknown
	}
	if err := b.sandboxTTLLive(ctx, c, d); err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if err := b.fenceSandboxTTL(ctx, d); err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if d.finalTried {
		outcome, err := b.ResolveStage(ctx, d.finalRef)
		if err != nil || outcome != OutcomeCommitted {
			return errors.Join(ErrOutcomeUnknown, err)
		}
		d.outcome = OutcomeAborted
		return ErrConflict
	}
	mutation, ref, err := b.sandboxTTLRejectStage(d.record, d.pendingRevision)
	if err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	d.finalRef, d.finalTried = ref, true
	locator := d.record.Final
	locator.StageID = "ttl_reject"
	d.final, err = b.beginStageWithLocator(ctx, locator, time.Until(d.effectDeadline), func(StageAttemptLocator) (Mutation, error) { return mutation, nil }, nil)
	if err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	d.finalSubmitted = true
	outcome, err := b.CommitStage(ctx, d.final)
	if err != nil || outcome != OutcomeCommitted {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	d.outcome = OutcomeAborted
	cleanup, cancel := b.requestContext(context.Background())
	cleanupErr := b.ReleaseStage(cleanup, d.final)
	cancel()
	if cleanupErr != nil {
		return errors.Join(ErrOutcomeUnknown, cleanupErr)
	}
	return ErrConflict
}

func (b *Backend) publishSandboxTTL(ctx context.Context, c *OperationCapability, d *sandboxTTLDraft) error {
	if d.pendingRevision != 0 {
		return nil
	}
	if d.publish == nil {
		if d.publishTried {
			outcome, err := b.ResolveStage(ctx, d.publishRef)
			if err != nil || outcome != OutcomeCommitted {
				return errors.Join(ErrOutcomeUnknown, err)
			}
		} else {
			mutation, _, err := b.sandboxTTLStage(d.record, 0)
			if err != nil {
				return err
			}
			d.publishTried = true
			d.publish, err = b.beginStageWithLocator(ctx, d.record.Publish, time.Until(d.effectDeadline), func(StageAttemptLocator) (Mutation, error) { return mutation, nil }, nil)
			if err != nil {
				return errors.Join(ErrOutcomeUnknown, err)
			}
			if d.publish.reference != d.publishRef {
				return ErrInvalidMutation
			}
		}
	}
	if d.publish != nil {
		var outcome Outcome
		var err error
		if d.publishSubmitted {
			outcome, err = b.ResolveStage(ctx, d.publishRef)
		} else {
			d.publishSubmitted = true
			outcome, err = b.CommitStage(ctx, d.publish)
		}
		if err != nil || outcome != OutcomeCommitted {
			return errors.Join(ErrOutcomeUnknown, err)
		}
	}
	pending, _, err := b.sandboxTTLKeys(d.record.Operation.Reference.Partition, d.record.Control.SandboxID)
	if err != nil {
		return err
	}
	points, err := b.readEnrolledPoints(ctx, []string{pending}, []int{maxRecordBytes})
	if err != nil {
		return err
	}
	wire, err := encodeSandboxTTL(d.record)
	if err != nil || !enrolledImmutableKV(points[0]) || string(points[0].Value) != wire {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	d.pendingRevision = points[0].ModRevision
	_, d.finalRef, err = b.sandboxTTLStage(d.record, d.pendingRevision)
	if err != nil {
		return err
	}
	if d.publish != nil {
		cleanup, cancel := b.requestContext(context.Background())
		d.guardCleanupError = errors.Join(d.guardCleanupError, b.ReleaseStage(cleanup, d.publish))
		cancel()
		d.publish = nil
	}
	return nil
}

func (b *Backend) sandboxTTLLive(ctx context.Context, c *OperationCapability, d *sandboxTTLDraft) error {
	// A fresh controlled-clock observation validates certification, but it may
	// not move the already-captured local deadline or let its own RPC overrun it.
	ctx, cancel := context.WithDeadline(ctx, d.effectDeadline)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.admissionLive(); err != nil {
		return err
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return err
	}
	if !now.Add(time.Second).Before(d.record.Control.ExpiresAt) || !now.Add(time.Second).Before(d.record.ExpiresAt) {
		return ErrOperationAdmissionClosed
	}
	if _, err = b.enrolledVerifier.VerifyTargetActivation(d.previous.Wire(), d.previous.RuntimeCertificate(), d.previous.IssuerCertificate(), d.previous.Birth(), now); err != nil {
		return err
	}
	_, err = b.enrolledVerifier.VerifyTargetActivation(d.activation.Wire(), d.activation.RuntimeCertificate(), d.activation.IssuerCertificate(), d.activation.Birth(), now)
	return err
}

// This default-linearizable native Range fences the original operation and
// every permanent source immediately before the Backend's own transport call.
// It does not renew the lease or turn the earlier publication into permission.
func (b *Backend) fenceSandboxTTL(ctx context.Context, d *sandboxTTLDraft) error {
	mutation, _, err := b.sandboxTTLStage(d.record, d.pendingRevision)
	if err != nil {
		return err
	}
	compares := append(b.baseComparisons(), mutation.Comparisons...)
	request := &pb.TxnRequest{Compare: taskProtoComparisons(compares), Success: taskProtoPoints([]string{b.identityKey}), Failure: taskProtoPoints([]string{b.identityKey})}
	if len(request.Compare)+len(request.Success)+len(request.Failure) > 64 || request.Size() > 128<<10 {
		return ErrInvalidMutation
	}
	rpc, cancel := b.requestContext(ctx)
	response, err := b.client.Txn(rpc).If(compares...).Then(clientv3.OpGet(b.identityKey)).Else(clientv3.OpGet(b.identityKey)).Commit()
	err = errors.Join(err, rpc.Err())
	cancel()
	if err != nil {
		return errors.Join(ErrOutcomeUnknown, err)
	}
	if err := b.validateFenceCheckResponse(response); err != nil {
		return err
	}
	if !response.Succeeded {
		return ErrConflict
	}
	return nil
}

func (b *Backend) sandboxTTLDestination(r sandboxTTLRecord, destination *ct.Destination) error {
	want := p.RuntimeIdentityContext{SandboxID: r.Control.SandboxID, WorkspaceHash: r.Control.WorkspaceHash, Generation: r.Control.Generation, Runtime: p.RuntimeReference(*r.Control.Runtime)}
	if destination == nil || destination.Identity() != want {
		return ErrInvalidRecord
	}
	return nil
}

// ReadSandboxTTLCurrent returns only the committed current material, or nil for
// an unchanged legacy object. Closing controls remain readable: cleanup needs
// current transport while historical publication/effect certification stays fixed.
func (b *Backend) ReadSandboxTTLCurrent(ctx context.Context, view *SandboxReadView) (*SandboxTTLCurrent, error) {
	r, kv, err := b.readSandboxTTLRecord(ctx, view, false)
	if err != nil || kv == nil {
		return nil, err
	}
	a, err := b.authenticateSandboxTTL(ctx, view, r)
	if err != nil {
		return nil, err
	}
	return &SandboxTTLCurrent{Baseline: r.Baseline, ExpiresAt: r.ExpiresAt, Timeout: r.Timeout, PreviousDigest: r.PreviousDigest, Activation: a}, nil
}

// ReadSandboxTTLPending returns actual fixed native candidate diagnostics. A
// missing row is only that snapshot's absence, not proof of failed installation.
func (b *Backend) ReadSandboxTTLPending(ctx context.Context, view *SandboxReadView) (*SandboxTTLPending, error) {
	r, kv, err := b.readSandboxTTLRecord(ctx, view, true)
	if err != nil || kv == nil {
		return nil, err
	}
	a, err := b.authenticateSandboxTTL(ctx, view, r)
	if err != nil {
		return nil, err
	}
	_, ref, err := b.sandboxTTLStage(r, kv.ModRevision)
	if err != nil {
		return nil, err
	}
	return &SandboxTTLPending{Baseline: r.Baseline, ExpiresAt: r.ExpiresAt, Timeout: r.Timeout, PreviousDigest: r.PreviousDigest, Activation: a, Operation: r.Operation.Reference, Stage: ref}, nil
}

func (b *Backend) readSandboxTTLRecord(ctx context.Context, view *SandboxReadView, pending bool) (sandboxTTLRecord, *mvccpb.KeyValue, error) {
	var r sandboxTTLRecord
	if b == nil || ctx == nil || view == nil || view.self != view || view.origin != b || view.records.Control == nil || view.records.Placement == nil {
		return r, nil, ErrInvalidRecord
	}
	if err := b.ConfirmSandboxView(ctx, view); err != nil {
		return r, nil, err
	}
	pendingKey, currentKey, err := b.sandboxTTLKeys(view.records.Placement.Partition, view.records.Control.SandboxID)
	if err != nil {
		return r, nil, err
	}
	points, err := b.readEnrolledPoints(ctx, []string{pendingKey, currentKey}, []int{maxRecordBytes, maxRecordBytes})
	if err != nil {
		return r, nil, err
	}
	kv := points[1]
	if pending {
		kv = points[0]
	}
	if kv == nil {
		return r, nil, b.ConfirmSandboxView(ctx, view)
	}
	r, err = decodeSandboxTTL(kv, pending)
	if err != nil || r.Operation.Reference.Namespace != b.namespace.Root() || r.Operation.Reference.RestoreEpoch != b.restoreEpoch || r.Operation.Reference.Partition != view.records.Placement.Partition {
		return r, nil, errors.Join(ErrCorruptRecord, err)
	}
	want := r.Control
	if !pending {
		want.ExpiresAt = r.ExpiresAt
	}
	actual := *view.records.Control
	// Only the phase may have moved into cleanup; neither TTL read grants a
	// new operation or changes the current gate, runtime tuple or expiry.
	actual.Phase = want.Phase
	if !reflect.DeepEqual(want, actual) {
		return r, nil, ErrConflict
	}
	_, publishRef, err := b.sandboxTTLStage(r, 0)
	if err != nil {
		return r, nil, err
	}
	_, publishReceipt, err := b.stageKeys(publishRef)
	if err != nil {
		return r, nil, err
	}
	keys := []string{publishReceipt}
	refs := []StageReference{publishRef}
	outcomes := []Outcome{OutcomeCommitted}
	publishBirth := kv.ModRevision
	if !pending {
		publishBirth = r.PendingRevision
	}
	if r.recovery != nil {
		// A recovery rewrite changes only pending's Mod. Publication remains
		// bound to the original v1 birth and its unchanged original digest.
		publishBirth = r.recovery.OriginalBirth
		original, err := r.recovery.original()
		if err != nil {
			return r, nil, err
		}
		_, oldFinal, err := b.sandboxTTLStage(original, publishBirth)
		if err != nil {
			return r, nil, err
		}
		_, oldReceipt, err := b.stageKeys(oldFinal)
		if err != nil {
			return r, nil, err
		}
		keys, refs, outcomes = append(keys, oldReceipt), append(refs, oldFinal), append(outcomes, OutcomeAborted)
	}
	births := make([]int64, len(keys))
	births[0] = publishBirth
	if !pending {
		_, finalRef, err := b.sandboxTTLStage(r, r.PendingRevision)
		if err != nil {
			return r, nil, err
		}
		_, finalReceipt, err := b.stageKeys(finalRef)
		if err != nil {
			return r, nil, err
		}
		keys, refs = append(keys, finalReceipt), append(refs, finalRef)
		outcomes, births = append(outcomes, OutcomeCommitted), append(births, kv.ModRevision)
	}
	caps := make([]int, len(keys))
	for i := range caps {
		caps[i] = 4096
	}
	receipts, err := b.readEnrolledPoints(ctx, keys, caps)
	if err != nil {
		return r, nil, err
	}
	for i, ref := range refs {
		outcome, err := decodeReceipt(receipts[i], ref)
		if err != nil || outcome != outcomes[i] {
			return r, nil, errors.Join(ErrCorruptReceipt, err)
		}
		if births[i] > 0 && receipts[i].CreateRevision != births[i] {
			return r, nil, ErrCorruptReceipt
		}
		if outcomes[i] == OutcomeAborted && receipts[i].CreateRevision >= r.recovery.reservedMod {
			return r, nil, ErrCorruptReceipt
		}
	}
	if err := b.ConfirmSandboxView(ctx, view); err != nil {
		return r, nil, err
	}
	return r, kv, nil
}

func (b *Backend) sandboxTTLInitialActivation(ctx context.Context, view *SandboxReadView) (p.TargetActivationEvidence, error) {
	if b.enrolledVerifier == nil {
		return p.TargetActivationEvidence{}, ErrInvalidConfiguration
	}
	history, err := b.ReadSandboxRuntime(ctx, view)
	if err != nil {
		return p.TargetActivationEvidence{}, err
	}
	if history == nil || history.Publication == nil {
		return p.TargetActivationEvidence{}, ErrCorruptRecord
	}
	receipt, err := p.DecodeNodeEnrollmentReceipt(history.Publication.Enrollment.Receipt)
	if err != nil {
		return p.TargetActivationEvidence{}, err
	}
	return b.enrolledVerifier.VerifyNodeEnrollmentRequest(history.Publication.Enrollment.Request, receipt.Claims.ObservedAt)
}

func (b *Backend) authenticateSandboxTTL(ctx context.Context, view *SandboxReadView, r sandboxTTLRecord) (p.TargetActivationEvidence, error) {
	initial, err := b.sandboxTTLInitialActivation(ctx, view)
	if err != nil {
		return p.TargetActivationEvidence{}, err
	}
	a, err := b.enrolledVerifier.VerifyTargetActivation(r.Activation, r.RuntimeCert, r.IssuerCert, initial.Birth(), r.Baseline)
	if err != nil {
		return p.TargetActivationEvidence{}, err
	}
	if a.Identity() != initial.Identity() || a.DataGateEpoch() != initial.DataGateEpoch() || a.ActivationID() != r.Final.AttemptID || !bytes.Equal(a.Wire(), r.Activation) || !bytes.Equal(a.RuntimeCertificate(), r.RuntimeCert) || !bytes.Equal(a.IssuerCertificate(), r.IssuerCert) || r.CurrentRevision == 0 && r.PreviousDigest != initial.Digest() {
		return p.TargetActivationEvidence{}, ErrCorruptRecord
	}
	return a, nil
}

func (b *Backend) querySandboxTTLPending(ctx context.Context, view *SandboxReadView, destination *ct.Destination) (SandboxTTLResult, error) {
	result := SandboxTTLResult{Outcome: OutcomeUnknown}
	pending, err := b.ReadSandboxTTLPending(ctx, view)
	if err != nil || pending == nil {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	result.Stage, result.Baseline, result.ExpiresAt, result.Timeout = pending.Stage, pending.Baseline, pending.ExpiresAt, pending.Timeout
	if destination.Identity() != pending.Activation.Identity() || !bytes.Equal(destination.RuntimeCertificate(), pending.Activation.RuntimeCertificate()) {
		return result, ErrInvalidRecord
	}
	response, err := destination.QueryRuntimeActivationRenewal(ctx, pending.Activation.DataGateEpoch(), pending.PreviousDigest, pending.Activation.Digest())
	if err != nil || !response.Installed || response.GateState != "open" || response.Identity != pending.Activation.Identity() || response.DataGateEpoch != pending.Activation.DataGateEpoch() || response.PreviousDigest != pending.PreviousDigest || response.CandidateDigest != pending.Activation.Digest() {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	// Resolve may durably abort the original metadata attempt. That never
	// rolls back installed runtime certification or authorizes a new Stage.
	result.Outcome, err = b.ResolveStage(ctx, pending.Stage)
	if err != nil || result.Outcome != OutcomeCommitted {
		return result, errors.Join(ErrOutcomeUnknown, err)
	}
	return result, nil
}
