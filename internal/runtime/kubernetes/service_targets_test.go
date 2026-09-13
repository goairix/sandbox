package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestServiceTargetCompilesExactNamespaceAndSelector(t *testing.T) {
	client := fake.NewClientset(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "ledger"}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.1.2", Selector: map[string]string{"app": "ledger", "tier": "backend"}}})
	require.NoError(t, client.Tracker().Add(&networkingv1.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "runtime", Name: "sandbox-sandbox", UID: "policy-uid", Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "sandbox"}}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"sandbox.id": "sandbox"}}}}))
	err := updateOrdinaryNetworkPolicy(context.Background(), client, "runtime", ordinaryNetworkIdentity{runtimeID: "pod", logicalID: "sandbox"}, true, []string{"k8s-service://payments/ledger"}, false)
	require.NoError(t, err)
	policy, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), "sandbox-sandbox", metav1.GetOptions{})
	require.NoError(t, err)
	require.Len(t, policy.Spec.Egress, 2, "service-only whitelist must not become open access")
	require.Nil(t, policy.Spec.Egress[1].To[0].IPBlock)
	require.Equal(t, map[string]string{"kubernetes.io/metadata.name": "payments"}, policy.Spec.Egress[1].To[0].NamespaceSelector.MatchLabels)
	require.Equal(t, map[string]string{"app": "ledger", "tier": "backend"}, policy.Spec.Egress[1].To[0].PodSelector.MatchLabels)
}

func TestNetworkTargetsRejectClusterCIDROverlapBeforeUpdate(t *testing.T) {
	client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.42.0.0/16"}}}, &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/12"}}})
	for _, entry := range []string{"10.42.1.1", "10.0.0.0/8", "10.96.0.0/16"} {
		err := updateOrdinaryNetworkPolicy(context.Background(), client, "runtime", ordinaryNetworkIdentity{runtimeID: "pod", logicalID: "sandbox"}, true, []string{entry}, false)
		require.ErrorIs(t, err, runtime.ErrInvalidNetworkTarget, entry)
	}
	for _, action := range client.Actions() {
		require.Contains(t, []string{"get", "list"}, action.GetVerb())
	}
}

func TestNetworkTargetsRejectUnsupportedServiceTargets(t *testing.T) {
	for _, entry := range []string{"k8s-service://default/", "k8s-service://Default/service", "k8s-service://default/service?all=true", "k8s-service://default/service/extra", "k8s-service://default/selectorless", "k8s-service://default/headless"} {
		client := fake.NewClientset(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "selectorless", Namespace: "default"}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.0.1"}}, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "headless", Namespace: "default"}, Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Selector: map[string]string{"app": "headless"}}})
		err := updateOrdinaryNetworkPolicy(context.Background(), client, "runtime", ordinaryNetworkIdentity{runtimeID: "pod", logicalID: "sandbox"}, true, []string{entry}, false)
		require.ErrorIs(t, err, runtime.ErrInvalidNetworkTarget, entry)
	}
}

func TestNetworkTargetsRejectUnallocatedServiceCIDR(t *testing.T) {
	client := fake.NewClientset(&networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/12", "fd00:96::/112"}}})
	for _, raw := range []string{"10.100.2.3/32", "fd00:96::2/128"} {
		_, err := resolveNetworkTargets(context.Background(), client, []string{raw})
		require.ErrorIs(t, err, runtime.ErrInvalidNetworkTarget)
	}
}

func TestNetworkTargetInventoryFailureDoesNotWritePolicy(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "nodes", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", fmt.Errorf("denied"))
	})
	err := updateOrdinaryNetworkPolicy(context.Background(), client, "runtime", ordinaryNetworkIdentity{runtimeID: "pod", logicalID: "sandbox"}, true, []string{"203.0.113.2"}, false)
	require.Error(t, err)
	for _, action := range client.Actions() {
		require.Contains(t, []string{"get", "list"}, action.GetVerb())
	}
}

func TestOrdinaryCiliumPrivateDenyUsesExplicitCIDRExceptions(t *testing.T) {
	policy, err := buildOrdinaryCiliumPrivateDeny("runtime", ordinaryNetworkIdentity{runtimeID: "pod", logicalID: "sandbox"}, "attempt", []string{"192.168.50.8/32"})
	require.NoError(t, err)
	rules, _, err := unstructured.NestedSlice(policy.Object, "spec", "egressDeny")
	require.NoError(t, err)
	cidrs := rules[0].(map[string]any)["toCIDRSet"].([]any)
	found := false
	for _, raw := range cidrs {
		rule := raw.(map[string]any)
		if rule["cidr"] == "192.168.0.0/16" {
			require.Equal(t, []any{"192.168.50.8/32"}, rule["except"])
			found = true
		}
	}
	require.True(t, found)
}

func TestServiceTargetsDoNotWeakenFUSEIsolationOrSystemContract(t *testing.T) {
	targets, err := resolveNetworkTargets(context.Background(), fake.NewClientset(&corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "payments", Name: "ledger"}, Spec: corev1.ServiceSpec{ClusterIP: "10.96.1.2", Selector: map[string]string{"app": "ledger"}}}), []string{"k8s-service://payments/ledger"})
	require.NoError(t, err)
	policy, err := buildFUSEUserNetworkPolicy("runtime", "instance", "uid", true, nil, false, []string{"10.96.0.10/32"}, targets.peers)
	require.NoError(t, err)
	require.Len(t, policy.Spec.Egress, 2)
	require.NotNil(t, policy.Spec.Egress[1].To[0].PodSelector)
	isolated, err := buildFUSEUserNetworkPolicy("runtime", "instance", "uid", false, nil, true, []string{"10.96.0.10/32"}, targets.peers)
	require.NoError(t, err)
	require.Empty(t, isolated.Spec.Egress)
	deny, err := buildFUSECiliumUserDenyPolicy("runtime", "instance", "uid", true, targets.cidrs)
	require.NoError(t, err)
	encoded, _ := json.Marshal(deny.Object)
	require.NotContains(t, string(encoded), "except", "Service identity must never become a broad private CIDR exception")
}

func TestNetworkTargetsPreserveExternalCIDRAndIPWhitelist(t *testing.T) {
	targets, err := resolveNetworkTargets(context.Background(), fake.NewClientset(), []string{"8.8.8.8", "203.0.113.8/24"})
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"8.8.8.8/32", "203.0.113.0/24"}, targets.cidrs)
	require.Empty(t, targets.peers)
}

func TestNetworkRangesDoNotListWholePodOrServiceInventory(t *testing.T) {
	client := fake.NewClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.42.0.0/16"}}}, &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/12"}}})
	_, err := resolveNetworkTargets(context.Background(), client, []string{"8.8.8.8"})
	require.NoError(t, err)
	for _, action := range client.Actions() {
		require.NotContains(t, []string{"pods", "services"}, action.GetResource().Resource, "authoritative ranges must eliminate full workload inventory")
	}
}

func TestNetworkRangePaginationRejectsUnboundedContinuation(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "nodes", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		return true, &corev1.NodeList{ListMeta: metav1.ListMeta{Continue: "more"}, Items: []corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.42.0.0/16"}}}}}, nil
	})
	_, err := resolveNetworkTargets(context.Background(), client, []string{"8.8.8.8"})
	require.Error(t, err)
	require.LessOrEqual(t, len(client.Actions()), 12)
}
