package kubernetes

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

var cleanupSteps = []TaskRuntimeCleanupStep{TaskRuntimeCleanupInstallRetainFinalizer, TaskRuntimeCleanupDeletePod, TaskRuntimeCleanupDeleteNetworkPolicyLogical, TaskRuntimeCleanupDeleteNetworkPolicyRuntime, TaskRuntimeCleanupDeleteCiliumPolicyLogical, TaskRuntimeCleanupDeleteCiliumPolicyRuntime, TaskRuntimeCleanupRemoveRetainFinalizer}

type cleanupReceived struct {
	method, path, contentType string
	body                      []byte
	protocol                  int
	remote                    string
}

type cleanupAPIFixture struct {
	t        *testing.T
	mu       sync.Mutex
	pod      *v1.Pod
	node     *v1.Node
	policies map[string]*unstructured.Unstructured
	received []cleanupReceived
	mutation func(http.ResponseWriter, *http.Request) bool
	server   *httptest.Server
}

func (f *cleanupAPIFixture) receipts() []cleanupReceived {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cleanupReceived(nil), f.received...)
}

func newCleanupAPIFixture(t *testing.T, sameName bool) (*cleanupAPIFixture, TaskRuntimeCleanupOptions, TaskRuntimeCleanupBinding) {
	t.Helper()
	b := cleanupBinding()
	if sameName {
		b.LogicalID = b.Runtime.ID
	}
	f := &cleanupAPIFixture{t: t, pod: cleanupPod(), node: cleanupNode(), policies: map[string]*unstructured.Unstructured{}}
	f.pod.Labels["sandbox.id"] = b.LogicalID
	for i, target := range taskCleanupTargetNames(b, true) {
		id := ordinaryNetworkIdentity{runtimeID: b.Runtime.ID, runtimeUID: types.UID(b.Runtime.UID), logicalID: b.LogicalID}
		if target.Name == "sandbox-"+b.Runtime.ID || target.Name == "sandbox-private-deny-"+b.Runtime.ID {
			id.logicalID = b.Runtime.ID
		}
		var object map[string]any
		if target.Kind == "NetworkPolicy" {
			policy, err := buildOrdinaryNetworkPolicy("runtimes", id, "attempt", false, nil, false)
			require.NoError(t, err)
			wire, err := json.Marshal(policy)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(wire, &object))
		} else {
			policy, err := buildOrdinaryCiliumPrivateDeny("runtimes", id, "attempt")
			require.NoError(t, err)
			object = policy.Object
		}
		policy := &unstructured.Unstructured{Object: object}
		policy.SetUID(types.UID("88888888-8888-4888-8888-88888888888" + strconv.Itoa(i)))
		policy.SetResourceVersion("10")
		prefix := "/apis/networking.k8s.io/v1/namespaces/runtimes/networkpolicies/"
		if target.Kind == "CiliumNetworkPolicy" {
			prefix = "/apis/cilium.io/v2/namespaces/runtimes/ciliumnetworkpolicies/"
		}
		f.policies[prefix+target.Name] = policy
	}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.server.EnableHTTP2 = true
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	o := cleanupOptions(f.server.URL)
	o.IncludeCilium = true
	o.Config.CAData = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw})
	return f, o, b
}

func (f *cleanupAPIFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		if r.URL.RawQuery != "" {
			f.t.Errorf("unexpected query: %s", r.URL.RawQuery)
		}
		var object any
		switch r.URL.Path {
		case "/api/v1/namespaces/runtimes/pods/runtime-a":
			if f.pod != nil {
				object = f.pod
			}
		case "/api/v1/nodes/node-a":
			object = f.node
		default:
			if policy := f.policies[r.URL.Path]; policy != nil {
				object = policy
			}
		}
		if object == nil {
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(metav1.Status{Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404})
		} else {
			json.NewEncoder(w).Encode(object)
		}
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("request body: %v", err)
		return
	}
	f.received = append(f.received, cleanupReceived{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), body: body, protocol: r.ProtoMajor, remote: r.RemoteAddr})
	if f.mutation != nil && f.mutation(w, r) {
		return
	}
	if r.Method == http.MethodPatch {
		var patch []struct {
			Op, Path string
			Value    json.RawMessage
		}
		if err := json.Unmarshal(body, &patch); err != nil || len(patch) != 3 || f.pod == nil {
			f.t.Errorf("invalid patch: %s", body)
			w.WriteHeader(400)
			return
		}
		var finalizers []string
		if err := json.Unmarshal(patch[2].Value, &finalizers); err != nil {
			f.t.Error(err)
			w.WriteHeader(400)
			return
		}
		f.pod.Finalizers = finalizers
		rv, err := strconv.Atoi(f.pod.ResourceVersion)
		if err != nil {
			f.t.Error(err)
			return
		}
		f.pod.ResourceVersion = strconv.Itoa(rv + 1)
		result := f.pod.DeepCopy()
		if f.pod.DeletionTimestamp != nil && len(finalizers) == 0 {
			f.pod = nil
		}
		json.NewEncoder(w).Encode(result)
		return
	}
	if r.Method != http.MethodDelete {
		f.t.Errorf("unexpected method %s", r.Method)
		w.WriteHeader(400)
		return
	}
	if r.URL.Path == "/api/v1/namespaces/runtimes/pods/runtime-a" {
		if f.pod == nil {
			f.t.Error("DELETE absent Pod")
			w.WriteHeader(404)
			return
		}
		now := metav1.NewTime(time.Now().UTC())
		f.pod.DeletionTimestamp = &now
		f.pod.ResourceVersion = "3"
	} else {
		delete(f.policies, r.URL.Path)
	}
	json.NewEncoder(w).Encode(metav1.Status{Status: "Success", Code: 200})
}

func TestTaskRuntimeCleanupClientEffects(t *testing.T) {
	for _, sameName := range []bool{false, true} {
		t.Run(strconv.FormatBool(sameName), func(t *testing.T) {
			f, options, binding := newCleanupAPIFixture(t, sameName)
			client, err := NewTaskRuntimeCleanupClient(options)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			h, err := client.Prepare(context.Background(), binding, nil)
			require.NoError(t, err)
			_, err = h.Apply(context.Background(), TaskRuntimeCleanupInstallRetainFinalizer)
			require.Error(t, err, "an exact preflight is mandatory")
			require.Empty(t, f.receipts())
			for _, step := range cleanupSteps {
				snapshot, err := h.ObserveStep(context.Background(), step)
				require.NoError(t, err)
				require.Equal(t, step, snapshot.Step)
				before := len(f.receipts())
				attempt, err := h.Apply(context.Background(), step)
				require.NoError(t, err)
				require.Equal(t, snapshot, attempt, "ACK does not invent readback or terminal proof")
				if snapshot.Absent {
					require.Len(t, f.receipts(), before)
					continue
				}
				require.Len(t, f.receipts(), before+1)
				r := f.receipts()[before]
				require.Equal(t, 1, r.protocol, "even an HTTP/2-enabled TLS server uses HTTP/1")
				if r.method == http.MethodDelete {
					var body metav1.DeleteOptions
					require.NoError(t, json.Unmarshal(r.body, &body))
					require.NotNil(t, body.Preconditions)
					require.Equal(t, snapshot.UID, string(*body.Preconditions.UID))
					require.Equal(t, snapshot.ResourceVersion, *body.Preconditions.ResourceVersion)
				} else {
					require.Equal(t, "application/json-patch+json", r.contentType)
					var patch []map[string]any
					require.NoError(t, json.Unmarshal(r.body, &patch))
					require.Equal(t, map[string]any{"op": "test", "path": "/metadata/uid", "value": snapshot.UID}, patch[0])
					require.Equal(t, map[string]any{"op": "test", "path": "/metadata/resourceVersion", "value": snapshot.ResourceVersion}, patch[1])
				}
				_, err = h.ObserveStep(context.Background(), step)
				require.NoError(t, err)
				_, err = h.Apply(context.Background(), step)
				require.Error(t, err, "reconciliation must never reset a consumed slot")
				require.Len(t, f.receipts(), before+1)
			}
			expected := 7
			if sameName {
				expected = 5
			}
			require.Len(t, f.receipts(), expected)
			remotes := map[string]bool{}
			for _, request := range f.receipts() {
				require.False(t, remotes[request.remote], "mutation connections are not reused")
				remotes[request.remote] = true
			}
			obs, err := h.Observe(context.Background())
			require.NoError(t, err)
			require.True(t, obs.Absent)
			require.True(t, obs.Terminal, "this is only the old Pod artifact's absence")
			t.Logf("actual received mutations=%d (PATCH=2; DELETE=%d)", len(f.receipts()), expected-2)
		})
	}
}

func TestTaskRuntimeCleanupClientEffectsSnapshot(t *testing.T) {
	for _, change := range []string{"pod-rv", "pod-uid", "policy-rv", "policy-uid", "foreign-policy", "legacy-policy", "foreign-finalizer", "node-boot", "unknown-mount"} {
		t.Run(change, func(t *testing.T) {
			f, options, binding := newCleanupAPIFixture(t, false)
			f.pod.Finalizers = []string{taskCleanupFinalizer}
			now := metav1.Now()
			f.pod.DeletionTimestamp = &now
			client, err := NewTaskRuntimeCleanupClient(options)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			h, err := client.Prepare(context.Background(), binding, nil)
			require.NoError(t, err)
			step := TaskRuntimeCleanupRemoveRetainFinalizer
			if change == "policy-rv" || change == "policy-uid" || change == "foreign-policy" || change == "legacy-policy" {
				step = TaskRuntimeCleanupDeleteNetworkPolicyLogical
			}
			_, err = h.ObserveStep(context.Background(), step)
			require.NoError(t, err)
			f.mu.Lock()
			policy := f.policies["/apis/networking.k8s.io/v1/namespaces/runtimes/networkpolicies/sandbox-logical-a"]
			switch change {
			case "pod-rv":
				f.pod.ResourceVersion = "99"
			case "pod-uid":
				f.pod.UID = "replacement"
			case "policy-rv":
				policy.SetResourceVersion("99")
			case "policy-uid":
				policy.SetUID("replacement")
			case "foreign-policy":
				labels := policy.GetLabels()
				labels[ordinaryRuntimeIDLabel] = "foreign"
				policy.SetLabels(labels)
			case "legacy-policy":
				labels := policy.GetLabels()
				delete(labels, ordinaryPolicyRoleLabel)
				policy.SetLabels(labels)
			case "foreign-finalizer":
				f.pod.Finalizers = append(f.pod.Finalizers, "foreign/finalizer")
			case "node-boot":
				f.node.Status.NodeInfo.BootID = "replacement"
			case "unknown-mount":
				f.pod.Spec.Volumes = []v1.Volume{{Name: "foreign", VolumeSource: v1.VolumeSource{HostPath: &v1.HostPathVolumeSource{Path: "/"}}}}
			}
			f.mu.Unlock()
			_, err = h.Apply(context.Background(), step)
			require.Error(t, err)
			require.Empty(t, f.receipts())
			_, err = h.ObserveStep(context.Background(), step)
			_, err = h.Apply(context.Background(), step)
			require.Error(t, err, "even a newer read cannot reset consumption")
			require.Empty(t, f.receipts())
		})
	}
}
