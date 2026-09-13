package kubernetes

import (
	"context"
	"testing"
	"time"

	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
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

func TestExactOrdinaryRemovalRecoversOnlyBoundUIDPolicies(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{ciliumNetworkPolicyGVR: "CiliumNetworkPolicyList"})
	for _, uid := range []string{"old-uid", "other-uid"} {
		identity := ordinaryNetworkIdentity{runtimeID: "pod-a", runtimeUID: types.UID(uid), logicalID: uid}
		np, err := buildOrdinaryNetworkPolicy("runtime", identity, "attempt", false, nil, false)
		require.NoError(t, err)
		np.UID = types.UID("np-" + uid)
		_, err = client.NetworkingV1().NetworkPolicies("runtime").Create(ctx, np, metav1.CreateOptions{})
		require.NoError(t, err)
		cnp, err := buildOrdinaryCiliumPrivateDeny("runtime", identity, "attempt")
		require.NoError(t, err)
		cnp.SetUID(types.UID("cnp-" + uid))
		_, err = dyn.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Create(ctx, cnp, metav1.CreateOptions{})
		require.NoError(t, err)
		if uid == "old-uid" {
			foreignNP := np.DeepCopy()
			foreignNP.Namespace = "other-release"
			_, err = client.NetworkingV1().NetworkPolicies("other-release").Create(ctx, foreignNP, metav1.CreateOptions{})
			require.NoError(t, err)
			foreignCNP := cnp.DeepCopy()
			foreignCNP.SetNamespace("other-release")
			_, err = dyn.Resource(ciliumNetworkPolicyGVR).Namespace("other-release").Create(ctx, foreignCNP, metav1.CreateOptions{})
			require.NoError(t, err)
		}
	}
	r := &Runtime{client: client, dynClient: dyn, namespace: "runtime", hasCiliumAPI: true}
	require.NoError(t, r.RemoveOrdinarySandbox(ctx, runtime.RuntimeRef{ID: "pod-a", UID: "old-uid"}))
	_, err := client.NetworkingV1().NetworkPolicies("runtime").Get(ctx, "sandbox-old-uid", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
	_, err = dyn.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(ctx, "sandbox-private-deny-old-uid", metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Get(ctx, "sandbox-other-uid", metav1.GetOptions{})
	require.NoError(t, err)
	_, err = dyn.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(ctx, "sandbox-private-deny-other-uid", metav1.GetOptions{})
	require.NoError(t, err)
	_, err = client.NetworkingV1().NetworkPolicies("other-release").Get(ctx, "sandbox-old-uid", metav1.GetOptions{})
	require.NoError(t, err)
	_, err = dyn.Resource(ciliumNetworkPolicyGVR).Namespace("other-release").Get(ctx, "sandbox-private-deny-old-uid", metav1.GetOptions{})
	require.NoError(t, err)
	for _, actions := range [][]ktesting.Action{client.Actions(), dyn.Actions()} {
		for _, action := range actions {
			if action.GetVerb() != "delete" {
				continue
			}
			deletion := action.(ktesting.DeleteAction)
			require.Equal(t, "runtime", action.GetNamespace())
			require.NotNil(t, deletion.GetDeleteOptions().Preconditions)
			require.NotNil(t, deletion.GetDeleteOptions().Preconditions.UID)
			require.Contains(t, string(*deletion.GetDeleteOptions().Preconditions.UID), "old-uid")
		}
	}
}

func TestExactOrdinaryRemovalRequiresPolicyDeletionReadback(t *testing.T) {
	identity := ordinaryNetworkIdentity{runtimeID: "pod-a", runtimeUID: "old-uid", logicalID: "sandbox-a"}
	np, err := buildOrdinaryNetworkPolicy("runtime", identity, "attempt", false, nil, false)
	require.NoError(t, err)
	np.UID = "np-uid"
	client := fake.NewSimpleClientset(np)
	client.PrependReactor("delete", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) { return true, nil, nil })
	r := &Runtime{client: client, namespace: "runtime"}
	require.ErrorContains(t, r.RemoveOrdinarySandbox(context.Background(), runtime.RuntimeRef{ID: "pod-a", UID: "old-uid"}), "deletion is unconfirmed")
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), np.Name, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestExactOrdinaryRemovalRetainsEvidenceForMalformedTargetPolicy(t *testing.T) {
	for _, malformedCilium := range []bool{false, true} {
		t.Run(map[bool]string{false: "networkpolicy", true: "ciliumpolicy"}[malformedCilium], func(t *testing.T) {
			ctx := context.Background()
			identity := ordinaryNetworkIdentity{runtimeID: "pod-a", runtimeUID: "old-uid", logicalID: "sandbox-a"}
			np, err := buildOrdinaryNetworkPolicy("runtime", identity, "attempt", false, nil, false)
			require.NoError(t, err)
			np.UID = "np-uid"
			cnp, err := buildOrdinaryCiliumPrivateDeny("runtime", identity, "attempt")
			require.NoError(t, err)
			cnp.SetUID("cnp-uid")
			if malformedCilium {
				labels := cnp.GetLabels()
				labels[ordinaryPolicyRoleLabel] = "foreign"
				cnp.SetLabels(labels)
			} else {
				np.Spec.PodSelector.MatchLabels = map[string]string{"sandbox.id": "other-sandbox"}
			}
			client := fake.NewSimpleClientset(np)
			dyn := dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme(), cnp)
			r := &Runtime{client: client, dynClient: dyn, namespace: "runtime", hasCiliumAPI: true}
			require.ErrorIs(t, r.RemoveOrdinarySandbox(ctx, runtime.RuntimeRef{ID: "pod-a", UID: "old-uid"}), runtime.ErrNetworkStateUncertain, "a successful sibling deletion must not hide malformed target evidence")
			if malformedCilium {
				_, err = dyn.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(ctx, cnp.GetName(), metav1.GetOptions{})
			} else {
				_, err = client.NetworkingV1().NetworkPolicies("runtime").Get(ctx, np.Name, metav1.GetOptions{})
			}
			require.NoError(t, err)
		})
	}
}

func TestExactOrdinaryRemovalDoesNotIgnoreCiliumPermissionFailure(t *testing.T) {
	ctx := context.Background()
	identity := ordinaryNetworkIdentity{runtimeID: "pod-a", runtimeUID: "old-uid", logicalID: "sandbox-a"}
	cnp, err := buildOrdinaryCiliumPrivateDeny("runtime", identity, "attempt")
	require.NoError(t, err)
	cnp.SetUID("cnp-uid")
	dyn := dynamicfake.NewSimpleDynamicClient(k8sruntime.NewScheme(), cnp)
	dyn.PrependReactor("delete", "ciliumnetworkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "cilium.io", Resource: "ciliumnetworkpolicies"}, cnp.GetName(), nil)
	})
	r := &Runtime{client: fake.NewSimpleClientset(), dynClient: dyn, namespace: "runtime", hasCiliumAPI: true}
	err = r.RemoveOrdinarySandbox(ctx, runtime.RuntimeRef{ID: "pod-a", UID: "old-uid"})
	require.True(t, apierrors.IsForbidden(err), "%v", err)
	_, err = dyn.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(ctx, cnp.GetName(), metav1.GetOptions{})
	require.NoError(t, err)
}

func TestExactOrdinaryRemovalRetainsPolicyOnDeleteFailureOrPodReappearance(t *testing.T) {
	for _, reappears := range []bool{false, true} {
		t.Run(map[bool]string{false: "forbidden", true: "replacement"}[reappears], func(t *testing.T) {
			ctx := context.Background()
			identity := ordinaryNetworkIdentity{runtimeID: "pod-a", runtimeUID: "old-uid", logicalID: "sandbox-a"}
			np, err := buildOrdinaryNetworkPolicy("runtime", identity, "attempt", false, nil, false)
			require.NoError(t, err)
			np.UID = "np-uid"
			client := fake.NewSimpleClientset(np)
			if reappears {
				gets := 0
				client.PrependReactor("get", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
					gets++
					if gets == 1 {
						return false, nil, nil
					}
					return true, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", UID: "new-uid"}}, nil
				})
			} else {
				client.PrependReactor("delete", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "networking.k8s.io", Resource: "networkpolicies"}, np.Name, nil)
				})
			}
			r := &Runtime{client: client, namespace: "runtime"}
			err = r.RemoveOrdinarySandbox(ctx, runtime.RuntimeRef{ID: "pod-a", UID: "old-uid"})
			if reappears {
				require.ErrorIs(t, err, runtime.ErrNetworkStateUncertain)
			} else {
				require.True(t, apierrors.IsForbidden(err), "%v", err)
			}
			_, err = client.NetworkingV1().NetworkPolicies("runtime").Get(ctx, np.Name, metav1.GetOptions{})
			require.NoError(t, err)
		})
	}
}

func TestExactOrdinaryRemovalAllowsMissingOptionalCiliumResource(t *testing.T) {
	identity := ordinaryNetworkIdentity{runtimeID: "pod-a", runtimeUID: "old-uid", logicalID: "sandbox-a"}
	np, err := buildOrdinaryNetworkPolicy("runtime", identity, "attempt", false, nil, false)
	require.NoError(t, err)
	np.UID = "np-uid"
	client := fake.NewSimpleClientset(np)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{ciliumNetworkPolicyGVR: "CiliumNetworkPolicyList"})
	dyn.PrependReactor("list", "ciliumnetworkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: "cilium.io", Resource: "ciliumnetworkpolicies"}, "")
	})
	r := &Runtime{client: client, dynClient: dyn, namespace: "runtime", hasCiliumAPI: true}
	require.NoError(t, r.RemoveOrdinarySandbox(context.Background(), runtime.RuntimeRef{ID: "pod-a", UID: "old-uid"}))
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), np.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
}

func TestOrdinaryDeletionTimeoutPreservesPendingClassification(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", UID: "old-uid"}}
	client := fake.NewSimpleClientset(pod)
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) { return true, nil, nil })
	err := deleteExactOrdinaryPod(context.Background(), client, "", pod, time.Millisecond, 5*time.Millisecond)
	require.ErrorIs(t, err, runtime.ErrTerminationUnconfirmed)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestOrdinaryDeletionTimeoutDoesNotHidePermissionFailure(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", UID: "old-uid"}}
	client := fake.NewSimpleClientset(pod)
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, pod.Name, nil)
	})
	err := deleteExactOrdinaryPod(context.Background(), client, "", pod, time.Millisecond, 5*time.Millisecond)
	require.True(t, apierrors.IsForbidden(err), "%v", err)
	require.NotErrorIs(t, err, runtime.ErrTerminationUnconfirmed)
}

func TestOrdinaryDeletionTimeoutClassifiesInterruptedAPICalls(t *testing.T) {
	for _, verb := range []string{"delete", "get"} {
		for _, cause := range []error{context.DeadlineExceeded, context.Canceled} {
			t.Run(verb+"/"+cause.Error(), func(t *testing.T) {
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod-a", UID: "old-uid"}}
				client := fake.NewSimpleClientset(pod)
				client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) { return true, nil, nil })
				client.PrependReactor(verb, "pods", func(action ktesting.Action) (bool, k8sruntime.Object, error) { return true, nil, cause })
				err := deleteExactOrdinaryPod(context.Background(), client, "", pod, time.Millisecond, 5*time.Millisecond)
				require.ErrorIs(t, err, runtime.ErrTerminationUnconfirmed)
				require.ErrorIs(t, err, cause)
			})
		}
	}
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
