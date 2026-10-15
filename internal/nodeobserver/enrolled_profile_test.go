package nodeobserver

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/goairix/sandbox/internal/runtime"
	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	kube "github.com/goairix/sandbox/internal/runtime/kubernetes"
	"github.com/goairix/sandbox/internal/runtime/launcherconfig"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

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
