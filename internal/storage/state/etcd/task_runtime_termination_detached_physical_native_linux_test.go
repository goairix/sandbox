//go:build linux && (amd64 || arm64)

package etcd

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/sys/unix"
	"k8s.io/client-go/rest"
)

// This ordinary JSON configuration mirrors the controlrunner test ELF. It
// deliberately has no TaskClaim, Run, authority DTO, native stamp or terminal
// private key. Every original capability is acquired inside this worker.
type detachedPhysicalWorkerSetup struct {
	Version uint32

	// Native store configuration; no task capability crosses this boundary.
	Endpoints []string
	Identity  Identity

	// The operator signer is restricted to the original source/command roles.
	Binding             p.TrustBinding
	Root                ed25519.PublicKey
	IssuerKey           ed25519.PrivateKey
	IssuerCertificate   []byte
	ObserverCertificate []byte

	// Source configuration is fixed before creation and the first signature.
	Directory   string
	Coordinator string
	Image       string
	Contract    string

	// Runtime/provider locations only narrow their independently verified peers.
	NodeUID         string
	NodeBootID      string
	ObserverAddress string
	ProviderAddress string
	BudgetID        string
	ProviderCA      []byte
}

type detachedPhysicalPhase struct {
	Phase string

	// Native source attribution returned by the worker's original APIs.
	IntentID      string
	SandboxID     string
	WorkspaceHash string
	Creation      CreationClaimReference
	Dispatch      RuntimeDispatchRecord

	// The parent forwards only original signed runtime material.
	Preparation    []byte
	Proof          []byte
	Enrollment     p.EnrolledRuntimeEvidence
	RuntimeAddress string

	Task TaskReference
}

func detachedPhysicalRead(t *testing.T, conn *net.UnixConn, phase string) detachedPhysicalPhase {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(90*time.Second)))
	wire, err := ct.ReadNodeObserverFrame(conn)
	require.NoError(t, err)
	var result detachedPhysicalPhase
	require.NoError(t, json.Unmarshal(wire, &result))
	require.Equal(t, phase, result.Phase)
	return result
}

func detachedPhysicalWrite(t *testing.T, conn *net.UnixConn, phase detachedPhysicalPhase) {
	t.Helper()
	require.NoError(t, conn.SetWriteDeadline(time.Now().Add(10*time.Second)))
	wire, err := json.Marshal(phase)
	require.NoError(t, err)
	require.NoError(t, ct.WriteNodeObserverFrame(conn, wire))
}

// TestDetachedRuntimeTerminationPhysicalWorkerChild is launched only by the
// owned controlrunner fixture. The existing TestMain HTTP-cluster mode remains
// off: this process bootstraps its own genuine zero-cap worker boundary and
// connects to the same TLS3 cluster as the physical observer.
func TestDetachedRuntimeTerminationPhysicalWorkerChild(t *testing.T) {
	path := os.Getenv("SANDBOX_DETACHED_RUNTIME_PHYSICAL_CHILD_SETUP")
	if path == "" {
		return
	}
	require.Equal(t, "1", os.Getenv("SANDBOX_DETACHED_RUNTIME_PHYSICAL_NATIVE"))
	require.Empty(t, os.Getenv("SANDBOX_WORKSPACE_SYNC_LOCAL_NATIVE"))
	wire, err := os.ReadFile(path)
	require.NoError(t, err)
	var setup detachedPhysicalWorkerSetup
	require.NoError(t, json.Unmarshal(wire, &setup))
	require.EqualValues(t, 1, setup.Version)
	// Identity deliberately omits RestoreEpoch from JSON; restore the ordinary
	// expected binding here, then New independently verifies the native epoch.
	setup.Identity.RestoreEpoch = setup.Binding.RestoreEpoch
	require.Len(t, setup.Endpoints, 3)
	ca, err := os.ReadFile(filepath.Join(setup.Directory, "ca.pem"))
	require.NoError(t, err)
	cert, err := tls.LoadX509KeyPair(filepath.Join(setup.Directory, "server.pem"), filepath.Join(setup.Directory, "server-key.pem"))
	require.NoError(t, err)
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: setup.Coordinator, Net: "unix"})
	require.NoError(t, err)
	defer conn.Close()

	// Read protected operator configuration first, then permanently discard
	// setup privilege before producing any source, Claim, job or effect.
	for capability := uintptr(0); capability <= 40; capability++ {
		_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, capability, 0, 0, 0, 0)
		require.Zero(t, errno)
	}
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_SETGROUPS, 0, 0, 0)
	require.Zero(t, errno)
	_, _, errno = syscall.AllThreadsSyscall(unix.SYS_SETRESGID, 1000, 1000, 1000)
	require.Zero(t, errno)
	_, _, errno = syscall.AllThreadsSyscall(unix.SYS_SETRESUID, 1000, 1000, 1000)
	require.Zero(t, errno)
	kernel, err := launcher.BootstrapWorkspaceSyncWorker()
	require.NoError(t, err)
	require.NoError(t, kernel.ValidateCurrent())
	status, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Cap") {
			require.Equal(t, "0000000000000000", strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
		}
	}
	require.Contains(t, string(status), "NoNewPrivs:\t1\n")
	t.Logf("original detached worker pid=%d uid=%d gid=%d caps0/NNP Kernel bootstrap authenticated", os.Getpid(), os.Geteuid(), os.Getegid())

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	t.Cleanup(cancel)
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca))
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{cert}}
	b, raw := detachedPhysicalBackend(t, ctx, setup, kernel, tlsConfig)
	source, creation := detachedPhysicalCreateSource(t, ctx, b)
	creationReleased := false
	t.Cleanup(func() {
		if !creationReleased {
			require.NoError(t, b.ReleaseCreationClaim(context.Background(), creation))
		}
	})
	detachedPhysicalWrite(t, conn, source)
	bound := detachedPhysicalRead(t, conn, "bind")
	result, err := b.BindRuntime(ctx, creation, bound.Preparation)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	mounted, err := b.ConsumeRuntimeMount(ctx, creation)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, mounted.Outcome)
	detachedPhysicalWrite(t, conn, detachedPhysicalPhase{Phase: "mount-consumed"})
	enrolled := detachedPhysicalRead(t, conn, "enrolled")
	published, err := b.PublishRuntimeEnrolled(ctx, creation, enrolled.Proof, enrolled.Enrollment)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, published.Outcome)
	require.NotNil(t, published.Entry)
	require.NoError(t, b.ReleaseCreationClaim(ctx, creation))
	creationReleased = true
	task := detachedPhysicalDestroy(t, ctx, b, raw, source)
	detachedPhysicalWrite(t, conn, detachedPhysicalPhase{Phase: "task-ready", Task: task})
	_ = detachedPhysicalRead(t, conn, "run")
	f := detachedPhysicalMetadataRun(t, ctx, b, raw, task, published.Entry, enrolled.RuntimeAddress, kernel)
	detachedPhysicalTerminate(t, f, setup)
	detachedPhysicalWrite(t, conn, detachedPhysicalPhase{Phase: "cleanup-complete"})
}

// The full optional configuration is validated by New before source creation.
// No dependency is patched afterward. LocalCleanup supplies the original
// Kernel/credentials for the test-scheduled private component, without claiming
// that the unchanged local-only managed admission accepts detached snapshots.
func detachedPhysicalBackend(t *testing.T, ctx context.Context, s detachedPhysicalWorkerSetup, kernel *launcher.KernelBoundary, tlsConfig *tls.Config) (*Backend, *clientv3.Client) {
	t.Helper()
	namespace, err := NewNamespace(s.Identity.Prefix, s.Identity.AuthorityID, s.Identity.Cell)
	require.NoError(t, err)
	require.Equal(t, s.Binding.Namespace, namespace.Root())
	clock := testAuthorityClock(func(ctx context.Context) (ClockObservation, error) {
		return ClockObservation{UTC: time.Now().UTC()}, ctx.Err()
	})
	verifier, err := p.NewManagementVerifier(s.Binding, []ed25519.PublicKey{s.Root})
	require.NoError(t, err)
	issuerIdentity, err := verifier.VerifyCommandIssuerCertificate(s.IssuerCertificate, time.Now().UTC())
	require.NoError(t, err)
	issuer := &taskCloseProvider{wire: bytes.Clone(s.IssuerCertificate), key: s.IssuerKey, identity: issuerIdentity}
	quiescence := &quiescenceProvider{old: issuer}
	archive := &archiveNativeIssuer{old: issuer}
	termination := &detachedTerminationFixtureIssuer{key: s.IssuerKey, wire: bytes.Clone(s.IssuerCertificate), identity: issuerIdentity}
	credential, err := p.NewManagementTLSCredential(s.IssuerKey, s.IssuerCertificate, "command_issuer")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, credential.Close()) })
	observerCredential, err := p.NewNodeObserverTLSCredential(s.IssuerKey, s.IssuerCertificate, false)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, observerCredential.Close()) })
	kube := &rest.Config{Host: s.ProviderAddress, TLSClientConfig: rest.TLSClientConfig{CAData: s.ProviderCA}}
	provider, err := k.NewTaskRuntimeCleanupClient(k.TaskRuntimeCleanupOptions{Config: kube, Namespace: "runtimes", IncludeCilium: true, MaxActive: 1, RequestTimeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, provider.Close()) })
	bridge, err := k.NewEnrolledBridgeClient(k.EnrolledBridgeOptions{Config: kube, Namespace: "runtimes", MaxActive: 1, RequestTimeout: time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, bridge.Close()) })
	dir, err := os.MkdirTemp("/tmp", "detached-physical-worker-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	for _, path := range []string{"spool", "target"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, path), 0700))
	}
	recovery := recoveryTestOptions(filepath.Join(dir, "spool"), 1)
	options := Options{
		Endpoints: s.Endpoints, Namespace: namespace, Identity: s.Identity, TLS: tlsConfig, RequestTimeout: time.Second,
		PublicationTrust:    &RuntimePublicationTrust{AuthorityID: s.Binding.AuthorityID, Target: s.Binding.Target, Roots: []ed25519.PublicKey{s.Root}},
		EnrolledPublication: &EnrolledPublicationOptions{ContractDigest: s.Contract, MaxActive: 4},
		DetachedCleanup:     &DetachedCleanupOptions{PinnedPlainImage: s.Image},
		Clock:               clock, WorkerControl: &WorkerControlOptions{SessionTTL: 60 * time.Second},
		TaskIssuer: issuer, TaskQuiescenceIssuer: quiescence, TaskWorkspaceArchiveIssuer: archive, TaskRuntimeTerminationIssuer: termination,
		TaskWorkspaceArchiveRecovery:     &recovery,
		TaskWorkspaceSyncRecyclingBudget: &TaskWorkspaceSyncBudgetOptions{BudgetID: s.BudgetID, SlotsPerPartition: 1, MaxIntentBytes: 16384},
		LocalRuntimeRelease:              &LocalRuntimeReleaseOptions{MaxActive: 1, Timeout: 30 * time.Second, Provider: provider},
		LocalCleanup: &LocalCleanupOptions{
			Worker: kernel, Bridge: bridge, Credential: credential, ObserverCredential: observerCredential,
			Nodes:    []LocalCleanupNode{{Name: "node-a", UID: s.NodeUID, BootID: s.NodeBootID, ObserverAddress: s.ObserverAddress}},
			Storage:  LocalCleanupStorage{Identity: s.Identity.StorageID, Bucket: "bucket", Directory: filepath.Join(dir, "target")},
			ClaimTTL: 60 * time.Second, JobTimeout: 120 * time.Second, ArchiveTimeout: 30 * time.Second, MaxActive: 32,
		},
	}
	b, err := New(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	raw, err := clientv3.New(clientv3.Config{Endpoints: s.Endpoints, TLS: tlsConfig, DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, raw.Close()) })
	return b, raw
}

func detachedPhysicalCreateSource(t *testing.T, ctx context.Context, b *Backend) (detachedPhysicalPhase, *CreationClaim) {
	t.Helper()
	idempotency := uuid.NewString()
	workspace, err := b.DetachedIdentity("detached-physical", idempotency)
	require.NoError(t, err)
	input := AcquireIntentInput{Workspace: workspace, Principal: "detached-physical", IdempotencyKey: idempotency, RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"fixture":"detached physical source"}`), Now: time.Now().UTC(), TTL: time.Hour}
	acquired, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, acquired.Outcome)
	creation, err := b.ClaimCreation(ctx, workspace, input.IntentID, "detached-physical-source", 2*time.Minute)
	require.NoError(t, err)
	dispatch, err := b.DeclareRuntimeDispatch(ctx, creation, RuntimeDispatchInput{Kind: RuntimeDispatchCreate, Target: b.publicationTarget, Payload: input.Payload})
	require.NoError(t, err)
	return detachedPhysicalPhase{Phase: "source-prepared", IntentID: input.IntentID, SandboxID: input.SandboxID, WorkspaceHash: workspace.Hash(), Creation: creation.Reference(), Dispatch: dispatch.Entry.Record}, creation
}

func detachedPhysicalDestroy(t *testing.T, ctx context.Context, b *Backend, raw *clientv3.Client, source detachedPhysicalPhase) TaskReference {
	t.Helper()
	controlKey, _, err := b.namespace.sandboxKeys(source.Creation.Partition, source.SandboxID, "read")
	require.NoError(t, err)
	control, err := raw.Get(ctx, controlKey)
	require.NoError(t, err)
	require.Len(t, control.Kvs, 1)
	stage, ref := taskDestroyPrepare(t, b, ctx, BeginDestroyInput{SandboxID: source.SandboxID, RequestID: uuid.NewString(), ExpectedControlRevision: control.Kvs[0].ModRevision})
	committed, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, committed)
	return ref
}

// Reuse B's accepted original queue/charged owner/Run machinery, with no copy
// of its codec or writer. Parking controls scheduling only; genuine ClaimTask
// and the live protected supervisor produce all actual drain/source evidence.
func detachedPhysicalMetadataRun(t *testing.T, ctx context.Context, b *Backend, raw *clientv3.Client, ref TaskReference, publication *RuntimeEnrolledPublicationEntry, address string, kernel *launcher.KernelBoundary) *cleanupDetachedNoEffectNativeFixture {
	t.Helper()
	f := &cleanupDetachedNoEffectNativeFixture{cleanupSlotTestFixture: &cleanupSlotTestFixture{b: b, raw: raw, ctx: ctx, ref: ref}, publication: publication}
	var err error
	f.session, err = b.OpenWorkerSession(ctx, "detached-physical-setup")
	require.NoError(t, err)
	sessionReleased := false
	t.Cleanup(func() {
		if !sessionReleased {
			require.NoError(t, b.ReleaseWorkerSession(context.Background(), f.session))
		}
	})
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
	sessionReleased = true
	f.worker, err = b.StartDueWorker(ctx, DueWorkerOptions{WorkerID: "detached-physical", Partitions: []uint8{ref.Partition}, RangeSlots: []uint16{0}, ClaimSlots: []uint16{0}, RelistSlot: 0, StageTTL: 30 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, f.worker.Stop())
		workerTestReceive(t, f.worker.Done())
	})
	f.job = f.park(t)
	t.Cleanup(func() {
		if f.job.claim != nil {
			require.NoError(t, b.ReleaseTaskClaim(context.Background(), f.job.claim))
			f.job.claim = nil
		}
		// Every synchronous runtime/provider call has returned, its original
		// sessions joined and the termination destination closed before this
		// cleanup. Only then may the original E join.
		f.job.effectsJoined = true
		if f.job.state != nil && f.job.state.record.Run != nil && !f.job.state.record.Run.Joined {
			require.NoError(t, b.joinCleanupRun(context.Background(), f.job))
		}
		b.finishCleanupJob(f.job, nil)
	})
	f.job.bundle, f.job.evidence = bundle, f.evidence
	f.startRun(t)
	f.job.claim, err = b.ClaimTask(f.job.ctx, ref, "detached-physical-original", 60*time.Second)
	require.NoError(t, err)
	activation, err := b.enrolledVerifier.VerifyNodeEnrollmentRequest(publication.Enrollment.Request, time.Now().UTC())
	require.NoError(t, err)
	f.destination, err = ct.NewDestination(ct.DestinationOptions{Identity: activation.Identity(), RuntimeCertificate: activation.RuntimeCertificate(), Credential: b.localCleanup.Credential, Verifier: b.enrolledVerifier, Clock: b.authorityClock, Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", address)
	}})
	require.NoError(t, err)
	worker, err := kernel.WorkspaceSyncWorkerReference()
	require.NoError(t, err)
	f.job.target = &cleanupLocalTarget{publication: publication, worker: worker, destination: f.destination}
	require.NoError(t, f.job.progress("claimed", f.job.pending("close", StageReference{}), nil))
	require.NoError(t, b.cleanupCloseData(f.job))
	require.NoError(t, b.cleanupQuiesceUsers(f.job))
	f.drained, err = b.PrepareAfterDualDrain(ctx, f.job.claim, f.destination)
	require.NoError(t, err)
	require.NoError(t, f.job.progress("drained", f.job.pending("final-sync", StageReference{}), nil))
	require.NoError(t, b.RenewTaskClaim(ctx, f.job.claim))
	draft, err := b.prepareCleanupDetachedNoEffect(ctx, f.job, f.drained, f.destination)
	require.NoError(t, err)
	settlement, err := b.commitCleanupDetachedNoEffect(ctx, f.job, draft)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, settlement.Outcome)
	require.NotNil(t, settlement.Entry)
	t.Logf("actual protected-runtime dual drain and original native settlement create=%d", settlement.Entry.kv.CreateRevision)
	return f
}

// Only actual writes to the two production termination keys and the exact
// identity-point guard have the flat wire shape measured here. Counts describe
// Commit submissions; they are not a claim about transport retry dispatches.
func detachedPhysicalTerminate(t *testing.T, f *cleanupDetachedNoEffectNativeFixture, s detachedPhysicalWorkerSetup) {
	t.Helper()
	b := f.b
	publication := f.publication
	destination, err := ct.NewTerminationDestination(ct.TerminationDestinationOptions{Address: s.ObserverAddress, Binding: s.Binding, Roots: []ed25519.PublicKey{s.Root}, Clock: b.authorityClock, Credential: b.localCleanup.ObserverCredential, ObserverCertificate: s.ObserverCertificate, NodeUID: publication.Record.NodeUID, NodeBootID: publication.Record.NodeBootID, EnrollmentDigest: publication.Record.EnrollmentDigest})
	require.NoError(t, err)
	defer destination.Close()
	require.NoError(t, b.RenewTaskClaim(f.ctx, f.job.claim))
	intentKey, providerKey, releaseKey, err := b.namespace.localRuntimeReleaseKeys(f.ref)
	require.NoError(t, err)
	before, err := b.readEnrolledPoints(f.ctx, []string{intentKey, providerKey, releaseKey}, []int{16384, 16384, 8192})
	require.NoError(t, err)
	for _, point := range before {
		require.Nil(t, point, "no test-generated provider/termination/release KV")
	}
	original := b.client.KV
	var commits, providerWrites, intentWrites, guards atomic.Int64
	b.client.KV = &finalSyncRecordingKV{KV: original, commit: func(ctx context.Context, cmps []clientv3.Cmp, yes, no []clientv3.Op) (*clientv3.TxnResponse, error) {
		commits.Add(1)
		var request *pb.TxnRequest
		kind := ""
		if len(yes) == 1 && len(no) == 0 && yes[0].IsPut() && (string(yes[0].KeyBytes()) == intentKey || string(yes[0].KeyBytes()) == providerKey) {
			key := string(yes[0].KeyBytes())
			request = &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoWrites([]Write{{Key: key, Value: yes[0].ValueBytes()}})}
			kind = "provider"
			if key == intentKey {
				kind = "termination"
				intentWrites.Add(1)
			} else {
				providerWrites.Add(1)
			}
		} else if len(cmps) >= 54 && len(yes) == 1 && len(no) == 0 && yes[0].IsGet() && string(yes[0].KeyBytes()) == b.identityKey {
			guards.Add(1)
			kind = "guard"
			request = &pb.TxnRequest{Compare: taskProtoComparisons(cmps), Success: taskProtoPoints([]string{b.identityKey})}
		}
		if request != nil {
			require.LessOrEqual(t, len(request.Compare)+len(request.Success)+len(request.Failure), 64)
			require.LessOrEqual(t, request.Size(), 128<<10)
		}
		response, err := original.Txn(ctx).If(cmps...).Then(yes...).Else(no...).Commit()
		if request != nil && response != nil {
			replyBytes := (*pb.TxnResponse)(response).Size()
			require.LessOrEqual(t, replyBytes, 256<<10)
			t.Logf("actual detached physical %s nodes=%d request_bytes=%d reply_bytes=%d", kind, len(request.Compare)+len(request.Success)+len(request.Failure), request.Size(), replyBytes)
		}
		return response, err
	}}
	defer func() { b.client.KV = original }()
	result, err := b.terminateTaskRuntimeDetached(f.ctx, f.job, f.job.claim, destination, nil)
	require.NoError(t, err)
	require.Equal(t, OutcomeCommitted, result.Outcome)
	require.NotNil(t, result.Entry)
	require.Equal(t, "cleanup-complete", result.Entry.Phase)
	require.EqualValues(t, 15, providerWrites.Load(), "initial record plus intent/complete for each of seven actual slots")
	require.EqualValues(t, 3, intentWrites.Load(), "reserved, terminated and cleanup-complete")
	require.Positive(t, guards.Load())
	points, err := b.readEnrolledPoints(f.ctx, []string{intentKey, providerKey, releaseKey}, []int{16384, 16384, 8192})
	require.NoError(t, err)
	require.Nil(t, points[2], "owner release is outside this component")
	intent, err := decodeLocalRuntimeTermination(points[0])
	require.NoError(t, err)
	require.EqualValues(t, 2, intent.Version)
	require.NotNil(t, intent.DetachedEffects)
	query, err := localRuntimeTerminationQuery(intent, points[0])
	require.NoError(t, err)
	proof, err := b.enrolledVerifier.VerifyNodeTerminationProof(intent.TerminalProof, s.ObserverCertificate, query)
	require.NoError(t, err)
	require.True(t, proof.PID1Dead)
	require.True(t, proof.CgroupEmpty)
	provider, err := decodeTaskRuntimeProviderCleanup(points[1])
	require.NoError(t, err)
	require.True(t, localRuntimeProviderMatches(provider, intent, b.detachedCleanup.contractDigest))
	for i, slot := range provider.Slots {
		require.Equal(t, "complete", slot.State, "physical fixture configures every distinct slot; no skipped policies")
		require.NotEmpty(t, slot.EffectID)
		require.NotNil(t, slot.Claim)
		require.Equal(t, localRuntimeClaimWire(f.job.claim), *slot.Claim)
		t.Logf("actual provider slot=%d state=%s UID=%s RV=%s effect=%s", i, slot.State, slot.UID, slot.ResourceVersion, slot.EffectID)
	}
	issuer := b.taskRuntimeTerminationIssuer.(*detachedTerminationFixtureIssuer)
	require.EqualValues(t, 1, issuer.certificates.Load())
	require.EqualValues(t, 1, issuer.signatures.Load())
	t.Logf("actual physical Commit submissions=%d provider_writes=%d termination_writes=%d guards=%d; transport dispatch/retries unmeasured", commits.Load(), providerWrites.Load(), intentWrites.Load(), guards.Load())
}
