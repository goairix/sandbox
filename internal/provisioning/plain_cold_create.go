package provisioning

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
)

type plainColdDispatch struct {
	Version   int                          `json:"version"`
	Capsule   plainColdCapsule             `json:"capsule"`
	CreatedAt time.Time                    `json:"created_at"`
	Pod       kube.EnrolledPlainPodRequest `json:"pod"`
}

func (s *PlainColdService) Create(ctx context.Context, w state.WorkspaceIdentity, request PlainColdRequest) (PlainColdResult, error) {
	result := PlainColdResult{State: PlainColdPending, RequestID: request.RequestID, IntentID: request.IntentID, SandboxID: request.SandboxID}
	call, err := s.enter(ctx)
	if err != nil {
		return result, err
	}
	defer s.leave(call)
	ctx = call.context
	wire, err := coldRequestCapsule(s.profile, w, request)
	if err != nil {
		return result, err
	}
	if err := s.ready(ctx); err != nil {
		return result, err
	}
	now, err := s.now(ctx)
	if err != nil {
		return result, err
	}
	acquired, err := s.backend.AcquireIntent(ctx, state.AcquireIntentInput{Workspace: w, Principal: request.Principal, IdempotencyKey: request.IdempotencyKey, RequestID: request.RequestID, IntentID: request.IntentID, SandboxID: request.SandboxID, SnapshotVersion: request.SnapshotVersion, Payload: wire, Now: now, TTL: request.TTL})
	result.Stage = acquired.Reference
	if err != nil {
		return result, err
	}
	if acquired.Disposition == state.AcquireOccupied {
		result.State = PlainColdRetryable
		return result, state.ErrConflict
	}
	if acquired.Outcome != state.OutcomeCommitted {
		return result, state.ErrOutcomeUnknown
	}
	return s.resume(ctx, w, result)
}

func (s *PlainColdService) Resume(ctx context.Context, w state.WorkspaceIdentity, prior PlainColdResult) (PlainColdResult, error) {
	result := PlainColdResult{State: PlainColdPending, RequestID: prior.RequestID, IntentID: prior.IntentID, SandboxID: prior.SandboxID, Stage: prior.Stage, Runtime: prior.Runtime}
	if prior.Attempt != nil {
		attempt := *prior.Attempt
		result.Attempt = &attempt
	}
	call, err := s.enter(ctx)
	if err != nil {
		return result, err
	}
	defer s.leave(call)
	if result.Stage != (state.StageReference{}) {
		outcome, err := s.arbitrate(call.context, w, result)
		if err != nil {
			return result, err
		}
		if outcome == state.OutcomeUnknown {
			return result, state.ErrOutcomeUnknown
		}
		if outcome == state.OutcomeAborted && result.Stage.StageID == "acquire" {
			result.State = PlainColdRetryable
			return result, state.ErrConflict
		}
	}
	return s.resume(call.context, w, result)
}

func (s *PlainColdService) arbitrate(ctx context.Context, w state.WorkspaceIdentity, result PlainColdResult) (state.Outcome, error) {
	ref := result.Stage
	if ref.Namespace != s.config.Binding.Namespace || ref.RestoreEpoch != s.config.Binding.RestoreEpoch || ref.Partition != w.Partition() || ref.RequestID != result.RequestID {
		return state.OutcomeUnknown, ErrPlainColdProfile
	}
	switch ref.StageID {
	case "runtime_publish_enrolled_v2":
		return s.backend.ReadRuntimeEnrolledPublicationAttempt(ctx, ref)
	case "acquire", "runtime_dispatch", "runtime_bind", "runtime_mount", "cold_preparation_settle":
		return s.backend.ResolveStage(ctx, ref)
	default:
		return state.OutcomeUnknown, ErrPlainColdProfile
	}
}

func (s *PlainColdService) resume(ctx context.Context, w state.WorkspaceIdentity, result PlainColdResult) (PlainColdResult, error) {
	intent := result.IntentID
	i, err := s.backend.LoadCreationIntent(ctx, w, intent)
	if err != nil {
		return result, err
	}
	if i == nil {
		return result, state.ErrConflict
	}
	result.SandboxID = i.SandboxID
	if i.Phase == "published" {
		return s.completed(ctx, w, result)
	}
	if err := s.ready(ctx); err != nil {
		return result, err
	}
	claim, err := s.backend.ClaimCreation(ctx, w, intent, s.worker, 30*time.Second)
	if err != nil {
		if errors.Is(err, state.ErrConflict) {
			result.State = PlainColdRetryable
		}
		return result, err
	}
	defer s.backend.ReleaseCreationClaim(context.Background(), claim)
	control, err := s.backend.LoadControl(ctx, w.Partition(), i.SandboxID)
	if err != nil {
		return result, err
	}
	if control == nil || control.IntentID != intent || control.WorkspaceHash != w.Hash() || control.Generation != claim.Reference().Generation {
		return result, state.ErrConflict
	}
	snapshot, err := s.backend.LoadSnapshot(ctx, w.Partition(), i.SandboxID, control.Snapshot)
	if err != nil {
		return result, err
	}
	if snapshot == nil {
		return result, state.ErrCorruptRecord
	}
	var capsule plainColdCapsule
	if err := decodeColdCapsule(snapshot.Payload, &capsule); err != nil {
		return result, err
	}
	if capsule.Version != 1 || capsule.Deployment != s.profile || capsule.WorkspaceHash != w.Hash() || capsule.Partition != w.Partition() || capsule.IntentID != intent || capsule.SandboxID != i.SandboxID || capsule.SnapshotVersion != control.Snapshot.Version || capsule.TTL <= 0 || capsule.TTL > 365*24*time.Hour {
		return result, ErrPlainColdProfile
	}
	if result.RequestID != "" && result.RequestID != capsule.RequestID {
		return result, ErrPlainColdProfile
	}
	result.RequestID = capsule.RequestID
	pod := s.pod(capsule.SandboxID, intent, control.Generation)
	dispatchInput := plainColdDispatch{Version: 1, Capsule: capsule, CreatedAt: control.ExpiresAt.Add(-capsule.TTL), Pod: pod}
	payload, err := json.Marshal(dispatchInput)
	if err != nil {
		return result, err
	}
	reservation, err := s.backend.ReserveColdPreparation(ctx, claim, s.quota)
	if err != nil {
		if errors.Is(err, state.ErrPreparationCapacity) {
			result.State = PlainColdCapacity
		} else if errors.Is(err, state.ErrConflict) {
			result.State = PlainColdRetryable
		}
		return result, err
	}
	if reservation.Outcome != state.OutcomeCommitted {
		return result, state.ErrOutcomeUnknown
	}
	dispatch, err := s.backend.DeclareRuntimeDispatch(ctx, claim, state.RuntimeDispatchInput{Kind: state.RuntimeDispatchCreate, Target: s.config.Binding.Target, Payload: payload})
	result.Stage = dispatch.Reference
	if err != nil {
		return result, err
	}
	if dispatch.Outcome != state.OutcomeCommitted || dispatch.Entry == nil {
		return result, state.ErrOutcomeUnknown
	}
	// The original opaque permit is retained only on this call's stack. Durable
	// attempt replay, including lost ACK recovery, always follows Inspect.
	attempt, err := s.backend.BeginColdCreateAttempt(ctx, claim)
	if attempt.Record != nil {
		result.Attempt = attempt.Record
	}
	if err != nil {
		return result, err
	}
	var observed kube.EnrolledCreateResult
	if attempt.Permit != nil {
		s.mu.Lock()
		if s.closed || ctx.Err() != nil {
			s.mu.Unlock()
			return result, context.Canceled
		}
		err = s.backend.ConsumeColdCreatePermit(claim, attempt.Permit)
		s.mu.Unlock()
		if err != nil {
			return result, err
		}
		observed, err = s.driver.CreateOnce(ctx, pod)
	} else {
		prior, loadErr := s.backend.LoadColdCreateObservation(ctx, w, intent)
		if loadErr != nil {
			return result, loadErr
		}
		uid := ""
		if prior != nil {
			uid = prior.PodUID
			result.Runtime.ID, result.Runtime.UID = pod.Spec.ID, uid
		}
		observed, err = s.driver.InspectOriginal(ctx, pod, uid)
	}
	if err != nil {
		return result, err
	}
	if observed.State != kube.EnrolledCreateObserved {
		return result, state.ErrOutcomeUnknown
	}
	observation, err := s.backend.RecordColdCreateObservation(ctx, claim, observed.PodUID)
	if err != nil {
		return result, err
	}
	result.Runtime.ID, result.Runtime.UID = pod.Spec.ID, observation.PodUID
	target := s.target(pod, observation.PodUID)
	stored, err := s.backend.LoadColdBootstrapMaterial(ctx, w, intent)
	if err != nil {
		result.State = PlainColdRecoveryRequired
		return result, err
	}
	existingMaterial := stored != nil
	if stored == nil {
		hello, helloErr := s.bootstrap(ctx, target, ct.BootstrapRequest{Version: 1, Purpose: "hello"})
		if helloErr != nil {
			return result, helloErr
		}
		if hello.Birth == nil || hello.Birth.UID != s.config.UID || hello.Birth.GID != s.config.GID || hello.Birth.NetworkAllowed != s.config.NetworkAllowed || hello.Birth.ContractDigest != s.config.ContractDigest {
			return result, ErrPlainColdProfile
		}
		material, signErr := s.material(ctx, *hello.Birth, dispatch.Entry.Record, pod.Spec.ID, observation.PodUID)
		if signErr != nil {
			result.State = PlainColdRecoveryRequired
			return result, signErr
		}
		stored, err = s.backend.FixColdBootstrapMaterial(ctx, claim, material)
		if err != nil {
			return result, err
		}
	}
	m := stored.Material
	result.Runtime.BootID = m.Birth.BootID
	identity := p.RuntimeIdentityContext{SandboxID: i.SandboxID, WorkspaceHash: w.Hash(), Generation: control.Generation, Runtime: p.RuntimeReference{ID: pod.Spec.ID, UID: observation.PodUID, BootID: m.Birth.BootID}}
	destination, credential, err := s.destination(target, m, identity)
	if err != nil {
		result.State = PlainColdRecoveryRequired
		return result, err
	}
	defer credential.Close()
	ref := claim.Reference()
	publicationClaim := p.ClaimReference{ClaimID: ref.ClaimID, CreateRevision: ref.CreateRevision, LeaseID: ref.LeaseID}
	var enrollment p.EnrolledRuntimeEvidence
	var proof []byte
	if existingMaterial {
		// ACK loss cannot mint a new Birth or retry an unauthenticated Hello.
		// First ask the exact original runtime over its pinned management TLS.
		enrollment, proof, err = destination.ReadEnrolledPublication(ctx, m.PublicationCertificate, publicationClaim)
	}
	if !existingMaterial || err != nil {
		if _, err = s.bootstrap(ctx, target, m.BootstrapRequest()); err != nil {
			return result, err
		}
		proof = nil
	}
	bound, err := s.backend.BindRuntime(ctx, claim, m.PublicationCertificate)
	result.Stage = bound.Reference
	if err != nil {
		return result, err
	}
	if bound.Outcome != state.OutcomeCommitted {
		return result, state.ErrOutcomeUnknown
	}
	mounted, err := s.backend.ConsumeRuntimeMount(ctx, claim)
	result.Stage = mounted.Reference
	if err != nil {
		return result, err
	}
	if mounted.Outcome != state.OutcomeCommitted {
		return result, state.ErrOutcomeUnknown
	}
	if proof == nil {
		enrollment, proof, err = destination.ReadEnrolledPublication(ctx, m.PublicationCertificate, publicationClaim)
		if err != nil {
			return result, err
		}
	}
	published, err := s.backend.PublishRuntimeEnrolled(ctx, claim, proof, enrollment)
	result.Stage = published.Reference
	if err != nil {
		return result, err
	}
	if published.Outcome != state.OutcomeCommitted {
		return result, state.ErrOutcomeUnknown
	}
	return s.completed(ctx, w, result)
}

func decodeColdCapsule(wire []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(wire))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	canonical, err := json.Marshal(value)
	if err != nil || !bytes.Equal(canonical, wire) {
		return ErrPlainColdProfile
	}
	return nil
}

func (s *PlainColdService) pod(id, intent string, generation int64) kube.EnrolledPlainPodRequest {
	spec := s.spec
	spec.ID = id
	return kube.EnrolledPlainPodRequest{Spec: spec, NodeName: s.nodeName, NodeUID: s.config.Enrollment.NodeUID, NodeBootID: s.config.Enrollment.NodeBootID, CreationIntentID: intent, Generation: generation, Config: bytes.Clone(s.configWire)}
}

func (s *PlainColdService) target(pod kube.EnrolledPlainPodRequest, uid string) kube.EnrolledBridgeTarget {
	return kube.EnrolledBridgeTarget{PodName: pod.Spec.ID, PodUID: uid, NodeName: s.nodeName, NodeUID: s.config.Enrollment.NodeUID, NodeBootID: s.config.Enrollment.NodeBootID, LogicalID: pod.Spec.ID, CreationIntentID: pod.CreationIntentID, Generation: pod.Generation, Authority: s.config.TrustBinding(), ContractDigest: s.config.ContractDigest, UID: s.config.UID, GID: s.config.GID, NetworkAllowed: s.config.NetworkAllowed}
}

func (s *PlainColdService) completed(ctx context.Context, w state.WorkspaceIdentity, result PlainColdResult) (PlainColdResult, error) {
	settled, err := s.backend.SettleColdPreparation(ctx, w, result.IntentID)
	if settled.Reference != (state.StageReference{}) {
		result.Stage = settled.Reference
	}
	if err != nil {
		return result, err
	}
	entry := settled.Entry
	if settled.Outcome != state.OutcomeCommitted {
		return result, state.ErrOutcomeUnknown
	}
	if entry == nil {
		return result, state.ErrConflict
	}
	result.State, result.Publication = PlainColdHistorical, entry
	result.Runtime = p.RuntimeReference(entry.Record.Runtime)
	snapshot, err := s.backend.LoadSnapshot(ctx, w.Partition(), entry.Record.SandboxID, entry.Record.Snapshot)
	if err != nil {
		return result, err
	}
	if snapshot == nil {
		return result, state.ErrCorruptRecord
	}
	var capsule plainColdCapsule
	if err := decodeColdCapsule(snapshot.Payload, &capsule); err != nil {
		return result, err
	}
	if capsule.Version != 1 || capsule.Deployment != s.profile || capsule.WorkspaceHash != w.Hash() || capsule.Partition != w.Partition() || capsule.IntentID != result.IntentID || capsule.SandboxID != entry.Record.SandboxID || result.RequestID != "" && result.RequestID != capsule.RequestID {
		result.State = PlainColdRecoveryRequired
		return result, ErrPlainColdProfile
	}
	result.RequestID = capsule.RequestID
	// Monotonic completion can return a prep slot after Destroy. It cannot
	// resurrect the released owner or represent current runtime health.
	view, err := s.backend.ReadWorkspaceView(ctx, w)
	if err != nil {
		return result, err
	}
	if view == nil {
		return result, nil
	}
	current := view.Records()
	if current.Owner.IntentID != result.IntentID || current.Owner.Runtime == nil || *current.Owner.Runtime != entry.Record.Runtime || current.Control.Phase != state.PhaseActive {
		return result, nil
	}
	now, err := s.now(ctx)
	if err != nil {
		return result, err
	}
	if !now.Before(current.Control.ExpiresAt) {
		return result, nil
	}
	if _, err := s.backend.LoadRuntimePublicationEnrolled(ctx, w, result.IntentID); err != nil {
		return result, err
	}
	if err := s.backend.ConfirmSandboxView(ctx, view); err != nil {
		return result, err
	}
	result.State = PlainColdCompleted
	return result, nil
}

func (s *PlainColdService) material(ctx context.Context, birth ct.Birth, d state.RuntimeDispatchRecord, pod, uid string) (state.ColdBootstrapMaterial, error) {
	m := state.ColdBootstrapMaterial{Birth: birth, IssuerCertificate: bytes.Clone(s.issuerWire)}
	now, err := s.now(ctx)
	if err != nil {
		return m, err
	}
	issuer, err := s.verifier.VerifyCommandIssuerCertificate(s.issuerWire, now)
	if err != nil {
		return m, err
	}
	nb, na := now.Add(-30*time.Second), now.Add(15*time.Minute)
	if issuer.NotBefore().After(nb) {
		nb = issuer.NotBefore()
	}
	if issuer.NotAfter().Before(na) {
		na = issuer.NotAfter()
	}
	if d.ExpiresAt.Before(na) {
		na = d.ExpiresAt
	}
	if !now.Add(2 * time.Second).Before(na) {
		return m, ErrPlainColdProfile
	}
	binding := s.config.TrustBinding()
	runtime := p.RuntimeReference{ID: pod, UID: uid, BootID: birth.BootID}
	identity := p.RuntimeIdentityContext{SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, Generation: d.Generation, Runtime: runtime}
	m.RuntimeCertificate, err = p.SignRuntimeIdentityCertificate(s.root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: birth.RuntimePublicKey, NotBefore: nb, NotAfter: na, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, Generation: d.Generation, Runtime: runtime})
	if err != nil {
		return m, err
	}
	m.PublicationCertificate, err = p.SignRuntimeCertificate(s.root, p.RuntimeCertificateClaims{Version: 1, Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IntentID: d.IntentID, SandboxID: d.SandboxID, WorkspaceHash: d.WorkspaceHash, Generation: d.Generation, OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, Snapshot: p.SnapshotReference(d.Snapshot), ExpiresAt: d.ExpiresAt, Runtime: runtime, WorkspaceMode: "plain", PublicKey: birth.RuntimePublicKey, NotBefore: nb, NotAfter: na})
	if err != nil {
		return m, err
	}
	m.Activation, err = p.SignTargetActivation(s.root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: identity, DataGateEpoch: d.DataGateEpoch, UID: birth.UID, GID: birth.GID, NetworkAllowed: birth.NetworkAllowed, ContractDigest: birth.ContractDigest, RuntimeCertificateDigest: coldHash(m.RuntimeCertificate), IssuerCertificateDigest: coldHash(m.IssuerCertificate), NotBefore: nb, NotAfter: na})
	return m, err
}
