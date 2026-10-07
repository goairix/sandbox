package etcd

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	transport "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type combinedOutput struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	markers []string
	ready   chan struct{}
	once    sync.Once
}

func (w *combinedOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.buf.Len()+len(data) > 4096 {
		return 0, fmt.Errorf("combined user output exceeds fixed4096bound")
	}
	n, err := w.buf.Write(data)
	for _, marker := range w.markers {
		if !bytes.Contains(w.buf.Bytes(), []byte(marker)) {
			return n, err
		}
	}
	w.once.Do(func() { close(w.ready) })
	return n, err
}
func (w *combinedOutput) wire() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return bytes.Clone(w.buf.Bytes())
}

func combinedLiveProcesses(t *testing.T, snapshot map[string]any, executions ...*combinedExecution) {
	t.Helper()
	processes, ok := snapshot["processes"].([]any)
	require.True(t, ok)
	pids := map[int]bool{}
	for _, value := range processes {
		process, ok := value.(map[string]any)
		require.True(t, ok)
		pid, ok := process["pid"].(float64)
		require.True(t, ok)
		pids[int(pid)] = true
	}
	for _, execution := range executions {
		execution.handle.mu.Lock()
		accepted := execution.handle.receipt
		execution.handle.mu.Unlock()
		require.Equal(t, "accepted", accepted.State())
		require.Zero(t, accepted.RootPID(), "accepted is a pre-spawn authority receipt, not a process proof")
		for _, line := range strings.Split(string(execution.out.wire()), "\n") {
			var pid, parent, uid, gid int
			if strings.HasPrefix(line, "USER_READY") || strings.HasPrefix(line, "DOUBLE_READY") {
				_, err := fmt.Sscanf(line, strings.Fields(line)[0]+" pid=%d uid=%d gid=%d", &pid, &uid, &gid)
				require.NoError(t, err)
				require.Equal(t, 1000, uid)
				require.Equal(t, 1000, gid)
				require.Greater(t, pid, 1)
				if execution.observedRootPID != 0 {
					require.Equal(t, execution.observedRootPID, pid)
				}
				execution.observedRootPID = pid
				require.True(t, pids[pid])
			} else if strings.HasPrefix(line, "LEAF_READY") {
				_, err := fmt.Sscanf(line, "LEAF_READY pid=%d parent=%d uid=%d gid=%d", &pid, &parent, &uid, &gid)
				require.NoError(t, err)
				require.Equal(t, 1000, uid)
				require.Equal(t, 1000, gid)
				require.Greater(t, pid, 1)
				require.True(t, pids[pid], "actual doublefork leaf not live in production census")
			}
		}
	}
}

type combinedExecutionResult struct {
	receipt p.LocalExecReceiptEvidence
	err     error
}
type combinedExecution struct {
	observedRootPID int
	capability      *OperationCapability
	prepared        *PreparedExecEffect
	handle          *ExecDeliveryHandle
	out             *combinedOutput
	errOut          *combinedOutput
	result          chan combinedExecutionResult
}

func (f *combinedFixture) prepareExec(t *testing.T, mode string) (*OperationCapability, *PreparedExecEffect) {
	t.Helper()
	require.Contains(t, []string{"hold", "double", "probe"}, mode)
	admission, err := f.b.BeginOperation(f.ctx, BeginOperationInput{SandboxID: f.input.SandboxID, RequestID: uuid.NewString(), Kind: OperationData})
	require.NoError(t, err)
	require.NotNil(t, admission.Capability)
	require.Equal(t, f.identity.Runtime, p.RuntimeReference(admission.Capability.record.Runtime))
	request := p.ExecutionRequest{Argv: []string{"/native/combined-user", mode}, Env: map[string]string{"PATH": "/native", "GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/", TimeoutSeconds: 60}
	prepared, err := f.b.PrepareExecEffect(f.ctx, admission.Capability, request)
	require.NoError(t, err)
	require.NoError(t, prepared.GuardCleanupError)
	require.Equal(t, OutcomeCommitted, prepared.Outcome)
	require.NotNil(t, prepared.Prepared)
	t.Logf("TASK5_ORIGINAL_EXEC mode=%s reference=%+v operation=%+v ticket_digest=%s descriptor_digest=%s deadline=%s", mode, prepared.Reference, admission.Capability.record, prepared.Prepared.draft.ticket.Digest(), prepared.Prepared.draft.descriptor.Digest(), prepared.Prepared.draft.deadline)
	return admission.Capability, prepared.Prepared
}
func (f *combinedFixture) liveExec(t *testing.T, mode string) *combinedExecution {
	t.Helper()
	c, prepared := f.prepareExec(t, mode)
	h, err := f.b.DeliverExecEffect(f.ctx, prepared, f.destination)
	require.NoError(t, err)
	require.NotNil(t, h)
	markers := []string{"USER_READY"}
	if mode == "double" {
		markers = []string{"DOUBLE_READY", "LEAF_READY"}
	}
	e := &combinedExecution{capability: c, prepared: prepared, handle: h, out: &combinedOutput{ready: make(chan struct{}), markers: markers}, errOut: &combinedOutput{ready: make(chan struct{})}, result: make(chan combinedExecutionResult, 1)}
	waitCtx, waitCancel := context.WithCancel(f.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		receipt, err := h.Wait(waitCtx, e.out, e.errOut)
		e.result <- combinedExecutionResult{receipt, err}
	}()
	t.Cleanup(func() { waitCancel(); require.NoError(t, h.Close()); <-done })
	select {
	case <-e.out.ready:
	case result := <-e.result:
		t.Fatalf("user ended before readiness: receipt=%s err=%v stdout=%s stderr=%s", result.receipt.Wire(), result.err, e.out.wire(), e.errOut.wire())
	case <-time.After(5 * time.Second):
		t.Fatal("original user did not become ready within5s")
	case <-f.ctx.Done():
		require.NoError(t, f.ctx.Err())
	}
	t.Logf("TASK5_USER_LIVE mode=%s stdout=%s", mode, e.out.wire())
	return e
}
func (f *combinedFixture) destroyAndCloseData(t *testing.T) (*TaskClaim, TaskCloseDataReference) {
	t.Helper()
	control, err := f.b.LoadControl(f.ctx, f.input.Workspace.Partition(), f.input.SandboxID)
	require.NoError(t, err)
	require.Equal(t, PhaseActive, control.Phase)
	key, _, err := f.b.namespace.sandboxKeys(f.input.Workspace.Partition(), f.input.SandboxID, f.input.SnapshotVersion)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	read, err := f.raw.Get(ctx, key)
	cancel()
	require.NoError(t, err)
	require.Len(t, read.Kvs, 1)
	stage, ref, err := f.b.PrepareDestroy(f.ctx, BeginDestroyInput{SandboxID: f.input.SandboxID, RequestID: uuid.NewString(), ExpectedControlRevision: read.Kvs[0].ModRevision}, 30*time.Second)
	require.NoError(t, err)
	outcome, err := f.b.CommitStage(f.ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)
	require.NoError(t, f.b.ReleaseStage(f.ctx, stage))
	claim, err := f.b.ClaimTask(f.ctx, ref, "combined-worker", 60*time.Second)
	require.NoError(t, err)
	closeData, err := f.b.PrepareTaskCloseData(f.ctx, claim)
	require.NoError(t, err)
	require.NoError(t, closeData.GuardCleanupError)
	require.Equal(t, OutcomeCommitted, closeData.Outcome)
	require.NotNil(t, closeData.Prepared)
	delivered, err := f.b.DeliverTaskCloseData(f.ctx, closeData.Prepared, f.destination)
	require.NoError(t, err)
	require.NotNil(t, delivered.Receipt)
	queried, err := f.b.QueryTaskCloseData(f.ctx, closeData.Reference, f.destination)
	require.NoError(t, err)
	require.NotNil(t, queried.Receipt)
	require.Equal(t, delivered.Receipt.Wire(), queried.Receipt.Wire())
	t.Logf("TASK5_DESTROY_CLOSE task=%+v original_claim=%+v birth=%d close_reference=%+v signed_receipt=%s", ref, claim.Reference(), claim.birth, closeData.Reference, queried.Receipt.Wire())
	return claim, closeData.Reference
}
func (f *combinedFixture) terminal(t *testing.T, claim *TaskClaim, ref TaskQuiescenceReference) p.TaskUserQuiescenceReceiptEvidence {
	t.Helper()
	deadline := time.Now().Add(33 * time.Second)
	original := claim.Reference()
	lastRenew := time.Now()
	for {
		q, err := f.b.QueryTaskQuiescence(f.ctx, ref.Task, f.destination)
		require.NoError(t, err)
		require.Equal(t, ref, q.Reference)
		if q.Receipt != nil {
			require.Nil(t, q.Accepted)
			require.Equal(t, "users_quiesced", q.Receipt.State())
			t.Logf("TASK5_AUTHENTICATED_TERMINAL receipt=%s", q.Receipt.Wire())
			return *q.Receipt
		}
		require.NotNil(t, q.Accepted)
		require.Equal(t, "quiescence_accepted", q.Accepted.State())
		require.True(t, time.Now().Before(deadline), "no terminal before original coordinator bound")
		if time.Since(lastRenew) > 10*time.Second {
			require.NoError(t, f.b.RenewTaskClaim(f.ctx, claim))
			require.Equal(t, original, claim.Reference(), "renewal changed original claimant")
			lastRenew = time.Now()
		}
		select {
		case <-f.ctx.Done():
			require.NoError(t, f.ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
func (f *combinedFixture) tokenPresent(t *testing.T, c *OperationCapability) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
	defer cancel()
	r, err := f.raw.Get(ctx, c.tokenKey)
	require.NoError(t, err)
	require.Len(t, r.Kvs, 1, "real original token must remain through local terminal")
	kv := r.Kvs[0]
	require.Equal(t, c.value, string(kv.Value))
	require.Equal(t, c.record.Reference.LeaseID, kv.Lease)
	require.Equal(t, kv.CreateRevision, kv.ModRevision)
	require.Equal(t, c.admitRevision, kv.CreateRevision)
	t.Logf("TASK5_REAL_TOKEN key=%s create=%d mod=%d lease=%d value=%s", kv.Key, kv.CreateRevision, kv.ModRevision, kv.Lease, kv.Value)
}
func (f *combinedFixture) expireTokens(t *testing.T, claim *TaskClaim, operations []*OperationCapability) {
	t.Helper()
	deadline := time.Now().Add(40 * time.Second)
	originalClaim := claim.Reference()
	lastRenew := time.Now()
	for {
		empty := true
		for _, op := range operations {
			ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
			read, err := f.raw.Get(ctx, op.tokenKey)
			cancel()
			require.NoError(t, err)
			if len(read.Kvs) != 0 {
				empty = false
				require.Len(t, read.Kvs, 1)
				require.Equal(t, op.value, string(read.Kvs[0].Value))
				require.Equal(t, op.record.Reference.LeaseID, read.Kvs[0].Lease)
				continue
			}
			ctx, cancel = context.WithTimeout(f.ctx, 3*time.Second)
			ttl, err := f.raw.TimeToLive(ctx, clientv3.LeaseID(op.record.Reference.LeaseID))
			cancel()
			require.NoError(t, err)
			require.Equal(t, int64(-1), ttl.TTL, "absent token alone is not native original lease expiry")
			t.Logf("TASK5_NATURAL_EXPIRY reference=%+v native_key_revision=%d native_lease_ttl=%d original_grant_seconds=%d", op.Reference(), read.Header.Revision, ttl.TTL, operationLeaseTTL)
		}
		if empty {
			return
		}
		require.True(t, time.Now().Before(deadline), "original30s native leases did not expire")
		if time.Since(lastRenew) > 10*time.Second {
			require.NoError(t, f.b.RenewTaskClaim(f.ctx, claim))
			require.Equal(t, originalClaim, claim.Reference())
			lastRenew = time.Now()
		}
		select {
		case <-f.ctx.Done():
			require.NoError(t, f.ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
func combinedUnchanged(t *testing.T, before, after map[string]*mvccpb.KeyValue, added map[string]bool, leases map[int64]bool) {
	t.Helper()
	for key, old := range before {
		current := after[key]
		if current == nil && old.Lease > 0 && leases[old.Lease] {
			t.Logf("TASK5_DECLARED_ORIGINAL_LEASE_EXPIRY key=%s lease=%d", key, old.Lease)
			continue
		}
		require.NotNil(t, current, "original record disappeared: %s", key)
		require.Equal(t, old, current, "original bytes/nativebirth/lease changed: %s", key)
	}
	for key := range after {
		if before[key] == nil {
			require.True(t, added[key], "unexpected metadata addition: %s", key)
		}
	}
}
func TestTaskDualDrainNativeActive(t *testing.T) {
	f := combinedSetup(t)
	probeToken, probePrepared := f.prepareExec(t, "probe")
	probe, err := f.b.DeliverExecEffect(f.ctx, probePrepared, f.destination)
	require.NoError(t, err)
	require.NotNil(t, probe)
	t.Cleanup(func() { require.NoError(t, probe.Close()) })
	var probeOut, probeErr bytes.Buffer
	probeReceipt, err := probe.Wait(f.ctx, &probeOut, &probeErr)
	require.NoError(t, err)
	require.True(t, probeReceipt.DrainConfirmed())
	var paths map[string]string
	require.NoError(t, json.Unmarshal(probeOut.Bytes(), &paths))
	require.Equal(t, "DAC_DENIED", paths[transport.ControlSocket])
	require.Equal(t, "DAC_DENIED", paths[transport.ClockSocket])
	require.Equal(t, "ABSENT", paths["/run/sandbox-metadata"])
	require.Equal(t, "ABSENT", paths["/run/sandbox-fixture/private"])
	t.Logf("TASK5_UID1000_PROBE receipt=%s stdout=%s stderr=%s", probeReceipt.Wire(), probeOut.Bytes(), probeErr.Bytes())
	// Public result consumption is not assumed to retire original owners. Wait
	// for actual original active retirement before fixing the two live users.
	retiredDeadline := time.Now().Add(5 * time.Second)
	for f.inspect(t, "probe_owner_retirement")["active"] != float64(0) {
		require.True(t, time.Now().Before(retiredDeadline))
		time.Sleep(20 * time.Millisecond)
	}
	hold := f.liveExec(t, "hold")
	double := f.liveExec(t, "double")
	pre := f.inspect(t, "original_live_users")
	require.Equal(t, float64(2), pre["active"])
	combinedLiveProcesses(t, pre, hold, double)
	claim, closeRef := f.destroyAndCloseData(t)
	cross := f.inspect(t, "live_after_destroy_and_close_data")
	require.Equal(t, float64(2), cross["active"])
	require.Equal(t, "closed", cross["gate"])
	combinedLiveProcesses(t, cross, hold, double)
	f.tokenPresent(t, hold.capability)
	f.tokenPresent(t, double.capability)
	before := f.snapshot(t, claim, closeRef, probePrepared, hold.prepared, double.prepared)
	prepared, err := f.b.PrepareTaskQuiescence(f.ctx, claim, f.destination)
	require.NoError(t, err)
	require.NoError(t, prepared.GuardCleanupError)
	require.Equal(t, OutcomeCommitted, prepared.Outcome)
	require.NotNil(t, prepared.Prepared)
	got, err := f.b.DeliverTaskQuiescence(f.ctx, prepared.Prepared, f.destination)
	require.NoError(t, err)
	require.Equal(t, prepared.Reference, got.Reference)
	require.True(t, (got.Accepted != nil) != (got.Receipt != nil))
	if got.Accepted != nil {
		t.Logf("TASK5_ACCEPTED_NOT_TERMINAL %s", got.Accepted.Wire())
	}
	// This finite sample may already be terminal; only actual active>0 is an
	// active-quiesce sample. Never delay the product to manufacture the phase.
	sample := f.inspect(t, "attempted_active_quiescence")
	if sample["active"] == float64(0) {
		t.Log("CannotVerify: active-quiesce cost sampling; actual sample already terminal")
	}
	terminal := f.terminal(t, claim, prepared.Reference)
	entries := []p.TaskQuiescenceExecution{}
	for _, execution := range []*combinedExecution{hold, double} {
		select {
		case result := <-execution.result:
			require.NoError(t, result.err)
			require.Equal(t, "local_terminal", result.receipt.State())
			require.True(t, result.receipt.DrainConfirmed())
			require.Greater(t, result.receipt.RootPID(), 1)
			require.Equal(t, execution.observedRootPID, result.receipt.RootPID(), "terminal must retain originally observed live root")
			d := execution.prepared.draft
			entries = append(entries, p.TaskQuiescenceExecution{CommandID: d.claims.Context.CommandID, ExecContext: d.claims.Context, TicketDigest: d.ticket.Digest(), Disposition: "local_terminal", TerminalReceiptDigest: combinedHash(result.receipt.Wire())})
			t.Logf("TASK5_ORIGINAL_OWNER_TERMINAL reference=%+v receipt=%s stdout=%s stderr=%s", execution.handle.Reference(), result.receipt.Wire(), execution.out.wire(), execution.errOut.wire())
		case <-f.ctx.Done():
			require.NoError(t, f.ctx.Err())
		}
	}
	slices.SortFunc(entries, func(a, b p.TaskQuiescenceExecution) int { return strings.Compare(a.CommandID, b.CommandID) })
	digest, err := p.DigestTaskQuiescenceExecutions(entries)
	require.NoError(t, err)
	require.Equal(t, uint32(2), terminal.RegisteredCount())
	require.Equal(t, uint32(0), terminal.NeverSpawnedCount())
	require.Equal(t, uint32(2), terminal.LocalTerminalCount())
	require.Equal(t, digest, terminal.ExecutionSetDigest())
	f.tokenPresent(t, hold.capability)
	blocked, err := f.b.PrepareAfterDualDrain(f.ctx, claim, f.destination)
	require.Error(t, err)
	require.Nil(t, blocked, "local terminal cannot replace native token zero")
	f.expireTokens(t, claim, []*OperationCapability{probeToken, hold.capability, double.capability})
	zero, err := f.b.PrepareAfterDualDrain(f.ctx, claim, f.destination)
	require.NoError(t, err)
	require.NotNil(t, zero)
	require.Same(t, claim, zero.claim)
	require.Equal(t, claim.Reference(), zero.claimReference)
	require.Positive(t, zero.emptyRevision)
	require.Positive(t, zero.pin.reservation.revision)
	require.Len(t, taskEmptyComparisons(f.b, claim, zero.pin.intent, zero.pin.receipt, zero.pin.reservation), 53)
	t.Logf("TASK5_REAL_DUAL_DRAIN native_empty_revision=%d literal_nodes=55 original_claim=%+v immutable_reservation_revision=%d intent_birth=%d receipt_birth=%d signed_receipt=%s", zero.emptyRevision, zero.claimReference, zero.pin.reservation.revision, zero.pin.intent.create, zero.pin.receipt.create, zero.receipt.Wire())
	quiesceKey, err := f.b.namespace.taskQuiescenceKey(claim.Reference().Task)
	require.NoError(t, err)
	reservationKey, err := f.b.namespace.taskQuiescenceAttemptKey(claim.Reference().Task)
	require.NoError(t, err)
	_, receiptKey, err := f.b.stageKeys(prepared.Reference.Stage)
	require.NoError(t, err)
	combinedUnchanged(t, before, f.snapshot(t, claim, closeRef, probePrepared, hold.prepared, double.prepared), map[string]bool{quiesceKey: true, reservationKey: true, receiptKey: true}, map[int64]bool{probeToken.Reference().LeaseID: true, hold.capability.Reference().LeaseID: true, double.capability.Reference().LeaseID: true, claim.Reference().LeaseID: true, f.creationLease: true})
	final := f.inspect(t, "users_drained_management_alive")
	require.Equal(t, float64(0), final["active"])
	require.Equal(t, "closed", final["gate"])
	q, err := f.b.QueryTaskQuiescence(f.ctx, prepared.Reference.Task, f.destination)
	require.NoError(t, err)
	require.NotNil(t, q.Receipt)
	require.Equal(t, terminal.Wire(), q.Receipt.Wire())
	f.members(t, "after")
	t.Log("CannotVerify: combined deterministic registered-before-spawn/Start-in-flight timing; accepted Task2 component evidence retains separate attribution")
	combinedEmit(t, "backend", "assertions_complete", "", transport.ControlSocket)
}

// Only original claim renewal is allowed while time advances naturally. This
// cannot replace the original send draft or alter any retained ticket interval.
func (f *combinedFixture) waitOriginalClock(t *testing.T, claim *TaskClaim, until time.Time) {
	t.Helper()
	original := claim.Reference()
	lastRenew := time.Now()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for time.Now().UTC().Before(until) {
		if time.Since(lastRenew) >= 10*time.Second {
			require.NoError(t, f.b.RenewTaskClaim(f.ctx, claim))
			require.Equal(t, original, claim.Reference())
			lastRenew = time.Now()
		}
		select {
		case <-f.ctx.Done():
			require.NoError(t, f.ctx.Err())
		case <-tick.C:
		}
	}
}

// This is an externally delayed negative input, not a Backend send capability.
// Fresh mutual TLS and actual application/transport EOF carry the exact frame.
func (f *combinedFixture) delayedFrame(t *testing.T, frame []byte, token *OperationCapability) {
	t.Helper()
	ctx, cancel := context.WithTimeout(f.ctx, 5*time.Second)
	defer cancel()
	config, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: f.b.execVerifier, Clock: f.clock, Credential: f.credential, PeerCertificate: f.runtimeWire, PeerRuntime: &f.identity})
	require.NoError(t, err)
	raw, err := f.dial(ctx)
	require.NoError(t, err)
	conn := tls.Client(raw, config)
	defer conn.Close()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(closed); conn.Close() })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	deadline, _ := ctx.Deadline()
	require.NoError(t, conn.SetDeadline(deadline))
	require.NoError(t, conn.HandshakeContext(ctx))
	// At the actual send boundary, strict native absence is a separate fact
	// from the retained packet's expired ticket and the durable closed gate.
	read, err := f.raw.Get(ctx, token.tokenKey)
	require.NoError(t, err)
	require.Empty(t, read.Kvs)
	written, err := io.Copy(conn, bytes.NewReader(frame))
	require.NoError(t, err)
	require.Equal(t, int64(len(frame)), written)
	require.NoError(t, conn.CloseWrite())
	require.NoError(t, raw.(*net.UnixConn).CloseWrite())
	kind, event, err := transport.ReadEvent(conn)
	require.NoError(t, err)
	require.Equal(t, transport.EventError, kind, "delayed packet must produce actual target refusal")
	t.Logf("TASK5_DELAYED_TARGET_REFUSAL original_reference=%+v frame_sha256=%s application_bytes=%d native_absent_revision=%d event_kind=%d event=%s current_runtime_not_after=%s", token.Reference(), combinedHash(frame), written, read.Header.Revision, kind, event, f.runtimeEnd.Format(time.RFC3339Nano))
	t.Log("CannotVerify: delayed frame rejection isolates closure-only cause; expired original ticket and durable DATA/USERS gates are both possible causes")
}

func combinedAssertOriginalFrame(t *testing.T, original transport.Envelope, frame []byte, decoded transport.Envelope) {
	t.Helper()
	// ReadRequest represents a zero-length body with a nonnil empty slice;
	// stdin's Go allocation shape is not part of the canonical wire contract.
	// Re-encoding both envelopes checks every header/body byte, not selected
	// fields, and retains the complete original frame and hash unchanged.
	var originalWire, decodedWire bytes.Buffer
	require.NoError(t, transport.WriteRequest(&originalWire, original))
	require.NoError(t, transport.WriteRequest(&decodedWire, decoded))
	require.Equal(t, frame, originalWire.Bytes())
	require.Equal(t, frame, decodedWire.Bytes())
	require.Equal(t, combinedHash(frame), combinedHash(decodedWire.Bytes()))
}

// Structural codec regression only: the opaque ticket/issuer bytes below grant
// no authority and are never sent or accepted as a native fixture result.
func TestTaskDualDrainFixtureOriginalFrameCodec(t *testing.T) {
	descriptor, err := p.NewExecutionDescriptor(p.ExecutionRequest{Argv: []string{"/native/combined-user", "hold"}, Env: map[string]string{"PATH": "/native", "GOMAXPROCS": "2"}, UID: 1000, GID: 1000, WorkDir: "/", TimeoutSeconds: 60})
	require.NoError(t, err)
	original := transport.StartEnvelope(p.ExecStartContext{}, descriptor, []byte("codec-only-ticket"), []byte("codec-only-issuer"))
	var wire bytes.Buffer
	require.NoError(t, transport.WriteRequest(&wire, original))
	decoded, err := transport.ReadRequest(bytes.NewReader(wire.Bytes()))
	require.NoError(t, err)
	require.Nil(t, original.Stdin)
	require.NotNil(t, decoded.Stdin)
	require.Empty(t, decoded.Stdin)
	combinedAssertOriginalFrame(t, original, wire.Bytes(), decoded)
	_, err = transport.ReadRequest(bytes.NewReader(append(bytes.Clone(wire.Bytes()), 0)))
	require.Error(t, err, "exact original application frame requires genuine EOF")
}

func TestTaskDualDrainNativeLateArrival(t *testing.T) {
	f := combinedSetup(t)
	token, execution := f.prepareExec(t, "hold")
	draft := execution.draft
	originalRef, originalDeadline := execution.Reference(), draft.deadline
	originalTicket := draft.ticket.Wire()
	envelope := transport.StartEnvelope(draft.claims.Context, draft.descriptor, originalTicket, draft.issuer.Record.Certificate)
	var retained bytes.Buffer
	require.NoError(t, transport.WriteRequest(&retained, envelope))
	frame := bytes.Clone(retained.Bytes())
	require.LessOrEqual(t, len(frame), 16384)
	decoded, err := transport.ReadRequest(bytes.NewReader(frame))
	require.NoError(t, err)
	combinedAssertOriginalFrame(t, envelope, frame, decoded)
	history, err := f.b.LoadExecEffect(f.ctx, originalRef)
	require.NoError(t, err)
	require.NotNil(t, history)
	require.Equal(t, OutcomeCommitted, history.Outcome)
	require.Equal(t, originalTicket, []byte(history.Record.Ticket))
	t.Logf("TASK5_RETAINED_ORIGINAL_FRAME reference=%+v native_birth=%d frame_sha256=%s frame=%x ticket_not_before=%s ticket_not_after=%s original_send_deadline=%s", originalRef, history.Revision, combinedHash(frame), frame, draft.claims.NotBefore.Format(time.RFC3339Nano), draft.claims.NotAfter.Format(time.RFC3339Nano), originalDeadline)
	claim, closeRef := f.destroyAndCloseData(t)
	before := f.snapshot(t, claim, closeRef, execution)
	prepared, err := f.b.PrepareTaskQuiescence(f.ctx, claim, f.destination)
	require.NoError(t, err)
	require.NoError(t, prepared.GuardCleanupError)
	require.NotNil(t, prepared.Prepared)
	require.Equal(t, OutcomeCommitted, prepared.Outcome)
	delivered, err := f.b.DeliverTaskQuiescence(f.ctx, prepared.Prepared, f.destination)
	require.NoError(t, err)
	require.Equal(t, prepared.Reference, delivered.Reference)
	receipt := f.terminal(t, claim, prepared.Reference)
	require.Zero(t, receipt.RegisteredCount())
	require.Zero(t, receipt.NeverSpawnedCount())
	require.Zero(t, receipt.LocalTerminalCount())
	emptyDigest, err := p.DigestTaskQuiescenceExecutions([]p.TaskQuiescenceExecution{})
	require.NoError(t, err)
	require.Equal(t, emptyDigest, receipt.ExecutionSetDigest())
	f.expireTokens(t, claim, []*OperationCapability{token})
	var dials atomic.Uint64
	counted := f.newDestination(t, func(ctx context.Context) (net.Conn, error) { dials.Add(1); return f.dial(ctx) })
	handle, err := f.b.DeliverExecEffect(f.ctx, execution, counted)
	require.Error(t, err)
	require.Nil(t, handle)
	require.Zero(t, dials.Load(), "expired original Backend must refuse before dialing")
	require.Equal(t, originalRef, execution.Reference())
	require.Equal(t, originalDeadline, draft.deadline)
	require.Equal(t, originalTicket, draft.ticket.Wire())
	t.Logf("TASK5_EXPIRED_BACKEND_REFUSAL reference=%+v dial_count=%d application_bytes=0 error=%v", originalRef, dials.Load(), err)
	pre := f.inspect(t, "late_before_external_packet")
	require.Equal(t, float64(0), pre["active"])
	require.Equal(t, "closed", pre["gate"])
	f.delayedFrame(t, frame, token)
	post := f.inspect(t, "late_after_external_packet")
	require.Equal(t, float64(0), post["active"])
	require.Equal(t, "closed", post["gate"])
	require.Equal(t, pre["processes"], post["processes"], "negative packet spawned a new process")
	queried, err := f.b.QueryTaskQuiescence(f.ctx, prepared.Reference.Task, f.destination)
	require.NoError(t, err)
	require.NotNil(t, queried.Receipt)
	require.Equal(t, receipt.Wire(), queried.Receipt.Wire())
	zero, err := f.b.PrepareAfterDualDrain(f.ctx, claim, f.destination)
	require.NoError(t, err)
	require.NotNil(t, zero)
	require.Positive(t, zero.emptyRevision)
	require.Len(t, taskEmptyComparisons(f.b, claim, zero.pin.intent, zero.pin.receipt, zero.pin.reservation), 53)
	quiesceKey, err := f.b.namespace.taskQuiescenceKey(claim.Reference().Task)
	require.NoError(t, err)
	reservationKey, err := f.b.namespace.taskQuiescenceAttemptKey(claim.Reference().Task)
	require.NoError(t, err)
	_, receiptKey, err := f.b.stageKeys(prepared.Reference.Stage)
	require.NoError(t, err)
	combinedUnchanged(t, before, f.snapshot(t, claim, closeRef, execution), map[string]bool{quiesceKey: true, reservationKey: true, receiptKey: true}, map[int64]bool{token.Reference().LeaseID: true, claim.Reference().LeaseID: true, f.creationLease: true})
	f.members(t, "after_late_positive")
	// Cross the retained certificate end using actual independent current clocks.
	// Original claimant renewal changes no captured send/ticket interval.
	f.waitOriginalClock(t, claim, f.runtimeEnd.Add(2*time.Second))
	require.True(t, time.Now().UTC().After(f.runtimeEnd))
	session, tlsErr := counted.Open(f.ctx)
	require.Error(t, tlsErr)
	require.Nil(t, session)
	require.Equal(t, uint64(1), dials.Load(), "fresh TLS expiry failure must actually dial")
	dials.Store(0)
	failedQuery, queryErr := f.b.QueryTaskQuiescence(f.ctx, prepared.Reference.Task, counted)
	require.Error(t, queryErr)
	require.Nil(t, failedQuery.Receipt)
	require.Nil(t, failedQuery.Accepted)
	require.Equal(t, prepared.Reference, failedQuery.Reference)
	require.Zero(t, dials.Load(), "public query must reject expired current certificate before dial")
	failedZero, zeroErr := f.b.PrepareAfterDualDrain(f.ctx, claim, counted)
	require.Error(t, zeroErr)
	require.Nil(t, failedZero)
	require.Zero(t, dials.Load())
	require.Equal(t, originalDeadline, draft.deadline)
	require.Equal(t, originalTicket, draft.ticket.Wire())
	t.Logf("TASK5_REAL_CURRENT_CERTIFICATE_EXPIRY not_after=%s observed_utc=%s fresh_tls_dials=1 backend_query_dials=0 prerequisite_dials=0 application_bytes=0 tls_error=%v query_error=%v prerequisite_error=%v", f.runtimeEnd.Format(time.RFC3339Nano), time.Now().UTC().Format(time.RFC3339Nano), tlsErr, queryErr, zeroErr)
	t.Log("CannotVerify: forced public registered-before-spawn/Start-in-flight timing; Task2 deterministic component attribution retained. Management authenticated success precedes original certificate expiry; target daemon remains alive with fresh authentication refusal afterwards.")
	combinedEmit(t, "backend", "assertions_complete", "", transport.ControlSocket)
}

var _ io.Writer = (*combinedOutput)(nil)
