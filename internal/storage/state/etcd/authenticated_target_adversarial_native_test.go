//go:build linux && (amd64 || arm64)

package etcd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	transport "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These triggers live solely in the trusted test parent's byte-connection owner.
// Clearing the deadline is Destination.Open's real post-handshake operation;
// neither the payload nor credentials are passed to the callback.
type nativeBoundaryConn struct {
	net.Conn
	afterTLS func()
	once     sync.Once
	dropACK  bool
	reading  bool
}

func (c *nativeBoundaryConn) SetDeadline(d time.Time) error {
	if d.IsZero() && c.afterTLS != nil {
		c.once.Do(c.afterTLS)
	}
	return c.Conn.SetDeadline(d)
}
func (c *nativeBoundaryConn) SetReadDeadline(d time.Time) error {
	c.reading = true
	return c.Conn.SetReadDeadline(d)
}
func (c *nativeBoundaryConn) Read(b []byte) (int, error) {
	if c.dropACK && c.reading {
		c.dropACK = false
		// Allow the quiet quick command to finish; discard only bytes this original
		// bounded reader actually receives. No detached relay or retained transcript.
		time.Sleep(100 * time.Millisecond)
		n, err := c.Conn.Read(b)
		if n > 0 {
			return 0, io.ErrUnexpectedEOF
		}
		return n, err
	}
	return c.Conn.Read(b)
}
func (f *nativeExecutionFixture) boundaryDestination(t *testing.T, after func(), dropACK bool) *transport.Destination {
	t.Helper()
	credential, err := p.NewManagementTLSCredential(f.secret.Issuer, f.secret.IssuerWire, "command_issuer")
	require.NoError(t, err)
	var first sync.Once
	d, err := transport.NewDestination(transport.DestinationOptions{Identity: f.identity, RuntimeCertificate: f.runtimeWire, Credential: credential, Verifier: f.b.execVerifier, Clock: f.clock, Dial: func(ctx context.Context) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", transport.ControlSocket)
		if err != nil {
			return nil, err
		}
		wrapped := &nativeBoundaryConn{Conn: conn}
		first.Do(func() { wrapped.afterTLS = after; wrapped.dropACK = dropACK })
		return wrapped, nil
	}})
	require.NoError(t, err)
	return d
}
func nativeRequest(mode string, seconds uint32) p.ExecutionRequest {
	return p.ExecutionRequest{Argv: []string{"/native/user", mode}, Env: map[string]string{"GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/", TimeoutSeconds: seconds}
}

// This fixed-layout physical observation is fixture custody only. It neither
// opens the journal nor authenticates absence on its own; a privileged parent
// replacement or an unpreemptible filesystem call remains an OS/operator limit.
func nativeCommandPath(t *testing.T, id string) string {
	t.Helper()
	parsed, err := uuid.Parse(id)
	require.NoError(t, err)
	require.Equal(t, parsed.String(), id)
	for _, parent := range []string{transport.ControlDirectory, transport.ControlDirectory + "/journal", transport.ControlDirectory + "/journal/commands"} {
		require.NoError(t, transport.CheckProtectedPath(parent, os.ModeDir|0700))
	}
	bucket := filepath.Join(transport.ControlDirectory, "journal", "commands", id[:2])
	err = transport.CheckProtectedPath(bucket, os.ModeDir|0700)
	require.True(t, err == nil || errors.Is(err, os.ErrNotExist), "untrusted fixed bucket: %v", err)
	return filepath.Join(bucket, id+".json")
}
func (f *nativeExecutionFixture) establishQueryControl(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, prepared := f.prepare(t, nativeRequest("quick", 3))
	h, err := f.b.DeliverExecEffect(ctx, prepared, f.destination)
	require.NoError(t, err)
	t.Cleanup(func() { h.Close() })
	receipt, err := h.Wait(ctx, io.Discard, io.Discard)
	require.NoError(t, err)
	require.Equal(t, "local_terminal", receipt.State())
	info, err := os.Lstat(nativeCommandPath(t, prepared.draft.commandID))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode())
	owner, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.Zero(t, owner.Uid)
	require.Zero(t, owner.Gid)
	require.EqualValues(t, 1, owner.Nlink)
	f.queryControl = h
	t.Logf("POSITIVE_TERMINAL_QUERY_CONTROL command=%s receipt=%s", prepared.draft.commandID, receipt.Wire())
}
func (f *nativeExecutionFixture) absentCommand(t *testing.T, prepared *PreparedExecEffect) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := f.destination.Open(ctx)
	require.NoError(t, err)
	defer session.Close()
	d := prepared.draft
	require.NoError(t, session.Send(ctx, transport.Envelope{Version: 1, Purpose: "exec_query", Context: d.claims.Context, DescriptorDigest: d.descriptor.Digest()}))
	kind, wire, err := session.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, transport.EventError, kind, "target unexpectedly has accepted history: %s", wire)
	require.NotNil(t, f.queryControl)
	control, err := f.queryControl.Wait(ctx, io.Discard, io.Discard)
	require.NoError(t, err)
	require.Equal(t, "local_terminal", control.State())
	snapshot := f.inspect(t, "REFUSAL_CURRENT_TARGET")
	require.Equal(t, "open", snapshot["gate"])
	require.Equal(t, float64(0), snapshot["active"])
	require.Zero(t, f.clock.offsetNS.Load(), "fixture clock must be restored before diagnostic absence")
	path := nativeCommandPath(t, d.commandID)
	_, err = os.Lstat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "durable acceptance exists at exact original command path")
	t.Logf("ACTUAL_TARGET_NO_COMMAND command=%s event=%d response=%s exact_journal_path=%s absence=ENOENT positive_control_root=%d", d.commandID, kind, wire, path, control.RootPID())
}
func TestAuthenticatedTargetNativeFences(t *testing.T) {
	f := nativeExecutionSetup(t)
	f.establishQueryControl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	families := []string{"placement", "control", "owner", "fence", "guard", "token", "receipt", "identity", "epoch", "effect"}
	for _, family := range families {
		for _, defect := range []string{"value", "lease", "recreation"} {
			// Identity/epoch are value+no-Lease fences; same-value recreation is valid.
			if defect == "recreation" && (family == "identity" || family == "epoch") {
				continue
			}
			t.Run(family+"/"+defect, func(t *testing.T) {
				f.fenceMutation(t, ctx, family, defect, true)
			})
		}
	}
	// Revoke only this exact original operation Lease. Never restore/reuse it.
	t.Run("original-lease-revoked", func(t *testing.T) {
		capability, prepared := f.prepare(t, nativeRequest("quick", 5))
		reached := false
		destination := f.boundaryDestination(t, func() {
			reached = true
			_, err := f.raw.Revoke(ctx, clientv3.LeaseID(capability.Reference().LeaseID))
			require.NoError(t, err)
		}, false)
		h, err := f.b.DeliverExecEffect(ctx, prepared, destination)
		require.True(t, reached)
		require.Error(t, err)
		require.Nil(t, h)
		f.absentCommand(t, prepared)
	})

	for _, seconds := range []int64{31, 46 * 60} {
		t.Run(fmt.Sprintf("trusted-clock-advance-%ds", seconds), func(t *testing.T) {
			_, prepared := f.prepare(t, nativeRequest("quick", 5))
			reached := false
			destination := f.boundaryDestination(t, func() { reached = true; f.clock.offsetNS.Store(int64(time.Duration(seconds) * time.Second)) }, false)
			h, err := f.b.DeliverExecEffect(ctx, prepared, destination)
			f.clock.offsetNS.Store(0)
			require.True(t, reached)
			require.Error(t, err)
			require.Nil(t, h)
			f.absentCommand(t, prepared)
		})
	}
	for _, source := range []string{"original-parent", "delivery-caller"} {
		t.Run(source+"-cancelled-after-TLS", func(t *testing.T) {
			parent, parentCancel := context.WithCancel(ctx)
			defer parentCancel()
			admission, err := f.b.BeginOperation(parent, BeginOperationInput{SandboxID: f.input.SandboxID, RequestID: uuid.NewString(), Kind: OperationData})
			require.NoError(t, err)
			prepared, err := f.b.PrepareExecEffect(parent, admission.Capability, nativeRequest("quick", 5))
			require.NoError(t, err)
			require.NotNil(t, prepared.Prepared)
			caller, callerCancel := context.WithCancel(ctx)
			defer callerCancel()
			reached := false
			destination := f.boundaryDestination(t, func() {
				reached = true
				if source == "original-parent" {
					parentCancel()
				} else {
					callerCancel()
				}
			}, false)
			h, err := f.b.DeliverExecEffect(caller, prepared.Prepared, destination)
			require.True(t, reached)
			require.Error(t, err)
			require.Nil(t, h)
			f.absentCommand(t, prepared.Prepared)
		})
	}
	require.Equal(t, float64(0), f.inspect(t, "FENCE_FINAL_IDLE")["active"])
}
func TestAuthenticatedTargetNativeLifecycle(t *testing.T) {
	f := nativeExecutionSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	t.Run("abandoned-normal-completion", func(t *testing.T) {
		_, prepared := f.prepare(t, nativeRequest("quick", 5))
		h, err := f.b.DeliverExecEffect(ctx, prepared, f.destination)
		require.NoError(t, err)
		defer h.Close()
		h.mu.Lock()
		reader := h.reader
		h.mu.Unlock()
		require.NotNil(t, reader)
		select {
		case <-reader.done:
		case <-time.After(5 * time.Second):
			t.Fatal("accepted handle retained reader without Wait")
		}
		require.NoError(t, reader.err)
		require.Equal(t, "local_terminal", reader.receipt.State())
		receipt, err := h.Wait(ctx, io.Discard, io.Discard)
		require.NoError(t, err)
		require.Equal(t, reader.receipt.Wire(), receipt.Wire())
		t.Logf("ABANDONED_READER_JOINED receipt=%s", receipt.Wire())
	})
	t.Run("uncertain-ACK-same-command-query", func(t *testing.T) {
		_, prepared := f.prepare(t, nativeRequest("quick", 5))
		destination := f.boundaryDestination(t, nil, true)
		h, err := f.b.DeliverExecEffect(ctx, prepared, destination)
		require.ErrorIs(t, err, ErrDeliveryUnknown)
		require.NotNil(t, h)
		defer h.Close()
		receipt, err := h.Wait(ctx, io.Discard, io.Discard)
		require.NoError(t, err)
		require.Equal(t, "local_terminal", receipt.State())
		require.True(t, receipt.DrainConfirmed())
		again, err := f.b.DeliverExecEffect(ctx, prepared, destination)
		require.ErrorIs(t, err, ErrConflict)
		require.Same(t, h, again)
		second, err := h.Wait(ctx, io.Discard, io.Discard)
		require.NoError(t, err)
		require.Equal(t, receipt.RootPID(), second.RootPID())
		require.Equal(t, receipt.Wire(), second.Wire())
		// The trusted adversarial fixture explicitly repeats the same original
		// finite frame to exercise target dedup as well as backend handle reuse.
		session, err := f.destination.Open(ctx)
		require.NoError(t, err)
		d := prepared.draft
		require.NoError(t, session.Send(ctx, transport.StartEnvelope(d.claims.Context, d.descriptor, d.ticket.Wire(), d.issuer.Record.Certificate)))
		kind, wire, err := session.Read(ctx)
		require.NoError(t, err)
		require.Equal(t, transport.EventReceipt, kind)
		repeated, err := h.observeReceipt(ctx, kind, wire)
		require.NoError(t, err)
		require.Equal(t, receipt.RootPID(), repeated.RootPID())
		require.Equal(t, receipt.Wire(), repeated.Wire())
		session.Close()
		t.Logf("UNCERTAIN_ACK_SAME_COMMAND_NO_REPLAY command=%s root_pid=%d receipt=%s", prepared.draft.commandID, receipt.RootPID(), receipt.Wire())
	})
	for _, mode := range []string{"tty", "double"} {
		t.Run(mode, func(t *testing.T) {
			request := nativeRequest(mode, 5)
			if mode == "tty" {
				request.Argv[1] = "probe"
				request.TTY = true
			}
			_, prepared := f.prepare(t, request)
			h, err := f.b.DeliverExecEffect(ctx, prepared, f.destination)
			require.NoError(t, err)
			defer h.Close()
			var output bytes.Buffer
			receipt, err := h.Wait(ctx, &output, io.Discard)
			require.NoError(t, err)
			require.Equal(t, "local_terminal", receipt.State())
			require.True(t, receipt.DrainConfirmed())
			require.Zero(t, receipt.RootWaitStatus())
			if mode == "tty" {
				require.Contains(t, output.String(), `"tty":[true,true,true]`)
			} else {
				require.Contains(t, output.String(), "DOUBLE_READY")
			}
			t.Logf("FULL_SERVE_%s receipt=%s output=%s", mode, receipt.Wire(), output.Bytes())
		})
	}
	require.Equal(t, float64(0), f.inspect(t, "LIFECYCLE_FINAL_IDLE")["active"])
}
func TestAuthenticatedTargetNativeStreamLoss(t *testing.T) {
	f := nativeExecutionSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, prepared := f.prepare(t, nativeRequest("flood", 10))
	h, err := f.b.DeliverExecEffect(ctx, prepared, f.destination)
	require.NoError(t, err)
	h.mu.Lock()
	reader := h.reader
	h.mu.Unlock()
	require.NotNil(t, reader)
	// Arbitrary caller Writer work stays in Wait's caller goroutine. Close must
	// join its owned reader without waiting for this deliberately blocked writer.
	writer := &nativeBlockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	waitDone := make(chan error, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(writer.release) }) }
	go func() { defer close(waitDone); _, err := h.Wait(ctx, writer, io.Discard); waitDone <- err }()
	defer func() { h.Close(); release(); <-waitDone }()
	select {
	case <-writer.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.Contains(t, string(writer.first), "USER_READY")
	started := time.Now()
	require.NoError(t, h.Close())
	require.Less(t, time.Since(started), time.Second)
	release()
	select {
	case err := <-waitDone:
		require.Error(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-reader.done:
	default:
		t.Fatal("Close did not join reader")
	}
	require.Error(t, f.b.RenewExecDelivery(ctx, h))
	require.False(t, h.terminal)
	time.Sleep(time.Second)
	receipt, err := h.Wait(ctx, io.Discard, io.Discard)
	require.Error(t, err)
	require.NotEqual(t, "local_terminal", receipt.State())
	t.Logf("ACTUAL_ACCEPTED_STREAM_CLOSE_UNKNOWN command=%s error=%v", prepared.draft.commandID, err)
}
func TestAuthenticatedTargetNativeStartFailure(t *testing.T) {
	f := nativeExecutionSetup(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request := nativeRequest("quick", 5)
	request.Argv = []string{"/native/absent-user-executable"}
	_, prepared := f.prepare(t, request)
	h, err := f.b.DeliverExecEffect(ctx, prepared, f.destination)
	if h != nil {
		defer h.Close()
	}
	if err == nil {
		var receipt p.LocalExecReceiptEvidence
		receipt, err = h.Wait(ctx, io.Discard, io.Discard)
		require.NotEqual(t, "local_terminal", receipt.State())
	}
	require.Error(t, err)
	require.False(t, errors.Is(err, context.Canceled))
	fmt.Printf("ACTUAL_MONITOR_USER_START_FAILURE command=%s error=%v\n", prepared.draft.commandID, err)
}

// Its blocking contract is intentional and released/joined by the test owner.
type nativeBlockingWriter struct {
	entered, release chan struct{}
	once             sync.Once
	first            []byte
}

func (w *nativeBlockingWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { w.first = bytes.Clone(b); close(w.entered) })
	<-w.release
	return len(b), nil
}

// Shared write-once index/issuer rows are never byte-restored for later cases.
// Every isolated selector uses a fresh Root-owned project and public admission.
func (f *nativeExecutionFixture) fenceMutation(t *testing.T, ctx context.Context, family, defect string, restoreAfter bool) {
	t.Helper()
	capability, prepared := f.prepare(t, nativeRequest("quick", 5))
	require.Len(t, capability.fences, 5)
	effect, err := f.b.namespace.execEffectKey(capability.Reference())
	require.NoError(t, err)
	keys := map[string]string{"placement": capability.fences[0].Key, "control": capability.fences[1].Key, "owner": capability.fences[2].Key, "fence": capability.fences[3].Key, "index": capability.fences[4].Key, "guard": capability.guardKey, "token": capability.tokenKey, "receipt": capability.receiptKey, "issuer": prepared.draft.issuerKey, "identity": f.b.identityKey, "epoch": f.b.restoreKey, "effect": effect}
	key := keys[family]
	old, err := f.raw.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, old.Kvs, 1)
	kv := old.Kvs[0]
	var added clientv3.LeaseID
	restore := func() {
		options := []clientv3.OpOption{}
		if kv.Lease != 0 {
			options = append(options, clientv3.WithLease(clientv3.LeaseID(kv.Lease)))
		}
		_, err := f.raw.Put(ctx, key, string(kv.Value), options...)
		require.NoError(t, err)
		if added != 0 {
			_, err = f.raw.Revoke(ctx, added)
			require.NoError(t, err)
		}
	}
	if restoreAfter {
		defer restore()
	} else {
		t.Cleanup(func() {
			bounded, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if added != 0 {
				_, err := f.raw.Revoke(bounded, added)
				require.NoError(t, err)
			}
		})
	}
	completedTLS := false
	destination := f.boundaryDestination(t, func() {
		completedTLS = true
		value := string(kv.Value)
		options := []clientv3.OpOption{}
		if kv.Lease != 0 {
			options = append(options, clientv3.WithLease(clientv3.LeaseID(kv.Lease)))
		}
		switch defect {
		case "value":
			value += " "
		case "lease":
			lease, err := f.raw.Grant(ctx, 30)
			require.NoError(t, err)
			added = lease.ID
			options = []clientv3.OpOption{clientv3.WithLease(added)}
		case "recreation":
			_, err := f.raw.Delete(ctx, key)
			require.NoError(t, err)
		}
		changed, err := f.raw.Put(ctx, key, value, options...)
		require.NoError(t, err)
		t.Logf("AFTER_ACTUAL_TLS_MUTATION family=%s defect=%s key=%s old_create=%d old_mod=%d old_lease=%d new_revision=%d", family, defect, key, kv.CreateRevision, kv.ModRevision, kv.Lease, changed.Header.Revision)
	}, false)
	h, err := f.b.DeliverExecEffect(ctx, prepared, destination)
	require.True(t, completedTLS, "case did not reach actual mutual TLS completion")
	require.Error(t, err)
	require.Nil(t, h)
	require.Nil(t, prepared.draft.delivery)
	f.absentCommand(t, prepared)
}
func nativeImmutableFence(t *testing.T, family, defect string) {
	f := nativeExecutionSetup(t)
	f.establishQueryControl(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f.fenceMutation(t, ctx, family, defect, false)
}
func TestAuthenticatedTargetNativeIndexValue(t *testing.T) { nativeImmutableFence(t, "index", "value") }
func TestAuthenticatedTargetNativeIndexLease(t *testing.T) { nativeImmutableFence(t, "index", "lease") }
func TestAuthenticatedTargetNativeIndexRecreation(t *testing.T) {
	nativeImmutableFence(t, "index", "recreation")
}
func TestAuthenticatedTargetNativeIssuerValue(t *testing.T) {
	nativeImmutableFence(t, "issuer", "value")
}
func TestAuthenticatedTargetNativeIssuerLease(t *testing.T) {
	nativeImmutableFence(t, "issuer", "lease")
}
func TestAuthenticatedTargetNativeIssuerRecreation(t *testing.T) {
	nativeImmutableFence(t, "issuer", "recreation")
}
