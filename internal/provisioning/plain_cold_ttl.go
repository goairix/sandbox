package provisioning

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/goairix/sandbox/pkg/types"
	"github.com/google/uuid"
)

// UpdateTTL renews one existing runtime under the original service call and a
// genuine mutation admission. It never changes the immutable creation request.
// An unresolved attempt is queried with its original candidate; this method
// cannot turn a lost acknowledgement into a new signature or a second install.
func (s *PlainColdService) UpdateTTL(ctx context.Context, sandboxID, requestID string, seconds int) (response types.UpdateTTLResponse, err error) {
	if seconds < 1 || seconds > 365*24*60*60 {
		return response, state.ErrInvalidRecord
	}
	return s.renewTTL(ctx, sandboxID, requestID, seconds, false)
}

// RefreshCredentials refreshes one live runtime's finite certification without
// extending its business lifetime or changing its displayed Timeout. It uses
// the same root signer, mutation admission and fixed-candidate transport as
// UpdateTTL. An unresolved user TTL change is not incidental maintenance: only
// an exact preserved business interval may be queried or repaired here.
// This entry point does not schedule automatic credential maintenance.
func (s *PlainColdService) RefreshCredentials(ctx context.Context, sandboxID, requestID string) (types.UpdateTTLResponse, error) {
	return s.renewTTL(ctx, sandboxID, requestID, 0, true)
}

// renewTTL owns one service lifetime across selection, actual authenticated
// transport and Stage return. The preserving branch changes only how the fixed
// business interval is selected; it never grants a separate runtime authority.
func (s *PlainColdService) renewTTL(ctx context.Context, sandboxID, requestID string, seconds int, preserveExpiry bool) (response types.UpdateTTLResponse, err error) {
	call, err := s.enter(ctx)
	if err != nil {
		return response, err
	}
	defer s.leave(call)
	ctx = call.context
	ttlCommitted := false
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		err = errors.Join(err, ctx.Err())
		if s.closed {
			err = errors.Join(err, context.Canceled)
		}
		if err != nil && ttlCommitted {
			// Losing the original response lifetime after a commit cannot turn
			// an installed TTL change into a definite no-effect failure.
			err = errors.Join(state.ErrOutcomeUnknown, err)
		}
		if err != nil {
			response = types.UpdateTTLResponse{}
		}
	}()
	loaded, err := s.loadColdTarget(ctx, sandboxID)
	if err != nil {
		return response, err
	}
	var baseline time.Time
	if preserveExpiry {
		baseline, seconds, err = credentialRefreshParameters(loaded)
		if err != nil {
			return response, err
		}
	}
	if loaded.ttlPending != nil {
		pending := loaded.ttlPending
		if pending.Timeout != seconds {
			return response, state.ErrOutcomeUnknown
		}
		// This fixed candidate's pin covers only the original runtime key and
		// identity. Its broader receipt interval can authenticate either the old
		// leaf or the installed leaf; it never selects trust from peer input.
		destination, credential, err := s.destination(s.target(loaded.dispatch.Pod, loaded.control.Runtime.UID), state.ColdBootstrapMaterial{RuntimeCertificate: pending.Activation.RuntimeCertificate(), IssuerCertificate: pending.Activation.IssuerCertificate()}, pending.Activation.Identity())
		if err != nil {
			return response, errors.Join(state.ErrOutcomeUnknown, err)
		}
		defer credential.Close()
		result, err := s.backend.RenewSandboxTTL(ctx, nil, loaded.view, pending.Baseline, pending.Timeout, pending.Activation, destination)
		ttlCommitted = result.Outcome == state.OutcomeCommitted
		return ttlResponse(result, err)
	}

	now, err := s.now(ctx)
	if err != nil {
		return response, err
	}
	if err := checkPlainColdLookupTTL(now, loaded.control.ExpiresAt); err != nil {
		return response, err
	}
	if !preserveExpiry {
		baseline = now
	}
	admitted, err := s.backend.BeginOperation(ctx, state.BeginOperationInput{SandboxID: sandboxID, RequestID: requestID, Kind: state.OperationMutation})
	if err != nil {
		return response, errors.Join(err, admitted.GuardCleanupError)
	}
	capability := admitted.Capability
	if capability == nil || admitted.Outcome != state.OperationCommitted {
		return response, state.ErrOutcomeUnknown
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cleanupErr := s.backend.CancelOperationCapability(cleanup, capability)
		cancel()
		err = errors.Join(err, cleanupErr)
		if err != nil {
			response = types.UpdateTTLResponse{}
		}
	}()
	if !reflect.DeepEqual(capability.Control(), *loaded.control) {
		return response, state.ErrConflict
	}

	current, err := s.currentTTLActivation(loaded, now)
	if err != nil {
		return response, err
	}
	next, err := s.ttlActivation(now, baseline.Add(time.Duration(seconds)*time.Second), current)
	if err != nil {
		return response, err
	}
	// The first install authenticates the original current TLS peer. Only the
	// Backend's successful fixed-attempt exchange may commit the new metadata.
	destination, credential, err := s.destination(s.target(loaded.dispatch.Pod, loaded.control.Runtime.UID), state.ColdBootstrapMaterial{RuntimeCertificate: current.RuntimeCertificate(), IssuerCertificate: current.IssuerCertificate()}, current.Identity())
	if err != nil {
		return response, err
	}
	defer credential.Close()
	result, err := s.backend.RenewSandboxTTL(ctx, capability, loaded.view, baseline, seconds, next, destination)
	ttlCommitted = result.Outcome == state.OutcomeCommitted
	return ttlResponse(result, err)
}

// credentialRefreshParameters selects only from already authenticated loader
// output. It grants no operation or install permission. Rejecting an unrelated
// pending interval here precedes the clock, admission, signing and transport;
// maintenance must not finish a caller's requested business TTL change.
func credentialRefreshParameters(loaded *plainColdLoadedTarget) (time.Time, int, error) {
	if loaded == nil || loaded.control == nil || loaded.control.Phase != state.PhaseActive || loaded.control.Runtime == nil {
		return time.Time{}, 0, ErrPlainColdUnavailable
	}
	seconds := 0
	if current := loaded.ttlCurrent; current != nil {
		seconds = current.Timeout
	} else {
		// Legacy capsules retain a duration, unlike the integer TTL API. A
		// fractional second cannot be silently rounded into a new lifetime.
		if loaded.capsule.TTL%time.Second != 0 {
			return time.Time{}, 0, state.ErrInvalidRecord
		}
		seconds = int(loaded.capsule.TTL / time.Second)
	}
	if seconds < 1 || seconds > 365*24*60*60 {
		return time.Time{}, 0, state.ErrInvalidRecord
	}
	expires := loaded.control.ExpiresAt
	baseline := expires.Add(-time.Duration(seconds) * time.Second)
	if current := loaded.ttlCurrent; current != nil && (!current.ExpiresAt.Equal(expires) || !current.Baseline.Equal(baseline)) {
		return time.Time{}, 0, state.ErrConflict
	}
	if pending := loaded.ttlPending; pending != nil && (pending.Timeout != seconds || !pending.ExpiresAt.Equal(expires) || !pending.Baseline.Equal(baseline)) {
		return time.Time{}, 0, state.ErrOutcomeUnknown
	}
	return baseline, seconds, nil
}

func ttlResponse(result state.SandboxTTLResult, err error) (types.UpdateTTLResponse, error) {
	err = errors.Join(err, result.GuardCleanupError)
	if err != nil {
		return types.UpdateTTLResponse{}, err
	}
	if result.Outcome != state.OutcomeCommitted {
		return types.UpdateTTLResponse{}, state.ErrOutcomeUnknown
	}
	return types.UpdateTTLResponse{Timeout: result.Timeout, ExpiresAt: result.ExpiresAt}, nil
}

// currentTTLActivation rechecks live certification after the caller has pinned
// the original metadata view. Historical publication remains immutable.
func (s *PlainColdService) currentTTLActivation(loaded *plainColdLoadedTarget, now time.Time) (p.TargetActivationEvidence, error) {
	r := loaded.history.Publication.Enrollment.Request
	if loaded.ttlCurrent != nil {
		a := loaded.ttlCurrent.Activation
		r.Activation, r.RuntimeCertificate, r.IssuerCertificate = a.Wire(), a.RuntimeCertificate(), a.IssuerCertificate()
	}
	a, err := s.verifier.VerifyNodeEnrollmentRequest(r, now)
	if err != nil {
		return p.TargetActivationEvidence{}, err
	}
	c := loaded.control
	if a.Identity() != (p.RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: p.RuntimeReference(*c.Runtime)}) || a.DataGateEpoch() != c.DataGateEpoch || a.Birth().UID != s.config.UID || a.Birth().GID != s.config.GID || a.Birth().ContractDigest != s.config.ContractDigest || !bytes.Equal(a.IssuerCertificate(), s.issuerWire) {
		return p.TargetActivationEvidence{}, ErrPlainColdProfile
	}
	return a, nil
}

// ttlActivation signs once from the actual trusted observation, independently
// of the preserved business baseline. Receipt identity is refreshed only when
// its current interval cannot cover the next finite window;
// keeping its original NotBefore also pins a lost-ACK query to both TLS states.
// Business authority stays within 15 minutes, the issuer, and the requested TTL.
func (s *PlainColdService) ttlActivation(now, expires time.Time, current p.TargetActivationEvidence) (p.TargetActivationEvidence, error) {
	identity, birth := current.Identity(), current.Birth()
	if _, err := s.verifier.VerifyTargetActivation(current.Wire(), current.RuntimeCertificate(), current.IssuerCertificate(), birth, now); err != nil {
		return p.TargetActivationEvidence{}, err
	}
	issuer, err := s.verifier.VerifyCommandIssuerCertificate(s.issuerWire, now)
	if err != nil {
		return p.TargetActivationEvidence{}, err
	}
	runtime, err := s.verifier.VerifyRuntimeIdentityCertificate(current.RuntimeCertificate(), identity, now)
	if err != nil {
		return p.TargetActivationEvidence{}, err
	}
	identityAfter := now.Add(15 * time.Minute)
	if issuer.NotAfter().Before(identityAfter) {
		identityAfter = issuer.NotAfter()
	}
	after := expires
	if identityAfter.Before(after) {
		after = identityAfter
	}
	if !now.Add(2 * time.Second).Before(after) {
		return p.TargetActivationEvidence{}, state.ErrOperationAdmissionClosed
	}
	runtimeWire := current.RuntimeCertificate()
	if runtime.NotAfter().Before(after) {
		b := current.Binding()
		runtimeWire, err = p.SignRuntimeIdentityCertificate(s.root, p.RuntimeIdentityCertificateClaims{Version: 1, Role: "runtime_receipt", CertificateID: uuid.NewString(), Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, PublicKey: runtime.PublicKey(), NotBefore: runtime.NotBefore(), NotAfter: identityAfter, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, Runtime: identity.Runtime})
		if err != nil {
			return p.TargetActivationEvidence{}, err
		}
	}
	before := now.Add(-time.Second)
	if !before.After(current.NotBefore()) {
		before = current.NotBefore().Add(time.Nanosecond)
	}
	if issuer.NotBefore().After(before) {
		before = issuer.NotBefore()
	}
	wire, err := p.SignTargetActivation(s.root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: current.Binding(), Identity: identity, DataGateEpoch: current.DataGateEpoch(), UID: birth.UID, GID: birth.GID, NetworkAllowed: birth.NetworkAllowed, ContractDigest: birth.ContractDigest, RuntimeCertificateDigest: coldHash(runtimeWire), IssuerCertificateDigest: coldHash(s.issuerWire), NotBefore: before, NotAfter: after})
	if err != nil {
		return p.TargetActivationEvidence{}, err
	}
	return s.verifier.VerifyTargetActivation(wire, runtimeWire, s.issuerWire, birth, now)
}
