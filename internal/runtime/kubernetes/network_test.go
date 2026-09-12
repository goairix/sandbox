package kubernetes

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	errors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/goairix/sandbox/internal/runtime"
)

func TestBuildSystemEgressPolicyUsesImmutableInstanceSelectorAndExactRules(t *testing.T) {
	policy, err := buildSystemEgressPolicy("runtime", "instance-a", runtime.SystemEgressSpec{
		Mode:          runtime.SystemEgressCIDR,
		DNSCIDRs:      []string{"1.1.1.1/32"},
		DNSPorts:      []int32{53, 53},
		EndpointCIDRs: []string{"192.0.2.10/32"},
		EndpointPorts: []int32{443, 443},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"sandbox.pool.instance": "instance-a"}, policy.Spec.PodSelector.MatchLabels)
	assert.NotContains(t, policy.Spec.PodSelector.MatchLabels, "sandbox.pool.state")
	require.Len(t, policy.Spec.Egress, 2)
	require.Len(t, policy.Spec.Egress[0].To, 1)
	assert.Equal(t, "1.1.1.1/32", policy.Spec.Egress[0].To[0].IPBlock.CIDR)
	require.Len(t, policy.Spec.Egress[0].Ports, 2)
	require.Len(t, policy.Spec.Egress[1].To, 1)
	assert.Equal(t, "192.0.2.10/32", policy.Spec.Egress[1].To[0].IPBlock.CIDR)
	require.Len(t, policy.Spec.Egress[1].Ports, 1)
}

func TestOrdinaryLogicalIDUsesExplicitSandboxLabel(t *testing.T) {
	logicalID, err := ordinaryLogicalID(runtime.SandboxSpec{
		ID:     "sandbox-pool-a",
		Labels: map[string]string{"sandbox.id": "customer-a"},
	})
	require.NoError(t, err)
	assert.Equal(t, "customer-a", logicalID)
}

func TestOrdinaryLogicalIDFallsBackToRuntimeID(t *testing.T) {
	logicalID, err := ordinaryLogicalID(runtime.SandboxSpec{ID: "sandbox-a"})
	require.NoError(t, err)
	assert.Equal(t, "sandbox-a", logicalID)
}

func TestOrdinaryLogicalIDRejectsInvalidOrFUSEIdentity(t *testing.T) {
	_, err := ordinaryLogicalID(runtime.SandboxSpec{ID: "invalid/id"})
	require.Error(t, err)

	_, err = ordinaryIdentityFromPod(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "runtime-a",
		Labels: map[string]string{
			"sandbox.managed":        "true",
			"sandbox.id":             "sandbox-a",
			"sandbox.workspace.mode": "fuse",
		},
	}}, "runtime-a")
	require.Error(t, err)
}

func TestBuildOrdinaryNetworkPolicyCarriesExactIdentity(t *testing.T) {
	identity := ordinaryNetworkIdentity{runtimeID: "sandbox-pool-a", logicalID: "customer-a"}
	policy, err := buildOrdinaryNetworkPolicy("runtime", identity, "attempt-a", false, nil, false)
	require.NoError(t, err)
	assert.Equal(t, "sandbox-customer-a", policy.Name)
	assert.Equal(t, map[string]string{"sandbox.id": "customer-a"}, policy.Spec.PodSelector.MatchLabels)
	assert.Equal(t, "true", policy.Labels["sandbox.managed"])
	assert.Equal(t, "customer-a", policy.Labels["sandbox.id"])
	assert.Equal(t, ordinaryPolicyRole, policy.Labels[ordinaryPolicyRoleLabel])
	assert.Equal(t, "sandbox-pool-a", policy.Labels[ordinaryRuntimeIDLabel])
	assert.Equal(t, "attempt-a", policy.Annotations[ordinaryPolicyAttemptAnnotation])
	assert.NotContains(t, policy.Annotations, ordinaryRuntimeUIDAnnotation)
}

func TestBuildOrdinaryCiliumPrivateDenyCarriesExactIdentity(t *testing.T) {
	identity := ordinaryNetworkIdentity{runtimeID: "sandbox-pool-a", logicalID: "customer-a"}
	policy, err := buildOrdinaryCiliumPrivateDeny("runtime", identity, "attempt-a")
	require.NoError(t, err)
	assert.Equal(t, "sandbox-private-deny-customer-a", policy.GetName())
	assert.Equal(t, "runtime", policy.GetNamespace())
	assert.Equal(t, "true", policy.GetLabels()["sandbox.managed"])
	assert.Equal(t, "customer-a", policy.GetLabels()["sandbox.id"])
	assert.Equal(t, ordinaryPrivateDenyPolicyRole, policy.GetLabels()[ordinaryPolicyRoleLabel])
	assert.Equal(t, "sandbox-pool-a", policy.GetLabels()[ordinaryRuntimeIDLabel])
	assert.Equal(t, "attempt-a", policy.GetAnnotations()[ordinaryPolicyAttemptAnnotation])
	assert.NotContains(t, policy.GetAnnotations(), ordinaryRuntimeUIDAnnotation)
	selector, found, err := unstructured.NestedStringMap(policy.Object, "spec", "endpointSelector", "matchLabels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, map[string]string{"sandbox.id": "customer-a"}, selector)
}

func TestReconcileOrdinaryPoliciesDeletesOnlyProvableOrphans(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	dynClient := fake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{
		ciliumNetworkPolicyGVR: "CiliumNetworkPolicyList",
	})
	orphanIdentity := ordinaryNetworkIdentity{runtimeID: "orphan-runtime", runtimeUID: types.UID("orphan-uid"), logicalID: "orphan"}
	orphan, err := buildOrdinaryNetworkPolicy("runtime", orphanIdentity, "attempt-a", false, nil, false)
	require.NoError(t, err)
	orphan.UID = "orphan-policy-uid"
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Create(context.Background(), orphan, metav1.CreateOptions{})
	require.NoError(t, err)
	cilium, err := buildOrdinaryCiliumPrivateDeny("runtime", orphanIdentity, "attempt-a")
	require.NoError(t, err)
	cilium.SetUID("orphan-cilium-uid")
	_, err = dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Create(context.Background(), cilium, metav1.CreateOptions{})
	require.NoError(t, err)
	liveIdentity := ordinaryNetworkIdentity{runtimeID: "live-runtime", runtimeUID: types.UID("live-uid"), logicalID: "live"}
	live, err := buildOrdinaryNetworkPolicy("runtime", liveIdentity, "attempt-b", false, nil, false)
	require.NoError(t, err)
	live.UID = "live-policy-uid"
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Create(context.Background(), live, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = client.CoreV1().Pods("runtime").Create(context.Background(), &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "live-runtime", UID: "live-uid", Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "live"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	require.NoError(t, reconcileOrphanedOrdinaryPolicies(context.Background(), client, dynClient, "runtime", true))
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), orphan.Name, metav1.GetOptions{})
	require.True(t, errors.IsNotFound(err))
	_, err = dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), cilium.GetName(), metav1.GetOptions{})
	require.True(t, errors.IsNotFound(err))
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), live.Name, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestReleaseReconcileDeletesUnboundOrdinaryAttemptOnlyAfterAPIsAreDrained(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	dynClient := fake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{
		ciliumNetworkPolicyGVR: "CiliumNetworkPolicyList",
	})
	identity := ordinaryNetworkIdentity{runtimeID: "sandbox-pool-orphan", logicalID: "sandbox-pool-orphan"}
	attempt := strings.Repeat("a", 64)
	policy, err := buildOrdinaryNetworkPolicy("runtime", identity, attempt, false, nil, false)
	require.NoError(t, err)
	policy.UID = "unbound-policy-uid"
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Create(context.Background(), policy, metav1.CreateOptions{})
	require.NoError(t, err)
	cilium, err := buildOrdinaryCiliumPrivateDeny("runtime", identity, attempt)
	require.NoError(t, err)
	cilium.SetUID("unbound-cilium-policy-uid")
	_, err = dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Create(context.Background(), cilium, metav1.CreateOptions{})
	require.NoError(t, err)

	require.NoError(t, reconcileOrphanedOrdinaryPolicies(context.Background(), client, dynClient, "runtime", true))
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), policy.Name, metav1.GetOptions{})
	require.NoError(t, err, "ordinary startup recovery must not race an in-flight Pod create")
	_, err = dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), cilium.GetName(), metav1.GetOptions{})
	require.NoError(t, err, "ordinary startup recovery must not race an in-flight Pod create")

	require.NoError(t, reconcileReleaseOrphanedOrdinaryPolicies(context.Background(), client, dynClient, "runtime", true))
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), policy.Name, metav1.GetOptions{})
	require.True(t, errors.IsNotFound(err))
	_, err = dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), cilium.GetName(), metav1.GetOptions{})
	require.True(t, errors.IsNotFound(err))
}

func TestReleaseReconcileKeepsUnboundAttemptWhenRuntimeNameExists(t *testing.T) {
	identity := ordinaryNetworkIdentity{runtimeID: "sandbox-pool-live", logicalID: "logical-id"}
	policy, err := buildOrdinaryNetworkPolicy("runtime", identity, strings.Repeat("b", 64), false, nil, false)
	require.NoError(t, err)
	policy.UID = "unbound-policy-uid"
	client := kubefake.NewSimpleClientset(policy, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      identity.runtimeID,
		Namespace: "runtime",
		UID:       "runtime-uid",
		Labels: map[string]string{
			"sandbox.managed": "true",
			"sandbox.id":      "different-logical-id",
		},
	}})
	stored, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), policy.Name, metav1.GetOptions{})
	require.NoError(t, err)
	_, recognized := classifyOrdinaryNetworkPolicy(stored, true)
	require.True(t, recognized)
	live, err := hasManagedOrdinaryPodForLogicalID(context.Background(), client, "runtime", identity.logicalID)
	require.NoError(t, err)
	require.False(t, live)

	err = reconcileReleaseOrphanedOrdinaryPolicies(context.Background(), client, nil, "runtime", false)
	require.ErrorIs(t, err, runtime.ErrNetworkStateUncertain)
	_, err = client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), policy.Name, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestReconcileOrdinaryPoliciesKeepsLiveFUSEForeignAndMalformedPolicies(t *testing.T) {
	client := kubefake.NewSimpleClientset()
	dynClient := fake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{
		ciliumNetworkPolicyGVR: "CiliumNetworkPolicyList",
	})
	policies := []*networkingv1.NetworkPolicy{
		{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-fuse-system-a", Namespace: "runtime", Labels: map[string]string{"sandbox.managed": "true", ordinaryPolicyRoleLabel: "system"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "third-party", Namespace: "runtime", Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "foreign"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-malformed", Namespace: "runtime", Labels: map[string]string{"sandbox.managed": "true", "sandbox.id": "malformed", ordinaryPolicyRoleLabel: ordinaryPolicyRole, ordinaryRuntimeIDLabel: "runtime-a"}, Annotations: map[string]string{ordinaryRuntimeUIDAnnotation: "uid-a"}}, Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"sandbox.id": "other"}}}},
	}
	for _, policy := range policies {
		_, err := client.NetworkingV1().NetworkPolicies("runtime").Create(context.Background(), policy, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	require.NoError(t, reconcileOrphanedOrdinaryPolicies(context.Background(), client, dynClient, "runtime", true))
	for _, policy := range policies {
		_, err := client.NetworkingV1().NetworkPolicies("runtime").Get(context.Background(), policy.Name, metav1.GetOptions{})
		require.NoError(t, err, policy.Name)
	}
}

func TestReconcileOrdinaryPoliciesFailsClosedOnListGetOrDeleteError(t *testing.T) {
	listErr := stderrors.New("list failed")
	client := kubefake.NewSimpleClientset()
	client.PrependReactor("list", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, listErr
	})
	dynClient := fake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{
		ciliumNetworkPolicyGVR: "CiliumNetworkPolicyList",
	})

	err := reconcileOrphanedOrdinaryPolicies(context.Background(), client, dynClient, "runtime", true)
	require.ErrorIs(t, err, listErr)
}

func TestBuildSystemEgressPolicyRequiresOnlyDNSPort53(t *testing.T) {
	for _, ports := range [][]int32{{}, {5353}, {53, 5353}} {
		_, err := buildSystemEgressPolicy("runtime", "instance-a", runtime.SystemEgressSpec{
			Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"1.1.1.1/32"}, DNSPorts: ports,
			EndpointCIDRs: []string{"192.0.2.10/32"}, EndpointPorts: []int32{443},
		})
		require.Error(t, err)
	}
}

func TestBuildSystemEgressPolicyRejectsSpecialPurposeDNSResolvers(t *testing.T) {
	for _, resolver := range []string{
		"100.64.0.53/32",
		"192.0.2.53/32",
		"198.18.0.53/32",
		"240.0.0.53/32",
		"100::53/128",
		"2001:db8::53/128",
	} {
		t.Run(resolver, func(t *testing.T) {
			_, err := buildSystemEgressPolicy("runtime", "instance-a", runtime.SystemEgressSpec{
				Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{resolver}, DNSPorts: []int32{53},
				EndpointCIDRs: []string{"192.0.2.10/32"}, EndpointPorts: []int32{443},
			})
			require.ErrorContains(t, err, "public")
		})
	}
}

func TestBuildSystemEgressPolicyRejectsFQDNMode(t *testing.T) {
	_, err := buildSystemEgressPolicy("runtime", "instance-a", runtime.SystemEgressSpec{
		Mode: runtime.SystemEgressCiliumFQDN, DNSCIDRs: []string{"1.1.1.1/32"}, DNSPorts: []int32{53},
		EndpointFQDNs: []string{"objects.example.com"}, EndpointPorts: []int32{443},
	})
	require.Error(t, err)
}

func TestBuildCiliumSystemEgressPolicyUsesExactFQDNAndAliasHosts(t *testing.T) {
	policy, err := buildCiliumSystemEgressPolicy("runtime", "instance-a", runtime.SystemEgressSpec{
		Mode: runtime.SystemEgressCiliumFQDN, DNSCIDRs: []string{"1.1.1.1/32"}, DNSPorts: []int32{53},
		EndpointCIDRs: []string{"192.0.2.0/24", "2001:db8::/64"}, EndpointFQDNs: []string{"objects.example.com"}, EndpointPorts: []int32{443},
	}, []string{"192.0.2.20", "2001:db8::20"})
	require.NoError(t, err)
	selector, found, err := unstructured.NestedStringMap(policy.Object, "spec", "endpointSelector", "matchLabels")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, map[string]string{"sandbox.pool.instance": "instance-a"}, selector)
	raw, err := json.Marshal(policy.Object["spec"])
	require.NoError(t, err)
	assert.Contains(t, string(raw), "objects.example.com")
	assert.Contains(t, string(raw), "192.0.2.20/32")
	assert.Contains(t, string(raw), "2001:db8::20/128")
	assert.NotContains(t, string(raw), "192.0.2.0/24")
	assert.NotContains(t, string(raw), "*")
}

func TestBuildCiliumSystemEgressPolicyRejectsWildcardAndUnapprovedAlias(t *testing.T) {
	base := runtime.SystemEgressSpec{
		Mode: runtime.SystemEgressCiliumFQDN, DNSCIDRs: []string{"1.1.1.1/32"}, DNSPorts: []int32{53},
		EndpointCIDRs: []string{"192.0.2.0/24"}, EndpointFQDNs: []string{"objects.example.com"}, EndpointPorts: []int32{443},
	}
	wildcard := base
	wildcard.EndpointFQDNs = []string{"*.example.com"}
	_, err := buildCiliumSystemEgressPolicy("runtime", "instance-a", wildcard, nil)
	require.Error(t, err)
	_, err = buildCiliumSystemEgressPolicy("runtime", "instance-a", base, []string{"198.51.100.10"})
	require.Error(t, err)
	ipLiteral := base
	ipLiteral.EndpointFQDNs = []string{"192.0.2.10"}
	_, err = buildCiliumSystemEgressPolicy("runtime", "instance-a", ipLiteral, nil)
	require.Error(t, err)
}

func TestFUSEPolicyNamesRequireDNS1123Instance(t *testing.T) {
	spec := runtime.SystemEgressSpec{Mode: runtime.SystemEgressCIDR, DNSCIDRs: []string{"1.1.1.1/32"}, DNSPorts: []int32{53}, EndpointCIDRs: []string{"192.0.2.10/32"}, EndpointPorts: []int32{443}}
	_, err := buildSystemEgressPolicy("runtime", "Instance_A", spec)
	require.Error(t, err)
	_, err = buildFUSEUserNetworkPolicy("runtime", "Instance_A", "uid-a", false, nil, false, []string{"8.8.8.8/32"})
	require.Error(t, err)
}

func TestBuildDisabledFUSEUserNetworkPolicyHasNoUserEgress(t *testing.T) {
	policy, err := buildFUSEUserNetworkPolicy("runtime", "instance-a", "uid-a", false, nil, false, []string{"8.8.8.8/32"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"sandbox.pool.instance": "instance-a"}, policy.Spec.PodSelector.MatchLabels)
	assert.Equal(t, "uid-a", policy.Annotations[fuseRuntimeUIDAnnotation])
	assert.Empty(t, policy.Spec.Egress)
}

func TestBuildEnabledFUSEUserNetworkPolicyAllowsOnlyExactApprovedDNSHosts(t *testing.T) {
	policy, err := buildFUSEUserNetworkPolicy("runtime", "instance-a", "uid-a", true, []string{"192.0.2.10/32"}, false, []string{"8.8.8.8/32", "2001:4860:4860::8888/128"})
	require.NoError(t, err)
	require.NotEmpty(t, policy.Spec.Egress)
	dns := policy.Spec.Egress[0]
	require.Len(t, dns.To, 2)
	assert.ElementsMatch(t, []string{"8.8.8.8/32", "2001:4860:4860::8888/128"}, []string{dns.To[0].IPBlock.CIDR, dns.To[1].IPBlock.CIDR})
	require.Len(t, dns.Ports, 2)
	for _, rule := range policy.Spec.Egress {
		assert.NotEmpty(t, rule.To, "user policy must never contain destination-less DNS or data egress")
	}

	_, err = buildFUSEUserNetworkPolicy("runtime", "instance-a", "uid-a", true, []string{"192.0.2.10/32"}, false, []string{"10.0.0.53/32"})
	require.NoError(t, err, "the exact cluster DNS Service IP may be private")
	_, err = buildFUSEUserNetworkPolicy("runtime", "instance-a", "uid-a", true, nil, false, []string{"10.0.0.0/24"})
	require.Error(t, err, "cluster DNS egress must remain host-only")
}

func TestBuildOpenFUSEUserNetworkPolicyKeepsExplicitDNSRule(t *testing.T) {
	policy, err := buildFUSEUserNetworkPolicy("runtime", "instance-a", "uid-a", true, nil, false, []string{"8.8.8.8/32"})
	require.NoError(t, err)
	require.Len(t, policy.Spec.Egress, 3)
	dns := policy.Spec.Egress[0]
	require.Len(t, dns.To, 1)
	assert.Equal(t, "8.8.8.8/32", dns.To[0].IPBlock.CIDR)
	require.Len(t, dns.Ports, 2)
	for _, port := range dns.Ports {
		require.NotNil(t, port.Port)
		assert.Equal(t, int32(53), port.Port.IntVal)
	}
}

func TestBuildFUSEUserPoliciesRejectPermanentlyDeniedDestinations(t *testing.T) {
	for _, destination := range []string{"0.0.0.0/32", "127.0.0.1/32", "169.254.169.254/32", "224.0.0.1/32", "::/128", "::1/128", "fe80::1/128", "ff02::1/128"} {
		_, err := buildFUSEUserNetworkPolicy("runtime", "instance-a", "uid-a", true, []string{destination}, false, []string{"8.8.8.8/32"})
		require.Error(t, err, destination)
	}
}

func TestBuildFUSECiliumUserDenyPreservesOnlyApprovedPrivateExceptions(t *testing.T) {
	policy, err := buildFUSECiliumUserDenyPolicy("runtime", "instance-a", "uid-a", true, []string{"10.10.0.10/32", "10.20.0.10/32", "192.0.2.10/32"})
	require.NoError(t, err)
	deny, found, err := unstructured.NestedSlice(policy.Object, "spec", "egressDeny")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, deny, 1)
	denyRule := deny[0].(map[string]any)
	cidrs := denyRule["toCIDRSet"].([]any)
	var tenRule map[string]any
	for _, raw := range cidrs {
		rule := raw.(map[string]any)
		if rule["cidr"] == "10.0.0.0/8" {
			tenRule = rule
		}
		if rule["cidr"] == "169.254.0.0/16" {
			assert.NotContains(t, rule, "except")
		}
	}
	require.NotNil(t, tenRule)
	assert.ElementsMatch(t, []any{"10.10.0.10/32", "10.20.0.10/32"}, tenRule["except"])
}

func TestCiliumUserPolicyIntentRequiresExactNetworkAttempt(t *testing.T) {
	desired, err := buildFUSECiliumUserDenyPolicy("runtime", "instance-a", "uid-a", true, []string{"10.20.0.10/32"})
	require.NoError(t, err)
	desired.SetAnnotations(map[string]string{
		fuseRuntimeUIDAnnotation:     "uid-a",
		fuseNetworkAttemptAnnotation: "attempt-a",
	})
	foreign := desired.DeepCopy()
	foreign.SetAnnotations(map[string]string{
		fuseRuntimeUIDAnnotation:     "uid-a",
		fuseNetworkAttemptAnnotation: "attempt-b",
	})
	assert.False(t, ciliumUserPolicyIntentMatches(foreign, desired))

	missing := desired.DeepCopy()
	missing.SetAnnotations(map[string]string{fuseRuntimeUIDAnnotation: "uid-a"})
	assert.False(t, ciliumUserPolicyIntentMatches(missing, desired))
}

func TestDeleteExactNetworkPolicyConfirmsPolicyObjectUID(t *testing.T) {
	newPolicy := func(uid types.UID) *networkingv1.NetworkPolicy {
		return &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "sandbox-fuse-system-instance-a", Namespace: "runtime", UID: uid,
				Labels:      map[string]string{"sandbox.managed": "true", "sandbox.pool.instance": "instance-a", "sandbox.policy.role": "system"},
				Annotations: map[string]string{fuseRuntimeUIDAnnotation: "runtime-a"}},
			Spec: networkingv1.NetworkPolicySpec{PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"sandbox.pool.instance": "instance-a"}}},
		}
	}
	t.Run("annotation drift on same object is not deletion", func(t *testing.T) {
		client := kubefake.NewSimpleClientset(newPolicy(types.UID("policy-old")))
		client.PrependReactor("delete", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			current, err := client.Tracker().Get(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), "runtime", action.(ktesting.DeleteAction).GetName())
			require.NoError(t, err)
			updated := current.(*networkingv1.NetworkPolicy).DeepCopy()
			updated.Annotations[fuseRuntimeUIDAnnotation] = "runtime-rebound"
			require.NoError(t, client.Tracker().Update(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), updated, "runtime"))
			return true, nil, nil
		})
		err := deleteExactNetworkPolicy(context.Background(), client, "runtime", "sandbox-fuse-system-instance-a", "instance-a", "system", "runtime-a", "", false)
		require.Error(t, err)
	})

	t.Run("replacement object with same annotation proves old deletion", func(t *testing.T) {
		client := kubefake.NewSimpleClientset(newPolicy(types.UID("policy-old")))
		client.PrependReactor("delete", "networkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
			name := action.(ktesting.DeleteAction).GetName()
			require.NoError(t, client.Tracker().Delete(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), "runtime", name))
			require.NoError(t, client.Tracker().Create(networkingv1.SchemeGroupVersion.WithResource("networkpolicies"), newPolicy(types.UID("policy-new")), "runtime"))
			return true, nil, nil
		})
		require.NoError(t, deleteExactNetworkPolicy(context.Background(), client, "runtime", "sandbox-fuse-system-instance-a", "instance-a", "system", "runtime-a", "", false))
	})
}

func TestDeleteAttemptOrdinaryCiliumPrivateDenyRequiresUIDDisappearance(t *testing.T) {
	dynClient := fake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{
		ciliumNetworkPolicyGVR: "CiliumNetworkPolicyList",
	})
	identity := ordinaryNetworkIdentity{runtimeID: "runtime-a", logicalID: "customer-a"}
	policy, err := buildOrdinaryCiliumPrivateDeny("runtime", identity, "attempt-a")
	require.NoError(t, err)
	policy.SetUID("policy-uid")
	_, err = dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Create(context.Background(), policy, metav1.CreateOptions{})
	require.NoError(t, err)
	dynClient.PrependReactor("delete", "ciliumnetworkpolicies", func(action ktesting.Action) (bool, k8sruntime.Object, error) {
		return true, nil, nil
	})

	err = deleteAttemptOrdinaryCiliumPrivateDeny(context.Background(), dynClient, "runtime", policy.GetName(), identity, "attempt-a")
	require.ErrorContains(t, err, "unconfirmed")
}
