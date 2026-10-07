//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Test-only clock enters the original Journal mutex through its configured
// AuthorityClock contract. No production hook or blocked-kernel-IO claim.
type quiesceInterleaveClock struct {
	mu   sync.Mutex
	hook func(context.Context) (p.ClockObservation, error)
}

func (c *quiesceInterleaveClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	c.mu.Lock()
	h := c.hook
	c.mu.Unlock()
	if h != nil {
		return h(ctx)
	}
	return quiesceObservation(ctx)
}
func (c *quiesceInterleaveClock) set(h func(context.Context) (p.ClockObservation, error)) {
	c.mu.Lock()
	c.hook = h
	c.mu.Unlock()
}
func quiesceObservation(ctx context.Context) (p.ClockObservation, error) {
	return p.ClockObservation{UTC: time.Now().UTC(), Uncertainty: time.Millisecond}, ctx.Err()
}
func newQuiesceClockFixture(t *testing.T, clock *quiesceInterleaveClock) nativeFixture {
	t.Helper()
	kernel := requireNative(t)
	a := nativeAttestation(t)
	pub, root, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	issuerPub, issuer, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	binding := p.TrustBinding{Namespace: "/sandbox/native/v1/", AuthorityID: "fixture", Target: "target", RestoreEpoch: "restore"}
	verifier, e := p.NewManagementVerifier(binding, []ed25519.PublicKey{pub})
	if e != nil {
		t.Fatal(e)
	}
	if e := os.MkdirAll("/journal/protected", 0700); e != nil {
		t.Fatal(e)
	}
	s, e := NewSupervisor(SupervisorOptions{Kernel: kernel, UID: 1000, GID: 1000, ContractDigest: a.ContractDigest, Verifier: verifier, Clock: clock, JournalDirectory: "/journal/protected/" + uuid.NewString(), Executable: os.Getenv("SANDBOX_TASK3_MONITOR")})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	})
	now := time.Now().UTC()
	nb, na := now.Add(-time.Minute), now.Add(10*time.Minute)
	iw, e := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerPub, NotBefore: nb, NotAfter: na})
	if e != nil {
		t.Fatal(e)
	}
	birth := s.Birth()
	identity := p.RuntimeIdentityContext{SandboxID: "native", WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: p.RuntimeReference{ID: a.RuntimeID, UID: a.RuntimeUID, BootID: birth.BootID}}
	rw, e := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: birth.RuntimePublicKey, NotBefore: nb, NotAfter: na, SandboxID: identity.SandboxID, WorkspaceHash: identity.WorkspaceHash, Generation: 1, Runtime: identity.Runtime})
	if e != nil {
		t.Fatal(e)
	}
	aw, e := p.SignTargetActivation(root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: identity, DataGateEpoch: 1, UID: 1000, GID: 1000, ContractDigest: a.ContractDigest, RuntimeCertificateDigest: hashBytes(rw), IssuerCertificateDigest: hashBytes(iw), NotBefore: nb, NotAfter: na})
	if e != nil {
		t.Fatal(e)
	}
	activation, e := verifier.VerifyTargetActivation(aw, rw, iw, birth, now)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Activate(context.Background(), activation); e != nil {
		t.Fatal(e)
	}
	issuerIdentity, e := verifier.VerifyCommandIssuerCertificate(iw, now)
	if e != nil {
		t.Fatal(e)
	}
	return nativeFixture{s: s, issuer: issuer, issuerWire: iw, issuerIdentity: issuerIdentity}
}

func waitQuiesceTest(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("controlled interleaving not reached")
	}
}
func quiesceTerminalBytes(t *testing.T, f nativeFixture) bool {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.s.options.JournalDirectory, "users-quiesce.json"))
	return err == nil && strings.Contains(string(b), `"state":"users_quiesced"`)
}
func expectQuiesceIsolation() {
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	<-timer.C
	// Do not run cleanup that could hide a missed deadline by eventually exiting70.
	fmt.Fprintln(os.Stderr, "QUIESCE_INTERLEAVING_DEADLINE_MISSED")
	os.Exit(71)
}
func TestTaskQuiesceNativeEarlierIsolation(t *testing.T) {
	f := newNativeFixture(t)
	e, _ := f.start(t, p.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "hold"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 25}, 25*time.Second)
	waitExecutionReady(t, e)
	e.mu.Lock() // Real cooperative isolate skips this owner then waits ownerDone.
	go f.s.isolate("controlled_earlier_deadline")
	deadline := time.Now().Add(time.Second)
	var original *isolationDeadline
	for time.Now().Before(deadline) {
		f.s.deadlineMu.Lock()
		original = f.s.isolationDeadline
		f.s.deadlineMu.Unlock()
		if original != nil {
			stack := make([]byte, 65536)
			n := runtime.Stack(stack, true)
			if n < len(stack) && strings.Contains(string(stack[:n]), "(*Supervisor).isolate.func1(") {
				break
			}
			original = nil
		}
		time.Sleep(time.Millisecond)
	}
	if original == nil {
		e.mu.Unlock()
		t.Fatal("actual isolation never armed")
	}
	earlier := time.Now().Add(150 * time.Millisecond)
	require.Same(t, original, f.s.armIsolationDeadline(earlier, "earlier_while_isolating"))
	original.mu.Lock()
	actual := original.deadline
	original.mu.Unlock()
	require.Equal(t, earlier, actual)
	fmt.Printf("QUIESCE_EARLIER_ISOLATION_ARMED monitor=%d root=%d original_owner=true e_mu_held=true\n", e.monitorPID, e.rootPID)
	expectQuiesceIsolation()
}
func nativeQuiescePausedDeadline(t *testing.T, terminal bool) {
	clock := &quiesceInterleaveClock{}
	f := newQuiesceClockFixture(t, clock)
	request := nativeQuiescenceRequest(t, f)
	entered := make(chan struct{})
	release := make(chan struct{})
	left := make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	clock.set(func(ctx context.Context) (p.ClockObservation, error) {
		hit := calls.Add(1) == 2
		if terminal {
			hit = quiesceTerminalBytes(t, f)
		}
		if hit {
			once.Do(func() { close(entered) })
			defer close(left)
			select {
			case <-release:
			case <-ctx.Done():
			}
			return quiesceObservation(ctx)
		}
		return quiesceObservation(ctx)
	})
	result := make(chan error, 1)
	go func() { _, err := f.s.beginUserQuiescence(context.Background(), request); result <- err }()
	waitQuiesceTest(t, entered)
	// The clock signal synchronizes the original published immutable pointer,
	// while the real journal and Supervisor locks are still held by that owner.
	a := f.s.quiescence
	require.NotNil(t, a)
	require.NotNil(t, a.timer)
	require.Equal(t, 31*time.Second, a.deadline.Sub(a.started))
	marker := "QUIESCE_PENDING_DEADLINE_ARMED"
	if terminal {
		marker = "QUIESCE_SIGNING_DEADLINE_ARMED"
		require.True(t, quiesceTerminalBytes(t, f))
	}
	fmt.Printf("%s actual_journal_mutex_pause=true terminal_readable=%t original_deadline=%s\n", marker, terminal, a.deadline.Format(time.RFC3339Nano))
	require.Same(t, a.timer, f.s.armIsolationDeadline(time.Now().Add(150*time.Millisecond), "controlled_journal_deadline"))
	expectQuiesceIsolation()
}
func TestTaskQuiesceNativePendingDeadline(t *testing.T) { nativeQuiescePausedDeadline(t, false) }
func TestTaskQuiesceNativeSigningDeadline(t *testing.T) { nativeQuiescePausedDeadline(t, true) }
func TestTaskQuiesceNativeFailedTerminalQuery(t *testing.T) {
	clock := &quiesceInterleaveClock{}
	f := newQuiesceClockFixture(t, clock)
	request := nativeQuiescenceRequest(t, f)
	// Controlled observation window only: immediate cooperative isolation has
	// already been consumed. The original production timer remains enforceable.
	f.s.failureOnce.Do(func() {})
	var reached atomic.Bool
	clock.set(func(ctx context.Context) (p.ClockObservation, error) {
		if quiesceTerminalBytes(t, f) {
			reached.Store(true)
			return p.ClockObservation{}, errors.New("post-persistence signing clock lost")
		}
		return quiesceObservation(ctx)
	})
	a, err := f.s.beginUserQuiescence(context.Background(), request)
	require.NoError(t, err)
	waitQuiesceTest(t, a.done)
	require.True(t, reached.Load())
	clock.set(nil)
	require.True(t, quiesceTerminalBytes(t, f))
	require.False(t, a.success)
	require.Error(t, a.err)
	require.Nil(t, a.terminalWire)
	observation, err := f.s.options.Kernel.ObserveNoUserDescendants(context.Background())
	require.NoError(t, err)
	require.NoError(t, observation.RevalidateCurrent(context.Background()))
	wire, err := f.s.queryUserQuiescence(context.Background(), request.evidence.Context(), request.evidence.Digest())
	require.Error(t, err)
	require.Nil(t, wire)
	require.Same(t, a.timer, f.s.isolationDeadline)
	a.timer.mu.Lock()
	bound := a.timer.deadline
	a.timer.mu.Unlock()
	require.False(t, bound.After(a.deadline))
	fmt.Println("QUIESCE_FAILED_TERMINAL_QUERY_REFUSED readable_terminal=true fresh_empty_census=true failed_original_coordinator=true nil_wire=true cooperative_once_controlled=true")
	require.Same(t, a.timer, f.s.armIsolationDeadline(time.Now().Add(150*time.Millisecond), "failed_terminal_query_checked"))
	expectQuiesceIsolation()
}

// exec.Cmd's existing context preflight blocks inside the SAME original Start
// call after the private commit transition, before the kernel spawn. This is
// not a claim to stall the OS syscall or an alternate production spawn callback.
type quiesceStartGate struct {
	context.Context
	once             sync.Once
	entered, release chan struct{}
}

func (g *quiesceStartGate) Done() <-chan struct{} {
	g.once.Do(func() { close(g.entered) })
	<-g.release
	return nil
}

func nativeQuiesceCommittedStart(t *testing.T, f nativeFixture) (*Execution, func(), <-chan error) {
	t.Helper()
	request := p.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "hold"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 25}
	descriptor, err := p.NewExecutionDescriptor(request)
	require.NoError(t, err)
	b, i := f.s.activation.Binding(), f.s.activation.Identity()
	now := time.Now().UTC()
	c := p.ExecStartContext{Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, IssuerCertificateID: f.issuerIdentity.CertificateID(), IssuerCertificateDigest: f.issuerIdentity.Digest(), CommandID: uuid.NewString(), OperationID: uuid.NewString(), RequestID: "original-start", OperationDigest: strings.Repeat("b", 64), SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, DataGateEpoch: 1, ControlRevision: 1, AdmissionRevision: 1, LeaseID: 1, Runtime: i.Runtime, ExpiresAt: now.Add(time.Minute)}
	ticket, err := p.SignExecStartTicket(f.issuer, f.issuerIdentity, p.ExecStartTicketClaims{Version: 1, Purpose: "operation_exec_start", Context: c, DescriptorDigest: descriptor.Digest(), NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(25 * time.Second)})
	require.NoError(t, err)
	evidence, err := f.s.options.Verifier.VerifyExecStartTicket(ticket, f.issuerWire, c, descriptor, now)
	require.NoError(t, err)
	accepted, err := f.s.journal.AcceptExecution(context.Background(), evidence)
	require.NoError(t, err)
	require.NoError(t, f.s.journal.ConsumeStart(context.Background(), accepted))
	record, err := accepted.Snapshot()
	require.NoError(t, err)
	e := &Execution{supervisor: f.s, accepted: accepted, descriptor: descriptor, commandID: c.CommandID, record: record, state: "accepted", ownerDone: make(chan struct{}), waitDone: make(chan struct{}), output: make(chan streamFrame, 2), ack: make(chan renewAck, 1)}
	e.self = e
	e.runContext, e.cancel = context.WithCancel(context.Background())
	e.authorityDeadlineNS, err = f.s.absoluteDeadline(context.Background(), record.AuthorityDeadline)
	require.NoError(t, err)
	mono, err := monitorMonotonic()
	require.NoError(t, err)
	e.commandDeadlineNS = mono + int64(25*time.Second)
	e.acceptedReceipt, err = f.s.signRecord(record)
	require.NoError(t, err)
	reqR, reqW, err := os.Pipe()
	require.NoError(t, err)
	controlR, controlW, err := os.Pipe()
	require.NoError(t, err)
	resultR, resultW, err := os.Pipe()
	require.NoError(t, err)
	e.control, e.request, e.result = controlW, reqW, resultR
	gate := &quiesceStartGate{Context: context.Background(), entered: make(chan struct{}), release: make(chan struct{})}
	cmd := exec.CommandContext(gate, f.s.options.Executable, "monitor")
	cmd.Env = []string{"GOMAXPROCS=2"}
	cmd.Dir = "/"
	cmd.ExtraFiles = []*os.File{reqR, controlR, resultW}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	require.NoError(t, sealExecDescriptors())
	f.s.mu.Lock()
	f.s.active[e.commandID] = e
	f.s.publishActiveLocked()
	f.s.mu.Unlock()
	start := monitorStart{Request: request, AuthorityDeadlineNS: e.authorityDeadlineNS, CommandDeadlineNS: e.commandDeadlineNS}
	started := make(chan error, 1)
	startOwnerDone := make(chan struct{})
	var releaseOnce sync.Once
	releaseOwner := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(func() {
		releaseOwner()
		select {
		case <-startOwnerDone:
		case <-time.After(3 * time.Second):
			t.Error("original gated start owner unjoined during cleanup")
		}
	})
	go func() {
		defer close(startOwnerDone)
		e.mu.Lock()
		err := e.beginStartLocked()
		if err == nil {
			err = f.s.startCommittedMonitor(e, start, cmd, mono, reqR, controlR, resultW)
		} else {
			e.mu.Unlock()
		}
		if err != nil {
			close(e.waitDone)
			f.s.finishExecution(e)
		}
		started <- err
	}()
	waitQuiesceTest(t, gate.entered)
	require.True(t, e.startCommitted)
	require.Zero(t, e.monitorPID)
	return e, releaseOwner, started
}
func TestTaskQuiesceNativeCommittedStart(t *testing.T) {
	f := newNativeFixture(t)
	e, release, started := nativeQuiesceCommittedStart(t, f)
	request := nativeQuiescenceRequest(t, f)
	a, err := f.s.beginUserQuiescence(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, a.executions, 1)
	require.Same(t, e, a.executions[0])
	select {
	case <-a.done:
		t.Fatal("committed original Start dropped before completion")
	default:
	}
	select {
	case <-e.ownerDone:
		t.Fatal("original owner prematurely joined")
	default:
	}
	release()
	select {
	case err := <-started:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("original Start not completed")
	}
	awaitNativeQuiescence(t, f, a)
	drainExecution(t, e)
	require.Greater(t, e.monitorPID, 1)
	require.Equal(t, "local_terminal", e.state)
	for _, done := range []<-chan struct{}{e.waitDone, e.watchdogDone, e.ownerDone, a.timer.done} {
		select {
		case <-done:
		default:
			t.Fatal("original owner/callback not joined")
		}
	}
	require.NoError(t, f.s.Close())
	require.NoError(t, f.s.Close())
	require.Same(t, a.timer, f.s.isolationDeadline)
	t.Logf("same original committed Start completed monitor=%d root=%d; sole Wait/result/watchdog/owner joined; repeated Close retained timer", e.monitorPID, e.rootPID)
}
func TestTaskQuiesceNativeRepeatedClose(t *testing.T) {
	f := newNativeFixture(t)
	e, release, started := nativeQuiesceCommittedStart(t, f)
	request := nativeQuiescenceRequest(t, f)
	a, err := f.s.beginUserQuiescence(context.Background(), request)
	require.NoError(t, err)
	f.s.failureOnce.Do(func() {}) // Controlled failed-owner observation window only.
	closed := make(chan error, 2)
	go func() { closed <- f.s.Close() }()
	go func() { closed <- f.s.Close() }()
	limit := time.Now().Add(time.Second)
	closing := false
	for time.Now().Before(limit) {
		f.s.mu.Lock()
		closing = f.s.closed
		f.s.mu.Unlock()
		if closing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	require.True(t, closing)
	require.Same(t, a.timer, f.s.isolationDeadline)
	a.timer.mu.Lock()
	original := a.timer.deadline
	a.timer.mu.Unlock()
	require.Equal(t, a.deadline, original)
	select {
	case <-closed:
		t.Fatal("Close returned before original in-flight Start owner")
	default:
	}
	release()
	select {
	case err := <-started:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("original Start unjoined")
	}
	waitQuiesceTest(t, a.done)
	for n := 0; n < 2; n++ {
		select {
		case err := <-closed:
			require.Error(t, err)
		case <-time.After(3 * time.Second):
			t.Fatal("same Close owner not joined")
		}
	}
	require.False(t, a.success)
	require.NotEmpty(t, f.s.key)
	require.Same(t, a.timer, f.s.isolationDeadline)
	a.timer.mu.Lock()
	after := a.timer.deadline
	a.timer.mu.Unlock()
	require.False(t, after.After(original))
	select {
	case <-e.ownerDone:
	default:
		t.Fatal("original execution owner not joined")
	}
	fmt.Println("QUIESCE_REPEATED_CLOSE_ARMED same_attempt=true same_timer=true no_extension=true no_resource_clear=true cooperative_once_controlled=true")
	f.s.armIsolationDeadline(time.Now().Add(150*time.Millisecond), "repeated_close_checked")
	expectQuiesceIsolation()
}
