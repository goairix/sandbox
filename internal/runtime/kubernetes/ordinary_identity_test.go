package kubernetes

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestPortableOrdinaryClaimKeepsCNILabelsAndPolicyIdentity(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	before := seedOrdinaryPoolPodAndPolicy(t, r, client, "sandbox-pool-stable", "sandbox-pool-stable")
	id := "sandbox-business-owner"
	require.NoError(t, r.UpdateLabels(context.Background(), before.Name, map[string]*string{"sandbox.pool": nil, "sandbox.id": &id}))
	after, err := client.CoreV1().Pods("runtime").Get(context.Background(), before.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, before.Labels, after.Labels, "CNI security identity must remain stable during checkout")
	require.Equal(t, id, after.Annotations["sandbox.claim.id"])
	policy, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), "sandbox-sandbox-pool-stable", metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"sandbox.id": "sandbox-pool-stable"}, policy.Spec.PodSelector.MatchLabels)
	pool, err := r.ListSandboxes(context.Background(), map[string]string{"sandbox.pool": "true"})
	require.NoError(t, err)
	require.Empty(t, pool, "claimed Pod must not be reclaimed despite its stable physical pool labels")
	business, err := r.ListSandboxes(context.Background(), map[string]string{"sandbox.id": id})
	require.NoError(t, err)
	require.Len(t, business, 1)
	require.Equal(t, id, business[0].ID)
	require.NotContains(t, business[0].Labels, "sandbox.pool")
}

func TestPortableStandardProviderAllowsMissingCiliumPolicyResource(t *testing.T) {
	r, _ := newFakeKubernetesRuntime(t, preparedScript())
	r.hasCilium = true
	WithNetworkPolicyProvider("standard")(r)
	require.NoError(t, r.configureNetworkPolicyProvider())
	r.dynClient.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "ciliumnetworkpolicies", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewNotFound(ciliumNetworkPolicyGVR.GroupResource(), "")
	})
	require.NoError(t, r.initializeOrdinaryPolicyRecovery(), "obsolete API group must not require a missing policy resource")
}

func TestPortableStandardProviderStillCleansOwnedLegacyCiliumPolicy(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	r.hasCilium = true
	pod := seedOrdinaryPodWithLogicalPolicy(t, r, client, "runtime-legacy", "sandbox-legacy")
	policy, err := buildOrdinaryCiliumPrivateDeny("runtime", ordinaryNetworkIdentity{runtimeID: pod.Name, runtimeUID: pod.UID, logicalID: "sandbox-legacy"}, "old-attempt")
	require.NoError(t, err)
	_, err = r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Create(context.Background(), policy, metav1.CreateOptions{})
	require.NoError(t, err)
	WithNetworkPolicyProvider("standard")(r)
	require.NoError(t, r.configureNetworkPolicyProvider())
	require.NoError(t, r.RemoveSandbox(context.Background(), pod.Name))
	_, err = r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), policy.GetName(), metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "provider selection must not hide owned legacy policies from cleanup")
}

func TestPortableStandardProviderIgnoresLeftoverCiliumAPI(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	pod := seedOrdinaryPoolPodAndPolicy(t, r, client, "sandbox-pool-standard", "sandbox-pool-standard")
	pod.Labels["sandbox.pool.state"], pod.Labels["sandbox.pool.key"], pod.Labels["sandbox.pool.instance"] = "preparing", "key", "instance"
	pod, err := client.CoreV1().Pods("runtime").Update(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	r.hasCilium = true // discovery sees obsolete APIs, but no Cilium agent exists.
	WithNetworkPolicyProvider("standard")(r)
	require.NoError(t, r.configureNetworkPolicyProvider())
	require.NoError(t, r.PublishOrdinaryPoolPrepared(context.Background(), runtime.RuntimeRef{ID: pod.Name, UID: string(pod.UID)}, "key", "instance"))
	for _, action := range r.dynClient.(*dynamicfake.FakeDynamicClient).Actions() {
		require.NotEqual(t, "cilium.io", action.GetResource().Group)
	}
	require.Contains(t, r.WarmPoolContract(), "cilium=false")
}

func TestPortableOrdinaryClaimUsesAcquiredRuntimeUID(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	pod := seedOrdinaryPoolPodAndPolicy(t, r, client, "sandbox-pool-replacement", "sandbox-pool-replacement")
	claimer, ok := any(r).(interface {
		ClaimOrdinaryPool(context.Context, runtime.RuntimeRef, string) error
	})
	require.True(t, ok, "ordinary pool claim must accept the original acquired RuntimeRef")
	err := claimer.ClaimOrdinaryPool(context.Background(), runtime.RuntimeRef{ID: pod.Name, UID: "previous-uid"}, "sandbox-customer")
	require.ErrorIs(t, err, runtime.ErrInvalidRuntimeRef)
	current, err := client.CoreV1().Pods("runtime").Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, current.Annotations[ordinaryClaimIDAnnotation])
}

func TestPortableCiliumPreparedPublicationRefreshesStatusResourceVersion(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	pod := seedOrdinaryPoolPodAndPolicy(t, r, client, "sandbox-pool-rv", "sandbox-pool-rv")
	pod.Labels["sandbox.pool.state"], pod.Labels["sandbox.pool.key"], pod.Labels["sandbox.pool.instance"] = "preparing", "key", "instance"
	pod, err := client.CoreV1().Pods("runtime").Update(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	r.hasCilium = true
	r.dynClient.(*dynamicfake.FakeDynamicClient).PrependReactor("get", "ciliumendpoints", func(ktesting.Action) (bool, k8sruntime.Object, error) {
		current := pod.DeepCopy()
		current.ResourceVersion = "network-ready-rv"
		require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), current, "runtime"))
		ep := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"state": "ready", "identity": map[string]any{"labels": []any{"k8s:sandbox.id=" + pod.Labels["sandbox.id"], "k8s:sandbox.managed=true"}}}}}
		ep.SetName(pod.Name)
		ep.SetNamespace(pod.Namespace)
		ep.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Pod", Name: pod.Name, UID: pod.UID}})
		return true, ep, nil
	})
	client.PrependReactor("patch", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		var patch struct {
			Metadata struct {
				ResourceVersion string `json:"resourceVersion"`
			} `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal(action.(ktesting.PatchAction).GetPatch(), &patch))
		if patch.Metadata.ResourceVersion != "network-ready-rv" {
			return true, nil, apierrors.NewConflict(corev1.Resource("pods"), pod.Name, nil)
		}
		return false, nil, nil
	})
	require.NoError(t, r.PublishOrdinaryPoolPrepared(context.Background(), runtime.RuntimeRef{ID: pod.Name, UID: string(pod.UID)}, "key", "instance"))
}

func TestPortablePreparedPublicationRejectsStaleUIDAnnotation(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	pod := seedOrdinaryPoolPodAndPolicy(t, r, client, "sandbox-pool-stale", "sandbox-pool-stale")
	pod.Labels["sandbox.pool.state"], pod.Labels["sandbox.pool.key"], pod.Labels["sandbox.pool.instance"] = "preparing", "key", "instance"
	pod.Annotations = map[string]string{ordinaryPreparedStateAnnotation: "prepared", ordinaryPreparedUIDAnnotation: "stale-uid"}
	pod, err := client.CoreV1().Pods("runtime").Update(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Error(t, r.PublishOrdinaryPoolPrepared(context.Background(), runtime.RuntimeRef{ID: pod.Name, UID: string(pod.UID)}, "key", "instance"))
}

func TestPortableCiliumEndpointRequiresExactOwnerAndStableIdentity(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "runtime", UID: "uid", Labels: map[string]string{"sandbox.id": "pool", "sandbox.managed": "true"}}}
	ep := &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"state": "ready", "identity": map[string]any{"labels": []any{"k8s:sandbox.id=pool", "k8s:sandbox.managed=true"}}}}}
	ep.SetName(pod.Name)
	ep.SetNamespace(pod.Namespace)
	ep.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Pod", Name: pod.Name, UID: pod.UID}})
	require.True(t, ciliumEndpointIdentityReady(ep, pod))
	ep.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Pod", Name: pod.Name, UID: types.UID("replacement")}})
	require.False(t, ciliumEndpointIdentityReady(ep, pod))
	ep.SetOwnerReferences([]metav1.OwnerReference{{Kind: "Pod", Name: pod.Name, UID: pod.UID}})
	require.NoError(t, unstructured.SetNestedSlice(ep.Object, []any{"k8s:sandbox.id=business", "k8s:sandbox.managed=true"}, "status", "identity", "labels"))
	require.False(t, ciliumEndpointIdentityReady(ep, pod))
}

func TestPortableOrdinaryInventoryKeepsStableServerFilters(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	_, err := r.ListSandboxes(context.Background(), map[string]string{"sandbox.pool": "true", "sandbox.pool.key": "key", "sandbox.pool.instance": "instance"})
	require.NoError(t, err)
	for _, action := range client.Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
			selector := action.(ktesting.ListAction).GetListRestrictions().Labels.String()
			require.Contains(t, selector, "sandbox.pool.key=key")
			require.Contains(t, selector, "sandbox.pool.instance=instance")
			require.Contains(t, selector, "sandbox.pool=true")
		}
	}
}

func TestPortableOrdinaryPreparedPublicationKeepsCNILabels(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	before := seedOrdinaryPoolPodAndPolicy(t, r, client, "sandbox-pool-prepared", "sandbox-pool-prepared")
	before.Labels["sandbox.pool.state"] = "preparing"
	before.Labels["sandbox.pool.key"] = "key"
	before.Labels["sandbox.pool.instance"] = "instance"
	before, err := client.CoreV1().Pods("runtime").Update(context.Background(), before, metav1.UpdateOptions{})
	require.NoError(t, err)
	ref := runtime.RuntimeRef{ID: before.Name, UID: string(before.UID)}
	require.NoError(t, r.PublishOrdinaryPoolPrepared(context.Background(), ref, "key", "instance"))
	after, err := client.CoreV1().Pods("runtime").Get(context.Background(), before.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, before.Labels, after.Labels)
	infos, err := r.ListSandboxes(context.Background(), map[string]string{"sandbox.pool.state": "prepared"})
	require.NoError(t, err)
	require.Len(t, infos, 1)
}

func TestPortableCiliumPoolCannotPublishBeforeEndpointIdentityReady(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	before := seedOrdinaryPoolPodAndPolicy(t, r, client, "sandbox-pool-endpoint", "sandbox-pool-endpoint")
	before.Labels["sandbox.pool.state"] = "preparing"
	before.Labels["sandbox.pool.key"] = "key"
	before.Labels["sandbox.pool.instance"] = "instance"
	before, err := client.CoreV1().Pods("runtime").Update(context.Background(), before, metav1.UpdateOptions{})
	require.NoError(t, err)
	r.hasCilium = true
	require.Error(t, r.PublishOrdinaryPoolPrepared(context.Background(), runtime.RuntimeRef{ID: before.Name, UID: string(before.UID)}, "key", "instance"), "Pod Ready alone is not evidence of Cilium endpoint identity readiness")
}
