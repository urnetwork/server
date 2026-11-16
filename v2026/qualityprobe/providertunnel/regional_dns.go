// Resolver selection follows the SDK catalog without sharing mutable DNS state.
package providertunnel

import (
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// An explicit override can select remote resolvers, never operator-host DNS.
var ErrOffTunnelDns = errors.New("providertunnel: host/local DNS is forbidden for provider probes")

// Custom remote settings win, then the SDK's provider-country recommendation,
// then the existing in-tunnel-only encrypted default. Regional recommendations
// use plaintext DNS through the provider, exactly as SDK/proxy do; they do not
// invent a regional DoH endpoint. How a tunnel uses the selection is
// providerDnsResolvers.
func providerDnsResolverSettings(cfg Config) (*connect.DnsResolverSettings, error) {
	base := cfg.DnsResolverSettings
	if base == nil {
		base = connect.RegionalDnsResolverSettings(strings.TrimSpace(cfg.ProviderCountry))
	}
	if base == nil {
		return inTunnelOnlyDnsResolverSettings(), nil
	}
	if base.EnableLocalDns || base.EnableLocalDoh {
		return nil, ErrOffTunnelDns
	}
	copy := *base
	copy.RemoteDohUrlsIpv4 = slices.Clone(base.RemoteDohUrlsIpv4)
	copy.RemoteDohUrlsIpv6 = slices.Clone(base.RemoteDohUrlsIpv6)
	copy.RemoteDnsIpv4 = slices.Clone(base.RemoteDnsIpv4)
	copy.RemoteDnsIpv6 = slices.Clone(base.RemoteDnsIpv6)
	// Disabled host-side endpoints are removed too, so this private owner has
	// no local path available to a later accidental flag change.
	copy.LocalDohUrlsIpv4, copy.LocalDohUrlsIpv6 = nil, nil
	copy.LocalDnsIpv4, copy.LocalDnsIpv6 = nil, nil
	if base.TlsConfig != nil {
		copy.TlsConfig = base.TlsConfig.Clone()
	}
	return &copy, nil
}

// The tunnel's own resolver, and the resolver whose lookups race it (nil for
// none). An explicit override is used alone, as is the in-tunnel default and
// any recommendation with DoH of its own.
//
// A recommendation that is plaintext DNS alone resolves over UDP only: the
// provider drops plaintext DNS over TCP (connect's CFAA port policy). Used
// alone, it made the URL probe a UDP test in exactly the countries that have
// one, and an exit that carries TCP but not UDP, such as a provider identity
// behind a proxy that only relays TCP, failed every URL there at DNS. On
// 2026-11-10 one provider network's exits passed 74% of their URL runs in
// de, fr, br and gb, where the tunnel resolves with DoH, and 0.1% in ru and
// 0% in kz, nearly every failure a 5 s DNS timeout. So such a tunnel keeps
// the in-tunnel DoH of every other country, which travels over TCP, and the
// recommendation's servers, which a national block of foreign DoH cannot
// stop, race it (raceProviderUrlQueries).
func providerDnsResolvers(cfg Config) (tunResolver *connect.DnsResolverSettings, raceResolver *connect.DnsResolverSettings, err error) {
	resolver, err := providerDnsResolverSettings(cfg)
	if err != nil {
		return nil, nil, err
	}
	if cfg.DnsResolverSettings != nil || resolver.EnableRemoteDoh {
		return resolver, nil, nil
	}
	return inTunnelOnlyDnsResolverSettings(), resolver, nil
}

// Builds the cache of a resolver that races the tunnel's own: a separate
// cache, so it shares no answers, connections or resolution slots with the
// tunnel's, that dials through the tunnel as the tunnel's own cache does
// (connect Tun.buildDohCache). It is a var so a test can supply the dialer and
// observe what Open asks for.
var newRaceDohCache = func(tun *connect.Tun, resolver *connect.DnsResolverSettings) *connect.DohCache {
	return newTunnelDohCache(tun.DialContext, tun.Ipv6Enabled(), resolver)
}

// A DoH cache over dial with the tunnel cache's timeouts. Over an IPv4-only
// tunnel the IPv6 servers are left out, as the tunnel's own cache leaves them
// out; the caller's settings are not changed.
func newTunnelDohCache(dial connect.DialContextFunction, ipv6Enabled bool, resolver *connect.DnsResolverSettings) *connect.DohCache {
	settings := connect.DefaultDohSettings()
	settings.RequestTimeout = connect.DefaultTunSettings().DohRequestTimeout
	settings.TlsTimeout = 30 * time.Second
	settings.DialContextSettings = &connect.DialContextSettings{DialContext: dial}
	private := *resolver
	if !ipv6Enabled {
		private.RemoteDohUrlsIpv6, private.RemoteDnsIpv6 = nil, nil
	}
	settings.DnsResolverSettings = &private
	return connect.NewDohCache(settings)
}
