package etcd

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	tr "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type deliveryCountReader struct {
	io.Reader
	count *atomic.Int64
}

func (r deliveryCountReader) Read(data []byte) (int, error) {
	n, err := r.Reader.Read(data)
	r.count.Add(int64(n))
	return n, err
}

type deliveryNativeWire struct {
	mu           sync.Mutex
	destination  *tr.Destination
	armed        atomic.Bool
	bytes        atomic.Int64
	sends        atomic.Int32
	sent         chan struct{}
	body         chan struct{}
	done         chan error
	gate         <-chan struct{}
	closeControl *archiveCloseControl
	dialHook     func(context.Context) error
	claimCancel  context.CancelFunc
}

// The peer uses genuine pinned mTLS, signed frames, TAR and authenticated EOF.
// It is a protocol fixture, not PID1 or provider settlement evidence.
func deliveryNativeTarget(t *testing.T, f *quiescenceNativeFixture) *deliveryNativeWire {
	return deliveryNativeTargetData(t, f, archiveValidTar(t), 4)
}

func deliveryNativeTargetData(t *testing.T, f *quiescenceNativeFixture, data []byte, entries uint64) *deliveryNativeWire {
	t.Helper()
	d := f.c.quiescenceDraft
	c := d.claims.Context.Current
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	rk := ed25519.NewKeyFromSeed([]byte("12345678901234567890123456789013"))
	rw, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, Role: "runtime_receipt", CertificateID: uuid.NewString(), Namespace: c.Namespace, AuthorityID: c.AuthorityID, Target: c.Target, RestoreEpoch: c.RestoreEpoch, PublicKey: rk.Public().(ed25519.PublicKey), NotBefore: f.now.Add(-time.Minute), NotAfter: f.now.Add(time.Hour), SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime})
	require.NoError(t, err)
	rc, err := p.NewManagementTLSCredential(rk, rw, "runtime_receipt")
	require.NoError(t, err)
	ic, err := p.NewManagementTLSCredential(f.issuer.old.key, d.certificate, "command_issuer")
	require.NoError(t, err)
	clock := archiveConsumerClock{f.now}
	cfg, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: f.b.taskWorkspaceArchiveVerifier, Clock: clock, Credential: rc, PeerCertificate: d.certificate, Server: true})
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	w := &deliveryNativeWire{sent: make(chan struct{}), body: make(chan struct{}), done: make(chan error, 1)}
	w.destination, err = tr.NewDestination(tr.DestinationOptions{Identity: p.RuntimeIdentityContext{SandboxID: c.SandboxID, WorkspaceHash: c.WorkspaceHash, Generation: c.Generation, Runtime: c.Runtime}, RuntimeCertificate: rw, Credential: ic, Verifier: f.b.taskWorkspaceArchiveVerifier, Clock: clock, Dial: func(ctx context.Context) (net.Conn, error) {
		var dial net.Dialer
		raw, err := dial.DialContext(ctx, "tcp", listener.Addr().String())
		if err == nil && w.armed.Load() {
			w.mu.Lock()
			hook, control := w.dialHook, w.closeControl
			w.mu.Unlock()
			if hook != nil {
				if e := hook(ctx); e != nil {
					raw.Close()
					return nil, e
				}
			}
			if control != nil {
				return &archiveCloseConn{Conn: raw, control: control}, nil
			}
		}
		return raw, err
	}})
	require.NoError(t, err)
	sum := sha256.Sum256(data)
	stop := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(12 * time.Second))
				armed := w.armed.Load()
				w.mu.Lock()
				gate := w.gate
				w.mu.Unlock()
				if armed && gate != nil {
					select {
					case <-gate:
					case <-stop:
						return
					}
				}
				conn := tls.Server(raw, cfg)
				if err := conn.Handshake(); err != nil {
					if armed {
						w.done <- err
					}
					return
				}
				reader := io.Reader(conn)
				if armed {
					reader = deliveryCountReader{Reader: conn, count: &w.bytes}
				}
				request, err := tr.ReadRuntimeRequest(reader)
				if err != nil {
					if armed {
						w.done <- err
					}
					return
				}
				if request.Quiesce != nil {
					e := request.Quiesce
					wire, err := p.SignTaskUserQuiescenceReceipt(rk, p.TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: e.Context, TicketDigest: e.TicketDigest, NotBefore: d.claims.NotBefore, NotAfter: d.claims.NotAfter, ExecutionSetDigest: strings.Repeat("a", 64)})
					if err == nil {
						_ = tr.WriteEvent(conn, tr.EventReceipt, wire)
					}
					return
				}
				if request.Archive == nil {
					w.done <- errTaskArchive
					return
				}
				e := request.Archive
				now, err := f.b.observePublicationClock(context.Background())
				if err != nil {
					w.done <- err
					return
				}
				ticket, err := f.b.taskWorkspaceArchiveVerifier.VerifyTaskWorkspaceArchiveTicket(e.Ticket, e.IssuerCertificate, e.Context, now)
				if err != nil {
					w.done <- err
					return
				}
				if ticket.Digest() != e.TicketDigest {
					w.done <- errTaskArchive
					return
				}
				w.sends.Add(1)
				close(w.sent)
				select {
				case <-w.body:
				case <-stop:
					return
				}
				receipt, err := p.SignTaskWorkspaceArchiveReceipt(rk, p.TaskWorkspaceArchiveReceiptClaims{Version: 1, State: "archive_complete", Context: e.Context, TicketDigest: e.TicketDigest, NotBefore: ticket.NotBefore(), NotAfter: ticket.NotAfter(), SessionNotAfter: ticket.SessionNotAfter(), ArchiveBytes: uint64(len(data)), EntryCount: entries, SHA256: hex.EncodeToString(sum[:])})
				if err == nil {
					for offset := 0; offset < len(data) && err == nil; {
						end := offset + 32768
						if end > len(data) {
							end = len(data)
						}
						err = tr.WriteTaskWorkspaceArchiveFrame(conn, tr.ArchiveData, data[offset:end])
						offset = end
					}
				}
				if err == nil {
					err = tr.WriteTaskWorkspaceArchiveFrame(conn, tr.ArchiveComplete, receipt)
				}
				if err == nil {
					err = conn.CloseWrite()
				}
				w.done <- err
			}()
		}
	}()
	t.Cleanup(func() {
		close(stop)
		listener.Close()
		select {
		case <-joined:
		case <-time.After(3 * time.Second):
			t.Error("delivery TLS fixture did not physically join")
		}
	})
	return w
}

func deliveryNativeFixture(t *testing.T, horizon time.Duration, ttl ...time.Duration) (*quiescenceNativeFixture, *PreparedTaskWorkspaceArchive, *deliveryNativeWire) {
	return deliveryNativeFixtureWithPlan(t, horizon, nil, ttl...)
}

// A pure optional builder binds canonical policy before immutable reservation.
// Nil retains the original opaque plan and 45 second session behavior.
func deliveryNativeFixtureWithPlan(t *testing.T, horizon time.Duration, builder func(TaskRecord) TaskFinalSyncPlan, ttl ...time.Duration) (*quiescenceNativeFixture, *PreparedTaskWorkspaceArchive, *deliveryNativeWire) {
	return deliveryNativeFixtureWithBudget(t, horizon, builder, nil, ttl...)
}

func deliveryNativeFixtureWithBudget(t *testing.T, horizon time.Duration, builder func(TaskRecord) TaskFinalSyncPlan, budget *TaskWorkspaceSyncBudgetOptions, ttl ...time.Duration) (*quiescenceNativeFixture, *PreparedTaskWorkspaceArchive, *deliveryNativeWire) {
	return deliveryNativeFixtureWithBudgetSetup(t, horizon, builder, budget, nil, ttl...)
}

func deliveryNativeFixtureWithBudgetSetup(t *testing.T, horizon time.Duration, builder func(TaskRecord) TaskFinalSyncPlan, budget *TaskWorkspaceSyncBudgetOptions, setup taskDeliverySetupFactory, ttl ...time.Duration) (*quiescenceNativeFixture, *PreparedTaskWorkspaceArchive, *deliveryNativeWire) {
	return deliveryNativeFixtureWithData(t, horizon, builder, budget, setup, nil, 0, ttl...)
}

func deliveryNativeFixtureWithData(t *testing.T, horizon time.Duration, builder func(TaskRecord) TaskFinalSyncPlan, budget *TaskWorkspaceSyncBudgetOptions, setup taskDeliverySetupFactory, data []byte, entries uint64, ttl ...time.Duration) (*quiescenceNativeFixture, *PreparedTaskWorkspaceArchive, *deliveryNativeWire) {
	return deliveryNativeFixtureWithProfileData(t, horizon, builder, budget, setup, data, entries, false, ttl...)
}

func deliveryNativeFixtureWithProfileData(t *testing.T, horizon time.Duration, builder func(TaskRecord) TaskFinalSyncPlan, budget *TaskWorkspaceSyncBudgetOptions, setup taskDeliverySetupFactory, data []byte, entries uint64, recycling bool, ttl ...time.Duration) (*quiescenceNativeFixture, *PreparedTaskWorkspaceArchive, *deliveryNativeWire) {
	t.Helper()
	base := newTaskDeliveryFixtureWithSetup(t, true, builder != nil, setup)
	archiveTrackCleanup(base.b)
	f := finishQuiescenceFixture(t, base)
	archiveTrackCleanup(f.b)
	require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.c))
	if builder != nil {
		lifetime := 40 * time.Second
		if len(ttl) > 0 {
			lifetime = ttl[0] + 10*time.Second
		}
		// The canonical source fixture may outlive the old 20sec setup ctx.
		// Its raw client remains owned by test cleanup; only the new current
		// Backend/claim uses this independent bounded parent.
		longCtx, cancelLong := context.WithTimeout(context.Background(), lifetime)
		t.Cleanup(cancelLong)
		f.ctx = longCtx
	}
	identity, err := decodeIdentity(f.b.identityValue, f.b.namespace)
	require.NoError(t, err)
	identity.RestoreEpoch = f.b.restoreEpoch
	root := ed25519.NewKeyFromSeed(make([]byte, 32))
	issuer := &archiveNativeIssuer{old: f.issuer.old}
	anchor := time.Now()
	options := deliverySpoolOptions(archiveTestDirectory(t))
	if recycling && data != nil {
		// Explicit bounded 65-file recycling proof; default/legacy fixture limits
		// and production options keep their existing independent admission bounds.
		require.LessOrEqual(t, entries, uint64(67))
		options.Entries = 67
		indexBytes, err := taskArchiveIndexBytes(options.Entries)
		require.NoError(t, err)
		options.JobIndexBytes = indexBytes
		options.TotalIndexBytes = options.Jobs * options.JobIndexBytes
	}
	if budget != nil {
		metadata := "workspace_sync_budget"
		if recycling {
			metadata = "workspace_sync_recycling_budget"
		}
		key, e := f.b.namespace.Key("meta", metadata)
		require.NoError(t, e)
		value, e := encodeOperationMetadata(taskWorkspaceSyncBudgetRecord{1, *budget}, 1024)
		if recycling {
			value, e = encodeOperationMetadata(taskWorkspaceSyncRecyclingBudgetRecord{2, *budget}, 1024)
		}
		require.NoError(t, e)
		got, e := f.raw.Get(f.ctx, key)
		require.NoError(t, e)
		if len(got.Kvs) == 0 {
			_, e = f.raw.Put(f.ctx, key, value)
			require.NoError(t, e)
			if recycling {
				// Explicit operator provisioning, before opening the consumer Backend.
				// Every partition is provisioned; native batches never exceed 64 ops.
				ops := make([]clientv3.Op, 0, 64)
				for partition := 0; partition < 256; partition++ {
					for slot := uint32(0); slot < budget.SlotsPerPartition; slot++ {
						ref := f.c.reference.Task
						ref.Partition = uint8(partition)
						_, slotKey, err := f.b.workspaceSyncRecyclingKeys(TaskWorkspaceSyncIntentReference{Task: ref, BudgetID: budget.BudgetID, IntentID: uuid.NewString(), Slot: slot})
						require.NoError(t, err)
						free, err := encodeWorkspaceSyncRecyclingSlot(taskWorkspaceSyncRecyclingSlotRecord{Version: 2, BudgetID: budget.BudgetID, Partition: uint8(partition), Slot: slot, State: "free"})
						require.NoError(t, err)
						ops = append(ops, clientv3.OpPut(slotKey, free))
						if len(ops) == 64 {
							_, err = f.raw.Txn(f.ctx).Then(ops...).Commit()
							require.NoError(t, err)
							ops = ops[:0]
						}
					}
				}
				if len(ops) > 0 {
					_, err := f.raw.Txn(f.ctx).Then(ops...).Commit()
					require.NoError(t, err)
				}
			}
		} else {
			require.Len(t, got.Kvs, 1)
			require.True(t, immutableWorkspaceSyncKV(got.Kvs[0]))
			require.Equal(t, value, string(got.Kvs[0].Value))
		}
	}
	legacyBudget, recyclingBudget, requestTimeout := budget, (*TaskWorkspaceSyncBudgetOptions)(nil), 500*time.Millisecond
	if recycling {
		legacyBudget, recyclingBudget, requestTimeout = nil, budget, 3*time.Second
	}
	b, err := New(f.ctx, Options{Endpoints: f.raw.Endpoints(), Namespace: f.b.namespace, Identity: identity, AllowInsecureLoopback: true, RequestTimeout: requestTimeout, TaskQuiescenceIssuer: f.issuer, TaskWorkspaceArchiveIssuer: issuer, TaskWorkspaceArchiveSpool: options, TaskWorkspaceSyncBudget: legacyBudget, TaskWorkspaceSyncRecyclingBudget: recyclingBudget, PublicationTrust: &RuntimePublicationTrust{AuthorityID: f.b.publicationAuthorityID, Target: f.b.publicationTarget, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, Clock: testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: f.now.Add(time.Since(anchor))}, ctx.Err()
	})})
	require.NoError(t, err)
	options.Jobs = 0
	require.EqualValues(t, 2, b.archiveStore.limits.Jobs, "constructor owns its copied options")
	archiveTrackCleanup(b)
	t.Cleanup(func() { _ = b.Close() })
	claimCtx, cancelClaim := context.WithCancel(f.ctx)
	t.Cleanup(cancelClaim)
	claimTTL := 30 * time.Second
	if len(ttl) > 0 {
		claimTTL = ttl[0]
	}
	c, err := b.ClaimTask(claimCtx, f.c.reference.Task, "delivery-worker", claimTTL)
	require.NoError(t, err)
	t.Cleanup(func() {
		if budget != nil {
			cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), time.Second)
			lease, leaseErr := f.raw.TimeToLive(cleanupCtx, clientv3.LeaseID(c.reference.LeaseID))
			cancelCleanup()
			require.NoError(t, leaseErr)
			require.NotNil(t, lease)
			require.NotNil(t, lease.ResponseHeader)
			require.Equal(t, b.clusterID, lease.ResponseHeader.ClusterId)
			require.Equal(t, clientv3.LeaseID(c.reference.LeaseID), lease.ID)
			if lease.TTL < 0 {
				t.Logf("optional intent fixture original lease %d is natively absent; skip redundant revoke", c.reference.LeaseID)
				return
			}
		}
		b.archiveLife.mu.Lock()
		closed := b.archiveLife.closed
		b.archiveLife.mu.Unlock()
		if closed {
			_, err := f.raw.Revoke(context.Background(), clientv3.LeaseID(c.reference.LeaseID))
			if errors.Is(err, rpctypes.ErrLeaseNotFound) {
				t.Logf("native fixture lease %d is already absent at cleanup", c.reference.LeaseID)
			} else {
				require.NoError(t, err)
			}
		} else {
			taskReleaseClaim(t, b, c)
		}
	})
	f.b, f.c = b, c
	q, err := b.PrepareTaskQuiescence(f.ctx, c, f.target.destination)
	require.NoError(t, err)
	require.NotNil(t, q.Prepared)
	var w *deliveryNativeWire
	if data == nil {
		w = deliveryNativeTarget(t, f)
	} else {
		w = deliveryNativeTargetData(t, f, data, entries)
	}
	w.claimCancel = cancelClaim
	if horizon > 0 {
		c.mu.Lock()
		c.deadline = time.Now().Add(horizon)
		c.mu.Unlock()
	}
	prepared, err := b.PrepareAfterDualDrain(f.ctx, c, w.destination)
	require.NoError(t, err)
	plan := finalSyncPlan()
	if builder != nil {
		plan = builder(prepared.pin.task)
	}
	_, err = b.ReserveTaskFinalSync(f.ctx, c, prepared, w.destination, plan)
	require.NoError(t, err)
	r, err := b.PrepareTaskWorkspaceArchive(f.ctx, c, prepared, w.destination, 45*time.Second)
	require.NoError(t, err)
	require.NotNil(t, r.Prepared)
	w.armed.Store(true)
	return f, r.Prepared, w
}

type deliveryResult struct {
	archive *ReceivedTaskWorkspaceArchive
	err     error
}

func deliveryBegin(f *quiescenceNativeFixture, prepared *PreparedTaskWorkspaceArchive) <-chan deliveryResult {
	done := make(chan deliveryResult, 1)
	go func() { w, err := f.b.ReceiveTaskWorkspaceArchive(f.ctx, prepared); done <- deliveryResult{w, err} }()
	return done
}
func deliveryWaitSent(t *testing.T, w *deliveryNativeWire, done <-chan deliveryResult) {
	t.Helper()
	select {
	case <-w.sent:
	case r := <-done:
		t.Fatalf("delivery refused before Send: %v", r.err)
	case err := <-w.done:
		t.Fatalf("TLS producer refused before Send: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("original archive command was not sent")
	}
}
func deliveryWait(t *testing.T, done <-chan deliveryResult) deliveryResult {
	t.Helper()
	select {
	case r := <-done:
		return r
	case <-time.After(4 * time.Second):
		t.Fatal("original archive receiver did not physically join")
		return deliveryResult{}
	}
}

func TestTaskArchiveDeliveryNativeOriginalQuotaAndOneShot(t *testing.T) {
	f, prepared, wire := deliveryNativeFixture(t, 0)
	copy := *prepared
	r, err := f.b.ReceiveTaskWorkspaceArchive(f.ctx, &copy)
	require.ErrorIs(t, err, ErrInvalidRecord)
	require.Nil(t, r)
	foreign := &Backend{archiveStore: f.b.archiveStore}
	_, err = foreign.ReceiveTaskWorkspaceArchive(f.ctx, prepared)
	require.ErrorIs(t, err, ErrInvalidRecord)
	r1, err := f.b.archiveStore.reserve(f.ctx)
	require.NoError(t, err)
	r2, err := f.b.archiveStore.reserve(f.ctx)
	require.NoError(t, err)
	_, err = f.b.ReceiveTaskWorkspaceArchive(f.ctx, prepared)
	require.Error(t, err)
	require.Zero(t, wire.sends.Load())
	require.Nil(t, f.c.archiveDelivery)
	require.NoError(t, r1.close())
	require.NoError(t, r2.close())
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	close(wire.body)
	result := deliveryWait(t, done)
	require.NoError(t, result.err)
	require.NotNil(t, result.archive)
	require.NoError(t, <-wire.done)
	diagnostics, err := result.archive.witness.inspect()
	require.NoError(t, err)
	require.EqualValues(t, 4, diagnostics.Entries)
	require.EqualValues(t, len(archiveValidTar(t)), diagnostics.Bytes)
	require.Equal(t, prepared.draft.claims.Context, diagnostics.Context)
	require.EqualValues(t, 1, f.b.archiveStore.jobs)
	t.Logf("delivery_guard comparisons=%d literal_nodes=%d job_bytes=%d index_bytes=%d", len(result.archive.owner.comparisons), len(result.archive.owner.comparisons)+2, f.b.archiveStore.limits.JobBytes, f.b.archiveStore.indexBytes)
	copyResult := *result.archive
	require.Error(t, copyResult.Close())
	_, err = f.b.ReceiveTaskWorkspaceArchive(f.ctx, prepared)
	require.ErrorIs(t, err, ErrConflict)
	require.EqualValues(t, 1, wire.sends.Load())
	require.NoError(t, result.archive.Close())
	require.NoError(t, result.archive.Close())
	require.Zero(t, f.b.archiveStore.jobs)
	require.Empty(t, f.b.archiveLife.owners, "Backend retains no historical attempted-owner registry")
}

func TestTaskArchiveDeliveryNativeRenewCannotRevivePreparation(t *testing.T) {
	f, prepared, wire := deliveryNativeFixture(t, 1200*time.Millisecond)
	deadline := prepared.draft.deadline
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
	time.Sleep(time.Until(deadline) + 20*time.Millisecond)
	r, err := f.b.ReceiveTaskWorkspaceArchive(f.ctx, prepared)
	require.ErrorIs(t, err, ErrGuardExpired)
	require.Nil(t, r)
	require.Zero(t, wire.bytes.Load())
	require.Nil(t, f.c.archiveDelivery, "expired admission creates no active owner")
	require.Empty(t, f.b.archiveLife.owners)
	require.Zero(t, f.b.archiveStore.jobs)
}

func TestTaskArchiveDeliveryNativeRenewBeyondOriginalDeadline(t *testing.T) {
	f, prepared, wire := deliveryNativeFixture(t, 1800*time.Millisecond)
	old := prepared.draft.deadline
	session := prepared.draft.claims.SessionNotAfter
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	time.Sleep(300 * time.Millisecond)
	started := time.Now()
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
	require.Less(t, time.Since(started), time.Second, "renewal cannot queue behind body IO")
	time.Sleep(time.Until(old) + 100*time.Millisecond)
	select {
	case result := <-done:
		t.Fatalf("renewed receive ended at old deadline: %v", result.err)
	default:
	}
	close(wire.body)
	result := deliveryWait(t, done)
	require.NoError(t, result.err)
	require.NotNil(t, result.archive)
	require.Equal(t, session, prepared.draft.claims.SessionNotAfter)
	require.Equal(t, old, prepared.draft.deadline)
	require.NoError(t, result.archive.Close())
}

func TestTaskArchiveDeliveryNativeRenewBeyondRealClaimDeadline(t *testing.T) {
	f, prepared, wire := deliveryNativeFixture(t, 0, 3*time.Second)
	f.c.mu.Lock()
	oldClaim := f.c.deadline
	f.c.mu.Unlock()
	oldPreparation := prepared.draft.deadline
	session := prepared.draft.claims.SessionNotAfter
	t.Logf("original_native_claim_deadline=%s preparation_deadline=%s native_ttl=%ds", oldClaim.UTC().Format(time.RFC3339Nano), oldPreparation.UTC().Format(time.RFC3339Nano), f.c.ttlSeconds)
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	time.Sleep(600 * time.Millisecond)
	require.NoError(t, f.b.RenewTaskClaim(f.ctx, f.c))
	f.c.mu.Lock()
	renewed := f.c.deadline
	f.c.mu.Unlock()
	require.True(t, renewed.After(oldClaim))
	time.Sleep(time.Until(oldClaim) + 100*time.Millisecond)
	select {
	case result := <-done:
		t.Fatalf("renewed body ended at original native claim deadline: %v", result.err)
	default:
	}
	close(wire.body)
	result := deliveryWait(t, done)
	require.NoError(t, result.err)
	require.NotNil(t, result.archive)
	require.True(t, time.Now().After(oldClaim))
	t.Logf("completed=%s renewed_claim_deadline=%s", time.Now().UTC().Format(time.RFC3339Nano), renewed.UTC().Format(time.RFC3339Nano))
	require.Equal(t, oldPreparation, prepared.draft.deadline)
	require.Equal(t, session, prepared.draft.claims.SessionNotAfter)
	require.NoError(t, result.archive.Close())
}

type deliveryBlockedRenewLease struct {
	clientv3.Lease
	entered  chan struct{}
	returned chan struct{}
}

func (l *deliveryBlockedRenewLease) KeepAliveOnce(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error) {
	close(l.entered)
	<-ctx.Done()
	close(l.returned)
	return nil, ctx.Err()
}

func TestTaskArchiveDeliveryNativeExpiryDuringBlockedRenew(t *testing.T) {
	f, prepared, wire := deliveryNativeFixture(t, 0)
	control := &archiveCloseControl{entered: make(chan struct{}), release: make(chan struct{})}
	wire.mu.Lock()
	wire.closeControl = control
	wire.mu.Unlock()
	done := deliveryBegin(f, prepared)
	deliveryWaitSent(t, wire, done)
	lease := &deliveryBlockedRenewLease{Lease: f.b.client.Lease, entered: make(chan struct{}), returned: make(chan struct{})}
	f.b.client.Lease = lease
	f.c.mu.Lock()
	deadline := time.Now().Add(120 * time.Millisecond)
	f.c.deadline = deadline
	f.c.archiveDelivery.deadline.Store(&deadline)
	f.c.mu.Unlock()
	select {
	case f.c.archiveDelivery.wake <- struct{}{}:
	default:
	}
	renewed := make(chan error, 1)
	go func() { renewed <- f.b.RenewTaskClaim(f.ctx, f.c) }()
	<-lease.entered // Renew holds c.mu across this bounded native dependency.
	select {
	case <-control.entered:
	case <-time.After(350 * time.Millisecond):
		t.Fatal("expiry waited behind renewal's claim mutex")
	}
	select {
	case <-lease.returned:
		t.Fatal("physical cancellation waited for blocked renewal")
	default:
	}
	f.b.archiveStore.mu.Lock()
	require.EqualValues(t, 1, f.b.archiveStore.jobs)
	f.b.archiveStore.mu.Unlock()
	close(control.release)
	require.Error(t, <-renewed)
	result := deliveryWait(t, done)
	require.Error(t, result.err)
	require.Nil(t, result.archive)
	require.Zero(t, f.b.archiveStore.jobs)
}

func TestTaskArchiveDeliveryNativeExpiredAndFailedRenewJoin(t *testing.T) {
	for _, fault := range []string{"expiry", "renew-failure", "release", "parent-cancel"} {
		t.Run(fault, func(t *testing.T) {
			f, prepared, wire := deliveryNativeFixture(t, 1200*time.Millisecond)
			done := deliveryBegin(f, prepared)
			deliveryWaitSent(t, wire, done)
			if fault == "renew-failure" {
				_, err := f.raw.Delete(f.ctx, f.c.guardKey)
				require.NoError(t, err)
				require.Error(t, f.b.RenewTaskClaim(f.ctx, f.c))
			} else if fault == "release" {
				require.NoError(t, f.b.ReleaseTaskClaim(f.ctx, f.c))
			} else if fault == "parent-cancel" {
				// Original claim parent cancellation is observed without idle watchers.
				wire.claimCancel()
			}
			result := deliveryWait(t, done)
			require.Error(t, result.err)
			require.Nil(t, result.archive)
			require.Zero(t, f.b.archiveStore.jobs)
			_, err := f.b.ReceiveTaskWorkspaceArchive(f.ctx, prepared)
			require.Error(t, err)
			require.EqualValues(t, 1, wire.sends.Load())
		})
	}
}

func TestTaskArchiveDeliveryNativeAfterSendAuthorityLoss(t *testing.T) {
	for _, fault := range []string{"restore", "command-aba", "operation", "local-loss"} {
		t.Run(fault, func(t *testing.T) {
			f, prepared, wire := deliveryNativeFixture(t, 0)
			done := deliveryBegin(f, prepared)
			deliveryWaitSent(t, wire, done)
			if fault == "restore" {
				_, err := f.raw.Put(f.ctx, f.b.restoreKey, "restored")
				require.NoError(t, err)
			} else if fault == "command-aba" {
				key, err := f.b.namespace.taskWorkspaceArchiveKey(f.c.reference.Task)
				require.NoError(t, err)
				_, err = f.raw.Delete(f.ctx, key)
				require.NoError(t, err)
				_, err = f.raw.Put(f.ctx, key, prepared.draft.value)
				require.NoError(t, err)
			} else if fault == "operation" {
				prefix, err := f.b.taskOperationsPrefix(f.c.reference.Task)
				require.NoError(t, err)
				_, err = f.raw.Put(f.ctx, prefix+uuid.NewString(), "present")
				require.NoError(t, err)
			} else {
				f.c.mu.Lock()
				f.c.lost = true
				f.c.mu.Unlock()
			}
			result := deliveryWait(t, done)
			require.Error(t, result.err)
			require.Nil(t, result.archive)
			require.Zero(t, f.b.archiveStore.jobs)
		})
	}
}

func TestTaskArchiveDeliveryNativeHandshakeNoBytesAndNoRetry(t *testing.T) {
	for _, fault := range []string{"slow", "guard-loss", "canceled", "pre-send-cancel"} {
		t.Run(fault, func(t *testing.T) {
			f, prepared, wire := deliveryNativeFixture(t, 0)
			callCtx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			if fault == "slow" {
				gate := make(chan struct{})
				wire.mu.Lock()
				wire.gate = gate
				wire.mu.Unlock()
				f.c.mu.Lock()
				prepared.draft.deadline = time.Now().Add(150 * time.Millisecond)
				f.c.mu.Unlock()
				go func() { time.Sleep(300 * time.Millisecond); close(gate) }()
			} else if fault == "pre-send-cancel" {
				originalClock := f.b.authorityClock
				var observations atomic.Int32
				f.b.authorityClock = testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
					// Receiver ticket check, pre-open check, then authenticated
					// post-handshake pre-send guard. No command has been emitted.
					if observations.Add(1) == 3 {
						cancel()
					}
					return originalClock.Observe(ctx)
				})
			} else {
				wire.mu.Lock()
				wire.dialHook = func(ctx context.Context) error {
					if fault == "canceled" {
						return context.Canceled
					}
					_, err := f.raw.Delete(f.ctx, f.c.guardKey)
					return err
				}
				wire.mu.Unlock()
			}
			r, err := f.b.ReceiveTaskWorkspaceArchive(callCtx, prepared)
			require.Error(t, err)
			require.Nil(t, r)
			select {
			case <-wire.done:
			case <-time.After(time.Second):
				t.Fatal("TLS peer did not observe physical pre-send close")
			}
			require.Zero(t, wire.bytes.Load(), "no application command bytes after handshake delay/refusal")
			require.Zero(t, wire.sends.Load())
			_, err = f.b.ReceiveTaskWorkspaceArchive(f.ctx, prepared)
			require.ErrorIs(t, err, ErrConflict)
			require.Zero(t, wire.sends.Load())
			require.Zero(t, f.b.archiveStore.jobs)
		})
	}
}

func TestTaskArchiveDeliveryNativePostReceiveAndShutdownJoin(t *testing.T) {
	for _, fault := range []string{"post-close-loss", "release", "renew-failure", "backend-close"} {
		t.Run(fault, func(t *testing.T) {
			f, prepared, wire := deliveryNativeFixture(t, 0)
			control := &archiveCloseControl{entered: make(chan struct{}), release: make(chan struct{})}
			wire.mu.Lock()
			wire.closeControl = control
			wire.mu.Unlock()
			done := deliveryBegin(f, prepared)
			deliveryWaitSent(t, wire, done)
			close(wire.body)
			<-control.entered
			joined := make(chan error, 2)
			if fault == "post-close-loss" {
				key, err := f.b.namespace.taskWorkspaceArchiveKey(f.c.reference.Task)
				require.NoError(t, err)
				_, err = f.raw.Put(f.ctx, key, prepared.draft.value)
				require.NoError(t, err)
			} else if fault == "release" {
				go func() { joined <- f.b.ReleaseTaskClaim(f.ctx, f.c) }()
			} else if fault == "renew-failure" {
				_, err := f.raw.Delete(f.ctx, f.c.guardKey)
				require.NoError(t, err)
				go func() { joined <- f.b.RenewTaskClaim(f.ctx, f.c) }()
			} else {
				for i := 0; i < 2; i++ {
					go func() { joined <- f.b.Close() }()
				}
			}
			select {
			case result := <-done:
				t.Fatalf("receiver returned before physical Close: %v", result.err)
			case <-time.After(40 * time.Millisecond):
			}
			if fault != "post-close-loss" {
				select {
				case err := <-joined:
					t.Fatalf("owner join returned before physical Close: %v", err)
				default:
				}
			}
			f.b.archiveStore.mu.Lock()
			require.EqualValues(t, 1, f.b.archiveStore.jobs, "cancellation cannot refund before physical Close")
			require.Equal(t, f.b.archiveStore.limits.JobBytes, f.b.archiveStore.disk)
			require.Equal(t, f.b.archiveStore.indexBytes, f.b.archiveStore.memory)
			f.b.archiveStore.mu.Unlock()
			if fault == "backend-close" {
				_, err := f.b.client.Status(f.ctx, f.raw.Endpoints()[0])
				require.NoError(t, err, "client remains open until original owner physically joins")
			}
			close(control.release)
			result := deliveryWait(t, done)
			require.Error(t, result.err)
			require.Nil(t, result.archive)
			if fault == "renew-failure" {
				require.Error(t, <-joined)
			} else if fault != "post-close-loss" {
				require.NoError(t, <-joined)
			}
			if fault == "backend-close" {
				require.NoError(t, <-joined)
				require.Error(t, f.b.client.Ctx().Err())
			}
			require.Zero(t, f.b.archiveStore.jobs)
		})
	}
}

func TestTaskArchiveDeliveryNativeConstructorOrderingCleanup(t *testing.T) {
	b, raw := integrationBackend(t)
	o := integrationOptions(b, raw)
	directory := archiveTestDirectory(t)
	o.TaskWorkspaceArchiveSpool = deliverySpoolOptions(directory)
	// Metadata failure precedes any spool opening, even with a cold file.
	require.NoError(t, os.WriteFile(directory+"/cold", []byte("retain"), 0600))
	o.Identity.RestoreEpoch = "other"
	opened, err := New(context.Background(), o)
	require.ErrorIs(t, err, ErrIdentityMismatch)
	require.Nil(t, opened)
	o.Identity.RestoreEpoch = b.restoreEpoch
	opened, err = New(context.Background(), o)
	require.ErrorIs(t, err, ErrInvalidConfiguration)
	require.Nil(t, opened)
	require.NoError(t, os.Remove(directory+"/cold"))
	opened, err = New(context.Background(), o)
	require.NoError(t, err, "failed constructor retained no directory lock")
	require.NoError(t, opened.Close())
	opened, err = New(context.Background(), o)
	require.NoError(t, err)
	require.NoError(t, opened.Close())
}
