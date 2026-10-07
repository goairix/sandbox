package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	j "github.com/goairix/sandbox/internal/runtime/controltarget"
	"github.com/google/uuid"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tpt "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/stretchr/testify/require"
)

// This private consumer test uses real persisted passive history, not a forged
// PID1/live activation. Closing admission must not sign unknown/accepted state.
func TestTaskCloseQueryRefusesNonterminalHistory(t *testing.T) {
	journal, id := shutdownHistory(t)
	r, err := journal.Lookup(context.Background(), id)
	require.NoError(t, err)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s := &Supervisor{journal: journal, key: key}
	_, wire, err := s.queryReceipt(context.Background(), tpt.Envelope{Purpose: "exec_query", Context: r.Context, DescriptorDigest: r.DescriptorDigest})
	require.Error(t, err)
	require.Nil(t, wire)
}

// This is a real authenticated, installed journal and valid terminal record.
// Its trusted local-result assertion is a fixture, not physical process proof.
func taskCloseTerminalHistory(t *testing.T) (*Supervisor, tpt.Envelope) {
	t.Helper()
	root, rootKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	issuerPub, issuerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	binding := p.TrustBinding{Namespace: "/sandbox/shutdown/v1/", AuthorityID: "test", Target: "test", RestoreEpoch: "epoch"}
	verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root})
	require.NoError(t, err)
	now := time.Now().UTC()
	cert, err := p.SignCommandIssuerCertificate(rootKey, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerPub, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, err)
	issuer, err := verifier.VerifyCommandIssuerCertificate(cert, now)
	require.NoError(t, err)
	identity := j.JournalIdentity{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, SandboxID: "history", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: p.RuntimeReference{ID: "runtime", UID: "uid", BootID: uuid.NewString()}}
	runtimePub, runtimeKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	birth := p.BirthContext{BootID: identity.Runtime.BootID, RuntimePublicKey: runtimePub, UID: 1000, GID: 1000, ContractDigest: strings.Repeat("e", 64)}
	ri := p.RuntimeIdentityContext{SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: identity.Generation, Runtime: identity.Runtime}
	rw, err := p.SignRuntimeIdentityCertificate(rootKey, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: runtimePub, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute), SandboxID: ri.SandboxID, WorkspaceHash: ri.WorkspaceHash, Generation: ri.Generation, Runtime: ri.Runtime})
	require.NoError(t, err)
	digest := func(w []byte) string { h := sha256.Sum256(w); return hex.EncodeToString(h[:]) }
	aw, err := p.SignTargetActivation(rootKey, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: ri, DataGateEpoch: 1, UID: 1000, GID: 1000, ContractDigest: birth.ContractDigest, RuntimeCertificateDigest: digest(rw), IssuerCertificateDigest: digest(cert), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute)})
	require.NoError(t, err)
	activation, err := verifier.VerifyTargetActivation(aw, rw, cert, birth, now)
	require.NoError(t, err)
	clock := &fixedClock{observation: p.ClockObservation{UTC: now}}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, os.Chmod(directory, 0700))
	journal, err := j.CreateClosedJournal(context.Background(), j.JournalOptions{Directory: filepath.Join(directory, "journal"), Identity: identity, DataGateEpoch: 1, ManagementUID: uint32(os.Geteuid()), Birth: &birth, Verifier: verifier, Clock: clock})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, journal.Close()) })
	descriptor, err := p.NewExecutionDescriptor(p.ExecutionRequest{Argv: []string{"/never-run"}, Env: map[string]string{}, UID: 1000, GID: 1000, WorkDir: "/", TimeoutSeconds: 1})
	require.NoError(t, err)
	c := p.ExecStartContext{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: uuid.NewString(), OperationID: uuid.NewString(), RequestID: "history", OperationDigest: strings.Repeat("b", 64), SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: 1, DataGateEpoch: 1, ControlRevision: 1, AdmissionRevision: 1, LeaseID: 1, Runtime: identity.Runtime, ExpiresAt: now.Add(time.Minute)}
	ticket, err := p.SignExecStartTicket(issuerKey, issuer, p.ExecStartTicketClaims{Version: 1, Purpose: "operation_exec_start", Context: c, DescriptorDigest: descriptor.Digest(), NotBefore: now.Add(-time.Second), NotAfter: now.Add(10 * time.Second)})
	require.NoError(t, err)
	evidence, err := verifier.VerifyExecStartTicket(ticket, cert, c, descriptor, now)
	require.NoError(t, err)
	require.NoError(t, journal.InstallActivation(context.Background(), activation))
	accepted, err := journal.AcceptExecution(context.Background(), evidence)
	require.NoError(t, err)
	require.NoError(t, journal.ConsumeStart(context.Background(), accepted))
	_, err = journal.RecordLocalTerminal(context.Background(), accepted, j.LocalExecutionResult{RootPID: 123, DrainConfirmed: true, Reason: "exited"})
	require.NoError(t, err)
	_, err = journal.CloseGate(context.Background(), 1)
	require.NoError(t, err)
	supervisor := &Supervisor{journal: journal, key: runtimeKey, activation: &activation, options: SupervisorOptions{Verifier: verifier, Clock: clock}}
	return supervisor, tpt.Envelope{Version: 1, Purpose: "exec_query", Context: c, DescriptorDigest: descriptor.Digest()}
}

func TestTaskCloseQueryEligibility(t *testing.T) {
	for _, fault := range []string{"healthy-closed", "pending", "closed", "failed", "no-activation", "context", "cancel", "lookup-pending"} {
		t.Run(fault, func(t *testing.T) {
			s, e := taskCloseTerminalHistory(t)
			require.NoError(t, s.transportExecContext(e.Context, true))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var query context.Context = ctx
			var lookup *taskCloseLookupContext
			switch fault {
			case "pending":
				s.failurePending.Store(true)
			case "closed":
				s.closed = true
			case "failed":
				s.failed = true
			case "no-activation":
				s.activation = nil
			case "context":
				e.Context.DataGateEpoch++
			case "cancel":
				cancel()
			case "lookup-pending":
				lookup = &taskCloseLookupContext{Context: ctx, flip: func() { s.failurePending.Store(true) }}
				query = lookup
			}
			kind, wire, err := s.queryReceipt(query, e)
			if fault == "healthy-closed" {
				require.NoError(t, err)
				require.Equal(t, tpt.EventReceipt, kind)
				r, err := s.journal.Lookup(ctx, e.Context.CommandID)
				require.NoError(t, err)
				proof, err := s.options.Verifier.VerifyLocalExecReceipt(wire, s.activation.RuntimeCertificate(), r.Context, r.DescriptorDigest, r.TicketDigest, r.NotBefore, r.NotAfter, r.AuthorityDeadline, time.Now().UTC())
				require.NoError(t, err)
				require.Equal(t, "local_terminal", proof.State())
			} else {
				require.Error(t, err)
				require.Nil(t, wire)
				require.Zero(t, kind)
			}
			if lookup != nil {
				require.True(t, lookup.hit.Load(), "must enter actual journal lookup boundary")
			}
		})
	}
}

// The third cancellation observation occurs inside the real Lookup: after its
// entry check/read on the old implementation and after entry/check on the fix.
// Flipping failurePending here keeps the original context valid, so Lookup
// still returns the real record and the query's final check must refuse it.
type taskCloseLookupContext struct {
	context.Context
	at    int32
	calls atomic.Int32
	hit   atomic.Bool
	flip  func()
}

func (c *taskCloseLookupContext) Err() error {
	at := c.at
	if at == 0 {
		at = 3
	}
	if c.calls.Add(1) == at {
		c.hit.Store(true)
		c.flip()
	}
	return c.Context.Err()
}
