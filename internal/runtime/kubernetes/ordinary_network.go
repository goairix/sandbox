package kubernetes

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kvalidation "k8s.io/apimachinery/pkg/util/validation"

	"github.com/goairix/sandbox/internal/runtime"
)

const (
	ordinaryPolicyRole              = "ordinary"
	ordinaryPrivateDenyPolicyRole   = "ordinary-private-deny"
	ordinaryPolicyRoleLabel         = "sandbox.policy.role"
	ordinaryRuntimeIDLabel          = "sandbox.runtime.id"
	ordinaryRuntimeUIDAnnotation    = "sandbox.runtime.uid"
	ordinaryPolicyAttemptAnnotation = "sandbox.network.attempt"
)

type ordinaryNetworkIdentity struct {
	runtimeID  string
	runtimeUID types.UID
	logicalID  string
}

func ordinaryLogicalID(spec runtime.SandboxSpec) (string, error) {
	if spec.WorkspaceFUSE != nil || spec.Labels["sandbox.workspace.mode"] == "fuse" {
		return "", fmt.Errorf("FUSE sandbox cannot use ordinary network identity")
	}
	logicalID := spec.ID
	if labelled := spec.Labels["sandbox.id"]; labelled != "" {
		logicalID = labelled
	}
	if errs := kvalidation.IsDNS1123Subdomain(logicalID); len(errs) != 0 {
		return "", fmt.Errorf("invalid ordinary sandbox logical ID %q: %s", logicalID, errs[0])
	}
	if errs := kvalidation.IsDNS1123Subdomain(spec.ID); len(errs) != 0 {
		return "", fmt.Errorf("invalid ordinary sandbox runtime ID %q: %s", spec.ID, errs[0])
	}
	return logicalID, nil
}

func ordinaryIdentityFromPod(pod *corev1.Pod, expectedRuntimeID string) (ordinaryNetworkIdentity, error) {
	if pod == nil || pod.Name != expectedRuntimeID {
		return ordinaryNetworkIdentity{}, fmt.Errorf("ordinary Pod runtime identity does not match %q", expectedRuntimeID)
	}
	if pod.Labels["sandbox.managed"] != "true" {
		return ordinaryNetworkIdentity{}, fmt.Errorf("Pod %q is not a managed sandbox", pod.Name)
	}
	if pod.Labels["sandbox.workspace.mode"] == "fuse" {
		return ordinaryNetworkIdentity{}, fmt.Errorf("Pod %q is a FUSE sandbox", pod.Name)
	}
	logicalID := pod.Labels["sandbox.id"]
	if errs := kvalidation.IsDNS1123Subdomain(pod.Name); len(errs) != 0 {
		return ordinaryNetworkIdentity{}, fmt.Errorf("invalid ordinary runtime ID %q: %s", pod.Name, errs[0])
	}
	if errs := kvalidation.IsDNS1123Subdomain(logicalID); len(errs) != 0 {
		return ordinaryNetworkIdentity{}, fmt.Errorf("invalid ordinary logical ID %q: %s", logicalID, errs[0])
	}
	return ordinaryNetworkIdentity{runtimeID: pod.Name, runtimeUID: pod.UID, logicalID: logicalID}, nil
}

func ordinaryPolicyMetadata(namespace string, identity ordinaryNetworkIdentity, role, attempt string) metav1.ObjectMeta {
	labels := map[string]string{
		"sandbox.managed":       "true",
		"sandbox.id":            identity.logicalID,
		ordinaryPolicyRoleLabel: role,
		ordinaryRuntimeIDLabel:  identity.runtimeID,
	}
	annotations := map[string]string{}
	if attempt != "" {
		annotations[ordinaryPolicyAttemptAnnotation] = attempt
	}
	if identity.runtimeUID != "" {
		annotations[ordinaryRuntimeUIDAnnotation] = string(identity.runtimeUID)
	}
	return metav1.ObjectMeta{Namespace: namespace, Labels: labels, Annotations: annotations}
}

func buildOrdinaryNetworkPolicy(namespace string, identity ordinaryNetworkIdentity, attempt string, enabled bool, whitelist []string, blockPrivate bool) (*networkingv1.NetworkPolicy, error) {
	if errs := kvalidation.IsDNS1123Subdomain(identity.runtimeID); len(errs) != 0 {
		return nil, fmt.Errorf("invalid ordinary runtime ID %q: %s", identity.runtimeID, errs[0])
	}
	if errs := kvalidation.IsDNS1123Subdomain(identity.logicalID); len(errs) != 0 {
		return nil, fmt.Errorf("invalid ordinary logical ID %q: %s", identity.logicalID, errs[0])
	}
	resolvedCIDRs, err := resolveToCIDRs(whitelist)
	if err != nil {
		return nil, err
	}
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	egress := []networkingv1.NetworkPolicyEgressRule{{Ports: []networkingv1.NetworkPolicyPort{
		{Protocol: &udp, Port: &intstr.IntOrString{IntVal: 53}},
		{Protocol: &tcp, Port: &intstr.IntOrString{IntVal: 53}},
	}}}
	switch {
	case enabled && !blockPrivate && len(resolvedCIDRs) == 0:
		egress = append(egress,
			networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"169.254.0.0/16"}}}}},
			networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: []string{"fe80::/10", "fc00::/7", "::1/128"}}}}},
		)
	case blockPrivate:
		for _, cidr := range resolvedCIDRs {
			egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}}})
		}
		egress = append(egress,
			networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "169.254.0.0/16"}}}}},
			networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: []string{"fc00::/7", "::1/128", "fe80::/10"}}}}},
		)
	case len(resolvedCIDRs) != 0:
		for _, cidr := range resolvedCIDRs {
			egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: cidr}}}})
		}
	}
	metadata := ordinaryPolicyMetadata(namespace, identity, ordinaryPolicyRole, attempt)
	metadata.Name = "sandbox-" + identity.logicalID
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metadata,
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"sandbox.id": identity.logicalID}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}, nil
}

func buildOrdinaryCiliumPrivateDeny(namespace string, identity ordinaryNetworkIdentity, attempt string) (*unstructured.Unstructured, error) {
	if errs := kvalidation.IsDNS1123Subdomain(identity.runtimeID); len(errs) != 0 {
		return nil, fmt.Errorf("invalid ordinary runtime ID %q: %s", identity.runtimeID, errs[0])
	}
	if errs := kvalidation.IsDNS1123Subdomain(identity.logicalID); len(errs) != 0 {
		return nil, fmt.Errorf("invalid ordinary logical ID %q: %s", identity.logicalID, errs[0])
	}
	metadata := ordinaryPolicyMetadata(namespace, identity, ordinaryPrivateDenyPolicyRole, attempt)
	metadata.Name = "sandbox-private-deny-" + identity.logicalID
	toCIDRSet := []any{
		map[string]any{"cidr": "10.0.0.0/8"}, map[string]any{"cidr": "172.16.0.0/12"},
		map[string]any{"cidr": "192.168.0.0/16"}, map[string]any{"cidr": "127.0.0.0/8"},
		map[string]any{"cidr": "169.254.0.0/16"}, map[string]any{"cidr": "fc00::/7"},
		map[string]any{"cidr": "::1/128"}, map[string]any{"cidr": "fe80::/10"},
	}
	policy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata": map[string]any{
			"name": metadata.Name, "namespace": namespace,
		},
		"spec": map[string]any{
			"endpointSelector": map[string]any{"matchLabels": map[string]any{"sandbox.id": identity.logicalID}},
			"egressDeny":       []any{map[string]any{"toCIDRSet": toCIDRSet}},
		},
	}}
	policy.SetLabels(metadata.Labels)
	policy.SetAnnotations(metadata.Annotations)
	return policy, nil
}
