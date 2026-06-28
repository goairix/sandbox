package kubernetes

import (
	"context"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/goairix/sandbox/internal/logger"
)

// detectCilium reports whether the CiliumNetworkPolicy CRD is available on this cluster.
// Called once at startup; result is cached in Runtime.hasCilium.
func detectCilium(client kubernetes.Interface) bool {
	groups, err := client.Discovery().ServerGroups()
	if err != nil {
		return false
	}
	for _, g := range groups.Groups {
		if g.Name == "cilium.io" {
			return true
		}
	}
	return false
}

// ciliumNetworkPolicyGVR is the GroupVersionResource for CiliumNetworkPolicy CRD.
var ciliumNetworkPolicyGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}

// applyCiliumPrivateDeny creates/updates a CiliumNetworkPolicy with egressDeny for all
// RFC1918 and link-local ranges. Standard K8s NetworkPolicy IPBlock/Except can be bypassed
// in Cilium when the destination IP falls back to the "world" identity before the CIDR
// label is assigned; an explicit eBPF-level deny is the only reliable fix.
func applyCiliumPrivateDeny(ctx context.Context, dynClient dynamic.Interface, namespace, sandboxID string) error {
	name := "sandbox-private-deny-" + sandboxID
	toCIDRSet := []any{
		map[string]any{"cidr": "10.0.0.0/8"},
		map[string]any{"cidr": "172.16.0.0/12"},
		map[string]any{"cidr": "192.168.0.0/16"},
		map[string]any{"cidr": "127.0.0.0/8"},
		map[string]any{"cidr": "169.254.0.0/16"},
		map[string]any{"cidr": "fc00::/7"},
		map[string]any{"cidr": "::1/128"},
		map[string]any{"cidr": "fe80::/10"},
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata": map[string]any{
			"name": name, "namespace": namespace,
			"labels": map[string]any{"sandbox.managed": "true", "sandbox.id": sandboxID},
		},
		"spec": map[string]any{
			"endpointSelector": map[string]any{
				"matchLabels": map[string]any{"sandbox.id": sandboxID},
			},
			"egressDeny": []any{
				map[string]any{"toCIDRSet": toCIDRSet},
			},
		},
	}}
	existing, getErr := dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if getErr == nil {
		obj.SetResourceVersion(existing.GetResourceVersion())
		_, err := dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(namespace).Update(ctx, obj, metav1.UpdateOptions{})
		if err != nil {
			logger.Error(ctx, "failed to update CiliumNetworkPolicy private deny",
				logger.AddField("sandbox_id", sandboxID), logger.ErrorField(err))
		}
		return err
	}
	if !errors.IsNotFound(getErr) {
		logger.Error(ctx, "failed to get CiliumNetworkPolicy private deny",
			logger.AddField("sandbox_id", sandboxID), logger.ErrorField(getErr))
		return getErr
	}
	logger.Info(ctx, "applying CiliumNetworkPolicy private deny", logger.AddField("sandbox_id", sandboxID))
	_, err := dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(namespace).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		logger.Error(ctx, "failed to create CiliumNetworkPolicy private deny",
			logger.AddField("sandbox_id", sandboxID), logger.ErrorField(err))
	}
	return err
}

// deleteCiliumPrivateDeny removes the CiliumNetworkPolicy created by applyCiliumPrivateDeny.
func deleteCiliumPrivateDeny(ctx context.Context, dynClient dynamic.Interface, namespace, sandboxID string) error {
	err := dynClient.Resource(ciliumNetworkPolicyGVR).Namespace(namespace).Delete(ctx, "sandbox-private-deny-"+sandboxID, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		logger.Error(ctx, "failed to delete CiliumNetworkPolicy private deny",
			logger.AddField("sandbox_id", sandboxID), logger.ErrorField(err))
	}
	return err
}

// applyNetworkPolicy builds and upserts the NetworkPolicy for a sandbox.
//
// Mode selection (evaluated in order):
//  1. blockPrivate=true: allow external, block RFC1918/ULA; whitelist = internal allowlist
//  2. len(whitelist)>0: whitelist-only egress
//  3. default: isolation — deny all egress except DNS
func applyNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace string, sandboxID string, whitelist []string, blockPrivate bool) error {
	policyName := fmt.Sprintf("sandbox-%s", sandboxID)

	resolvedCIDRs, err := resolveToCIDRs(whitelist)
	if err != nil {
		return err
	}

	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	var egressRules []networkingv1.NetworkPolicyEgressRule

	// Always allow DNS
	egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{
			{Protocol: &udp, Port: &intstr.IntOrString{IntVal: 53}},
			{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 53}},
		},
	})

	switch {
	case blockPrivate:
		// Allow whitelisted internal addresses individually (before the block rules)
		for _, cidr := range resolvedCIDRs {
			egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{
					{IPBlock: &networkingv1.IPBlock{CIDR: cidr}},
				},
			})
		}
		// Allow all external IPv4 traffic, excluding RFC1918 private ranges.
		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{
				{
					IPBlock: &networkingv1.IPBlock{
						CIDR: "0.0.0.0/0",
						Except: []string{
							"10.0.0.0/8",
							"172.16.0.0/12",
							"192.168.0.0/16",
							"127.0.0.0/8",
							"169.254.0.0/16",
						},
					},
				},
			},
		})
		// Allow all external IPv6 traffic, excluding private/ULA ranges.
		egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{
				{
					IPBlock: &networkingv1.IPBlock{
						CIDR: "::/0",
						Except: []string{
							"fc00::/7",  // ULA
							"::1/128",   // loopback
							"fe80::/10", // link-local
						},
					},
				},
			},
		})

	case len(resolvedCIDRs) > 0:
		// Whitelist-only mode: allow only specified destinations
		for _, cidr := range resolvedCIDRs {
			egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{
					{IPBlock: &networkingv1.IPBlock{CIDR: cidr}},
				},
			})
		}

	default:
		// Isolation mode: only DNS is allowed (the rule added above).
		// PolicyTypeEgress with no additional To rules means deny-all except
		// what is explicitly listed — in this case, DNS only.
	}

	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policyName,
			Namespace: namespace,
			Labels: map[string]string{
				"sandbox.managed": "true",
				"sandbox.id":      sandboxID,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"sandbox.id": sandboxID},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egressRules,
		},
	}

	// Upsert: update if exists, create otherwise.
	existing, getErr := client.NetworkingV1().NetworkPolicies(namespace).Get(ctx, policyName, metav1.GetOptions{})
	if getErr == nil {
		policy.ResourceVersion = existing.ResourceVersion
		_, err = client.NetworkingV1().NetworkPolicies(namespace).Update(ctx, policy, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("update network policy: %w", err)
		}
		return nil
	}
	if !errors.IsNotFound(getErr) {
		return fmt.Errorf("get network policy: %w", getErr)
	}
	_, err = client.NetworkingV1().NetworkPolicies(namespace).Create(ctx, policy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create network policy: %w", err)
	}
	return nil
}

// resolveToCIDRs converts a list of IPs, CIDRs, or domain names to CIDR strings.
func resolveToCIDRs(entries []string) ([]string, error) {
	var cidrs []string
	for _, entry := range entries {
		if ip := net.ParseIP(entry); ip != nil {
			if ip.To4() != nil {
				cidrs = append(cidrs, entry+"/32")
			} else {
				cidrs = append(cidrs, entry+"/128")
			}
		} else if _, _, err := net.ParseCIDR(entry); err == nil {
			cidrs = append(cidrs, entry)
		} else {
			ips, err := net.LookupIP(entry)
			if err != nil {
				return nil, fmt.Errorf("resolve whitelist domain %q: %w", entry, err)
			}
			for _, ip := range ips {
				if ip.To4() != nil {
					cidrs = append(cidrs, ip.String()+"/32")
				} else {
					cidrs = append(cidrs, ip.String()+"/128")
				}
			}
		}
	}
	return cidrs, nil
}

// deleteNetworkPolicy removes the sandbox network policy. Ignores not-found errors.
func deleteNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, sandboxID string) error {
	policyName := fmt.Sprintf("sandbox-%s", sandboxID)
	err := client.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, policyName, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete network policy: %w", err)
	}
	return nil
}

// applyOpenNetworkPolicy creates a NetworkPolicy that allows all egress except the
// cloud metadata endpoint (169.254.0.0/16) and IPv6 link-local/ULA ranges.
// Used for open mode — metadata must always be blocked regardless of other settings.
func applyOpenNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, sandboxID string) error {
	policyName := fmt.Sprintf("sandbox-%s", sandboxID)
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policyName,
			Namespace: namespace,
			Labels:    map[string]string{"sandbox.managed": "true", "sandbox.id": sandboxID},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{"sandbox.id": sandboxID},
			},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{Ports: []networkingv1.NetworkPolicyPort{
					{Protocol: &udp, Port: &intstr.IntOrString{IntVal: 53}},
					{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 53}},
				}},
				{To: []networkingv1.NetworkPolicyPeer{
					{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"169.254.0.0/16"}}},
				}},
				{To: []networkingv1.NetworkPolicyPeer{
					{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: []string{"fe80::/10", "fc00::/7", "::1/128"}}},
				}},
			},
		},
	}
	existing, getErr := client.NetworkingV1().NetworkPolicies(namespace).Get(ctx, policyName, metav1.GetOptions{})
	if getErr == nil {
		policy.ResourceVersion = existing.ResourceVersion
		_, err := client.NetworkingV1().NetworkPolicies(namespace).Update(ctx, policy, metav1.UpdateOptions{})
		return err
	}
	if !errors.IsNotFound(getErr) {
		return getErr
	}
	_, err := client.NetworkingV1().NetworkPolicies(namespace).Create(ctx, policy, metav1.CreateOptions{})
	return err
}

// updateNetworkPolicy upserts or removes the NetworkPolicy for a sandbox.
//
//   - enabled=false: isolation mode — deny all egress except DNS
//   - enabled=true, blockPrivate=false, whitelist=[]: open mode — allow all except metadata endpoint
//   - enabled=true, whitelist=[...]: whitelist-only egress
//   - enabled=true, blockPrivate=true: allow external, block RFC1918/ULA; whitelist = internal allowlist
func updateNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, sandboxID string, enabled bool, whitelist []string, blockPrivate bool) error {
	if !enabled {
		// Isolation: deny-all except DNS
		return applyNetworkPolicy(ctx, client, namespace, sandboxID, nil, false)
	}
	if !blockPrivate && len(whitelist) == 0 {
		// Open mode: allow all egress but always block cloud metadata endpoint.
		return applyOpenNetworkPolicy(ctx, client, namespace, sandboxID)
	}
	return applyNetworkPolicy(ctx, client, namespace, sandboxID, whitelist, blockPrivate)
}
