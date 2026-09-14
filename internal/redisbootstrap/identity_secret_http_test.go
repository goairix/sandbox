package redisbootstrap

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	api "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
)

type identityWarningCounter struct{ seen atomic.Int32 }

func (w *identityWarningCounter) HandleWarningHeaderWithContext(context.Context, int, string, string) {
	w.seen.Add(1)
}

// Exercises the real REST decoder/transport. This is not Kubernetes RBAC or CSI.
func TestEnsureIdentityHTTPCommittedServerFailure(t *testing.T) {
	const admissionWarning = "299 kube \"PRIVATE-ADMISSION-WARNING\""
	o := IdentitySecretOptions{Namespace: "isolated", StatefulSetName: "redis", SecretName: "redis-identity", StateConfigMap: "state", Members: [3]string{"redis-0", "redis-1", "redis-2"}, FreshClusterID: "http-install", Timeout: 2 * time.Second}
	data, _ := json.Marshal(ClusterState{ClusterID: o.FreshClusterID, Members: o.Members, Phase: Pending})
	cm := &api.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Namespace: o.Namespace, Name: o.StateConfigMap, UID: "cm-http-uid", ResourceVersion: "1"}, Data: map[string]string{"cluster.json": string(data)}}
	var mu sync.Mutex
	var warnings identityWarningCounter
	var stored *api.Secret
	creates := 0
	forbidden := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Warning", admissionWarning)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/isolated/configmaps/state":
			_ = json.NewEncoder(w).Encode(cm)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/namespaces/isolated/persistentvolumeclaims/data-redis-"):
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/isolated/secrets/redis-identity":
			if stored == nil {
				w.WriteHeader(404)
				_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonNotFound, Code: 404})
			} else {
				_ = json.NewEncoder(w).Encode(stored)
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/namespaces/isolated/secrets":
			creates++
			if stored != nil {
				w.WriteHeader(409)
				_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonAlreadyExists, Code: 409})
				return
			}
			var received api.Secret
			if json.NewDecoder(r.Body).Decode(&received) != nil {
				forbidden = true
				w.WriteHeader(400)
				return
			}
			stored = received.DeepCopy()
			stored.UID = "secret-http-uid"
			stored.ResourceVersion = "1"
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(500)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonInternalError, Message: "PRIVATE-REJECTED-SECRET-BODY", Code: 500})
		default:
			forbidden = true
			w.WriteHeader(403)
		}
	}))
	defer server.Close()
	c, err := corev1.NewForConfig(&rest.Config{Host: server.URL, Timeout: 5 * time.Second, QPS: 100, Burst: 100, ContentConfig: rest.ContentConfig{ContentType: "application/json"}, WarningHandlerWithContext: &warnings})
	if err != nil {
		t.Fatal(err)
	}
	// Positive control proves the Warning is valid and this client's ordinary
	// typed requests reach the supplied logging handler.
	if _, err := c.ConfigMaps(o.Namespace).Get(context.Background(), o.StateConfigMap, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if warnings.seen.Load() != 1 {
		t.Fatal("invalid Warning fixture; ordinary client did not observe it")
	}
	warnings.seen.Store(0)
	if err := EnsureIdentitySecret(context.Background(), c, o); err != nil {
		t.Fatal("committed result could not be resolved safely", err)
	}
	mu.Lock()
	original := stored.DeepCopy()
	mu.Unlock()
	o.FreshClusterID = ""
	if err := EnsureIdentitySecret(context.Background(), c, o); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 || forbidden || !reflect.DeepEqual(original, stored) {
		t.Fatal("unexpected API access or identity mutation")
	}
	if warnings.seen.Load() != 0 {
		t.Fatal("admission warning escaped into caller's logging handler")
	}
}
