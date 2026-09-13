package kubernetes

import (
	"context"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"testing"
)

func TestCiliumAllocatedRangesCoverEveryNodeAndFutureConfiguredAllocation(t *testing.T) {
	client := kubefake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.42.0.0/24"}}}, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-b"}}, &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/12"}}})
	ciliumNode := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cilium.io/v2", "kind": "CiliumNode", "metadata": map[string]any{"name": "node-b"}, "spec": map[string]any{"ipam": map[string]any{"podCIDRs": []any{"10.43.0.0/24"}}}}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{ciliumNodeGVR: "CiliumNodeList"}, ciliumNode)
	r := &Runtime{client: client, dynClient: dyn, hasCilium: true}
	for _, raw := range []string{"10.42.0.8", "10.43.0.8", "10.100.0.8"} {
		_, err := r.resolveNetworkTargets(context.Background(), []string{raw})
		require.ErrorIs(t, err, runtime.ErrInvalidNetworkTarget, raw)
	}
	client.ClearActions()
	WithNetworkCIDRs([]string{"10.0.0.0/8"}, []string{"172.20.0.0/16"})(r)
	_, err := r.resolveNetworkTargets(context.Background(), []string{"10.250.0.8"})
	require.ErrorIs(t, err, runtime.ErrInvalidNetworkTarget, "operator-declared future allocation must already be excluded")
	_, err = r.resolveNetworkTargets(context.Background(), []string{"8.8.8.8"})
	require.NoError(t, err)
	require.Empty(t, client.Actions(), "configured complete ranges require no allocation inventory lists")
}

func TestCiliumMissingNodeCIDRIsFailClosedEvenWithLivePodIPs(t *testing.T) {
	client := kubefake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}}, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "default"}, Status: corev1.PodStatus{PodIP: "10.42.1.2"}}, &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/12"}}})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{ciliumNodeGVR: "CiliumNodeList"})
	_, err := (&Runtime{client: client, dynClient: dyn, hasCilium: true}).resolveNetworkTargets(context.Background(), []string{"8.8.8.8"})
	require.ErrorContains(t, err, "missing")
	for _, action := range client.Actions() {
		require.NotEqual(t, "pods", action.GetResource().Resource)
	}
}

func TestConfiguredRangeFastPathHonorsCanceledRequest(t *testing.T) {
	r := &Runtime{client: kubefake.NewSimpleClientset(), hasCilium: true}
	WithNetworkCIDRs([]string{"10.42.0.0/16"}, []string{"10.96.0.0/12"})(r)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.resolveNetworkTargets(ctx, []string{"8.8.8.8"})
	require.ErrorIs(t, err, context.Canceled)
}

func TestNetworkTargetCountIsBoundedBeforeAnyInventoryRead(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	entries := make([]string, 257)
	for i := range entries {
		entries[i] = "8.8.8.8"
	}
	_, err := resolveNetworkTargets(context.Background(), client, entries)
	require.ErrorIs(t, err, runtime.ErrInvalidNetworkTarget)
	require.Empty(t, client.Actions())
}

func TestCiliumIncompleteDualStackAllocationEvidenceFailsClosed(t *testing.T) {
	client := kubefake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.42.0.0/16"}}}, &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/12", "fd00:96::/112"}}})
	_, err := (&Runtime{client: client, hasCilium: true}).resolveNetworkTargets(context.Background(), []string{"fd00:42::8"})
	require.Error(t, err, "IPv6 Service allocation is known but this Node's IPv6 Pod allocation is not")
}

func TestConfiguredNetworkRangesRejectIncompleteAddressFamilies(t *testing.T) {
	for _, sets := range []struct{ pods, services []string }{
		{[]string{"10.42.0.0/16"}, []string{"10.96.0.0/12", "fd00:96::/112"}},
		{[]string{"10.42.0.0/16", "fd00:42::/64"}, []string{"10.96.0.0/12"}},
	} {
		client := kubefake.NewSimpleClientset()
		r := &Runtime{client: client, hasCilium: true}
		WithNetworkCIDRs(sets.pods, sets.services)(r)
		_, err := r.resolveNetworkTargets(context.Background(), []string{"fd00:42::8"})
		require.Error(t, err, "configured authoritative ranges must cover matching Pod and Service address families")
		require.Empty(t, client.Actions(), "invalid configuration must not fall back to incomplete inventory")
	}
}
