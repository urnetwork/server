// Redirect permissions are exact, immutable values on one request context.
package providertunnel

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
)

type providerUrlProbeKey struct{}
type providerUrlProbeTarget struct {
	host     string
	redirect bool
	// The one IP family every hop of the request resolves and dials: 4 (the
	// URL's A record) or 6 (its AAAA record). Zero reads as 4.
	ipVersion int
}

// The target's IP family: 6, or 4 for anything else.
func (self providerUrlProbeTarget) family() int {
	if self.ipVersion == 6 {
		return 6
	}
	return 4
}

// Retains normal TLS verification and the tunnel's own resolver. The initial
// request still needs catalog admission; a redirect grants only its exact host.
// The request resolves and dials IPv4 alone, as every URL request did before
// families were probed.
func (self *providerHttpTransport) ProviderUrlProbeContext(ctx context.Context, target *url.URL, redirect bool) (context.Context, error) {
	return self.ProviderUrlProbeFamilyContext(ctx, target, redirect, 4)
}

// Implements egresshealth's per-request family grant: the URL grant above,
// for one IP family. Every hop resolves only the family's record (A for 4,
// AAAA for 6) through the tunnel's own resolver and dials only that family's
// addresses, with no fallback to the other family: a URL with no record of
// the family fails at DNS, never over the other family. An IPv4 request
// refuses a literal host that is not a public IPv4 address, as before families
// were probed. An IPv6 request refuses a literal that is not public in its own
// family; an IPv4 literal is refused when the request resolves it (a policy
// stage, which measures nothing about the provider's IPv6).
func (self *providerHttpTransport) ProviderUrlProbeFamilyContext(ctx context.Context, target *url.URL, redirect bool, ipVersion int) (context.Context, error) {
	if target == nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || (target.Port() != "" && target.Port() != "443") {
		return nil, fmt.Errorf("provider URL probe requires credential-free HTTPS on port 443")
	}
	if ipVersion != 4 && ipVersion != 6 {
		return nil, fmt.Errorf("provider URL probe IP version %d is neither 4 nor 6", ipVersion)
	}
	if literal, err := netip.ParseAddr(target.Hostname()); err == nil {
		public := publicUrlProbeAddress(literal)
		if ipVersion == 6 {
			public = publicUrlProbeAddressFor(literal, addressFamily(literal))
		}
		if !public {
			return nil, fmt.Errorf("provider URL probe target is not public")
		}
	}
	return context.WithValue(ctx, providerUrlProbeKey{}, providerUrlProbeTarget{host: normalizeHost(target.Hostname()), redirect: redirect, ipVersion: ipVersion}), nil
}

// The IP family an address is in: 4 for IPv4 and IPv4-mapped IPv6, 6 for
// every other IPv6 address.
func addressFamily(ip netip.Addr) int {
	if ip.Unmap().Is4() {
		return 4
	}
	return 6
}

// This rejects special-purpose DNS answers before any target socket is opened.
var nonPublicUrlProbePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// The IPv6 blocks inside 2000::/3 that are not a URL target's: the IANA
// special-purpose registry's blocks that are not globally reachable, those
// that embed or tunnel to an IPv4 address, and documentation (connect's
// ip_security_public.go and IPV6.md C4 hold the same blocks for provider
// egress). Only 2000::/3 is allocated global unicast, so the rest of the
// address space is refused by that rule alone.
var nonPublicUrlProbeIpv6Prefixes = []netip.Prefix{
	// ietf protocol assignments, with teredo 2001::/32 (embeds an ipv4
	// address), benchmarking 2001:2::/48 and orchid
	netip.MustParsePrefix("2001::/23"),
	// documentation (RFC 3849)
	netip.MustParsePrefix("2001:db8::/32"),
	// 6to4 (RFC 3056): embeds an ipv4 address
	netip.MustParsePrefix("2002::/16"),
	// the historical 6bone
	netip.MustParsePrefix("3ffe::/16"),
	// documentation (RFC 9637)
	netip.MustParsePrefix("3fff::/20"),
}

var globalUnicastIpv6Prefix = netip.MustParsePrefix("2000::/3")

// IPv4 URL targets. Neither private nor documentation addresses are public
// URL targets, even when an untrusted resolver returns them.
func publicUrlProbeAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Zone() != "" {
		return false
	}
	for _, prefix := range nonPublicUrlProbePrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

// Whether ip is a public URL target of the request's family: an IPv4 target
// for 4 (publicUrlProbeAddress), and for 6 a global unicast IPv6 address in
// 2000::/3 outside the special-purpose blocks. An IPv4-mapped IPv6 address is
// IPv4, so it is never an IPv6 target.
func publicUrlProbeAddressFor(ip netip.Addr, ipVersion int) bool {
	if ipVersion != 6 {
		return publicUrlProbeAddress(ip)
	}
	if !ip.Is6() || ip.Is4In6() || ip.Zone() != "" || !ip.IsGlobalUnicast() || ip.IsPrivate() || !globalUnicastIpv6Prefix.Contains(ip) {
		return false
	}
	for _, prefix := range nonPublicUrlProbeIpv6Prefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
