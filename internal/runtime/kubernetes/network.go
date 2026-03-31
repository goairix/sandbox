package kubernetes

import (
	"context"
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// ensureNetworkPolicy creates a NetworkPolicy that restricts egress traffic
// for sandbox pods. If whitelist is empty, all egress is denied.
func ensureNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace string, sandboxID string, whitelist []string) error {
	policyName := fmt.Sprintf("sandbox-%s", sandboxID)

	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	var egressRules []networkingv1.NetworkPolicyEgressRule

	// Always allow DNS (both UDP and TCP)
	dnsPortUDP := networkingv1.NetworkPolicyPort{
		Protocol: &udp,
		Port:     &intstr.IntOrString{IntVal: 53},
	}
	dnsPortTCP := networkingv1.NetworkPolicyPort{
		Protocol: &tcp,
		Port:     &intstr.IntOrString{IntVal: 53},
	}
	egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
		Ports: []networkingv1.NetworkPolicyPort{dnsPortUDP, dnsPortTCP},
	})

	// Allow whitelisted CIDRs (resolve domain names to IPs first)
	for _, entry := range whitelist {
		var cidrs []string
		if ip := net.ParseIP(entry); ip != nil {
			if ip.To4() != nil {
				cidrs = append(cidrs, entry+"/32")
			} else {
				cidrs = append(cidrs, entry+"/128")
			}
		} else if _, _, err := net.ParseCIDR(entry); err == nil {
			cidrs = append(cidrs, entry)
		} else {
			// Treat as domain name, resolve to IPs
			ips, lookupErr := net.LookupIP(entry)
			if lookupErr != nil {
				return fmt.Errorf("resolve whitelist domain %q: %w", entry, lookupErr)
			}
			for _, ip := range ips {
				if ip.To4() != nil {
					cidrs = append(cidrs, ip.String()+"/32")
				} else {
					cidrs = append(cidrs, ip.String()+"/128")
				}
			}
		}
		for _, cidr := range cidrs {
			egressRules = append(egressRules, networkingv1.NetworkPolicyEgressRule{
				To: []networkingv1.NetworkPolicyPeer{
					{
						IPBlock: &networkingv1.IPBlock{
							CIDR: cidr,
						},
					},
				},
			})
		}
	}

	policy := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      policyName,
			Namespace: namespace,
			Labels: map[string]string{
				"sandbox.managed": "true",
				"sandbox.id":     sandboxID,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					"sandbox.id": sandboxID,
				},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeEgress,
			},
			Egress: egressRules,
		},
	}

	_, err := client.NetworkingV1().NetworkPolicies(namespace).Create(ctx, policy, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create network policy: %w", err)
	}
	return nil
}

// deleteNetworkPolicy removes the sandbox network policy.
func deleteNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, sandboxID string) error {
	policyName := fmt.Sprintf("sandbox-%s", sandboxID)
	return client.NetworkingV1().NetworkPolicies(namespace).Delete(ctx, policyName, metav1.DeleteOptions{})
}

// updateNetworkPolicy updates or creates/deletes a network policy for a sandbox.
func updateNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, sandboxID string, enabled bool, whitelist []string) error {
	if !enabled {
		err := deleteNetworkPolicy(ctx, client, namespace, sandboxID)
		if err != nil && !strings.Contains(err.Error(), "not found") {
			return err
		}
		return nil
	}

	// Delete existing policy first, then recreate
	_ = deleteNetworkPolicy(ctx, client, namespace, sandboxID)

	if len(whitelist) > 0 {
		return ensureNetworkPolicy(ctx, client, namespace, sandboxID, whitelist)
	}
	return nil
}
