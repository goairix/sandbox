package controlrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
)

// A TLS snapshot is retained only by its actual connection borrowers. Renewal
// retires the old credential but never clears a key still used by an original
// handshake/stream. The existing 16-connection listener bounds retained owners;
// there is no background collector or credential registry. Fields use s.mu.
type supervisorTLSOwner struct {
	credential *p.ManagementTLSCredential
	borrowers  int
	retired    bool
}

// The single in-flight owner is registered before external clock/config IO.
// Concurrent requests cannot turn another owner's possible effect into a
// refusal. All fields and the monotonic rejected signing baseline use s.mu.
type supervisorRenewalAttempt struct {
	previousDigest  string
	candidateDigest string
}

// This marker is minted only after root verification and permanently fencing
// this signing baseline under the original owner lock. A finite authenticated
// refusal must still finish before a caller may use this no-install result.
type runtimeRenewalRefusal struct {
	cause           error
	previousDigest  string
	candidateDigest string
	notBefore       time.Time
}

func (e *runtimeRenewalRefusal) Error() string { return e.cause.Error() }
func (e *runtimeRenewalRefusal) Unwrap() error { return e.cause }

func (s *Supervisor) borrowTLSLocked() *supervisorTLSOwner {
	if s.tlsOwner == nil && s.credential != nil {
		s.tlsOwner = &supervisorTLSOwner{credential: s.credential}
	}
	owner := s.tlsOwner
	if owner != nil {
		owner.borrowers++
	}
	return owner
}

func (s *Supervisor) releaseTLS(owner *supervisorTLSOwner) {
	if owner == nil {
		return
	}
	s.mu.Lock()
	owner.borrowers--
	closeCredential := owner.retired && owner.borrowers == 0
	s.mu.Unlock()
	if closeCredential {
		_ = owner.credential.Close()
	}
}

func (s *Supervisor) renewalContextLocked(e t.RuntimeRenewalEnvelope, install bool) error {
	if s.closed || s.failed || s.closeStarted.Load() || s.failurePending.Load() || s.activation == nil || s.journal == nil {
		return ErrAdmissionClosed
	}
	a := s.activation
	if e.Identity != a.Identity() || e.DataGateEpoch != a.DataGateEpoch() {
		return ErrInvalidConfiguration
	}
	if install && (!s.admission || s.usersClosed.Load() || s.enrollmentSealed) {
		return ErrAdmissionClosed
	}
	return nil
}

// Only the genuine finite management-TLS handler calls this branch. It never
// re-enters bootstrap/enrollment or reopens admission. A response reports only
// one exact durable pair; the original native metadata CAS remains separate.
func (s *Supervisor) dispatchRuntimeRenewal(ctx context.Context, conn net.Conn, e t.RuntimeRenewalEnvelope) error {
	var response t.RuntimeRenewalResponse
	var err error
	if e.Purpose == t.RuntimeRenewalPurpose {
		if installErr := s.installRuntimeRenewal(ctx, e); installErr != nil {
			var refusal *runtimeRenewalRefusal
			if !errors.As(installErr, &refusal) {
				return installErr
			}
			response, err = s.refuseRuntimeRenewal(ctx, e, refusal)
			if err != nil {
				return err
			}
		}
	}
	if !response.Rejected {
		response, err = s.queryRuntimeRenewal(ctx, e)
		if err != nil {
			return err
		}
	}
	if err := t.WriteRuntimeRenewalResponse(conn, response); err != nil {
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

func (s *Supervisor) installRuntimeRenewal(ctx context.Context, e t.RuntimeRenewalEnvelope) error {
	if nilValue(ctx) || ctx.Err() != nil || !s.mu.TryLock() {
		return ErrUnavailable
	}
	if err := s.renewalContextLocked(e, false); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.renewalInFlight != nil || s.renewalUncertain {
		s.mu.Unlock()
		return ErrUnavailable
	}
	old, journal := s.activation, s.journal
	if old.Digest() != e.PreviousDigest && old.Digest() != e.CandidateDigest {
		s.mu.Unlock()
		return ErrInvalidConfiguration
	}
	owner := &supervisorRenewalAttempt{previousDigest: e.PreviousDigest, candidateDigest: e.CandidateDigest}
	s.renewalInFlight = owner
	birth := s.birth
	birth.RuntimePublicKey = append([]byte(nil), birth.RuntimePublicKey...)
	key := append(ed25519.PrivateKey(nil), s.key...)
	s.mu.Unlock()
	defer clear(key)
	defer func() {
		s.mu.Lock()
		if s.renewalInFlight == owner {
			s.renewalInFlight = nil
		}
		s.mu.Unlock()
	}()

	// Supervisor clock and credential construction happen outside its mutex.
	// The final journal call deliberately retains the mutex through the
	// journal's existing bounded fresh-clock/fsync checks: this serializes the
	// durable replacement with Close/Destroy without another reservation owner.
	now, err := observeControlled(ctx, s.options.Clock)
	if err != nil {
		return err
	}
	next, err := s.options.Verifier.VerifyTargetActivation(e.Activation, e.RuntimeCertificate, e.IssuerCertificate, birth, now)
	if err != nil {
		return err
	}
	if next.Digest() != e.CandidateDigest || next.Identity() != e.Identity || next.DataGateEpoch() != e.DataGateEpoch {
		return ErrInvalidConfiguration
	}
	if old.Digest() == e.CandidateDigest {
		// Even a duplicate authenticates its exact payload first, but neither
		// rewrites the journal nor replaces its current credential/config.
		if ctx.Err() != nil || !s.mu.TryLock() {
			return ErrUnavailable
		}
		defer s.mu.Unlock()
		if err := s.renewalContextLocked(e, true); err != nil {
			return err
		}
		if s.renewalInFlight != owner || s.activation != old || s.journal != journal {
			return ErrUnavailable
		}
		_, err := journal.QueryActivationRenewal(ctx, e.PreviousDigest, e.CandidateDigest)
		return err
	}
	credential, err := p.NewManagementTLSCredential(key, next.RuntimeCertificate(), "runtime_receipt")
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = credential.Close()
		}
	}()
	config, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: s.options.Verifier, Clock: s.options.Clock, Credential: credential, PeerCertificate: next.IssuerCertificate(), Server: true})
	if err != nil {
		return err
	}
	if ctx.Err() != nil || !s.mu.TryLock() {
		return ErrUnavailable
	}
	if err := s.renewalContextLocked(e, false); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.renewalInFlight != owner || s.activation != old || s.journal != journal || s.renewalUncertain {
		s.mu.Unlock()
		return ErrUnavailable
	}
	// Only a verified candidate owned by this handler may be refused. The
	// high-water mark never decreases, even after another busy attempt, so an
	// acknowledged refusal cannot later install when its busy condition clears.
	refuse := func(cause error) error {
		if next.NotBefore().After(s.renewalRejectedBefore) {
			s.renewalRejectedBefore = next.NotBefore()
		}
		return &runtimeRenewalRefusal{cause: cause, previousDigest: owner.previousDigest, candidateDigest: owner.candidateDigest, notBefore: next.NotBefore()}
	}
	if !next.NotBefore().After(s.renewalRejectedBefore) || !next.NotBefore().After(old.NotBefore()) || next.ActivationID() == old.ActivationID() {
		err := refuse(ErrInvalidConfiguration)
		s.mu.Unlock()
		return err
	}
	if err := s.renewalContextLocked(e, true); err != nil {
		err = refuse(err)
		s.mu.Unlock()
		return err
	}
	if err := s.renewalExecutionsLocked(*old, next); err != nil {
		err = refuse(err)
		s.mu.Unlock()
		return err
	}
	if err := journal.RenewActivation(ctx, e.PreviousDigest, next); err != nil {
		// Once the journal was called, no later busy/closed request may turn
		// an uncertain effect into Rejected. Exact read-only query remains.
		s.renewalUncertain = true
		if journal.Status().Poisoned {
			s.admission = false
			s.failurePending.Store(true)
		}
		s.mu.Unlock()
		return err
	}

	// The installed journal and current TLS projection change under this one
	// supervisor lock. Existing executions retain their original records,
	// tickets, authority deadlines and already-open transport owners unchanged.
	oldOwner := s.tlsOwner
	if oldOwner == nil && s.credential != nil {
		oldOwner = &supervisorTLSOwner{credential: s.credential}
	}
	s.activation = &next
	s.credential, s.tlsConfig = credential, config
	s.tlsOwner = &supervisorTLSOwner{credential: credential}
	installed = true
	closeOld := oldOwner != nil && oldOwner.borrowers == 0
	if oldOwner != nil {
		oldOwner.retired = true
	}
	s.mu.Unlock()
	if closeOld {
		_ = oldOwner.credential.Close()
	}
	return nil
}

func (s *Supervisor) refuseRuntimeRenewal(ctx context.Context, e t.RuntimeRenewalEnvelope, refusal *runtimeRenewalRefusal) (t.RuntimeRenewalResponse, error) {
	if nilValue(ctx) || ctx.Err() != nil || !s.mu.TryLock() {
		return t.RuntimeRenewalResponse{}, ErrUnavailable
	}
	defer s.mu.Unlock()
	if refusal == nil || refusal.previousDigest != e.PreviousDigest || refusal.candidateDigest != e.CandidateDigest || refusal.notBefore.IsZero() || refusal.notBefore.After(s.renewalRejectedBefore) || s.renewalUncertain {
		return t.RuntimeRenewalResponse{}, ErrUnavailable
	}
	if s.activation == nil || s.activation.Identity() != e.Identity || s.activation.DataGateEpoch() != e.DataGateEpoch || s.activation.Digest() == e.CandidateDigest {
		return t.RuntimeRenewalResponse{}, ErrUnavailable
	}
	gate := "closed"
	if s.admission && !s.usersClosed.Load() && !s.closed && !s.failed && !s.closeStarted.Load() && !s.failurePending.Load() {
		gate = "open"
	}
	return t.RuntimeRenewalResponse{Version: 1, Purpose: "runtime_activation_renewed", Identity: e.Identity, DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, PreviousDigest: e.PreviousDigest, CandidateDigest: e.CandidateDigest, Rejected: true, GateState: gate}, nil
}

// Existing handles pin their original TLS and receipt certificates. Certificate
// replacement is therefore busy until their physical execution/IO owners leave
// the registry. Same-certificate renewal must also preserve their original
// acceptance windows; it never widens a command deadline or rewrites a record.
func (s *Supervisor) renewalExecutionsLocked(old, next p.TargetActivationEvidence) error {
	if len(s.active) == 0 {
		return nil
	}
	if !bytes.Equal(old.RuntimeCertificate(), next.RuntimeCertificate()) || !bytes.Equal(old.IssuerCertificate(), next.IssuerCertificate()) {
		return ErrUnavailable
	}
	for _, e := range s.active {
		if e == nil || !e.mu.TryLock() {
			return ErrUnavailable
		}
		r := e.record
		e.mu.Unlock()
		if next.NotBefore().After(r.NotBefore) || next.NotAfter().Before(r.AuthorityDeadline) {
			return ErrUnavailable
		}
	}
	return nil
}

func (s *Supervisor) queryRuntimeRenewal(ctx context.Context, e t.RuntimeRenewalEnvelope) (t.RuntimeRenewalResponse, error) {
	if nilValue(ctx) || ctx.Err() != nil || !s.mu.TryLock() {
		return t.RuntimeRenewalResponse{}, ErrUnavailable
	}
	defer s.mu.Unlock()
	if err := s.renewalContextLocked(e, false); err != nil {
		return t.RuntimeRenewalResponse{}, err
	}
	installed, err := s.journal.QueryActivationRenewal(ctx, e.PreviousDigest, e.CandidateDigest)
	if err != nil {
		return t.RuntimeRenewalResponse{}, err
	}
	gate, err := s.journal.ObserveGate(ctx)
	if err != nil {
		return t.RuntimeRenewalResponse{}, err
	}
	if ctx.Err() != nil || s.failurePending.Load() || s.closeStarted.Load() {
		return t.RuntimeRenewalResponse{}, ErrUnavailable
	}
	return t.RuntimeRenewalResponse{Version: 1, Purpose: "runtime_activation_renewed", Identity: e.Identity, DataGateEpoch: e.DataGateEpoch, Nonce: e.Nonce, PreviousDigest: e.PreviousDigest, CandidateDigest: e.CandidateDigest, Installed: installed, GateState: gate.GateState}, nil
}
