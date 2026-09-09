package runtime

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// LookupNetIPFunc matches net.Resolver.LookupIP and is injected so endpoint
// policy resolution is deterministic in tests.
type LookupNetIPFunc func(context.Context, string, string) ([]net.IP, error)

// EndpointAddressFamily is the address-family contract supported by a runtime.
type EndpointAddressFamily uint8

const (
	EndpointIPv4Only EndpointAddressFamily = iota + 1
	EndpointIPv4AndIPv6
)

// ResolveFUSEEndpointPolicy derives the exact, fail-closed destinations for a
// prepared FUSE runtime. The input is cloned and is never mutated.
func ResolveFUSEEndpointPolicy(ctx context.Context, spec *WorkspaceFUSESpec, lookup LookupNetIPFunc, family EndpointAddressFamily) (*WorkspaceFUSESpec, error) {
	if spec == nil {
		return nil, fmt.Errorf("resolve workspace FUSE endpoint: spec is nil")
	}
	if lookup == nil {
		resolver := net.DefaultResolver
		lookup = resolver.LookupIP
	}
	resolved := cloneWorkspaceFUSESpec(spec)
	endpoint, hosts, port, privateHTTP, err := canonicalFUSEEndpoint(spec)
	if err != nil {
		return nil, err
	}
	resolved.Endpoint = endpoint

	mappings := make([]EndpointHostMapping, 0, len(hosts))
	cidrSet := make(map[string]struct{})
	for _, host := range hosts {
		addresses, err := resolveEndpointHost(ctx, lookup, host, family)
		if err != nil {
			return nil, err
		}
		if privateHTTP {
			for _, address := range addresses {
				if !address.Is4() || !address.IsPrivate() {
					return nil, fmt.Errorf("resolve workspace FUSE endpoint %q: HTTP MinIO endpoint must resolve only to private addresses", host)
				}
			}
		}
		ips := make([]string, 0, len(addresses))
		for _, address := range addresses {
			ips = append(ips, address.String())
			cidrSet[netip.PrefixFrom(address, address.BitLen()).String()] = struct{}{}
		}
		mappings = append(mappings, EndpointHostMapping{Host: host, IPs: ips})
	}
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].Host < mappings[j].Host })
	cidrs := make([]string, 0, len(cidrSet))
	for cidr := range cidrSet {
		cidrs = append(cidrs, cidr)
	}
	sort.Strings(cidrs)
	resolved.EndpointHostIPs = nil
	resolved.SystemEgress = SystemEgressSpec{
		Mode:          SystemEgressCIDR,
		Hosts:         mappings,
		EndpointCIDRs: cidrs,
		EndpointPorts: []int32{port},
	}
	return resolved, nil
}

func cloneWorkspaceFUSESpec(spec *WorkspaceFUSESpec) *WorkspaceFUSESpec {
	clone := *spec
	clone.EndpointHostIPs = append([]string(nil), spec.EndpointHostIPs...)
	clone.SystemEgress.DNSCIDRs = append([]string(nil), spec.SystemEgress.DNSCIDRs...)
	clone.SystemEgress.DNSPorts = append([]int32(nil), spec.SystemEgress.DNSPorts...)
	clone.SystemEgress.EndpointCIDRs = append([]string(nil), spec.SystemEgress.EndpointCIDRs...)
	clone.SystemEgress.EndpointFQDNs = append([]string(nil), spec.SystemEgress.EndpointFQDNs...)
	clone.SystemEgress.EndpointPorts = append([]int32(nil), spec.SystemEgress.EndpointPorts...)
	clone.SystemEgress.Hosts = make([]EndpointHostMapping, len(spec.SystemEgress.Hosts))
	for index, mapping := range spec.SystemEgress.Hosts {
		clone.SystemEgress.Hosts[index] = EndpointHostMapping{Host: mapping.Host, IPs: append([]string(nil), mapping.IPs...)}
	}
	return &clone
}

func canonicalFUSEEndpoint(spec *WorkspaceFUSESpec) (string, []string, int32, bool, error) {
	const invalid = "workspace FUSE endpoint is invalid"
	var parsed *url.URL
	var err error
	switch spec.Provider {
	case "minio":
		if strings.Contains(spec.Endpoint, "://") {
			return "", nil, 0, false, fmt.Errorf("%s", invalid)
		}
		scheme := "http"
		if spec.UseSSL {
			scheme = "https"
		}
		parsed, err = url.Parse(scheme + "://" + spec.Endpoint)
	case "obs":
		if !spec.UseSSL {
			return "", nil, 0, false, fmt.Errorf("workspace FUSE OBS endpoint requires HTTPS")
		}
		parsed, err = url.Parse(spec.Endpoint)
		if err == nil && parsed.Scheme != "https" {
			err = fmt.Errorf("OBS endpoint must use HTTPS")
		}
	default:
		return "", nil, 0, false, fmt.Errorf("workspace FUSE provider is invalid")
	}
	if err != nil || parsed == nil || parsed.Host == "" || strings.HasSuffix(parsed.Host, ":") || parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", nil, 0, false, fmt.Errorf("%s", invalid)
	}
	host := parsed.Hostname()
	if !canonicalEndpointHost(host) {
		return "", nil, 0, false, fmt.Errorf("%s", invalid)
	}
	port := int32(80)
	if parsed.Scheme == "https" {
		port = 443
	}
	if rawPort := parsed.Port(); rawPort != "" {
		value, parseErr := strconv.Atoi(rawPort)
		if parseErr != nil || value < 1 || value > 65535 || strconv.Itoa(value) != rawPort {
			return "", nil, 0, false, fmt.Errorf("%s", invalid)
		}
		port = int32(value)
	}
	hosts := []string{host}
	if spec.Provider == "obs" {
		if !canonicalEndpointHost(spec.Bucket) || strings.Contains(spec.Bucket, ".") {
			return "", nil, 0, false, fmt.Errorf("workspace FUSE OBS bucket is invalid for virtual-host addressing")
		}
		hosts = append(hosts, spec.Bucket+"."+host)
	}
	sort.Strings(hosts)
	return parsed.String(), hosts, port, spec.Provider == "minio" && !spec.UseSSL, nil
}

func resolveEndpointHost(ctx context.Context, lookup LookupNetIPFunc, host string, family EndpointAddressFamily) ([]netip.Addr, error) {
	var raw []net.IP
	if literal, err := netip.ParseAddr(host); err == nil {
		raw = []net.IP{net.IP(literal.AsSlice())}
	} else {
		var err error
		raw, err = lookup(ctx, "ip", host)
		if err != nil {
			return nil, fmt.Errorf("resolve workspace FUSE endpoint %q: %w", host, err)
		}
	}
	set := make(map[netip.Addr]struct{})
	for _, value := range raw {
		address, ok := netip.AddrFromSlice(value)
		if !ok {
			continue
		}
		address = address.Unmap()
		if family == EndpointIPv4Only && !address.Is4() {
			continue
		}
		if forbiddenEndpointAddress(address) {
			return nil, fmt.Errorf("resolve workspace FUSE endpoint %q: forbidden address %s", host, address)
		}
		set[address] = struct{}{}
	}
	if len(set) == 0 {
		if family == EndpointIPv4Only {
			return nil, fmt.Errorf("resolve workspace FUSE endpoint %q: no usable IPv4 address", host)
		}
		return nil, fmt.Errorf("resolve workspace FUSE endpoint %q: no usable address", host)
	}
	addresses := make([]netip.Addr, 0, len(set))
	for address := range set {
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i].Compare(addresses[j]) < 0 })
	return addresses, nil
}

func forbiddenEndpointAddress(address netip.Addr) bool {
	return !address.IsValid() || address.IsUnspecified() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast()
}

func canonicalEndpointHost(host string) bool {
	if host == "" || host != strings.ToLower(host) || strings.HasSuffix(host, ".") || len(host) > 253 {
		return false
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return !address.Is4In6() && address.Zone() == "" && address.String() == host
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}
