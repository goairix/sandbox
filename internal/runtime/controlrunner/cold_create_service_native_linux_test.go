//go:build linux && (amd64 || arm64)

package controlrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
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
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/nodeobserver"
	"github.com/goairix/sandbox/internal/provisioning"
	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

func TestColdCreateServiceNative(t *testing.T) {
	if os.Getenv("SANDBOX_COLD_CREATE_SERVICE_NATIVE") != "1" {
		t.Skip("requires exact owned cold-create service Linux fixture")
	}
	for _, loseACK := range []bool{false, true} {
		t.Run(fmt.Sprintf("activation-ACK-lost-%t", loseACK), func(t *testing.T) {
			runColdCreateServiceNative(t, loseACK)
		})
	}
}

func runColdCreateServiceNative(t *testing.T, loseACK bool) {
	require.Zero(t, os.Geteuid())
	require.NoError(t, unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""))
	dir, err := os.MkdirTemp("/tmp", "cold-service-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(dir)) })
	mount := filepath.Join(dir, "cgroup")
	require.NoError(t, os.Mkdir(mount, 0700))
	require.NoError(t, unix.Mount("none", mount, "cgroup2", 0, ""))
	t.Cleanup(func() { require.NoError(t, unix.Unmount(mount, 0)) })
	require.NoError(t, unix.Mount("tmpfs", "/run", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=0700,size=1048576"))
	t.Cleanup(func() { require.NoError(t, unix.Unmount("/run", 0)) })
	require.NoError(t, os.Mkdir(ct.ControlDirectory, 0755))
	require.NoError(t, unix.Mount("tmpfs", ct.ControlDirectory, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "size=1048576"))
	t.Cleanup(func() { require.NoError(t, unix.Unmount(ct.ControlDirectory, 0)) })
	startupNativeInitialPermissions(t)
	require.NoError(t, os.Mkdir("/run/sandbox-node-observer", 0700))
	require.NoError(t, os.Mkdir(filepath.Dir(ct.ClockSocket), 0700))
	admin, observerTLS, cluster := observerNativeEtcd(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	t.Cleanup(cancel)
	_, root, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, issuerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	_, observerKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	clockPublic, clockKey, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	ns, err := state.NewNamespace("/sandbox", "observer", "v1")
	require.NoError(t, err)
	identity := state.Identity{SchemaVersion: 1, Prefix: "/sandbox", AuthorityID: "observer", Cell: "v1", ClusterID: cluster, StorageID: "storage", RuntimeID: "launcher", RestoreEpoch: "restore"}
	identityWire, err := json.Marshal(identity)
	require.NoError(t, err)
	_, err = admin.Put(ctx, ns.Root()+"meta/identity", string(identityWire))
	require.NoError(t, err)
	_, err = admin.Put(ctx, ns.Root()+"meta/restore_epoch", identity.RestoreEpoch)
	require.NoError(t, err)
	_, err = admin.UserAddWithOptions(ctx, "server", "", &clientv3.UserAddOptions{NoPassword: true})
	require.NoError(t, err)
	_, err = admin.RoleAdd(ctx, "cold-writer")
	require.NoError(t, err)
	_, err = admin.RoleGrantPermission(ctx, "cold-writer", ns.Root(), clientv3.GetPrefixRangeEnd(ns.Root()), clientv3.PermissionType(clientv3.PermReadWrite))
	require.NoError(t, err)
	_, err = admin.UserGrantRole(ctx, "server", "cold-writer")
	require.NoError(t, err)
	certificate, err := tls.LoadX509KeyPair(filepath.Join(dir, "server.pem"), filepath.Join(dir, "server-key.pem"))
	require.NoError(t, err)
	var inventory state.PreparationInventoryOptions
	inventory.QuotaSlots[7] = 1
	binding := p.TrustBinding{Namespace: ns.Root(), AuthorityID: "launcher", Target: "target", RestoreEpoch: "restore"}
	backendOptions := state.Options{Endpoints: []string{"https://127.0.0.1:2379"}, Namespace: ns, Identity: identity, TLS: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: observerTLS.RootCAs, Certificates: []tls.Certificate{certificate}}, RequestTimeout: 2 * time.Second, Clock: nativeFixtureClock{}, PublicationTrust: &state.RuntimePublicationTrust{AuthorityID: binding.AuthorityID, Target: binding.Target, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, EnrolledPublication: &state.EnrolledPublicationOptions{ContractDigest: profileFixtureContract(t), MaxActive: 4}, PreparationInventory: &inventory}
	backend, err := state.New(ctx, backendOptions)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, backend.Close()) })
	for {
		progress, err := backend.InitializePreparationInventory(ctx)
		require.NoError(t, err)
		if progress.Ready {
			break
		}
	}
	bootWire, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	require.NoError(t, err)
	boot, nodeUID := strings.TrimSpace(string(bootWire)), uuid.NewString()
	nb, na := time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(5*time.Minute)
	observerCert, err := p.SignNodeObserverCertificate(root, p.NodeObserverCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "node_observer", Namespace: ns.Root(), AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, NodeUID: nodeUID, NodeBootID: boot, PublicKey: observerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	issuerCert, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: ns.Root(), AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	config, err := json.Marshal(launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding(binding), Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, UID: 1000, GID: 1000, ContractDigest: profileFixtureContract(t), ClockSocket: ct.ClockSocket, ClockPublicKey: clockPublic, ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: nodeUID, NodeBootID: boot}})
	require.NoError(t, err)
	clockListener, err := net.Listen("unix", ct.ClockSocket)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(ct.ClockSocket, 0600))
	clockCtx, stopClock := context.WithCancel(ctx)
	clockDone := make(chan error, 1)
	go func() {
		clockDone <- p.ServeSignedClock(clockCtx, clockListener, p.SignedClockServerOptions{Binding: binding, Key: clockKey, Source: nativeFixtureClock{}, MaxConnections: 4})
	}()
	t.Cleanup(func() {
		stopClock()
		clockListener.Close()
		err := <-clockDone
		require.True(t, err == nil || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed), "clock joined: %v", err)
	})
	request := provisioning.PlainColdRequest{Principal: "native", IdempotencyKey: uuid.NewString(), RequestID: uuid.NewString(), IntentID: uuid.NewString(), SandboxID: uuid.NewString(), SnapshotVersion: uuid.NewString(), TTL: time.Minute}
	w, err := state.NewWorkspaceIdentity("s3", identity.StorageID, "bucket", uuid.NewString()+"/")
	require.NoError(t, err)
	expected, err := kube.BuildEnrolledPlainPod("runtimes", kube.EnrolledPlainPodRequest{Spec: sandboxruntime.SandboxSpec{ID: request.SandboxID, Image: enrolledProfileFixtureImage, CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi", ReadOnlyRootFS: true, RunAsUser: 1000, SeccompProfile: "RuntimeDefault"}, NodeName: "node-a", NodeUID: nodeUID, NodeBootID: boot, CreationIntentID: request.IntentID, Generation: 1, Config: config})
	require.NoError(t, err)
	expected.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	node := &v1.Node{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"}, ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID(nodeUID)}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: boot, OperatingSystem: "linux", Architecture: runtime.GOARCH}}}
	owned := filepath.Join(mount, "cold-"+uuid.NewString())
	require.NoError(t, os.Mkdir(owned, 0700))
	t.Cleanup(func() {
		wire, err := os.ReadFile(filepath.Join(owned, "cgroup.events"))
		require.NoError(t, err)
		require.Contains(t, string(wire), "populated 0\n")
		require.NoError(t, os.Remove(owned))
	})
	var child *exec.Cmd
	var childWait chan error
	var mu sync.Mutex
	var pod *v1.Pod
	var postCount, startCount, execCount atomic.Int32
	var bridges []*startupBridgeFixture
	// Only this POST handler starts the original formal launcher. GET, service
	// construction and setup cannot create a runtime or fabricate publication.
	api := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		path := "/api/v1/namespaces/runtimes/pods"
		if r.Method == http.MethodPost && r.URL.Path == path {
			if postCount.Add(1) != 1 {
				http.Error(rw, "duplicate POST", http.StatusConflict)
				return
			}
			var actual v1.Pod
			if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 65536)).Decode(&actual); err != nil || !equality.Semantic.DeepEqual(expected, &actual) {
				http.Error(rw, "incorrect complete POST body/GVK", http.StatusBadRequest)
				return
			}
			fd, err := unix.Open(owned, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
			if err != nil {
				http.Error(rw, err.Error(), 500)
				return
			}
			command := exec.Command(os.Args[0], "-test.run=^TestEnrolledProfileNativeExec$", "-test.v", "-test.timeout=30s")
			command.Env = append(os.Environ(), "ENROLLED_LAUNCHER_EXEC_CGROUP_MOUNT="+mount, "SANDBOX_LAUNCHER_CONFIG_V2="+string(config), "ENROLLED_PROFILE_EXEC_MODE=active", "ENROLLED_STARTUP_PROFILE_EXEC=1")
			command.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS, UseCgroupFD: true, CgroupFD: fd}
			command.Stdout, command.Stderr = os.Stdout, os.Stderr
			err = command.Start()
			closeErr := unix.Close(fd)
			if err != nil {
				http.Error(rw, err.Error(), 500)
				return
			}
			wait := make(chan error, 1)
			go func() { wait <- command.Wait() }()
			actual.UID, actual.ResourceVersion = types.UID(uuid.NewString()), "1"
			mu.Lock()
			child, childWait, pod = command, wait, actual.DeepCopy()
			mu.Unlock()
			startCount.Add(1)
			if closeErr != nil {
				http.Error(rw, closeErr.Error(), 500)
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if _, err := os.Lstat(ct.ControlSocket); err == nil {
					break
				}
				if time.Now().After(deadline) {
					http.Error(rw, "formal control socket missing", 500)
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(rw).Encode(&actual)
			return
		}
		if r.Method != http.MethodGet {
			rw.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == path+"/"+request.SandboxID+"/exec" {
			mu.Lock()
			if child == nil || len(bridges) >= 16 {
				mu.Unlock()
				rw.WriteHeader(http.StatusConflict)
				return
			}
			bridge := newStartupBridgeFixture()
			bridge.discardStdout = loseACK && execCount.Add(1) == 2
			err := bridge.bindOriginalMountNamespace(child.Process.Pid)
			bridges = append(bridges, bridge)
			mu.Unlock()
			if err != nil {
				http.Error(rw, err.Error(), 500)
				return
			}
			bridge.ServeHTTP(rw, r)
			return
		}
		rw.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/nodes/node-a" {
			_ = json.NewEncoder(rw).Encode(node)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == path+"/"+request.SandboxID && pod != nil {
			_ = json.NewEncoder(rw).Encode(pod)
			return
		}
		rw.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(api.Close)
	apiConfig := &rest.Config{Host: api.URL, TLSClientConfig: rest.TLSClientConfig{CAData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw})}}
	journal := filepath.Join(dir, "observer-journal")
	require.NoError(t, os.Mkdir(journal, 0700))
	observer, err := nodeobserver.New(nodeobserver.Options{Binding: binding, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, NodeName: "node-a", NodeUID: nodeUID, Certificate: observerCert, PrivateKey: observerKey, Clock: nativeFixtureClock{}, EtcdEndpoints: []string{"https://127.0.0.1:2379"}, EtcdTLS: observerTLS, EtcdClusterID: cluster, EtcdIdentity: identityWire, KubernetesConfig: apiConfig, KubernetesNamespace: "runtimes", JournalDirectory: journal, MaxSlots: 1, MaxActive: 2, RequestTimeout: 5 * time.Second, TerminationTimeout: 20 * time.Second})
	require.NoError(t, err)
	uds, err := net.ListenUnix("unix", &net.UnixAddr{Name: "/run/sandbox-node-observer/enrollment.sock", Net: "unix"})
	require.NoError(t, err)
	require.NoError(t, os.Chmod(uds.Addr().String(), 0600))
	require.NoError(t, os.WriteFile(ct.EnrollmentSocket, nil, 0600))
	tcp, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	observerDone := make(chan error, 1)
	go func() { observerDone <- observer.Serve(ctx, uds, tcp) }()
	t.Cleanup(func() {
		require.NoError(t, observer.Close())
		uds.Close()
		tcp.Close()
		err := <-observerDone
		require.True(t, err == nil || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed), "observer joined: %v", err)
	})
	t.Cleanup(func() {
		mu.Lock()
		ownedBridges := append([]*startupBridgeFixture(nil), bridges...)
		original, wait := child, childWait
		mu.Unlock()
		for _, bridge := range ownedBridges {
			bridge.Close()
			t.Logf("cold service bridge original Cmd.Wait called=%t result=%v relay=%v", bridge.waited, bridge.waitErr, bridge.result)
		}
		if original != nil {
			_ = original.Process.Signal(syscall.SIGTERM)
			var err error
			select {
			case err = <-wait:
			case <-time.After(5 * time.Second):
				_ = original.Process.Kill()
				err = <-wait
			}
			t.Logf("cold service original formal launcher Cmd.Wait=%v", err)
			require.NoError(t, err)
			if err == nil {
				t.Log("cold service original formal launcher Cmd.Wait=exit0")
			}
		}
	})
	options := provisioning.PlainColdOptions{Backend: backend, API: kube.EnrolledBridgeOptions{Config: apiConfig, Namespace: "runtimes", MaxActive: 4, RequestTimeout: 5 * time.Second}, Image: enrolledProfileFixtureImage, NodeName: "node-a", NodeUID: nodeUID, NodeBootID: boot, WorkerID: "cold-native", CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi", QuotaPartition: 7, LauncherConfig: config, RootPrivateKey: root, IssuerPrivateKey: issuerKey, IssuerCertificate: issuerCert, Clock: nativeFixtureClock{}}
	service, err := provisioning.NewPlainColdService(ctx, options)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, service.Close()) })
	activeService := service
	result, createErr := service.Create(ctx, w, request)
	require.Equal(t, int32(1), postCount.Load())
	require.Equal(t, int32(1), startCount.Load())
	mu.Lock()
	original := child
	mu.Unlock()
	require.NotNil(t, original)
	pid := original.Process.Pid
	executable, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	require.NoError(t, err)
	require.Equal(t, "/usr/local/bin/sandbox-launcher", executable)
	profileNativeProvisioned(t, pid, config)
	if loseACK {
		require.Error(t, createErr, "original activation response was dropped after real bridge consumed it")
		before, err := backend.LoadColdBootstrapMaterial(ctx, w, request.IntentID)
		require.NoError(t, err)
		require.NotNil(t, before)
		require.NotNil(t, result.Attempt)
		observation, err := backend.LoadColdCreateObservation(ctx, w, request.IntentID)
		require.NoError(t, err)
		require.NotNil(t, observation)
		require.Equal(t, p.RuntimeReference{ID: request.SandboxID, UID: observation.PodUID, BootID: before.Material.Birth.BootID}, result.Runtime, "pending activation ACK loss carries the authenticated original UID and Birth BootID")
		materialKey := ns.Root() + fmt.Sprintf("p/%02x/intents/%s/cold-create-attempt-material", w.Partition(), request.IntentID)
		beforeKV, err := admin.Get(ctx, materialKey)
		require.NoError(t, err)
		require.Len(t, beforeKV.Kvs, 1)
		require.NoError(t, service.Close())
		early, earlyErr := service.Resume(ctx, w, result)
		require.Error(t, earlyErr, "closed admission returns diagnostic copies without any action")
		require.Equal(t, result.Runtime, early.Runtime)
		require.Equal(t, result.Attempt, early.Attempt)
		require.NotSame(t, result.Attempt, early.Attempt)
		require.Equal(t, result.Stage, early.Stage)
		require.Equal(t, int32(1), postCount.Load())
		require.Equal(t, int32(1), startCount.Load())
		require.NoError(t, backend.Close())
		backend, err = state.New(ctx, backendOptions)
		require.NoError(t, err)
		options.Backend = backend
		recovered, err := provisioning.NewPlainColdService(ctx, options)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, recovered.Close()) })
		activeService = recovered
		// Caller-owned diagnostics deliberately name another runtime and attempt.
		// The original Backend evidence alone selects the target and grants work.
		forged := early
		forged.Runtime = p.RuntimeReference{ID: "foreign-pod", UID: uuid.NewString(), BootID: uuid.NewString()}
		forged.Attempt = &state.ColdCreateAttemptRecord{Version: 1, OperationID: uuid.NewString()}
		forged.Publication = &state.RuntimeEnrolledPublicationEntry{}
		result, createErr = recovered.Resume(ctx, w, forged)
		require.Equal(t, early.Runtime, result.Runtime)
		require.Equal(t, early.Attempt, result.Attempt)
		afterKV, err := admin.Get(ctx, materialKey)
		require.NoError(t, err)
		require.Equal(t, beforeKV.Kvs, afterKV.Kvs, "exact original immutable material bytes and revisions across service restart; settled history does not reopen pending-material admission")
	}
	// This assertion intentionally reaches the production settlement stub in RED.
	// A failure before publication is a setup/flow defect, not that RED evidence.
	publication, publicationErr := backend.LoadRuntimePublicationEnrolled(ctx, w, request.IntentID)
	require.NoError(t, publicationErr)
	require.NotNil(t, publication)
	require.NoError(t, createErr)
	require.Equal(t, provisioning.PlainColdCompleted, result.State)
	require.Equal(t, request.RequestID, result.RequestID)
	require.Equal(t, p.RuntimeReference(publication.Record.Runtime), result.Runtime)
	require.Equal(t, int32(1), postCount.Load())
	require.Equal(t, int32(1), startCount.Load())
	reservation, err := backend.LoadColdPreparation(ctx, w, request.IntentID)
	require.NoError(t, err)
	require.Equal(t, "settled", reservation.Phase)
	completedInput := result
	completedInput.Runtime = p.RuntimeReference{ID: "foreign-completed", UID: uuid.NewString(), BootID: uuid.NewString()}
	settledAgain, err := activeService.Resume(ctx, w, completedInput)
	require.NoError(t, err)
	require.Equal(t, provisioning.PlainColdCompleted, settledAgain.State)
	require.Equal(t, result.Stage, settledAgain.Stage)
	require.Equal(t, result.Runtime, settledAgain.Runtime, "completed metadata refreshes copied runtime attribution")
	quotaKey := ns.Root() + fmt.Sprintf("quota/%02x/slots/%08d", reservation.Quota.Partition, reservation.Quota.ID)
	quota, err := admin.Get(ctx, quotaKey)
	require.NoError(t, err)
	require.Len(t, quota.Kvs, 1)
	var quotaValue struct {
		Occupied bool                `json:"occupied"`
		Owner    state.ColdSlotOwner `json:"owner"`
	}
	require.NoError(t, json.Unmarshal(quota.Kvs[0].Value, &quotaValue))
	require.True(t, quotaValue.Occupied)
	require.Equal(t, reservation.Owner, quotaValue.Owner)
	free, err := admin.Get(ctx, ns.Root()+fmt.Sprintf("preparation/free/%02d", reservation.Preparation.ID))
	require.NoError(t, err)
	require.Len(t, free.Kvs, 1)
	require.Equal(t, int32(1), postCount.Load())
	require.Equal(t, int32(1), startCount.Load())
	mu.Lock()
	ownedBridges := append([]*startupBridgeFixture(nil), bridges...)
	mu.Unlock()
	for _, bridge := range ownedBridges {
		bridge.requireJoined(t)
	}
	t.Log("cold service actual POST-to-formal-launcher/publication/independent preparation settlement completed")
}
