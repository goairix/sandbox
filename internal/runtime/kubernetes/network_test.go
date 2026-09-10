package kubernetes

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
