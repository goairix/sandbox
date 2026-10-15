package kubernetes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/kubecontract"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
)

func bridgeCandidate(t *testing.T) (*v1.Pod, *v1.Node, EnrolledBridgeTarget) {
	t.Helper()
	pod, err := BuildEnrolledPlainPod("runtimes", enrolledPodRequest(t))
	require.NoError(t, err)
	pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
	pod.UID = types.UID("11111111-1111-4111-8111-111111111111")
	profile, err := kubecontract.InspectEnrolledPlainPod(pod)
	require.NoError(t, err)
	target := EnrolledBridgeTarget{PodName: pod.Name, PodUID: string(pod.UID), NodeName: pod.Spec.NodeName, NodeUID: profile.NodeUID, NodeBootID: profile.NodeBootID, LogicalID: profile.LogicalID, CreationIntentID: profile.CreationIntentID, Generation: profile.Generation, Authority: profile.Config.TrustBinding(), ContractDigest: profile.Config.ContractDigest, UID: profile.Config.UID, GID: profile.Config.GID, NetworkAllowed: profile.Config.NetworkAllowed}
	node := &v1.Node{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"}, ObjectMeta: metav1.ObjectMeta{Name: target.NodeName, UID: types.UID(target.NodeUID)}}
	node.Status.NodeInfo.BootID, node.Status.NodeInfo.OperatingSystem = target.NodeBootID, "linux"
	return pod, node, target
}

func bridgeOptions(origin string) EnrolledBridgeOptions {
	return EnrolledBridgeOptions{Config: &rest.Config{Host: origin}, Namespace: "runtimes", MaxActive: 2, RequestTimeout: time.Second}
}

func TestEnrolledBridgePointChecks(t *testing.T) {
	pod, node, target := bridgeCandidate(t)
	var mu sync.Mutex
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/exec") {
			http.Error(w, "fixture refuses upgrade", http.StatusBadRequest)
			return
		}
		require.True(t, r.Close)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/nodes/"+node.Name {
			_ = json.NewEncoder(w).Encode(node)
		} else {
			_ = json.NewEncoder(w).Encode(pod)
		}
	}))
	defer server.Close()
	c, err := NewEnrolledBridgeClient(bridgeOptions(server.URL))
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Dial(context.Background(), target)
	require.Error(t, err, "the exact candidate reaches the real upgrade request")
	mu.Lock()
	require.Equal(t, []string{"GET /api/v1/namespaces/runtimes/pods/" + pod.Name, "GET /api/v1/nodes/" + node.Name, "GET /api/v1/namespaces/runtimes/pods/" + pod.Name + "/exec"}, paths)
	mu.Unlock()
	for name, mutate := range map[string]func(*EnrolledBridgeTarget){
		"pod uid":    func(v *EnrolledBridgeTarget) { v.PodUID = "22222222-2222-4222-8222-222222222222" },
		"node uid":   func(v *EnrolledBridgeTarget) { v.NodeUID = "88888888-8888-4888-8888-888888888888" },
		"node boot":  func(v *EnrolledBridgeTarget) { v.NodeBootID = "22222222-2222-4222-8222-222222222222" },
		"logical":    func(v *EnrolledBridgeTarget) { v.LogicalID = "different" },
		"intent":     func(v *EnrolledBridgeTarget) { v.CreationIntentID = "22222222-2222-4222-8222-222222222222" },
		"generation": func(v *EnrolledBridgeTarget) { v.Generation++ },
		"authority":  func(v *EnrolledBridgeTarget) { v.Authority.AuthorityID = "different" },
		"uid":        func(v *EnrolledBridgeTarget) { v.UID++ },
		"gid":        func(v *EnrolledBridgeTarget) { v.GID++ },
		"network":    func(v *EnrolledBridgeTarget) { v.NetworkAllowed = !v.NetworkAllowed },
		"contract": func(v *EnrolledBridgeTarget) {
			v.ContractDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := target
			mutate(&candidate)
			_, err := c.Dial(context.Background(), candidate)
			require.ErrorIs(t, err, ErrEnrolledBridge)
		})
	}
}

func TestEnrolledBridgeStaticAndOriginSeal(t *testing.T) {
	for _, mutate := range []func(*rest.Config){
		func(c *rest.Config) { c.BearerTokenFile = "forbidden" },
		func(c *rest.Config) { c.Insecure = true },
		func(c *rest.Config) { c.Host += "/path" },
		func(c *rest.Config) { c.BearerToken = "token\r\n" },
		func(c *rest.Config) { c.Proxy = http.ProxyFromEnvironment },
	} {
		o := bridgeOptions("http://127.0.0.1:1")
		mutate(o.Config)
		_, err := NewEnrolledBridgeClient(o)
		require.Error(t, err)
		_, err = NewTaskRuntimeCleanupClient(TaskRuntimeCleanupOptions{Config: o.Config, Namespace: "runtimes", MaxActive: 1, RequestTimeout: time.Second})
		require.Error(t, err, "cleanup static rejection must survive extraction")
	}
	c, err := NewEnrolledBridgeClient(bridgeOptions("http://127.0.0.1:1"))
	require.NoError(t, err)
	defer c.Close()
	copy := reflect.New(reflect.TypeOf(c).Elem())
	copy.Elem().Set(reflect.ValueOf(c).Elem())
	require.ErrorIs(t, copy.Interface().(*EnrolledBridgeClient).Close(), ErrEnrolledBridge)
	var zero EnrolledBridgeClient
	require.ErrorIs(t, zero.Close(), ErrEnrolledBridge)
}

func TestEnrolledBridgeCleanupSnapshotRegression(t *testing.T) {
	server, requests := cleanupServer(t, cleanupPod())
	client, err := NewTaskRuntimeCleanupClient(cleanupOptions(server.URL))
	require.NoError(t, err)
	defer client.Close()
	handle, err := client.Prepare(context.Background(), cleanupBinding(), nil)
	require.NoError(t, err)
	require.NotEmpty(t, handle.Descriptor())
	require.Positive(t, requests.Load(), "original cleanup performs its real finite GETs")
}
