package proxy

// A hosted proxy keeps the resolver settings auth-client stored when it was
// created (model proxyDnsResolverSettings): the client's own, or for a client
// that sent none the regional recommendation connect gave the proxy's country
// that day (connect RegionalDnsResolverSettings). Before 2026-09-26 the
// recommendation was stored whatever the client sent. A hosted device builds
// its tun from the stored settings each time it is created, so a proxy kept
// the recommendation of its creation day after connect replaced it. The kz and
// tm recommendations named Google Public DNS until 2026-11-08. The owner
// decided on 2026-11-09 that a kz or tm proxy stored with a former
// recommendation of its country uses the current one, and that only an exact
// former recommendation changes. tm has had no recommendation since
// 2026-11-11 (connect net_http_doh_regional.go: Turkmentelecom refuses clients
// outside its network and answers the platform's names with 127.0.0.1), so a
// tm proxy stored with any of them builds its tun with connect's defaults.
// The same rule covers ir and tr, whose recommendations named servers that
// cannot answer an exit until 2026-11-11: ir 403.online's private
// 10.202.10.10, and tr the resolvers of TTNET and Turkcell Superonline, which
// answer only their own customers, so a tr proxy stored with them resolved
// nothing on an exit of any other network. tr has had no recommendation since
// (DoH works in Turkey), so a tr proxy stored with one builds its tun with
// connect's defaults, as a tm proxy does.
//
// The replacement happens when a device reads its settings, not in storage.
// The device is the only user of the stored resolver settings, so the stored
// config is not rewritten (bringyourctl's proxy inspect prints it as stored).

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server/model"
)

// Every regional recommendation connect has given kz, tm, ir and tr, as auth-client
// stored it, by lower case country code: oldest first, the current one last,
// nil once the country has none. Each keeps the dns upgrade mask address it
// was stored with, so a later change of connect's default does not unmatch
// it. When connect changes a recommendation, append the new one
// (TestCountryCodeRegionalDnsRecommendationsEndWithTheCurrentOne).
var countryCodeRegionalDnsRecommendations = map[string][]*connect.DnsResolverSettings{
	"kz": {
		// connect e61efd34, 2026-02-09: Google, and Cloudflare as a local resolver
		{
			EnableRemoteDns: true,
			EnableLocalDns:  true,
			RemoteDnsIpv4:   []string{"8.8.8.8"},
			LocalDnsIpv4:    []string{"1.1.1.1"},
		},
		// connect ee7a476c, 2026-07-10: remote only
		{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   []string{"8.8.8.8"},
		},
		// connect 45357960, 2026-08-02: the dns upgrade mask address
		{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: "65.49.70.65",
			RemoteDnsIpv4:         []string{"8.8.8.8"},
		},
		// connect 588ad5b9, 2026-09-11: Google's v6 address
		{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: "65.49.70.65",
			RemoteDnsIpv4:         []string{"8.8.8.8"},
			RemoteDnsIpv6:         []string{"2001:4860:4860::8888"},
		},
		// connect b49cbc21, 2026-11-08: Cloudflare and Quad9's filtering 9.9.9.9
		{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: "65.49.70.65",
			RemoteDnsIpv4:         []string{"1.1.1.1", "9.9.9.9"},
			RemoteDnsIpv6:         []string{"2606:4700:4700::1111", "2620:fe::fe"},
		},
		// connect 829a5a54, 2026-11-08: Quad9's unfiltered 9.9.9.10
		{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: "65.49.70.65",
			RemoteDnsIpv4:         []string{"1.1.1.1", "9.9.9.10"},
			RemoteDnsIpv6:         []string{"2606:4700:4700::1111", "2620:fe::10"},
		},
	},
	"tm": {
		// connect e61efd34, 2026-02-09: Google, and Cloudflare as a local resolver
		{
			EnableRemoteDns: true,
			EnableLocalDns:  true,
			RemoteDnsIpv4:   []string{"8.8.4.4"},
			LocalDnsIpv4:    []string{"1.1.1.1"},
		},
		// connect c33aafe1, 2026-02-09: Turkmentelecom ahead of Google
		{
			EnableRemoteDns: true,
			EnableLocalDns:  true,
			RemoteDnsIpv4:   []string{"217.174.238.141", "8.8.4.4"},
			LocalDnsIpv4:    []string{"1.1.1.1"},
		},
		// connect ee7a476c, 2026-07-10: remote only
		{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   []string{"217.174.238.141", "8.8.4.4"},
		},
		// connect 45357960, 2026-08-02: the dns upgrade mask address
		{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: "65.49.70.65",
			RemoteDnsIpv4:         []string{"217.174.238.141", "8.8.4.4"},
		},
		// connect 588ad5b9, 2026-09-11: Google's v6 address
		{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: "65.49.70.65",
			RemoteDnsIpv4:         []string{"217.174.238.141", "8.8.4.4"},
			RemoteDnsIpv6:         []string{"2001:4860:4860::8844"},
		},
		// connect b49cbc21, 2026-11-08: Turkmentelecom alone
		{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: "65.49.70.65",
			RemoteDnsIpv4:         []string{"217.174.238.141"},
		},
		// connect 8e0fe91e, 2026-11-11: none
		nil,
	},
	"ir": {
		// connect e61efd34, 2026-02-09: Shecan and 403.online, and Cloudflare as a local resolver
		{
			EnableRemoteDns: true,
			EnableLocalDns:  true,
			RemoteDnsIpv4:   []string{"178.22.122.100", "10.202.10.10"},
			LocalDnsIpv4:    []string{"1.1.1.1"},
		},
		// connect ee7a476c, 2026-07-10: remote only
		{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   []string{"178.22.122.100", "10.202.10.10"},
		},
		// connect 45357960, 2026-08-02: the dns upgrade mask address
		{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: "65.49.70.65",
			RemoteDnsIpv4:         []string{"178.22.122.100", "10.202.10.10"},
		},
		// connect 8e0fe91e, 2026-11-11: Shecan and Electro
		{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: "65.49.70.65",
			RemoteDnsIpv4:         []string{"178.22.122.100", "78.157.42.100"},
		},
	},
	"tr": {
		// connect c33aafe1, 2026-02-09: TTNET and Turkcell Superonline, and Cloudflare as a local resolver
		{
			EnableRemoteDns: true,
			EnableLocalDns:  true,
			RemoteDnsIpv4:   []string{"195.175.39.39", "212.252.114.8"},
			LocalDnsIpv4:    []string{"1.1.1.1"},
		},
		// connect ee7a476c, 2026-07-10: remote only
		{
			EnableRemoteDns: true,
			RemoteDnsIpv4:   []string{"195.175.39.39", "212.252.114.8"},
		},
		// connect 45357960, 2026-08-02: the dns upgrade mask address
		{
			EnableRemoteDns:       true,
			DnsUpgradeMaskAddress: "65.49.70.65",
			RemoteDnsIpv4:         []string{"195.175.39.39", "212.252.114.8"},
		},
		// connect 1a3a71b8, 2026-11-11: none
		nil,
	},
}

// The resolver settings a hosted device's tun is built with, from its stored
// initial device state. Stored settings that are exactly a recommendation
// connect has given the device's country (countryCodeRegionalDnsRecommendations)
// read as the current recommendation, and any other stored settings read as
// stored. The host side then keeps only built-in servers
// (hostedDnsResolverSettings). The stored settings are not changed.
func proxyDeviceDnsResolverSettings(initialDeviceState *model.ProxyDeviceState) *connect.DnsResolverSettings {
	dnsResolverSettings := initialDeviceState.DnsResolverSettings
	// settings match a recommendation when they store the same json. Every
	// field counts and lists keep their order, so a reordered or extended list,
	// another toggle, or a field no recommendation set is the user's own.
	recommended := func(countryCode string) bool {
		recommendations := countryCodeRegionalDnsRecommendations[countryCode]
		if len(recommendations) == 0 {
			return false
		}
		storedJson, err := json.Marshal(dnsResolverSettings)
		if err != nil {
			return false
		}
		return slices.ContainsFunc(recommendations, func(recommendation *connect.DnsResolverSettings) bool {
			if recommendation == nil {
				// the country's recommendation ended; stored settings are never nil here
				return false
			}
			recommendationJson, err := json.Marshal(recommendation)
			return err == nil && bytes.Equal(storedJson, recommendationJson)
		})
	}
	if dnsResolverSettings != nil && initialDeviceState.Location != nil {
		// lower case, as connect matched the country when it stored the recommendation
		countryCode := strings.ToLower(initialDeviceState.Location.CountryCode)
		if recommended(countryCode) {
			dnsResolverSettings = connect.RegionalDnsResolverSettings(countryCode)
		}
	}
	return hostedDnsResolverSettings(dnsResolverSettings)
}
