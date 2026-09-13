package kubernetes

import (
	"context"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestOrdinaryCleanupRecoversPolicyAfterPodDeletion(t *testing.T) {
	identity := ordinaryNetworkIdentity{runtimeID: "pod-a", runtimeUID: types.UID("old-uid"), logicalID: "sandbox-a"}
	policy, err := buildOrdinaryNetworkPolicy("runtime", identity, "attempt", false, nil, false)
	require.NoError(t, err)
	policy.UID = "policy-uid"
	client := fake.NewSimpleClientset(policy)
	r := &Runtime{client: client, namespace: "runtime"}
	require.NoError(t, r.CleanupOrdinarySandboxPolicies(context.Background(), runtime.RuntimeRef{ID: "pod-a", UID: "old-uid"}, "sandbox-a"))
	policies, err := client.NetworkingV1().NetworkPolicies("runtime").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	require.Empty(t, policies.Items)
}

func TestOrdinaryCleanupRetainsForeignPolicyAndReplacementPod(t *testing.T) {
	identity := ordinaryNetworkIdentity{runtimeID: "pod-a", runtimeUID: "new-uid", logicalID: "sandbox-a"}
	policy, err := buildOrdinaryNetworkPolicy("runtime", identity, "attempt", false, nil, false)
	require.NoError(t, err)
	policy.UID = "policy-uid"
	for _, replacement := range []bool{false, true} {
		client := fake.NewSimpleClientset(policy.DeepCopy())
		if replacement {
			_, err := client.CoreV1().Pods("runtime").Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", UID: "new-uid"}}, metav1.CreateOptions{})
			require.NoError(t, err)
		}
		r := &Runtime{client: client, namespace: "runtime"}
		require.Error(t, r.CleanupOrdinarySandboxPolicies(context.Background(), runtime.RuntimeRef{ID: "pod-a", UID: "old-uid"}, "sandbox-a"))
		_, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), policy.Name, metav1.GetOptions{})
		require.NoError(t, err)
	}
}
