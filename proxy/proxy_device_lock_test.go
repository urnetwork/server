package proxy

import (
	"net/netip"
	"testing"

	"github.com/go-playground/assert/v2"
)

// TestSubnetContainsNormalizesV4InV6 pins the trap that would lock a customer out of
// their OWN proxy.
//
// A dual-stack listener reports an ipv4 peer as a v4-mapped v6 address (::ffff:a.b.c.d),
// and netip.Prefix.Contains is false across address families. So a naive implementation
// of the ip lock would compare an ipv4 lock against a v6-shaped caller, never match, and
// refuse the very person who asked for the lock.
func TestSubnetContainsNormalizesV4InV6(t *testing.T) {
	v4Lock := netip.MustParsePrefix("203.0.113.7/32")

	// the caller as a plain ipv4
	assert.Equal(t, subnetContains(v4Lock, netip.MustParseAddr("203.0.113.7")), true)

	// the SAME caller, reported by a dual-stack listener as v4-mapped v6
	assert.Equal(t, subnetContains(v4Lock, netip.MustParseAddr("::ffff:203.0.113.7")), true)

	// a different address is still refused, in either shape
	assert.Equal(t, subnetContains(v4Lock, netip.MustParseAddr("203.0.113.8")), false)
	assert.Equal(t, subnetContains(v4Lock, netip.MustParseAddr("::ffff:203.0.113.8")), false)

	// and the mirror case: the lock itself recorded as v4-mapped v6
	v4In6Lock := netip.MustParsePrefix("::ffff:203.0.113.7/128")
	assert.Equal(t, subnetContains(v4In6Lock, netip.MustParseAddr("203.0.113.7")), true)
	assert.Equal(t, subnetContains(v4In6Lock, netip.MustParseAddr("203.0.113.8")), false)
}

// TestSubnetContainsCidr pins that lock_ip_list accepts CIDRs, not just single hosts.
func TestSubnetContainsCidr(t *testing.T) {
	officeNetwork := netip.MustParsePrefix("198.51.100.0/24")

	assert.Equal(t, subnetContains(officeNetwork, netip.MustParseAddr("198.51.100.1")), true)
	assert.Equal(t, subnetContains(officeNetwork, netip.MustParseAddr("198.51.100.254")), true)
	assert.Equal(t, subnetContains(officeNetwork, netip.MustParseAddr("::ffff:198.51.100.42")), true)

	// one octet outside
	assert.Equal(t, subnetContains(officeNetwork, netip.MustParseAddr("198.51.101.1")), false)
}

// TestSubnetContainsIpv6 pins that an ipv6 lock works on its own terms.
func TestSubnetContainsIpv6(t *testing.T) {
	v6Lock := netip.MustParsePrefix("2001:db8::/32")

	assert.Equal(t, subnetContains(v6Lock, netip.MustParseAddr("2001:db8::1")), true)
	assert.Equal(t, subnetContains(v6Lock, netip.MustParseAddr("2001:db9::1")), false)
	// an ipv4 caller is not inside an ipv6 lock
	assert.Equal(t, subnetContains(v6Lock, netip.MustParseAddr("203.0.113.7")), false)
}
