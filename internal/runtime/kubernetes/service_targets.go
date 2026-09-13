package kubernetes

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/goairix/sandbox/internal/runtime"
)

type networkTargets struct {
	cidrs []string
	peers []networkingv1.NetworkPolicyPeer
}

type networkRangeOptions struct {
	cilium         bool
	dynamic        dynamic.Interface
	pods, services []string
}

func (r *Runtime) resolveNetworkTargets(ctx context.Context, entries []string) (networkTargets, error) {
	return resolveNetworkTargets(ctx, r.client, entries, networkRangeOptions{cilium: r.hasCilium, dynamic: r.dynClient, pods: r.networkPodCIDRs, services: r.networkServiceCIDRs})
}

// Resolve user targets before any policy mutation. Kubernetes destinations must
// use identities: CIDR rules do not select Cilium-managed Pod endpoints.
func resolveNetworkTargets(ctx context.Context, client kubernetes.Interface, entries []string, rangeOptions ...networkRangeOptions) (networkTargets, error) {
	ctx, cancel := context.WithTimeout(ctx, networkInventoryTimeout)
	defer cancel()
	var result networkTargets
	if len(entries) > 256 {
		return result, invalidNetworkTarget("whitelist exceeds 256 targets")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var external []string
	for _, entry := range entries {
		if !strings.HasPrefix(entry, "k8s-service://") {
			if strings.Contains(entry, "://") {
				return result, invalidNetworkTarget("unsupported target scheme")
			}
			external = append(external, entry)
			continue
		}
		parts := strings.Split(strings.TrimPrefix(entry, "k8s-service://"), "/")
		if len(parts) != 2 || len(validation.IsDNS1123Label(parts[0])) != 0 || len(validation.IsDNS1123Label(parts[1])) != 0 {
			return result, invalidNetworkTarget("Service target requires k8s-service://namespace/name with DNS labels")
		}
		service, err := client.CoreV1().Services(parts[0]).Get(ctx, parts[1], metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return result, invalidNetworkTarget("Service target does not exist")
		}
		if err != nil {
			return result, fmt.Errorf("read Service network target: %w", err)
		}
		if service.Spec.Type == corev1.ServiceTypeExternalName || service.Spec.ClusterIP == corev1.ClusterIPNone || len(service.Spec.Selector) == 0 {
			return result, invalidNetworkTarget("Service target requires a non-headless selector-based Service")
		}
		labels := make(map[string]string, len(service.Spec.Selector))
		for key, value := range service.Spec.Selector {
			if len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
				return result, invalidNetworkTarget("Service selector is invalid")
			}
			labels[key] = value
		}
		result.peers = append(result.peers, networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": parts[0]}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: labels},
		})
	}
	cidrs, err := resolveUserTargetCIDRs(ctx, external)
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		return result, fmt.Errorf("%w: %v", runtime.ErrInvalidNetworkTarget, err)
	}
	// Keep the existing external CIDR syntax (including host bits) while
	// compiling a canonical network prefix for validation and policy output.
	for index, raw := range cidrs {
		if prefix, parseErr := netip.ParsePrefix(raw); parseErr == nil {
			cidrs[index] = prefix.Masked().String()
		}
	}
	cidrs, err = canonicalFUSEUserCIDRs(cidrs)
	if err != nil {
		return result, fmt.Errorf("%w: %v", runtime.ErrInvalidNetworkTarget, err)
	}
	if len(cidrs) > 0 {
		ranges, err := clusterDestinationRanges(ctx, client, rangeOptions...)
		if err != nil {
			return result, err
		}
		for _, raw := range cidrs {
			prefix, _ := netip.ParsePrefix(raw)
			for _, cluster := range ranges {
				if prefixesOverlap(prefix, cluster) {
					return result, invalidNetworkTarget("CIDR overlaps an in-cluster Pod or Service range; use k8s-service://namespace/name")
				}
			}
		}
	}
	result.cidrs = cidrs
	if err := ctx.Err(); err != nil {
		return networkTargets{}, err
	}
	return result, nil
}

func resolveUserTargetCIDRs(ctx context.Context, entries []string) ([]string, error) {
	var cidrs []string
	for _, entry := range entries {
		if ip, err := netip.ParseAddr(entry); err == nil {
			cidrs = append(cidrs, netip.PrefixFrom(ip, ip.BitLen()).String())
		} else if _, err := netip.ParsePrefix(entry); err == nil {
			cidrs = append(cidrs, entry)
		} else {
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", entry)
			if err != nil {
				return nil, fmt.Errorf("resolve whitelist domain %q: %w", entry, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("whitelist domain has no addresses")
			}
			for _, ip := range ips {
				ip = ip.Unmap()
				cidrs = append(cidrs, netip.PrefixFrom(ip, ip.BitLen()).String())
			}
		}
	}
	return cidrs, nil
}

func invalidNetworkTarget(message string) error {
	return fmt.Errorf("%w: %s", runtime.ErrInvalidNetworkTarget, message)
}

const (
	networkInventoryTimeout   = 5 * time.Second
	networkInventoryPageSize  = 100
	networkInventoryMaxPages  = 10
	networkInventoryMaxItems  = 1000
	networkInventoryMaxRanges = 4096
)

var ciliumNodeGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnodes"}

func configuredNetworkRanges(pods, services []string) ([]netip.Prefix, error) {
	if len(pods) == 0 && len(services) == 0 {
		return nil, nil
	}
	if len(pods) == 0 || len(services) == 0 || len(pods) > 256 || len(services) > 256 {
		return nil, fmt.Errorf("authoritative Pod/Service CIDRs must both be complete nonempty sets, at most 256 each")
	}
	var ranges []netip.Prefix
	families := [2]map[int]bool{{}, {}}
	for index, set := range [][]string{pods, services} {
		for _, raw := range set {
			prefix, err := netip.ParsePrefix(raw)
			if err != nil || prefix.Addr().Is4In6() || prefix.Masked().String() != raw {
				return nil, fmt.Errorf("authoritative Pod/Service CIDRs require canonical prefixes")
			}
			ranges = append(ranges, prefix)
			families[index][prefix.Addr().BitLen()] = true
		}
	}
	if families[0][32] != families[1][32] || families[0][128] != families[1][128] {
		return nil, fmt.Errorf("authoritative Pod/Service CIDRs must cover matching address families")
	}
	return ranges, nil
}

// Only allocation ranges are read, never whole Pod/Service inventories. Every
// Cilium node must have allocated PodCIDRs, and ServiceCIDRs must be known.
// Page/time/item bounds fail closed instead of truncating coverage.
func clusterDestinationRanges(ctx context.Context, client kubernetes.Interface, rangeOptions ...networkRangeOptions) ([]netip.Prefix, error) {
	var options networkRangeOptions
	if len(rangeOptions) > 0 {
		options = rangeOptions[0]
	}
	configured, err := configuredNetworkRanges(options.pods, options.services)
	if err != nil || configured != nil {
		return configured, err
	}
	ctx, cancel := context.WithTimeout(ctx, networkInventoryTimeout)
	defer cancel()
	var ranges []netip.Prefix
	add := func(raw string) error {
		if raw == "" {
			return nil
		}
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Addr().Is4In6() {
			return fmt.Errorf("invalid authoritative allocation CIDR")
		}
		ranges = append(ranges, prefix.Masked())
		if len(ranges) > networkInventoryMaxRanges {
			return fmt.Errorf("network allocation range limit exceeded")
		}
		return nil
	}
	var nodes []corev1.Node
	continuation := ""
	for page := 0; page < networkInventoryMaxPages; page++ {
		list, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{Limit: networkInventoryPageSize, Continue: continuation})
		if err != nil {
			return nil, fmt.Errorf("inventory Node PodCIDRs: %w", err)
		}
		nodes = append(nodes, list.Items...)
		if len(nodes) > networkInventoryMaxItems {
			return nil, fmt.Errorf("network Node inventory limit exceeded")
		}
		continuation = list.Continue
		if continuation == "" {
			break
		}
		if page == networkInventoryMaxPages-1 {
			return nil, fmt.Errorf("network Node inventory page limit exceeded")
		}
	}
	missing := map[string]bool{}
	nodeFamilies := map[string]map[int]bool{}
	for _, node := range nodes {
		nodeFamilies[node.Name] = map[int]bool{}
		count := 0
		for _, raw := range canonicalStringSet(append([]string{node.Spec.PodCIDR}, node.Spec.PodCIDRs...)) {
			if raw != "" {
				if err := add(raw); err != nil {
					return nil, err
				}
				count++
				prefix, _ := netip.ParsePrefix(raw)
				nodeFamilies[node.Name][prefix.Addr().BitLen()] = true
			}
		}
		if count == 0 {
			missing[node.Name] = true
		}
		for _, address := range node.Status.Addresses {
			if ip, err := netip.ParseAddr(address.Address); err == nil {
				if err := add(netip.PrefixFrom(ip, ip.BitLen()).String()); err != nil {
					return nil, err
				}
			}
		}
	}
	if options.cilium && len(nodes) == 0 {
		return nil, fmt.Errorf("authoritative Pod allocation inventory has no Nodes")
	}
	if options.cilium && len(missing) > 0 {
		if options.dynamic == nil {
			return nil, fmt.Errorf("CiliumNode allocation inventory is unavailable")
		}
		continuation = ""
		count := 0
		for page := 0; page < networkInventoryMaxPages; page++ {
			list, err := options.dynamic.Resource(ciliumNodeGVR).List(ctx, metav1.ListOptions{Limit: networkInventoryPageSize, Continue: continuation})
			if err != nil {
				return nil, fmt.Errorf("inventory CiliumNode PodCIDRs: %w", err)
			}
			count += len(list.Items)
			if count > networkInventoryMaxItems {
				return nil, fmt.Errorf("CiliumNode inventory limit exceeded")
			}
			for _, node := range list.Items {
				cidrs, found, err := unstructured.NestedStringSlice(node.Object, "spec", "ipam", "podCIDRs")
				if err != nil {
					return nil, fmt.Errorf("CiliumNode allocation data is invalid")
				}
				if !found || len(cidrs) == 0 {
					continue
				}
				for _, raw := range cidrs {
					if raw == "" {
						return nil, fmt.Errorf("CiliumNode allocation CIDR is empty")
					}
					if err := add(raw); err != nil {
						return nil, err
					}
					if families, ok := nodeFamilies[node.GetName()]; ok {
						prefix, _ := netip.ParsePrefix(raw)
						families[prefix.Addr().BitLen()] = true
					}
				}
				delete(missing, node.GetName())
			}
			continuation = list.GetContinue()
			if continuation == "" {
				break
			}
			if page == networkInventoryMaxPages-1 {
				return nil, fmt.Errorf("CiliumNode inventory page limit exceeded")
			}
		}
		if len(missing) > 0 {
			return nil, fmt.Errorf("authoritative PodCIDRs missing for %d Nodes; configure complete pod_cidrs and service_cidrs", len(missing))
		}
	}
	continuation = ""
	serviceCount := 0
	knownServiceRanges := 0
	serviceFamilies := map[int]bool{}
	for page := 0; page < networkInventoryMaxPages; page++ {
		list, err := client.NetworkingV1().ServiceCIDRs().List(ctx, metav1.ListOptions{Limit: networkInventoryPageSize, Continue: continuation})
		if err != nil {
			if !options.cilium && (apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err)) {
				break
			}
			return nil, fmt.Errorf("authoritative ServiceCIDR inventory unavailable; configure complete pod_cidrs and service_cidrs: %w", err)
		}
		serviceCount += len(list.Items)
		if serviceCount > networkInventoryMaxItems {
			return nil, fmt.Errorf("ServiceCIDR inventory limit exceeded")
		}
		for _, service := range list.Items {
			for _, raw := range service.Spec.CIDRs {
				if raw == "" {
					return nil, fmt.Errorf("ServiceCIDR allocation range is empty")
				}
				if err := add(raw); err != nil {
					return nil, err
				}
				knownServiceRanges++
				prefix, _ := netip.ParsePrefix(raw)
				serviceFamilies[prefix.Addr().BitLen()] = true
			}
		}
		continuation = list.Continue
		if continuation == "" {
			break
		}
		if page == networkInventoryMaxPages-1 {
			return nil, fmt.Errorf("ServiceCIDR inventory page limit exceeded")
		}
	}
	if options.cilium && knownServiceRanges == 0 {
		return nil, fmt.Errorf("authoritative ServiceCIDR inventory is empty; configure complete pod_cidrs and service_cidrs")
	}
	if options.cilium {
		for _, families := range nodeFamilies {
			for family := range serviceFamilies {
				if !families[family] {
					return nil, fmt.Errorf("authoritative Pod allocation address family is missing; configure complete pod_cidrs and service_cidrs")
				}
			}
		}
	}
	return ranges, nil
}

func appendServiceTargetRules(egress []networkingv1.NetworkPolicyEgressRule, enabled bool, peers [][]networkingv1.NetworkPolicyPeer) []networkingv1.NetworkPolicyEgressRule {
	if enabled {
		for _, set := range peers {
			for _, peer := range set {
				egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{peer}})
			}
		}
	}
	return egress
}

func hasServiceTargetPeers(peers [][]networkingv1.NetworkPolicyPeer) bool {
	for _, set := range peers {
		if len(set) > 0 {
			return true
		}
	}
	return false
}
