//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
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
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/goairix/sandbox/internal/nodeobserver"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	"github.com/goairix/sandbox/internal/runtime/controltarget"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
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

type enrolledNativeSetup struct {
	Binding                                                                 p.TrustBinding
	Root                                                                    ed25519.PublicKey
	Issuer                                                                  ed25519.PrivateKey
	Socket, Coordinator, WritableMount, Contract, NodeUID, NodeBootID, Mode string
}

type enrolledNativeCommand struct {
	Action      string
	Request     p.NodeEnrollmentRequest
	Certificate []byte
	Claim       p.ClaimReference
}

type enrolledNativeReply struct {
	State      string
	Proof      []byte
	Enrollment p.EnrolledRuntimeEvidence
}

// Pause the actual Journal.InstallActivation's final clock observation, after
// its open gate was durably persisted. This is a test clock, not an enrollment
// or kernel-success callback. Release is explicit even after cancellation so
// the test can distinguish cancellation from joining the original borrower.
type enrolledJournalPauseClock struct {
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (c *enrolledJournalPauseClock) Observe(ctx context.Context) (p.ClockObservation, error) {
	now := time.Now().UTC()
	if c.calls.Add(1) == 6 {
		close(c.entered)
		<-c.release
	}
	return p.ClockObservation{UTC: now, Uncertainty: time.Millisecond}, ctx.Err()
}

// Same independently sealed private PID/mount/proc/cgroup profile as the tracked
// observer fixture, now constructing the actual immutable enrolled Supervisor.
func TestEnrolledSupervisorNativeChild(t *testing.T) {
	path := os.Getenv("ENROLLED_NATIVE_CHILD_SETUP")
	if path == "" {
		return
	}
	require.Equal(t, 1, os.Getpid())
	wire, err := os.ReadFile(path)
	require.NoError(t, err)
	var setup enrolledNativeSetup
	require.NoError(t, json.Unmarshal(wire, &setup))
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	require.NoError(t, unix.Unmount(setup.WritableMount, 0))
	require.NoError(t, unix.Unmount("/proc", unix.MNT_DETACH))
	require.NoError(t, unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""))
	for _, cap := range []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SYS_PTRACE} {
		_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, cap, 0, 0, 0, 0)
		require.Zero(t, errno)
	}
	header := &unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := &[2]unix.CapUserData{{Permitted: 0x1e0, Effective: 0x1e0}}
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(header)), uintptr(unsafe.Pointer(data)), 0)
	runtime.KeepAlive(header)
	runtime.KeepAlive(data)
	require.Zero(t, errno)
	kernel, err := launcher.BootstrapPID1()
	require.NoError(t, err)
	verifier, err := p.NewManagementVerifier(setup.Binding, []ed25519.PublicKey{setup.Root})
	require.NoError(t, err)
	var clock p.AuthorityClock = nativeFixtureClock{}
	var paused *enrolledJournalPauseClock
	if strings.HasPrefix(setup.Mode, "journal-") {
		paused = &enrolledJournalPauseClock{entered: make(chan struct{}), release: make(chan struct{})}
		clock = paused
	}
	journalDir := filepath.Join(filepath.Dir(path), "supervisor-journal")
	s, err := NewEnrolledSupervisor(SupervisorOptions{Kernel: kernel, UID: 1000, GID: 1000, ContractDigest: setup.Contract, Verifier: verifier, Clock: clock, JournalDirectory: journalDir, Executable: "/fixture/monitor"}, NodeEnrollmentOptions{SocketPath: setup.Socket, NodeUID: setup.NodeUID, NodeBootID: setup.NodeBootID})
	require.NoError(t, err)
	defer s.Close()
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: setup.Coordinator, Net: "unix"})
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(25*time.Second)))
	require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, s.Birth())))
	read := func() enrolledNativeCommand {
		wire, err := ct.ReadNodeObserverFrame(conn)
		require.NoError(t, err)
		var command enrolledNativeCommand
		require.NoError(t, json.Unmarshal(wire, &command))
		return command
	}
	command := read()
	a, err := verifier.VerifyTargetActivation(command.Request.Activation, command.Request.RuntimeCertificate, command.Request.IssuerCertificate, s.Birth(), time.Now().UTC())
	require.NoError(t, err)
	activated := make(chan error, 1)
	go func() { activated <- s.Activate(context.Background(), a) }()
	command = read()
	require.Equal(t, "pending", command.Action)
	_, err = s.accept(context.Background(), &authenticatedStart{supervisor: s})
	require.ErrorIs(t, err, ErrAdmissionClosed)
	children, err := filepath.Glob("/proc/1/task/*/children")
	require.NoError(t, err)
	require.NotEmpty(t, children)
	for _, path := range children {
		wire, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Empty(t, strings.TrimSpace(string(wire)), "actual pending PID1 has a child")
	}
	require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, enrolledNativeReply{State: "pending-no-child"})))
	command = read()
	if paused != nil {
		require.Equal(t, "journal", command.Action)
		select {
		case <-paused.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("actual Journal.InstallActivation did not reach final observation")
		}
		gateState := func() string {
			wire, err := os.ReadFile(filepath.Join(journalDir, "gate.json"))
			require.NoError(t, err)
			var gate struct {
				State string `json:"gate_state"`
			}
			require.NoError(t, json.Unmarshal(wire, &gate))
			return gate.State
		}
		require.Equal(t, "open", gateState(), "actual persisted gate must precede pause")
		require.True(t, s.mu.TryLock(), "clock I/O must not hold Supervisor.mu")
		require.False(t, s.admission)
		require.Nil(t, s.journal)
		require.Nil(t, s.activation)
		owner := s.enrollment
		s.mu.Unlock()
		if setup.Mode == "journal-close-admission" {
			require.NoError(t, s.CloseAdmission(context.Background()))
			close(paused.release)
			require.Error(t, <-activated)
			require.False(t, s.admission)
			require.Nil(t, s.journal)
			require.Nil(t, s.activation)
			require.Equal(t, "closed", gateState())
			_, err := s.EnrolledPublicationProof(context.Background(), command.Certificate, command.Claim)
			require.Error(t, err)
			require.NoError(t, s.Close())
			require.Equal(t, make([]byte, ed25519.PrivateKeySize), []byte(s.key))
			require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, enrolledNativeReply{State: "journal-seal-cannot-install"})))
			return
		}
		closed := make(chan error, 1)
		go func() { closed <- s.Close() }()
		select {
		case <-owner.context.Done():
		case <-time.After(500 * time.Millisecond):
			t.Fatal("Close did not cancel original journal owner")
		}
		select {
		case err := <-closed:
			t.Fatalf("Close returned before original journal borrower: %v", err)
		default:
		}
		s.mu.Lock()
		require.NotEqual(t, make([]byte, ed25519.PrivateKeySize), []byte(s.key))
		s.mu.Unlock()
		require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, enrolledNativeReply{State: "journal-close-still-joining"})))
		require.Equal(t, "release-journal", read().Action)
		close(paused.release)
		require.Error(t, <-activated)
		require.NoError(t, <-closed)
		require.Equal(t, make([]byte, ed25519.PrivateKeySize), []byte(s.key))
		require.False(t, s.admission)
		require.Nil(t, s.activation)
		require.Nil(t, s.journal)
		_, err := s.EnrolledPublicationProof(context.Background(), command.Certificate, command.Claim)
		require.ErrorIs(t, err, ErrAdmissionClosed)
		_, err = s.accept(context.Background(), &authenticatedStart{supervisor: s})
		require.ErrorIs(t, err, ErrAdmissionClosed)
		// Cancellation poisons the uninstalled live journal. Its open diagnostic
		// bytes are retained; the existing cold opener durably closes them and
		// never reconstructs a fresh activation capability.
		require.Equal(t, "open", gateState())
		wire, err := os.ReadFile(filepath.Join(journalDir, "gate.json"))
		require.NoError(t, err)
		var manifest controltarget.GateManifest
		require.NoError(t, json.Unmarshal(wire, &manifest))
		cold, err := controltarget.OpenClosedJournal(context.Background(), controltarget.JournalOptions{Directory: journalDir, Birth: &s.birth, Verifier: verifier, Clock: nativeFixtureClock{}, Identity: manifest.Identity, DataGateEpoch: a.DataGateEpoch(), ManagementUID: 0})
		require.NoError(t, err)
		require.Equal(t, "closed", cold.Status().Gate.GateState)
		require.Equal(t, "closed", gateState())
		require.Error(t, cold.InstallActivation(context.Background(), a))
		require.NoError(t, cold.Close())
		require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, enrolledNativeReply{State: "journal-close-joined-key-zero"})))
		return
	}
	if setup.Mode == "close" {
		require.Equal(t, "close", command.Action)
		require.NoError(t, s.Close())
		require.Error(t, <-activated)
		require.Equal(t, make([]byte, ed25519.PrivateKeySize), []byte(s.key))
		require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, enrolledNativeReply{State: "closed-joined-key-zero"})))
		return
	}
	if setup.Mode == "close-admission" {
		require.Equal(t, "close-admission", command.Action)
		require.NoError(t, s.CloseAdmission(context.Background()))
		require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, enrolledNativeReply{State: "admission-sealed"})))
		command = read()
		require.Equal(t, "finish", command.Action)
		require.Error(t, <-activated)
		require.False(t, s.admission)
		_, err := s.EnrolledPublicationProof(context.Background(), command.Certificate, command.Claim)
		require.Error(t, err)
		require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, enrolledNativeReply{State: "late-ACK-cannot-open"})))
		return
	}
	require.Equal(t, "ready", command.Action)
	require.NoError(t, <-activated)
	proof, err := s.EnrolledPublicationProof(context.Background(), command.Certificate, command.Claim)
	require.NoError(t, err)
	enrollment, err := s.Enrollment()
	require.NoError(t, err)
	require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, enrolledNativeReply{State: "ready", Proof: proof, Enrollment: enrollment})))
	command = read()
	require.Equal(t, "execute", command.Action)
	issuer, err := verifier.VerifyCommandIssuerCertificate(a.IssuerCertificate(), time.Now().UTC())
	require.NoError(t, err)
	f := nativeFixture{s: s, issuer: setup.Issuer, issuerWire: a.IssuerCertificate(), issuerIdentity: issuer}
	execution, _ := f.start(t, p.ExecutionRequest{Argv: []string{"/bin/sh", "-c", "printf enrolled-user; sleep 1"}, Env: map[string]string{}, UID: 1000, GID: 1000, WorkDir: "/tmp", TimeoutSeconds: 3}, 4*time.Second)
	observeActualUser(t, execution)
	stdout, stderr := drainExecution(t, execution)
	require.Equal(t, "enrolled-user", string(stdout))
	require.Empty(t, stderr)
	require.NoError(t, execution.waitErr)
	execution.mu.Lock()
	require.Equal(t, "local_terminal", execution.state)
	require.True(t, execution.record.DrainConfirmed)
	require.Zero(t, execution.record.RootWaitStatus)
	execution.mu.Unlock()
	t.Log("actual controlled UID1000 user + fixed monitor original Cmd.Wait=exit0, terminal drain joined")
	require.NoError(t, s.Close())
	require.Equal(t, make([]byte, ed25519.PrivateKeySize), []byte(s.key))
	require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, enrolledNativeReply{State: "executed-and-closed"})))
}

func TestEnrolledSupervisorNative(t *testing.T) {
	runEnrolledSupervisorNative(t, []string{"active", "close-admission", "close"}, true)
}

func TestEnrolledSupervisorNativeJournal(t *testing.T) {
	runEnrolledSupervisorNative(t, []string{"journal-close-admission", "journal-close"}, false)
}

func runEnrolledSupervisorNative(t *testing.T, modes []string, withBackend bool) {
	if os.Getenv("SANDBOX_ENROLLED_PUBLICATION_NATIVE") != "1" {
		t.Skip("requires exact owned enrolled Linux fixture")
	}
	require.Zero(t, os.Geteuid())
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	dir, err := os.MkdirTemp("/tmp", "enrolled-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	mount := filepath.Join(dir, "cgroup")
	require.NoError(t, os.Mkdir(mount, 0700))
	require.NoError(t, unix.Mount("none", mount, "cgroup2", 0, ""))
	t.Cleanup(func() { require.NoError(t, unix.Unmount(mount, 0)) })
	admin, observerTLS, cluster := observerNativeEtcd(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err = admin.UserAddWithOptions(ctx, "server", "", &clientv3.UserAddOptions{NoPassword: true})
	require.NoError(t, err)
	_, err = admin.RoleAdd(ctx, "enrolled-write")
	require.NoError(t, err)
	for _, prefix := range []string{"/sandbox/observer/v1/", "/sandbox/enrolled-native/"} {
		_, err = admin.RoleGrantPermission(ctx, "enrolled-write", prefix, clientv3.GetPrefixRangeEnd(prefix), clientv3.PermissionType(clientv3.PermReadWrite))
		require.NoError(t, err)
	}
	_, err = admin.UserGrantRole(ctx, "server", "enrolled-write")
	require.NoError(t, err)
	appCertificate, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"))
	require.NoError(t, err)
	appTLS := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: observerTLS.RootCAs, Certificates: []tls.Certificate{appCertificate}}
	_, root, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, issuerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, observerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	contract := hashBytes([]byte("exact-owned-enrolled-supervisor-contract-v2"))
	binding := p.TrustBinding{Namespace: "/sandbox/observer/v1/", AuthorityID: "enrolled-runtime", Target: "kubernetes", RestoreEpoch: "restore"}
	namespace, err := state.NewNamespace("/sandbox", "observer", "v1")
	require.NoError(t, err)
	identity := state.Identity{SchemaVersion: 1, Prefix: "/sandbox", AuthorityID: "observer", Cell: "v1", ClusterID: cluster, StorageID: "enrolled-storage", RuntimeID: binding.AuthorityID, RestoreEpoch: binding.RestoreEpoch}
	identityWire := observerNativeJSON(t, identity)
	_, err = admin.Put(ctx, binding.Namespace+"meta/identity", string(identityWire))
	require.NoError(t, err)
	_, err = admin.Put(ctx, binding.Namespace+"meta/restore_epoch", binding.RestoreEpoch)
	require.NoError(t, err)
	backendOptions := state.Options{Endpoints: []string{"https://127.0.0.1:2379"}, Namespace: namespace, Identity: identity, TLS: appTLS, PublicationTrust: &state.RuntimePublicationTrust{AuthorityID: binding.AuthorityID, Target: binding.Target, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, Clock: nativeFixtureClock{}, EnrolledPublication: &state.EnrolledPublicationOptions{ContractDigest: contract, MaxActive: 4}, RequestTimeout: time.Second}
	backend, err := state.New(ctx, backendOptions)
	require.NoError(t, err)
	defer backend.Close()
	bootWire, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	require.NoError(t, err)
	boot, nodeUID := strings.TrimSpace(string(bootWire)), uuid.NewString()
	now := time.Now().UTC()
	nb, na := now.Add(-30*time.Second), now.Add(5*time.Minute)
	observerCert, err := p.SignNodeObserverCertificate(root, p.NodeObserverCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "node_observer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, NodeUID: nodeUID, NodeBootID: boot, PublicKey: observerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	issuerCert, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			caseDir := filepath.Join(dir, mode)
			require.NoError(t, os.Mkdir(caseDir, 0700))
			workspace, err := state.NewWorkspaceIdentity("s3", identity.StorageID, "bucket", mode+"/")
			require.NoError(t, err)
			input := state.AcquireIntentInput{Workspace: workspace, Principal: "fixture", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), Payload: json.RawMessage(`{"mode":"plain"}`), Now: time.Now().UTC(), TTL: time.Hour}
			acquired, err := backend.AcquireIntent(ctx, input)
			require.NoError(t, err)
			require.Equal(t, state.OutcomeCommitted, acquired.Outcome)
			claim, err := backend.ClaimCreation(ctx, workspace, input.IntentID, "worker", 30*time.Second)
			require.NoError(t, err)
			defer backend.ReleaseCreationClaim(context.Background(), claim)
			dispatch, err := backend.DeclareRuntimeDispatch(ctx, claim, state.RuntimeDispatchInput{Kind: state.RuntimeDispatchCreate, Target: binding.Target, Payload: json.RawMessage(`{"mode":"plain"}`)})
			require.NoError(t, err)
			zero, no, yes := int64(0), false, true
			podName, podUID := "pod-"+mode, uuid.NewString()
			pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "runtimes", UID: types.UID(podUID), ResourceVersion: "1", Annotations: map[string]string{"sandbox.creation.intent": input.IntentID}}, Spec: v1.PodSpec{NodeName: "node-a", RestartPolicy: v1.RestartPolicyNever, SecurityContext: &v1.PodSecurityContext{SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}}, Containers: []v1.Container{{Name: "sandbox", SecurityContext: &v1.SecurityContext{RunAsUser: &zero, Privileged: &no, AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &v1.Capabilities{Drop: []v1.Capability{"ALL"}, Add: []v1.Capability{"KILL", "SETUID", "SETGID", "SETPCAP"}}}}}}}
			node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID(nodeUID)}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: boot, OperatingSystem: "linux", Architecture: runtime.GOARCH}}}
			barrier, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, http.MethodGet, r.Method)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/nodes/node-a" {
					_ = json.NewEncoder(w).Encode(node)
					return
				}
				if r.URL.Path == "/api/v1/namespaces/runtimes/pods/"+podName {
					if calls.Add(1) == 2 {
						close(barrier)
						select {
						case <-release:
						case <-r.Context().Done():
							return
						}
					}
					_ = json.NewEncoder(w).Encode(pod)
					return
				}
				w.WriteHeader(404)
			}))
			defer api.Close()
			observerJournal := filepath.Join(caseDir, "observer-journal")
			require.NoError(t, os.Mkdir(observerJournal, 0700))
			observer, err := nodeobserver.New(nodeobserver.Options{Binding: binding, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, NodeName: "node-a", NodeUID: nodeUID, Certificate: observerCert, PrivateKey: observerKey, Clock: nativeFixtureClock{}, EtcdEndpoints: []string{"https://127.0.0.1:2379"}, EtcdTLS: observerTLS, EtcdClusterID: cluster, EtcdIdentity: identityWire, KubernetesConfig: &rest.Config{Host: api.URL}, KubernetesNamespace: "runtimes", JournalDirectory: observerJournal, MaxSlots: 1, MaxActive: 2, RequestTimeout: 5 * time.Second, TerminationTimeout: 20 * time.Second})
			require.NoError(t, err)
			uds, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(caseDir, "enroll.sock"), Net: "unix"})
			require.NoError(t, err)
			require.NoError(t, os.Chmod(uds.Addr().String(), 0600))
			tcp, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			served := make(chan error, 1)
			go func() { served <- observer.Serve(ctx, uds, tcp) }()
			defer func() {
				require.NoError(t, observer.Close())
				err := <-served
				require.True(t, err == nil || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed), "actual observer Serve join: %v", err)
			}()
			coord, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(caseDir, "coord.sock"), Net: "unix"})
			require.NoError(t, err)
			defer coord.Close()
			owned := filepath.Join(mount, "enrolled-"+uuid.NewString())
			require.NoError(t, os.Mkdir(owned, 0700))
			defer func() {
				wire, err := os.ReadFile(filepath.Join(owned, "cgroup.events"))
				require.NoError(t, err)
				require.Contains(t, string(wire), "populated 0\n")
				require.NoError(t, os.Remove(owned))
			}()
			setup := enrolledNativeSetup{Binding: binding, Root: root.Public().(ed25519.PublicKey), Issuer: issuerKey, Socket: uds.Addr().String(), Coordinator: coord.Addr().String(), WritableMount: mount, Contract: contract, NodeUID: nodeUID, NodeBootID: boot, Mode: mode}
			setupPath := filepath.Join(caseDir, "child.json")
			require.NoError(t, os.WriteFile(setupPath, observerNativeJSON(t, setup), 0600))
			fd, err := unix.Open(owned, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			require.NoError(t, err)
			child := exec.Command(os.Args[0], "-test.run=^TestEnrolledSupervisorNativeChild$", "-test.v", "-test.timeout=30s")
			child.Env = append(os.Environ(), "ENROLLED_NATIVE_CHILD_SETUP="+setupPath)
			child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS, UseCgroupFD: true, CgroupFD: fd}
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			require.NoError(t, child.Start())
			require.NoError(t, unix.Close(fd))
			waited := false
			defer func() {
				if !waited {
					_ = child.Process.Kill()
					t.Logf("original enrolled Supervisor cleanup Cmd.Wait=%v", child.Wait())
				}
			}()
			require.NoError(t, coord.SetDeadline(time.Now().Add(15*time.Second)))
			conn, err := coord.AcceptUnix()
			require.NoError(t, err)
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(20*time.Second)))
			wire, err := ct.ReadNodeObserverFrame(conn)
			require.NoError(t, err)
			var birth p.BirthContext
			require.NoError(t, json.Unmarshal(wire, &birth))
			ri := p.RuntimeIdentityContext{SandboxID: input.SandboxID, WorkspaceHash: workspace.Hash(), Generation: claim.Reference().Generation, Runtime: p.RuntimeReference{ID: podName, UID: podUID, BootID: birth.BootID}}
			runtimeCert, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: birth.RuntimePublicKey, NotBefore: nb, NotAfter: na, SandboxID: ri.SandboxID, WorkspaceHash: ri.WorkspaceHash, Generation: ri.Generation, Runtime: ri.Runtime})
			require.NoError(t, err)
			activation, err := p.SignTargetActivation(root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: ri, DataGateEpoch: claim.Reference().DataGateEpoch, UID: 1000, GID: 1000, ContractDigest: contract, RuntimeCertificateDigest: hashBytes(runtimeCert), IssuerCertificateDigest: hashBytes(issuerCert), NotBefore: nb, NotAfter: na})
			require.NoError(t, err)
			d := dispatch.Entry.Record
			certificate, err := p.SignRuntimeCertificate(root, p.RuntimeCertificateClaims{Version: 1, Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IntentID: input.IntentID, SandboxID: input.SandboxID, WorkspaceHash: workspace.Hash(), Generation: ri.Generation, OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, Snapshot: p.SnapshotReference(d.Snapshot), ExpiresAt: d.ExpiresAt, Runtime: ri.Runtime, WorkspaceMode: "plain", PublicKey: birth.RuntimePublicKey, NotBefore: nb, NotAfter: na})
			require.NoError(t, err)
			_, err = backend.BindRuntime(ctx, claim, certificate)
			require.NoError(t, err)
			_, err = backend.ConsumeRuntimeMount(ctx, claim)
			require.NoError(t, err)
			send := func(c enrolledNativeCommand) {
				require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, c)))
			}
			read := func() enrolledNativeReply {
				wire, err := ct.ReadNodeObserverFrame(conn)
				require.NoError(t, err)
				var r enrolledNativeReply
				require.NoError(t, json.Unmarshal(wire, &r))
				return r
			}
			claimReference := p.ClaimReference{ClaimID: claim.Reference().ClaimID, CreateRevision: claim.Reference().CreateRevision, LeaseID: claim.Reference().LeaseID}
			send(enrolledNativeCommand{Action: "activate", Request: p.NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", Activation: activation, RuntimeCertificate: runtimeCert, IssuerCertificate: issuerCert}})
			select {
			case <-barrier:
			case <-time.After(3 * time.Second):
				t.Fatal("genuine observer second provider GET not reached")
			}
			send(enrolledNativeCommand{Action: "pending"})
			require.Equal(t, "pending-no-child", read().State)
			paths, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", child.Process.Pid))
			require.NoError(t, err)
			for _, path := range paths {
				wire, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Empty(t, strings.TrimSpace(string(wire)))
			}
			if strings.HasPrefix(mode, "journal-") {
				close(release)
				send(enrolledNativeCommand{Action: "journal", Certificate: certificate, Claim: claimReference})
				if mode == "journal-close" {
					require.Equal(t, "journal-close-still-joining", read().State)
					send(enrolledNativeCommand{Action: "release-journal"})
					require.Equal(t, "journal-close-joined-key-zero", read().State)
				} else {
					require.Equal(t, "journal-seal-cannot-install", read().State)
				}
			} else if mode == "close" {
				send(enrolledNativeCommand{Action: "close"})
				require.Equal(t, "closed-joined-key-zero", read().State)
				close(release)
			} else if mode == "close-admission" {
				send(enrolledNativeCommand{Action: "close-admission"})
				require.Equal(t, "admission-sealed", read().State)
				close(release)
				send(enrolledNativeCommand{Action: "finish", Certificate: certificate, Claim: claimReference})
				require.Equal(t, "late-ACK-cannot-open", read().State)
			} else {
				close(release)
				send(enrolledNativeCommand{Action: "ready", Certificate: certificate, Claim: claimReference})
				ready := read()
				require.Equal(t, "ready", ready.State)
				published, err := backend.PublishRuntimeEnrolled(ctx, claim, ready.Proof, ready.Enrollment)
				require.NoError(t, err)
				require.Equal(t, state.OutcomeCommitted, published.Outcome)
				require.NoError(t, published.GuardCleanupError)
				cold, err := state.New(ctx, backendOptions)
				require.NoError(t, err)
				entry, err := cold.LoadRuntimePublicationEnrolled(ctx, workspace, input.IntentID)
				require.NoError(t, err)
				require.Equal(t, published.Entry, entry)
				require.NoError(t, cold.Close())
				send(enrolledNativeCommand{Action: "execute"})
				require.Equal(t, "executed-and-closed", read().State)
			}
			err = child.Wait()
			waited = true
			require.NoError(t, err)
			t.Logf("original enrolled Supervisor mode=%s Cmd.Wait=exit0; actual cgroup emptiness checked before removal", mode)
		})
	}
	// The observer App certificate remains unable to modify its read-only prefix.
	readOnly, err := clientv3.New(clientv3.Config{Endpoints: []string{"https://127.0.0.1:2379"}, TLS: observerTLS, DialTimeout: time.Second})
	require.NoError(t, err)
	_, err = readOnly.Put(ctx, binding.Namespace+"must-not-write", "denied")
	require.Error(t, err)
	require.NoError(t, readOnly.Close())
	require.False(t, bytes.Equal(observerTLS.Certificates[0].Certificate[0], appCertificate.Certificate[0]))
	if withBackend {
		// One independently manifested test ELF shares this actual server using its
		// own exact-prefix App identity. No physical producer matrix is replayed.
		backendTests := exec.Command("/fixture/etcd-tests", "-test.run=^TestEnrolledPublicationNative$", "-test.v", "-test.timeout=60s")
		backendTests.Env = append(os.Environ(), "ENROLLED_NATIVE_TLS_DIRECTORY="+dir)
		backendTests.Stdout, backendTests.Stderr = os.Stdout, os.Stderr
		require.NoError(t, backendTests.Start())
		err = backendTests.Wait()
		t.Logf("original enrolled Backend native test ELF Cmd.Wait=%v", err)
		require.NoError(t, err)
	}
}

// Reuse the same TLS helper for a bounded metadata-only rerun when a new
// backend test changes. It does not replay the already proven Supervisor chain.
func TestEnrolledPublicationNativeFixture(t *testing.T) {
	if os.Getenv("SANDBOX_ENROLLED_PUBLICATION_NATIVE") != "1" {
		t.Skip("requires exact owned enrolled Linux fixture")
	}
	dir := t.TempDir()
	admin, _, _ := observerNativeEtcd(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := admin.UserAddWithOptions(ctx, "server", "", &clientv3.UserAddOptions{NoPassword: true})
	require.NoError(t, err)
	_, err = admin.RoleAdd(ctx, "enrolled-write")
	require.NoError(t, err)
	_, err = admin.RoleGrantPermission(ctx, "enrolled-write", "/sandbox/enrolled-native/", clientv3.GetPrefixRangeEnd("/sandbox/enrolled-native/"), clientv3.PermissionType(clientv3.PermReadWrite))
	require.NoError(t, err)
	_, err = admin.UserGrantRole(ctx, "server", "enrolled-write")
	require.NoError(t, err)
	child := exec.Command("/fixture/etcd-tests", "-test.run=^TestEnrolledPublicationNative$", "-test.v", "-test.timeout=60s")
	child.Env = append(os.Environ(), "ENROLLED_NATIVE_TLS_DIRECTORY="+dir)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	require.NoError(t, child.Start())
	err = child.Wait()
	t.Logf("original enrolled Backend native test ELF Cmd.Wait=%v", err)
	require.NoError(t, err)
}
