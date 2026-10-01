// Resolver selection follows the SDK catalog without sharing mutable DNS state.
package providertunnel

import (
	"errors"
	"slices"
	"strings"

	"github.com/urnetwork/connect/v2026"
)

// An explicit override can select remote resolvers, never operator-host DNS.
var ErrOffTunnelDns = errors.New("providertunnel: host/local DNS is forbidden for provider probes")

// Custom remote settings win, then the SDK's provider-country recommendation,
// then the existing in-tunnel-only encrypted default. Regional recommendations
// intentionally use plaintext DNS through the provider, exactly as SDK/proxy do;
// they do not invent a regional DoH endpoint or a new fallback chain.
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
