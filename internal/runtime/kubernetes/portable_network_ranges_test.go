package kubernetes

import (
	"context"
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
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestPortableCiliumRangesOverrideStaleNodeEvidence(t *testing.T) {
	client := kubefake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.5.0/24"}}},
		&networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/16"}}},
	)
	node := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "cilium.io/v2", "kind": "CiliumNode", "metadata": map[string]any{"name": "worker"}, "spec": map[string]any{"ipam": map[string]any{"podCIDRs": []any{"10.0.5.0/24"}}}}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{ciliumNodeGVR: "CiliumNodeList"}, node)
	r := &Runtime{client: client, dynClient: dyn, hasCilium: true}
	_, err := r.resolveNetworkTargets(context.Background(), []string{"10.0.5.51/32"})
	require.ErrorIs(t, err, runtime.ErrInvalidNetworkTarget)
}

func TestPortableCalicoEvidenceErrorsFailClosed(t *testing.T) {
	for _, code := range []string{"empty", "forbidden", "malformed", "pagination"} {
		t.Run(code, func(t *testing.T) {
			client := kubefake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.0.0/16"}}}, &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/16"}}})
			dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{calicoPoolGVR: "IPPoolList"})
			dyn.PrependReactor("list", "ippools", func(ktesting.Action) (bool, k8sruntime.Object, error) {
				if code == "forbidden" {
					return true, nil, apierrors.NewForbidden(calicoPoolGVR.GroupResource(), "", nil)
				}
				list := &unstructured.UnstructuredList{}
				if code == "malformed" {
					list.Items = []unstructured.Unstructured{{Object: map[string]any{"spec": map[string]any{"cidr": "not-a-cidr"}}}}
				}
				if code == "pagination" {
					list.SetContinue("never-ended")
				}
				return true, list, nil
			})
			_, err := (&Runtime{client: client, dynClient: dyn}).resolveNetworkTargets(context.Background(), []string{"8.8.8.8"})
			require.Error(t, err)
			require.LessOrEqual(t, len(dyn.Actions()), networkInventoryMaxPages)
		})
	}
}

func TestPortableStandardNodeIPAMWithoutCalicoResource(t *testing.T) {
	client := kubefake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.0.0/16", "fd00:42::/64"}}}, &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/16", "fd00:96::/112"}}})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{calicoPoolGVR: "IPPoolList"})
	dyn.PrependReactor("list", "ippools", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewNotFound(calicoPoolGVR.GroupResource(), "")
	})
	r := &Runtime{client: client, dynClient: dyn}
	_, err := r.resolveNetworkTargets(context.Background(), []string{"8.8.8.8"})
	require.NoError(t, err)
	_, err = r.resolveNetworkTargets(context.Background(), []string{"fd00:42::10"})
	require.ErrorIs(t, err, runtime.ErrInvalidNetworkTarget)
	for _, action := range dyn.Actions() {
		require.Equal(t, "ippools", action.GetResource().Resource, "standard CNI must never query Cilium resources")
	}
}

func TestPortableUnknownAllocationFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		objects []k8sruntime.Object
	}{
		{"no-nodes", nil},
		{"vpc-node-without-pod-ranges", []k8sruntime.Object{&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}}, &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/16"}}}}},
		{"missing-service-ranges", []k8sruntime.Object{&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.0.0/16"}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := kubefake.NewSimpleClientset(tc.objects...)
			_, err := (&Runtime{client: client}).resolveNetworkTargets(context.Background(), []string{"8.8.8.8"})
			require.Error(t, err, "incomplete evidence must not silently authorize literal destinations")
			for _, action := range client.Actions() {
				require.Contains(t, []string{"get", "list"}, action.GetVerb())
			}
		})
	}
}

func TestPortableConfiguredVPCRangesAvoidDiscovery(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	r := &Runtime{client: client}
	WithNetworkCIDRs([]string{"172.30.0.0/16"}, []string{"10.96.0.0/16"})(r)
	_, err := r.resolveNetworkTargets(context.Background(), []string{"172.30.99.10"})
	require.ErrorIs(t, err, runtime.ErrInvalidNetworkTarget)
	_, err = r.resolveNetworkTargets(context.Background(), []string{"8.8.8.8"})
	require.NoError(t, err)
	require.Empty(t, client.Actions())
}

func TestPortableCalicoPoolRangesIncludeBorrowedAndDisabledAllocations(t *testing.T) {
	client := kubefake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker"}, Spec: corev1.NodeSpec{PodCIDRs: []string{"10.244.0.0/16"}}}, &networkingv1.ServiceCIDR{ObjectMeta: metav1.ObjectMeta{Name: "kubernetes"}, Spec: networkingv1.ServiceCIDRSpec{CIDRs: []string{"10.96.0.0/16"}}})
	gvr := schema.GroupVersionResource{Group: "crd.projectcalico.org", Version: "v1", Resource: "ippools"}
	pool := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "crd.projectcalico.org/v1", "kind": "IPPool", "metadata": map[string]any{"name": "disabled-pool"}, "spec": map[string]any{"cidr": "172.30.0.0/16", "disabled": true}}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{gvr: "IPPoolList"}, pool)
	_, err := resolveNetworkTargets(context.Background(), client, []string{"172.30.99.10"}, networkRangeOptions{dynamic: dyn, requireAuthoritative: true})
	require.ErrorIs(t, err, runtime.ErrInvalidNetworkTarget, "Calico can allocate outside Node PodCIDRs; disabled pools can still contain allocated Pods")
}
