package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	j "github.com/goairix/sandbox/internal/runtime/controltarget"
	t "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// This owns a real root-authenticated live journal and credentials for the
// private installation component. It deliberately supplies no KernelBoundary,
// process effect, native PID1 assertion or exported Supervisor capability.
func runtimeRenewalFixture(tst *testing.T, rotate bool) (*Supervisor, p.TargetActivationEvidence, *fixedClock, func(time.Time) p.TargetActivationEvidence) {
	tst.Helper()
	root, rootKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(tst, err)
	issuerPublic, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(tst, err)
	runtimePublic, runtimeKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(tst, err)
	binding := p.TrustBinding{Namespace: "/sandbox/renewal/component/", AuthorityID: "test", Target: "test", RestoreEpoch: "epoch"}
	verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root})
	require.NoError(tst, err)
	now := time.Now().UTC()
	clock := &fixedClock{observation: p.ClockObservation{UTC: now}}
	identity := p.RuntimeIdentityContext{SandboxID: "sandbox", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: p.RuntimeReference{ID: "runtime", UID: "uid", BootID: uuid.NewString()}}
	birth := p.BirthContext{BootID: identity.Runtime.BootID, RuntimePublicKey: runtimePublic, UID: 1000, GID: 1000, ContractDigest: strings.Repeat("e", 64)}
	issuer, err := p.SignCommandIssuerCertificate(rootKey, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerPublic, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * time.Minute)})
	require.NoError(tst, err)
	runtimeCertificate := func(after time.Time) []byte {
		wire, err := p.SignRuntimeIdentityCertificate(rootKey, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: runtimePublic, NotBefore: now.Add(-time.Minute), NotAfter: after, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, Runtime: identity.Runtime})
		require.NoError(tst, err)
		return wire
	}
	activation := func(certificate []byte, before, after time.Time) p.TargetActivationEvidence {
		runtimeDigest, issuerDigest := sha256.Sum256(certificate), sha256.Sum256(issuer)
		wire, err := p.SignTargetActivation(rootKey, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: identity, DataGateEpoch: 1, UID: birth.UID, GID: birth.GID, ContractDigest: birth.ContractDigest, RuntimeCertificateDigest: hex.EncodeToString(runtimeDigest[:]), IssuerCertificateDigest: hex.EncodeToString(issuerDigest[:]), NotBefore: before, NotAfter: after})
		require.NoError(tst, err)
		verified, err := verifier.VerifyTargetActivation(wire, certificate, issuer, birth, now)
		require.NoError(tst, err)
		return verified
	}
	certificate := runtimeCertificate(now.Add(time.Minute))
	old := activation(certificate, now.Add(-30*time.Second), now.Add(10*time.Second))
	if rotate {
		certificate = runtimeCertificate(now.Add(2 * time.Minute))
	}
	next := activation(certificate, now.Add(-20*time.Second), now.Add(30*time.Second))

	parent, err := filepath.EvalSymlinks(tst.TempDir())
	require.NoError(tst, err)
	require.NoError(tst, os.Chmod(parent, 0700))
	journal, err := j.CreateClosedJournal(context.Background(), j.JournalOptions{Directory: filepath.Join(parent, "journal"), Identity: j.JournalIdentity{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, Runtime: identity.Runtime}, DataGateEpoch: 1, ManagementUID: uint32(os.Geteuid()), Birth: &birth, Verifier: verifier, Clock: clock})
	require.NoError(tst, err)
	tst.Cleanup(func() { require.NoError(tst, journal.Close()) })
	require.NoError(tst, journal.InstallActivation(context.Background(), old))
	credential, err := p.NewManagementTLSCredential(runtimeKey, old.RuntimeCertificate(), "runtime_receipt")
	require.NoError(tst, err)
	tst.Cleanup(func() { require.NoError(tst, credential.Close()) })
	config, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: verifier, Clock: clock, Credential: credential, PeerCertificate: issuer, Server: true})
	require.NoError(tst, err)
	s := &Supervisor{activation: &old, birth: birth, key: runtimeKey, journal: journal, credential: credential, tlsConfig: config, admission: true, active: make(map[string]*Execution), options: SupervisorOptions{Verifier: verifier, Clock: clock}}
	tst.Cleanup(func() { require.NoError(tst, s.credential.Close()) })
	return s, next, clock, func(before time.Time) p.TargetActivationEvidence {
		return activation(certificate, before, now.Add(30*time.Second))
	}
}

func runtimeRenewalEnvelope(s *Supervisor, next p.TargetActivationEvidence) t.RuntimeRenewalEnvelope {
	return t.RuntimeRenewalEnvelope{Version: 1, Purpose: t.RuntimeRenewalPurpose, Identity: next.Identity(), DataGateEpoch: next.DataGateEpoch(), Nonce: strings.Repeat("a", 64), PreviousDigest: s.activation.Digest(), CandidateDigest: next.Digest(), Activation: next.Wire(), RuntimeCertificate: next.RuntimeCertificate(), IssuerCertificate: next.IssuerCertificate()}
}

func TestSupervisorRuntimeRenewalInstallationAndClosedQuery(tst *testing.T) {
	s, next, clock, _ := runtimeRenewalFixture(tst, true)
	e := runtimeRenewalEnvelope(s, next)
	old := s.activation
	credential, config := s.credential, s.tlsConfig
	require.NoError(tst, s.installRuntimeRenewal(context.Background(), e))
	require.Equal(tst, next.Digest(), s.activation.Digest())
	require.NotSame(tst, credential, s.credential)
	require.NotSame(tst, config, s.tlsConfig)
	currentCredential := s.credential
	require.NoError(tst, s.installRuntimeRenewal(context.Background(), e))
	require.Same(tst, currentCredential, s.credential, "idempotent install does not create another TLS owner")

	_, err := s.journal.CloseGate(context.Background(), 1)
	require.NoError(tst, err)
	s.admission = false
	s.usersClosed.Store(true)
	clock.observation.UTC = old.NotAfter().Add(2 * time.Second)
	e.Purpose = t.RuntimeRenewalQueryPurpose
	response, err := s.queryRuntimeRenewal(context.Background(), e)
	require.NoError(tst, err)
	require.True(tst, response.Installed)
	require.False(tst, response.Rejected)
	require.Equal(tst, "closed", response.GateState)
	require.Equal(tst, e.Nonce, response.Nonce)
	e.PreviousDigest = strings.Repeat("f", 64)
	response, err = s.queryRuntimeRenewal(context.Background(), e)
	require.Error(tst, err)
	require.Equal(tst, t.RuntimeRenewalResponse{}, response)
}

func TestSupervisorRuntimeRenewalRetainsBorrowedCredential(tst *testing.T) {
	s, next, _, _ := runtimeRenewalFixture(tst, true)
	s.mu.Lock()
	owner := s.borrowTLSLocked()
	s.mu.Unlock()
	released := false
	defer func() {
		if !released {
			s.releaseTLS(owner)
		}
	}()
	options := p.ManagementTLSOptions{Verifier: s.options.Verifier, Clock: s.options.Clock, Credential: s.credential, PeerCertificate: s.activation.IssuerCertificate(), Server: true}
	require.NoError(tst, s.installRuntimeRenewal(context.Background(), runtimeRenewalEnvelope(s, next)))
	_, err := p.NewManagementTLSConfig(options)
	require.NoError(tst, err, "original borrowed handshake key must remain usable")
	s.releaseTLS(owner)
	released = true
	_, err = p.NewManagementTLSConfig(options)
	require.Error(tst, err, "retired key must close after its last original borrower")
}

func TestSupervisorRuntimeRenewalRejectsBeforeJournalEffect(tst *testing.T) {
	for _, mode := range []string{"active-cert", "active-window", "closed", "wrong-digest"} {
		tst.Run(mode, func(tst *testing.T) {
			s, next, _, _ := runtimeRenewalFixture(tst, mode != "active-window")
			e := runtimeRenewalEnvelope(s, next)
			switch mode {
			case "active-cert":
				// Refusal-only model: any outstanding physical registry owner
				// prevents changing its original certificate, even before record IO.
				s.active["unjoined"] = &Execution{}
			case "active-window":
				s.active["unjoined"] = &Execution{record: j.ExecJournalRecord{NotBefore: next.NotBefore().Add(-time.Second), AuthorityDeadline: next.NotAfter().Add(-time.Second)}}
			case "closed":
				_, err := s.journal.CloseGate(context.Background(), 1)
				require.NoError(tst, err)
				s.admission = false
			case "wrong-digest":
				e.CandidateDigest = strings.Repeat("f", 64)
			}
			old := s.activation
			err := s.installRuntimeRenewal(context.Background(), e)
			var refusal *runtimeRenewalRefusal
			require.Error(tst, err)
			if mode == "wrong-digest" {
				require.False(tst, errors.As(err, &refusal), "an unauthenticated candidate cannot establish a permanent refusal")
			} else {
				require.True(tst, errors.As(err, &refusal), "only a verified, permanently fenced candidate may be explicitly refused")
				response, refusalErr := s.refuseRuntimeRenewal(context.Background(), e, refusal)
				require.NoError(tst, refusalErr)
				require.True(tst, response.Rejected)
				require.False(tst, response.Installed)
			}
			require.Same(tst, old, s.activation)
			installed, queryErr := s.journal.QueryActivationRenewal(context.Background(), e.PreviousDigest, e.CandidateDigest)
			require.Error(tst, queryErr)
			require.False(tst, installed, "a refusal must not create installation evidence")
		})
	}
}

// Only the first external observation blocks. Other requests can make progress
// in the old implementation, exposing a refusal while that first owner can
// still install. Context cancellation and cleanup always join the original call.
type renewalBarrierClock struct {
	base    p.AuthorityClock
	entered chan struct{}
	release chan struct{}
	first   atomic.Bool
}

func (c *renewalBarrierClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	if c.first.CompareAndSwap(false, true) {
		close(c.entered)
		select {
		case <-c.release:
		case <-ctx.Done():
			return p.ClockObservation{}, ctx.Err()
		}
	}
	return c.base.Observe(ctx)
}

func TestSupervisorRuntimeRenewalConcurrentRefusalCannotRaceInstall(tst *testing.T) {
	s, next, clock, _ := runtimeRenewalFixture(tst, true)
	e := runtimeRenewalEnvelope(s, next)
	barrier := &renewalBarrierClock{base: clock, entered: make(chan struct{}), release: make(chan struct{})}
	s.options.Clock = barrier
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var firstErr error
	var release sync.Once
	go func() {
		defer close(done)
		firstErr = s.installRuntimeRenewal(ctx, e)
	}()
	tst.Cleanup(func() {
		release.Do(func() { close(barrier.release) })
		cancel()
		<-done
	})
	select {
	case <-barrier.entered:
	case <-done:
		tst.Fatalf("first install exited before the external clock barrier: %v", firstErr)
	}

	// Neither a canceled request nor a live concurrent request may claim that
	// this exact pair was rejected while the first owner can still install it.
	canceled, stop := context.WithCancel(context.Background())
	stop()
	for _, other := range []context.Context{canceled, context.Background()} {
		err := s.installRuntimeRenewal(other, e)
		var refusal *runtimeRenewalRefusal
		require.Error(tst, err)
		require.False(tst, errors.As(err, &refusal))
	}
	release.Do(func() { close(barrier.release) })
	<-done
	require.NoError(tst, firstErr)
	installed, err := s.journal.QueryActivationRenewal(context.Background(), e.PreviousDigest, e.CandidateDigest)
	require.NoError(tst, err)
	require.True(tst, installed, "the original owner really installed after the concurrent calls returned Unknown")
}

func TestSupervisorRuntimeRenewalRefusalFencesOlderCandidates(tst *testing.T) {
	s, next, _, candidate := runtimeRenewalFixture(tst, true)
	e := runtimeRenewalEnvelope(s, next)
	old := s.activation
	// This registry entry is a refusal-only busy model. Removing it below
	// models the end of that condition, not a physical execution assertion.
	s.active["unjoined"] = &Execution{}
	err := s.installRuntimeRenewal(context.Background(), e)
	var refused *runtimeRenewalRefusal
	require.True(tst, errors.As(err, &refused))
	response, err := s.refuseRuntimeRenewal(context.Background(), e, refused)
	require.NoError(tst, err)
	require.True(tst, response.Rejected)

	newer := candidate(next.NotBefore().Add(time.Second))
	newerEnvelope := runtimeRenewalEnvelope(s, newer)
	err = s.installRuntimeRenewal(context.Background(), newerEnvelope)
	var newerRefusal *runtimeRenewalRefusal
	require.True(tst, errors.As(err, &newerRefusal))
	delete(s.active, "unjoined")
	for _, blocked := range []t.RuntimeRenewalEnvelope{e, newerEnvelope, e} {
		err = s.installRuntimeRenewal(context.Background(), blocked)
		var refusal *runtimeRenewalRefusal
		require.True(tst, errors.As(err, &refusal))
		require.Same(tst, old, s.activation, "clearing busy or refusing another pair cannot revive a refused candidate")
	}
	// The first marker remains valid after a later busy result; there is no
	// last-pair slot whose replacement could erase the earlier no-install fact.
	response, err = s.refuseRuntimeRenewal(context.Background(), e, refused)
	require.NoError(tst, err)
	require.True(tst, response.Rejected)
	fresh := candidate(newer.NotBefore().Add(time.Second))
	require.NoError(tst, s.installRuntimeRenewal(context.Background(), runtimeRenewalEnvelope(s, fresh)))
	require.Equal(tst, fresh.Digest(), s.activation.Digest())
}
