//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

// This helper only prepares the approved private kernel profile, then replaces
// itself with the actual production binary. It never calls serve or constructs
// a Supervisor. The production executable performs BootstrapPID1 itself.
func TestEnrolledLauncherNativeExec(t *testing.T) {
	mount := os.Getenv("ENROLLED_LAUNCHER_EXEC_CGROUP_MOUNT")
	if mount == "" {
		return
	}
	require.Equal(t, 1, os.Getpid())
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	require.NoError(t, unix.Unmount(mount, 0))
	require.NoError(t, unix.Unmount("/proc", unix.MNT_DETACH))
	require.NoError(t, unix.Mount("proc", "/proc", "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""))
	for _, capability := range []uintptr{unix.CAP_SYS_ADMIN, unix.CAP_SYS_PTRACE} {
		_, _, errno := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, capability, 0, 0, 0, 0)
		require.Zero(t, errno)
	}
	header := &unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := &[2]unix.CapUserData{{Permitted: 0x1e0, Effective: 0x1e0}}
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(header)), uintptr(unsafe.Pointer(data)), 0)
	runtime.KeepAlive(header)
	runtime.KeepAlive(data)
	require.Zero(t, errno)
	t.Log("exec formal /fixture/launcher serve as PID1 after private mount/cap preparation")
	require.NoError(t, syscall.Exec("/fixture/launcher", []string{"/fixture/launcher", "serve"}, os.Environ()))
}

func launcherNativeBootstrap(ctx context.Context, request ct.BootstrapRequest) (ct.BootstrapResponse, error) {
	var response ct.BootstrapResponse
	raw, err := (&net.Dialer{}).DialContext(ctx, "unix", ct.ControlSocket)
	if err != nil {
		return response, err
	}
	defer raw.Close()
	conn := raw.(*net.UnixConn)
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err == nil {
		err = ct.WriteBootstrap(conn, request)
	}
	if err == nil {
		err = conn.CloseWrite()
	}
	if err == nil {
		err = ct.ReadBootstrap(conn, &response)
	}
	return response, err
}

func TestEnrolledLauncherNative(t *testing.T) {
	runEnrolledLauncherNative(t, []string{"active", "wrong-node"})
}

func TestEnrolledLauncherNativeLiveGate(t *testing.T) {
	runEnrolledLauncherNative(t, []string{"live-gate"})
}

func runEnrolledLauncherNative(t *testing.T, modes []string) {
	runEnrolledLauncherNativeWithProfile(t, modes, false)
}

func runEnrolledLauncherNativeWithProfile(t *testing.T, modes []string, enrolledProfile bool) {
	enabled := "SANDBOX_ENROLLED_LAUNCHER_NATIVE"
	if enrolledProfile {
		enabled = "SANDBOX_ENROLLED_PROFILE_NATIVE"
	}
	if os.Getenv(enabled) != "1" {
		t.Skip("requires exact owned enrolled launcher Linux fixture")
	}
	require.Zero(t, os.Geteuid())
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	dir, err := os.MkdirTemp("/tmp", "enrolled-launcher-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	mount := filepath.Join(dir, "cgroup")
	require.NoError(t, os.Mkdir(mount, 0700))
	require.NoError(t, unix.Mount("none", mount, "cgroup2", 0, ""))
	t.Cleanup(func() { require.NoError(t, unix.Unmount(mount, 0)) })
	// Root approved this bounded private tmpfs solely for fixed protected paths.
	require.NoError(t, unix.Mount("tmpfs", "/run", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0700,size=1048576"))
	t.Cleanup(func() { require.NoError(t, unix.Unmount("/run", 0)) })
	controlMode := os.FileMode(0700)
	if enrolledProfile {
		controlMode = 0755
	}
	require.NoError(t, os.Mkdir(ct.ControlDirectory, controlMode))
	if enrolledProfile {
		require.NoError(t, os.Mkdir("/run/sandbox-node-observer", 0700))
	}
	require.NoError(t, os.Mkdir(filepath.Dir(ct.ClockSocket), 0700))
	admin, observerTLS, cluster := observerNativeEtcd(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	t.Cleanup(cancel)
	_, root, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, issuerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, observerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	clockPublic, clockKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	binding := p.TrustBinding{Namespace: "/sandbox/observer/v1/", AuthorityID: "launcher", Target: "target", RestoreEpoch: "restore"}
	verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	publicationVerifier, err := p.NewPublicationVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
	require.NoError(t, err)
	contract := hashBytes([]byte("formal-enrolled-launcher-v2"))
	if enrolledProfile {
		contract = profileFixtureContract(t)
	}
	bootWire, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	require.NoError(t, err)
	boot, nodeUID := strings.TrimSpace(string(bootWire)), uuid.NewString()
	now := time.Now().UTC()
	nb, na := now.Add(-time.Minute), now.Add(5*time.Minute)
	observerCert, err := p.SignNodeObserverCertificate(root, p.NodeObserverCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "node_observer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, NodeUID: nodeUID, NodeBootID: boot, PublicKey: observerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	issuerCert, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	identity := []byte(`{"schema_version":1,"prefix":"/sandbox/observer/v1/","authority_id":"launcher","cell":"fixture","cluster_id":"local","storage_id":"storage","runtime_id":"runtime"}`)
	_, err = admin.Put(ctx, binding.Namespace+"meta/identity", string(identity))
	require.NoError(t, err)
	_, err = admin.Put(ctx, binding.Namespace+"meta/restore_epoch", binding.RestoreEpoch)
	require.NoError(t, err)
	clockListener, err := net.Listen("unix", ct.ClockSocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = clockListener.Close() })
	require.NoError(t, os.Chmod(ct.ClockSocket, 0600))
	clockContext, stopClock := context.WithCancel(ctx)
	clockDone := make(chan error, 1)
	go func() {
		clockDone <- p.ServeSignedClock(clockContext, clockListener, p.SignedClockServerOptions{Binding: binding, Key: clockKey, Source: nativeFixtureClock{}, MaxConnections: 4})
	}()
	t.Cleanup(func() {
		stopClock()
		err := <-clockDone
		require.True(t, err == nil || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed), "original signed clock Serve join: %v", err)
	})
	for _, mode := range modes {
		t.Run(mode, func(t *testing.T) {
			caseDir := filepath.Join(dir, mode)
			require.NoError(t, os.Mkdir(caseDir, 0700))
			creation, podUID := uuid.NewString(), uuid.NewString()
			podName := "launcher-" + mode
			zero, no, yes := int64(0), false, true
			pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: "runtimes", UID: types.UID(podUID), ResourceVersion: "1", Annotations: map[string]string{"sandbox.creation.intent": creation}}, Spec: v1.PodSpec{NodeName: "node-a", RestartPolicy: v1.RestartPolicyNever, SecurityContext: &v1.PodSecurityContext{SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}}, Containers: []v1.Container{{Name: "sandbox", SecurityContext: &v1.SecurityContext{RunAsUser: &zero, Privileged: &no, AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &v1.Capabilities{Drop: []v1.Capability{"ALL"}, Add: []v1.Capability{"KILL", "SETUID", "SETGID", "SETPCAP"}}}}}}}
			if enrolledProfile {
				pod = profileFixturePod(t, binding, root.Public().(ed25519.PublicKey), clockPublic, nodeUID, boot, creation, podName, podUID, mode)
			}
			node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID(nodeUID)}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: boot, OperatingSystem: "linux", Architecture: runtime.GOARCH}}}
			var podReads atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/nodes/node-a":
					_ = json.NewEncoder(w).Encode(node)
				case "/api/v1/namespaces/runtimes/pods/" + podName:
					podReads.Add(1)
					_ = json.NewEncoder(w).Encode(pod)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(api.Close)
			journal := filepath.Join(caseDir, "observer-journal")
			require.NoError(t, os.Mkdir(journal, 0700))
			observer, err := nodeobserver.New(nodeobserver.Options{Binding: binding, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, NodeName: "node-a", NodeUID: nodeUID, Certificate: observerCert, PrivateKey: observerKey, Clock: nativeFixtureClock{}, EtcdEndpoints: []string{"https://127.0.0.1:2379"}, EtcdTLS: observerTLS, EtcdClusterID: cluster, EtcdIdentity: identity, KubernetesConfig: &rest.Config{Host: api.URL}, KubernetesNamespace: "runtimes", JournalDirectory: journal, MaxSlots: 1, MaxActive: 2, RequestTimeout: 5 * time.Second, TerminationTimeout: 20 * time.Second})
			require.NoError(t, err)
			var observerDone chan error
			t.Cleanup(func() {
				require.NoError(t, observer.Close())
				if observerDone != nil {
					err := <-observerDone
					require.True(t, err == nil || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed), "original observer Serve join: %v", err)
				}
			})
			enrollmentSocket := ct.EnrollmentSocket
			if enrolledProfile {
				enrollmentSocket = "/run/sandbox-node-observer/enrollment.sock"
			}
			uds, err := net.ListenUnix("unix", &net.UnixAddr{Name: enrollmentSocket, Net: "unix"})
			require.NoError(t, err)
			t.Cleanup(func() { _ = uds.Close() })
			require.NoError(t, os.Chmod(enrollmentSocket, 0600))
			if enrolledProfile {
				require.NoError(t, os.WriteFile(ct.EnrollmentSocket, nil, 0600))
			}
			tcp, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			t.Cleanup(func() { _ = tcp.Close() })
			observerDone = make(chan error, 1)
			go func() { observerDone <- observer.Serve(ctx, uds, tcp) }()
			configuredNode := nodeUID
			if mode == "wrong-node" {
				configuredNode = uuid.NewString()
			}
			bindingWire := observerNativeRecord(t, "namespace,authority_id,target,restore_epoch", binding.Namespace, binding.AuthorityID, binding.Target, binding.RestoreEpoch)
			enrollmentWire := observerNativeRecord(t, "node_uid,node_boot_id", configuredNode, boot)
			config := observerNativeRecord(t, "version,binding,roots,uid,gid,network_allowed,contract_digest,clock_socket,clock_public_key,clock_audience,max_active,enrollment", 2, json.RawMessage(bindingWire), []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, 1000, 1000, false, contract, ct.ClockSocket, clockPublic, "launcher", 4, json.RawMessage(enrollmentWire))
			if enrolledProfile {
				require.Equal(t, string(config), pod.Spec.Containers[0].Env[0].Value)
				profileNativeRefusals(t, mount, config)
			} else {
				require.NoError(t, os.WriteFile(ct.ControlDirectory+"/config.json", config, 0600))
			}
			owned := filepath.Join(mount, "launcher-"+uuid.NewString())
			require.NoError(t, os.Mkdir(owned, 0700))
			t.Cleanup(func() {
				wire, err := os.ReadFile(filepath.Join(owned, "cgroup.events"))
				require.NoError(t, err)
				require.Contains(t, string(wire), "populated 0\n")
				require.NoError(t, os.Remove(owned))
			})
			fd, err := unix.Open(owned, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			require.NoError(t, err)
			child := exec.Command(os.Args[0], "-test.run=^TestEnrolledLauncherNativeExec$", "-test.v", "-test.timeout=30s")
			child.Env = append(os.Environ(), "ENROLLED_LAUNCHER_EXEC_CGROUP_MOUNT="+mount)
			if enrolledProfile {
				child.Args = []string{os.Args[0], "-test.run=^TestEnrolledProfileNativeExec$", "-test.v", "-test.timeout=30s"}
				child.Env = append(child.Env, "SANDBOX_LAUNCHER_CONFIG_V2="+string(config), "ENROLLED_PROFILE_EXEC_MODE=active")
			}
			child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS, UseCgroupFD: true, CgroupFD: fd}
			child.Stdout, child.Stderr = os.Stdout, os.Stderr
			startErr := child.Start()
			closeErr := unix.Close(fd)
			require.NoError(t, startErr)
			wait := make(chan error, 1)
			go func() { wait <- child.Wait() }()
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = child.Process.Kill()
					t.Logf("original formal launcher cleanup Cmd.Wait=%v", <-wait)
				}
			})
			require.NoError(t, closeErr)
			waitChild := func() error {
				var err error
				select {
				case err = <-wait:
				case <-ctx.Done():
					_ = child.Process.Kill()
					err = errors.Join(ctx.Err(), <-wait)
				}
				waited = true
				return err
			}
			readyDeadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Lstat(ct.ControlSocket); err == nil {
					break
				}
				require.True(t, time.Now().Before(readyDeadline), "formal launcher control socket did not appear")
				time.Sleep(10 * time.Millisecond)
			}
			executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", child.Process.Pid))
			require.NoError(t, err)
			if enrolledProfile {
				require.Equal(t, "/usr/local/bin/sandbox-launcher", executable)
				profileNativeProvisioned(t, child.Process.Pid, config)
			} else {
				require.Equal(t, "/fixture/launcher", executable)
			}
			call, cancelCall := context.WithTimeout(ctx, 10*time.Second)
			defer cancelCall()
			hello, err := launcherNativeBootstrap(call, ct.BootstrapRequest{Version: 1, Purpose: "hello"})
			require.NoError(t, err)
			require.Equal(t, "closed_hello", hello.Purpose)
			require.NotNil(t, hello.Birth)
			ri := p.RuntimeIdentityContext{SandboxID: "sandbox-" + mode, WorkspaceHash: strings.Repeat("a", 64), Generation: 1, Runtime: p.RuntimeReference{ID: podName, UID: podUID, BootID: hello.Birth.BootID}}
			runtimeCert, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: hello.Birth.RuntimePublicKey, NotBefore: nb, NotAfter: na, SandboxID: ri.SandboxID, WorkspaceHash: ri.WorkspaceHash, Generation: ri.Generation, Runtime: ri.Runtime})
			require.NoError(t, err)
			activation, err := p.SignTargetActivation(root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: ri, DataGateEpoch: 1, UID: 1000, GID: 1000, ContractDigest: contract, RuntimeCertificateDigest: hashBytes(runtimeCert), IssuerCertificateDigest: hashBytes(issuerCert), NotBefore: nb, NotAfter: na})
			require.NoError(t, err)
			activated, err := launcherNativeBootstrap(call, ct.BootstrapRequest{Version: 1, Purpose: "activate", RuntimeCertificate: runtimeCert, IssuerCertificate: issuerCert, Activation: activation})
			if mode == "wrong-node" {
				require.Error(t, err)
				require.NotEqual(t, "activated", activated.Purpose)
				require.Positive(t, podReads.Load(), "real independent observer must reach enrollment identity observation")
			} else {
				require.NoError(t, err)
				require.Equal(t, "activated", activated.Purpose)
				claims := p.RuntimeCertificateClaims{Version: 1, Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IntentID: creation, SandboxID: ri.SandboxID, WorkspaceHash: ri.WorkspaceHash, Generation: 1, OperationID: uuid.NewString(), PayloadDigest: strings.Repeat("b", 64), Snapshot: p.SnapshotReference{Version: "snapshot", Digest: strings.Repeat("c", 64)}, ExpiresAt: na, Runtime: ri.Runtime, WorkspaceMode: "plain", PublicKey: hello.Birth.RuntimePublicKey, NotBefore: nb, NotAfter: na}
				certificate, err := p.SignRuntimeCertificate(root, claims)
				require.NoError(t, err)
				credential, err := p.NewManagementTLSCredential(issuerKey, issuerCert, "command_issuer")
				require.NoError(t, err)
				defer credential.Close()
				options := ct.DestinationOptions{Identity: ri, RuntimeCertificate: runtimeCert, Credential: credential, Verifier: verifier, Clock: nativeFixtureClock{}, Dial: func(ctx context.Context) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", ct.ControlSocket)
				}}
				destination, err := ct.NewDestination(options)
				require.NoError(t, err)
				claim := p.ClaimReference{ClaimID: uuid.NewString(), CreateRevision: 1, LeaseID: 2}
				enrollment, proof, err := destination.ReadEnrolledPublication(call, certificate, claim)
				require.NoError(t, err)
				publicationContext := p.PublicationContext{Certificate: p.CertificateContext{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IntentID: creation, SandboxID: ri.SandboxID, WorkspaceHash: ri.WorkspaceHash, Generation: 1, OperationID: claims.OperationID, PayloadDigest: claims.PayloadDigest, Snapshot: claims.Snapshot, ExpiresAt: claims.ExpiresAt}, Runtime: ri.Runtime, CertificateDigest: hashBytes(certificate), Claim: claim, DataGateEpoch: 1}
				evidence, err := publicationVerifier.VerifyEnrolledPublication(proof, publicationContext, enrollment, contract, time.Now().UTC())
				require.NoError(t, err)
				require.Equal(t, ri.Runtime, evidence.EnrollmentReceipt().Runtime)
				require.Equal(t, nodeUID, evidence.EnrollmentReceipt().NodeUID)
				require.True(t, bytes.Equal(activation, enrollment.Request.Activation))
				t.Logf("formal CLI PID1 original verifier authenticated enrolled ready=%s bytes=%d", evidence.Digest(), len(proof))
				if enrolledProfile {
					profileNativeCleanupPrepare(t, api.URL, binding, ri, creation, nodeUID, boot, contract, evidence.EnrollmentReceipt().EnrollmentDigest)
				}
				if mode == "live-gate" {
					// The fixture's original certified issuer only tightens the gate.
					// These fixed claims do not demonstrate a Backend creation claim.
					closeNow := time.Now().UTC()
					issuer, err := verifier.VerifyCommandIssuerCertificate(issuerCert, closeNow)
					require.NoError(t, err)
					closeContext := p.TaskCloseDataContext{Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IssuerCertificateID: issuer.CertificateID(), IssuerCertificateDigest: issuer.Digest(), CommandID: uuid.NewString(), TaskID: uuid.NewString(), TaskDigest: strings.Repeat("d", 64), ClaimID: uuid.NewString(), WorkerID: "launcher-fixture", SandboxID: ri.SandboxID, WorkspaceHash: ri.WorkspaceHash, Generation: ri.Generation, DataGateEpoch: 1, ControlRevision: 10, ClaimCreateRevision: 11, LeaseID: 12, Runtime: ri.Runtime}
					closeTicket, err := p.SignTaskCloseDataTicket(issuerKey, issuer, p.TaskCloseDataTicketClaims{Version: 1, Purpose: "task_close_data", Context: closeContext, NotBefore: closeNow.Add(-2 * time.Second), NotAfter: closeNow.Add(20 * time.Second)})
					require.NoError(t, err)
					closeEvidence, err := verifier.VerifyTaskCloseDataTicket(closeTicket, issuerCert, closeContext, closeNow)
					require.NoError(t, err)
					closeEnvelope := ct.TaskCloseEnvelope{Version: 1, Purpose: "task_close_data", Context: closeContext, TicketDigest: closeEvidence.Digest(), Ticket: closeTicket, IssuerCertificate: issuerCert}
					kind, closeReceipt := nativeTaskCall(t, destination, closeEnvelope)
					require.Equal(t, ct.EventReceipt, kind)
					closed, err := verifier.VerifyTaskDataClosedReceipt(closeReceipt, runtimeCert, closeContext, closeEvidence.Digest(), closeEvidence.NotBefore(), closeEvidence.NotAfter(), time.Now().UTC())
					require.NoError(t, err)
					require.Equal(t, "data_closed", closed.State())
					require.NoError(t, child.Process.Signal(syscall.Signal(0)))
					_, _, err = destination.ReadEnrolledPublication(call, certificate, claim)
					require.Error(t, err, "live but data-closed launcher must refuse new publication")
					closeEnvelope.Purpose = "task_close_data_query"
					closeEnvelope.Ticket, closeEnvelope.IssuerCertificate = nil, nil
					kind, retained := nativeTaskCall(t, destination, closeEnvelope)
					require.Equal(t, ct.EventReceipt, kind)
					_, err = verifier.VerifyTaskDataClosedReceipt(retained, runtimeCert, closeContext, closeEvidence.Digest(), closeEvidence.NotBefore(), closeEvidence.NotAfter(), time.Now().UTC())
					require.NoError(t, err, "same live TLS endpoint must retain authenticated closed-gate history")
					executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", child.Process.Pid))
					require.NoError(t, err)
					require.Equal(t, "/fixture/launcher", executable)
					t.Log("original signed data_closed receipt authenticated; same formal CLI alive with TLS history available; new publication refused")
				} else if !enrolledProfile {
					badCertificate := bytes.Clone(certificate)
					badCertificate[len(badCertificate)-2] ^= 1
					_, _, err = destination.ReadEnrolledPublication(call, badCertificate, claim)
					require.Error(t, err)
					wrongClaims := claims
					wrongClaims.Runtime.UID = uuid.NewString()
					wrongCertificate, err := p.SignRuntimeCertificate(root, wrongClaims)
					require.NoError(t, err)
					_, _, err = destination.ReadEnrolledPublication(call, wrongCertificate, claim)
					require.Error(t, err)
					_, wrongIssuerKey, err := ed25519.GenerateKey(rand.Reader)
					require.NoError(t, err)
					wrongIssuerWire, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: wrongIssuerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
					require.NoError(t, err)
					wrongCredential, err := p.NewManagementTLSCredential(wrongIssuerKey, wrongIssuerWire, "command_issuer")
					require.NoError(t, err)
					defer wrongCredential.Close()
					options.Credential = wrongCredential
					wrongDestination, err := ct.NewDestination(options)
					require.NoError(t, err)
					_, _, err = wrongDestination.ReadEnrolledPublication(call, certificate, claim)
					require.Error(t, err)
				}
				require.NoError(t, child.Process.Signal(syscall.SIGTERM))
				require.NoError(t, waitChild())
				if mode != "live-gate" && !enrolledProfile {
					_, _, err = destination.ReadEnrolledPublication(call, certificate, claim)
					require.Error(t, err)
					t.Log("wrong certificate/runtime/issuer and closed formal launcher refused publication")
				}
			}
			if !waited {
				require.NoError(t, child.Process.Signal(syscall.SIGTERM))
				require.NoError(t, waitChild())
			}
			t.Logf("original formal launcher mode=%s Cmd.Wait=exit0; actual cgroup empty checked before removal", mode)
		})
	}
}
