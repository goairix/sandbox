package controlrunner

import (
	"context"
	"errors"

	"github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controltarget"
)

func (s *Supervisor) Activate(ctx context.Context, e controlprotocol.TargetActivationEvidence) error {
	if err := s.validateReceiver(); err != nil {
		return err
	}
	if nilValue(ctx) {
		return ErrInvalidConfiguration
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.failed {
		return ErrAdmissionClosed
	}
	if err := s.options.Kernel.ValidateCurrent(); err != nil {
		return err
	}
	now, err := observeControlled(ctx, s.options.Clock)
	if err != nil {
		return err
	}
	fresh, err := s.options.Verifier.VerifyTargetActivation(e.Wire(), e.RuntimeCertificate(), e.IssuerCertificate(), s.birth, now)
	if err != nil {
		return err
	}
	if s.activation != nil {
		if fresh.Digest() != s.activation.Digest() || !s.admission {
			return ErrAdmissionClosed
		}
		return s.journal.InstallActivation(ctx, fresh)
	}
	if s.journal != nil {
		return ErrAdmissionClosed
	}
	credential, err := controlprotocol.NewManagementTLSCredential(s.key, fresh.RuntimeCertificate(), "runtime_receipt")
	if err != nil {
		return err
	}
	config, err := controlprotocol.NewManagementTLSConfig(controlprotocol.ManagementTLSOptions{Verifier: s.options.Verifier, Clock: s.options.Clock, Credential: credential, PeerCertificate: fresh.IssuerCertificate(), Server: true})
	if err != nil {
		return err
	}
	i, b := fresh.Identity(), fresh.Binding()
	journal, err := controltarget.CreateClosedJournal(ctx, controltarget.JournalOptions{Birth: &s.birth, Verifier: s.options.Verifier, Clock: s.options.Clock, Directory: s.options.JournalDirectory, Identity: controltarget.JournalIdentity{Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, Runtime: i.Runtime}, DataGateEpoch: fresh.DataGateEpoch(), ManagementUID: 0, MaxBytes: s.options.JournalMaxBytes})
	if err != nil {
		return err
	}
	s.journal = journal
	if err = journal.InstallActivation(ctx, fresh); err != nil {
		s.failed = true
		return errors.Join(err, journal.Close())
	}
	s.activation = &fresh
	s.credential = credential
	s.tlsConfig = config
	s.admission = true
	return nil
}
func (s *Supervisor) CloseAdmission(ctx context.Context) error {
	if err := s.validateReceiver(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admission = false
	if s.journal == nil {
		return nil
	}
	return closeJournalGate(ctx, s)
}
func closeJournalGate(ctx context.Context, s *Supervisor) error {
	if s.activation == nil {
		return nil
	}
	_, err := s.journal.CloseGate(ctx, s.activation.DataGateEpoch())
	return err
}
