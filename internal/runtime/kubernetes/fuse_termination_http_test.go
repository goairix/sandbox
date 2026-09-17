package kubernetes

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	sandboxruntime "github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type fuseTerminationWarnings struct{ seen atomic.Int32 }

func (w *fuseTerminationWarnings) HandleWarningHeaderWithContext(context.Context, int, string, string) {
	w.seen.Add(1)
}

// Real REST transport proves a committed 500 + Retry-After cannot replay DELETE.
// This does not simulate a CSI driver, kubelet, cluster RBAC or CRI enforcement.
func TestFUSEUnstartedHTTPUncertainDeleteIsNotReplayed(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "not committed"
		if committed {
			name = "committed"
		}
		t.Run(name, func(t *testing.T) {
			rt, _, pod, script := unstartedFUSEFixture(t)
			pod.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}
			var mu sync.Mutex
			var warnings fuseTerminationWarnings
			deletes, invalid := 0, false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path != "/api/v1/namespaces/runtime/pods/"+pod.Name {
					invalid = true
					w.WriteHeader(http.StatusForbidden)
					return
				}
				switch r.Method {
				case http.MethodGet:
					_ = json.NewEncoder(w).Encode(pod)
				case http.MethodDelete:
					deletes++
					var options metav1.DeleteOptions
					if json.NewDecoder(r.Body).Decode(&options) != nil || options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != pod.UID || options.GracePeriodSeconds == nil || *options.GracePeriodSeconds != *pod.Spec.TerminationGracePeriodSeconds {
						invalid = true
					}
					if committed {
						terminalUnstartedFUSE(pod)
					}
					w.Header().Set("Warning", `299 kube "PRIVATE-DELETE-WARNING"`)
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusInternalServerError)
					_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Reason: metav1.StatusReasonInternalError, Message: "PRIVATE-DELETE-BODY", Code: 500})
				default:
					invalid = true
					w.WriteHeader(http.StatusForbidden)
				}
			}))
			defer server.Close()
			client, err := kubeclient.NewForConfig(&rest.Config{Host: server.URL, QPS: 100, Burst: 100,
				ContentConfig: rest.ContentConfig{ContentType: "application/json"}, WarningHandlerWithContext: &warnings})
			require.NoError(t, err)
			rt.client = client
			evidence, err := rt.ConfirmPreparedSandboxTermination(context.Background(), pod.Name, string(pod.UID))
			if committed {
				require.NoError(t, err)
				require.True(t, evidence.ProcessExited)
				require.False(t, evidence.GracefulUnmount)
			} else {
				require.ErrorIs(t, err, sandboxruntime.ErrTerminationUnconfirmed)
			}
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, 1, deletes)
			require.False(t, invalid)
			require.Zero(t, warnings.seen.Load())
			require.Empty(t, script.commands)
			require.Contains(t, pod.Finalizers, fuseRuntimeCleanupFinalizer)
		})
	}
}
