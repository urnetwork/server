package monitor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// §28.2's per-country half asks a region's resolvers for the region's clients'
// location. This file uses only what the half exposed before it did, so it
// also runs against that code, where it fails.

// The client subnet a question carries (RFC 7871), or none.
func extenderDnsTestQuestionSubnet(t *testing.T, query dnsmessage.Message) netip.Prefix {
	t.Helper()
	for _, additional := range query.Additionals {
		option, ok := additional.Body.(*dnsmessage.OPTResource)
		if !ok {
			continue
		}
		for _, one := range option.Options {
			if one.Code != dnsClientSubnetOptionCode || len(one.Data) < 4 {
				continue
			}
			if binary.BigEndian.Uint16(one.Data[:2]) != 1 || one.Data[3] != 0 {
				t.Errorf("client subnet option %x", one.Data)
				return netip.Prefix{}
			}
			var address [4]byte
			copy(address[:], one.Data[4:])
			return netip.PrefixFrom(netip.AddrFrom4(address), int(one.Data[2]))
		}
	}
	return netip.Prefix{}
}

// The answer a resolver of China gave on 2026-11-12 for these values: whole
// within trimBytes; trimmed, unmarked, to the values that fit trimBytes; and
// marked truncated with none from truncatedBytes on, when that is set.
func extenderDnsTestChinaResolverAnswer(t *testing.T, query dnsmessage.Message, values []string, trimBytes int, truncatedBytes int) *dnsmessage.Message {
	t.Helper()
	size := func(message *dnsmessage.Message) int {
		packed, err := message.Pack()
		if err != nil {
			t.Fatal(err)
		}
		return len(packed)
	}
	whole := extenderDnsFakeTxtResponse(query, values, nil)
	if 0 < truncatedBytes && truncatedBytes <= size(whole) {
		return extenderDnsFakeTxtResponse(query, nil, func(message *dnsmessage.Message) { message.Header.Truncated = true })
	}
	for kept := len(values); 0 <= kept; kept-- {
		answer := extenderDnsFakeTxtResponse(query, values[:kept], nil)
		if size(answer) <= trimBytes {
			return answer
		}
	}
	return extenderDnsFakeTxtResponse(query, nil, nil)
}

// China's resolvers forward the asker's subnet to the name's nameservers, so
// asked from outside China they are answered for the monitor's location, and
// they trim what they are answered: Alidns to about 1.2 KB, DNSPod to 512
// bytes, and past 4.5 KB DNSPod answers truncated and empty (2026-11-12).
// China's own set, one record, comes back whole from both, but only to a
// question that carries a Chinese subnet. The half asks each region's
// resolvers with its own /24 as the subnet, standing in for the region's
// clients, judges China's whole set fresh and whole, and asks the default
// path without one, as a client asks it.
func TestExtenderDnsCountryAsksARegionsResolversForItsClientsLocation(t *testing.T) {
	issue := extenderDnsTestNow.Add(-4 * time.Minute)
	// the set Route 53 answers the monitor's location, 18 records in about 4.7
	// KB, past what DNSPod answers with any record, as the continents' sets of
	// 11-12 were; and China's own
	here := extenderDnsCountryTestValues(t, 0, 18, issue)
	china := extenderDnsCountryTestValues(t, 200, 1, issue)
	// every fake is on loopback, so a region's resolver asks for 127.0.0.0/24,
	// which stands for China here
	chinaSubnet := netip.MustParsePrefix("127.0.0.0/24")
	route53 := func(subnet netip.Prefix) []string {
		if subnet == chinaSubnet {
			return china
		}
		return here
	}
	authority := newExtenderDnsFakeAuthority(t, route53, func(netip.Prefix) {})

	var stateLock sync.Mutex
	subnets := map[string][]string{}
	asked := func(name string, query dnsmessage.Message) []string {
		subnet := extenderDnsTestQuestionSubnet(t, query)
		stateLock.Lock()
		subnets[name] = append(subnets[name], subnet.String())
		stateLock.Unlock()
		return route53(subnet)
	}
	chinaDoh := func(name string, trimBytes int, truncatedBytes int) (string, *x509.Certificate) {
		return newExtenderDnsFakeDoh(t, func(query dnsmessage.Message) (int, []byte) {
			answer, err := extenderDnsTestChinaResolverAnswer(t, query, asked(name, query), trimBytes, truncatedBytes).Pack()
			if err != nil {
				t.Fatal(err)
			}
			return http.StatusOK, answer
		})
	}
	alidns, certificate := chinaDoh("alidns", 1_204, 0)
	dnspod, _ := chinaDoh("dnspod", 512, 4_511)
	defaultDoh, _ := newExtenderDnsFakeDoh(t, func(query dnsmessage.Message) (int, []byte) {
		return http.StatusOK, extenderDnsFakeDohBody(t, query, asked("default", query), nil)
	})
	// a region whose plain resolver forwards the subnet and trims nothing
	forwardingPlain := newExtenderDnsFakeResolver(t, func(_ bool, query dnsmessage.Message) *dnsmessage.Message {
		return extenderDnsFakeTxtResponse(query, asked("plain", query), nil)
	})
	zonePlain := newExtenderDnsFakeResolver(t, func(_ bool, query dnsmessage.Message) *dnsmessage.Message {
		return extenderDnsFakeTxtResponse(query, here, nil)
	})

	named := func(name string, transport string, endpoint string) ExtenderDnsResolver {
		return ExtenderDnsResolver{Name: name, Transport: transport, Endpoint: endpoint}
	}
	settings := extenderDnsTestCountrySettings(
		ExtenderDnsCountry{Code: ExtenderDnsDefaultCountry, Doh: []ExtenderDnsResolver{
			named("DoH default", ExtenderDnsTransportDoh, defaultDoh),
		}},
		ExtenderDnsCountry{Code: "cn", Doh: []ExtenderDnsResolver{
			named("DoH Alidns", ExtenderDnsTransportDoh, alidns),
			named("DoH DNSPod", ExtenderDnsTransportDoh, dnspod),
		}},
		ExtenderDnsCountry{Code: "ir", Dns: []ExtenderDnsResolver{
			named("Forwarding", ExtenderDnsTransportDns, forwardingPlain),
		}},
	)
	settings.Resolvers = []string{zonePlain}
	settings.AuthoritativeServers = []string{authority}
	settings.CountryPacing = 5 * time.Millisecond
	settings.CountryBudget = 20 * time.Second

	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	cfg := &monitorConfig{env: "main", commandTimeout: 5 * time.Second}
	env := &probeEnv{
		cfg:    cfg,
		runner: &runner{cfg: cfg, dohTLSConfig: &tls.Config{RootCAs: pool}},
		now:    func() time.Time { return extenderDnsTestNow },
	}
	findings, err := extenderDnsRecordsProbe{settings: settings}.check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	observed := map[string]string{}
	for _, one := range findings {
		state := one.tier
		switch {
		case one.clearHold:
			state = "hold"
		case one.healthy:
			state = "healthy"
		case one.isUnobservable():
			state = "unobservable"
		}
		states[one.class+" "+one.target] = state
		observed[one.class+" "+one.target] = one.observed
	}
	stale := func(path string) string { return "extender-dns-country-stale main/extender-dns-records/" + path }
	partial := func(path string) string { return "extender-dns-country-partial main/extender-dns-records/" + path }
	want := map[string]string{
		"extender-dns-records-stale main/extender-dns-records":                  "healthy",
		"extender-dns-country-unobservable main/extender-dns-records/countries": "healthy",
		stale("default/DoH default"):                                            "healthy",
		partial("default/DoH default"):                                          "healthy",
		stale("cn/DoH Alidns"):                                                  "healthy",
		partial("cn/DoH Alidns"):                                                "healthy",
		stale("cn/DoH DNSPod"):                                                  "healthy",
		partial("cn/DoH DNSPod"):                                                "healthy",
		stale("ir/Forwarding"):                                                  "healthy",
		partial("ir/Forwarding"):                                                "healthy",
	}
	for identity, state := range want {
		if states[identity] != state {
			t.Errorf("%s is %q, want %q: %s", identity, states[identity], state, observed[identity])
		}
	}

	stateLock.Lock()
	defer stateLock.Unlock()
	for _, name := range []string{"alidns", "dnspod", "plain"} {
		if !slices.Equal(subnets[name], []string{chinaSubnet.String()}) {
			t.Errorf("the %s resolver was asked for %s, want its own /24 once", name, strings.Join(subnets[name], ","))
		}
	}
	for _, subnet := range subnets["default"] {
		if subnet != (netip.Prefix{}).String() {
			t.Errorf("the default path was asked for %s, want no client subnet", subnet)
		}
	}
}
