package kubernetes

import (
	"context"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

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

// updateNetworkPolicy upserts or removes the NetworkPolicy for a sandbox.
//
//   - enabled=false: isolation mode — deny all egress except DNS
//   - enabled=true, blockPrivate=false, whitelist=[]: open mode — delete policy (K8s default = allow all)
//   - enabled=true, whitelist=[...]: whitelist-only egress
//   - enabled=true, blockPrivate=true: allow external, block RFC1918/ULA; whitelist = internal allowlist
func updateNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, sandboxID string, enabled bool, whitelist []string, blockPrivate bool) error {
	if !enabled {
		// Isolation: deny-all except DNS
		return applyNetworkPolicy(ctx, client, namespace, sandboxID, nil, false)
	}
	if !blockPrivate && len(whitelist) == 0 {
		// Open: delete the NetworkPolicy entirely — K8s default allows all egress
		return deleteNetworkPolicy(ctx, client, namespace, sandboxID)
	}
	return applyNetworkPolicy(ctx, client, namespace, sandboxID, whitelist, blockPrivate)
}
