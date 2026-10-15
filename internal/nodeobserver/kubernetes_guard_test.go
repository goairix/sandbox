package nodeobserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	p "github.com/goairix/sandbox/internal/runtime/controlprotocol"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestNodeObserverProviderExactHTTP(t *testing.T) {
	zero, no, yes := int64(0), false, true
	nodeUID, boot := "da1c1595-cbfd-4d2e-9ef7-f4dd3c84b172", "ea1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
	intent := "ca1c1595-cbfd-4d2e-9ef7-f4dd3c84b172"
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", Namespace: "runtimes", UID: "pod-uid", Annotations: map[string]string{"sandbox.creation.intent": intent}, Finalizers: []string{runtimeTerminationFinalizer}, DeletionTimestamp: &metav1.Time{Time: time.Now().UTC()}}, Spec: v1.PodSpec{NodeName: "node-a", RestartPolicy: v1.RestartPolicyNever, SecurityContext: &v1.PodSecurityContext{SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}}, Containers: []v1.Container{{Name: "sandbox", SecurityContext: &v1.SecurityContext{RunAsUser: &zero, Privileged: &no, AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: &yes, Capabilities: &v1.Capabilities{Drop: []v1.Capability{"ALL"}, Add: []v1.Capability{"KILL", "SETUID", "SETGID", "SETPCAP"}}}}}}}
	node := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a", UID: types.UID(nodeUID)}, Status: v1.NodeStatus{NodeInfo: v1.NodeSystemInfo{BootID: boot, OperatingSystem: "linux", Architecture: "arm64"}}}
	pod.ResourceVersion = "1"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected provider mutation %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/nodes/node-a":
			_ = json.NewEncoder(w).Encode(node)
		case "/api/v1/namespaces/runtimes/pods/pod-a":
			_ = json.NewEncoder(w).Encode(pod)
		default:
			t.Errorf("unexpected provider point %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	guard := providerGuard{client: client, namespace: "runtimes", nodeName: "node-a", nodeUID: nodeUID, nodeBootID: boot}
	reg := providerRegistration{Identity: p.RuntimeIdentityContext{SandboxID: "sandbox-a", Generation: 1, Runtime: p.RuntimeReference{ID: "pod-a", UID: "pod-uid", BootID: boot}}, Birth: p.BirthContext{UID: 1000, GID: 1000}, CreationIntentID: intent}
	if _, err := guard.observe(context.Background(), reg, true); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(){
		"uid":      func() { pod.UID = "replacement" },
		"version":  func() { pod.ResourceVersion = "" },
		"Never":    func() { pod.Spec.RestartPolicy = v1.RestartPolicyAlways },
		"deleting": func() { pod.DeletionTimestamp = nil },
		"sidecar":  func() { pod.Spec.InitContainers = []v1.Container{{Name: "sidecar"}} },
		"node":     func() { node.UID = "replacement" },
		"boot":     func() { node.Status.NodeInfo.BootID = "replacement" },
	} {
		t.Run(name, func(t *testing.T) {
			oldPod, oldNode := pod.DeepCopy(), node.DeepCopy()
			mutate()
			if _, err := guard.observe(context.Background(), reg, true); err == nil {
				t.Fatal("unresolved provider condition accepted")
			}
			pod, node = oldPod, oldNode
		})
	}
}
