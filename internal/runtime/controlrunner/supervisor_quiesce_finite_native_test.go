//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Err is observed by the actual Lookup while it owns Journal.mu. This is a
// controlled cooperative contention seam, not stalled OS IO or clock lateness.
type finiteQuiesceLookupContext struct {
	context.Context
	once             sync.Once
	entered, release chan struct{}
}

func (c *finiteQuiesceLookupContext) Err() error {
	c.once.Do(func() {
		close(c.entered)
		select {
		case <-c.release:
		case <-c.Context.Done():
		}
	})
	return c.Context.Err()
}

type finiteQuiesceListener struct {
	net.Listener
	accepted chan *quiesceDeadlineConn
}

func (l *finiteQuiesceListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &quiesceDeadlineConn{Conn: c, deadline: make(chan time.Time, 1)}
	l.accepted <- tracked
	return tracked, nil
}

// One native changed-seam chain: real mTLS caller expiry, original finite
// borrower/connection release, same registered irreversible coordinator, fresh
// duplicate/query terminal proof, and joined concurrent/repeated Close.
func TestTaskQuiesceNativeFiniteBorrowerPending(t *testing.T) {
	f := newNativeFixture(t)
	e, _ := f.start(t, p.ExecutionRequest{Argv: []string{os.Getenv("SANDBOX_TASK3_USER"), "hold"}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 25}, 25*time.Second)
	waitExecutionReady(t, e)
	request := nativeQuiescenceRequest(t, f)
	lookupContext, lookupCancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer lookupCancel()
	gate := &finiteQuiesceLookupContext{Context: lookupContext, entered: make(chan struct{}), release: make(chan struct{})}
	lookupDone := make(chan error, 1)
	go func() { _, err := f.s.journal.Lookup(gate, e.commandID); lookupDone <- err }()
	waitQuiesceTest(t, gate.entered)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	defer release()

	socket := filepath.Join("/journal/protected", "quiesce-finite-"+uuid.NewString()+".sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(socket, 0600))
	trackedListener := &finiteQuiesceListener{Listener: listener, accepted: make(chan *quiesceDeadlineConn, 8)}
	serveContext, serveCancel := context.WithCancel(context.Background())
	defer serveCancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- f.s.Serve(serveContext, trackedListener) }()
	credential, err := p.NewManagementTLSCredential(f.issuer, f.issuerWire, "command_issuer")
	require.NoError(t, err)
	d, err := tr.NewDestination(tr.DestinationOptions{Identity: f.s.activation.Identity(), RuntimeCertificate: f.s.activation.RuntimeCertificate(), Credential: credential, Verifier: f.s.options.Verifier, Clock: f.s.options.Clock, Dial: func(ctx context.Context) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(ctx, "unix", socket)
	}})
	require.NoError(t, err)
	call, finish := context.WithTimeout(context.Background(), 8*time.Second)
	defer finish()
	session, err := d.Open(call)
	require.NoError(t, err)
	defer session.Close()
	tracked := <-trackedListener.accepted
	originalDeadline := <-tracked.deadline
	tracked.armed.Store(true)
	envelope := tr.TaskQuiesceEnvelope{Version: 1, Purpose: "task_quiesce_users", Context: request.evidence.Context(), TicketDigest: request.evidence.Digest(), Ticket: request.evidence.Wire(), IssuerCertificate: f.issuerWire, OriginalCloseDataReceipt: request.originalCloseReceipt}
	require.NoError(t, session.SendTaskQuiesce(call, envelope))
	kind, wire, err := session.Read(call)
	require.Error(t, err, "pending IO must outlive the original finite caller")
	require.Zero(t, kind)
	require.Empty(t, wire)
	require.True(t, f.s.usersClosed.Load(), "original irreversible barrier must already be registered")
	require.Zero(t, tracked.writes.Load(), "expired caller must not attempt late encrypted ACK/refusal")
	// Observe actual borrower accounting, beyond socket deadline/error alone.
	limit := originalDeadline.Add(time.Second)
	for {
		f.s.transport.mu.Lock()
		borrowers := len(f.s.transport.users)
		f.s.transport.mu.Unlock()
		if borrowers == 0 {
			break
		}
		if time.Now().After(limit) {
			t.Fatal("original finite handler retained borrower after five-second deadline")
		}
		time.Sleep(time.Millisecond)
	}
	require.False(t, time.Now().Before(originalDeadline), "must exercise the genuine original five-second bound")
	require.True(t, time.Now().Before(limit), "borrower must join promptly at original expiry")
	select {
	case <-lookupDone:
		t.Fatal("controlled journal owner ended before caller expiry")
	default:
	}
	fmt.Printf("QUIESCE_FINITE_PENDING_BORROWER_RELEASED pid1=%d original5s=true borrowers=0 late_writes=0 users_closed=true journal_lookup_held=true\n", os.Getpid())
	// Duplicate/query handshakes refuse the pending admission mutex promptly.
	for range 2 {
		short, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
		s, err := d.Open(short)
		cancel()
		if s != nil {
			s.Close()
		}
		require.Error(t, err)
	}
	release()
	require.NoError(t, <-lookupDone)
	f.s.mu.Lock()
	a := f.s.quiescence
	f.s.mu.Unlock()
	require.NotNil(t, a)
	require.Len(t, a.executions, 1)
	require.Same(t, e, a.executions[0])
	require.True(t, a.started.Before(originalDeadline))
	require.Equal(t, 31*time.Second, a.deadline.Sub(a.started))
	awaitNativeQuiescence(t, f, a)
	drainExecution(t, e)
	for _, purpose := range []string{"task_quiesce_users", "task_quiesce_users_query"} {
		bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s, err := d.Open(bounded)
		require.NoError(t, err)
		query := tr.TaskQuiesceEnvelope{Version: 1, Purpose: purpose, Context: envelope.Context, TicketDigest: envelope.TicketDigest}
		if purpose == "task_quiesce_users" {
			query = envelope
		}
		require.NoError(t, s.SendTaskQuiesce(bounded, query))
		kind, wire, err := s.Read(bounded)
		s.Close()
		cancel()
		require.NoError(t, err)
		require.Equal(t, tr.EventReceipt, kind)
		proof, err := d.VerifyTaskQuiesceReceipt(f.s.options.Verifier, wire, envelope.Context, envelope.TicketDigest, request.evidence.NotBefore(), request.evidence.NotAfter(), time.Now().UTC())
		require.NoError(t, err)
		require.EqualValues(t, 1, proof.RegisteredCount())
		require.EqualValues(t, 1, proof.LocalTerminalCount())
		require.Same(t, a, f.s.quiescence)
		require.Same(t, a.timer, f.s.isolationDeadline)
	}
	closed := make(chan error, 2)
	for range 2 {
		go func() { closed <- f.s.Close() }()
	}
	for range 2 {
		select {
		case err := <-closed:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("same registered Close owner did not join")
		}
	}
	require.NoError(t, <-serveDone)
	require.NoError(t, f.s.Close())
	require.True(t, f.s.journal.Status().Closed)
	for _, b := range f.s.key {
		require.Zero(t, b)
	}
	for _, done := range []<-chan struct{}{a.done, a.timer.done, e.ownerDone, e.waitDone, e.watchdogDone} {
		select {
		case <-done:
		default:
			t.Fatal("original coordinator/execution/timer owner unjoined")
		}
	}
	require.Less(t, time.Since(a.started), 31*time.Second)
	fmt.Printf("QUIESCE_FINITE_ORIGINAL_COORDINATOR_JOINED same_attempt=true same_timer=true original31s=true registered=1 local=1 duplicate_query_verified=true concurrent_repeated_close_joined=true key_cleared=true elapsed=%s\n", time.Since(a.started))
}
