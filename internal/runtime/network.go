package runtime

import "net/netip"

var nonPublicDNSPrefixes = mustParseDNSPrefixes(
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.88.99.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"64:ff9b::/96",
	"64:ff9b:1::/48",
	"100::/64",
	"2001::/23",
	"2001:db8::/32",
	"2002::/16",
	"3fff::/20",
	"5f00::/16",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
)

// IsPublicDNSAddress reports whether addr is a canonical, globally routable
// unicast address suitable for an explicitly approved external DNS resolver.
// Go's IsGlobalUnicast intentionally includes several RFC 6890 special-purpose
// ranges, so those ranges must be excluded separately.
func IsPublicDNSAddress(addr netip.Addr) bool {
	if !addr.IsValid() || addr.Is4In6() || addr.Zone() != "" || !addr.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicDNSPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}
	return true
}

func mustParseDNSPrefixes(values ...string) []netip.Prefix {
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}
