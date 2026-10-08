//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func archiveNativeFixture(t *testing.T, window time.Duration) (nativeFixture, tr.TaskWorkspaceArchiveEnvelope, p.TaskWorkspaceArchiveEvidence) {
	t.Helper()
	f := newNativeFixture(t)
	cmd := exec.Command("/payload/archive-user", "prepare")
	cmd.Env = []string{"GOMAXPROCS=2"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{}}}
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	t.Logf("physically joined user fixture: %s", out)
	request := nativeQuiescenceRequest(t, f)
	q, err := f.s.beginUserQuiescence(context.Background(), request)
	require.NoError(t, err)
	awaitNativeQuiescence(t, f, q)
	current := q.evidence.Context().Current
	current.CommandID = uuid.NewString()
	c := p.TaskWorkspaceArchiveContext{Current: current, QuiescenceContext: q.evidence.Context(), QuiescenceTicketDigest: q.evidence.Digest(), QuiescenceReceiptDigest: hashBytes(q.terminalWire), PlanDigest: strings.Repeat("c", 64), QuiescenceIntentRevision: 30, FinalSyncRevision: 40}
	now := time.Now().UTC()
	wire, err := p.SignTaskWorkspaceArchiveTicket(f.issuer, f.issuerIdentity, p.TaskWorkspaceArchiveTicketClaims{Version: 1, Purpose: "task_workspace_archive", Context: c, NotBefore: now.Add(-2 * time.Second), NotAfter: now.Add(3 * time.Second), SessionNotAfter: now.Add(window)})
	require.NoError(t, err)
	evidence, err := f.s.options.Verifier.VerifyTaskWorkspaceArchiveTicket(wire, f.issuerWire, c, now)
	require.NoError(t, err)
	return f, tr.TaskWorkspaceArchiveEnvelope{Version: 1, Purpose: "task_workspace_archive", Context: c, TicketDigest: evidence.Digest(), Ticket: wire, IssuerCertificate: f.issuerWire, OriginalQuiescenceReceipt: q.terminalWire}, evidence
}

func archiveNativeDestination(t *testing.T, f nativeFixture, dial func(context.Context) (net.Conn, error)) *tr.Destination {
	t.Helper()
	credential, err := p.NewManagementTLSCredential(f.issuer, f.issuerWire, "command_issuer")
	require.NoError(t, err)
	d, err := tr.NewDestination(tr.DestinationOptions{Identity: f.s.activation.Identity(), RuntimeCertificate: f.s.activation.RuntimeCertificate(), Credential: credential, Verifier: f.s.options.Verifier, Clock: f.s.options.Clock, Dial: dial})
	require.NoError(t, err)
	return d
}

func archiveNativeRead(t *testing.T, f nativeFixture, session *tr.Session, ctx context.Context, e p.TaskWorkspaceArchiveEvidence) []byte {
	t.Helper()
	var data bytes.Buffer
	var receipt []byte
	for {
		kind, wire, err := session.ReadTaskWorkspaceArchiveFrame(ctx)
		require.NoError(t, err)
		if kind == tr.ArchiveComplete {
			receipt = wire
			break
		}
		require.Equal(t, tr.ArchiveData, kind)
		data.Write(wire)
	}
	require.NoError(t, session.RequireTaskWorkspaceArchiveEOF(ctx))
	proof, err := f.s.options.Verifier.VerifyTaskWorkspaceArchiveReceipt(receipt, f.s.activation.RuntimeCertificate(), e.Context(), e.Digest(), e.NotBefore(), e.NotAfter(), e.SessionNotAfter(), time.Now().UTC())
	require.NoError(t, err)
	require.Equal(t, uint64(data.Len()), proof.ArchiveBytes())
	require.Equal(t, hashBytes(data.Bytes()), proof.SHA256())
	reader := tar.NewReader(bytes.NewReader(data.Bytes()))
	count := uint64(0)
	found := false
	for {
		h, err := reader.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		count++
		if h.Name == "workspace/private/content" {
			body, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.Equal(t, "user private physical content\n", string(body))
			require.Equal(t, int64(0600), h.Mode)
			require.Zero(t, h.Uid)
			require.Zero(t, h.Gid)
			found = true
		}
	}
	require.True(t, found)
	require.Equal(t, count, proof.EntryCount())
	fmt.Printf("archive TAR base64: %s\narchive receipt base64: %s\n", base64.StdEncoding.EncodeToString(data.Bytes()), base64.StdEncoding.EncodeToString(receipt))
	t.Logf("actualreceived bytes=%d entries=%d sha256=%s signed=true TLS-close-notify=true PID=%d", data.Len(), count, proof.SHA256(), os.Getpid())
	return receipt
}

func TestTaskArchiveNativeTLS(t *testing.T) {
	f, e, evidence := archiveNativeFixture(t, 10*time.Second)
	socket := filepath.Join("/journal/protected", uuid.NewString()+".sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(socket, 0600))
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.s.Serve(ctx, listener) }()
	d := archiveNativeDestination(t, f, func(ctx context.Context) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "unix", socket)
	})
	session, err := d.Open(ctx)
	require.NoError(t, err)
	require.NoError(t, session.SendTaskWorkspaceArchive(ctx, e))
	receipt := archiveNativeRead(t, f, session, ctx, evidence)
	session.Close()
	// Historical query remains a management operation after the send deadline;
	// it cannot create DATA or renew the original timer/source.
	time.Sleep(11 * time.Second)
	session, err = d.Open(ctx)
	require.NoError(t, err)
	query := tr.TaskWorkspaceArchiveEnvelope{Version: 1, Purpose: "task_workspace_archive_query", Context: e.Context, TicketDigest: e.TicketDigest}
	require.NoError(t, session.SendTaskWorkspaceArchive(ctx, query))
	kind, wire, err := session.ReadTaskWorkspaceArchiveFrame(ctx)
	require.NoError(t, err)
	require.Equal(t, tr.ArchiveComplete, kind)
	require.Equal(t, receipt, wire)
	require.NoError(t, session.RequireTaskWorkspaceArchiveEOF(ctx))
	session.Close()
	query.Context.Current.CommandID = uuid.NewString()
	session, err = d.Open(ctx)
	require.NoError(t, err)
	require.NoError(t, session.SendTaskWorkspaceArchive(ctx, query))
	_, _, err = session.ReadTaskWorkspaceArchiveFrame(ctx)
	require.Error(t, err)
	session.Close()
	require.False(t, f.s.closed, "successful archive leaves management usable")
	cancel()
	require.NoError(t, <-done)
	fmt.Println("TASK_ARCHIVE_NATIVE_TLS_PASS historical_query=true foreign_refused=true management_usable=true physical_close=true")
}

func TestTaskArchiveNativeAdmissionTransfer(t *testing.T) {
	f, e, evidence := archiveNativeFixture(t, 20*time.Second)
	raw, peer := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	borrow, call := f.s.transport.borrow(ctx, raw)
	done := make(chan struct{})
	go func() { defer close(done); defer borrow.release(); defer raw.Close(); f.s.serveConnection(call, raw) }()
	d := archiveNativeDestination(t, f, func(context.Context) (net.Conn, error) { return peer, nil })
	session, err := d.Open(ctx)
	require.NoError(t, err)
	require.NoError(t, session.SendTaskWorkspaceArchive(ctx, e))
	// Unbuffered real encrypted DATA IO stays blocked across admission expiry.
	require.Eventually(t, func() bool { f.s.mu.Lock(); defer f.s.mu.Unlock(); return f.s.archive != nil }, time.Second, time.Millisecond)
	original := f.s.archive
	_, err = f.s.registerWorkspaceArchive(ctx, ctx, raw, e)
	require.Error(t, err, "pending duplicate must refuse without another producer")
	require.Same(t, original, f.s.archive)
	time.Sleep(6 * time.Second)
	archiveNativeRead(t, f, session, ctx, evidence)
	session.Close()
	<-done
	f.s.transport.workers.Wait()
	require.NoError(t, f.s.Close())
	fmt.Println("TASK_ARCHIVE_ADMISSION_TRANSFER_PASS real_TLS_DATA_blocked_beyond_5s=true physical_join=true")
}

func TestTaskArchiveNativeBlockedDeadline(t *testing.T) {
	f, e, _ := archiveNativeFixture(t, 7*time.Second)
	raw, peer := net.Pipe()
	borrow, call := f.s.transport.borrow(context.Background(), raw)
	go func() { defer borrow.release(); defer raw.Close(); f.s.serveConnection(call, raw) }()
	d := archiveNativeDestination(t, f, func(context.Context) (net.Conn, error) { return peer, nil })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := d.Open(ctx)
	require.NoError(t, err)
	require.NoError(t, session.SendTaskWorkspaceArchive(ctx, e))
	fmt.Println("TASK_ARCHIVE_BLOCKED_ARMED PID=1 actual_TLS_nonreading_DATA=true expected_exit=70")
	time.Sleep(10 * time.Second)
	t.Fatal("blocked archive failed independent isolation")
}

func TestTaskArchiveNativePhysicalClose(t *testing.T) {
	f, e, _ := archiveNativeFixture(t, 15*time.Second)
	raw, peer := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	borrow, call := f.s.transport.borrow(ctx, raw)
	done := make(chan struct{})
	go func() { defer close(done); defer borrow.release(); defer raw.Close(); f.s.serveConnection(call, raw) }()
	d := archiveNativeDestination(t, f, func(context.Context) (net.Conn, error) { return peer, nil })
	session, err := d.Open(ctx)
	require.NoError(t, err)
	require.NoError(t, session.SendTaskWorkspaceArchive(ctx, e))
	for {
		kind, _, err := session.ReadTaskWorkspaceArchiveFrame(ctx)
		require.NoError(t, err)
		if kind == tr.ArchiveComplete {
			break
		}
	}
	// The original writer now blocks in real TLS close_notify. Close cancels,
	// physically closes the original transport and joins before key destruction.
	var attempt *workspaceArchiveAttempt
	f.s.mu.Lock()
	attempt = f.s.archive
	require.NotEmpty(t, attempt.terminal)
	f.s.mu.Unlock()
	_, err = f.s.queryWorkspaceArchive(ctx, tr.TaskWorkspaceArchiveEnvelope{Context: e.Context, TicketDigest: e.TicketDigest})
	require.Error(t, err, "query cannot precede original completion IO join")
	select {
	case <-attempt.done:
		t.Fatal("nonreading peer failed to block TLS close_notify")
	default:
	}
	closed := make(chan error, 2)
	go func() { closed <- f.s.Close() }()
	go func() { closed <- f.s.Close() }()
	require.NoError(t, <-closed)
	require.NoError(t, <-closed)
	<-done
	select {
	case <-attempt.done:
	default:
		t.Fatal("Close destroyed resources before physical worker join")
	}
	require.Empty(t, f.s.transport.users)
	for _, b := range f.s.key {
		require.Zero(t, b)
	}
	select {
	case <-f.s.closeTimer.done:
	default:
		t.Fatal("Close timer owner not joined")
	}
	session.Close()
	fmt.Println("TASK_ARCHIVE_PHYSICAL_CLOSE_PASS blocked_TLS_close_notify=true concurrent_close=true actual_worker_join=true terminal_delivery_separate=true key_zeroed=true")
}

func TestTaskArchiveNativeCompletionDeliveryFailure(t *testing.T) {
	f, e, _ := archiveNativeFixture(t, 15*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	open := func() (*tr.Session, net.Conn, chan struct{}) {
		raw, peer := net.Pipe()
		borrow, call := f.s.transport.borrow(ctx, raw)
		done := make(chan struct{})
		go func() { defer close(done); defer borrow.release(); defer raw.Close(); f.s.serveConnection(call, raw) }()
		d := archiveNativeDestination(t, f, func(context.Context) (net.Conn, error) { return peer, nil })
		session, err := d.Open(ctx)
		require.NoError(t, err)
		return session, peer, done
	}
	session, peer, done := open()
	require.NoError(t, session.SendTaskWorkspaceArchive(ctx, e))
	var receipt []byte
	for {
		kind, wire, err := session.ReadTaskWorkspaceArchiveFrame(ctx)
		require.NoError(t, err)
		if kind == tr.ArchiveComplete {
			receipt = wire
			break
		}
	}
	// Break the actual delivery of close_notify after genuine signed local
	// completion. This is failed response delivery, not a second DATA attempt.
	require.NoError(t, peer.Close())
	<-done
	a := f.s.archive
	require.NotEmpty(t, a.terminal)
	require.NoError(t, a.err)
	require.Error(t, a.deliveryErr)
	session.Close()
	session, _, done = open()
	query := tr.TaskWorkspaceArchiveEnvelope{Version: 1, Purpose: "task_workspace_archive_query", Context: e.Context, TicketDigest: e.TicketDigest}
	require.NoError(t, session.SendTaskWorkspaceArchive(ctx, query))
	kind, wire, err := session.ReadTaskWorkspaceArchiveFrame(ctx)
	require.NoError(t, err)
	require.Equal(t, tr.ArchiveComplete, kind)
	require.Equal(t, receipt, wire)
	require.NoError(t, session.RequireTaskWorkspaceArchiveEOF(ctx))
	session.Close()
	<-done
	require.Same(t, a, f.s.archive)
	require.NoError(t, f.s.Close())
	fmt.Println("TASK_ARCHIVE_DELIVERY_FAILURE_PASS real_TLS_close_notify_failed=true original_terminal_retained=true fresh_TLS_query_complete_only=true DATA_replay=false")
}
