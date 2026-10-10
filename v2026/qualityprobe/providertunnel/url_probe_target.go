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
}

// Retains normal TLS verification and the tunnel's own resolver. The initial
// request still needs catalog admission; a redirect grants only its exact host.
func (self *providerHttpTransport) ProviderUrlProbeContext(ctx context.Context, target *url.URL, redirect bool) (context.Context, error) {
	if target == nil || target.Scheme != "https" || target.Hostname() == "" || target.User != nil || (target.Port() != "" && target.Port() != "443") {
		return nil, fmt.Errorf("provider URL probe requires credential-free HTTPS on port 443")
	}
	if literal, err := netip.ParseAddr(target.Hostname()); err == nil && !publicUrlProbeAddress(literal) {
		return nil, fmt.Errorf("provider URL probe target is not public")
	}
	return context.WithValue(ctx, providerUrlProbeKey{}, providerUrlProbeTarget{host: normalizeHost(target.Hostname()), redirect: redirect}), nil
}

// This rejects special-purpose DNS answers before any target socket is opened.
var nonPublicUrlProbePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// Probe tunnels are IPv4-only. Neither private nor documentation addresses
// are public URL targets, even when an untrusted resolver returns them.
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
