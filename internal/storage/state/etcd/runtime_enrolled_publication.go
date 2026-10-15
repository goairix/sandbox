package etcd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type runtimeEnrolledPublicationOwner struct {
	backend *Backend
	context context.Context
	cancel  context.CancelFunc
	done    chan struct{}
}

func enrolledPublicationInput(proof []byte, enrollment p.EnrolledRuntimeEvidence) (string, error) {
	if len(proof) == 0 || len(proof) > 8192 || len(enrollment.ObserverCertificate) == 0 || len(enrollment.ObserverCertificate) > 2048 || len(enrollment.Receipt) == 0 || len(enrollment.Receipt) > 4096 {
		return "", ErrInvalidRecord
	}
	if _, err := p.EncodeNodeEnrollmentRequest(enrollment.Request); err != nil {
		return "", err
	}
	wire, err := json.Marshal(struct {
		Proof      []byte
		Enrollment p.EnrolledRuntimeEvidence
	}{proof, enrollment})
	if err != nil {
		return "", ErrInvalidRecord
	}
	digest := sha256.Sum256(wire)
	return hex.EncodeToString(digest[:]), nil
}

// PublishRuntimeEnrolled atomically writes metadata under one original claim
// and one frozen Stage attempt. Its result grants no runtime effects.
func (b *Backend) PublishRuntimeEnrolled(ctx context.Context, c *CreationClaim, proof []byte, enrollment p.EnrolledRuntimeEvidence) (result RuntimeEnrolledPublishResult, err error) {
	result.Outcome = OutcomeUnknown
	if b == nil || b.enrolledPublication == nil || b.publicationVerifier == nil || b.enrolledVerifier == nil {
		return result, ErrInvalidConfiguration
	}
	if c == nil || c.origin != b {
		return result, ErrInvalidRecord
	}
	owned, material := bytes.Clone(proof), enrollment.Clone()
	input, err := enrolledPublicationInput(owned, material)
	if err != nil {
		return result, err
	}
	o, err := b.registerEnrolledPublication(ctx)
	if err != nil {
		return result, err
	}
	defer o.finish()
	ctx = o.context
	c.mu.Lock()
	frozen := c.enrolledAttempt
	c.mu.Unlock()
	if frozen != nil {
		return b.replayEnrolledPublication(ctx, c, input, frozen)
	}
	comparisons, err := b.creationClaimComparisons(c)
	if err != nil {
		return result, err
	}
	bundle, err := b.loadEnrolledPreparation(ctx, c.workspace, c.reference.IntentID)
	if err != nil {
		return result, err
	}
	evidence, err := b.authenticateEnrolledPublication(ctx, c, bundle, owned, material)
	if err != nil {
		return result, err
	}
	d := bundle.Dispatch.Record
	e, a := enrollmentRecords(d, material)
	eWire, err := encodeEnrolledRecord(e)
	if err != nil {
		return result, err
	}
	aWire, err := encodeEnrolledRecord(a)
	if err != nil {
		return result, err
	}
	eDigest, _ := snapshotDigest([]byte(eWire))
	aDigest, _ := snapshotDigest([]byte(aWire))
	rc := evidence.EnrollmentReceipt()
	r := RuntimeEnrolledPublicationRecord{RuntimePublicationRecord: RuntimePublicationRecord{Version: 2, IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, RestoreEpoch: d.RestoreEpoch, Generation: d.Generation, DataGateEpoch: c.control.DataGateEpoch, Snapshot: d.Snapshot, ExpiresAt: d.ExpiresAt, OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, ProofDigest: evidence.Digest(), CertificateDigest: bundle.Binding.Record.CertificateDigest, Runtime: RuntimeReference(evidence.Certificate().Runtime()), Claim: preparationClaim(c)}, EnrollmentDigest: rc.EnrollmentDigest, ObserverCertificateDigest: rc.ObserverCertificateDigest, ActivationDigest: rc.ActivationDigest, NodeUID: rc.NodeUID, NodeBootID: rc.NodeBootID, EnrollmentRecordDigest: eDigest, ActivationRecordDigest: aDigest, DispatchAttempt: d.Attempt, BindingAttempt: bundle.Binding.Record.Attempt, MountAttemptLocator: bundle.Mount.Record.Attempt}
	locator := StageAttemptLocator{Namespace: b.namespace.Root(), Partition: c.reference.Partition, RequestID: c.request.RequestID, StageID: enrolledPublicationStage, AttemptID: uuid.NewString(), RestoreEpoch: b.restoreEpoch}
	r.Attempt = locator
	body := RuntimeEnrolledPublicationProofRecord{RuntimePublicationProofRecord: RuntimePublicationProofRecord{Version: 2, IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, RestoreEpoch: d.RestoreEpoch, Generation: d.Generation, ProofDigest: evidence.Digest(), Payload: evidence.Wire()}}
	ownerKey, _, err := b.namespace.workspaceKeys(c.workspace)
	if err != nil {
		return result, err
	}
	controlKey, _, err := b.namespace.sandboxKeys(c.reference.Partition, c.reference.SandboxID, c.control.Snapshot.Version)
	if err != nil {
		return result, err
	}
	requestKey, err := b.namespace.requestKey(c.request.RequestHash)
	if err != nil {
		return result, err
	}
	intentKey, err := b.namespace.intentKey(c.reference.Partition, c.reference.IntentID)
	if err != nil {
		return result, err
	}
	journalKey, proofKey, err := b.namespace.runtimePublicationKeys(c.reference.Partition, c.reference.IntentID)
	if err != nil {
		return result, err
	}
	enrollmentKey, activationKey, err := b.namespace.runtimeEnrollmentKeys(c.reference.Partition, c.reference.IntentID)
	if err != nil {
		return result, err
	}
	var owner WorkspaceOwnerRecord
	var intent CreationIntentRecord
	if err := decodePublicationOriginal(comparisons, ownerKey, &owner); err != nil {
		return result, err
	}
	if err := decodePublicationOriginal(comparisons, intentKey, &intent); err != nil {
		return result, err
	}
	ownerRuntime, controlRuntime := r.Runtime, r.Runtime
	owner.Runtime, owner.MountAttempt = &ownerRuntime, 0
	control, request := c.control, c.request
	control.Runtime, control.MountAttempt, control.Phase = &controlRuntime, 0, PhaseActive
	request.Phase, intent.Phase = "completed", "published"
	comparisons = append(comparisons, immutablePreparationComparisons(bundle.DispatchKVs)...)
	comparisons = append(comparisons, immutablePreparationComparisons(bundle.BindingKVs)...)
	comparisons = append(comparisons, immutablePreparationComparisons(bundle.MountKVs)...)
	for _, key := range []string{journalKey, proofKey, enrollmentKey, activationKey} {
		comparisons = append(comparisons, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	}
	mutation := Mutation{Comparisons: comparisons}
	for i, record := range []interface{ Validate() error }{owner, control, request, intent} {
		wire, err := encodeDomainRecord(record)
		if err != nil {
			return result, err
		}
		mutation.Writes = append(mutation.Writes, Write{Key: []string{ownerKey, controlKey, requestKey, intentKey}[i], Value: []byte(wire)})
	}
	for i, record := range []interface{ Validate() error }{r, body, e, a} {
		wire, err := encodeEnrolledRecord(record)
		if err != nil {
			return result, err
		}
		mutation.Writes = append(mutation.Writes, Write{Key: []string{journalKey, proofKey, enrollmentKey, activationKey}[i], Value: []byte(wire)})
	}
	mutation, digest, err := prepareEnrolledMutation(b.namespace, mutation)
	if err != nil {
		return result, err
	}
	ref := locator.reference(digest)
	if err := b.preflightEnrolledMaximum(ref, mutation); err != nil {
		return result, err
	}
	frozen, original, err := c.freezeEnrolledPublication(input, ref, o)
	if err != nil {
		return result, err
	}
	if !original {
		return b.replayEnrolledPublication(ctx, c, input, frozen)
	}
	result.Reference = ref // fixed before the first Grant, including unknown ACK.
	stage, cleanup, err := b.beginEnrolledStage(ctx, ref, mutation)
	result.GuardCleanupError = cleanup
	if err != nil {
		return result, err
	}
	defer func() {
		result.GuardCleanupError = errors.Join(result.GuardCleanupError, b.revokeEnrolledStage(stage.lease))
	}()
	if _, err := b.creationClaimComparisons(c); err != nil {
		return result, err
	}
	if _, err := b.authenticateEnrolledPublication(ctx, c, bundle, owned, material); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Outcome, err = b.commitEnrolledStage(ctx, stage)
	if result.Outcome == OutcomeCommitted {
		result.Disposition = PublishDeclared
		result.Entry = &RuntimeEnrolledPublicationEntry{Record: r, Proof: bytes.Clone(body.Payload), Enrollment: material.Clone(), Reference: ref}
	}
	return result, err
}

func (b *Backend) authenticateEnrolledPublication(ctx context.Context, c *CreationClaim, bundle *runtimePreparationBundle, proof []byte, enrollment p.EnrolledRuntimeEvidence) (p.EnrolledPublicationEvidence, error) {
	if bundle == nil || bundle.Dispatch == nil || bundle.Binding == nil || bundle.Mount == nil || bundle.Binding.Record.WorkspaceMode != "plain" || bundle.Mount.Record.MountAttempt != 0 {
		return p.EnrolledPublicationEvidence{}, ErrConflict
	}
	if _, err := b.authenticatePreparation(ctx, c, bundle, bundle.Binding.Certificate); err != nil {
		return p.EnrolledPublicationEvidence{}, err
	}
	now, err := b.observePublicationClock(ctx)
	if err != nil {
		return p.EnrolledPublicationEvidence{}, err
	}
	ref := c.reference
	expected := p.PublicationContext{Certificate: b.preparationCertificateContext(bundle.Dispatch.Record), Runtime: p.RuntimeReference(bundle.Binding.Record.Runtime), CertificateDigest: bundle.Binding.Record.CertificateDigest, Claim: p.ClaimReference{ClaimID: ref.ClaimID, CreateRevision: ref.CreateRevision, LeaseID: ref.LeaseID}, DataGateEpoch: c.control.DataGateEpoch}
	e, err := b.publicationVerifier.VerifyEnrolledPublication(proof, expected, enrollment, b.enrolledPublication.ContractDigest, now)
	if err != nil {
		return p.EnrolledPublicationEvidence{}, err
	}
	if !bytes.Equal(e.Certificate().Wire(), bundle.Binding.Certificate) {
		return p.EnrolledPublicationEvidence{}, ErrCorruptRecord
	}
	return e, nil
}

func (b *Backend) replayEnrolledPublication(ctx context.Context, c *CreationClaim, input string, frozen *runtimeEnrolledFrozenAttempt) (result RuntimeEnrolledPublishResult, err error) {
	result.Outcome, result.Reference = OutcomeUnknown, frozen.reference
	if frozen.inputDigest != input {
		return result, ErrIdempotencyConflict
	}
	select {
	case <-frozen.owner.done:
	case <-ctx.Done():
		return result, ctx.Err()
	}
	result.Outcome, err = b.readRuntimeEnrolledAttempt(ctx, frozen.reference)
	if err != nil || result.Outcome != OutcomeCommitted {
		return result, err
	}
	entry, err := b.loadRuntimePublicationEnrolled(ctx, c.workspace, c.reference.IntentID)
	if err != nil {
		result.Outcome = OutcomeUnknown
		return result, err
	}
	if entry == nil || entry.Reference != frozen.reference {
		result.Outcome = OutcomeUnknown
		return result, ErrCorruptRecord
	}
	result.Disposition, result.Entry = PublishReplay, entry
	return result, nil
}

type runtimeEnrolledFrozenAttempt struct {
	inputDigest string
	reference   StageReference
	owner       *runtimeEnrolledPublicationOwner
}

func (b *Backend) registerEnrolledPublication(ctx context.Context) (*runtimeEnrolledPublicationOwner, error) {
	if ctx == nil || b == nil || b.enrolledPublication == nil || b.archiveLife == nil {
		return nil, ErrInvalidConfiguration
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.archiveLife.mu.Lock()
	defer b.archiveLife.mu.Unlock()
	if b.archiveLife.closed || len(b.archiveLife.enrolled) >= int(b.enrolledPublication.MaxActive) {
		return nil, ErrConflict
	}
	owned, cancel := context.WithCancel(ctx)
	o := &runtimeEnrolledPublicationOwner{backend: b, context: owned, cancel: cancel, done: make(chan struct{})}
	if b.archiveLife.enrolled == nil {
		b.archiveLife.enrolled = make(map[*runtimeEnrolledPublicationOwner]struct{})
	}
	b.archiveLife.enrolled[o] = struct{}{}
	return o, nil
}

func (o *runtimeEnrolledPublicationOwner) finish() {
	o.cancel()
	b := o.backend
	b.archiveLife.mu.Lock()
	delete(b.archiveLife.enrolled, o)
	close(o.done)
	b.archiveLife.mu.Unlock()
}

func (c *CreationClaim) freezeEnrolledPublication(input string, ref StageReference, owner *runtimeEnrolledPublicationOwner) (*runtimeEnrolledFrozenAttempt, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.enrolledAttempt != nil {
		if c.enrolledAttempt.inputDigest != input {
			return nil, false, ErrIdempotencyConflict
		}
		return c.enrolledAttempt, false, nil
	}
	a := &runtimeEnrolledFrozenAttempt{inputDigest: input, reference: ref, owner: owner}
	c.enrolledAttempt = a
	return a, true, nil
}
