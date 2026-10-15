package controlrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controltarget"
	"github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
)

type NodeEnrollmentOptions struct {
	SocketPath string
	NodeUID    string
	NodeBootID string
}

type supervisorEnrollmentAttempt struct {
	digest      string
	context     context.Context
	cancel      context.CancelFunc
	lifeContext context.Context
	lifeCancel  context.CancelFunc
	done        chan struct{}
	conn        *net.UnixConn // protected by Supervisor.mu
	err         error         // published by done
	evidence    p.EnrolledRuntimeEvidence
	signers     sync.WaitGroup
	signing     uint32 // protected by Supervisor.mu
}

func canonicalEnrollmentUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u != uuid.Nil && u.String() == s
}

func NewEnrolledSupervisor(o SupervisorOptions, enrollment NodeEnrollmentOptions) (*Supervisor, error) {
	if !filepath.IsAbs(enrollment.SocketPath) || filepath.Clean(enrollment.SocketPath) != enrollment.SocketPath || len(enrollment.SocketPath) > 103 || !canonicalEnrollmentUUID(enrollment.NodeUID) || !canonicalEnrollmentUUID(enrollment.NodeBootID) {
		return nil, ErrInvalidConfiguration
	}
	info, err := os.Lstat(enrollment.SocketPath)
	if err != nil {
		return nil, errors.Join(ErrInvalidConfiguration, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return nil, ErrInvalidConfiguration
	}
	s, err := NewSupervisor(o)
	if err != nil {
		return nil, err
	}
	owned := enrollment
	s.enrollmentOptions = &owned
	return s, nil
}

func (s *Supervisor) enrollmentOpenLocked() bool {
	return !s.closed && !s.failed && !s.failurePending.Load() && !s.closeStarted.Load() && !s.enrollmentSealed
}

func (s *Supervisor) reserveEnrollment(ctx context.Context, digest string) (*supervisorEnrollmentAttempt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enrollmentOpenLocked() {
		return nil, false, ErrAdmissionClosed
	}
	if s.enrollment != nil {
		if s.enrollment.digest != digest {
			return nil, false, ErrAdmissionClosed
		}
		return s.enrollment, false, nil
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	life, lifeCancel := context.WithCancel(context.Background())
	a := &supervisorEnrollmentAttempt{digest: digest, context: bounded, cancel: cancel, lifeContext: life, lifeCancel: lifeCancel, done: make(chan struct{})}
	s.enrollment = a
	s.admission = false
	return a, true, nil
}

func (s *Supervisor) activateEnrolled(ctx context.Context, input p.TargetActivationEvidence) error {
	a, original, err := s.reserveEnrollment(ctx, input.Digest())
	if err != nil {
		return err
	}
	if !original {
		select {
		case <-a.done:
		case <-ctx.Done():
			return ctx.Err()
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if a.err != nil {
			return a.err
		}
		if !s.enrollmentOpenLocked() || !s.admission || s.activation == nil || s.activation.Digest() != a.digest {
			return ErrAdmissionClosed
		}
		return nil
	}
	err = s.runEnrollment(a, input)
	s.mu.Lock()
	a.err = err
	if err != nil {
		s.failed = true
		s.admission = false
	}
	s.mu.Unlock()
	a.cancel()
	close(a.done)
	return err
}

func (s *Supervisor) runEnrollment(a *supervisorEnrollmentAttempt, input p.TargetActivationEvidence) (err error) {
	ctx := a.context
	if err := s.options.Kernel.ValidateCurrent(); err != nil {
		return err
	}
	now, err := observeControlled(ctx, s.options.Clock)
	if err != nil {
		return err
	}
	fresh, err := s.options.Verifier.VerifyTargetActivation(input.Wire(), input.RuntimeCertificate(), input.IssuerCertificate(), s.birth, now)
	if err != nil || fresh.Digest() != a.digest {
		return errors.Join(ErrAdmissionClosed, err)
	}
	request := p.NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", RuntimeCertificate: fresh.RuntimeCertificate(), Activation: fresh.Wire(), IssuerCertificate: fresh.IssuerCertificate()}
	var dialer net.Dialer
	connection, err := dialer.DialContext(ctx, "unix", s.enrollmentOptions.SocketPath)
	if err != nil {
		return err
	}
	conn, ok := connection.(*net.UnixConn)
	if !ok {
		_ = connection.Close()
		return ErrUnavailable
	}
	defer conn.Close()
	s.mu.Lock()
	allowed := s.enrollment == a && s.enrollmentOpenLocked()
	if allowed {
		a.conn = conn
	}
	s.mu.Unlock()
	if !allowed {
		return ErrAdmissionClosed
	}
	session, challenge, err := controltransport.OpenNodeEnrollment(ctx, conn, request)
	if err != nil {
		return err
	}
	defer session.Close()
	now, err = observeControlled(ctx, s.options.Clock)
	if err != nil {
		return err
	}
	observer, err := s.options.Verifier.VerifyNodeObserverCertificate(challenge.ObserverCertificate, s.enrollmentOptions.NodeUID, s.enrollmentOptions.NodeBootID, now)
	if err != nil || challenge.Runtime != fresh.Identity().Runtime || challenge.NodeUID != observer.NodeUID() || challenge.NodeBootID != observer.NodeBootID() {
		return errors.Join(ErrAdmissionClosed, err)
	}
	signingBytes, err := p.NodeEnrollmentChallengeSigningBytes(challenge)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.enrollment != a || !s.enrollmentOpenLocked() {
		s.mu.Unlock()
		return ErrAdmissionClosed
	}
	signature := ed25519.Sign(s.key, signingBytes)
	s.mu.Unlock()
	receipt, err := session.Enroll(ctx, p.NodeEnrollmentResponse{Version: 1, Purpose: "runtime_node_enrollment_response", Signature: signature})
	if err != nil {
		return err
	}
	r := receipt.Claims
	i := fresh.Identity()
	if r.Runtime != i.Runtime || r.SandboxID != i.SandboxID || r.WorkspaceHash != i.WorkspaceHash || r.Generation != i.Generation || r.ActivationDigest != fresh.Digest() || r.NodeUID != observer.NodeUID() || r.NodeBootID != observer.NodeBootID() || r.ObserverCertificateDigest != observer.Digest() {
		return ErrAdmissionClosed
	}
	now, err = observeControlled(ctx, s.options.Clock)
	if err != nil {
		return err
	}
	if _, err := s.options.Verifier.VerifyNodeEnrollmentRequest(request, r.ObservedAt); err != nil {
		return err
	}
	if _, err := s.options.Verifier.VerifyNodeEnrollmentReceipt(receipt.Wire(), observer.Wire(), r, now); err != nil {
		return err
	}
	s.mu.Lock()
	if s.enrollment != a || !s.enrollmentOpenLocked() {
		s.mu.Unlock()
		return ErrAdmissionClosed
	}
	credential, err := p.NewManagementTLSCredential(s.key, fresh.RuntimeCertificate(), "runtime_receipt")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	installed := false
	defer func() {
		if !installed {
			_ = credential.Close()
		}
	}()
	config, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: s.options.Verifier, Clock: s.options.Clock, Credential: credential, PeerCertificate: fresh.IssuerCertificate(), Server: true})
	if err != nil {
		return err
	}
	b := fresh.Binding()
	journal, err := controltarget.CreateClosedJournal(ctx, controltarget.JournalOptions{Birth: &s.birth, Verifier: s.options.Verifier, Clock: s.options.Clock, Directory: s.options.JournalDirectory, Identity: controltarget.JournalIdentity{Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, Runtime: i.Runtime}, DataGateEpoch: fresh.DataGateEpoch(), ManagementUID: 0, MaxBytes: s.options.JournalMaxBytes})
	if err != nil {
		return err
	}
	defer func() {
		if !installed {
			_, _ = journal.CloseGate(context.Background(), fresh.DataGateEpoch())
			_ = journal.Close()
		}
	}()
	if err := journal.InstallActivation(ctx, fresh); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enrollment != a || !s.enrollmentOpenLocked() || ctx.Err() != nil {
		return ErrAdmissionClosed
	}
	a.evidence = p.EnrolledRuntimeEvidence{Request: request, ObserverCertificate: observer.Wire(), Receipt: receipt.Wire()}.Clone()
	s.activation, s.credential, s.tlsConfig, s.journal = &fresh, credential, config, journal
	s.admission = true
	installed = true
	return nil
}

func (s *Supervisor) joinEnrollment(a *supervisorEnrollmentAttempt) {
	if a == nil {
		return
	}
	s.mu.Lock()
	conn := a.conn
	s.mu.Unlock()
	a.lifeCancel()
	a.cancel()
	if conn != nil {
		_ = conn.Close()
	}
	<-a.done
	a.signers.Wait()
}

func (s *Supervisor) Enrollment() (p.EnrolledRuntimeEvidence, error) {
	if err := s.validateReceiver(); err != nil {
		return p.EnrolledRuntimeEvidence{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enrollmentOptions == nil || s.enrollment == nil || s.activation == nil {
		return p.EnrolledRuntimeEvidence{}, ErrUnavailable
	}
	return s.enrollment.evidence.Clone(), nil
}

func (s *Supervisor) EnrolledPublicationProof(ctx context.Context, certificate []byte, claim p.ClaimReference) ([]byte, error) {
	if err := s.validateReceiver(); err != nil {
		return nil, err
	}
	if nilValue(ctx) {
		return nil, ErrInvalidConfiguration
	}
	owned := bytes.Clone(certificate)
	s.mu.Lock()
	a, activation, journal := s.enrollment, s.activation, s.journal
	if s.enrollmentOptions == nil || a == nil || activation == nil || journal == nil || !s.admission || !s.enrollmentOpenLocked() || a.signing >= s.options.MaxActive {
		s.mu.Unlock()
		return nil, ErrAdmissionClosed
	}
	a.signing++
	a.signers.Add(1)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); a.signing--; s.mu.Unlock(); a.signers.Done() }()
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	joined := make(chan struct{})
	stop := context.AfterFunc(a.lifeContext, func() { defer close(joined); cancel() })
	defer func() {
		if !stop() {
			<-joined
		}
	}()
	now, err := observeControlled(bounded, s.options.Clock)
	if err != nil {
		return nil, err
	}
	ce, err := s.options.Verifier.VerifyEnrolledPublicationCertificate(owned, *activation, now)
	if err != nil {
		return nil, err
	}
	r, err := p.DecodeNodeEnrollmentReceipt(a.evidence.Receipt)
	if err != nil {
		return nil, err
	}
	// Parse attribution only from ce's independently authenticated canonical
	// certificate, never from the original caller bytes or observed hint alone.
	var publication struct {
		Claims p.RuntimeCertificateClaims `json:"claims"`
	}
	if json.Unmarshal(ce.Wire(), &publication) != nil || publication.Claims.IntentID != r.Claims.CreationIntentID || now.Before(r.Claims.ObservedAt) {
		return nil, ErrAdmissionClosed
	}
	observer, err := s.options.Verifier.VerifyNodeObserverCertificate(a.evidence.ObserverCertificate, r.Claims.NodeUID, r.Claims.NodeBootID, now)
	if err != nil {
		return nil, err
	}
	runtime, err := s.options.Verifier.VerifyRuntimeIdentityCertificate(activation.RuntimeCertificate(), activation.Identity(), now)
	if err != nil {
		return nil, err
	}
	issuer, err := s.options.Verifier.VerifyCommandIssuerCertificate(activation.IssuerCertificate(), now)
	if err != nil {
		return nil, err
	}
	until := now.Add(5 * time.Second)
	for _, end := range []time.Time{publication.Claims.NotAfter, publication.Claims.ExpiresAt, activation.NotAfter(), observer.NotAfter(), runtime.NotAfter(), issuer.NotAfter()} {
		if end.Before(until) {
			until = end
		}
	}
	status := journal.Status()
	if status.Closed || status.Poisoned || !status.AccountingKnown || status.Gate.GateState != "open" || status.Gate.DataGateEpoch != activation.DataGateEpoch() || bounded.Err() != nil {
		return nil, ErrAdmissionClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enrollment != a || !s.enrollmentOpenLocked() || !s.admission || bounded.Err() != nil {
		return nil, ErrAdmissionClosed
	}
	return p.SignEnrolledReadyReceipt(s.key, ce.Wire(), p.EnrolledReadyReceiptClaims{ReadyReceiptClaims: p.ReadyReceiptClaims{Version: 2, CertificateDigest: ce.Digest(), Claim: claim, DataGateEpoch: activation.DataGateEpoch(), GateState: "open", ObservedAt: now, ValidUntil: until}, EnrollmentDigest: r.Claims.EnrollmentDigest, ObserverCertificateDigest: r.Claims.ObserverCertificateDigest, ActivationDigest: activation.Digest()})
}
