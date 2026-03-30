package kubernetes

import (
	"context"
	"fmt"
	"net"

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

	// Allow whitelisted CIDRs
	for _, entry := range whitelist {
		cidr := entry
		if ip := net.ParseIP(entry); ip != nil {
			// Pure IP, convert to CIDR
			if ip.To4() != nil {
				cidr = entry + "/32"
			} else {
				cidr = entry + "/128"
			}
		} else if _, _, err := net.ParseCIDR(entry); err != nil {
			return fmt.Errorf("invalid whitelist entry %q: must be a valid IP or CIDR", entry)
		}
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
