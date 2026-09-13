package kubernetes

import (
	"context"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestOrdinaryNetworkUpdateSharesOneAttemptAcrossPolicies(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	r.hasCilium = true
	pod := seedOrdinaryPodWithLogicalPolicy(t, r, client, "runtime-a", "customer-a")
	require.NoError(t, r.UpdateNetwork(context.Background(), pod.Name, true, nil, true))
	allow, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), "sandbox-customer-a", metav1.GetOptions{})
	require.NoError(t, err)
	deny, err := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), "sandbox-private-deny-customer-a", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, allow.Annotations[ordinaryPolicyAttemptAnnotation])
	require.Equal(t, allow.Annotations[ordinaryPolicyAttemptAnnotation], deny.GetAnnotations()[ordinaryPolicyAttemptAnnotation])
	require.Equal(t, string(pod.UID), allow.Annotations[ordinaryRuntimeUIDAnnotation])
	require.Equal(t, string(pod.UID), deny.GetAnnotations()[ordinaryRuntimeUIDAnnotation])
}

func TestOrdinaryNetworkUpdateJointReadbackRejectsLaterAllowChange(t *testing.T) {
	for _, replaceUID := range []bool{false, true} {
		t.Run(map[bool]string{false: "attempt changed", true: "policy UID replaced"}[replaceUID], func(t *testing.T) {
			r, client := newFakeKubernetesRuntime(t, preparedScript())
			r.hasCilium = true
			pod := seedOrdinaryPodWithLogicalPolicy(t, r, client, "runtime-a", "customer-a")
			r.dynClient.(*fake.FakeDynamicClient).PrependReactor("create", "ciliumnetworkpolicies", func(ktesting.Action) (bool, k8sruntime.Object, error) {
				obj, err := client.Tracker().Get(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), "runtime", "sandbox-customer-a")
				require.NoError(t, err)
				policy := obj.(*networkingv1.NetworkPolicy).DeepCopy()
				if replaceUID {
					policy.UID = "replacement-policy-uid"
				} else {
					policy.Annotations[ordinaryPolicyAttemptAnnotation] = "competing-update"
				}
				require.NoError(t, client.Tracker().Update(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), policy, "runtime"))
				return false, nil, nil
			})
			require.ErrorIs(t, r.UpdateNetwork(context.Background(), pod.Name, true, nil, true), runtime.ErrNetworkStateUncertain)
		})
	}
}

func TestCiliumNetworkTargetUnavailableServiceCIDRRejectsBeforeWrites(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	r.hasCilium = true
	r.networkPodCIDRs, r.networkServiceCIDRs = nil, nil
	pod := seedOrdinaryPodWithLogicalPolicy(t, r, client, "runtime-a", "customer-a")
	_, err := client.CoreV1().Nodes().Create(context.Background(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.42.0.0/16"}}}, metav1.CreateOptions{})
	require.NoError(t, err)
	client.PrependReactor("list", "servicecidrs", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "networking.k8s.io", Resource: "servicecidrs"}, "")
	})
	client.ClearActions()
	require.Error(t, r.UpdateNetwork(context.Background(), pod.Name, true, []string{"8.8.8.8"}, false))
	for _, action := range client.Actions() {
		require.Contains(t, []string{"get", "list"}, action.GetVerb())
	}
}

func TestOrdinaryNetworkCreationJointReadbackRejectsAllowChangedDuringDenyBind(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	r.hasCilium = true
	r.readyTimeout = time.Second
	r.dynClient.(*fake.FakeDynamicClient).PrependReactor("update", "ciliumnetworkpolicies", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		obj, err := client.Tracker().Get(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), "runtime", "sandbox-sandbox-a")
		require.NoError(t, err)
		policy := obj.(*networkingv1.NetworkPolicy).DeepCopy()
		policy.Annotations[ordinaryPolicyAttemptAnnotation] = "competing-update"
		require.NoError(t, client.Tracker().Update(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), policy, "runtime"))
		return false, nil, nil
	})
	_, err := r.CreateSandbox(context.Background(), runtime.SandboxSpec{ID: "sandbox-a", Image: "sandbox:latest", NetworkEnabled: true})
	require.ErrorIs(t, err, runtime.ErrNetworkStateUncertain)
}
