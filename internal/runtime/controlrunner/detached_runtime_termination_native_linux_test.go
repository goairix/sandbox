//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	"strings"
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
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

// This test-only JSON setup is mirrored by the etcd test ELF. Operator inputs
// and signed source material are ordinary configuration, not an in-process
// Claim/Run/authority capability. The terminal signer stays in NodeObserver.
type detachedPhysicalWorkerSetup struct {
	Version uint32

	// Native store configuration; no task capability crosses this boundary.
	Endpoints []string
	Identity  state.Identity

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

// Each phase acknowledges an actual completed API boundary. Creation/Task
// references carry attribution only; the child retains the original pointers.
type detachedPhysicalPhase struct {
	Phase string

	// Native source attribution returned by the worker's original APIs.
	IntentID      string
	SandboxID     string
	WorkspaceHash string
	Creation      state.CreationClaimReference
	Dispatch      state.RuntimeDispatchRecord

	// The parent forwards only original signed runtime material.
	Preparation    []byte
	Proof          []byte
	Enrollment     p.EnrolledRuntimeEvidence
	RuntimeAddress string

	Task state.TaskReference
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
	require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, phase)))
}

// The probe is a real UID1000 descendant of the protected PID1. Its original
// parent releases and joins it before ready: leaving a sleeping orphan would
// correctly fail the subsequent real quiescence ECHILD/no-descendants census.
func detachedPhysicalUserBoundary(t *testing.T) {
	t.Helper()
	require.Equal(t, 1, os.Getpid())
	ackRead, ackWrite, err := os.Pipe()
	require.NoError(t, err)
	defer ackRead.Close()
	releaseRead, releaseWrite, err := os.Pipe()
	require.NoError(t, err)
	defer releaseWrite.Close()
	user := exec.Command(os.Args[0], "-test.run=^TestDetachedRuntimeTerminationPhysicalUserChild$", "-test.v")
	user.Env = append(os.Environ(), "SANDBOX_DETACHED_PHYSICAL_USER=1")
	user.ExtraFiles = []*os.File{ackWrite, releaseRead}
	user.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 1000, NoSetGroups: true}}
	user.Stdout, user.Stderr = os.Stdout, os.Stderr
	require.NoError(t, user.Start())
	joined := false
	defer func() {
		if !joined {
			_ = user.Process.Kill()
			t.Logf("failed boundary user original Cmd.Wait=%v", user.Wait())
		}
	}()
	require.NoError(t, ackWrite.Close())
	require.NoError(t, releaseRead.Close())
	require.NoError(t, ackRead.SetReadDeadline(time.Now().Add(5*time.Second)))
	wire, err := ct.ReadNodeObserverFrame(ackRead)
	require.NoError(t, err)
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	require.NoError(t, err)
	require.Equal(t, cgroup, wire, "probe must inherit this actual PID1 cgroup")
	_, err = releaseWrite.Write([]byte{1})
	require.NoError(t, err)
	err = user.Wait()
	joined = true
	require.NoError(t, err)
	t.Logf("actual UID1000 descendant pid=%d has zero effective/permitted/inheritable/ambient capabilities and NNP; released and original Cmd.Wait joined before ready", user.Process.Pid)
}

// TestDetachedRuntimeTerminationPhysicalUserChild has both an exact selector
// and an explicit environment guard; ordinary test runs create no process.
func TestDetachedRuntimeTerminationPhysicalUserChild(t *testing.T) {
	if os.Getenv("SANDBOX_DETACHED_PHYSICAL_USER") != "1" {
		return
	}
	require.Equal(t, 1000, os.Geteuid())
	require.Equal(t, 1000, os.Getegid())
	require.Equal(t, 1, os.Getppid())
	// Like the existing archiveuser asset, this ordinary fixture must clear
	// inherited bits on all threads before probe IO. This initializes only the
	// user child; it neither changes the parent PID1 seal nor tests the managed
	// production monitor's user-execution admission path.
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var capabilities [2]unix.CapUserData
	_, _, errno := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&header)), uintptr(unsafe.Pointer(&capabilities[0])), 0)
	runtime.KeepAlive(&header)
	runtime.KeepAlive(&capabilities)
	require.Zero(t, errno)

	status, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "CapEff:") || strings.HasPrefix(line, "CapPrm:") || strings.HasPrefix(line, "CapInh:") || strings.HasPrefix(line, "CapAmb:") || strings.HasPrefix(line, "CapBnd:") {
			require.Equal(t, "0000000000000000", strings.TrimSpace(strings.SplitN(line, ":", 2)[1]))
		}
	}
	require.Contains(t, string(status), "NoNewPrivs:\t1\n")
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	require.NoError(t, err)
	ack := os.NewFile(3, "physical-user-ack")
	require.NotNil(t, ack)
	require.NoError(t, ct.WriteNodeObserverFrame(ack, cgroup))
	require.NoError(t, ack.Close())
	release := os.NewFile(4, "physical-user-release")
	require.NotNil(t, release)
	var token [1]byte
	_, err = io.ReadFull(release, token[:])
	require.NoError(t, err)
	require.Equal(t, byte(1), token[0])
	require.NoError(t, release.Close())
}

// This HTTPS fixture reuses the original exact Pod/node API behavior and the
// four formal owned policy objects. Native reads below are audit assertions,
// not authority supplied to the worker or a replacement provider writer.
type detachedPhysicalAPI struct {
	base     cleanupExecutorAPI
	policies map[string]*unstructured.Unstructured
	slots    map[string]int
	admin    *clientv3.Client
	ctx      context.Context
	task     state.TaskReference
	effects  [7]int
}

func (a *detachedPhysicalAPI) nativeIntent(slot int, uid, rv string) error {
	if a.task.TaskID == "" {
		return fmt.Errorf("no actual Task locator")
	}
	key := fmt.Sprintf("%sp/%02x/tasks/%s/runtime-provider-cleanup", a.task.Namespace, a.task.Partition, a.task.TaskID)
	response, err := a.admin.Get(a.ctx, key)
	if err != nil || len(response.Kvs) != 1 {
		return fmt.Errorf("missing native provider intent: %w", err)
	}
	var record struct {
		Slots [7]struct {
			State, UID      string
			EffectID        string `json:"effect_id"`
			ResourceVersion string `json:"resource_version"`
			Claim           *json.RawMessage
		}
	}
	if json.Unmarshal(response.Kvs[0].Value, &record) != nil {
		return fmt.Errorf("invalid native provider diagnostics")
	}
	s := record.Slots[slot]
	if s.State != "intent" || s.UID != uid || s.ResourceVersion != rv || s.EffectID == "" || s.Claim == nil {
		return fmt.Errorf("HTTP slot %d lacks original durable effect/UID/RV/Claim", slot)
	}
	if slot >= 2 {
		termination, err := a.admin.Get(a.ctx, strings.TrimSuffix(key, "runtime-provider-cleanup")+"runtime-termination")
		if err != nil || len(termination.Kvs) != 1 {
			return fmt.Errorf("missing physical termination fact: %w", err)
		}
		intent, err := p.DecodeTaskRuntimeTerminationIntent(termination.Kvs[0].Value)
		if err != nil || intent.Version != 2 || intent.Phase != "terminated" || len(intent.TerminalProof) == 0 {
			return fmt.Errorf("proof-dependent effect preceded real terminal proof")
		}
	}
	return nil
}

func (a *detachedPhysicalAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.base.mu.Lock()
	if slot, ok := a.slots[r.URL.Path]; ok {
		defer a.base.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		object := a.policies[r.URL.Path]
		if object == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: http.StatusNotFound})
			return
		}
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(object)
			return
		}
		var options metav1.DeleteOptions
		if r.Method != http.MethodDelete || json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384)).Decode(&options) != nil || options.Preconditions == nil || options.Preconditions.UID == nil || options.Preconditions.ResourceVersion == nil || string(*options.Preconditions.UID) != string(object.GetUID()) || *options.Preconditions.ResourceVersion != object.GetResourceVersion() {
			http.Error(w, "policy UID/RV precondition refused", http.StatusConflict)
			return
		}
		if err := a.nativeIntent(slot, string(object.GetUID()), object.GetResourceVersion()); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		a.effects[slot]++
		delete(a.policies, r.URL.Path)
		_ = json.NewEncoder(w).Encode(metav1.Status{Status: "Success", Code: http.StatusOK})
		return
	}
	if r.Method == http.MethodDelete || r.Method == http.MethodPatch {
		slot := 1
		if r.Method == http.MethodPatch {
			slot = 6
		}
		if a.base.pod == nil || r.URL.Path != "/api/v1/namespaces/runtimes/pods/"+a.base.pod.Name {
			a.base.mu.Unlock()
			http.Error(w, "unowned mutation", http.StatusConflict)
			return
		}
		if err := a.nativeIntent(slot, string(a.base.pod.UID), a.base.pod.ResourceVersion); err != nil {
			a.base.mu.Unlock()
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		a.effects[slot]++
	}
	a.base.mu.Unlock()
	a.base.serve(w, r)
}

// TestDetachedRuntimeTerminationPhysicalNative joins the real detached source,
// original native metadata writer, protected PID1 and original observer into
// the production seven-slot termination state machine. The job is scheduled
// through the accepted test parking seam; this is not managed admission,
// workspace-owner release, quota return or a completed business cleanup task.
func TestDetachedRuntimeTerminationPhysicalNative(t *testing.T) {
	if os.Getenv("SANDBOX_DETACHED_RUNTIME_PHYSICAL_NATIVE") != "1" {
		t.Skip("requires the exact owned detached physical fixture")
	}
	require.Empty(t, os.Getenv("SANDBOX_WORKSPACE_SYNC_LOCAL_NATIVE"), "the worker must share TLS3, not start an unrelated HTTP cluster")
	for _, asset := range []string{"/fixture/etcd", "/fixture/monitor", "/fixture/etcd.test"} {
		info, err := os.Stat(asset)
		require.NoError(t, err, "required exact fixture asset %s", asset)
		require.False(t, info.IsDir())
	}
	require.Zero(t, os.Geteuid())
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	dir, err := os.MkdirTemp("/tmp", "detached-physical-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	for _, name := range []string{"cgroup", "observer-journal"} {
		require.NoError(t, os.Mkdir(filepath.Join(dir, name), 0700))
	}
	mount := filepath.Join(dir, "cgroup")
	require.NoError(t, unix.Mount("none", mount, "cgroup2", 0, ""))
	t.Cleanup(func() { require.NoError(t, unix.Unmount(mount, 0)) })
	admin, observerTLS, cluster := observerNativeEtcdMembers(t, dir, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	// The worker's ordinary TLS identity can write this configured namespace;
	// the observer keeps its original read-only identity and separate private key.
	_, err = admin.UserAddWithOptions(ctx, "server", "", &clientv3.UserAddOptions{NoPassword: true})
	require.NoError(t, err)
	_, err = admin.RoleAdd(ctx, "detached-physical-write")
	require.NoError(t, err)
	_, err = admin.RoleGrantPermission(ctx, "detached-physical-write", "/sandbox/observer/v1/", clientv3.GetPrefixRangeEnd("/sandbox/observer/v1/"), clientv3.PermissionType(clientv3.PermReadWrite))
	require.NoError(t, err)
	_, err = admin.UserGrantRole(ctx, "server", "detached-physical-write")
	require.NoError(t, err)
	_, root, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, issuerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, observerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	bootWire, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	require.NoError(t, err)
	boot, nodeUID := strings.TrimSpace(string(bootWire)), uuid.NewString()
	binding := p.TrustBinding{Namespace: "/sandbox/observer/v1/", AuthorityID: "detached-physical", Target: "kubernetes", RestoreEpoch: "restore"}
	identity := state.Identity{SchemaVersion: 1, Prefix: "/sandbox", AuthorityID: "observer", Cell: "v1", ClusterID: cluster, StorageID: "detached-physical-storage", RuntimeID: binding.AuthorityID, RestoreEpoch: binding.RestoreEpoch}
	identityWire := observerNativeJSON(t, identity)
	_, err = admin.Put(ctx, binding.Namespace+"meta/identity", string(identityWire))
	require.NoError(t, err)
	_, err = admin.Put(ctx, binding.Namespace+"meta/restore_epoch", binding.RestoreEpoch)
	require.NoError(t, err)
	profile := fmt.Sprintf(`{"version":1,"profile_id":"partition-worker-v1","namespace":%q,"restore_epoch":%q,"identity_digest":%q,"partition_count":256,"relist_slots":4,"range_slots":20,"range_bytes_per_second":1048576,"range_burst_bytes":262144,"claim_slots":100,"claim_per_second":1,"claim_burst":1}`, binding.Namespace, binding.RestoreEpoch, hashBytes(identityWire))
	_, err = admin.Put(ctx, binding.Namespace+"meta/worker_control_budget", profile)
	require.NoError(t, err)
	budgetID := uuid.NewString()
	_, err = admin.Put(ctx, binding.Namespace+"meta/workspace_sync_recycling_budget", fmt.Sprintf(`{"version":2,"budget_id":%q,"slots_per_partition":1,"max_intent_bytes":16384}`, budgetID))
	require.NoError(t, err)
	image := "fixture.test/sandbox@sha256:" + strings.Repeat("a", 64)
	contract, err := kubecontract.EnrolledPlainContractDigest(image)
	require.NoError(t, err)
	nb, na := time.Now().UTC().Add(-30*time.Second), time.Now().UTC().Add(5*time.Minute)
	issuerCert, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	observerCert, err := p.SignNodeObserverCertificate(root, p.NodeObserverCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "node_observer", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, NodeUID: nodeUID, NodeBootID: boot, PublicKey: observerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)

	// New authenticates the configured node immediately. Its real identity is
	// already known; only the source-dependent Pod must wait for source-prepared.
	node := &v1.Node{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"}, ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID(nodeUID)}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: boot, OperatingSystem: "linux", Architecture: runtime.GOARCH}}}
	apiState := &detachedPhysicalAPI{base: cleanupExecutorAPI{node: node, connections: make(map[net.Conn]struct{})}, admin: admin, ctx: ctx}
	api := httptest.NewTLSServer(http.HandlerFunc(apiState.serve))
	defer api.Close()
	providerCA := cleanupExecutorLoaderCA(api.Certificate())
	uds, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "observer.sock"), Net: "unix"})
	require.NoError(t, err)
	defer uds.Close()
	require.NoError(t, os.Chmod(uds.Addr().String(), 0600))
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer tcp.Close()
	observer, err := nodeobserver.New(nodeobserver.Options{Binding: binding, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, NodeName: "node-a", NodeUID: nodeUID, Certificate: observerCert, PrivateKey: observerKey, Clock: nativeFixtureClock{}, EtcdEndpoints: admin.Endpoints(), EtcdTLS: observerTLS, EtcdClusterID: cluster, EtcdIdentity: identityWire, KubernetesConfig: &rest.Config{Host: api.URL, TLSClientConfig: rest.TLSClientConfig{CAData: providerCA}}, KubernetesNamespace: "runtimes", JournalDirectory: filepath.Join(dir, "observer-journal"), MaxSlots: 1, MaxActive: 2, RequestTimeout: 3 * time.Second, TerminationTimeout: 20 * time.Second})
	require.NoError(t, err)
	served := make(chan error, 1)
	go func() { served <- observer.Serve(ctx, uds, tcp) }()
	defer func() {
		require.NoError(t, observer.Close())
		serveErr := <-served
		require.True(t, serveErr == nil || errors.Is(serveErr, context.Canceled) || errors.Is(serveErr, net.ErrClosed), "%v", serveErr)
	}()
	workerCoordinator, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "worker.sock"), Net: "unix"})
	require.NoError(t, err)
	defer workerCoordinator.Close()
	setup := detachedPhysicalWorkerSetup{Version: 1, Endpoints: admin.Endpoints(), Identity: identity, Binding: binding, Root: root.Public().(ed25519.PublicKey), IssuerKey: issuerKey, IssuerCertificate: issuerCert, ObserverCertificate: observerCert, Directory: dir, Coordinator: workerCoordinator.Addr().String(), Image: image, Contract: contract, NodeUID: nodeUID, NodeBootID: boot, ObserverAddress: tcp.Addr().String(), ProviderAddress: api.URL, ProviderCA: providerCA, BudgetID: budgetID}
	workerPath := filepath.Join(dir, "detached-worker.json")
	require.NoError(t, os.WriteFile(workerPath, observerNativeJSON(t, setup), 0600))
	worker := exec.Command("/fixture/etcd.test", "-test.run=^TestDetachedRuntimeTerminationPhysicalWorkerChild$", "-test.v", "-test.timeout=160s")
	worker.Env = append(os.Environ(), "SANDBOX_DETACHED_RUNTIME_PHYSICAL_CHILD_SETUP="+workerPath)
	worker.Stdout, worker.Stderr = os.Stdout, os.Stderr
	require.NoError(t, worker.Start())
	workerJoined := false
	defer func() {
		if !workerJoined {
			_ = worker.Process.Kill()
			t.Logf("failed detached worker original Cmd.Wait=%v", worker.Wait())
		}
	}()
	require.NoError(t, workerCoordinator.SetDeadline(time.Now().Add(30*time.Second)))
	workerConn, err := workerCoordinator.AcceptUnix()
	require.NoError(t, err)
	defer workerConn.Close()
	source := detachedPhysicalRead(t, workerConn, "source-prepared")
	require.Equal(t, source.IntentID, source.Creation.IntentID)
	require.Equal(t, source.WorkspaceHash, source.Creation.WorkspaceHash)
	require.Equal(t, source.SandboxID, source.Creation.SandboxID)
	require.Equal(t, source.IntentID, source.Dispatch.IntentID)

	config := launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding(binding), Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, UID: 1000, GID: 1000, ContractDigest: contract, ClockSocket: ct.ClockSocket, ClockPublicKey: root.Public().(ed25519.PublicKey), ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: nodeUID, NodeBootID: boot}}
	pod, err := k.BuildEnrolledPlainPod("runtimes", k.EnrolledPlainPodRequest{Spec: sandboxruntime.SandboxSpec{ID: "detached-pod", Image: image, CPU: "500m", Memory: "128Mi", Disk: "1Mi", TmpDisk: "1Mi", ReadOnlyRootFS: true, RunAsUser: 1000, Labels: map[string]string{"sandbox.id": source.SandboxID}}, NodeName: "node-a", NodeUID: nodeUID, NodeBootID: boot, CreationIntentID: source.IntentID, Generation: source.Creation.Generation, Config: observerNativeJSON(t, config)})
	require.NoError(t, err)
	require.Equal(t, []string{kubecontract.EnrolledRetainFinalizer}, pod.Finalizers)
	pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	pod.UID, pod.ResourceVersion = types.UID(uuid.NewString()), "1"
	apiState.base.mu.Lock()
	apiState.base.pod = pod
	apiState.policies = localReleaseNativePolicies(source.SandboxID, pod.Name, string(pod.UID))
	apiState.slots = map[string]int{
		"/apis/networking.k8s.io/v1/namespaces/runtimes/networkpolicies/sandbox-" + source.SandboxID:            2,
		"/apis/networking.k8s.io/v1/namespaces/runtimes/networkpolicies/sandbox-" + pod.Name:                    3,
		"/apis/cilium.io/v2/namespaces/runtimes/ciliumnetworkpolicies/sandbox-private-deny-" + source.SandboxID: 4,
		"/apis/cilium.io/v2/namespaces/runtimes/ciliumnetworkpolicies/sandbox-private-deny-" + pod.Name:         5,
	}
	apiState.base.mu.Unlock()

	// Reuse the original protected supervisor bootstrap, private namespaces,
	// real node enrollment and receipt signer. Only this original PID1 holds its key.
	coordinator, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "supervisor.sock"), Net: "unix"})
	require.NoError(t, err)
	defer coordinator.Close()
	owned := filepath.Join(mount, "detached-"+uuid.NewString())
	require.NoError(t, os.Mkdir(owned, 0700))
	defer func() {
		wire, err := os.ReadFile(filepath.Join(owned, "cgroup.events"))
		require.NoError(t, err)
		require.Contains(t, string(wire), "populated 0\n")
		require.NoError(t, os.Remove(owned))
	}()
	supervisorSetup := cleanupExecutorSetup{localReleaseNativeSetup: localReleaseNativeSetup{Binding: binding, Root: setup.Root, IssuerKey: issuerKey, IssuerCertificate: issuerCert, ObserverCertificate: observerCert, Identity: identity, SandboxID: source.SandboxID, IntentID: source.IntentID, Contract: contract, NodeUID: nodeUID, NodeBootID: boot, Directory: dir, Coordinator: coordinator.Addr().String(), EnrollmentSocket: uds.Addr().String(), CgroupMount: mount}, Role: "supervisor", DetachedPhysicalUser: true}
	supervisorPath := filepath.Join(dir, "supervisor.json")
	require.NoError(t, os.WriteFile(supervisorPath, observerNativeJSON(t, supervisorSetup), 0600))
	fd, err := unix.Open(owned, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	fdOpen := true
	defer func() {
		if fdOpen {
			_ = unix.Close(fd)
		}
	}()
	child := exec.Command(os.Args[0], "-test.run=^TestCleanupExecutorWorkerChild$", "-test.v", "-test.timeout=160s")
	child.Env = append(os.Environ(), "CLEANUP_EXECUTOR_CHILD_SETUP="+supervisorPath)
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS, UseCgroupFD: true, CgroupFD: fd}
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	require.NoError(t, child.Start())
	childJoined := false
	defer func() {
		if !childJoined {
			_ = child.Process.Kill()
			t.Logf("failed detached PID1 original Cmd.Wait=%v", child.Wait())
		}
	}()
	closeErr := unix.Close(fd)
	fdOpen = false
	require.NoError(t, closeErr)
	require.NoError(t, coordinator.SetDeadline(time.Now().Add(30*time.Second)))
	conn, err := coordinator.AcceptUnix()
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(60*time.Second)))
	wire, err := ct.ReadNodeObserverFrame(conn)
	require.NoError(t, err)
	var reply localReleaseNativeReply
	require.NoError(t, json.Unmarshal(wire, &reply))
	birth := reply.Birth
	ri := p.RuntimeIdentityContext{SandboxID: source.SandboxID, WorkspaceHash: source.WorkspaceHash, Generation: source.Creation.Generation, Runtime: p.RuntimeReference{ID: pod.Name, UID: string(pod.UID), BootID: birth.BootID}}
	runtimeCert, err := p.SignRuntimeIdentityCertificate(root, p.RuntimeIdentityCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "runtime_receipt", Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: birth.RuntimePublicKey, NotBefore: nb, NotAfter: na, SandboxID: ri.SandboxID, WorkspaceHash: ri.WorkspaceHash, Generation: ri.Generation, Runtime: ri.Runtime})
	require.NoError(t, err)
	activation, err := p.SignTargetActivation(root, p.TargetActivationClaims{Version: 1, Role: "target_activation", ActivationID: uuid.NewString(), Binding: binding, Identity: ri, DataGateEpoch: source.Creation.DataGateEpoch, UID: 1000, GID: 1000, ContractDigest: contract, RuntimeCertificateDigest: hashBytes(runtimeCert), IssuerCertificateDigest: hashBytes(issuerCert), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	d := source.Dispatch
	preparation, err := p.SignRuntimeCertificate(root, p.RuntimeCertificateClaims{Version: 1, Namespace: binding.Namespace, AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, IntentID: source.IntentID, SandboxID: source.SandboxID, WorkspaceHash: source.WorkspaceHash, Generation: ri.Generation, OperationID: d.OperationID, PayloadDigest: d.PayloadDigest, Snapshot: p.SnapshotReference(d.Snapshot), ExpiresAt: d.ExpiresAt, Runtime: ri.Runtime, WorkspaceMode: "plain", PublicKey: birth.RuntimePublicKey, NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	detachedPhysicalWrite(t, workerConn, detachedPhysicalPhase{Phase: "bind", Preparation: preparation})
	_ = detachedPhysicalRead(t, workerConn, "mount-consumed")
	command := enrolledNativeCommand{Action: "activate", Request: p.NodeEnrollmentRequest{Version: 1, Purpose: "runtime_node_enrollment", Activation: activation, RuntimeCertificate: runtimeCert, IssuerCertificate: issuerCert}, Certificate: preparation, Claim: p.ClaimReference{ClaimID: source.Creation.ClaimID, CreateRevision: source.Creation.CreateRevision, LeaseID: source.Creation.LeaseID}}
	require.NoError(t, ct.WriteNodeObserverFrame(conn, observerNativeJSON(t, command)))
	wire, err = ct.ReadNodeObserverFrame(conn)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(wire, &reply))
	detachedPhysicalWrite(t, workerConn, detachedPhysicalPhase{Phase: "enrolled", Proof: reply.Proof, Enrollment: reply.Enrollment, RuntimeAddress: reply.Address})
	ready := detachedPhysicalRead(t, workerConn, "task-ready")
	require.Equal(t, source.SandboxID, ready.Task.SandboxID)
	apiState.base.mu.Lock()
	apiState.task = ready.Task
	apiState.base.mu.Unlock()
	detachedPhysicalWrite(t, workerConn, detachedPhysicalPhase{Phase: "run"})
	_ = detachedPhysicalRead(t, workerConn, "cleanup-complete")
	workerErr := worker.Wait()
	workerJoined = true
	t.Logf("original detached metadata worker pid=%d Cmd.Wait=%v", worker.Process.Pid, workerErr)
	require.NoError(t, workerErr)
	childErr := child.Wait()
	childJoined = true
	var exited *exec.ExitError
	require.ErrorAs(t, childErr, &exited)
	require.Equal(t, syscall.SIGKILL, exited.Sys().(syscall.WaitStatus).Signal())
	t.Logf("original protected PID1 hostPID=%d Cmd.Wait=SIGKILL", child.Process.Pid)

	// Independently verify the observer's retained signed proof against the
	// actual mutable native intent and its original create-once reservation.
	key := fmt.Sprintf("%sp/%02x/tasks/%s/runtime-termination", binding.Namespace, ready.Task.Partition, ready.Task.TaskID)
	terminated, err := admin.Get(ctx, key)
	require.NoError(t, err)
	require.Len(t, terminated.Kvs, 1)
	intent, err := p.DecodeTaskRuntimeTerminationIntent(terminated.Kvs[0].Value)
	require.NoError(t, err)
	require.EqualValues(t, 2, intent.Version)
	require.Equal(t, "cleanup-complete", intent.Phase)
	require.NotNil(t, intent.DetachedEffects)
	reserved := intent
	reserved.Phase, reserved.TerminalProof, reserved.CleanupDigest = "reserved", nil, ""
	reservedWire, err := p.EncodeTaskRuntimeTerminationIntent(reserved)
	require.NoError(t, err)
	query := p.NodeTerminationQuery{Version: 1, Purpose: "task_runtime_terminate_query", Partition: intent.Partition, TaskID: intent.Current.TaskID, CommandID: intent.Current.CommandID, IntentCreateRevision: terminated.Kvs[0].CreateRevision, IntentDigest: hashBytes(reservedWire), EnrollmentDigest: intent.EnrollmentDigest}
	verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{setup.Root})
	require.NoError(t, err)
	proof, err := verifier.VerifyNodeTerminationProof(intent.TerminalProof, observerCert, query)
	require.NoError(t, err)
	require.True(t, proof.PID1Dead)
	require.True(t, proof.CgroupEmpty)
	require.Equal(t, intent.Current, proof.Current)
	require.Equal(t, string(pod.UID), proof.PodUID)
	apiState.base.mu.Lock()
	defer apiState.base.mu.Unlock()
	require.Nil(t, apiState.base.pod)
	require.Empty(t, apiState.policies)
	require.Equal(t, [7]int{0, 1, 1, 1, 1, 1, 1}, apiState.effects)
	require.Equal(t, 1, apiState.base.patches)
	require.Equal(t, 1, apiState.base.deletes)
	t.Log("actual detached source/B writer + native v2 authority + observer signed PID1-dead/cgroup-empty proof + all seven provider slots; user probe joined before genuine quiescence; managed admission/owner release/quota remain outside this test")
}
