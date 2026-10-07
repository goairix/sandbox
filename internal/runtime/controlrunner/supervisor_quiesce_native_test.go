//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func nativeQuiescenceRequest(t *testing.T, f nativeFixture) *authenticatedUserQuiescence {
	t.Helper()
	_, closeEvidence := nativeCloseEnvelope(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.s.mu.Lock()
	f.s.admission = false
	_, err := f.s.journal.CloseData(ctx, closeEvidence)
	var receipt []byte
	if err == nil {
		receipt, err = f.s.journal.SignDataCloseReceipt(ctx, closeEvidence.Context(), closeEvidence.Digest(), f.s.key)
	}
	f.s.mu.Unlock()
	require.NoError(t, err)
	current := closeEvidence.Context()
	current.CommandID = uuid.NewString()
	c := p.TaskUserQuiescenceContext{Current: current, CloseDataContext: closeEvidence.Context(), CloseDataTicketDigest: closeEvidence.Digest(), CloseDataReceiptDigest: hashBytes(receipt), CloseDataIntentRevision: 20}
	now := time.Now().UTC()
	wire, err := p.SignTaskUserQuiescenceTicket(f.issuer, f.issuerIdentity, p.TaskUserQuiescenceTicketClaims{Version: 1, Purpose: "task_quiesce_users", Context: c, NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(20 * time.Second)})
	require.NoError(t, err)
	evidence, err := f.s.options.Verifier.VerifyTaskUserQuiescenceTicket(wire, f.issuerWire, c, now)
	require.NoError(t, err)
	return &authenticatedUserQuiescence{supervisor: f.s, evidence: evidence, originalCloseReceipt: receipt}
}
func awaitNativeQuiescence(t *testing.T, f nativeFixture, a *userQuiescenceAttempt) {
	t.Helper()
	select {
	case <-a.done:
	case <-time.After(32 * time.Second):
		t.Fatal("original coordinator not joined within absolute budget")
	}
	f.s.mu.Lock()
	success, err, wire := a.success, a.err, append([]byte(nil), a.terminalWire...)
	f.s.mu.Unlock()
	require.NoError(t, err)
	require.True(t, success)
	require.NotEmpty(t, wire)
	require.Less(t, time.Since(a.started), 31*time.Second)
	select {
	case <-a.timer.done:
	default:
		t.Fatal("deadline owner not joined")
	}
	proof, err := f.s.options.Verifier.VerifyTaskUserQuiescenceReceipt(wire, f.s.activation.RuntimeCertificate(), a.evidence.Context(), a.evidence.Digest(), a.evidence.NotBefore(), a.evidence.NotAfter(), time.Now().UTC())
	require.NoError(t, err)
	observation, err := f.s.options.Kernel.ObserveNoUserDescendants(context.Background())
	require.NoError(t, err)
	require.NoError(t, observation.RevalidateCurrent(context.Background()))
	t.Logf("actual PID=%d kernel=%+v terminal=%s registered=%d never=%d local=%d digest=%s elapsed=%s; original namespace census and non-reaping ECHILD revalidated", os.Getpid(), f.s.options.Kernel.Snapshot(), proof.State(), proof.RegisteredCount(), proof.NeverSpawnedCount(), proof.LocalTerminalCount(), proof.ExecutionSetDigest(), time.Since(a.started))
}
func TestTaskQuiesceNativeLocal(t *testing.T) {
	f := newNativeFixture(t)
	e, _ := f.start(t, p.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "hold"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 25}, 25*time.Second)
	waitExecutionReady(t, e)
	request := nativeQuiescenceRequest(t, f)
	// Disconnection of the request cannot cancel the registered cleanup owner.
	ctx, cancel := context.WithCancel(context.Background())
	a, err := f.s.beginUserQuiescence(ctx, request)
	require.NoError(t, err)
	cancel()
	require.Len(t, a.executions, 1)
	require.Same(t, e, a.executions[0])
	same, err := f.s.beginUserQuiescence(context.Background(), request)
	require.NoError(t, err)
	require.Same(t, a, same)
	awaitNativeQuiescence(t, f, a)
	drainExecution(t, e)
	require.Equal(t, "local_terminal", e.state)
	// Real elapsed time exceeds the original deadline/ticket interval. Historical
	// success must use fresh bounded query authority, without creating a new timer.
	timer := time.NewTimer(time.Until(a.deadline.Add(time.Second)))
	defer timer.Stop()
	<-timer.C
	wire, err := f.s.queryUserQuiescence(context.Background(), request.evidence.Context(), request.evidence.Digest())
	require.NoError(t, err)
	require.NotEmpty(t, wire)
	require.Same(t, a.timer, f.s.isolationDeadline)
	t.Logf("genuine successful query after original deadline: elapsed=%s receipt=%s", time.Since(a.started), hashBytes(wire))
}
func TestTaskQuiesceNativeDoubleFork(t *testing.T) {
	f := newNativeFixture(t)
	e, _ := f.start(t, p.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "double"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 20}, 25*time.Second)
	waitExecutionReady(t, e)
	// Wait for actual setsid/doublefork marker while preserving the original sole
	// output reader/Wait owners; this reads only their public stream channel.
	var marker strings.Builder
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !strings.Contains(marker.String(), "DOUBLE_READY") {
		select {
		case frame, ok := <-e.output:
			if !ok {
				t.Fatal("missing doublefork marker")
			}
			marker.Write(frame.data)
		case <-deadline.C:
			t.Fatal("doublefork not ready")
		}
	}
	request := nativeQuiescenceRequest(t, f)
	a, err := f.s.beginUserQuiescence(context.Background(), request)
	require.NoError(t, err)
	awaitNativeQuiescence(t, f, a)
	drainExecution(t, e)
	t.Logf("actual DOUBLE_READY root=%d monitor=%d terminal=%s; historical already-joined entries may be excluded from frozen set", e.rootPID, e.monitorPID, e.state)
}
func TestTaskQuiesceNativeUnknownChild(t *testing.T) {
	f := newNativeFixture(t)
	child := exec.Command(os.Getenv("SANDBOX_TASK3_USER"), "leaf")
	child.Env = []string{"GOMAXPROCS=2"}
	require.NoError(t, child.Start())
	joined := make(chan error, 1)
	go func() { joined <- child.Wait() }()
	t.Cleanup(func() {
		_ = child.Process.Kill()
		select {
		case <-joined:
		case <-time.After(2 * time.Second):
			t.Error("test unknown-child original Wait unjoined")
		}
	})
	observation, err := f.s.options.Kernel.ObserveNoUserDescendants(context.Background())
	require.Error(t, err)
	require.Nil(t, observation)
	request := nativeQuiescenceRequest(t, f)
	fmt.Printf("QUIESCE_UNKNOWN_CHILD_ARMED pid1=%d unknown_child=%d observation=nil no_terminal=true\n", os.Getpid(), child.Process.Pid)
	_, err = f.s.beginUserQuiescence(context.Background(), request)
	require.NoError(t, err)
	// Expected production isolation exit70, never clean Go PASS.
	timer := time.NewTimer(33 * time.Second)
	defer timer.Stop()
	<-timer.C
	t.Fatal("unknown child did not isolate")
}
func TestTaskQuiesceNativeMonitorLoss(t *testing.T) {
	f := newNativeFixture(t)
	e, _ := f.start(t, p.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "hold"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 25}, 25*time.Second)
	waitExecutionReady(t, e)
	e.mu.Lock()
	monitor, root := e.monitorPID, e.rootPID
	e.mu.Unlock()
	fd, err := unix.PidfdOpen(monitor, 0)
	require.NoError(t, err)
	defer unix.Close(fd)
	// Freeze the exact original monitor before sending stop, preventing a racing
	// successful drain from turning this into a late signal to an exited process.
	require.NoError(t, unix.PidfdSendSignal(fd, unix.SIGSTOP, nil, 0))
	request := nativeQuiescenceRequest(t, f)
	a, err := f.s.beginUserQuiescence(context.Background(), request)
	require.NoError(t, err)
	fmt.Printf("QUIESCE_MONITOR_LOSS_ARMED pid1=%d monitor=%d root=%d original_accepted=%t no_terminal=true\n", os.Getpid(), monitor, root, a.accepted != nil)
	require.NoError(t, unix.PidfdSendSignal(fd, unix.SIGKILL, nil, 0))
	timer := time.NewTimer(33 * time.Second)
	defer timer.Stop()
	<-timer.C
	t.Fatal("monitor loss did not isolate")
}
