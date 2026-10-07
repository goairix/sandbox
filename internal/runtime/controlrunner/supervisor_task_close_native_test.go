//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func taskNativeDestination(t *testing.T, f nativeFixture) *tr.Destination {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.s.Serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		listener.Close()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(8 * time.Second):
			t.Error("Serve borrower join exceeded bound")
		}
	})
	credential, err := p.NewManagementTLSCredential(f.issuer, f.issuerWire, "command_issuer")
	require.NoError(t, err)
	d, err := tr.NewDestination(tr.DestinationOptions{Identity: f.s.activation.Identity(), RuntimeCertificate: f.s.activation.RuntimeCertificate(), Credential: credential, Verifier: f.s.options.Verifier, Clock: f.s.options.Clock, Dial: func(ctx context.Context) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "tcp", listener.Addr().String())
	}})
	require.NoError(t, err)
	return d
}
func nativeCloseEnvelope(t *testing.T, f nativeFixture) (tr.TaskCloseEnvelope, p.TaskCloseDataEvidence) {
	t.Helper()
	a := f.s.activation
	b, i := a.Binding(), a.Identity()
	now := time.Now().UTC()
	c := p.TaskCloseDataContext{Namespace: b.Namespace, AuthorityID: b.AuthorityID, Target: b.Target, RestoreEpoch: b.RestoreEpoch, IssuerCertificateID: f.issuerIdentity.CertificateID(), IssuerCertificateDigest: f.issuerIdentity.Digest(), CommandID: uuid.NewString(), TaskID: uuid.NewString(), TaskDigest: strings.Repeat("b", 64), ClaimID: uuid.NewString(), WorkerID: "native-worker", SandboxID: i.SandboxID, WorkspaceHash: i.WorkspaceHash, Generation: i.Generation, DataGateEpoch: a.DataGateEpoch(), ControlRevision: 10, ClaimCreateRevision: 11, LeaseID: 12, Runtime: i.Runtime}
	wire, err := p.SignTaskCloseDataTicket(f.issuer, f.issuerIdentity, p.TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: c, NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(20 * time.Second)})
	require.NoError(t, err)
	e, err := f.s.options.Verifier.VerifyTaskCloseDataTicket(wire, f.issuerWire, c, now)
	require.NoError(t, err)
	return tr.TaskCloseEnvelope{Version: 1, Purpose: "task_close_data", Context: c, TicketDigest: e.Digest(), Ticket: wire, IssuerCertificate: f.issuerWire}, e
}
func nativeTaskCall(t *testing.T, d *tr.Destination, e tr.TaskCloseEnvelope) (byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := d.Open(ctx)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.SendTaskClose(ctx, e))
	kind, wire, err := s.Read(ctx)
	require.NoError(t, err)
	return kind, wire
}
func nativeExecCall(t *testing.T, d *tr.Destination, e tr.Envelope) (byte, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := d.Open(ctx)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.Send(ctx, e))
	kind, wire, err := s.Read(ctx)
	require.NoError(t, err)
	return kind, wire
}
func TestTaskCloseNative(t *testing.T) {
	f := newNativeFixture(t)
	t.Logf("actual PID=%d kernel=%+v", os.Getpid(), f.s.options.Kernel.Snapshot())
	// Accepted before closure; its existing monitor/output/Wait owner is retained.
	execution, start := f.start(t, p.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "binary"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 10}, 15*time.Second)
	d := taskNativeDestination(t, f)
	env, e := nativeCloseEnvelope(t, f)
	kind, wire := nativeTaskCall(t, d, env)
	require.Equal(t, tr.EventReceipt, kind)
	proof, err := f.s.options.Verifier.VerifyTaskDataClosedReceipt(wire, f.s.activation.RuntimeCertificate(), e.Context(), e.Digest(), e.NotBefore(), e.NotAfter(), time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, "data_closed", proof.State())
	require.False(t, f.s.admission)
	require.Equal(t, "closed", f.s.journal.Status().Gate.GateState)
	query := env
	query.Purpose = "task_close_data_query"
	query.Ticket = nil
	query.IssuerCertificate = nil
	kind, wire = nativeTaskCall(t, d, query)
	require.Equal(t, tr.EventReceipt, kind)
	_, err = f.s.options.Verifier.VerifyTaskDataClosedReceipt(wire, f.s.activation.RuntimeCertificate(), e.Context(), e.Digest(), e.NotBefore(), e.NotAfter(), time.Now().UTC())
	require.NoError(t, err)
	bad := query
	bad.Context.ClaimID = uuid.NewString()
	kind, _ = nativeTaskCall(t, d, bad)
	require.Equal(t, tr.EventError, kind)
	kind, _ = nativeExecCall(t, d, tr.StartEnvelope(start.evidence.Context(), start.descriptor, start.evidence.Wire(), f.issuerWire))
	require.Equal(t, tr.EventError, kind)
	kind, _ = nativeExecCall(t, d, tr.Envelope{Version: 1, Purpose: "exec_renew", Context: start.evidence.Context(), DescriptorDigest: start.descriptor.Digest(), Ticket: []byte("denied before verifier"), IssuerCertificate: f.issuerWire})
	require.Equal(t, tr.EventError, kind)
	out, _ := drainExecution(t, execution)
	require.NotEmpty(t, out)
	kind, wire = nativeExecCall(t, d, tr.Envelope{Version: 1, Purpose: "exec_query", Context: start.evidence.Context(), DescriptorDigest: start.descriptor.Digest()})
	require.Equal(t, tr.EventReceipt, kind)
	execution.mu.Lock()
	record := execution.record
	execution.mu.Unlock()
	require.Equal(t, "local_terminal", record.State)
	_, err = f.s.options.Verifier.VerifyLocalExecReceipt(wire, f.s.activation.RuntimeCertificate(), record.Context, record.DescriptorDigest, record.TicketDigest, record.NotBefore, record.NotAfter, record.AuthorityDeadline, time.Now().UTC())
	require.NoError(t, err)
	t.Logf("durable data_closed command=%s retained query authenticated; preaccepted actual monitor=%d root=%d local_terminal=%s; no metadata End or remote settlement", e.Context().CommandID, execution.monitorPID, record.RootPID, record.State)
}
