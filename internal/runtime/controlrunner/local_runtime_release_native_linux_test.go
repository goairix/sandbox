//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/goairix/sandbox/internal/nodeobserver"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

type localReleaseNativeSetup struct {
	Binding                                                               p.TrustBinding
	Root                                                                  ed25519.PublicKey
	IssuerKey                                                             ed25519.PrivateKey
	IssuerCertificate                                                     []byte
	ObserverCertificate                                                   []byte
	RuntimeCertificate                                                    []byte
	Runtime                                                               p.RuntimeReference
	Identity                                                              state.Identity
	WorkspaceProvider, WorkspaceStorage, WorkspaceBucket, WorkspacePrefix string
	SandboxID, IntentID, Contract, NodeUID, NodeBootID                    string
	Directory, Coordinator, EnrollmentSocket, CgroupMount                 string
	RuntimeAddress, ObserverAddress, ProviderAddress, BudgetID            string
	ControlRevision                                                       int64
	Generation                                                            int64
	Fault                                                                 string
	EtcdAddress                                                           string
}

type localReleaseNativeReply struct {
	Birth      p.BirthContext
	Address    string
	Proof      []byte
	Enrollment p.EnrolledRuntimeEvidence
}

// This fresh PID1 owns its genuine Supervisor. The test never supplies a
// physical-success callback; the observer must kill and observe this process.
func TestLocalRuntimeReleaseNativeSupervisor(t *testing.T) {
	path := os.Getenv("LOCAL_RELEASE_SUPERVISOR_SETUP")
	if path == "" {
		return
	}
	require.Equal(t, 1, os.Getpid())
	var s localReleaseNativeSetup
	wire, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(wire, &s))
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	require.NoError(t, unix.Unmount(s.CgroupMount, 0))
	require.NoError(t, unix.Unmount("/proc", unix.MNT_DETACH))
	require.NoError(t, unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""))
	// /workspace is the private empty runtime filesystem, separate from sync target.
	require.NoError(t, unix.Mount("tmpfs", "/workspace", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=1048576,mode=0700,uid=1000,gid=1000"))
	for _, cap := range []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SYS_PTRACE} {
		_, _, e := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, cap, 0, 0, 0, 0)
		require.Zero(t, e)
	}
	header := &unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := &[2]unix.CapUserData{{Permitted: 0x1e0, Effective: 0x1e0}}
	_, _, e := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(header)), uintptr(unsafe.Pointer(data)), 0)
	runtime.KeepAlive(header)
	runtime.KeepAlive(data)
	require.Zero(t, e)
	kernel, err := launcher.BootstrapPID1()
	require.NoError(t, err)
	t.Logf("actual genuine PID1 boundary %+v", kernel.Snapshot())
	verifier, err := p.NewManagementVerifier(s.Binding, []ed25519.PublicKey{s.Root})
	require.NoError(t, err)
	supervisor, err := NewEnrolledSupervisor(SupervisorOptions{Kernel: kernel, UID: 1000, GID: 1000, ContractDigest: s.Contract, Verifier: verifier, Clock: nativeFixtureClock{}, JournalDirectory: filepath.Join(s.Directory, "supervisor-journal"), Executable: "/fixture/monitor"}, NodeEnrollmentOptions{SocketPath: s.EnrollmentSocket, NodeUID: s.NodeUID, NodeBootID: s.NodeBootID})
	require.NoError(t, err)
	defer supervisor.Close()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: s.Coordinator, Net: "unix"})
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(60*time.Second)))
	require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, localReleaseNativeReply{Birth: supervisor.Birth()})))
	wire, err = ct.ReadNodeObserverFrame(conn)
	require.NoError(t, err)
	var command enrolledNativeCommand
	require.NoError(t, json.Unmarshal(wire, &command))
	activation, err := verifier.VerifyTargetActivation(command.Request.Activation, command.Request.RuntimeCertificate, command.Request.IssuerCertificate, supervisor.Birth(), time.Now().UTC())
	require.NoError(t, err)
	require.NoError(t, supervisor.Activate(context.Background(), activation))
	proof, err := supervisor.EnrolledPublicationProof(context.Background(), command.Certificate, command.Claim)
	require.NoError(t, err)
	enrollment, err := supervisor.Enrollment()
	require.NoError(t, err)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, localReleaseNativeReply{Address: listener.Addr().String(), Proof: proof, Enrollment: enrollment})))
	err = supervisor.Serve(context.Background(), listener)
	t.Fatalf("genuine Supervisor exited before observer physical termination: %v", err)
}

type localReleaseNativeIssuer struct {
	key         ed25519.PrivateKey
	certificate []byte
	identity    p.CommandIssuerIdentity
}

func (s localReleaseNativeIssuer) Certificate(ctx context.Context) ([]byte, error) {
	return append([]byte(nil), s.certificate...), ctx.Err()
}
func (s localReleaseNativeIssuer) SignCloseData(ctx context.Context, digest string, c p.TaskCloseDataTicketClaims) ([]byte, error) {
	if ctx.Err() != nil || digest != s.identity.Digest() {
		return nil, errors.New("issuer mismatch")
	}
	return p.SignTaskCloseDataTicket(s.key, s.identity, c)
}
func (s localReleaseNativeIssuer) SignQuiesceUsers(ctx context.Context, digest string, c p.TaskUserQuiescenceTicketClaims) ([]byte, error) {
	if ctx.Err() != nil || digest != s.identity.Digest() {
		return nil, errors.New("issuer mismatch")
	}
	return p.SignTaskUserQuiescenceTicket(s.key, s.identity, c)
}
func (s localReleaseNativeIssuer) SignWorkspaceArchive(ctx context.Context, digest string, c p.TaskWorkspaceArchiveTicketClaims) ([]byte, error) {
	if ctx.Err() != nil || digest != s.identity.Digest() {
		return nil, errors.New("issuer mismatch")
	}
	return p.SignTaskWorkspaceArchiveTicket(s.key, s.identity, c)
}
func (s localReleaseNativeIssuer) SignRuntimeTermination(ctx context.Context, digest string, c p.TaskRuntimeTerminationTicketClaims) ([]byte, error) {
	if ctx.Err() != nil || digest != s.identity.Digest() {
		return nil, errors.New("issuer mismatch")
	}
	return p.SignTaskRuntimeTerminationTicket(s.key, s.identity, c)
}

func TestLocalRuntimeReleaseNativeWorker(t *testing.T) {
	path := os.Getenv("LOCAL_RELEASE_WORKER_SETUP")
	if path == "" {
		return
	}
	var s localReleaseNativeSetup
	wire, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(wire, &s))
	// Identity deliberately excludes the separately provisioned restore epoch
	// from its JSON. The root-issued binding carries the exact setup epoch.
	s.Identity.RestoreEpoch = s.Binding.RestoreEpoch
	ca, err := os.ReadFile(filepath.Join(s.Directory, "ca.pem"))
	require.NoError(t, err)
	certPEM, err := os.ReadFile(filepath.Join(s.Directory, "server.pem"))
	require.NoError(t, err)
	keyPEM, err := os.ReadFile(filepath.Join(s.Directory, "server-key.pem"))
	require.NoError(t, err)
	// This dedicated child alone drops every bounding bit and all credentials.
	for cap := uintptr(0); cap <= 40; cap++ {
		_, _, e := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, cap, 0, 0, 0, 0)
		require.Zero(t, e)
	}
	_, _, e := syscall.AllThreadsSyscall(unix.SYS_SETGROUPS, 0, 0, 0)
	require.Zero(t, e)
	_, _, e = syscall.AllThreadsSyscall(unix.SYS_SETRESGID, 1000, 1000, 1000)
	require.Zero(t, e)
	_, _, e = syscall.AllThreadsSyscall(unix.SYS_SETRESUID, 1000, 1000, 1000)
	require.Zero(t, e)
	worker, err := launcher.BootstrapWorkspaceSyncWorker()
	require.NoError(t, err)
	t.Logf("actual zero-cap local worker boundary %+v", worker.Snapshot())
	// Create the writable recovery tree as the actual zero-cap worker; the
	// fixture does not add CAP_CHOWN or make observer directories user-owned.
	s.Directory, err = os.MkdirTemp("/tmp", "local-release-worker-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(s.Directory)) })
	for _, name := range []string{"spool", "target"} {
		require.NoError(t, os.Mkdir(filepath.Join(s.Directory, name), 0700))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	pool := x509.NewCertPool()
	require.True(t, pool.AppendCertsFromPEM(ca))
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: []tls.Certificate{cert}}
	ns, err := state.NewNamespace("/sandbox", "observer", "v1")
	require.NoError(t, err)
	verifier, err := p.NewManagementVerifier(s.Binding, []ed25519.PublicKey{s.Root})
	require.NoError(t, err)
	i, err := verifier.VerifyCommandIssuerCertificate(s.IssuerCertificate, time.Now().UTC())
	require.NoError(t, err)
	issuer := localReleaseNativeIssuer{s.IssuerKey, s.IssuerCertificate, i}
	provider, err := k.NewTaskRuntimeCleanupClient(k.TaskRuntimeCleanupOptions{Config: &rest.Config{Host: s.ProviderAddress}, Namespace: "runtimes", RequestTimeout: 2 * time.Second, MaxActive: 1, IncludeCilium: strings.HasPrefix(s.Fault, "http-")})
	require.NoError(t, err)
	defer func() { require.NoError(t, provider.Close()) }()
	workspace, err := state.NewWorkspaceIdentity(s.WorkspaceProvider, s.WorkspaceStorage, s.WorkspaceBucket, s.WorkspacePrefix)
	require.NoError(t, err)
	plan, err := state.NewTaskWorkspaceSyncPlan(state.TaskWorkspaceSyncPlanOptions{Workspace: workspace})
	require.NoError(t, err)
	o := state.Options{Endpoints: []string{"https://127.0.0.1:2379"}, Namespace: ns, Identity: s.Identity, TLS: tlsConfig, RequestTimeout: 2 * time.Second, PublicationTrust: &state.RuntimePublicationTrust{AuthorityID: s.Binding.AuthorityID, Target: s.Binding.Target, Roots: []ed25519.PublicKey{s.Root}}, Clock: nativeFixtureClock{}, EnrolledPublication: &state.EnrolledPublicationOptions{ContractDigest: s.Contract, MaxActive: 2}, TaskIssuer: issuer, TaskQuiescenceIssuer: issuer, TaskWorkspaceArchiveIssuer: issuer, TaskRuntimeTerminationIssuer: issuer, LocalRuntimeRelease: &state.LocalRuntimeReleaseOptions{MaxActive: 1, Timeout: 30 * time.Second, Provider: provider}, TaskWorkspaceArchiveRecovery: &state.TaskWorkspaceArchiveRecoveryOptions{TaskWorkspaceArchiveSpoolOptions: state.TaskWorkspaceArchiveSpoolOptions{Directory: filepath.Join(s.Directory, "spool"), Jobs: 1, JobBytes: 1 << 20, TotalBytes: 1 << 20, Entries: 16, JobIndexBytes: 1 << 20, TotalIndexBytes: 1 << 20}}, TaskWorkspaceSyncRecyclingBudget: &state.TaskWorkspaceSyncBudgetOptions{BudgetID: s.BudgetID, SlotsPerPartition: 1, MaxIntentBytes: 16384}}
	if s.EtcdAddress != "" {
		o.Endpoints = []string{s.EtcdAddress}
	}
	b, err := state.New(ctx, o)
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	credential, err := p.NewManagementTLSCredential(s.IssuerKey, s.IssuerCertificate, "command_issuer")
	require.NoError(t, err)
	defer credential.Close()
	destination, err := ct.NewDestination(ct.DestinationOptions{Identity: p.RuntimeIdentityContext{SandboxID: s.SandboxID, WorkspaceHash: workspace.Hash(), Generation: s.Generation, Runtime: s.Runtime}, RuntimeCertificate: s.RuntimeCertificate, Credential: credential, Verifier: verifier, Clock: nativeFixtureClock{}, Dial: func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", s.RuntimeAddress)
	}})
	require.NoError(t, err)
	stage, task, err := b.PrepareDestroy(ctx, state.BeginDestroyInput{SandboxID: s.SandboxID, RequestID: uuid.NewString(), ExpectedControlRevision: s.ControlRevision}, time.Minute)
	require.NoError(t, err)
	outcome, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, outcome)
	claim, err := b.ClaimTask(ctx, task, "fullchain-archive", time.Minute)
	require.NoError(t, err)
	closeData, err := b.PrepareTaskCloseData(ctx, claim)
	require.NoError(t, err)
	_, err = b.DeliverTaskCloseData(ctx, closeData.Prepared, destination)
	require.NoError(t, err)
	quiesce, err := b.PrepareTaskQuiescence(ctx, claim, destination)
	require.NoError(t, err)
	_, err = b.DeliverTaskQuiescence(ctx, quiesce.Prepared, destination)
	require.NoError(t, err)
	drained, err := b.PrepareAfterDualDrain(ctx, claim, destination)
	require.NoError(t, err)
	_, err = b.ReserveTaskWorkspaceSync(ctx, claim, drained, destination, plan)
	require.NoError(t, err)
	archive, err := b.PrepareTaskWorkspaceArchive(ctx, claim, drained, destination, 90*time.Second)
	require.NoError(t, err)
	received, err := b.ReceiveTaskWorkspaceArchive(ctx, archive.Prepared)
	require.NoError(t, err)
	slot, err := b.BindTaskWorkspaceArchiveLocal(ctx, received, plan, worker, filepath.Join(s.Directory, "target"))
	require.NoError(t, err)
	require.NoError(t, b.ReleaseTaskClaim(ctx, claim))
	require.NoError(t, b.Close())
	b, err = state.New(ctx, o)
	require.NoError(t, err)
	claim, err = b.ClaimTask(ctx, task, "fullchain-finalize", time.Minute)
	require.NoError(t, err)
	finalized, err := b.FinalizeTaskWorkspaceSyncLocal(ctx, claim, slot, destination, worker)
	require.NoError(t, err)
	require.True(t, finalized.LocalComplete)
	t.Logf("actual public empty-tree local finalization complete: slot=%d", slot)
	observerCredential, err := p.NewNodeObserverTLSCredential(s.IssuerKey, s.IssuerCertificate, false)
	require.NoError(t, err)
	defer observerCredential.Close()
	termination, err := ct.NewTerminationDestination(ct.TerminationDestinationOptions{Address: s.ObserverAddress, Binding: s.Binding, Roots: []ed25519.PublicKey{s.Root}, Clock: nativeFixtureClock{}, Credential: observerCredential, ObserverCertificate: s.ObserverCertificate, NodeUID: s.NodeUID, NodeBootID: s.NodeBootID, EnrollmentDigest: enrollmentDigestForRelease(t, b, ctx, workspace, s.IntentID)})
	require.NoError(t, err)
	defer termination.Close()
	if strings.HasPrefix(s.Fault, "enrollment-") {
		localReleaseEnrollmentRefusal(t, ctx, b, claim, termination, s, tlsConfig, ns, workspace, task)
		return
	}
	if strings.HasPrefix(s.Fault, "http-") {
		localReleaseUnknownEffect(t, ctx, b, claim, termination, s, tlsConfig, ns, task)
		return
	}
	terminated, err := b.TerminateTaskRuntimeLocal(ctx, claim, termination)
	if s.Fault == "ack-reserved" {
		require.Error(t, err)
		require.Equal(t, state.OutcomeUnknown, terminated.Outcome)
		original, readErr := b.LoadTaskRuntimeTermination(ctx, task)
		require.NoError(t, readErr)
		require.Equal(t, "reserved", original.Phase)
		repeated, repeatErr := b.TerminateTaskRuntimeLocal(ctx, claim, termination)
		require.NoError(t, repeatErr)
		require.Equal(t, state.OutcomeUnknown, repeated.Outcome)
		require.Equal(t, original, repeated.Entry)
		require.NoError(t, b.ReleaseTaskClaim(ctx, claim))
		claim, err = b.ClaimTask(ctx, task, "reserved-no-proof", time.Minute)
		require.NoError(t, err)
		repeated, err = b.TerminateTaskRuntimeLocal(ctx, claim, termination)
		require.Error(t, err)
		require.Equal(t, state.OutcomeUnknown, repeated.Outcome)
		after, readErr := b.LoadTaskRuntimeTermination(ctx, task)
		require.NoError(t, readErr)
		require.Equal(t, original, after)
		release, releaseErr := b.ReleaseTaskWorkspaceOwnerLocal(ctx, claim)
		require.Error(t, releaseErr)
		require.Equal(t, state.OutcomeUnknown, release.Outcome)
		require.NoError(t, b.ReleaseTaskClaim(ctx, claim))
		t.Log("actual reserve ACK loss: same attempt read only; fresh claim preserved original Current; no proof, no release")
		return
	}
	if s.Fault == "ack-cleanup-complete" {
		require.Error(t, err)
		require.Equal(t, state.OutcomeUnknown, terminated.Outcome)
		terminated, err = b.TerminateTaskRuntimeLocal(ctx, claim, termination)
	}
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, terminated.Outcome)
	require.Equal(t, "cleanup-complete", terminated.Entry.Phase)
	// The runtime is already physically dead. A fresh claimant must validate
	// retained history with its own native guards and preserve the old sender.
	require.NoError(t, b.ReleaseTaskClaim(ctx, claim))
	claim, err = b.ClaimTask(ctx, task, "fullchain-after-death", time.Minute)
	require.NoError(t, err)
	recovered, err := b.TerminateTaskRuntimeLocal(ctx, claim, termination)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, recovered.Outcome)
	require.Equal(t, terminated.Entry, recovered.Entry, "new claim must not replace original Current or proof")
	released, err := b.ReleaseTaskWorkspaceOwnerLocal(ctx, claim)
	if s.Fault == "ack-release" {
		require.Error(t, err)
		require.Equal(t, state.OutcomeUnknown, released.Outcome)
		released, err = b.ReleaseTaskWorkspaceOwnerLocal(ctx, claim)
	}
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, released.Outcome)
	newInput := state.AcquireIntentInput{Workspace: workspace, Principal: "fixture", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"mode":"plain"}`), Now: time.Now().UTC(), TTL: time.Hour}
	newOwner, err := b.AcquireIntent(ctx, newInput)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, newOwner.Outcome)
	require.NoError(t, b.ReleaseTaskClaim(ctx, claim))
	require.NoError(t, b.Close())
	cold, err := state.New(ctx, o)
	require.NoError(t, err)
	read, err := cold.LoadTaskOwnerRelease(ctx, task)
	require.NoError(t, err)
	require.Equal(t, released.Entry, read)
	require.NoError(t, cold.Close())
	t.Logf("public fullchain complete: task=%s physicalproof+cleanup-complete+atomic-release=%d newowner-cold-receipt=true", task.TaskID, released.Entry.CreateRevision)
}

func enrollmentDigestForRelease(t *testing.T, b *state.Backend, ctx context.Context, w state.WorkspaceIdentity, intent string) string {
	t.Helper()
	entry, err := b.LoadRuntimePublicationEnrolled(ctx, w, intent)
	require.NoError(t, err)
	require.NotNil(t, entry)
	return entry.Record.EnrollmentDigest
}

func TestLocalRuntimeReleaseNative(t *testing.T) {
	runLocalRuntimeReleaseNative(t, "")
}

func TestLocalRuntimeReleaseNativeFaults(t *testing.T) {
	for _, record := range []string{"runtime-enrollment", "runtime-enrollment-activation"} {
		for _, mutation := range []string{"same-value", "lease", "recreate"} {
			mode := "enrollment-" + record + "-" + mutation
			if !t.Run(mode, func(t *testing.T) { runLocalRuntimeReleaseNative(t, mode) }) {
				return
			}
		}
	}
}

func TestLocalRuntimeReleaseNativeACKs(t *testing.T) {
	for _, mode := range []string{"ack-reserved", "ack-cleanup-complete", "ack-release"} {
		if !t.Run(mode, func(t *testing.T) { runLocalRuntimeReleaseNative(t, mode) }) {
			return
		}
	}
}

func runLocalRuntimeReleaseNative(t *testing.T, fault string) {
	if os.Getenv("SANDBOX_LOCAL_RELEASE_NATIVE") != "1" {
		t.Skip("explicit exact owned Linux fixture required")
	}
	require.Zero(t, os.Geteuid())
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	dir, err := os.MkdirTemp("/tmp", "local-release-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	for _, name := range []string{"cgroup", "observer-journal"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0700))
	}
	mount := filepath.Join(dir, "cgroup")
	require.NoError(t, unix.Mount("none", mount, "cgroup2", 0, ""))
	t.Cleanup(func() { require.NoError(t, unix.Unmount(mount, 0)) })
	admin, observerTLS, cluster := observerNativeEtcd(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	_, err = admin.UserAddWithOptions(ctx, "server", "", &clientv3.UserAddOptions{NoPassword: true})
	require.NoError(t, err)
	_, err = admin.RoleAdd(ctx, "local-release-write")
	require.NoError(t, err)
	_, err = admin.RoleGrantPermission(ctx, "local-release-write", "/sandbox/observer/v1/", clientv3.GetPrefixRangeEnd("/sandbox/observer/v1/"), clientv3.PermissionType(clientv3.PermReadWrite))
	require.NoError(t, err)
	_, err = admin.UserGrantRole(ctx, "server", "local-release-write")
	require.NoError(t, err)
	appCert, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"))
	require.NoError(t, err)
	appTLS := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: observerTLS.RootCAs, Certificates: []tls.Certificate{appCert}}
	_, root, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, issuerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, observerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	bootWire, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	require.NoError(t, err)
	boot, nodeUID := strings.TrimSpace(string(bootWire)), uuid.NewString()
	binding := p.TrustBinding{Namespace: "/sandbox/observer/v1/", AuthorityID: "local-release-runtime", Target: "kubernetes", RestoreEpoch: "restore"}
	ns, err := state.NewNamespace("/sandbox", "observer", "v1")
	require.NoError(t, err)
	identity := state.Identity{SchemaVersion: 1, Prefix: "/sandbox", AuthorityID: "observer", Cell: "v1", ClusterID: cluster, StorageID: "local-release-storage", RuntimeID: binding.AuthorityID, RestoreEpoch: binding.RestoreEpoch}
	identityWire := observerNativeJSON(t, identity)
	_, err = admin.Put(ctx, binding.Namespace+"meta/identity", string(identityWire))
	require.NoError(t, err)
	_, err = admin.Put(ctx, binding.Namespace+"meta/restore_epoch", binding.RestoreEpoch)
	require.NoError(t, err)
	contract := hashBytes([]byte("local-release-genuine-empty-workspace-v1"))
	options := state.Options{Endpoints: []string{"https://127.0.0.1:2379"}, Namespace: ns, Identity: identity, TLS: appTLS, PublicationTrust: &state.RuntimePublicationTrust{AuthorityID: binding.AuthorityID, Target: binding.Target, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, Clock: nativeFixtureClock{}, EnrolledPublication: &state.EnrolledPublicationOptions{ContractDigest: contract, MaxActive: 2}, RequestTimeout: 2 * time.Second}
	b, err := state.New(ctx, options)
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	workspace, err := state.NewWorkspaceIdentity("local", identity.StorageID, "bucket", "empty/")
	require.NoError(t, err)
	input := state.AcquireIntentInput{Workspace: workspace, Principal: "fixture", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"mode":"plain"}`), Now: time.Now().UTC(), TTL: time.Hour}
	acquired, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, acquired.Outcome)
	creation, err := b.ClaimCreation(ctx, workspace, input.IntentID, "fullchain-creation", time.Minute)
	require.NoError(t, err)
	defer b.ReleaseCreationClaim(context.Background(), creation)
	dispatch, err := b.DeclareRuntimeDispatch(ctx, creation, state.RuntimeDispatchInput{Kind: state.RuntimeDispatchCreate, Target: binding.Target, Payload: json.RawMessage(`{"mode":"plain"}`)})
	require.NoError(t, err)
	now := time.Now().UTC()
	nb, na := now.Add(-30*time.Second), now.Add(5*time.Minute)
	observerCert, err := p.SignNodeObserverCertificate(root, p.NodeObserverCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "node_observer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, NodeUID: nodeUID, NodeBootID: boot, PublicKey: observerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	issuerCert, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	podName, podUID := "local-release-pod", uuid.NewString()
	zero, no, yes := int64(0), false, true
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "runtimes", UID: types.UID(podUID), ResourceVersion: "1", Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": input.SandboxID}, Annotations: map[string]string{"sandbox.creation.intent": input.IntentID}}, Spec: v1.PodSpec{NodeName: "node-a", RestartPolicy: v1.RestartPolicyNever, AutomountServiceAccountToken: &no, EnableServiceLinks: &no, SecurityContext: &v1.PodSecurityContext{SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}}, Containers: []v1.Container{{Name: "sandbox", Image: "image@sha256:" + strings.Repeat("a", 64), SecurityContext: &v1.SecurityContext{RunAsUser: &zero, Privileged: &no, AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &v1.Capabilities{Drop: []v1.Capability{"ALL"}, Add: []v1.Capability{"KILL", "SETUID", "SETGID", "SETPCAP"}}}}}}}
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID(nodeUID)}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: boot, OperatingSystem: "linux", Architecture: runtime.GOARCH}}}
	var apiMu sync.Mutex
	mutations := map[string]int{}
	receivedMutations := map[string]int{}
	var proxy *localReleaseNativeProxy
	var effectErrors []error
	var effectCounts [7]int
	policies := localReleaseNativePolicies(input.SandboxID, podName, podUID)
	effectIndex := -1
	if strings.HasPrefix(fault, "http-") {
		effectIndex, err = strconv.Atoi(strings.TrimPrefix(fault, "http-"))
		require.NoError(t, err)
	}
	beforeEffect := func(step int, uid, rv string) bool {
		if effectIndex < 0 {
			return true
		}
		if err := proxy.beforeEffect(ctx, admin, step, uid, rv); err != nil {
			effectErrors = append(effectErrors, err)
			return false
		}
		effectCounts[step]++
		return true
	}
	dropEffect := func(w http.ResponseWriter, step int) bool {
		if effectIndex != step {
			return false
		}
		if err := localReleaseDropHTTP(w); err != nil {
			effectErrors = append(effectErrors, err)
		}
		return true
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiMu.Lock()
		defer apiMu.Unlock()
		if r.Method != http.MethodGet {
			receivedMutations[r.Method+" "+r.URL.Path]++
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes/node-a" {
			_ = json.NewEncoder(w).Encode(node)
			return
		}
		if effectIndex >= 0 && policies[r.URL.Path] != nil {
			object := policies[r.URL.Path]
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(object)
				return
			}
			if r.Method != http.MethodDelete {
				w.WriteHeader(405)
				return
			}
			var request metav1.DeleteOptions
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&request) != nil || request.Preconditions == nil || request.Preconditions.UID == nil || request.Preconditions.ResourceVersion == nil || *request.Preconditions.UID != object.GetUID() || *request.Preconditions.ResourceVersion != object.GetResourceVersion() {
				w.WriteHeader(409)
				return
			}
			step := 2
			if object.GetKind() == "CiliumNetworkPolicy" {
				step = 4
			}
			if strings.HasSuffix(object.GetName(), podName) {
				step++
			}
			if !beforeEffect(step, string(object.GetUID()), object.GetResourceVersion()) {
				w.WriteHeader(409)
				return
			}
			delete(policies, r.URL.Path)
			if dropEffect(w, step) {
				return
			}
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Success", Code: 200})
			return
		}
		if r.URL.Path != "/api/v1/namespaces/runtimes/pods/"+podName || pod == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404})
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(pod)
			return
		}
		mutations[r.Method]++
		if r.Method == http.MethodPatch {
			var patch []struct {
				Op, Path string
				Value    json.RawMessage
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&patch) != nil || len(patch) != 3 {
				w.WriteHeader(400)
				return
			}
			var uid, rv string
			var finalizers []string
			if json.Unmarshal(patch[0].Value, &uid) != nil || json.Unmarshal(patch[1].Value, &rv) != nil || json.Unmarshal(patch[2].Value, &finalizers) != nil || patch[0].Op != "test" || patch[0].Path != "/metadata/uid" || patch[1].Op != "test" || patch[1].Path != "/metadata/resourceVersion" || patch[2].Op != "add" || patch[2].Path != "/metadata/finalizers" || uid != string(pod.UID) || rv != pod.ResourceVersion {
				w.WriteHeader(409)
				return
			}
			if len(finalizers) > 1 || len(finalizers) == 1 && finalizers[0] != "goairix.github.io/sandbox-runtime-termination" {
				w.WriteHeader(400)
				return
			}
			step := 0
			if len(finalizers) == 0 {
				step = 6
			}
			if !beforeEffect(step, uid, rv) {
				w.WriteHeader(409)
				return
			}
			pod.Finalizers = finalizers
			version, _ := strconv.Atoi(pod.ResourceVersion)
			pod.ResourceVersion = strconv.Itoa(version + 1)
			response := pod.DeepCopy()
			if pod.DeletionTimestamp != nil && len(finalizers) == 0 {
				pod = nil
			}
			if dropEffect(w, step) {
				return
			}
			_ = json.NewEncoder(w).Encode(response)
			return
		}
		if r.Method == http.MethodDelete {
			var request metav1.DeleteOptions
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&request) != nil || request.Preconditions == nil || request.Preconditions.UID == nil || request.Preconditions.ResourceVersion == nil || *request.Preconditions.UID != pod.UID || *request.Preconditions.ResourceVersion != pod.ResourceVersion || len(pod.Finalizers) != 1 {
				w.WriteHeader(409)
				return
			}
			at := metav1.Now()
			if !beforeEffect(1, string(pod.UID), pod.ResourceVersion) {
				w.WriteHeader(409)
				return
			}
			pod.DeletionTimestamp = &at
			version, _ := strconv.Atoi(pod.ResourceVersion)
			pod.ResourceVersion = strconv.Itoa(version + 1)
			if dropEffect(w, 1) {
				return
			}
			_ = json.NewEncoder(w).Encode(pod)
			return
		}
		w.WriteHeader(405)
	}))
	defer api.Close()
	uds, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "observer.sock"), Net: "unix"})
	require.NoError(t, err)
	require.NoError(t, os.Chmod(uds.Addr().String(), 0600))
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	observer, err := nodeobserver.New(nodeobserver.Options{Binding: binding, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, NodeName: "node-a", NodeUID: nodeUID, Certificate: observerCert, PrivateKey: observerKey, Clock: nativeFixtureClock{}, EtcdEndpoints: []string{"https://127.0.0.1:2379"}, EtcdTLS: observerTLS, EtcdClusterID: cluster, EtcdIdentity: identityWire, KubernetesConfig: &rest.Config{Host: api.URL}, KubernetesNamespace: "runtimes", JournalDirectory: filepath.Join(dir, "observer-journal"), MaxSlots: 1, MaxActive: 2, RequestTimeout: 3 * time.Second, TerminationTimeout: 20 * time.Second})
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- observer.Serve(ctx, uds, tcp) }()
	defer func() {
		require.NoError(t, observer.Close())
		err := <-served
		require.True(t, err == nil || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed), "original observer Serve join: %v", err)
		t.Logf("original observer Serve joined: %v", err)
	}()
	coordinator, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "coordinator.sock"), Net: "unix"})
	require.NoError(t, err)
	defer coordinator.Close()
	owned := filepath.Join(mount, "local-release-"+uuid.NewString())
	require.NoError(t, os.Mkdir(owned, 0700))
	defer func() {
		wire, err := os.ReadFile(filepath.Join(owned, "cgroup.events"))
		require.NoError(t, err)
		require.Contains(t, string(wire), "populated 0\n")
		require.NoError(t, os.Remove(owned))
	}()
	setup := localReleaseNativeSetup{Binding: binding, Root: root.Public().(ed25519.PublicKey), IssuerKey: issuerKey, IssuerCertificate: issuerCert, ObserverCertificate: observerCert, Identity: identity, WorkspaceProvider: "local", WorkspaceStorage: identity.StorageID, WorkspaceBucket: "bucket", WorkspacePrefix: "empty/", SandboxID: input.SandboxID, IntentID: input.IntentID, Contract: contract, NodeUID: nodeUID, NodeBootID: boot, Directory: dir, Coordinator: coordinator.Addr().String(), EnrollmentSocket: uds.Addr().String(), CgroupMount: mount, ObserverAddress: tcp.Addr().String(), ProviderAddress: api.URL, BudgetID: uuid.NewString()}
	setup.Fault = fault
	if strings.HasPrefix(fault, "ack-") || strings.HasPrefix(fault, "http-") {
		setup.EtcdAddress, proxy = newLocalReleaseNativeProxy(t, appTLS, fault)
	}
	setupPath := filepath.Join(dir, "supervisor.json")
	require.NoError(t, os.WriteFile(setupPath, observerNativeJSON(t, setup), 0600))
	fd, err := unix.Open(owned, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	child := exec.Command(os.Args[0], "-test.run=^TestLocalRuntimeReleaseNativeSupervisor$", "-test.v", "-test.timeout=140s")
	child.Env = append(os.Environ(), "LOCAL_RELEASE_SUPERVISOR_SETUP="+setupPath)
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS, UseCgroupFD: true, CgroupFD: fd}
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	require.NoError(t, child.Start())
	require.NoError(t, unix.Close(fd))
	childWaited := false
	defer func() {
		if !childWaited {
			_ = child.Process.Kill()
			t.Logf("original failed Supervisor cleanup Cmd.Wait=%v", child.Wait())
		}
	}()
	require.NoError(t, coordinator.SetDeadline(time.Now().Add(30*time.Second)))
	conn, err := coordinator.AcceptUnix()
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(30*time.Second)))
	wire, err := ct.ReadNodeObserverFrame(conn)
	require.NoError(t, err)
	var reply localReleaseNativeReply
	require.NoError(t, json.Unmarshal(wire, &reply))
	birth := reply.Birth
	ri := p.RuntimeIdentityContext{SandboxID: input.SandboxID, WorkspaceHash: workspace.Hash(), Generation: creation.Reference().Generation, Runtime: p.RuntimeReference{ID: podName, UID: podUID, BootID: birth.BootID}}
	runtimeCert, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: birth.RuntimePublicKey, NotBefore: nb, NotAfter: na, SandboxID: ri.SandboxID, WorkspaceHash: ri.WorkspaceHash, Generation: ri.Generation, Runtime: ri.Runtime})
	require.NoError(t, err)
	activation, err := p.SignTargetActivation(root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: ri, DataGateEpoch: creation.Reference().DataGateEpoch, UID: 1000, GID: 1000, ContractDigest: contract, RuntimeCertificateDigest: hashBytes(runtimeCert), IssuerCertificateDigest: hashBytes(issuerCert), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	d := dispatch.Entry.Record
	certificate, err := p.SignRuntimeCertificate(root, p.RuntimeCertificateClaims{Version: 1, Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IntentID: input.IntentID, SandboxID: input.SandboxID, WorkspaceHash: workspace.Hash(), Generation: ri.Generation, OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, Snapshot: p.SnapshotReference(d.Snapshot), ExpiresAt: d.ExpiresAt, Runtime: ri.Runtime, WorkspaceMode: "plain", PublicKey: birth.RuntimePublicKey, NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	_, err = b.BindRuntime(ctx, creation, certificate)
	require.NoError(t, err)
	_, err = b.ConsumeRuntimeMount(ctx, creation)
	require.NoError(t, err)
	command := enrolledNativeCommand{Action: "activate", Request: p.NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", Activation: activation, RuntimeCertificate: runtimeCert, IssuerCertificate: issuerCert}, Certificate: certificate, Claim: p.ClaimReference{ClaimID: creation.Reference().ClaimID, CreateRevision: creation.Reference().CreateRevision, LeaseID: creation.Reference().LeaseID}}
	require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, command)))
	wire, err = ct.ReadNodeObserverFrame(conn)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(wire, &reply))
	published, err := b.PublishRuntimeEnrolled(ctx, creation, reply.Proof, reply.Enrollment)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, published.Outcome)
	controlKey, err := ns.Key("p", fmt.Sprintf("%02x", workspace.Partition()), "controls", input.SandboxID)
	require.NoError(t, err)
	control, err := admin.Get(ctx, controlKey)
	require.NoError(t, err)
	require.Len(t, control.Kvs, 1)
	setup.ControlRevision, setup.RuntimeAddress, setup.RuntimeCertificate, setup.Runtime = control.Kvs[0].ModRevision, reply.Address, runtimeCert, ri.Runtime
	setup.Generation = ri.Generation
	budgetWire := fmt.Sprintf(`{"version":2,"budget_id":%q,"slots_per_partition":1,"max_intent_bytes":16384}`, setup.BudgetID)
	_, err = admin.Put(ctx, binding.Namespace+"meta/workspace_sync_recycling_budget", budgetWire)
	require.NoError(t, err)
	slotKey, err := ns.Key("p", fmt.Sprintf("%02x", workspace.Partition()), "workspace-sync-recycling-slots", setup.BudgetID, "00")
	require.NoError(t, err)
	free := fmt.Sprintf(`{"version":2,"budget_id":%q,"partition":%d,"slot":0,"state":"free","intent_id":"","intent_digest":"","intent_birth":0,"evidence_digest":""}`, setup.BudgetID, workspace.Partition())
	_, err = admin.Put(ctx, slotKey, free)
	require.NoError(t, err)
	workerPath := filepath.Join(dir, "worker.json")
	require.NoError(t, os.WriteFile(workerPath, observerNativeJSON(t, setup), 0600))
	worker := exec.Command(os.Args[0], "-test.run=^TestLocalRuntimeReleaseNativeWorker$", "-test.v", "-test.timeout=110s")
	worker.Env = append(os.Environ(), "LOCAL_RELEASE_WORKER_SETUP="+workerPath)
	worker.Stdout, worker.Stderr = os.Stdout, os.Stderr
	require.NoError(t, worker.Start())
	workerErr := worker.Wait()
	t.Logf("original local worker Cmd.Wait=%v", workerErr)
	require.NoError(t, workerErr)
	if strings.HasPrefix(fault, "enrollment-") || fault == "ack-reserved" || effectIndex == 0 || effectIndex == 1 {
		require.NoError(t, child.Process.Kill())
	}
	childErr := child.Wait()
	childWaited = true
	var exited *exec.ExitError
	require.ErrorAs(t, childErr, &exited)
	require.Equal(t, syscall.SIGKILL, exited.Sys().(syscall.WaitStatus).Signal())
	if fault == "" || fault == "ack-cleanup-complete" || fault == "ack-release" || effectIndex >= 2 {
		t.Logf("original genuine Supervisor Cmd.Wait=%v (observer SIGKILL)", childErr)
	} else {
		t.Logf("original refused-case Supervisor cleanup Cmd.Wait=%v (fixture cleanup, not physical proof)", childErr)
	}
	apiMu.Lock()
	actualPatch, actualDelete := mutations[http.MethodPatch], mutations[http.MethodDelete]
	absent := pod == nil
	apiMu.Unlock()
	if effectIndex >= 0 {
		require.Empty(t, effectErrors)
		totalReceived := 0
		for key, count := range receivedMutations {
			totalReceived += count
			if strings.HasPrefix(key, "PATCH ") && effectIndex == 6 {
				require.Equal(t, 2, count)
			} else {
				require.Equal(t, 1, count)
			}
		}
		require.Equal(t, effectIndex+1, totalReceived, "count all requests, including retries rejected before UID/RV validation")
		for step, count := range effectCounts {
			if step <= effectIndex {
				require.Equal(t, 1, count)
			} else {
				require.Zero(t, count)
			}
		}
		require.Equal(t, effectIndex == 6, absent)
		t.Logf("actual seven-slot HTTP received total=%d effects=%v; each send independently checked native durable intent and immediately preceding fresh provider MOD guard; four unique configured policies", totalReceived, effectCounts)
	} else if fault == "" || fault == "ack-cleanup-complete" || fault == "ack-release" {
		require.Equal(t, 2, actualPatch)
		require.Equal(t, 1, actualDelete)
		require.True(t, absent)
	} else if fault == "ack-reserved" {
		require.Equal(t, 1, actualPatch)
		require.Zero(t, actualDelete)
		require.False(t, absent)
	} else {
		require.Zero(t, actualPatch)
		require.Zero(t, actualDelete)
		require.False(t, absent)
	}
	t.Logf("actual received HTTP mutations patch=%d delete=%d; exact original UID/RV enforced; absent policies are skipped diagnostics", actualPatch, actualDelete)
	if proxy != nil {
		proxy.verify(t)
	}
}

func localReleaseEnrollmentRefusal(t *testing.T, ctx context.Context, b *state.Backend, claim *state.TaskClaim, destination *ct.TerminationDestination, s localReleaseNativeSetup, tlsConfig *tls.Config, ns state.Namespace, workspace state.WorkspaceIdentity, task state.TaskReference) {
	t.Helper()
	raw, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: tlsConfig, DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer func() { require.NoError(t, raw.Close()) }()
	record := "runtime-enrollment"
	if strings.Contains(s.Fault, "-activation-") {
		record += "-activation"
	}
	key, err := ns.Key("p", fmt.Sprintf("%02x", workspace.Partition()), "intents", s.IntentID, record)
	require.NoError(t, err)
	original, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, original.Kvs, 1)
	before := original.Kvs[0]
	require.EqualValues(t, 1, before.Version)
	require.Zero(t, before.Lease)
	var lease clientv3.LeaseID
	switch {
	case strings.HasSuffix(s.Fault, "-same-value"):
		_, err = raw.Put(ctx, key, string(before.Value))
	case strings.HasSuffix(s.Fault, "-lease"):
		grant, grantErr := raw.Grant(ctx, 60)
		require.NoError(t, grantErr)
		lease = grant.ID
		_, err = raw.Put(ctx, key, string(before.Value), clientv3.WithLease(lease))
	case strings.HasSuffix(s.Fault, "-recreate"):
		deleted, deleteErr := raw.Delete(ctx, key)
		require.NoError(t, deleteErr)
		require.EqualValues(t, 1, deleted.Deleted)
		_, err = raw.Put(ctx, key, string(before.Value))
	default:
		t.Fatal("unrecognized finite enrollment fault")
	}
	require.NoError(t, err)
	after, err := raw.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, after.Kvs, 1)
	require.Equal(t, before.Value, after.Kvs[0].Value)
	require.Greater(t, after.Kvs[0].ModRevision, before.ModRevision)
	if lease != 0 {
		require.EqualValues(t, lease, after.Kvs[0].Lease)
		defer func() { _, revokeErr := raw.Revoke(ctx, lease); require.NoError(t, revokeErr) }()
	}
	result, err := b.TerminateTaskRuntimeLocal(ctx, claim, destination)
	require.Error(t, err)
	require.Equal(t, state.OutcomeUnknown, result.Outcome)
	require.Nil(t, result.Entry)
	release, err := b.ReleaseTaskWorkspaceOwnerLocal(ctx, claim)
	require.Error(t, err)
	require.Equal(t, state.OutcomeUnknown, release.Outcome)
	for _, suffix := range []string{"runtime-termination", "runtime-provider-cleanup", "owner-release"} {
		key, err := ns.Key("p", fmt.Sprintf("%02x", task.Partition), "tasks", task.TaskID, suffix)
		require.NoError(t, err)
		point, err := raw.Get(ctx, key)
		require.NoError(t, err)
		require.Empty(t, point.Kvs, "rejected authority must not create transition state")
	}
	ownerKey, err := ns.Key("p", fmt.Sprintf("%02x", task.Partition), "workspaces", workspace.Hash(), "owner")
	require.NoError(t, err)
	owner, err := raw.Get(ctx, ownerKey)
	require.NoError(t, err)
	require.Len(t, owner.Kvs, 1)
	require.NoError(t, b.ReleaseTaskClaim(ctx, claim))
	t.Logf("actual enrollment refusal %s originalMOD=%d changedMOD=%d: transition keys absent, owner retained", s.Fault, before.ModRevision, after.Kvs[0].ModRevision)
}
