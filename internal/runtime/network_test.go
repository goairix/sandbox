package runtime

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsPublicDNSAddressRejectsSpecialPurposeRanges(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.0", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.1.1",
		"192.0.2.53", "198.18.0.1", "198.51.100.53", "203.0.113.53", "240.0.0.1",
		"64:ff9b::1", "100::1", "2001::1", "2001:db8::53", "2002::1", "3fff::1", "5f00::1", "fd00::1", "fe80::1",
	} {
		t.Run(raw, func(t *testing.T) {
			assert.False(t, IsPublicDNSAddress(netip.MustParseAddr(raw)))
		})
	}

	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2001:4860:4860::8888", "2606:4700:4700::1111"} {
		t.Run(raw, func(t *testing.T) {
			assert.True(t, IsPublicDNSAddress(netip.MustParseAddr(raw)))
		})
	}
}
