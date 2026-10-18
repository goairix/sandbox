package nodeobserver

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/goairix/sandbox/internal/fuseprotocol"
	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/goairix/sandbox/internal/runtime"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// This API/renderer component uses retained original intent, not a signed
// enrollment positive. A real helper channel must additionally locate the
// original verified slot and authenticate the independent kernel peer.
func TestEnrolledObserverFUSEV2RequiresOriginalTuple(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	launcherImage := "example.test/sandbox@sha256:" + strings.Repeat("b", 64)
	mounterImage := "example.test/mounter@sha256:" + strings.Repeat("c", 64)
	appArmor := "sandbox-fuse-" + strings.Repeat("e", 64)
	contract, err := kubecontract.EnrolledFUSEContractDigest(launcherImage, mounterImage, appArmor)
	require.NoError(t, err)
	config := launcherconfig.Config{
		Version: 2,
		Binding: launcherconfig.Binding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "restore"},
		Roots:   []ed25519.PublicKey{key}, UID: 1000, GID: 1000, ContractDigest: contract,
		ClockSocket: "/run/sandbox-clock/clock.sock", ClockPublicKey: key, ClockAudience: "launcher", MaxActive: 4,
		Enrollment: launcherconfig.Enrollment{NodeUID: "22222222-2222-4222-8222-222222222222", NodeBootID: "33333333-3333-4333-8333-333333333333"},
	}
	configWire, err := json.Marshal(config)
	require.NoError(t, err)
	resources := v1.ResourceRequirements{
		Limits:   v1.ResourceList{v1.ResourceCPU: resource.MustParse("250m"), v1.ResourceMemory: resource.MustParse("256Mi")},
		Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m"), v1.ResourceMemory: resource.MustParse("128Mi")},
	}
	input := kubecontract.EnrolledFUSEPodSpecInput{
		NodeName: "node-a", LauncherImage: launcherImage, MounterImage: mounterImage,
		MounterAppArmorProfile: appArmor, LauncherConfig: configWire,
		LauncherResources: resources, MounterResources: resources, WorkspaceBytes: 64 << 20, TmpBytes: 32 << 20,
		Bootstrap: fuseprotocol.BootstrapConfig{
			Version: 1, Provider: "minio", Bucket: "workspaces", Endpoint: "https://storage.example.test", Region: "us-east-1",
			Profile: "minio-sigv4-path-style-v1", PasswdFile: "/run/s3fs/passwd-s3fs", CacheDir: "/var/cache/s3fs", MountPath: "/workspace",
			CacheLimitBytes: 128 << 20, PoolKey: strings.Repeat("d", 64), MountTimeoutSeconds: 30, FlushTimeoutSeconds: 30, UnmountTimeoutSeconds: 15,
		},
	}
	spec, err := kubecontract.EnrolledFUSEPodSpec(input)
	require.NoError(t, err)
	digest := sha256.Sum256(configWire)
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "pod-a", Namespace: "runtimes", UID: "55555555-5555-4555-8555-555555555555", ResourceVersion: "1",
		Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "pod-a", "sandbox.workspace.mode": "fuse"},
		Annotations: map[string]string{
			kubecontract.EnrolledProfileAnnotation:        kubecontract.EnrolledFUSEProfile,
			kubecontract.EnrolledGenerationAnnotation:     "1",
			kubecontract.EnrolledNodeUIDAnnotation:        config.Enrollment.NodeUID,
			kubecontract.EnrolledNodeBootIDAnnotation:     config.Enrollment.NodeBootID,
			kubecontract.EnrolledConfigDigestAnnotation:   hex.EncodeToString(digest[:]),
			kubecontract.EnrolledImageDigestAnnotation:    strings.Repeat("b", 64),
			kubecontract.EnrolledCreationIntentAnnotation: "44444444-4444-4444-8444-444444444444",
		},
	}, Spec: spec}
	_, err = kubecontract.InspectEnrolledFUSEPod(pod)
	require.NoError(t, err, "the positive source must satisfy the real complete Inspector")
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID(config.Enrollment.NodeUID)}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: config.Enrollment.NodeBootID, OperatingSystem: "linux", Architecture: "arm64"}}}
	registration := providerRegistration{
		Binding:          config.TrustBinding(),
		Identity:         p.RuntimeIdentityContext{SandboxID: "pod-a", Generation: 1, Runtime: p.RuntimeReference{ID: "pod-a", UID: string(pod.UID), BootID: config.Enrollment.NodeBootID}},
		Birth:            p.BirthContext{UID: 1000, GID: 1000, ContractDigest: contract},
		CreationIntentID: pod.Annotations[kubecontract.EnrolledCreationIntentAnnotation],
	}
	var podGets atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		switch r.URL.Path {
		case "/api/v1/nodes/node-a":
			_ = json.NewEncoder(w).Encode(node)
		case "/api/v1/namespaces/runtimes/pods/pod-a":
			podGets.Add(1)
			_ = json.NewEncoder(w).Encode(pod)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer api.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: api.URL})
	require.NoError(t, err)
	guard := providerGuard{client: client, namespace: "runtimes", nodeName: "node-a", nodeUID: config.Enrollment.NodeUID, nodeBootID: config.Enrollment.NodeBootID}
	_, err = guard.observe(context.Background(), registration, false)
	require.NoError(t, err, "the explicit FUSE branch must accept the exact fixed helper without widening launcher confinement")
	require.Equal(t, int32(1), podGets.Load())

	for name, mutate := range map[string]func(*providerRegistration){
		"contract":   func(r *providerRegistration) { r.Birth.ContractDigest = strings.Repeat("f", 64) },
		"birth-user": func(r *providerRegistration) { r.Birth.UID++ },
		"generation": func(r *providerRegistration) { r.Identity.Generation++ },
		"intent":     func(r *providerRegistration) { r.CreationIntentID = "66666666-6666-4666-8666-666666666666" },
		"authority":  func(r *providerRegistration) { r.Binding.AuthorityID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := registration
			mutate(&changed)
			_, err := guard.observe(context.Background(), changed, false)
			require.Error(t, err)
		})
	}
	for name, mutate := range map[string]func(*v1.Pod){
		"unknown-profile": func(p *v1.Pod) { p.Annotations[kubecontract.EnrolledProfileAnnotation] = "sandbox-enrolled-fuse/v1" },
		"extra-helper":    func(p *v1.Pod) { p.Spec.InitContainers = append(p.Spec.InitContainers, v1.Container{Name: "other"}) },
		"launcher-escape": func(p *v1.Pod) {
			p.Spec.Containers[0].SecurityContext.Capabilities.Add = append(p.Spec.Containers[0].SecurityContext.Capabilities.Add, "SYS_ADMIN")
		},
		"helper-health-write": func(p *v1.Pod) { p.Spec.InitContainers[0].VolumeMounts[4].ReadOnly = false },
	} {
		t.Run(name, func(t *testing.T) {
			original := pod
			pod = pod.DeepCopy()
			defer func() { pod = original }()
			mutate(pod)
			_, err := guard.observe(context.Background(), registration, false)
			require.Error(t, err)
		})
	}
}

func TestEnrolledObserverRequiresOriginalTuple(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	image := "example.test/sandbox@sha256:" + strings.Repeat("b", 64)
	contract, err := kubecontract.EnrolledPlainContractDigest(image)
	require.NoError(t, err)
	c := launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "restore"}, Roots: []ed25519.PublicKey{key}, UID: 1000, GID: 1000, ContractDigest: contract, ClockSocket: "/run/sandbox-clock/clock.sock", ClockPublicKey: key, ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: "22222222-2222-4222-8222-222222222222", NodeBootID: "33333333-3333-4333-8333-333333333333"}}
	wire, err := json.Marshal(c)
	require.NoError(t, err)
	pod, err := kube.BuildEnrolledPlainPod("runtimes", kube.EnrolledPlainPodRequest{Spec: runtime.SandboxSpec{ID: "pod-a", Image: image, CPU: "500m", Memory: "512Mi", Disk: "100Mi", ReadOnlyRootFS: true, RunAsUser: 1000}, NodeName: "node-a", NodeUID: c.Enrollment.NodeUID, NodeBootID: c.Enrollment.NodeBootID, CreationIntentID: "44444444-4444-4444-8444-444444444444", Generation: 1, Config: wire})
	require.NoError(t, err)
	pod.UID, pod.ResourceVersion = "55555555-5555-4555-8555-555555555555", "1"
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID(c.Enrollment.NodeUID)}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: c.Enrollment.NodeBootID, OperatingSystem: "linux", Architecture: "arm64"}}}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.WriteHeader(405)
			return
		}
		switch r.URL.Path {
		case "/api/v1/nodes/node-a":
			_ = json.NewEncoder(w).Encode(node)
		case "/api/v1/namespaces/runtimes/pods/pod-a":
			_ = json.NewEncoder(w).Encode(pod)
		default:
			w.WriteHeader(404)
		}
	}))
	defer api.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: api.URL})
	require.NoError(t, err)
	guard := providerGuard{client: client, namespace: "runtimes", nodeName: "node-a", nodeUID: c.Enrollment.NodeUID, nodeBootID: c.Enrollment.NodeBootID}
	registration := providerRegistration{Identity: p.RuntimeIdentityContext{SandboxID: "pod-a", Generation: 1, Runtime: p.RuntimeReference{ID: "pod-a", UID: string(pod.UID), BootID: c.Enrollment.NodeBootID}}, Birth: p.BirthContext{UID: 1000, GID: 1000, ContractDigest: contract}, CreationIntentID: pod.Annotations[kubecontract.EnrolledCreationIntentAnnotation]}
	// Binding is set separately by the new verified-activation integration.
	registration.Binding = c.TrustBinding()
	_, err = guard.observe(context.Background(), registration, false)
	require.NoError(t, err)
	for name, mutate := range map[string]func(*providerRegistration){
		"logical":    func(r *providerRegistration) { r.Identity.SandboxID = "other" },
		"generation": func(r *providerRegistration) { r.Identity.Generation++ },
		"user":       func(r *providerRegistration) { r.Birth.UID++ },
		"group":      func(r *providerRegistration) { r.Birth.GID++ },
		"network":    func(r *providerRegistration) { r.Birth.NetworkAllowed = true },
		"contract":   func(r *providerRegistration) { r.Birth.ContractDigest = strings.Repeat("a", 64) },
		"authority":  func(r *providerRegistration) { r.Binding.AuthorityID = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			copy := registration
			mutate(&copy)
			_, err := guard.observe(context.Background(), copy, false)
			require.Error(t, err)
		})
	}
	for _, marker := range []string{"unknown", ""} {
		pod.Annotations[kubecontract.EnrolledProfileAnnotation] = marker
		if marker == "" {
			delete(pod.Annotations, kubecontract.EnrolledProfileAnnotation)
		}
		_, err := guard.observe(context.Background(), registration, false)
		require.Error(t, err)
	}
}

func TestEnrolledObserverRejectsSelfConsistentImageReplacement(t *testing.T) {
	key := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
	image := "example.test/sandbox@sha256:" + strings.Repeat("b", 64)
	contract, err := kubecontract.EnrolledPlainContractDigest(image)
	require.NoError(t, err)
	c := launcherconfig.Config{Version: 2, Binding: launcherconfig.Binding{Namespace: "/sandbox/control/authority/", AuthorityID: "authority", Target: "target", RestoreEpoch: "restore"}, Roots: []ed25519.PublicKey{key}, UID: 1000, GID: 1000, ContractDigest: contract, ClockSocket: "/run/sandbox-clock/clock.sock", ClockPublicKey: key, ClockAudience: "launcher", MaxActive: 4, Enrollment: launcherconfig.Enrollment{NodeUID: "22222222-2222-4222-8222-222222222222", NodeBootID: "33333333-3333-4333-8333-333333333333"}}
	wire, err := json.Marshal(c)
	require.NoError(t, err)
	pod, err := kube.BuildEnrolledPlainPod("runtimes", kube.EnrolledPlainPodRequest{Spec: runtime.SandboxSpec{ID: "pod-a", Image: image, CPU: "500m", Memory: "512Mi", Disk: "100Mi", ReadOnlyRootFS: true, RunAsUser: 1000}, NodeName: "node-a", NodeUID: c.Enrollment.NodeUID, NodeBootID: c.Enrollment.NodeBootID, CreationIntentID: "44444444-4444-4444-8444-444444444444", Generation: 1, Config: wire})
	require.NoError(t, err)
	pod.UID, pod.ResourceVersion = "55555555-5555-4555-8555-555555555555", "1"
	registration := providerRegistration{Binding: c.TrustBinding(), Identity: p.RuntimeIdentityContext{SandboxID: "pod-a", Generation: 1, Runtime: p.RuntimeReference{ID: "pod-a", UID: string(pod.UID), BootID: c.Enrollment.NodeBootID}}, Birth: p.BirthContext{UID: 1000, GID: 1000, ContractDigest: contract}, CreationIntentID: pod.Annotations[kubecontract.EnrolledCreationIntentAnnotation]}
	originalRegistration := registration
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID(c.Enrollment.NodeUID)}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: c.Enrollment.NodeBootID, OperatingSystem: "linux", Architecture: "arm64"}}}
	observe := func(candidate *v1.Pod) error {
		t.Helper()
		var podGets atomic.Int32
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			switch r.URL.Path {
			case "/api/v1/nodes/node-a":
				_ = json.NewEncoder(w).Encode(node)
			case "/api/v1/namespaces/runtimes/pods/pod-a":
				podGets.Add(1)
				_ = json.NewEncoder(w).Encode(candidate)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}))
		defer api.Close()
		client, err := kubernetes.NewForConfig(&rest.Config{Host: api.URL})
		require.NoError(t, err)
		guard := providerGuard{client: client, namespace: "runtimes", nodeName: "node-a", nodeUID: c.Enrollment.NodeUID, nodeBootID: c.Enrollment.NodeBootID}
		_, err = guard.observe(context.Background(), registration, false)
		require.Equal(t, int32(1), podGets.Load(), "consumer must fetch the candidate Pod")
		return err
	}
	require.NoError(t, observe(pod))

	replacement := pod.DeepCopy()
	replacement.Spec.Containers[0].Image = "example.test/sandbox@sha256:" + strings.Repeat("c", 64)
	replacement.Annotations[kubecontract.EnrolledImageDigestAnnotation] = strings.Repeat("c", 64)
	replacementConfig, err := launcherconfig.Decode([]byte(replacement.Spec.Containers[0].Env[0].Value))
	require.NoError(t, err)
	replacementConfig.ContractDigest, err = kubecontract.EnrolledPlainContractDigest(replacement.Spec.Containers[0].Image)
	require.NoError(t, err)
	replacementWire, err := json.Marshal(replacementConfig)
	require.NoError(t, err)
	replacement.Spec.Containers[0].Env[0].Value = string(replacementWire)
	digest := sha256.Sum256(replacementWire)
	replacement.Annotations[kubecontract.EnrolledConfigDigestAnnotation] = hex.EncodeToString(digest[:])
	checked, err := kubecontract.InspectEnrolledPlainPod(replacement)
	require.NoError(t, err, "replacement must pass structural validation")
	require.NotEqual(t, registration.Birth.ContractDigest, checked.Config.ContractDigest)
	require.EqualError(t, observe(replacement), "observer provider refused: enrolled profile differs from verified activation")
	require.Equal(t, originalRegistration, registration, "the original verified tuple must stay unchanged")
}
