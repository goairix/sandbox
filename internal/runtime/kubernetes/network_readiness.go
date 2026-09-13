package kubernetes

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
)

var ciliumEndpointGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumendpoints"}

// Standard NetworkPolicy has no portable dataplane-ready API. Only Cilium
// advertises this endpoint identity check; other CNIs never query this CRD.
// This confirms the identity, not acknowledgement of every later policy update.
func (r *Runtime) waitNetworkIdentity(ctx context.Context, pod *corev1.Pod) error {
	if !r.hasCilium {
		return nil
	}
	if r.dynClient == nil {
		return fmt.Errorf("cilium endpoint readiness client is unavailable")
	}
	timeout := r.readyTimeout
	if timeout <= 0 {
		timeout = defaultKubernetesControlTimeout
	}
	interval := r.pollInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	err := wait.PollUntilContextTimeout(ctx, interval, timeout, true, func(ctx context.Context) (bool, error) {
		current, err := r.client.CoreV1().Pods(r.namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if current.UID != pod.UID || current.DeletionTimestamp != nil {
			return false, fmt.Errorf("runtime Pod changed during network identity readiness")
		}
		endpoint, err := r.dynClient.Resource(ciliumEndpointGVR).Namespace(r.namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return ciliumEndpointIdentityReady(endpoint, pod), nil
	})
	if err != nil {
		return fmt.Errorf("wait for exact Cilium endpoint identity: %w", err)
	}
	return nil
}

func ciliumEndpointIdentityReady(endpoint *unstructured.Unstructured, pod *corev1.Pod) bool {
	if endpoint == nil || endpoint.GetName() != pod.Name || endpoint.GetNamespace() != pod.Namespace || endpoint.GetDeletionTimestamp() != nil {
		return false
	}
	owned := false
	for _, owner := range endpoint.GetOwnerReferences() {
		if owner.Kind == "Pod" && owner.Name == pod.Name && owner.UID == pod.UID {
			owned = true
		}
	}
	if !owned {
		return false
	}
	state, _, err := unstructured.NestedString(endpoint.Object, "status", "state")
	if err != nil || state != "ready" {
		return false
	}
	labels, _, err := unstructured.NestedStringSlice(endpoint.Object, "status", "identity", "labels")
	if err != nil {
		return false
	}
	for _, key := range []string{"sandbox.id", "sandbox.managed"} {
		found := false
		for _, label := range labels {
			if label == "k8s:"+key+"="+pod.Labels[key] {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
