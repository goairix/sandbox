package kubernetes

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	typednetworkingv1 "k8s.io/client-go/kubernetes/typed/networking/v1"

	"github.com/goairix/sandbox/internal/logger"
	"github.com/goairix/sandbox/internal/runtime"
)

const (
	fuseSystemPolicyPrefix       = "sandbox-fuse-system-"
	fuseUserPolicyPrefix         = "sandbox-fuse-user-"
	fuseUserDenyPolicyPrefix     = "sandbox-fuse-user-deny-"
	fuseRuntimeUIDAnnotation     = "sandbox.runtime.uid"
	fusePrepareAttemptAnnotation = "sandbox.prepare.attempt"
	fuseNetworkAttemptAnnotation = "sandbox.network.attempt"
	fuseEndpointCIDRsAnnotation  = "sandbox.system.endpoint-cidrs"
)

var (
	permanentlyDeniedCIDRs = []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4", "::/128", "::1/128", "fe80::/10", "ff00::/8"}
	privateCIDRs           = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}
)

// buildSystemEgressPolicy renders the standard-NetworkPolicy variant of the
// immutable FUSE system egress contract. It deliberately accepts CIDR mode
// only; FQDN policy requires Cilium's DNS-aware policy type.
func buildSystemEgressPolicy(namespace, instance string, spec runtime.SystemEgressSpec) (*networkingv1.NetworkPolicy, error) {
	dnsCIDRs, endpointCIDRs, endpointFQDNs, endpointPorts, err := validateSystemEgressPolicySpec(instance, spec)
	if err != nil {
		return nil, err
	}
	if spec.Mode != runtime.SystemEgressCIDR {
		return nil, fmt.Errorf("standard NetworkPolicy requires CIDR system egress")
	}
	if len(endpointCIDRs) == 0 {
		return nil, fmt.Errorf("CIDR system egress requires endpoint CIDRs")
	}
	_ = endpointFQDNs // An inactive approved set remains part of PoolKey only.

	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	egress := make([]networkingv1.NetworkPolicyEgressRule, 0, 2)
	if len(dnsCIDRs) != 0 {
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: cidrPeers(dnsCIDRs),
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: portValue(53)},
				{Protocol: &tcp, Port: portValue(53)},
			},
		})
	}
	egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: cidrPeers(endpointCIDRs), Ports: tcpPorts(endpointPorts)})
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fuseSystemPolicyPrefix + instance,
			Namespace: namespace,
			Labels: map[string]string{
				"sandbox.managed":       "true",
				"sandbox.pool.instance": instance,
				"sandbox.policy.role":   "system",
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"sandbox.pool.instance": instance}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}, nil
}

// buildCiliumSystemEgressPolicy renders exact endpoint names. hostAliases do
// not traverse Cilium's DNS proxy, so only those exact approved host IPs are
// added as /32 or /128 destinations; the wider approval envelope is not.
func buildCiliumSystemEgressPolicy(namespace, instance string, spec runtime.SystemEgressSpec, hostIPs []string) (*unstructured.Unstructured, error) {
	dnsCIDRs, endpointCIDRs, endpointFQDNs, endpointPorts, err := validateSystemEgressPolicySpec(instance, spec)
	if err != nil {
		return nil, err
	}
	if spec.Mode != runtime.SystemEgressCiliumFQDN {
		return nil, fmt.Errorf("CiliumNetworkPolicy requires FQDN system egress")
	}
	if len(endpointFQDNs) == 0 {
		return nil, fmt.Errorf("Cilium FQDN system egress requires endpoint FQDNs")
	}
	encodedEndpointCIDRs, err := json.Marshal(endpointCIDRs)
	if err != nil {
		return nil, fmt.Errorf("encode approved endpoint CIDRs")
	}
	approved := make([]netip.Prefix, 0, len(endpointCIDRs))
	for _, raw := range endpointCIDRs {
		prefix, _ := netip.ParsePrefix(raw)
		approved = append(approved, prefix)
	}
	aliasPrefixes := make([]string, 0, len(hostIPs))
	for _, raw := range canonicalStringSet(hostIPs) {
		addr, parseErr := netip.ParseAddr(raw)
		if parseErr != nil || addr.Is4In6() || addr.Zone() != "" || addr.String() != raw {
			return nil, fmt.Errorf("Cilium endpoint host alias must be a canonical literal IP")
		}
		allowed := false
		for _, prefix := range approved {
			allowed = allowed || prefix.Contains(addr)
		}
		if !allowed {
			return nil, fmt.Errorf("Cilium endpoint host alias is outside approved endpoint CIDRs")
		}
		aliasPrefixes = append(aliasPrefixes, netip.PrefixFrom(addr, addr.BitLen()).String())
	}

	dnsNames := make([]any, 0, len(endpointFQDNs))
	toFQDNs := make([]any, 0, len(endpointFQDNs))
	for _, name := range endpointFQDNs {
		dnsNames = append(dnsNames, map[string]any{"matchName": name})
		toFQDNs = append(toFQDNs, map[string]any{"matchName": name})
	}
	egress := []any{
		map[string]any{
			"toCIDRSet": ciliumCIDRSet(dnsCIDRs),
			"toPorts": []any{map[string]any{
				"ports": []any{
					map[string]any{"port": "53", "protocol": "UDP"},
					map[string]any{"port": "53", "protocol": "TCP"},
				},
				"rules": map[string]any{"dns": dnsNames},
			}},
		},
		map[string]any{
			"toFQDNs": toFQDNs,
			"toPorts": []any{map[string]any{"ports": ciliumTCPPorts(endpointPorts)}},
		},
	}
	if len(aliasPrefixes) > 0 {
		egress = append(egress, map[string]any{
			"toCIDRSet": ciliumCIDRSet(aliasPrefixes),
			"toPorts":   []any{map[string]any{"ports": ciliumTCPPorts(endpointPorts)}},
		})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata": map[string]any{
			"name": fuseSystemPolicyPrefix + instance, "namespace": namespace,
			"labels": map[string]any{
				"sandbox.managed": "true", "sandbox.pool.instance": instance, "sandbox.policy.role": "system",
			},
			"annotations": map[string]any{fuseEndpointCIDRsAnnotation: string(encodedEndpointCIDRs)},
		},
		"spec": map[string]any{
			"endpointSelector": map[string]any{"matchLabels": map[string]any{"sandbox.pool.instance": instance}},
			"egress":           egress,
		},
	}}, nil
}

func validateSystemEgressPolicySpec(instance string, spec runtime.SystemEgressSpec) ([]string, []string, []string, []int32, error) {
	if errs := validation.IsValidLabelValue(instance); len(errs) != 0 || instance == "" ||
		len(validation.IsDNS1123Subdomain(instance)) != 0 || len(validation.IsDNS1123Subdomain(fuseSystemPolicyPrefix+instance)) != 0 {
		return nil, nil, nil, nil, fmt.Errorf("FUSE pool instance is invalid")
	}
	if spec.ProxyURL != "" {
		return nil, nil, nil, nil, fmt.Errorf("system egress proxy is unsupported")
	}
	dnsPorts := canonicalPortSet(spec.DNSPorts)
	endpointPorts := canonicalPortSet(spec.EndpointPorts)
	if len(endpointPorts) == 0 {
		return nil, nil, nil, nil, fmt.Errorf("system egress endpoint ports must not be empty")
	}
	for _, port := range endpointPorts {
		if port < 1 || port > 65535 {
			return nil, nil, nil, nil, fmt.Errorf("system egress endpoint port is invalid")
		}
	}
	dnsCIDRs, err := canonicalHostCIDRs(spec.DNSCIDRs, len(spec.Hosts) == 0)
	if len(spec.Hosts) == 0 {
		if err != nil || len(dnsCIDRs) == 0 || len(dnsPorts) != 1 || dnsPorts[0] != 53 {
			return nil, nil, nil, nil, fmt.Errorf("system egress DNS CIDRs must be canonical public host CIDRs and port 53 is required for legacy policy")
		}
	} else if err != nil || len(dnsCIDRs) == 0 || len(dnsCIDRs) > 3 || len(dnsPorts) != 1 || dnsPorts[0] != 53 {
		return nil, nil, nil, nil, fmt.Errorf("resolved system egress requires exact cluster DNS CIDRs and port 53")
	}
	endpointCIDRs, err := canonicalEndpointCIDRs(spec.EndpointCIDRs)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	endpointFQDNs := canonicalStringSet(spec.EndpointFQDNs)
	for _, name := range endpointFQDNs {
		_, isIP := netip.ParseAddr(name)
		if name == "" || isIP == nil || name != strings.ToLower(name) || strings.Contains(name, "*") || len(validation.IsDNS1123Subdomain(name)) != 0 {
			return nil, nil, nil, nil, fmt.Errorf("system egress endpoint FQDN must be exact and canonical")
		}
	}
	return dnsCIDRs, endpointCIDRs, endpointFQDNs, endpointPorts, nil
}

func canonicalHostCIDRs(values []string, publicOnly bool) ([]string, error) {
	result := canonicalStringSet(values)
	for _, raw := range result {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.String() != raw || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" || prefix.Bits() != prefix.Addr().BitLen() {
			return nil, fmt.Errorf("CIDR is not a canonical host prefix")
		}
		addr := prefix.Addr()
		if publicOnly && !runtime.IsPublicDNSAddress(addr) {
			return nil, fmt.Errorf("DNS resolver is not public")
		}
	}
	return result, nil
}

func canonicalNameserverHostCIDRs(values []string) ([]string, error) {
	values = canonicalStringSet(values)
	if len(values) < 1 || len(values) > 3 {
		return nil, fmt.Errorf("DNS nameserver set must contain between one and three addresses")
	}
	result := make([]string, 0, len(values))
	for _, raw := range values {
		addr, err := netip.ParseAddr(raw)
		if err != nil || addr.String() != raw || addr.Is4In6() || addr.Zone() != "" {
			return nil, fmt.Errorf("DNS nameserver must be a canonical literal IP")
		}
		result = append(result, netip.PrefixFrom(addr, addr.BitLen()).String())
	}
	return canonicalHostCIDRs(result, false)
}

func canonicalEndpointCIDRs(values []string) ([]string, error) {
	result := canonicalStringSet(values)
	for _, raw := range result {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.String() != raw || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" || prefix != prefix.Masked() {
			return nil, fmt.Errorf("system egress endpoint CIDR must be canonical")
		}
		addr := prefix.Addr()
		if addr.IsLoopback() || addr.IsLinkLocalUnicast() || addr.IsMulticast() || addr.IsUnspecified() {
			return nil, fmt.Errorf("system egress endpoint CIDR is forbidden")
		}
	}
	return result, nil
}

func canonicalPortSet(values []int32) []int32 {
	copyValues := append([]int32(nil), values...)
	sort.Slice(copyValues, func(i, j int) bool { return copyValues[i] < copyValues[j] })
	result := copyValues[:0]
	for _, value := range copyValues {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func cidrPeers(cidrs []string) []networkingv1.NetworkPolicyPeer {
	peers := make([]networkingv1.NetworkPolicyPeer, 0, len(cidrs))
	for _, cidr := range cidrs {
		peers = append(peers, networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr}})
	}
	return peers
}

func portValue(port int32) *intstr.IntOrString {
	value := intstr.FromInt32(port)
	return &value
}

func tcpPorts(ports []int32) []networkingv1.NetworkPolicyPort {
	tcp := corev1.ProtocolTCP
	result := make([]networkingv1.NetworkPolicyPort, 0, len(ports))
	for _, port := range ports {
		result = append(result, networkingv1.NetworkPolicyPort{Protocol: &tcp, Port: portValue(port)})
	}
	return result
}

func ciliumCIDRSet(cidrs []string) []any {
	result := make([]any, 0, len(cidrs))
	for _, cidr := range cidrs {
		result = append(result, map[string]any{"cidr": cidr})
	}
	return result
}

func ciliumTCPPorts(ports []int32) []any {
	result := make([]any, 0, len(ports))
	for _, port := range ports {
		result = append(result, map[string]any{"port": strconv.FormatInt(int64(port), 10), "protocol": "TCP"})
	}
	return result
}

func buildFUSEUserNetworkPolicy(namespace, instance, runtimeUID string, enabled bool, whitelist []string, blockPrivate bool, dnsCIDRs []string, servicePeers ...[]networkingv1.NetworkPolicyPeer) (*networkingv1.NetworkPolicy, error) {
	if errs := validation.IsValidLabelValue(instance); len(errs) != 0 || instance == "" ||
		len(validation.IsDNS1123Subdomain(instance)) != 0 || len(validation.IsDNS1123Subdomain(fuseUserPolicyPrefix+instance)) != 0 {
		return nil, fmt.Errorf("FUSE pool instance is invalid")
	}
	if runtimeUID == "" || len(runtimeUID) > 1024 {
		return nil, fmt.Errorf("FUSE runtime UID is invalid")
	}
	approvedDNS, err := canonicalHostCIDRs(dnsCIDRs, false)
	if err != nil || len(approvedDNS) == 0 || len(approvedDNS) > 3 {
		return nil, fmt.Errorf("FUSE user policy DNS CIDRs must be exact cluster resolver host CIDRs")
	}
	resolvedCIDRs, err := resolveToCIDRs(whitelist)
	if err != nil {
		return nil, err
	}
	resolvedCIDRs, err = canonicalFUSEUserCIDRs(resolvedCIDRs)
	if err != nil {
		return nil, err
	}
	var egress []networkingv1.NetworkPolicyEgressRule
	if enabled {
		udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: cidrPeers(approvedDNS),
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: portValue(53)},
				{Protocol: &tcp, Port: portValue(53)},
			},
		})
		switch {
		case blockPrivate:
			for _, cidr := range resolvedCIDRs {
				egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: cidrPeers([]string{cidr})})
			}
			ipv4 := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{
				CIDR: "0.0.0.0/0", Except: []string{"0.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4"},
			}}
			ipv6 := networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{
				CIDR: "::/0", Except: []string{"::/128", "fc00::/7", "::1/128", "fe80::/10", "ff00::/8"},
			}}
			egress = append(egress,
				networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{ipv4}},
				networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{ipv6}},
			)
		case len(resolvedCIDRs) > 0 || hasServiceTargetPeers(servicePeers):
			for _, cidr := range resolvedCIDRs {
				egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: cidrPeers([]string{cidr})})
			}
		default:
			egress = append(egress,
				networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: []string{"0.0.0.0/8", "127.0.0.0/8", "169.254.0.0/16", "224.0.0.0/4"}}}}},
				networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{IPBlock: &networkingv1.IPBlock{CIDR: "::/0", Except: []string{"::/128", "fe80::/10", "fc00::/7", "::1/128", "ff00::/8"}}}}},
			)
		}
	}
	egress = appendServiceTargetRules(egress, enabled, servicePeers)
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: fuseUserPolicyPrefix + instance, Namespace: namespace,
			Labels:      map[string]string{"sandbox.managed": "true", "sandbox.pool.instance": instance, "sandbox.policy.role": "user"},
			Annotations: map[string]string{fuseRuntimeUIDAnnotation: runtimeUID},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"sandbox.pool.instance": instance}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}, nil
}

func canonicalFUSEUserCIDRs(values []string) ([]string, error) {
	result := canonicalStringSet(values)
	for _, raw := range result {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Addr().Is4In6() || prefix.Addr().Zone() != "" || prefix.Masked().String() != raw {
			return nil, fmt.Errorf("FUSE user destination must be a canonical CIDR")
		}
		for _, deniedRaw := range permanentlyDeniedCIDRs {
			denied, _ := netip.ParsePrefix(deniedRaw)
			if prefixesOverlap(prefix, denied) {
				return nil, fmt.Errorf("FUSE user destination overlaps a permanently denied range")
			}
		}
	}
	return result, nil
}

func prefixesOverlap(left, right netip.Prefix) bool {
	return left.Addr().BitLen() == right.Addr().BitLen() && (left.Contains(right.Addr()) || right.Contains(left.Addr()))
}

// CIDR intersections are either disjoint or one canonical prefix contains the
// other. A parent/equal allowance covers the entire deny, so omit that private
// deny rule; only narrower intersections belong in Cilium's except list.
func ciliumPrivateCIDRDenyRule(deniedRaw string, exceptions []string) (map[string]any, bool) {
	denied, _ := netip.ParsePrefix(deniedRaw)
	except := make([]any, 0)
	for _, allowedRaw := range exceptions {
		allowed, _ := netip.ParsePrefix(allowedRaw)
		if !prefixesOverlap(denied, allowed) {
			continue
		}
		if allowed.Bits() <= denied.Bits() {
			return nil, false
		}
		except = append(except, allowedRaw)
	}
	rule := map[string]any{"cidr": deniedRaw}
	if len(except) != 0 {
		rule["except"] = except
	}
	return rule, true
}

func buildFUSECiliumUserDenyPolicy(namespace, instance, runtimeUID string, blockPrivate bool, privateExceptions []string) (*unstructured.Unstructured, error) {
	if instance == "" || len(validation.IsDNS1123Subdomain(fuseUserDenyPolicyPrefix+instance)) != 0 || runtimeUID == "" || len(runtimeUID) > 1024 {
		return nil, fmt.Errorf("FUSE Cilium user deny identity is invalid")
	}
	exceptions, err := canonicalFUSEUserCIDRs(privateExceptions)
	if err != nil {
		return nil, err
	}
	denyRules := make([]any, 0, len(permanentlyDeniedCIDRs)+len(privateCIDRs))
	for _, cidr := range permanentlyDeniedCIDRs {
		denyRules = append(denyRules, map[string]any{"cidr": cidr})
	}
	if blockPrivate {
		for _, deniedRaw := range privateCIDRs {
			if rule, keep := ciliumPrivateCIDRDenyRule(deniedRaw, exceptions); keep {
				denyRules = append(denyRules, rule)
			}
		}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata": map[string]any{
			"name": fuseUserDenyPolicyPrefix + instance, "namespace": namespace,
			"labels": map[string]any{
				"sandbox.managed": "true", "sandbox.pool.instance": instance, "sandbox.policy.role": "user-deny",
			},
			"annotations": map[string]any{fuseRuntimeUIDAnnotation: runtimeUID},
		},
		"spec": map[string]any{
			"endpointSelector": map[string]any{"matchLabels": map[string]any{"sandbox.pool.instance": instance}},
			"egressDeny":       []any{map[string]any{"toCIDRSet": denyRules}},
		},
	}}, nil
}

func ciliumUserPolicyIntentMatches(current, desired *unstructured.Unstructured) bool {
	if current == nil || desired == nil || !ciliumUserPolicyOwnershipMatches(current, desired.GetLabels()["sandbox.pool.instance"]) ||
		!reflect.DeepEqual(current.GetAnnotations(), desired.GetAnnotations()) {
		return false
	}
	return reflect.DeepEqual(current.Object["spec"], desired.Object["spec"])
}

func ciliumUserPolicyOwnershipMatches(current *unstructured.Unstructured, instance string) bool {
	if current == nil || current.GetLabels()["sandbox.managed"] != "true" || current.GetLabels()["sandbox.pool.instance"] != instance ||
		current.GetLabels()["sandbox.policy.role"] != "user-deny" {
		return false
	}
	selector, found, err := unstructured.NestedStringMap(current.Object, "spec", "endpointSelector", "matchLabels")
	return err == nil && found && reflect.DeepEqual(selector, map[string]string{"sandbox.pool.instance": instance})
}

func upsertExactCiliumUserPolicy(ctx context.Context, policies dynamic.ResourceInterface, desired *unstructured.Unstructured) error {
	current, err := policies.Get(ctx, desired.GetName(), metav1.GetOptions{})
	if errors.IsNotFound(err) {
		created, createErr := policies.Create(ctx, desired, metav1.CreateOptions{})
		if createErr == nil {
			if ciliumUserPolicyIntentMatches(created, desired) {
				return nil
			}
			cleanupCtx, cancel := networkCleanupContext(ctx)
			defer cancel()
			cleanupErr := deleteCiliumPolicyForNetworkAttempt(cleanupCtx, policies, desired.GetName(), desired.GetAnnotations()[fuseNetworkAttemptAnnotation])
			if cleanupErr != nil {
				return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("created Cilium user deny policy does not match requested intent"), cleanupErr)
			}
			return fmt.Errorf("created Cilium user deny policy does not match requested intent")
		}
		if errors.IsAlreadyExists(createErr) {
			return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("create Cilium user deny policy: %w", createErr))
		}
		if !mayVerifyAmbiguousCreate(createErr) {
			return fmt.Errorf("create Cilium user deny policy: %w", createErr)
		}
		verifyCtx, cancel := networkCleanupContext(ctx)
		defer cancel()
		verified, verifyErr := policies.Get(verifyCtx, desired.GetName(), metav1.GetOptions{})
		if verifyErr == nil && ciliumUserPolicyIntentMatches(verified, desired) {
			return nil
		}
		if verifyErr == nil && verified.GetAnnotations()[fuseNetworkAttemptAnnotation] == desired.GetAnnotations()[fuseNetworkAttemptAnnotation] {
			cleanupErr := deleteCiliumPolicyForNetworkAttempt(verifyCtx, policies, desired.GetName(), desired.GetAnnotations()[fuseNetworkAttemptAnnotation])
			if cleanupErr == nil {
				return fmt.Errorf("create Cilium user deny policy: %w", createErr)
			}
			return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("create Cilium user deny policy: %w", createErr), cleanupErr)
		}
		if errors.IsNotFound(verifyErr) {
			return fmt.Errorf("create Cilium user deny policy: %w", createErr)
		}
		return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("create Cilium user deny policy: %w", createErr), verifyErr)
	}
	if err != nil {
		return fmt.Errorf("get Cilium user deny policy: %w", err)
	}
	if current.GetUID() == "" {
		return fmt.Errorf("Cilium user deny policy has no immutable UID")
	}
	if !ciliumUserPolicyIntentMatches(current, desired) &&
		(current.GetLabels()["sandbox.managed"] != "true" || current.GetLabels()["sandbox.pool.instance"] != desired.GetLabels()["sandbox.pool.instance"] ||
			current.GetLabels()["sandbox.policy.role"] != "user-deny" || current.GetAnnotations()[fuseRuntimeUIDAnnotation] != desired.GetAnnotations()[fuseRuntimeUIDAnnotation]) {
		return runtime.ErrInvalidRuntimeRef
	}
	updated := desired.DeepCopy()
	updated.SetResourceVersion(current.GetResourceVersion())
	updated.SetUID(current.GetUID())
	result, updateErr := policies.Update(ctx, updated, metav1.UpdateOptions{})
	if updateErr == nil {
		if result.GetUID() == current.GetUID() && ciliumUserPolicyIntentMatches(result, updated) {
			return nil
		}
		return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("updated Cilium user deny policy does not match requested intent"))
	}
	verifyCtx, cancel := networkCleanupContext(ctx)
	defer cancel()
	verified, verifyErr := policies.Get(verifyCtx, desired.GetName(), metav1.GetOptions{})
	if verifyErr == nil && verified.GetUID() == current.GetUID() && ciliumUserPolicyIntentMatches(verified, updated) {
		return nil
	}
	return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("update Cilium user deny policy: %w", updateErr), verifyErr)
}

func networkCleanupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), defaultKubernetesControlTimeout)
}

func upsertExactNetworkPolicy(ctx context.Context, client kubernetes.Interface, desired *networkingv1.NetworkPolicy) error {
	policies := client.NetworkingV1().NetworkPolicies(desired.Namespace)
	current, err := policies.Get(ctx, desired.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		created, createErr := policies.Create(ctx, desired, metav1.CreateOptions{})
		if createErr == nil {
			if networkPolicyIntentMatches(created, desired) {
				return nil
			}
			cleanupCtx, cancel := networkCleanupContext(ctx)
			defer cancel()
			cleanupErr := deleteNetworkPolicyForNetworkAttempt(cleanupCtx, policies, desired.Name, desired.Annotations[fuseNetworkAttemptAnnotation])
			if cleanupErr != nil {
				return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("created exact network policy does not match requested intent"), cleanupErr)
			}
			return fmt.Errorf("created exact network policy does not match requested intent")
		}
		if errors.IsAlreadyExists(createErr) {
			return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("create exact network policy: %w", createErr))
		}
		if !mayVerifyAmbiguousCreate(createErr) {
			return fmt.Errorf("create exact network policy: %w", createErr)
		}
		verifyCtx, cancel := networkCleanupContext(ctx)
		defer cancel()
		verified, verifyErr := policies.Get(verifyCtx, desired.Name, metav1.GetOptions{})
		if verifyErr == nil && networkPolicyIntentMatches(verified, desired) {
			return nil
		}
		if verifyErr == nil && verified.Annotations[fuseNetworkAttemptAnnotation] == desired.Annotations[fuseNetworkAttemptAnnotation] {
			cleanupErr := deleteNetworkPolicyForNetworkAttempt(verifyCtx, policies, desired.Name, desired.Annotations[fuseNetworkAttemptAnnotation])
			if cleanupErr == nil {
				return fmt.Errorf("create exact network policy: %w", createErr)
			}
			return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("create exact network policy: %w", createErr), cleanupErr)
		}
		if errors.IsNotFound(verifyErr) {
			return fmt.Errorf("create exact network policy: %w", createErr)
		}
		return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("create exact network policy: %w", createErr), verifyErr)
	}
	if err != nil {
		return fmt.Errorf("get exact network policy: %w", err)
	}
	if !networkPolicyOwnershipMatches(current, desired.Labels["sandbox.pool.instance"], desired.Labels["sandbox.policy.role"]) {
		return fmt.Errorf("network policy ownership does not match")
	}
	if current.Annotations[fuseRuntimeUIDAnnotation] != desired.Annotations[fuseRuntimeUIDAnnotation] {
		return runtime.ErrInvalidRuntimeRef
	}
	desired = desired.DeepCopy()
	desired.ResourceVersion = current.ResourceVersion
	updated, updateErr := policies.Update(ctx, desired, metav1.UpdateOptions{})
	if updateErr == nil {
		if updated.UID == current.UID && networkPolicyIntentMatches(updated, desired) {
			return nil
		}
		return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("updated exact network policy does not match requested intent"))
	}
	verifyCtx, cancel := networkCleanupContext(ctx)
	defer cancel()
	verified, verifyErr := policies.Get(verifyCtx, desired.Name, metav1.GetOptions{})
	if verifyErr == nil && verified.UID == current.UID && networkPolicyIntentMatches(verified, desired) {
		return nil
	}
	return stderrors.Join(runtime.ErrFUSENetworkStateUncertain, fmt.Errorf("update exact network policy: %w", updateErr), verifyErr)
}

func deleteNetworkPolicyForNetworkAttempt(ctx context.Context, policies typednetworkingv1.NetworkPolicyInterface, name, attempt string) error {
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read network update attempt for cleanup: %w", err)
	}
	if attempt == "" || current.Annotations[fuseNetworkAttemptAnnotation] != attempt || current.UID == "" || current.ResourceVersion == "" {
		return fmt.Errorf("network update attempt no longer owns policy")
	}
	uid, resourceVersion := current.UID, current.ResourceVersion
	deleteErr := policies.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}})
	verified, verifyErr := policies.Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(verifyErr) || (verifyErr == nil && verified.UID != uid) {
		return nil
	}
	if deleteErr != nil {
		return fmt.Errorf("delete network update attempt: %w", deleteErr)
	}
	if verifyErr != nil {
		return fmt.Errorf("verify network update attempt cleanup: %w", verifyErr)
	}
	return fmt.Errorf("network update attempt cleanup is unconfirmed")
}

func deleteCiliumPolicyForNetworkAttempt(ctx context.Context, policies dynamic.ResourceInterface, name, attempt string) error {
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read Cilium network update attempt for cleanup: %w", err)
	}
	if attempt == "" || current.GetAnnotations()[fuseNetworkAttemptAnnotation] != attempt || current.GetUID() == "" || current.GetResourceVersion() == "" {
		return fmt.Errorf("network update attempt no longer owns Cilium policy")
	}
	uid, resourceVersion := current.GetUID(), current.GetResourceVersion()
	deleteErr := policies.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}})
	verified, verifyErr := policies.Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(verifyErr) || (verifyErr == nil && verified.GetUID() != uid) {
		return nil
	}
	if deleteErr != nil {
		return fmt.Errorf("delete Cilium network update attempt: %w", deleteErr)
	}
	if verifyErr != nil {
		return fmt.Errorf("verify Cilium network update attempt cleanup: %w", verifyErr)
	}
	return fmt.Errorf("Cilium network update attempt cleanup is unconfirmed")
}

func networkPolicyIntentMatches(current, desired *networkingv1.NetworkPolicy) bool {
	return networkPolicyOwnershipMatches(current, desired.Labels["sandbox.pool.instance"], desired.Labels["sandbox.policy.role"]) &&
		reflect.DeepEqual(current.Annotations, desired.Annotations) &&
		reflect.DeepEqual(current.Spec, desired.Spec)
}

func networkPolicyOwnershipMatches(policy *networkingv1.NetworkPolicy, instance, role string) bool {
	return policy != nil && policy.Labels["sandbox.managed"] == "true" &&
		policy.Labels["sandbox.pool.instance"] == instance && policy.Labels["sandbox.policy.role"] == role &&
		reflect.DeepEqual(policy.Spec.PodSelector.MatchLabels, map[string]string{"sandbox.pool.instance": instance})
}

func deleteExactNetworkPolicy(ctx context.Context, client kubernetes.Interface, namespace, name, instance, role, runtimeUID, prepareAttempt string, allowUnbound bool) error {
	policies := client.NetworkingV1().NetworkPolicies(namespace)
	current, err := policies.Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get exact network policy for deletion: %w", err)
	}
	if !networkPolicyOwnershipMatches(current, instance, role) {
		return fmt.Errorf("network policy ownership does not match")
	}
	boundUID := current.Annotations[fuseRuntimeUIDAnnotation]
	if boundUID != runtimeUID && !(allowUnbound && boundUID == "") {
		return runtime.ErrInvalidRuntimeRef
	}
	if prepareAttempt != "" && current.Annotations[fusePrepareAttemptAnnotation] != prepareAttempt {
		return runtime.ErrInvalidRuntimeRef
	}
	if current.UID == "" {
		return fmt.Errorf("network policy has no immutable UID")
	}
	originalUID := current.UID
	options := metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &originalUID}}
	deleteErr := policies.Delete(ctx, name, options)
	verified, verifyErr := policies.Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(verifyErr) || (verifyErr == nil && verified.UID != originalUID) {
		return nil
	}
	if deleteErr != nil {
		return fmt.Errorf("delete exact network policy: %w", deleteErr)
	}
	if verifyErr != nil {
		return fmt.Errorf("verify exact network policy deletion: %w", verifyErr)
	}
	return fmt.Errorf("exact network policy deletion is unconfirmed")
}

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

// applyCiliumPrivateDeny creates/updates explicit external RFC1918/link-local
// egress deny rules. CIDR rules do not select Cilium-managed Pod endpoints;
// managed cluster destinations require namespace-scoped endpoint selectors.
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
