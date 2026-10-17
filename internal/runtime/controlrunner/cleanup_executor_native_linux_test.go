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
	"io"
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

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/goairix/sandbox/internal/nodeobserver"
	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	k "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcher"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

const cleanupExecutorPayload = "cleanup executor archive\n"

type cleanupExecutorSetup struct {
	localReleaseNativeSetup
	Role                                     string
	Task                                     state.TaskReference
	Recovery, RecoveryStage, WorkerDirectory string
	CloseMode                                string
}

// The child selector has three actual process roles. Only worker drops to the
// zero-cap boundary; supervisor is genuine PID1, and seed exits after writing
// the runtime's real nonempty workspace as its ordinary UID/GID.
func TestCleanupExecutorWorkerChild(t *testing.T) {
	path := os.Getenv("CLEANUP_EXECUTOR_CHILD_SETUP")
	if path == "" {
		return
	}
	wire, err := os.ReadFile(path)
	require.NoError(t, err)
	var s cleanupExecutorSetup
	require.NoError(t, json.Unmarshal(wire, &s))
	if os.Getenv("CLEANUP_EXECUTOR_SEED") == "1" {
		require.Equal(t, 1000, os.Geteuid())
		require.NoError(t, os.Mkdir("/workspace/nested", 0750))
		require.NoError(t, os.WriteFile("/workspace/nested/data", []byte(cleanupExecutorPayload), 0640))
		require.NoError(t, os.Chmod("/workspace/nested", 0750))
		require.NoError(t, os.Chmod("/workspace/nested/data", 0640))
		at := time.Unix(1700000000, 0)
		require.NoError(t, os.Chtimes("/workspace/nested/data", at, at))
		require.NoError(t, os.Chtimes("/workspace/nested", at, at))
		return
	}
	s.Identity.RestoreEpoch = s.Binding.RestoreEpoch
	switch s.Role {
	case "supervisor":
		cleanupExecutorSupervisor(t, path, s)
	case "worker":
		cleanupExecutorWorker(t, s)
	default:
		t.Fatalf("unknown child role %q", s.Role)
	}
}

func cleanupExecutorSupervisor(t *testing.T, path string, s cleanupExecutorSetup) {
	t.Helper()
	require.Equal(t, 1, os.Getpid())
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	require.NoError(t, unix.Unmount(s.CgroupMount, 0))
	require.NoError(t, unix.Unmount("/proc", unix.MNT_DETACH))
	require.NoError(t, unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""))
	require.NoError(t, unix.Mount("tmpfs", "/workspace", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=1048576,mode=0700,uid=1000,gid=1000"))
	// The setup is read before dropping credentials. A separate readable copy
	// contains only the role marker needed by the short-lived seed child.
	seedPath := filepath.Join("/tmp", "cleanup-executor-seed-"+uuid.NewString()+".json")
	require.NoError(t, os.WriteFile(seedPath, []byte(`{"Role":"seed"}`), 0644))
	seed := exec.Command(os.Args[0], "-test.run=^TestCleanupExecutorWorkerChild$", "-test.v")
	seed.Env = append(os.Environ(), "CLEANUP_EXECUTOR_CHILD_SETUP="+seedPath, "CLEANUP_EXECUTOR_SEED=1")
	seed.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, NoSetGroups: true}}
	seed.Stdout, seed.Stderr = os.Stdout, os.Stderr
	require.NoError(t, seed.Run())
	require.NoError(t, os.Remove(seedPath))
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
	verifier, err := p.NewManagementVerifier(s.Binding, []ed25519.PublicKey{s.Root})
	require.NoError(t, err)
	supervisor, err := NewEnrolledSupervisor(SupervisorOptions{Kernel: kernel, UID: 1000, GID: 1000, ContractDigest: s.Contract, Verifier: verifier, Clock: nativeFixtureClock{}, JournalDirectory: filepath.Join(s.Directory, "supervisor-journal"), Executable: "/fixture/monitor"}, NodeEnrollmentOptions{SocketPath: s.EnrollmentSocket, NodeUID: s.NodeUID, NodeBootID: s.NodeBootID})
	require.NoError(t, err)
	defer supervisor.Close()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: s.Coordinator, Net: "unix"})
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(time.Minute)))
	require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, localReleaseNativeReply{Birth: supervisor.Birth()})))
	wire, err := ct.ReadNodeObserverFrame(conn)
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
	t.Fatalf("genuine supervisor returned before observer termination: %v", err)
}

func cleanupExecutorWorker(t *testing.T, s cleanupExecutorSetup) {
	t.Helper()
	ca, err := os.ReadFile(filepath.Join(s.Directory, "ca.pem"))
	require.NoError(t, err)
	certPEM, err := os.ReadFile(filepath.Join(s.Directory, "server.pem"))
	require.NoError(t, err)
	keyPEM, err := os.ReadFile(filepath.Join(s.Directory, "server-key.pem"))
	require.NoError(t, err)
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
	if s.Recovery != "" && s.RecoveryStage == "cleanup" {
		cleanupExecutorRecoveryRemove(t, s)
		return
	}
	kernel, err := launcher.BootstrapWorkspaceSyncWorker()
	require.NoError(t, err)
	dir := s.WorkerDirectory
	if dir == "" {
		dir, err = os.MkdirTemp("/tmp", "cleanup-executor-worker-")
		require.NoError(t, err)
		defer os.RemoveAll(dir)
		for _, name := range []string{"spool", "target", "target/cleanup-executor"} {
			require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0700))
		}
	} else if s.Recovery != "" && s.RecoveryStage == "first" {
		// The container deliberately has neither CHOWN nor DAC_OVERRIDE. The
		// actual ordinary worker owns the persistent recovery tree from birth.
		for _, name := range []string{"", "spool", "target", "target/cleanup-executor"} {
			require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0700))
		}
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
	identity, err := verifier.VerifyCommandIssuerCertificate(s.IssuerCertificate, time.Now().UTC())
	require.NoError(t, err)
	issuer := localReleaseNativeIssuer{s.IssuerKey, s.IssuerCertificate, identity}
	credential, err := p.NewManagementTLSCredential(s.IssuerKey, s.IssuerCertificate, "command_issuer")
	require.NoError(t, err)
	defer credential.Close()
	observerCredential, err := p.NewNodeObserverTLSCredential(s.IssuerKey, s.IssuerCertificate, false)
	require.NoError(t, err)
	defer observerCredential.Close()
	bridge, err := k.NewEnrolledBridgeClient(k.EnrolledBridgeOptions{Config: &rest.Config{Host: s.ProviderAddress}, Namespace: "runtimes", MaxActive: 16, RequestTimeout: 3 * time.Second})
	require.NoError(t, err)
	defer bridge.Close()
	provider, err := k.NewTaskRuntimeCleanupClient(k.TaskRuntimeCleanupOptions{Config: &rest.Config{Host: s.ProviderAddress}, Namespace: "runtimes", MaxActive: 4, RequestTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer provider.Close()
	options := state.Options{
		Endpoints: []string{"https://127.0.0.1:2379"}, Namespace: ns,
		Identity: s.Identity, TLS: tlsConfig, RequestTimeout: 2 * time.Second,
		PublicationTrust:    &state.RuntimePublicationTrust{AuthorityID: s.Binding.AuthorityID, Target: s.Binding.Target, Roots: []ed25519.PublicKey{s.Root}},
		Clock:               nativeFixtureClock{},
		EnrolledPublication: &state.EnrolledPublicationOptions{ContractDigest: s.Contract, MaxActive: 32},

		TaskIssuer: issuer, TaskQuiescenceIssuer: issuer,
		TaskWorkspaceArchiveIssuer: issuer, TaskRuntimeTerminationIssuer: issuer,
		LocalRuntimeRelease: &state.LocalRuntimeReleaseOptions{MaxActive: 4, Timeout: 30 * time.Second, Provider: provider},
		TaskWorkspaceArchiveRecovery: &state.TaskWorkspaceArchiveRecoveryOptions{
			TaskWorkspaceArchiveSpoolOptions: state.TaskWorkspaceArchiveSpoolOptions{
				Directory: filepath.Join(dir, "spool"), Jobs: 1, Entries: 16,
				JobBytes: 1 << 20, TotalBytes: 1 << 20,
				JobIndexBytes: 1 << 20, TotalIndexBytes: 1 << 20,
			},
		},
		TaskWorkspaceSyncRecyclingBudget: &state.TaskWorkspaceSyncBudgetOptions{BudgetID: s.BudgetID, SlotsPerPartition: 1, MaxIntentBytes: 16384},
		WorkerControl:                    &state.WorkerControlOptions{SessionTTL: 60 * time.Second},
		LocalCleanup: &state.LocalCleanupOptions{
			Worker: kernel, Bridge: bridge, Credential: credential, ObserverCredential: observerCredential,
			Nodes:    []state.LocalCleanupNode{{Name: "node-a", UID: s.NodeUID, BootID: s.NodeBootID, ObserverAddress: s.ObserverAddress}},
			Storage:  state.LocalCleanupStorage{Identity: s.WorkspaceStorage, Bucket: s.WorkspaceBucket, Directory: filepath.Join(dir, "target")},
			ClaimTTL: 120 * time.Second, JobTimeout: 90 * time.Second, ArchiveTimeout: 60 * time.Second, MaxActive: 32,
		},
	}
	if s.CloseMode != "" {
		cleanupExecutorCloseWorker(t, ctx, s, options, tlsConfig)
		return
	}
	if s.Recovery != "" {
		cleanupExecutorRecoveryExecute(t, ctx, s, options, dir, tlsConfig)
		return
	}
	for _, change := range []func(*state.Options){
		func(o *state.Options) { o.WorkerControl = &state.WorkerControlOptions{SessionTTL: 59 * time.Second} },
		func(o *state.Options) { o.RequestTimeout = 3*time.Second + time.Nanosecond },
	} {
		bad := options
		change(&bad)
		backend, err := state.New(ctx, bad)
		require.ErrorIs(t, err, state.ErrInvalidConfiguration)
		require.Nil(t, backend)
	}
	b, err := state.New(ctx, options)
	require.NoError(t, err)
	defer b.Close()
	// Mutating the caller's backing slice after startup cannot retarget the
	// queued job. The backend must have captured its finite authority mapping.
	options.LocalCleanup.Nodes[0].ObserverAddress = "127.0.0.1:1"
	shares := make([]uint16, 32)
	for i := range shares {
		shares[i] = uint16(i)
	}
	w, err := b.StartDueWorker(ctx, state.DueWorkerOptions{WorkerID: "cleanup-native", Partitions: []uint8{s.Task.Partition}, RangeSlots: []uint16{0}, ClaimSlots: shares, RelistSlot: 0, StageTTL: 30 * time.Second})
	require.NoError(t, err)
	defer w.Stop()
	raw, err := clientv3.New(clientv3.Config{Endpoints: options.Endpoints, TLS: tlsConfig, DialTimeout: 2 * time.Second})
	require.NoError(t, err)
	defer raw.Close()
	taskPrefix := fmt.Sprintf("%sp/%02x/tasks/%s/", s.Binding.Namespace, s.Task.Partition, s.Task.TaskID)
	watchCtx, stopWatch := context.WithCancel(ctx)
	watch := raw.Watch(watchCtx, taskPrefix, clientv3.WithPrefix(), clientv3.WithCreatedNotify())
	created := <-watch
	require.NoError(t, created.Err())
	require.True(t, created.Created)
	started := make(chan struct{})
	traceDone := make(chan cleanupExecutorClaimTrace, 1)
	go func() { traceDone <- cleanupExecutorObserveClaims(watch, taskPrefix, started) }()
	traceJoined := false
	defer func() {
		stopWatch()
		if !traceJoined {
			<-traceDone
		}
	}()

	type completion struct {
		result state.CleanupLocalResult
		err    error
	}
	completed := make(chan completion, 1)
	executeDone := make(chan struct{})
	go func() {
		defer close(executeDone)
		result, err := w.ExecuteCleanupLocal(ctx, s.Task)
		completed <- completion{result: result, err: err}
	}()
	defer func() {
		cancel()
		_ = w.Stop()
		<-executeDone
	}()
	select {
	case <-started:
	case early := <-completed:
		t.Fatalf("cleanup ended before its actual execution row: %+v, %v", early.result, early.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	// This is discovery metadata only. It is inserted after this real cleanup
	// starts, and must be consumed by the original due Range/Watch while the
	// cleanup is still running. It does not represent another physical Pod.
	workspace, err := state.NewWorkspaceIdentity("local", s.WorkspaceStorage, s.WorkspaceBucket, s.WorkspacePrefix)
	require.NoError(t, err)
	future := state.DueCandidateRecord{Version: 1, Partition: s.Task.Partition, SandboxID: uuid.NewString(), WorkspaceHash: workspace.Hash(), RestoreEpoch: s.Binding.RestoreEpoch, Generation: 1, IntentID: uuid.NewString(), ExpiresAt: time.Now().UTC().Add(25 * time.Second)}
	require.NoError(t, future.Validate())
	futureKey := fmt.Sprintf("%sp/%02x/due/%s/%s/%019d/%s", s.Binding.Namespace, future.Partition, future.ExpiresAt.Format("20060102T150405.000000000Z"), future.SandboxID, future.Generation, future.IntentID)
	put, err := raw.Put(ctx, futureKey, string(observerNativeJSON(t, future)))
	require.NoError(t, err)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	progressed := false
	for !progressed {
		status := w.Status()
		for _, partition := range status.Partitions {
			if partition.Partition == future.Partition && partition.Queued > 0 && partition.OldestExpiry.Equal(future.ExpiresAt) && partition.CoveredRevision >= put.Header.Revision {
				progressed = true
				t.Logf("original due progressed during cleanup: queued=%d covered=%d inserted=%d session=%+v", partition.Queued, partition.CoveredRevision, put.Header.Revision, status.Session)
			}
		}
		if progressed {
			select {
			case early := <-completed:
				t.Fatalf("cleanup already returned when due progress was observed: %+v, %v", early.result, early.err)
			default:
			}
			break
		}
		select {
		case early := <-completed:
			t.Fatalf("cleanup ended before original due covered the new future row: %+v, %v; status=%+v", early.result, early.err, status)
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	var finished completion
	select {
	case finished = <-completed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	result, err := finished.result, finished.err
	t.Logf("cleanup result: phase=%s outcome=%v retained=%t reason=%q", result.Phase, result.Outcome, result.Retained, result.Reason)
	if result.Slot != nil {
		t.Logf("cleanup slot: %+v", *result.Slot)
	}
	if result.Pending != nil {
		t.Logf("cleanup pending: %+v", *result.Pending)
	}
	if err != nil {
		// Observe only this Task's persisted execution row after the actual
		// job returns. Independent bounded diagnostics cannot replay an effect.
		diagnostic, cancelDiagnostic := context.WithTimeout(context.Background(), 2*time.Second)
		observed, readErr := raw.Get(diagnostic, taskPrefix+"execution")
		cancelDiagnostic()
		t.Logf("cleanup execution read: error=%v", readErr)
		if observed != nil && observed.Header != nil {
			t.Logf("cleanup execution read: revision=%d count=%d", observed.Header.Revision, len(observed.Kvs))
			for _, kv := range observed.Kvs {
				if len(kv.Value) <= 4096 {
					t.Logf("cleanup execution: create=%d mod=%d json=%s", kv.CreateRevision, kv.ModRevision, kv.Value)
				} else {
					t.Logf("cleanup execution: create=%d mod=%d oversized=%d", kv.CreateRevision, kv.ModRevision, len(kv.Value))
				}
			}
		}

		// These two exact points distinguish authority admission from the
		// original observer/provider stages without scanning retained history.
		historyContext, cancelHistory := context.WithTimeout(context.Background(), 2*time.Second)
		history, historyErr := raw.Txn(historyContext).Then(
			clientv3.OpGet(taskPrefix+"runtime-termination"),
			clientv3.OpGet(taskPrefix+"runtime-provider-cleanup"),
		).Commit()
		cancelHistory()
		t.Logf("cleanup terminal history read: error=%v", historyErr)
		if history != nil && history.Header != nil {
			t.Logf("cleanup terminal history read: revision=%d responses=%d", history.Header.Revision, len(history.Responses))
			for i, operation := range history.Responses {
				if operation == nil || operation.GetResponseRange() == nil {
					t.Logf("cleanup terminal history point[%d]: missing range response", i)
					continue
				}
				point := operation.GetResponseRange()
				t.Logf("cleanup terminal history point[%d]: present=%t count=%d", i, len(point.Kvs) != 0, len(point.Kvs))
				for _, kv := range point.Kvs {
					if len(kv.Value) > 16384 {
						t.Logf("cleanup terminal history point[%d]: create=%d mod=%d oversized=%d", i, kv.CreateRevision, kv.ModRevision, len(kv.Value))
						continue
					}
					var diagnosticRow struct {
						Phase string `json:"phase"`
						Slots []struct {
							State string `json:"state"`
						} `json:"slots"`
					}
					decodeErr := json.Unmarshal(kv.Value, &diagnosticRow)
					t.Logf("cleanup terminal history point[%d]: create=%d mod=%d phase=%q slots=%+v decode=%v", i, kv.CreateRevision, kv.ModRevision, diagnosticRow.Phase, diagnosticRow.Slots, decodeErr)
				}
			}
		}
	}
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, result.Outcome)
	require.Equal(t, "settled", string(result.Phase))
	require.False(t, result.Retained)
	require.NotNil(t, result.Slot)
	require.NotNil(t, result.Release)
	require.Nil(t, result.Pending)
	// The sentinel is only a test observation barrier. Receiving it proves
	// the watch consumed all earlier real Claim puts/deletes, including the
	// final release; cancellation alone would not prove that coverage.
	_, err = raw.Put(ctx, taskPrefix+"test-observation-end", "complete")
	require.NoError(t, err)
	trace := <-traceDone
	traceJoined = true
	stopWatch()
	require.NoError(t, trace.err)
	require.Empty(t, trace.active)
	require.Len(t, trace.ids, 2, "bound local archive must roll over to a genuinely new TaskClaim")
	require.NotEqual(t, trace.ids[0], trace.ids[1])
	require.Equal(t, []string{"put", "delete", "put", "delete"}, trace.events)
	t.Logf("actual original TaskClaim rollover: %v", trace.ids)
	content, err := os.ReadFile(filepath.Join(dir, "target", "cleanup-executor", "nested", "data"))
	require.NoError(t, err)
	require.Equal(t, cleanupExecutorPayload, string(content))
	for path, mode := range map[string]os.FileMode{"nested": 0750, "nested/data": 0640} {
		info, err := os.Stat(filepath.Join(dir, "target", "cleanup-executor", path))
		require.NoError(t, err)
		require.Equal(t, mode, info.Mode().Perm())
		// Existing local sync restores content, directories and modes; it does
		// not restore archive mtime. The source retains its fixed timestamp.
	}
	repeat, err := w.ExecuteCleanupLocal(ctx, s.Task)
	require.NoError(t, err)
	require.Equal(t, result.Slot, repeat.Slot)
	require.Equal(t, result.Release, repeat.Release)
	// Stop joins native work but preserves an uncertain write canceled while
	// the original due actor was active; that outcome must not be silenced.
	stopErr := w.Stop()
	t.Logf("original cleanup due worker Stop=%v", stopErr)
	if stopErr != nil {
		require.ErrorIs(t, stopErr, state.ErrOutcomeUnknown)
		require.ErrorIs(t, stopErr, context.Canceled)
	}
	<-w.Done()
	stopped := w.Status()
	require.False(t, stopped.Running)
	require.False(t, stopped.Synchronized)
	if stopErr == nil {
		require.Equal(t, state.DueWorkerPhase("stopped"), stopped.Phase)
	} else {
		require.Equal(t, state.DueWorkerPhase("unknown"), stopped.Phase)
	}
	require.NoError(t, b.Close())
	t.Logf("actual consumer complete: Task=%s slot=%+v release=%+v; real archive content, directory/file metadata, rollover and all local joins", s.Task.TaskID, result.Slot, result.Release)
}

type cleanupExecutorClaimTrace struct {
	ids, events []string
	active      string
	err         error
}

// Observe native etcd events, never fabricate claims or physical authority.
// The result is published once so all trace fields have a single owner.
func cleanupExecutorObserveClaims(watch clientv3.WatchChan, prefix string, started chan<- struct{}) cleanupExecutorClaimTrace {
	var trace cleanupExecutorClaimTrace
	var once sync.Once
	for response := range watch {
		if err := response.Err(); err != nil {
			trace.err = err
			return trace
		}
		for _, event := range response.Events {
			switch string(event.Kv.Key) {
			case prefix + "execution":
				once.Do(func() { close(started) })
			case prefix + "claim":
				if event.Type == clientv3.EventTypeDelete {
					if trace.active == "" {
						trace.err = errors.New("claim delete without an observed live claim")
						return trace
					}
					trace.active = ""
					trace.events = append(trace.events, "delete")
					continue
				}
				var claim struct {
					ID string `json:"claim_id"`
				}
				if err := json.Unmarshal(event.Kv.Value, &claim); err != nil || claim.ID == "" || trace.active != "" || !event.IsCreate() {
					trace.err = fmt.Errorf("invalid or overlapping actual Claim event: %s", event.Kv.Value)
					return trace
				}
				trace.ids = append(trace.ids, claim.ID)
				trace.events = append(trace.events, "put")
				trace.active = claim.ID
			case prefix + "test-observation-end":
				return trace
			}
		}
	}
	trace.err = errors.New("claim watch ended before observation barrier")
	return trace
}

// The API fixture enforces the exact enrolled Pod/Node reads and Kubernetes v5
// exec framing. The upgraded bytes reach the real Supervisor; no handler can
// synthesize close, archive, observer, or cleanup evidence.
type cleanupExecutorAPI struct {
	mu                         sync.Mutex
	pod                        *v1.Pod
	node                       *v1.Node
	address                    string
	patches, deletes, upgrades int
	connections                map[net.Conn]struct{}
	wg                         sync.WaitGroup
}

func (a *cleanupExecutorAPI) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/exec") {
		a.bridge(w, r)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/nodes/node-a" {
		_ = json.NewEncoder(w).Encode(a.node)
		return
	}
	if a.pod == nil || r.URL.Path != "/api/v1/namespaces/runtimes/pods/"+a.pod.Name {
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404})
		return
	}
	switch r.Method {
	case http.MethodGet:
		_ = json.NewEncoder(w).Encode(a.pod)
	case http.MethodPatch:
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
		if json.Unmarshal(patch[0].Value, &uid) != nil || json.Unmarshal(patch[1].Value, &rv) != nil || json.Unmarshal(patch[2].Value, &finalizers) != nil || patch[0].Op != "test" || patch[0].Path != "/metadata/uid" || patch[1].Op != "test" || patch[1].Path != "/metadata/resourceVersion" || patch[2].Op != "add" || patch[2].Path != "/metadata/finalizers" || uid != string(a.pod.UID) || rv != a.pod.ResourceVersion {
			w.WriteHeader(409)
			return
		}
		if len(finalizers) > 1 || len(finalizers) == 1 && finalizers[0] != kubecontract.EnrolledRetainFinalizer {
			w.WriteHeader(400)
			return
		}
		a.patches++
		a.pod.Finalizers = finalizers
		version, _ := strconv.Atoi(a.pod.ResourceVersion)
		a.pod.ResourceVersion = strconv.Itoa(version + 1)
		response := a.pod.DeepCopy()
		if a.pod.DeletionTimestamp != nil && len(finalizers) == 0 {
			a.pod = nil
		}
		_ = json.NewEncoder(w).Encode(response)
	case http.MethodDelete:
		var request metav1.DeleteOptions
		if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&request) != nil || request.Preconditions == nil || request.Preconditions.UID == nil || request.Preconditions.ResourceVersion == nil || *request.Preconditions.UID != a.pod.UID || *request.Preconditions.ResourceVersion != a.pod.ResourceVersion || len(a.pod.Finalizers) != 1 {
			w.WriteHeader(409)
			return
		}
		a.deletes++
		now := metav1.Now()
		a.pod.DeletionTimestamp = &now
		version, _ := strconv.Atoi(a.pod.ResourceVersion)
		a.pod.ResourceVersion = strconv.Itoa(version + 1)
		_ = json.NewEncoder(w).Encode(a.pod)
	default:
		w.WriteHeader(405)
	}
}

func (a *cleanupExecutorAPI) bridge(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	valid := a.pod != nil && r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/runtimes/pods/"+a.pod.Name+"/exec" && a.pod.DeletionTimestamp == nil
	address := a.address
	a.mu.Unlock()
	q := r.URL.Query()
	commands := q["command"]
	if !valid || address == "" || len(commands) != 2 || commands[0] != "/usr/local/bin/sandbox-launcher" || commands[1] != "bridge" || q.Get("container") != "sandbox" || q.Get("stdin") != "true" || q.Get("stdout") != "true" || q.Get("stderr") != "true" || q.Get("tty") != "false" {
		http.Error(w, "invalid exact bridge target", 409)
		return
	}
	raw, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		http.Error(w, "supervisor unavailable", 503)
		return
	}
	upgrader := websocket.Upgrader{Subprotocols: []string{"v5.channel.k8s.io"}}
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		raw.Close()
		return
	}
	a.mu.Lock()
	a.upgrades++
	a.connections[raw] = struct{}{}
	a.wg.Add(1)
	a.mu.Unlock()
	defer func() {
		raw.Close()
		ws.Close()
		a.mu.Lock()
		delete(a.connections, raw)
		a.mu.Unlock()
		a.wg.Done()
	}()
	// One writer forwards actual stdout bytes. The request goroutine is the
	// sole reader; either exit closes both sockets and then joins the writer.
	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		defer ws.Close()
		buffer := make([]byte, 32768)
		for {
			n, err := raw.Read(buffer[1:])
			buffer[0] = 1
			if n > 0 && ws.WriteMessage(websocket.BinaryMessage, buffer[:n+1]) != nil {
				return
			}
			if err != nil {
				// client-go v5 inherits v4's channel-3 Status decoder. A
				// normal TCP EOF ends the real bridge successfully; another
				// read error remains a transport failure. Neither is a runtime
				// business receipt. Normal WS close terminates all readers.
				status := metav1.Status{Status: metav1.StatusSuccess}
				if !errors.Is(err, io.EOF) {
					status.Status = metav1.StatusFailure
					status.Message = err.Error()
				}
				wire, marshalErr := json.Marshal(status)
				if marshalErr == nil {
					_ = ws.WriteMessage(websocket.BinaryMessage, append([]byte{3}, wire...))
					_ = ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
				}
				return
			}
		}
	}()
	for {
		kind, data, err := ws.ReadMessage()
		if err != nil || kind != websocket.BinaryMessage || len(data) == 0 {
			break
		}
		if data[0] == 255 && len(data) == 2 && data[1] == 0 {
			_ = raw.(*net.TCPConn).CloseWrite()
			continue
		}
		if data[0] != 0 {
			break
		}
		if _, err := io.Copy(raw, strings.NewReader(string(data[1:]))); err != nil {
			break
		}
	}
	raw.Close()
	ws.Close()
	<-stdoutDone
}

func (a *cleanupExecutorAPI) close() {
	a.mu.Lock()
	for raw := range a.connections {
		raw.Close()
	}
	a.mu.Unlock()
	a.wg.Wait()
}

func TestCleanupExecutorNative(t *testing.T) {
	if os.Getenv("SANDBOX_CLEANUP_EXECUTOR_NATIVE") != "1" {
		t.Skip("explicit owned cleanup executor Linux fixture required")
	}
	t.Run("complete", func(t *testing.T) { cleanupExecutorNativeCase(t) })
	t.Run("bound-recovery", func(t *testing.T) { cleanupExecutorNativeCase(t, "bound-recovery") })
	t.Run("release-settlement-recovery", func(t *testing.T) { cleanupExecutorNativeCase(t, "release-settlement-recovery") })
	t.Run("observer-ack-recovery", func(t *testing.T) { cleanupExecutorNativeCase(t, "observer-ack-recovery") })
	t.Run("close-join", func(t *testing.T) {
		for _, mode := range []string{"stop", "backend-close"} {
			t.Run(mode, func(t *testing.T) { cleanupExecutorNativeCase(t, "close-join-"+mode) })
		}
	})
}

func cleanupExecutorNativeCase(t *testing.T, recovery ...string) {
	t.Helper()
	require.LessOrEqual(t, len(recovery), 1)
	require.Zero(t, os.Geteuid())
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	dir, err := os.MkdirTemp("/tmp", "cleanup-executor-")
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
	_, err = admin.RoleAdd(ctx, "cleanup-executor-write")
	require.NoError(t, err)
	_, err = admin.RoleGrantPermission(ctx, "cleanup-executor-write", "/sandbox/observer/v1/", clientv3.GetPrefixRangeEnd("/sandbox/observer/v1/"), clientv3.PermissionType(clientv3.PermReadWrite))
	require.NoError(t, err)
	_, err = admin.UserGrantRole(ctx, "server", "cleanup-executor-write")
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
	binding := p.TrustBinding{Namespace: "/sandbox/observer/v1/", AuthorityID: "cleanup-executor", Target: "kubernetes", RestoreEpoch: "restore"}
	ns, err := state.NewNamespace("/sandbox", "observer", "v1")
	require.NoError(t, err)
	identity := state.Identity{SchemaVersion: 1, Prefix: "/sandbox", AuthorityID: "observer", Cell: "v1", ClusterID: cluster, StorageID: "cleanup-executor-storage", RuntimeID: binding.AuthorityID, RestoreEpoch: binding.RestoreEpoch}
	identityWire := observerNativeJSON(t, identity)
	_, err = admin.Put(ctx, binding.Namespace+"meta/identity", string(identityWire))
	require.NoError(t, err)
	_, err = admin.Put(ctx, binding.Namespace+"meta/restore_epoch", binding.RestoreEpoch)
	require.NoError(t, err)
	profile := fmt.Sprintf(`{"version":1,"profile_id":"partition-worker-v1","namespace":%q,"restore_epoch":%q,"identity_digest":%q,"partition_count":256,"relist_slots":4,"range_slots":20,"range_bytes_per_second":1048576,"range_burst_bytes":262144,"claim_slots":100,"claim_per_second":1,"claim_burst":1}`, binding.Namespace, binding.RestoreEpoch, hashBytes(identityWire))
	_, err = admin.Put(ctx, binding.Namespace+"meta/worker_control_budget", profile)
	require.NoError(t, err)
	image := "fixture.test/sandbox@sha256:" + strings.Repeat("a", 64)
	contract, err := kubecontract.EnrolledPlainContractDigest(image)
	require.NoError(t, err)
	options := state.Options{Endpoints: []string{"https://127.0.0.1:2379"}, Namespace: ns, Identity: identity, TLS: appTLS, PublicationTrust: &state.RuntimePublicationTrust{AuthorityID: binding.AuthorityID, Target: binding.Target, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, Clock: nativeFixtureClock{}, EnrolledPublication: &state.EnrolledPublicationOptions{ContractDigest: contract, MaxActive: 32}, RequestTimeout: 2 * time.Second}
	b, err := state.New(ctx, options)
	require.NoError(t, err)
	defer b.Close()
	workspace, err := state.NewWorkspaceIdentity("local", identity.StorageID, "bucket", "cleanup-executor/")
	require.NoError(t, err)
	plan, err := state.NewTaskWorkspaceSyncPlan(state.TaskWorkspaceSyncPlanOptions{Workspace: workspace, DeleteExtraneous: true})
	require.NoError(t, err)
	input := state.AcquireIntentInput{Workspace: workspace, Principal: "cleanup-executor", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"fixture":"cleanup-executor"}`), LocalCleanupPlan: &plan, Now: time.Now().UTC(), TTL: time.Hour}
	acquired, err := b.AcquireIntent(ctx, input)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, acquired.Outcome)
	creation, err := b.ClaimCreation(ctx, workspace, input.IntentID, "cleanup-native-create", time.Minute)
	require.NoError(t, err)
	defer b.ReleaseCreationClaim(context.Background(), creation)
	dispatch, err := b.DeclareRuntimeDispatch(ctx, creation, state.RuntimeDispatchInput{Kind: state.RuntimeDispatchCreate, Target: binding.Target, Payload: input.Payload})
	require.NoError(t, err)
	nb, na := time.Now().UTC().Add(-30*time.Second), time.Now().UTC().Add(5*time.Minute)
	observerCert, err := p.SignNodeObserverCertificate(root, p.NodeObserverCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "node_observer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, NodeUID: nodeUID, NodeBootID: boot, PublicKey: observerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	issuerCert, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	config := launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch}, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, UID: 1000, GID: 1000, ContractDigest: contract, ClockSocket: "/run/sandbox-clock/clock.sock", ClockPublicKey: root.Public().(ed25519.PublicKey), ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: nodeUID, NodeBootID: boot}}
	pod, err := k.BuildEnrolledPlainPod("runtimes", k.EnrolledPlainPodRequest{Spec: sandboxruntime.SandboxSpec{ID: "cleanup-pod", Image: image, CPU: "500m", Memory: "128Mi", Disk: "1Mi", TmpDisk: "1Mi", ReadOnlyRootFS: true, RunAsUser: 1000, Labels: map[string]string{"sandbox.id": input.SandboxID}}, NodeName: "node-a", NodeUID: nodeUID, NodeBootID: boot, CreationIntentID: input.IntentID, Generation: creation.Reference().Generation, Config: observerNativeJSON(t, config)})
	require.NoError(t, err)
	require.Equal(t, []string{kubecontract.EnrolledRetainFinalizer}, pod.Finalizers, "production enrolled Pod starts retained")
	pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	pod.UID, pod.ResourceVersion = types.UID(uuid.NewString()), "1"
	node := &v1.Node{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"}, ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID(nodeUID)}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: boot, OperatingSystem: "linux", Architecture: runtime.GOARCH}}}
	apiState := &cleanupExecutorAPI{pod: pod, node: node, connections: make(map[net.Conn]struct{})}
	api := httptest.NewServer(http.HandlerFunc(apiState.serve))
	defer func() {
		apiState.close()
		api.Close()
	}()
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
		serveErr := <-served
		require.True(t, serveErr == nil || errors.Is(serveErr, context.Canceled) || errors.Is(serveErr, net.ErrClosed), "%v", serveErr)
	}()
	coordinator, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "coordinator.sock"), Net: "unix"})
	require.NoError(t, err)
	defer coordinator.Close()
	owned := filepath.Join(mount, "cleanup-executor-"+uuid.NewString())
	require.NoError(t, os.Mkdir(owned, 0700))
	defer func() {
		wire, err := os.ReadFile(filepath.Join(owned, "cgroup.events"))
		require.NoError(t, err)
		require.Contains(t, string(wire), "populated 0\n")
		require.NoError(t, os.Remove(owned))
	}()
	s := cleanupExecutorSetup{localReleaseNativeSetup: localReleaseNativeSetup{Binding: binding, Root: root.Public().(ed25519.PublicKey), IssuerKey: issuerKey, IssuerCertificate: issuerCert, ObserverCertificate: observerCert, Identity: identity, WorkspaceProvider: "local", WorkspaceStorage: identity.StorageID, WorkspaceBucket: "bucket", WorkspacePrefix: "cleanup-executor/", SandboxID: input.SandboxID, IntentID: input.IntentID, Contract: contract, NodeUID: nodeUID, NodeBootID: boot, Directory: dir, Coordinator: coordinator.Addr().String(), EnrollmentSocket: uds.Addr().String(), CgroupMount: mount, ObserverAddress: tcp.Addr().String(), ProviderAddress: api.URL, BudgetID: uuid.NewString()}, Role: "supervisor"}
	setupPath := filepath.Join(dir, "supervisor.json")
	require.NoError(t, os.WriteFile(setupPath, observerNativeJSON(t, s), 0600))
	fd, err := unix.Open(owned, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	child := exec.Command(os.Args[0], "-test.run=^TestCleanupExecutorWorkerChild$", "-test.v", "-test.timeout=140s")
	child.Env = append(os.Environ(), "CLEANUP_EXECUTOR_CHILD_SETUP="+setupPath)
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS, UseCgroupFD: true, CgroupFD: fd}
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	require.NoError(t, child.Start())
	require.NoError(t, unix.Close(fd))
	waited := false
	defer func() {
		if !waited {
			_ = child.Process.Kill()
			t.Logf("failed-case supervisor cleanup Wait=%v", child.Wait())
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
	ri := p.RuntimeIdentityContext{SandboxID: input.SandboxID, WorkspaceHash: workspace.Hash(), Generation: creation.Reference().Generation, Runtime: p.RuntimeReference{ID: pod.Name, UID: string(pod.UID), BootID: birth.BootID}}
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
	require.NoError(t, b.ReleaseCreationClaim(ctx, creation))
	apiState.mu.Lock()
	apiState.address = reply.Address
	apiState.mu.Unlock()
	controlKey, err := ns.Key("p", fmt.Sprintf("%02x", workspace.Partition()), "controls", input.SandboxID)
	require.NoError(t, err)
	control, err := admin.Get(ctx, controlKey)
	require.NoError(t, err)
	require.Len(t, control.Kvs, 1)
	stage, task, err := b.PrepareDestroy(ctx, state.BeginDestroyInput{SandboxID: input.SandboxID, RequestID: uuid.NewString(), ExpectedControlRevision: control.Kvs[0].ModRevision}, time.Minute)
	require.NoError(t, err)
	outcome, err := b.CommitStage(ctx, stage)
	require.NoError(t, err)
	require.Equal(t, state.OutcomeCommitted, outcome)
	s.Role, s.Task = "worker", task
	if len(recovery) == 1 {
		if strings.HasPrefix(recovery[0], "close-join-") {
			s.CloseMode = strings.TrimPrefix(recovery[0], "close-join-")
		} else {
			s.Recovery = recovery[0]
		}
	}
	s.Runtime, s.RuntimeCertificate, s.RuntimeAddress = ri.Runtime, runtimeCert, reply.Address
	s.Generation = ri.Generation
	_, err = admin.Put(ctx, binding.Namespace+"meta/workspace_sync_recycling_budget", fmt.Sprintf(`{"version":2,"budget_id":%q,"slots_per_partition":1,"max_intent_bytes":16384}`, s.BudgetID))
	require.NoError(t, err)
	slotKey := fmt.Sprintf("%sp/%02x/workspace-sync-recycling-slots/%s/00", binding.Namespace, task.Partition, s.BudgetID)
	_, err = admin.Put(ctx, slotKey, fmt.Sprintf(`{"version":2,"budget_id":%q,"partition":%d,"slot":0,"state":"free","intent_id":"","intent_digest":"","intent_birth":0,"evidence_digest":""}`, s.BudgetID, task.Partition))
	require.NoError(t, err)
	if s.CloseMode != "" {
		cleanupExecutorCloseParent(t, ctx, s, admin, apiState, child, &waited)
		return
	}
	if s.Recovery != "" {
		var observerProxy *cleanupExecutorObserverProxy
		if s.Recovery == "observer-ack-recovery" {
			observerProxy = newCleanupExecutorObserverProxy(t, ctx, s, observerKey)
			defer observerProxy.close(t)
			s.ObserverAddress = observerProxy.listener.Addr().String()
		}
		cleanupExecutorRunRecoveryWorkers(t, ctx, s, admin, apiState, observerProxy)
	} else {
		workerPath := filepath.Join(dir, "worker.json")
		require.NoError(t, os.WriteFile(workerPath, observerNativeJSON(t, s), 0600))
		worker := exec.Command(os.Args[0], "-test.run=^TestCleanupExecutorWorkerChild$", "-test.v", "-test.timeout=110s")
		worker.Env = append(os.Environ(), "CLEANUP_EXECUTOR_CHILD_SETUP="+workerPath)
		worker.Stdout, worker.Stderr = os.Stdout, os.Stderr
		require.NoError(t, worker.Start())
		workerErr := worker.Wait()
		t.Logf("original cleanup worker Wait=%v", workerErr)
		require.NoError(t, workerErr)
	}
	childErr := child.Wait()
	waited = true
	var exited *exec.ExitError
	require.ErrorAs(t, childErr, &exited)
	require.Equal(t, syscall.SIGKILL, exited.Sys().(syscall.WaitStatus).Signal())
	executionKey := fmt.Sprintf("%sp/%02x/tasks/%s/execution", binding.Namespace, task.Partition, task.TaskID)
	execution, err := admin.Get(ctx, executionKey)
	require.NoError(t, err)
	require.Len(t, execution.Kvs, 1)
	var row struct {
		Phase, Confirmed string
		Run              struct {
			Joined bool
		}
		Release *struct {
			CreateRevision int64 `json:"create_revision"`
			Digest         string
		}
	}
	require.NoError(t, json.Unmarshal(execution.Kvs[0].Value, &row))
	require.Equal(t, "settled", row.Phase)
	require.Equal(t, "settled", row.Confirmed)
	require.True(t, row.Run.Joined)
	require.NotNil(t, row.Release)
	occupied, err := admin.Get(ctx, binding.Namespace+"cleanup-execution-slots/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.Empty(t, occupied.Kvs)
	apiState.mu.Lock()
	patches, deletes, upgrades, absent := apiState.patches, apiState.deletes, apiState.upgrades, apiState.pod == nil
	apiState.mu.Unlock()
	// The production Pod is already retained; only finalizer removal writes a
	// PATCH. Installation still uses the original guarded Apply and readback.
	require.Equal(t, 1, patches)
	require.Equal(t, 1, deletes)
	require.Positive(t, upgrades)
	require.True(t, absent)
	t.Logf("actual exact provider patches=%d deletes=%d bridge upgrades=%d; permanent settled E and joined Run, no occupied S", patches, deletes, upgrades)
}
