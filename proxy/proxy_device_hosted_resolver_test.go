package proxy

// The resolver settings a hosted device's tun is built with: a cloud host
// queries no server a proxy client names (hostedDnsResolverSettings).

import (
	"slices"
	"testing"

	"github.com/urnetwork/connect"
)

// A hosted device's tun queries from the proxy host only the built-in
// servers: the local DoH and dns servers a proxy client names in its
// initial device state are left out, a built-in one it lists stays, and an
// empty list stays empty. The remote servers, queried through the tunnel from
// a provider, and the toggles are kept as provisioned, and the provisioned
// settings are not changed.
func TestHostedDnsResolverSettingsKeepOnlyBuiltInHostServers(t *testing.T) {
	builtInSettings := connect.DefaultDnsResolverSettings()
	builtInDohUrl := builtInSettings.RemoteDohUrlsIpv4[0]
	builtInDnsServer := builtInSettings.LocalDnsIpv4[0]
	provisioned := &connect.DnsResolverSettings{
		EnableRemoteDoh:   true,
		EnableLocalDoh:    true,
		EnableLocalDns:    true,
		RemoteDohUrlsIpv4: []string{"https://192.0.2.53/dns-query"},
		RemoteDnsIpv4:     []string{"192.0.2.54"},
		LocalDohUrlsIpv4:  []string{"https://192.0.2.55/dns-query", builtInDohUrl},
		LocalDohUrlsIpv6:  []string{"https://[2001:db8::55]/dns-query"},
		LocalDnsIpv4:      []string{"192.0.2.56", builtInDnsServer},
		LocalDnsIpv6:      []string{},
	}

	hosted := hostedDnsResolverSettings(provisioned)
	if !slices.Equal(hosted.LocalDohUrlsIpv4, []string{builtInDohUrl}) {
		t.Fatalf("local v4 DoH servers = %v, expected only the built-in one", hosted.LocalDohUrlsIpv4)
	}
	if len(hosted.LocalDohUrlsIpv6) != 0 {
		t.Fatalf("local v6 DoH servers = %v, expected none", hosted.LocalDohUrlsIpv6)
	}
	if !slices.Equal(hosted.LocalDnsIpv4, []string{builtInDnsServer}) {
		t.Fatalf("local v4 dns servers = %v, expected only the built-in one", hosted.LocalDnsIpv4)
	}
	if len(hosted.LocalDnsIpv6) != 0 {
		t.Fatalf("local v6 dns servers = %v, expected none", hosted.LocalDnsIpv6)
	}
	if !slices.Equal(hosted.RemoteDohUrlsIpv4, provisioned.RemoteDohUrlsIpv4) ||
		!slices.Equal(hosted.RemoteDnsIpv4, provisioned.RemoteDnsIpv4) {
		t.Fatal("the remote servers, queried through the tunnel, were not kept")
	}
	if !hosted.EnableRemoteDoh || !hosted.EnableLocalDoh || !hosted.EnableLocalDns || hosted.EnableRemoteDns {
		t.Fatalf("the toggles changed: %+v", hosted)
	}
	if !slices.Equal(provisioned.LocalDnsIpv4, []string{"192.0.2.56", builtInDnsServer}) {
		t.Fatal("the provisioned settings must not change")
	}
	if hostedDnsResolverSettings(nil) != nil {
		t.Fatal("no provisioned settings must stay none, which is the tun's default")
	}
}
