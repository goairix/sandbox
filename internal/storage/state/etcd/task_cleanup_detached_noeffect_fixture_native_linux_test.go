//go:build linux

package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/kubecontract"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// The fixture owns real three-member KV history, the original TestMain Kernel,
// a real queued registry job and original public ClaimTask pointer. The test
// parks the queued job using started solely to control test scheduling. Its
// original actor writes Run and handles all later progress. This does not prove
// managed profile,
// inventory or consumer admission; the old local-only admission stays strict.
// Its mTLS test runtime
// supplies signed attribution only; it proves neither a protected PID1 nor a
// physical cgroup census. Full detached consumer/PID1 coverage belongs to Task5.
type cleanupDetachedNoEffectNativeFixture struct {
	*cleanupSlotTestFixture
	worker      *DueWorker
	job         *cleanupLocalJob
	destination *ct.Destination
	drained     *PreparedTaskQuiescence
	publication *RuntimeEnrolledPublicationEntry
	queryCount  atomic.Int64
	effectCount atomic.Int64
}

func newCleanupDetachedNoEffectNativeFixture(t *testing.T, claimWorker ...string) *cleanupDetachedNoEffectNativeFixture {
	t.Helper()
	return newCleanupDetachedNoEffectNativeFixtureWithOptions(t, nil, claimWorker...)
}

// Optional component dependencies are installed before the second formal New.
// The callback receives only the original just-published signed enrollment;
// existing callers retain precisely their original setup and worker attribution.
func newCleanupDetachedNoEffectNativeFixtureWithOptions(t *testing.T, configure func(*Options, p.EnrolledRuntimeEvidence), claimWorker ...string) *cleanupDetachedNoEffectNativeFixture {
	t.Helper()
	return newCleanupDetachedNoEffectNativeFixtureWithClaimTTL(t, 30*time.Second, configure, claimWorker...)
}

// A short test TTL still goes through the original ClaimTask Grant and native
// lease checks. Default callers retain thirty seconds; renewal tests never
// assign c.deadline or reconstruct a capability from copied lease diagnostics.
func newCleanupDetachedNoEffectNativeFixtureWithClaimTTL(t *testing.T, claimTTL time.Duration, configure func(*Options, p.EnrolledRuntimeEvidence), claimWorker ...string) *cleanupDetachedNoEffectNativeFixture {
	t.Helper()
	require.Positive(t, claimTTL)
	if workspaceSyncLocalNativeWorker == nil {
		t.Skip("requires the existing explicit original Linux worker TestMain")
	}
	require.NotEmpty(t, os.Getenv("TEST_ETCD_ENDPOINTS"), "native writer requires the current real three-member fixture")
	base, raw := integrationBackend(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	now := time.Now().UTC()
	root := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, 32))
	o := integrationOptions(base, raw)
	o.RequestTimeout = time.Second
	o.WorkerControl = &WorkerControlOptions{SessionTTL: 60 * time.Second}
	o.Clock = testAuthorityClock(func(ctx context.Context) (ClockObservation, error) { return ClockObservation{UTC: now}, ctx.Err() })
	o.PublicationTrust = &RuntimePublicationTrust{AuthorityID: o.Identity.RuntimeID, Target: "target", Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}
	contract, err := kubecontract.EnrolledPlainContractDigest(detachedCleanupPlainImageForTest)
	require.NoError(t, err)
	o.EnrolledPublication = &EnrolledPublicationOptions{ContractDigest: contract, MaxActive: 4}
	o.DetachedCleanup = &DetachedCleanupOptions{PinnedPlainImage: detachedCleanupPlainImageForTest}
	profileKey, err := base.namespace.Key("meta", "worker_control_budget")
	require.NoError(t, err)
	_, err = raw.Put(ctx, profileKey, workerTestProfile(o))
	require.NoError(t, err)
	producer, err := New(ctx, o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, producer.Close()) })
	idempotency := uuid.NewString()
	workspace, err := producer.DetachedIdentity("noeffect-native", idempotency)
	require.NoError(t, err)
	input := AcquireIntentInput{Workspace: workspace, Principal: "noeffect-native", IdempotencyKey: idempotency, RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"native":"detached plain source"}`), Now: now, TTL: time.Hour}
	acquired, err := producer.AcquireIntent(ctx, input)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquired.Outcome)
	creation, err := producer.ClaimCreation(ctx, workspace, input.IntentID, "source", 30*time.Second)
	require.NoError(t, err)
	t.Cleanup(func() { _ = producer.ReleaseCreationClaim(context.Background(), creation) })
	dispatch, err := producer.DeclareRuntimeDispatch(ctx, creation, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: "target", Payload: json.RawMessage(`{"native":"detached plain source"}`)})
	require.NoError(t, err)
	runtime := RuntimeReference{ID: "plain-pod", UID: uuid.NewString(), BootID: uuid.NewString()}
	certificate := preparationCertificate(t, producer, dispatch.Entry.Record, root, "plain", runtime)
	bound, err := producer.BindRuntime(ctx, creation, certificate)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, bound.Outcome)
	mounted, err := producer.ConsumeRuntimeMount(ctx, creation)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, mounted.Outcome)
	proof, enrollment := duePublicationMaterial(t, producer, creation, root, certificate, runtime, now)
	published, err := producer.PublishRuntimeEnrolled(ctx, creation, proof, enrollment)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	require.NoError(t, producer.ReleaseCreationClaim(ctx, creation))

	// Reconstruct through New with the genuine original enrollment issuer,
	// rather than patching a new signer or accepting a copied authority token.
	issuerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{8}, 32))
	issuer := &taskCloseProvider{wire: bytes.Clone(enrollment.Request.IssuerCertificate), key: issuerKey}
	quiescence := &quiescenceProvider{old: issuer}
	o.TaskIssuer, o.TaskQuiescenceIssuer = issuer, quiescence
	if configure != nil {
		configure(&o, enrollment)
	}
	b, err := New(ctx, o)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	issuer.identity, err = b.taskVerifier.VerifyCommandIssuerCertificate(issuer.wire, now)
	require.NoError(t, err)
	controlKey, _, err := b.namespace.sandboxKeys(workspace.Partition(), input.SandboxID, "read")
	require.NoError(t, err)
	control, err := raw.Get(ctx, controlKey)
	require.NoError(t, err)
	require.Len(t, control.Kvs, 1)
	destroy, ref := taskDestroyPrepare(t, b, ctx, BeginDestroyInput{SandboxID: input.SandboxID, RequestID: uuid.NewString(), ExpectedControlRevision: control.Kvs[0].ModRevision})
	outcome, err := b.CommitStage(ctx, destroy)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, outcome)

	f := &cleanupDetachedNoEffectNativeFixture{cleanupSlotTestFixture: &cleanupSlotTestFixture{b: b, raw: raw, ctx: ctx, ref: ref}, publication: published.Entry}
	f.destination = f.runtime(t, enrollment, issuer, now)
	// Complete the standalone charged slot setup before the resident actor
	// exists. Its session is actually released before the actor acquires the
	// same partition; no synchronous turn races the actor's discovery calls.
	f.session, err = b.OpenWorkerSession(ctx, "detached-noeffect-setup")
	require.NoError(t, err)
	t.Cleanup(func() { _ = b.ReleaseWorkerSession(context.Background(), f.session) })
	f.part, err = b.ClaimWorkerPartition(ctx, f.session, ref.Partition)
	require.NoError(t, err)
	f.share, err = b.AcquireWorkerBudget(ctx, f.session, WorkerBudgetClaim, 0)
	require.NoError(t, err)
	var bundle *taskClaimBundle
	f.turn(t, func(owner *workerDueOwner) {
		bundle, err = b.loadCleanupClaimBundle(owner, ref)
		require.NoError(t, err)
	})
	f.turn(t, func(owner *workerDueOwner) {
		f.evidence, err = b.loadCleanupSlotEvidence(owner, bundle)
		require.NoError(t, err)
	})
	f.occupy(t)
	require.NoError(t, b.ReleaseWorkerSession(ctx, f.session))

	// Only the original Kernel pointer is made available to the private Task3
	// entry. No local descriptor, target root, archive or local effect exists.
	b.localCleanup = &LocalCleanupOptions{MaxActive: 32, Worker: workspaceSyncLocalNativeWorker, ClaimTTL: 30 * time.Second}
	f.worker, err = b.StartDueWorker(ctx, DueWorkerOptions{WorkerID: "detached-noeffect", Partitions: []uint8{ref.Partition}, RangeSlots: []uint16{0}, ClaimSlots: []uint16{0}, RelistSlot: 0, StageTTL: 30 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = f.worker.Stop()
		workerTestReceive(t, f.worker.Done())
	})
	f.job = f.park(t)
	t.Cleanup(func() {
		if f.job == nil {
			return
		}
		if f.job.claim != nil {
			require.NoError(t, b.ReleaseTaskClaim(context.Background(), f.job.claim))
			f.job.claim = nil
		}
		// Every call below has returned and every per-call mTLS session has
		// reached EOF/Close. The fixture never starts a physical cleanup loop.
		f.job.effectsJoined = true
		if f.job.runID != "" && f.job.state != nil && !f.job.state.record.Run.Joined {
			require.NoError(t, b.joinCleanupRun(context.Background(), f.job))
		}
		b.finishCleanupJob(f.job, nil)
		f.job = nil
	})
	f.job.bundle = bundle
	f.job.evidence = f.evidence
	f.startRun(t)
	// Task3's private writer requires a genuine original capability and exact
	// native binding, not a claim reconstructed from diagnostics. This public
	// ClaimTask is deliberately not evidence of managed consumer admission.
	workerID := "noeffect-original"
	if len(claimWorker) != 0 {
		workerID = claimWorker[0]
	}
	f.job.claim, err = b.ClaimTask(f.job.ctx, ref, workerID, claimTTL)
	require.NoError(t, err)
	worker, err := workspaceSyncLocalNativeWorker.WorkspaceSyncWorkerReference()
	require.NoError(t, err)
	require.NoError(t, f.job.progress("claimed", f.job.pending("close", StageReference{}), nil))
	f.job.target = &cleanupLocalTarget{publication: published.Entry, worker: worker, destination: f.destination}
	require.NoError(t, b.cleanupCloseData(f.job))
	require.NoError(t, b.cleanupQuiesceUsers(f.job))
	f.drained, err = b.PrepareAfterDualDrain(ctx, f.job.claim, f.destination)
	require.NoError(t, err)
	require.NoError(t, f.job.progress("drained", f.job.pending("final-sync", StageReference{}), nil))
	return f
}

// Park only after the original native E observation entered the client, then
// join that exact charged owner. This pauses this job, not the resident actor:
// subsequent metadata changes must still use its original continuations.
// Setting started is test scheduling, not managed profile/admission evidence.
func (f *cleanupDetachedNoEffectNativeFixture) park(t *testing.T) *cleanupLocalJob {
	t.Helper()
	b := f.b
	key, err := b.namespace.cleanupExecutionKey(f.ref)
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	owners := make(chan *workerDueOwner, 1)
	var selected atomic.Bool
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	original := b.client.KV
	b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
		for _, op := range ops {
			if op.IsGet() && string(op.KeyBytes()) == key && selected.CompareAndSwap(false, true) {
				b.archiveLife.mu.Lock()
				owners <- f.worker.claims[0].dueOwner
				b.archiveLife.mu.Unlock()
				return true
			}
		}
		return false
	}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		close(entered)
		<-release
		return r, err
	}}
	j, err := f.worker.enqueueCleanupLocal(f.ctx, f.ref)
	require.NoError(t, err)
	workerTestReceive(t, entered)
	owner := workerTestReceive(t, owners)
	require.NotNil(t, owner)
	b.archiveLife.mu.Lock()
	j.started = true
	b.archiveLife.mu.Unlock()
	unblock()
	workerTestReceive(t, owner.done)
	f.joinActorTail(t)
	b.client.KV = original
	j.step = "physical"
	return j
}

// The original actor, original charged owner and existing run phase generate
// and persist Run/Kernel identity. The ACK hook merely parks further admission;
// it preserves the actual response and never writes or supplies native facts.
func (f *cleanupDetachedNoEffectNativeFixture) startRun(t *testing.T) {
	t.Helper()
	b, j := f.b, f.job
	key, err := b.namespace.cleanupExecutionKey(f.ref)
	require.NoError(t, err)
	owners := make(chan *workerDueOwner, 1)
	var selected atomic.Bool
	var owner *workerDueOwner
	original := b.client.KV
	b.client.KV = &faultKV{KV: original, match: func(ops []clientv3.Op) bool {
		for _, op := range ops {
			if op.IsPut() && string(op.KeyBytes()) == key && selected.CompareAndSwap(false, true) {
				b.archiveLife.mu.Lock()
				owner = f.worker.claims[0].dueOwner
				b.archiveLife.mu.Unlock()
				return true
			}
		}
		return false
	}, after: func(r *clientv3.TxnResponse, err error) (*clientv3.TxnResponse, error) {
		b.archiveLife.mu.Lock()
		j.started = true
		b.archiveLife.mu.Unlock()
		owners <- owner
		return r, err
	}}
	b.archiveLife.mu.Lock()
	j.step, j.started = "run", false
	b.archiveLife.mu.Unlock()
	f.worker.wakeCleanup()
	actual := workerTestReceive(t, owners)
	require.NotNil(t, actual)
	workerTestReceive(t, actual.done)
	f.joinActorTail(t)
	b.client.KV = original
	require.Nil(t, j.pendingProgress)
	require.NotNil(t, j.state.record.Run)
	require.Equal(t, j.runID, j.state.record.Run.ID)
	require.False(t, j.state.record.Run.Joined)
}

// owner.done joins native ownership before cleanupTurnCompleted's reply tail.
// A later queued missing Task must pass through the sole actor before it can
// finish, so this barrier joins that earlier tail as well. It does not pause
// discovery or grant exclusive ownership of the actor's session.
func (f *cleanupDetachedNoEffectNativeFixture) joinActorTail(t *testing.T) {
	t.Helper()
	missing := f.ref
	missing.TaskID = uuid.NewString()
	barrier, err := f.worker.enqueueCleanupLocal(f.ctx, missing)
	require.NoError(t, err)
	workerTestReceive(t, barrier.done)
	require.Error(t, barrier.err)
}

// The test target validates original tickets before it signs and retains any
// result. Query can return only that exact saved response; it cannot manufacture
// an effect receipt from caller fields. Actual wire EOF and server close join.
func (f *cleanupDetachedNoEffectNativeFixture) runtime(t *testing.T, enrolled p.EnrolledRuntimeEvidence, issuer *taskCloseProvider, now time.Time) *ct.Destination {
	t.Helper()
	runtimeKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, 32))
	runtimeCredential, err := p.NewManagementTLSCredential(runtimeKey, enrolled.Request.RuntimeCertificate, "runtime_receipt")
	require.NoError(t, err)
	issuerCredential, err := p.NewManagementTLSCredential(issuer.key, issuer.wire, "command_issuer")
	require.NoError(t, err)
	config, err := p.NewManagementTLSConfig(p.ManagementTLSOptions{Verifier: f.b.taskVerifier, Clock: f.b.authorityClock, Credential: runtimeCredential, PeerCertificate: issuer.wire, Server: true})
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	task, err := f.b.LoadTask(f.ctx, f.ref)
	require.NoError(t, err)
	identity := p.RuntimeIdentityContext{SandboxID: f.ref.SandboxID, WorkspaceHash: task.WorkspaceHash, Generation: task.Generation, Runtime: p.RuntimeReference(task.Runtime)}
	destination, err := ct.NewDestination(ct.DestinationOptions{Identity: identity, RuntimeCertificate: enrolled.Request.RuntimeCertificate, Credential: issuerCredential, Verifier: f.b.taskVerifier, Clock: f.b.authorityClock, Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	}})
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		defer close(done)
		closed := make(map[string][]byte)
		quiesced := make(map[string][]byte)
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(4 * time.Second))
				conn := tls.Server(raw, config)
				if conn.Handshake() != nil {
					return
				}
				request, err := ct.ReadRuntimeRequest(conn)
				if err != nil {
					return
				}
				var wire []byte
				if e := request.CloseData; e != nil {
					key := e.TicketDigest
					if e.Purpose == "task_close_data" {
						f.effectCount.Add(1)
						evidence, err := f.b.taskVerifier.VerifyTaskCloseDataTicket(e.Ticket, e.IssuerCertificate, e.Context, now)
						if err != nil || evidence.Digest() != key {
							return
						}
						wire, err = p.SignTaskDataClosedReceipt(runtimeKey, p.TaskDataClosedReceiptClaims{Version: 1, State: "data_closed", Context: e.Context, TicketDigest: key, NotBefore: evidence.NotBefore(), NotAfter: evidence.NotAfter()})
						if err != nil {
							return
						}
						closed[key] = wire
					} else {
						f.queryCount.Add(1)
						wire = closed[key]
					}
				} else if e := request.Quiesce; e != nil {
					key := e.TicketDigest
					if e.Purpose == "task_quiesce_users" {
						f.effectCount.Add(1)
						evidence, err := f.b.taskQuiescenceVerifier.VerifyTaskUserQuiescenceTicket(e.Ticket, e.IssuerCertificate, e.Context, now)
						if err != nil || evidence.Digest() != key || !bytes.Equal(closed[e.Context.CloseDataTicketDigest], e.OriginalCloseDataReceipt) {
							return
						}
						wire, err = p.SignTaskUserQuiescenceReceipt(runtimeKey, p.TaskUserQuiescenceReceiptClaims{Version: 1, State: "users_quiesced", Context: e.Context, TicketDigest: key, NotBefore: evidence.NotBefore(), NotAfter: evidence.NotAfter(), ExecutionSetDigest: strings.Repeat("a", 64)})
						if err != nil {
							return
						}
						quiesced[key] = wire
					} else {
						f.queryCount.Add(1)
						wire = quiesced[key]
					}
				}
				if len(wire) != 0 {
					_ = ct.WriteEvent(conn, ct.EventReceipt, wire)
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		workerTestReceive(t, done)
	})
	return destination
}
