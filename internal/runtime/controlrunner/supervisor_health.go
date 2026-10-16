package controlrunner

import (
	"context"
	"net"

	target "github.com/goairix/sandbox/internal/runtime/controltarget"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

// queryRuntimeHealth samples the actual activation and journal gate. No process,
// record enumeration or persistent admission state participates in this query.
func (s *Supervisor) queryRuntimeHealth(ctx context.Context, e t.RuntimeHealthEnvelope) (t.RuntimeHealthResponse, error) {
	if nilValue(ctx) || ctx.Err() != nil || !s.mu.TryLock() {
		return t.RuntimeHealthResponse{}, ErrUnavailable
	}
	if s.closed || s.failed || s.closeStarted.Load() || s.failurePending.Load() || s.activation == nil || s.journal == nil {
		s.mu.Unlock()
		return t.RuntimeHealthResponse{}, ErrAdmissionClosed
	}
	a, journal := s.activation, s.journal
	if e.Identity != a.Identity() || e.DataGateEpoch != a.DataGateEpoch() {
		s.mu.Unlock()
		return t.RuntimeHealthResponse{}, ErrInvalidConfiguration
	}
	birth := s.birth
	s.mu.Unlock()
	// Clock I/O must never retain the admission mutex. Recheck its original
	// activation owner after independently verifying the current interval.
	now, err := observeControlled(ctx, s.options.Clock)
	if err != nil {
		return t.RuntimeHealthResponse{}, err
	}
	fresh, err := s.options.Verifier.VerifyTargetActivation(a.Wire(), a.RuntimeCertificate(), a.IssuerCertificate(), birth, now)
	if err != nil {
		return t.RuntimeHealthResponse{}, err
	}
	if ctx.Err() != nil || !s.mu.TryLock() {
		return t.RuntimeHealthResponse{}, ErrUnavailable
	}
	defer s.mu.Unlock()
	if s.closed || s.failed || s.closeStarted.Load() || s.failurePending.Load() || s.activation != a || s.journal != journal || fresh.Digest() != a.Digest() {
		return t.RuntimeHealthResponse{}, ErrAdmissionClosed
	}
	gate, err := journal.ObserveGate(ctx)
	if err != nil {
		return t.RuntimeHealthResponse{}, err
	}
	b, i := a.Binding(), a.Identity()
	identity := target.JournalIdentity{Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, Runtime: i.Runtime}
	if gate.Identity != identity || gate.DataGateEpoch != a.DataGateEpoch() {
		return t.RuntimeHealthResponse{}, ErrInvalidConfiguration
	}
	state := "closed"
	if s.admission && !s.usersClosed.Load() && gate.GateState == "open" {
		state = "open"
	}
	if ctx.Err() != nil || s.failurePending.Load() || s.closeStarted.Load() {
		return t.RuntimeHealthResponse{}, ErrUnavailable
	}
	return t.RuntimeHealthResponse{Version: 1, Purpose: "runtime_health", Identity: e.Identity, DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, GateState: state}, nil
}

// Only the real activated mTLS handler calls this finite response branch.
func (s *Supervisor) dispatchRuntimeHealth(ctx context.Context, conn net.Conn, e t.RuntimeHealthEnvelope) error {
	response, err := s.queryRuntimeHealth(ctx, e)
	if err != nil {
		return err
	}
	if err := t.WriteRuntimeHealthResponse(conn, response); err != nil {
		return err
	}
	if ctx.Err() != nil || s.failurePending.Load() || s.closeStarted.Load() {
		return ErrUnavailable
	}
	half, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		return ErrUnavailable
	}
	return half.CloseWrite()
}
