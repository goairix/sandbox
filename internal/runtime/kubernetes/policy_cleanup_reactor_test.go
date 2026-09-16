package kubernetes

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

// Exercise the production cleanup proof with the same concurrent-first-create
// store used by the ownership tests: a successful delete must really remove it.
func TestConcurrentPolicyStoreSupportsExactAttemptCleanup(t *testing.T) {
	t.Run("standard", func(t *testing.T) {
		store := newSharedFirstNetworkPolicyStore("test-policy")
		// Verification after deletion must not wait for a concurrent initial GET.
		close(store.release)
		store.policy = &networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{
			Name: store.name, UID: types.UID("test-uid"), ResourceVersion: "1",
			Annotations: map[string]string{fuseNetworkAttemptAnnotation: "test-attempt"},
		}}
		client := kubefake.NewSimpleClientset()
		client.PrependReactor("*", "networkpolicies", store.react)
		require.NoError(t, deleteNetworkPolicyForNetworkAttempt(context.Background(), client.NetworkingV1().NetworkPolicies("test"), store.name, "test-attempt"))
		policy, deletes := store.snapshot()
		require.Nil(t, policy)
		require.Equal(t, 1, deletes)
	})
	t.Run("cilium", func(t *testing.T) {
		store := newSharedFirstCiliumPolicyStore("test-policy")
		close(store.release)
		store.policy = &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "cilium.io/v2", "kind": "CiliumNetworkPolicy",
			"metadata": map[string]interface{}{
				"name": store.name, "uid": "test-uid", "resourceVersion": "1",
				"annotations": map[string]interface{}{fuseNetworkAttemptAnnotation: "test-attempt"},
			},
		}}
		client := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
		client.PrependReactor("*", "ciliumnetworkpolicies", store.react)
		require.NoError(t, deleteCiliumPolicyForNetworkAttempt(context.Background(), client.Resource(ciliumNetworkPolicyGVR).Namespace("test"), store.name, "test-attempt"))
		policy, deletes := store.snapshot()
		require.Nil(t, policy)
		require.Equal(t, 1, deletes)
	})
}
