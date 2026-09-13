package kubernetes

import (
	"context"
	"github.com/goairix/sandbox/internal/runtime"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"testing"
)

func TestPrivateCIDRDenyOmitsFullyWhitelistedRange(t *testing.T) {
	for _, tc := range []struct{ allow, omit string }{
		{"10.0.0.0/7", "10.0.0.0/8"},
		{"172.0.0.0/11", "172.16.0.0/12"},
		{"192.168.0.0/15", "192.168.0.0/16"},
		{"fc00::/7", "fc00::/7"},
	} {
		for _, mode := range []string{"ordinary", "fuse"} {
			t.Run(mode+"/"+tc.allow, func(t *testing.T) {
				var policy *unstructured.Unstructured
				var err error
				if mode == "ordinary" {
					policy, err = buildOrdinaryCiliumPrivateDeny("runtime", ordinaryNetworkIdentity{runtimeID: "pod", logicalID: "sandbox"}, "attempt", []string{tc.allow})
				} else {
					policy, err = buildFUSECiliumUserDenyPolicy("runtime", "instance", "uid", true, []string{tc.allow})
				}
				require.NoError(t, err)
				rules, _, err := unstructured.NestedSlice(policy.Object, "spec", "egressDeny")
				require.NoError(t, err)
				cidrs := rules[0].(map[string]any)["toCIDRSet"].([]any)
				actual := map[string]map[string]any{}
				for _, raw := range cidrs {
					rule := raw.(map[string]any)
					actual[rule["cidr"].(string)] = rule
				}
				require.NotContains(t, actual, tc.omit, "a private deny must not shadow the accepted broader whitelist")
				require.Contains(t, actual, "169.254.0.0/16", "permanent metadata deny must remain")
				require.Contains(t, actual, "127.0.0.0/8", "permanent loopback deny must remain")
				require.Contains(t, actual, "fe80::/10", "permanent IPv6 link-local deny must remain")
			})
		}
	}
}

func TestSupernetWhitelistStillRejectsAuthoritativeClusterOverlap(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	r.hasCilium = true
	pod := seedOrdinaryPodWithLogicalPolicy(t, r, client, "pod", "sandbox")
	client.ClearActions()
	require.ErrorIs(t, r.UpdateNetwork(context.Background(), pod.Name, true, []string{"10.0.0.0/7"}, true), runtime.ErrInvalidNetworkTarget)
	for _, action := range client.Actions() {
		require.Equal(t, "get", action.GetVerb())
	}
}

func TestOrdinaryExternalSupernetUpdateDoesNotShadowAcceptedAllowance(t *testing.T) {
	r, client := newFakeKubernetesRuntime(t, preparedScript())
	r.hasCilium = true
	WithNetworkCIDRs([]string{"172.22.0.0/16"}, []string{"172.23.0.0/16"})(r)
	pod := seedOrdinaryPodWithLogicalPolicy(t, r, client, "pod", "sandbox")
	require.NoError(t, r.UpdateNetwork(context.Background(), pod.Name, true, []string{"10.0.0.0/7"}, true))
	deny, err := r.dynClient.Resource(ciliumNetworkPolicyGVR).Namespace("runtime").Get(context.Background(), "sandbox-private-deny-sandbox", metav1.GetOptions{})
	require.NoError(t, err)
	rules, _, err := unstructured.NestedSlice(deny.Object, "spec", "egressDeny")
	require.NoError(t, err)
	for _, raw := range rules[0].(map[string]any)["toCIDRSet"].([]any) {
		require.NotEqual(t, "10.0.0.0/8", raw.(map[string]any)["cidr"])
	}
}
