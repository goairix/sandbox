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
	"io"
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

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/goairix/sandbox/internal/nodeobserver"
	"github.com/goairix/sandbox/internal/provisioning"
	"github.com/goairix/sandbox/internal/provisioning/deployment"
	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	ct "github.com/goairix/sandbox/internal/runtime/controltransport"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/goairix/sandbox/internal/sandboxsnapshot"
	state "github.com/goairix/sandbox/internal/storage/state/etcd"
	business "github.com/goairix/sandbox/pkg/types"
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
	runColdCreateServiceBusinessNative(t, loseACK, false)
}

func runColdCreateServiceBusinessNative(t *testing.T, loseACK, withBusiness bool) {
	runColdCreateServiceVariantNative(t, loseACK, withBusiness, false, false)
}

func runColdCreateServiceVariantNative(t *testing.T, loseACK, withBusiness, detached, concurrent bool, liveLookup ...bool) {
	runColdCreateServiceFixtureNative(t, coldServiceNativeVariant{loseACK: loseACK, withBusiness: withBusiness, detached: detached, concurrent: concurrent, live: len(liveLookup) == 1 && liveLookup[0]})
}

type coldServiceNativeVariant struct {
	loseACK, withBusiness, detached, concurrent, live, execService bool
	httpGateway, ttlGateway, fileAPI                               bool
	ttlRecovery, ttlRotation                                       bool
	inventoryGateway                                               bool
	largeDownload                                                  bool
	destroyLocal, destroyDetached                                  bool
	destroyTerminal                                                bool
}

func runColdCreateServiceFixtureNative(t *testing.T, variant coldServiceNativeVariant) {
	loseACK, withBusiness, detached, concurrent, live := variant.loseACK, variant.withBusiness, variant.detached, variant.concurrent, variant.live
	detachedTerminal := variant.destroyDetached && variant.destroyTerminal
	physicalDestroy := variant.destroyLocal || detachedTerminal
	var lookupHooks *liveLookupNativeHooks
	if live {
		lookupHooks = &liveLookupNativeHooks{}
	}
	var execHooks *execServiceNativeHooks
	if variant.execService || variant.httpGateway || variant.ttlGateway || variant.fileAPI || variant.largeDownload || variant.destroyLocal || variant.destroyDetached {
		execHooks = &execServiceNativeHooks{}
	}
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
	timeout := 40 * time.Second
	if variant.execService {
		timeout = 100 * time.Second
	}
	if variant.httpGateway || variant.ttlGateway || variant.fileAPI || variant.largeDownload {
		timeout = 60 * time.Second
	}
	if variant.destroyLocal || variant.destroyDetached {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
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
	if physicalDestroy {
		binding.Target = "kubernetes"
	}
	backendOptions := state.Options{Endpoints: []string{"https://127.0.0.1:2379"}, Namespace: ns, Identity: identity, TLS: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: observerTLS.RootCAs, Certificates: []tls.Certificate{certificate}}, RequestTimeout: 2 * time.Second, Clock: nativeFixtureClock{}, PublicationTrust: &state.RuntimePublicationTrust{AuthorityID: binding.AuthorityID, Target: binding.Target, Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}}, EnrolledPublication: &state.EnrolledPublicationOptions{ContractDigest: profileFixtureContract(t), MaxActive: 4}, PreparationInventory: &inventory}
	if detachedTerminal {
		// Select the actual immutable image profile before either Backend New.
		// Existing unsupported-detached and local variants retain their options.
		backendOptions.DetachedCleanup = &state.DetachedCleanupOptions{PinnedPlainImage: enrolledProfileFixtureImage}
	}
	var execIssuer *deployment.ExecIssuer
	var mountedOwner *deployment.Deployment
	backend, err := state.New(ctx, backendOptions)
	require.NoError(t, err)
	t.Cleanup(func() {
		if mountedOwner == nil {
			require.NoError(t, backend.Close())
		}
		require.NoError(t, execIssuer.Close())
	})
	if !variant.inventoryGateway {
		for {
			progress, err := backend.InitializePreparationInventory(ctx)
			require.NoError(t, err)
			if progress.Ready {
				break
			}
		}
	}
	bootWire, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	require.NoError(t, err)
	boot, nodeUID := strings.TrimSpace(string(bootWire)), uuid.NewString()
	nb, na := time.Now().UTC().Add(-time.Minute), time.Now().UTC().Add(5*time.Minute)
	observerCert, err := p.SignNodeObserverCertificate(root, p.NodeObserverCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "node_observer", Namespace: ns.Root(), AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, NodeUID: nodeUID, NodeBootID: boot, PublicKey: observerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: na})
	require.NoError(t, err)
	issuerAfter := na
	if variant.ttlRotation {
		// Only this scenario removes the original five-minute issuer ceiling.
		// Create still obtains its production fifteen-minute receipt leaf; the
		// later thirty-minute TTL request must renew that leaf on the same key.
		issuerAfter = time.Now().UTC().Add(45 * time.Minute)
	}
	issuerCert, err := p.SignCommandIssuerCertificate(root, p.CommandIssuerCertificateClaims{Version: 1, CertificateID: uuid.NewString(), Role: "command_issuer", Namespace: ns.Root(), AuthorityID: binding.AuthorityID, Target: binding.Target, RestoreEpoch: binding.RestoreEpoch, PublicKey: issuerKey.Public().(ed25519.PublicKey), NotBefore: nb, NotAfter: issuerAfter})
	require.NoError(t, err)
	config, err := json.Marshal(launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding(binding), Roots: []ed25519.PublicKey{root.Public().(ed25519.PublicKey)}, UID: 1000, GID: 1000, ContractDigest: profileFixtureContract(t), ClockSocket: ct.ClockSocket, ClockPublicKey: clockPublic, ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: nodeUID, NodeBootID: boot}})
	require.NoError(t, err)
	if variant.execService || variant.httpGateway || variant.ttlGateway || variant.fileAPI || variant.largeDownload || variant.destroyLocal || variant.destroyDetached {
		launcher, err := launcherconfig.Decode(config)
		require.NoError(t, err)
		execIssuer, err = deployment.NewExecIssuer(ctx, issuerKey, issuerCert, launcher, nativeFixtureClock{})
		require.NoError(t, err)
		require.NoError(t, backend.Close())
		backendOptions.ExecIssuer = execIssuer
		backend, err = state.New(ctx, backendOptions)
		require.NoError(t, err)
	}
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
	if variant.execService {
		request.TTL = 90 * time.Second
	}
	if withBusiness {
		request.Business, err = sandboxsnapshot.Build(business.CreateSandboxRequest{Mode: "persistent", Resources: &business.ResourceLimits{CPU: "2000m"}}, sandboxsnapshot.Defaults{TimeoutSeconds: 60, Resources: business.ResourceLimits{CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi"}})
		require.NoError(t, err)
	}
	w, err := state.NewWorkspaceIdentity("s3", identity.StorageID, "bucket", uuid.NewString()+"/")
	require.NoError(t, err)
	if variant.destroyLocal {
		w, err = state.NewWorkspaceIdentity("local", identity.StorageID, "bucket", "http-destroy/")
		require.NoError(t, err)
	}
	if detached {
		w, err = backend.DetachedIdentity(request.Principal, request.IdempotencyKey)
		require.NoError(t, err)
		require.Equal(t, state.WorkspaceIdentityDetached, w.Kind())
	}
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
	var loseNextCompletion atomic.Bool
	bridgeLimit := 16
	var destroyProvider *httpDestroyNativeProvider
	if physicalDestroy {
		destroyProvider = &httpDestroyNativeProvider{}
		// This composition shares Create(3), Exec(1) and cleanup(at least13)
		// bridges, plus terminal-quiescence queries. Keep a fixed fixture-only
		// bound; existing variants retain16 and overflow still fails closed.
		bridgeLimit = 32
	} else if variant.fileAPI {
		// Create(3), setup(2), the original file calls(9), two upload/download
		// pairs(4), stat(2) and chmod(1) require exactly21 original bridges.
		// Only this expanded fixture changes; production admission is intact.
		bridgeLimit = 21
	}
	// Only this POST handler starts the original formal launcher. GET, service
	// construction and setup cannot create a runtime or fabricate publication.
	api := httptest.NewTLSServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		path := "/api/v1/namespaces/runtimes/pods"
		if destroyProvider != nil && destroyProvider.handle(rw, r, &mu, &pod) {
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == path {
			if postCount.Add(1) != 1 {
				http.Error(rw, "duplicate POST", http.StatusConflict)
				return
			}
			var actual v1.Pod
			if err := json.NewDecoder(http.MaxBytesReader(rw, r.Body, 65536)).Decode(&actual); err != nil {
				http.Error(rw, "invalid POST JSON", http.StatusBadRequest)
				return
			}
			want := expected
			if detached {
				// Only server-generated logical/intent UUIDs vary. Rebuild the
				// entire original expected body before accepting any POST field.
				id, intent := actual.Name, actual.Annotations[kubecontract.EnrolledCreationIntentAnnotation]
				parsedID, idErr := uuid.Parse(id)
				parsedIntent, intentErr := uuid.Parse(intent)
				if idErr != nil || intentErr != nil || parsedID == uuid.Nil || parsedIntent == uuid.Nil || parsedID.String() != id || parsedIntent.String() != intent {
					http.Error(rw, "invalid generated IDs", http.StatusBadRequest)
					return
				}
				var buildErr error
				want, buildErr = kube.BuildEnrolledPlainPod("runtimes", kube.EnrolledPlainPodRequest{Spec: sandboxruntime.SandboxSpec{ID: id, Image: enrolledProfileFixtureImage, CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi", ReadOnlyRootFS: true, RunAsUser: 1000, SeccompProfile: "RuntimeDefault"}, NodeName: "node-a", NodeUID: nodeUID, NodeBootID: boot, CreationIntentID: intent, Generation: 1, Config: config})
				if buildErr != nil {
					http.Error(rw, "invalid generated pod", http.StatusBadRequest)
					return
				}
				want.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
			}
			if !equality.Semantic.DeepEqual(want, &actual) {
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
		mu.Lock()
		actualName := ""
		if pod != nil {
			actualName = pod.Name
		}
		mu.Unlock()
		if actualName != "" && r.URL.Path == path+"/"+actualName+"/exec" {
			mu.Lock()
			if child == nil || len(bridges) >= bridgeLimit {
				mu.Unlock()
				rw.WriteHeader(http.StatusConflict)
				return
			}
			bridge := newStartupBridgeFixture()
			bridge.loseCompletion = loseNextCompletion.Swap(false)
			if execHooks != nil {
				bridge.timeout = 75 * time.Second
				execHooks.register(bridge)
			}
			bridge.discardStdout = loseACK && execCount.Add(1) == 2
			err := bridge.bindOriginalMountNamespace(child.Process.Pid)
			bridges = append(bridges, bridge)
			mu.Unlock()
			if err != nil {
				http.Error(rw, err.Error(), 500)
				return
			}
			if lookupHooks != nil {
				if err := lookupHooks.beforeBridge(bridge); err != nil {
					http.Error(rw, err.Error(), 500)
					return
				}
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
		if pod != nil && r.URL.Path == path+"/"+pod.Name {
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
			if variant.destroyLocal || detachedTerminal && !t.Failed() {
				var killed *exec.ExitError
				require.ErrorAs(t, err, &killed)
				require.Equal(t, syscall.SIGKILL, killed.Sys().(syscall.WaitStatus).Signal())
				t.Log("HTTP destroy original formal launcher Cmd.Wait=SIGKILL physically joined")
			} else if !detachedTerminal {
				require.NoError(t, err)
				if err == nil {
					t.Log("cold service original formal launcher Cmd.Wait=exit0")
				}
			}
		}
	})
	options := provisioning.PlainColdOptions{Backend: backend, API: kube.EnrolledBridgeOptions{Config: apiConfig, Namespace: "runtimes", MaxActive: 4, RequestTimeout: 5 * time.Second}, Image: enrolledProfileFixtureImage, NodeName: "node-a", NodeUID: nodeUID, NodeBootID: boot, WorkerID: "cold-native", CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi", QuotaPartition: 7, LauncherConfig: config, RootPrivateKey: root, IssuerPrivateKey: issuerKey, IssuerCertificate: issuerCert, Clock: nativeFixtureClock{}}
	var lookupClock *liveLookupNativeClock
	if live {
		lookupClock = &liveLookupNativeClock{}
		options.Clock = lookupClock
	}
	var acquisitionClock *detachedNativeAcquisitionClock
	if detached {
		options.Business = &provisioning.PlainColdBusinessOptions{Principal: request.Principal, Defaults: sandboxsnapshot.Defaults{TimeoutSeconds: 60, Resources: business.ResourceLimits{CPU: "2", Memory: "512Mi", Disk: "1Mi", TmpDisk: "50Mi"}}}
		if concurrent {
			acquisitionClock = &detachedNativeAcquisitionClock{ready: make(chan struct{})}
			options.Clock = acquisitionClock
		}
	}
	var service *provisioning.PlainColdService
	if variant.inventoryGateway {
		// Only this opt-in case omits the fixture's direct initialization. The
		// formal mounted operator and OpenHTTP own the actual replacement
		// Backend/issuer/Service; borrowed resources are closed through that owner.
		mountedOwner = initializeHTTPInventoryNative(t, ctx, httpDestroyNativeContext{
			backend: backend, raw: admin, options: options, identity: identity, directory: dir,
		})
		require.Zero(t, postCount.Load(), "not-ready OpenHTTP, inventory initialization and ready startup never create a Pod")
		require.Zero(t, startCount.Load(), "operator setup never starts a runtime")
		require.NoError(t, backend.Close())
		backend, service = mountedOwner.Backend(), mountedOwner.Service()
		options.Backend = backend
	} else {
		service, err = provisioning.NewPlainColdService(ctx, options)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, service.Close()) })
	}
	joinOriginalBridges := func() {
		mu.Lock()
		ownedBridges := append([]*startupBridgeFixture(nil), bridges...)
		mu.Unlock()
		for _, bridge := range ownedBridges {
			if bridge.loseCompletion {
				// The application response came from the original child. Only its
				// final adapter status is lost, after all original owners joined.
				<-bridge.done
				bridge.wg.Wait()
				require.True(t, bridge.waited && bridge.ioJoined && bridge.stdinEOF)
				require.NoError(t, bridge.waitErr)
				require.Positive(t, bridge.stdout)
				require.True(t, bridge.completionFaultWritten)
				require.False(t, bridge.successWritten)
				require.NoError(t, errors.Join(bridge.result, bridge.finalCloseErr))
				t.Log("TTL completion fault followed actual bridge Wait/IO/EOF and original TLS response")
			} else if physicalDestroy || variant.fileAPI {
				// File clients also close after the authenticated response. Reuse
				// the exact final-Close EPIPE classification: original Wait/IO
				// and the Success frame must already have completed successfully.
				requireHTTPDestroyNativeBridgeJoined(t, bridge)
			} else if execHooks != nil && execHooks.expectedFailure(bridge) {
				requireExecServiceFailedBridgeJoined(t, bridge)
			} else if lookupHooks != nil && lookupHooks.expectedFailure(bridge) {
				requireLiveLookupFailedBridgeJoined(t, bridge)
			} else {
				bridge.requireJoined(t)
			}
		}
	}
	if variant.inventoryGateway {
		exerciseHTTPGatewayInventoryNative(t, ctx, mountedOwner, httpDestroyNativeContext{
			service: service, backend: backend, raw: admin, options: options,
			request: request, workspace: w, identity: identity, directory: dir,
			observerAddress: tcp.Addr().String(), clockKey: clockKey, provider: destroyProvider,
			terminal: true, detached: true,
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return len(bridges)
		})
		require.Equal(t, int32(1), postCount.Load(), "mounted inventory business uses one actual Pod")
		require.Equal(t, int32(1), startCount.Load(), "mounted inventory business uses one actual formal launcher")
		joinOriginalBridges()
		return
	}
	if (variant.destroyLocal || variant.destroyDetached) && !variant.ttlGateway {
		exerciseHTTPDestroyNative(t, ctx, httpDestroyNativeContext{
			service: service, backend: backend, raw: admin, options: options,
			request: request, workspace: w, identity: identity, directory: dir,
			observerAddress: tcp.Addr().String(), clockKey: clockKey, provider: destroyProvider,
			terminal: variant.destroyTerminal,
			detached: detachedTerminal,
		}, variant.destroyLocal)
		joinOriginalBridges()
		return
	}
	if variant.httpGateway {
		exerciseHTTPGatewayNative(t, ctx, service, execHooks, func() {
			require.Zero(t, postCount.Load())
			require.Zero(t, startCount.Load())
		}, func() {
			require.Equal(t, int32(1), postCount.Load(), "HTTP uses one original Pod")
			require.Equal(t, int32(1), startCount.Load(), "HTTP uses one original formal launcher")
		})
		joinOriginalBridges()
		return
	}
	if variant.ttlGateway {
		var recovery *httpNativeTTLRecovery
		if variant.ttlRecovery {
			var installedJournal []byte
			type activationJournal struct {
				Version            uint32 `json:"version"`
				Digest             string `json:"digest"`
				PreviousDigest     string `json:"previous_digest"`
				Wire               []byte `json:"wire"`
				RuntimeCertificate []byte `json:"runtime_certificate"`
				IssuerCertificate  []byte `json:"issuer_certificate"`
			}
			readJournal := func() (activationJournal, []byte) {
				// These bounded reads inspect only the original launcher's journal
				// and never feed authority or replacement material to production.
				mu.Lock()
				pid := child.Process.Pid
				mu.Unlock()
				file, err := os.Open(fmt.Sprintf("/proc/%d/root%s/journal/activation.json", pid, ct.ControlDirectory))
				require.NoError(t, err)
				wire, readErr := io.ReadAll(io.LimitReader(file, 16385))
				require.NoError(t, errors.Join(readErr, file.Close()))
				require.LessOrEqual(t, len(wire), 16384)
				var actual activationJournal
				require.NoError(t, json.Unmarshal(wire, &actual))
				return actual, wire
			}
			var originalReceipt p.RuntimeReceiptIdentity
			verifier, err := p.NewManagementVerifier(binding, []ed25519.PublicKey{root.Public().(ed25519.PublicKey)})
			require.NoError(t, err)
			recovery = &httpNativeTTLRecovery{
				rotation: variant.ttlRotation,
				arm:      func() { require.False(t, loseNextCompletion.Swap(true)) },
				checkInstalled: func(candidate *state.SandboxTTLPending) {
					actual, wire := readJournal()
					require.Equal(t, uint32(2), actual.Version)
					require.Equal(t, candidate.Activation.Digest(), actual.Digest)
					require.Equal(t, candidate.PreviousDigest, actual.PreviousDigest)
					require.Equal(t, candidate.Activation.Wire(), actual.Wire)
					require.Equal(t, candidate.Activation.RuntimeCertificate(), actual.RuntimeCertificate)
					require.Equal(t, candidate.Activation.IssuerCertificate(), actual.IssuerCertificate)
					if variant.ttlRotation {
						renewed, err := verifier.VerifyRuntimeIdentityCertificate(actual.RuntimeCertificate, candidate.Activation.Identity(), time.Now().UTC())
						require.NoError(t, err)
						require.NotEmpty(t, originalReceipt.CertificateID())
						require.NotEqual(t, originalReceipt.Wire(), renewed.Wire())
						require.NotEqual(t, originalReceipt.CertificateID(), renewed.CertificateID())
						require.True(t, renewed.NotAfter().After(originalReceipt.NotAfter()))
						require.True(t, candidate.Activation.NotAfter().After(originalReceipt.NotAfter()), "the old leaf cannot cover this activation")
						require.Equal(t, originalReceipt.NotBefore(), renewed.NotBefore())
						require.Equal(t, originalReceipt.Context(), renewed.Context())
						require.Equal(t, originalReceipt.PublicKey(), renewed.PublicKey())
					}
					if installedJournal == nil {
						installedJournal = append([]byte(nil), wire...)
					} else {
						require.Equal(t, installedJournal, wire, "metadata recovery leaves the original runtime journal unchanged")
					}
				},
				restart: func() (*provisioning.PlainColdService, *state.Backend) {
					require.NoError(t, service.Close())
					require.NoError(t, backend.Close())
					backend, err = state.New(ctx, backendOptions)
					require.NoError(t, err)
					options.Backend = backend
					resumed, err := provisioning.NewPlainColdService(ctx, options)
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, resumed.Close()) })
					return resumed, backend
				},
			}
			if variant.ttlRotation {
				recovery.checkOriginal = func(view *state.SandboxReadView) {
					actual, _ := readJournal()
					require.Equal(t, uint32(1), actual.Version)
					require.Empty(t, actual.PreviousDigest)
					history, err := backend.ReadSandboxRuntime(ctx, view)
					require.NoError(t, err)
					require.NotNil(t, history)
					require.NotNil(t, history.Publication)
					request := history.Publication.Enrollment.Request
					activation, err := verifier.VerifyNodeEnrollmentRequest(request, time.Now().UTC())
					require.NoError(t, err)
					require.Equal(t, activation.Digest(), actual.Digest)
					require.Equal(t, activation.Wire(), actual.Wire)
					require.Equal(t, activation.RuntimeCertificate(), actual.RuntimeCertificate)
					require.Equal(t, activation.IssuerCertificate(), actual.IssuerCertificate)
					originalReceipt, err = verifier.VerifyRuntimeIdentityCertificate(actual.RuntimeCertificate, activation.Identity(), time.Now().UTC())
					require.NoError(t, err)
					require.True(t, originalReceipt.NotAfter().Equal(originalReceipt.NotBefore().Add(15*time.Minute+30*time.Second)), "Create retains its original fifteen-minute receipt window")
				}
			}
		}
		var destroy *httpDestroyNativeContext
		if detachedTerminal {
			// This optional continuation reuses the same formal source and
			// operator setup; it never creates a second sandbox after renewal.
			destroy = &httpDestroyNativeContext{
				service: service, backend: backend, raw: admin, options: options,
				request: request, workspace: w, identity: identity, directory: dir,
				observerAddress: tcp.Addr().String(), clockKey: clockKey, provider: destroyProvider,
				terminal: true, detached: true,
			}
		}
		exerciseHTTPGatewayTTLNative(t, ctx, service, backend, func() {
			require.Equal(t, int32(1), postCount.Load(), "TTL renewal retains the original Pod")
			require.Equal(t, int32(1), startCount.Load(), "TTL renewal retains the original formal launcher")
		}, destroy, recovery)
		joinOriginalBridges()
		return
	}
	activeService := service
	var result provisioning.PlainColdResult
	var createErr error
	var firstBusiness provisioning.PlainColdBusinessResult
	businessInput := provisioning.PlainColdBusinessInput{IdempotencyKey: request.IdempotencyKey, Request: business.CreateSandboxRequest{Mode: "persistent", Resources: &business.ResourceLimits{CPU: "2000m"}}}
	if detached && !loseACK && !concurrent {
		businessInput.IdempotencyKey = ""
	}
	if detached {
		// Mutating caller-owned options after constructor return cannot change
		// the service's operator identity or first-use defaults.
		options.Business.Principal = "mutated-operator"
		options.Business.Defaults.TimeoutSeconds = 90
	}
	if !detached {
		result, createErr = service.Create(ctx, w, request)
	} else {
		if concurrent {
			acquisitionClock.armed.Store(true)
			type answer struct {
				result provisioning.PlainColdBusinessResult
				err    error
			}
			answers := make(chan answer, 2)
			for n := 0; n < 2; n++ {
				go func() {
					value, callErr := service.CreateBusiness(ctx, businessInput)
					answers <- answer{value, callErr}
				}()
			}
			one, two := <-answers, <-answers
			if one.err != nil && two.err == nil {
				one, two = two, one
			}
			require.NoError(t, one.err, "one original first caller completes")
			require.Equal(t, one.result.RequestID, two.result.RequestID, "first-absent loser authenticates original request")
			require.Equal(t, one.result.IntentID, two.result.IntentID)
			require.Equal(t, one.result.SandboxID, two.result.SandboxID)
			if two.err != nil {
				require.Equal(t, provisioning.PlainColdRetryable, two.result.State)
			}
			firstBusiness, createErr = one.result, one.err
		} else {
			firstBusiness, createErr = service.CreateBusiness(ctx, businessInput)
		}
		result = firstBusiness.PlainColdResult
		if businessInput.IdempotencyKey == "" {
			_, err := uuid.Parse(firstBusiness.IdempotencyKey)
			require.NoError(t, err)
			businessInput.IdempotencyKey = firstBusiness.IdempotencyKey
		} else {
			require.Equal(t, businessInput.IdempotencyKey, firstBusiness.IdempotencyKey)
		}
		request.IdempotencyKey = businessInput.IdempotencyKey
		w, err = backend.DetachedIdentity(request.Principal, request.IdempotencyKey)
		require.NoError(t, err)
		require.NotNil(t, firstBusiness.Business)
		require.Equal(t, int64(60), firstBusiness.Business.Resolved.TTL.Seconds)
		fixedRequest, err := backend.LoadRequest(ctx, request.Principal, request.IdempotencyKey)
		require.NoError(t, err)
		require.NotNil(t, fixedRequest)
		foreignRequest, err := backend.LoadRequest(ctx, "mutated-operator", request.IdempotencyKey)
		require.NoError(t, err)
		require.Nil(t, foreignRequest)
		options.Business.Principal = request.Principal
		options.Business.Defaults.TimeoutSeconds = 60
		request.RequestID, request.IntentID, request.SandboxID = result.RequestID, result.IntentID, result.SandboxID
		request.Business = firstBusiness.Business
	}
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
		if detached {
			var resumed provisioning.PlainColdBusinessResult
			resumed, createErr = recovered.CreateBusiness(ctx, businessInput)
			require.Equal(t, firstBusiness.RequestID, resumed.RequestID)
			require.Equal(t, firstBusiness.IntentID, resumed.IntentID)
			require.Equal(t, firstBusiness.SandboxID, resumed.SandboxID)
			require.Equal(t, firstBusiness.Business, resumed.Business)
			result = resumed.PlainColdResult
		} else {
			result, createErr = recovered.Resume(ctx, w, forged)
		}
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
	if variant.largeDownload {
		exerciseLargeDownloadNative(t, ctx, service, request.SandboxID)
		require.Equal(t, int32(1), postCount.Load(), "large download uses original Pod")
		require.Equal(t, int32(1), startCount.Load(), "large download uses original formal launcher")
		mu.Lock()
		bridgeCount := len(bridges)
		mu.Unlock()
		require.Equal(t, 9, bridgeCount, "three original Create, one seed and five download Exec exchanges")
		joinOriginalBridges()
		t.Log("large download all nine original bridge Cmd.Wait/IO owners joined; original16 limit unchanged")
		return
	}
	if variant.fileAPI {
		exerciseFileAPINative(t, ctx, service, request.SandboxID)
		require.Equal(t, int32(1), postCount.Load(), "file API uses original Pod")
		require.Equal(t, int32(1), startCount.Load(), "file API uses original formal launcher")
		mu.Lock()
		bridgeCount := len(bridges)
		mu.Unlock()
		require.Equal(t, 21, bridgeCount, "three Create, two setup, thirteen file and three permission-check Exec exchanges")
		joinOriginalBridges()
		t.Log("file API original formal launcher and all twenty-one physical bridge/Wait/IO owners joined")
		return
	}
	if withBusiness {
		read, err := backend.ReadWorkspaceSnapshot(ctx, w)
		require.NoError(t, err)
		require.NotNil(t, read)
		require.NotNil(t, read.Snapshot)
		var capsule struct {
			Version  int             `json:"version"`
			Business json.RawMessage `json:"business"`
		}
		require.NoError(t, json.Unmarshal(read.Snapshot.Payload, &capsule))
		require.Equal(t, 2, capsule.Version)
		restored, err := sandboxsnapshot.Decode(capsule.Business)
		require.NoError(t, err)
		require.Equal(t, request.Business, restored, "Create/ACK-loss Resume consume exact frozen v2 business through coherent snapshot read")
	}
	completedInput := result
	completedInput.Runtime = p.RuntimeReference{ID: "foreign-completed", UID: uuid.NewString(), BootID: uuid.NewString()}
	settledAgain, err := activeService.Resume(ctx, w, completedInput)
	require.NoError(t, err)
	require.Equal(t, provisioning.PlainColdCompleted, settledAgain.State)
	require.Equal(t, result.Stage, settledAgain.Stage)
	require.Equal(t, result.Runtime, settledAgain.Runtime, "completed metadata refreshes copied runtime attribution")
	if detached {
		replayed, err := activeService.CreateBusiness(ctx, businessInput)
		require.NoError(t, err)
		require.Equal(t, firstBusiness.RequestID, replayed.RequestID)
		require.Equal(t, firstBusiness.IntentID, replayed.IntentID)
		require.Equal(t, firstBusiness.SandboxID, replayed.SandboxID)
		require.Equal(t, firstBusiness.Business, replayed.Business)
		// A new service resolves new requests differently; this key must still
		// use the persisted original defaults and capsule.
		require.NoError(t, activeService.Close())
		options.Business.Defaults.TimeoutSeconds = 120
		drifted, err := provisioning.NewPlainColdService(ctx, options)
		require.NoError(t, err)
		if live {
			activeService = drifted
		}
		t.Cleanup(func() { require.NoError(t, drifted.Close()) })
		replayed, err = drifted.CreateBusiness(ctx, businessInput)
		require.NoError(t, err)
		require.Equal(t, firstBusiness.RequestID, replayed.RequestID)
		require.Equal(t, firstBusiness.IntentID, replayed.IntentID)
		require.Equal(t, firstBusiness.Business, replayed.Business)
		require.Equal(t, int64(60), replayed.Business.Resolved.TTL.Seconds)
		changed := businessInput
		changed.Request.Timeout = 90
		_, err = drifted.CreateBusiness(ctx, changed)
		require.ErrorIs(t, err, state.ErrIdempotencyConflict)
		require.Equal(t, int32(1), postCount.Load())
		require.Equal(t, int32(1), startCount.Load())
	}
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
	if live {
		exerciseLiveLookupNative(t, ctx, activeService, backend, admin, backendOptions, options, ns, w, request, result, lookupHooks, lookupClock)
		require.Equal(t, int32(1), postCount.Load(), "Lookup never creates a pod")
		require.Equal(t, int32(1), startCount.Load(), "Lookup never starts a runtime")
	}
	if variant.execService {
		exerciseExecServiceNative(t, ctx, activeService, backend, admin, options, ns, w, request, result, execHooks)
		require.Equal(t, int32(1), postCount.Load(), "Exec never creates an extra Pod")
		require.Equal(t, int32(1), startCount.Load(), "Exec uses the original runtime")
	}
	joinOriginalBridges()
	t.Log("cold service actual POST-to-formal-launcher/publication/independent preparation settlement completed")
}
