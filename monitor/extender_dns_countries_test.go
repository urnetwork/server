package monitor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
)

// The paths come from the sdk's per-country DNS settings, read at each call:
// the undisclosed path is the default DoH servers a client's control names
// ask, and each connection region the sdk offers has its DoH presets and its
// plain resolvers, named and ordered as the sdk names them.
func TestExtenderDnsCountriesComeFromTheSdk(t *testing.T) {
	countries := DefaultExtenderDnsRecordsSettings().Countries
	if !extenderDnsCountriesValid(countries) {
		t.Fatalf("the sdk's paths do not validate: %+v", countries)
	}
	v4 := func(values []string) []string {
		kept := []string{}
		for _, value := range values {
			if address, err := netip.ParseAddr(value); err == nil && address.Is4() {
				kept = append(kept, net.JoinHostPort(value, "53"))
			} else if address, ok := extenderDnsDohAddress(value); ok && address.Is4() {
				kept = append(kept, value)
			}
		}
		return kept
	}
	endpoints := func(resolvers []ExtenderDnsResolver) []string {
		values := []string{}
		for _, resolver := range resolvers {
			values = append(values, resolver.Endpoint)
		}
		return values
	}

	codes := []string{}
	for _, country := range countries {
		codes = append(codes, country.Code)
	}
	wantCodes := append([]string{ExtenderDnsDefaultCountry}, extenderDnsStringListValues(sdk.IngressCountryCodes())...)
	if !slices.Equal(codes, wantCodes) {
		t.Fatalf("paths %v, want the undisclosed path and the sdk's regions %v", codes, wantCodes)
	}

	undisclosed := countries[0]
	wantDefault := v4(extenderDnsStringListValues(sdk.GetDefaultDnsResolverSettings().RemoteDohUrlsIpv4))
	if !slices.Equal(endpoints(undisclosed.Doh), wantDefault) || len(undisclosed.Dns) != 0 {
		t.Fatalf("undisclosed path %+v, want the sdk's default DoH servers %v alone", undisclosed, wantDefault)
	}
	// the servers a client's control names ask (connect IngressDohSettings)
	if clientDefault := v4(connect.IngressDohSettings("", nil, nil).DnsResolverSettings.RemoteDohUrlsIpv4); !slices.Equal(wantDefault, clientDefault) {
		t.Fatalf("the sdk's default DoH servers %v are not the ones a client's control names ask, %v", wantDefault, clientDefault)
	}

	for _, country := range countries[1:] {
		if want := v4(extenderDnsStringListValues(sdk.IngressDohUrls(country.Code))); !slices.Equal(endpoints(country.Doh), want) {
			t.Fatalf("%s DoH %v, want the sdk's %v", country.Code, endpoints(country.Doh), want)
		}
		if want := v4(extenderDnsStringListValues(sdk.IngressPlainDnsServers(country.Code))); !slices.Equal(endpoints(country.Dns), want) {
			t.Fatalf("%s plain %v, want the sdk's %v", country.Code, endpoints(country.Dns), want)
		}
		names := map[string]bool{}
		for _, resolver := range slices.Concat(country.Doh, country.Dns) {
			if names[resolver.Name] {
				t.Fatalf("%s names %q twice", country.Code, resolver.Name)
			}
			names[resolver.Name] = true
		}
	}

	byCode := map[string]ExtenderDnsCountry{}
	for _, country := range countries {
		byCode[country.Code] = country
	}
	// Iran: Shecan, then Electro, at the addresses the sdk gives them
	regional := sdk.GetRegionalDnsServers()
	sdkAddress := func(countryCode string, name string) string {
		for index := 0; index < regional.Len(); index++ {
			if server := regional.Get(index); server.CountryCode == countryCode && server.Name == name {
				return net.JoinHostPort(server.Ipv4, "53")
			}
		}
		t.Fatalf("the sdk names no %s resolver %s", countryCode, name)
		return ""
	}
	iran := byCode["ir"]
	if len(iran.Doh) != 0 || len(iran.Dns) != 2 ||
		iran.Dns[0].Name != "Shecan" || iran.Dns[0].Endpoint != sdkAddress("ir", "Shecan") ||
		iran.Dns[1].Name != "Electro" || iran.Dns[1].Endpoint != sdkAddress("ir", "Electro") {
		t.Fatalf("ir path = %+v, want Shecan then Electro", iran)
	}
	// China: its DoH presets, named by address since the sdk names them by url
	if china := byCode["cn"]; len(china.Doh) == 0 || !strings.HasPrefix(china.Doh[0].Name, "DoH ") || china.Doh[0].Transport != ExtenderDnsTransportDoh {
		t.Fatalf("cn path = %+v", china)
	}
	// Russia names NSDI twice, so each carries its address
	nsdi := 0
	for _, resolver := range byCode["ru"].Dns {
		if !strings.HasPrefix(resolver.Name, "National DNS (NSDI)") {
			continue
		}
		nsdi++
		if host, _, _ := net.SplitHostPort(resolver.Endpoint); resolver.Name != "National DNS (NSDI) "+host {
			t.Fatalf("ru resolver %+v", resolver)
		}
	}
	if nsdi < 2 {
		t.Fatalf("ru path = %+v, want both NSDI addresses", byCode["ru"])
	}
}

// DNSPod's trim is recognized by the endpoints the sdk gives cn: one of them
// renamed in the sdk's table would turn DNSPod's expected trim into partial
// alerts, so the default list must name endpoints of cn's path.
func TestExtenderDnsTrimmingResolversAreOnChinasPath(t *testing.T) {
	endpoints := []string{}
	for _, country := range ExtenderDnsCountriesFromSdk() {
		if country.Code != "cn" {
			continue
		}
		for _, resolver := range slices.Concat(country.Doh, country.Dns) {
			endpoints = append(endpoints, resolver.Endpoint)
		}
	}
	trimming := ExtenderDnsDnspodTrimmingResolvers()
	if len(trimming) != 3 {
		t.Fatalf("DNSPod's trimming resolvers are %v, want its two DoH servers and its plain resolver", trimming)
	}
	for endpoint, wholeBytes := range trimming {
		if !slices.Contains(endpoints, endpoint) {
			t.Errorf("%s is not on cn's path %v", endpoint, endpoints)
		}
		if wholeBytes != 481 {
			t.Errorf("%s delivers %d bytes whole, want the 481 measured on 2026-11-12", endpoint, wholeBytes)
		}
	}
	connect.AssertEqual(t, DefaultExtenderDnsRecordsSettings().TrimmingResolvers, trimming)
}

// The client's DoH parse (connect parseDohWire): values joined from their
// strings, empty and repeated values left out, the answers read until the
// first that does not parse, none for a name error, and a policy extended
// dns error read only when every section before it parses.
func TestParseExtenderDnsDohTxtReadsLikeAClient(t *testing.T) {
	values := []string{strings.Repeat("A", 300), strings.Repeat("A", 300), "", "second"}
	view := parseExtenderDnsDohTxt(extenderDnsCountryDohBody(t, values, nil))
	if !view.answered || view.policyEde || view.truncatedFlag || !slices.Equal(view.values, []string{strings.Repeat("A", 300), "second"}) {
		t.Fatalf("view = %+v", view)
	}

	nameError := parseExtenderDnsDohTxt(extenderDnsCountryDohBody(t, []string{"x"}, func(message *dnsmessage.Message) {
		message.Header.RCode = dnsmessage.RCodeNameError
	}))
	if !nameError.answered || len(nameError.values) != 0 || nameError.responseCode != DNSResponseNameError {
		t.Fatalf("name error = %+v", nameError)
	}
	for rcode, failure := range map[dnsmessage.RCode]string{
		dnsmessage.RCodeServerFailure:  "server_failure",
		dnsmessage.RCodeRefused:        "refused",
		dnsmessage.RCodeNotImplemented: "rcode_other",
	} {
		view := parseExtenderDnsDohTxt(extenderDnsCountryDohBody(t, []string{"x"}, func(message *dnsmessage.Message) { message.Header.RCode = rcode }))
		if view.answered || view.failure != failure {
			t.Fatalf("rcode %v = %+v", rcode, view)
		}
	}
	if view := parseExtenderDnsDohTxt([]byte{1, 2, 3}); view.answered || view.failure != "malformed" {
		t.Fatalf("malformed = %+v", view)
	}

	withEde := func(code uint16) func(message *dnsmessage.Message) {
		return func(message *dnsmessage.Message) {
			var header dnsmessage.ResourceHeader
			if err := header.SetEDNS0(1232, dnsmessage.RCodeSuccess, false); err != nil {
				t.Fatal(err)
			}
			data := binary.BigEndian.AppendUint16(nil, code)
			message.Additionals = append(message.Additionals, dnsmessage.Resource{Header: header, Body: &dnsmessage.OPTResource{
				Options: []dnsmessage.Option{{Code: extenderDnsEdeOptionCode, Data: data}},
			}})
		}
	}
	if view := parseExtenderDnsDohTxt(extenderDnsCountryDohBody(t, []string{"x"}, withEde(17))); !view.policyEde {
		t.Fatalf("a Filtered extended error = %+v", view)
	}
	if view := parseExtenderDnsDohTxt(extenderDnsCountryDohBody(t, []string{"x"}, withEde(6))); view.policyEde {
		t.Fatalf("a DNSSEC extended error = %+v", view)
	}
	filtered := extenderDnsDohAnswer(extenderDnsCountryDohBody(t, []string{"x"}, withEde(15)), 16*1024, 64*1024)
	if !filtered.filtered || len(filtered.kept) != 0 || filtered.served != 1 {
		t.Fatalf("a Blocked answer = %+v", filtered)
	}

	// cut inside the 5th record: a client keeps the 4 before it
	records := extenderDnsCountryTestValues(t, 0, 8, extenderDnsTestNow)
	body := extenderDnsCountryDohBody(t, records, nil)
	cutAt := 0
	for index := range records {
		prefix := extenderDnsCountryDohBody(t, records[:index+1], nil)
		if index == 4 {
			cutAt = len(prefix) - 20
			break
		}
	}
	cut := extenderDnsDohAnswer(body, cutAt, 64*1024)
	if len(cut.kept) != 4 || cut.served != 8 || !slices.Equal(cut.kept, records[:4]) {
		t.Fatalf("a cut answer kept %d of %d", len(cut.kept), cut.served)
	}
	whole := extenderDnsDohAnswer(body, 64*1024, 64*1024)
	if len(whole.kept) != 8 || whole.served != 8 || whole.responseBytes != len(body) {
		t.Fatalf("a whole answer = kept %d served %d bytes %d", len(whole.kept), whole.served, whole.responseBytes)
	}
}

// One path for the logic tests, with synthetic endpoints.
func extenderDnsTestCountrySettings(countries ...ExtenderDnsCountry) *ExtenderDnsRecordsSettings {
	settings := DefaultExtenderDnsRecordsSettings()
	settings.Countries = countries
	settings.CountryPacing = 0
	settings.CountryBudget = 5 * time.Second
	return settings
}

func extenderDnsTestDoh(name string, address string) ExtenderDnsResolver {
	return ExtenderDnsResolver{Name: name, Transport: ExtenderDnsTransportDoh, Endpoint: "https://" + address + "/dns-query"}
}

func extenderDnsTestDns(name string, address string) ExtenderDnsResolver {
	return ExtenderDnsResolver{Name: name, Transport: ExtenderDnsTransportDns, Endpoint: net.JoinHostPort(address, "53")}
}

// A client asks a region's plain resolvers only when its DoH servers yield no
// TXT value: none served, a truncated answer with none, a filtered answer or
// no answer at all.
func TestExtenderDnsCountryFallsBackLikeAClient(t *testing.T) {
	fresh := extenderDnsCountryTestValues(t, 0, 4, extenderDnsTestNow.Add(-time.Minute))
	settings := extenderDnsTestCountrySettings(ExtenderDnsCountry{
		Code: "cn",
		Doh:  []ExtenderDnsResolver{extenderDnsTestDoh("DoH one", "192.0.2.1"), extenderDnsTestDoh("DoH two", "192.0.2.2")},
		Dns:  []ExtenderDnsResolver{extenderDnsTestDns("Plain one", "198.51.100.1"), extenderDnsTestDns("Plain two", "198.51.100.2")},
	})
	settings.Resolvers = []string{"203.0.113.53:53"}
	for _, test := range []struct {
		name     string
		doh      func(string) (int, []byte, error)
		fallback bool
	}{
		{name: "records", doh: func(string) (int, []byte, error) { return 200, extenderDnsCountryDohBody(t, fresh, nil), nil }},
		{name: "one server's records", doh: func(dohUrl string) (int, []byte, error) {
			if strings.Contains(dohUrl, "192.0.2.2") {
				return 200, extenderDnsCountryDohBody(t, fresh, nil), nil
			}
			return 0, nil, syscall.ECONNREFUSED
		}},
		{name: "none served", fallback: true, doh: func(string) (int, []byte, error) { return 200, extenderDnsCountryDohBody(t, nil, nil), nil }},
		{name: "truncated with none", fallback: true, doh: func(string) (int, []byte, error) {
			return 200, extenderDnsCountryDohBody(t, nil, func(message *dnsmessage.Message) { message.Header.Truncated = true }), nil
		}},
		{name: "refused", fallback: true, doh: func(string) (int, []byte, error) { return 403, nil, errors.New("doh query: HTTP status 403") }},
		{name: "unreachable", fallback: true, doh: func(string) (int, []byte, error) { return 0, nil, syscall.ECONNREFUSED }},
	} {
		source := &extenderDnsCountrySource{
			syntheticSource: &syntheticSource{},
			txt:             func(string) (DNSResponseObservation, error) { return extenderDnsTestAnswer(fresh), nil },
			doh:             func(dohUrl string, _ int) (int, []byte, error) { return test.doh(dohUrl) },
			authoritative:   func(netip.Prefix) (DNSResponseObservation, error) { return extenderDnsTestAnswer(fresh), nil },
		}
		states, _ := extenderDnsCountryStates(t, settings, source)
		asked := strings.Join(source.askedSorted(), ",")
		fellBack := strings.Contains(asked, "dns 198.51.100.1:53") && strings.Contains(asked, "dns 198.51.100.2:53")
		if fellBack != test.fallback || (!test.fallback && strings.Contains(asked, "198.51.100")) {
			t.Fatalf("%s: asked %s, fallback %t", test.name, asked, test.fallback)
		}
		if test.fallback {
			requireStormStates(t, test.name, states, map[string]string{
				"extender-dns-country-stale main/extender-dns-records/cn/Plain one":   "healthy",
				"extender-dns-country-partial main/extender-dns-records/cn/Plain two": "healthy",
			})
		} else if _, ok := states["extender-dns-country-stale main/extender-dns-records/cn/Plain one"]; ok {
			t.Fatalf("%s: a resolver off the client's path was judged: %v", test.name, states)
		}
	}
}

// A zone gone stale is the whole-zone alert's: each resolver serving the
// same staleness holds, while a resolver failing on its own still warns.
func TestExtenderDnsZoneStaleHoldsCountryStaleness(t *testing.T) {
	expired := extenderDnsCountryTestValues(t, 0, 4, extenderDnsTestNow.Add(-50*time.Hour))
	settings := extenderDnsTestCountrySettings(ExtenderDnsCountry{
		Code: "ru",
		Dns:  []ExtenderDnsResolver{extenderDnsTestDns("Same", "198.51.100.1"), extenderDnsTestDns("Empty", "198.51.100.2")},
	})
	settings.Resolvers = []string{"203.0.113.53:53"}
	source := &extenderDnsCountrySource{
		syntheticSource: &syntheticSource{},
		txt: func(resolver string) (DNSResponseObservation, error) {
			if resolver == "198.51.100.2:53" {
				return extenderDnsTestAnswer(nil), nil
			}
			return extenderDnsTestAnswer(expired), nil
		},
		authoritative: func(netip.Prefix) (DNSResponseObservation, error) { return extenderDnsTestAnswer(expired), nil },
	}
	states, _ := extenderDnsCountryStates(t, settings, source)
	requireStormStates(t, "a stale zone", states, map[string]string{
		"extender-dns-records-stale main/extender-dns-records":          "warn",
		"extender-dns-country-stale main/extender-dns-records/ru/Same":  "hold",
		"extender-dns-country-stale main/extender-dns-records/ru/Empty": "warn",
	})
}

// A set smaller than the set from here is partial only against the set of
// the resolver's own location, which may be that smaller set.
func TestExtenderDnsCountryPartialNeedsTheLocationSet(t *testing.T) {
	fresh := extenderDnsCountryTestValues(t, 0, 15, extenderDnsTestNow.Add(-time.Minute))
	settings := extenderDnsTestCountrySettings(ExtenderDnsCountry{
		Code: "ir",
		Dns:  []ExtenderDnsResolver{extenderDnsTestDns("Small", "198.51.100.1")},
	})
	settings.Resolvers = []string{"203.0.113.53:53"}
	const partial = "extender-dns-country-partial main/extender-dns-records/ir/Small"
	const visibility = "extender-dns-country-unobservable main/extender-dns-records/countries"
	for _, test := range []struct {
		name          string
		authoritative func(netip.Prefix) (DNSResponseObservation, error)
		want          map[string]string
	}{
		{name: "the location's set is the small one", authoritative: func(subnet netip.Prefix) (DNSResponseObservation, error) {
			if subnet == netip.MustParsePrefix("198.51.100.0/24") {
				return extenderDnsTestAnswer(fresh[:3]), nil
			}
			return extenderDnsTestAnswer(fresh), nil
		}, want: map[string]string{partial: "healthy", visibility: "healthy"}},
		{name: "the location's set is the whole one", authoritative: func(netip.Prefix) (DNSResponseObservation, error) {
			return extenderDnsTestAnswer(fresh), nil
		}, want: map[string]string{partial: "warn", visibility: "healthy"}},
		{name: "the location's set cannot be read", authoritative: func(subnet netip.Prefix) (DNSResponseObservation, error) {
			if subnet.IsValid() {
				return DNSResponseObservation{}, errors.New("synthetic timeout")
			}
			return extenderDnsTestAnswer(fresh), nil
		}, want: map[string]string{partial: "hold", visibility: "unobservable"}},
		{name: "no set can be read", authoritative: func(netip.Prefix) (DNSResponseObservation, error) {
			return DNSResponseObservation{ResponseCode: DNSResponseServerFailure}, nil
		}, want: map[string]string{partial: "hold", visibility: "unobservable"}},
	} {
		source := &extenderDnsCountrySource{
			syntheticSource: &syntheticSource{},
			txt: func(resolver string) (DNSResponseObservation, error) {
				if resolver == "198.51.100.1:53" {
					return extenderDnsTestAnswer(fresh[:3]), nil
				}
				return extenderDnsTestAnswer(fresh), nil
			},
			authoritative: test.authoritative,
		}
		states, findings := extenderDnsCountryStates(t, settings, source)
		requireStormStates(t, test.name, states, test.want)
		for _, one := range findings {
			if one.class == "extender-dns-country-unobservable" && !one.healthy && !strings.Contains(one.observed, "visibility_reasons=authority_unavailable") {
				t.Fatalf("%s: %s", test.name, one.observed)
			}
		}
	}
}

// Unreachable from here is visibility only: a region none of whose
// resolvers answers holds its identities and is listed as blind, and only a
// run in which no resolver answered at all reports a visibility state.
func TestExtenderDnsCountryUnreachableIsVisibilityOnly(t *testing.T) {
	fresh := extenderDnsCountryTestValues(t, 0, 4, extenderDnsTestNow.Add(-time.Minute))
	expired := extenderDnsCountryTestValues(t, 0, 4, extenderDnsTestNow.Add(-50*time.Hour))
	settings := extenderDnsTestCountrySettings(
		ExtenderDnsCountry{Code: "ir", Dns: []ExtenderDnsResolver{extenderDnsTestDns("Refuses", "198.51.100.1"), extenderDnsTestDns("Silent", "198.51.100.2")}},
		ExtenderDnsCountry{Code: "ru", Dns: []ExtenderDnsResolver{extenderDnsTestDns("Old", "198.51.100.3")}},
	)
	settings.Resolvers = []string{"203.0.113.53:53"}
	refuse := func(resolver string) (DNSResponseObservation, error) {
		switch resolver {
		case "198.51.100.1:53":
			return DNSResponseObservation{ResponseCode: DNSResponseRefused}, nil
		case "198.51.100.2:53":
			return DNSResponseObservation{}, &net.OpError{Op: "read", Net: "udp", Err: syscall.ECONNREFUSED}
		case "198.51.100.3:53":
			return extenderDnsTestAnswer(expired), nil
		}
		return extenderDnsTestAnswer(fresh), nil
	}
	source := &extenderDnsCountrySource{
		syntheticSource: &syntheticSource{},
		txt:             refuse,
		authoritative:   func(netip.Prefix) (DNSResponseObservation, error) { return extenderDnsTestAnswer(fresh), nil },
	}
	states, findings := extenderDnsCountryStates(t, settings, source)
	requireStormStates(t, "a blind region", states, map[string]string{
		"extender-dns-country-stale main/extender-dns-records/ir/Refuses":       "hold",
		"extender-dns-country-partial main/extender-dns-records/ir/Silent":      "hold",
		"extender-dns-country-stale main/extender-dns-records/ru/Old":           "warn",
		"extender-dns-country-unobservable main/extender-dns-records/countries": "healthy",
	})
	for _, one := range findings {
		if one.class == "extender-dns-country-stale" && !one.healthy && !one.clearHold && !strings.Contains(one.observed, "blind=ir") {
			t.Fatalf("the warning does not list the blind region: %s", one.observed)
		}
	}

	source.txt = func(resolver string) (DNSResponseObservation, error) {
		if resolver == "203.0.113.53:53" {
			return extenderDnsTestAnswer(fresh), nil
		}
		return DNSResponseObservation{ResponseCode: DNSResponseServerFailure}, nil
	}
	states, findings = extenderDnsCountryStates(t, settings, source)
	requireStormStates(t, "nothing answers", states, map[string]string{
		"extender-dns-country-stale main/extender-dns-records/ru/Old":           "hold",
		"extender-dns-country-unobservable main/extender-dns-records/countries": "unobservable",
	})
	for _, one := range findings {
		if one.class == "extender-dns-country-unobservable" && !strings.Contains(one.observed, "visibility_reasons=none_answered blind=ir,ru") {
			t.Fatalf("visibility = %s", one.observed)
		}
	}
}

// A plain resolver that leaves the TXT question unanswered is asked the A
// question: one that answers it drops the TXT answer, as the monitor host's
// router did on 2026-11-11, and warns; one that answers neither is
// unreachable from here and holds. A refusal is an answer, so it is not asked
// again.
func TestExtenderDnsCountryTellsADroppedTxtFromAnUnreachableResolver(t *testing.T) {
	fresh := extenderDnsCountryTestValues(t, 0, 4, extenderDnsTestNow.Add(-time.Minute))
	settings := extenderDnsTestCountrySettings(ExtenderDnsCountry{Code: "ir", Dns: []ExtenderDnsResolver{
		extenderDnsTestDns("Router", "198.51.100.1"),
		extenderDnsTestDns("Gone", "198.51.100.2"),
		extenderDnsTestDns("Refuses", "198.51.100.3"),
		extenderDnsTestDns("Answers", "198.51.100.4"),
	}})
	settings.Resolvers = []string{"203.0.113.53:53"}
	timeout := &net.DNSError{Err: "i/o timeout", IsTimeout: true}
	source := &extenderDnsCountrySource{
		syntheticSource: &syntheticSource{},
		txt: func(resolver string) (DNSResponseObservation, error) {
			switch resolver {
			case "198.51.100.1:53", "198.51.100.2:53":
				return DNSResponseObservation{}, fmt.Errorf("txt query: %w", timeout)
			case "198.51.100.3:53":
				return DNSResponseObservation{ResponseCode: DNSResponseRefused}, nil
			}
			return extenderDnsTestAnswer(fresh), nil
		},
		address: func(resolver string) (DNSResponseObservation, error) {
			if resolver == "198.51.100.1:53" {
				return DNSResponseObservation{ResponseCode: DNSResponseSuccess, Addresses: []string{"192.0.2.60"}}, nil
			}
			return DNSResponseObservation{}, fmt.Errorf("address query: %w", timeout)
		},
		authoritative: func(netip.Prefix) (DNSResponseObservation, error) { return extenderDnsTestAnswer(fresh), nil },
	}
	states, findings := extenderDnsCountryStates(t, settings, source)
	requireStormStates(t, "a dropped txt", states, map[string]string{
		"extender-dns-country-stale main/extender-dns-records/ir/Router":        "warn",
		"extender-dns-country-partial main/extender-dns-records/ir/Router":      "healthy",
		"extender-dns-country-stale main/extender-dns-records/ir/Gone":          "hold",
		"extender-dns-country-stale main/extender-dns-records/ir/Refuses":       "hold",
		"extender-dns-country-stale main/extender-dns-records/ir/Answers":       "healthy",
		"extender-dns-country-unobservable main/extender-dns-records/countries": "healthy",
	})
	for _, one := range findings {
		if one.class == "extender-dns-country-stale" && one.target == "main/extender-dns-records/ir/Router" &&
			(!strings.Contains(one.observed, "reasons=txt_dropped") || !strings.Contains(one.observed, "path_unreachable=Gone,Refuses")) {
			t.Fatalf("Router: %s", one.observed)
		}
	}
	asked := strings.Join(source.askedSorted(), ",")
	if !strings.Contains(asked, "address 198.51.100.1:53") || !strings.Contains(asked, "address 198.51.100.2:53") ||
		strings.Contains(asked, "address 198.51.100.3:53") || strings.Contains(asked, "address 198.51.100.4:53") {
		t.Fatalf("asked %s, want the A question of the two that left the TXT question unanswered", asked)
	}
}

// The half stops at its budget: questions it could not finish hold their
// identities, and the run reports the gap instead of waiting.
func TestExtenderDnsCountryBudgetBoundsTheRun(t *testing.T) {
	fresh := extenderDnsCountryTestValues(t, 0, 4, extenderDnsTestNow.Add(-time.Minute))
	resolvers := []ExtenderDnsResolver{}
	for index := 1; index <= 12; index++ {
		resolvers = append(resolvers, extenderDnsTestDns(fmt.Sprintf("Slow %d", index), fmt.Sprintf("198.51.100.%d", index)))
	}
	settings := extenderDnsTestCountrySettings(ExtenderDnsCountry{Code: "ru", Dns: resolvers})
	settings.Resolvers = []string{"203.0.113.53:53"}
	settings.CountryBudget = 300 * time.Millisecond
	settings.CountryConcurrency = 2
	source := &extenderDnsCountrySource{syntheticSource: &syntheticSource{}, authoritative: func(netip.Prefix) (DNSResponseObservation, error) {
		return extenderDnsTestAnswer(fresh), nil
	}}
	source.txt = func(resolver string) (DNSResponseObservation, error) {
		if resolver == "203.0.113.53:53" {
			return extenderDnsTestAnswer(fresh), nil
		}
		time.Sleep(time.Second)
		return extenderDnsTestAnswer(fresh), nil
	}
	start := time.Now()
	states, _ := extenderDnsCountryStates(t, settings, source)
	if elapsed := time.Since(start); 3*time.Second < elapsed {
		t.Fatalf("the half took %s past a budget of %s", elapsed, settings.CountryBudget)
	}
	requireStormStates(t, "an exhausted budget", states, map[string]string{
		"extender-dns-country-stale main/extender-dns-records/ru/Slow 12":       "hold",
		"extender-dns-country-unobservable main/extender-dns-records/countries": "unobservable",
	})
	if asked := len(source.askedSorted()); 6 < asked {
		t.Fatalf("asked %d questions in a budget that holds about 2", asked)
	}
}

// The launcher keeps at most its limit in flight, leaves at least its pacing
// between two launches, and launches nothing once the budget has ended.
func TestExtenderDnsLauncherBoundsTheRun(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	const pacing = 20 * time.Millisecond
	launcher := newExtenderDnsLauncher(ctx, 2, pacing)
	var stateLock sync.Mutex
	inFlight, peak := 0, 0
	launches := []time.Time{}
	for range 40 {
		if !launcher.launch(func(ctx context.Context) {
			stateLock.Lock()
			inFlight++
			peak = max(peak, inFlight)
			stateLock.Unlock()
			select {
			case <-ctx.Done():
			case <-time.After(60 * time.Millisecond):
			}
			stateLock.Lock()
			inFlight--
			stateLock.Unlock()
		}) {
			break
		}
		launches = append(launches, launcher.last)
	}
	launcher.finish()
	if peak < 1 || 2 < peak {
		t.Fatalf("peak in flight %d, want at most 2", peak)
	}
	for index := 1; index < len(launches); index++ {
		if gap := launches[index].Sub(launches[index-1]); gap < pacing {
			t.Fatalf("launches %d and %d %s apart, under the pacing %s", index-1, index, gap, pacing)
		}
	}
	if len(launches) == 0 || 40 <= len(launches) {
		t.Fatalf("launched %d of 40 within a 400ms budget", len(launches))
	}
	if launcher.launch(func(context.Context) { t.Error("launched after the budget") }) {
		t.Fatal("a launch after the budget succeeded")
	}
}

// A fake recursive resolver on loopback, UDP and TCP on one port. answer gets
// each question and whether it came over TCP, and returns the response, or
// nil to drop it (no UDP reply; the TCP connection closed). A UDP response
// over 512 bytes goes out truncated, with no answers, as a server answers a
// question without EDNS.
func newExtenderDnsFakeResolver(t *testing.T, answer func(tcp bool, query dnsmessage.Message) *dnsmessage.Message) string {
	t.Helper()
	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpListener, err := net.ListenPacket("udp", tcpListener.Addr().String())
	if err != nil {
		tcpListener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		tcpListener.Close()
		udpListener.Close()
	})
	go func() {
		buffer := make([]byte, 65535)
		for {
			count, peer, err := udpListener.ReadFrom(buffer)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if query.Unpack(buffer[:count]) != nil {
				continue
			}
			response := answer(false, query)
			if response == nil {
				continue
			}
			packet, err := response.Pack()
			if err != nil {
				continue
			}
			if 512 < len(packet) {
				truncated := *response
				truncated.Header.Truncated = true
				truncated.Answers = nil
				if packet, err = truncated.Pack(); err != nil {
					continue
				}
			}
			udpListener.WriteTo(packet, peer)
		}
	}()
	go func() {
		for {
			connection, err := tcpListener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				connection.SetDeadline(time.Now().Add(5 * time.Second))
				var length [2]byte
				if _, err := io.ReadFull(connection, length[:]); err != nil {
					return
				}
				packet := make([]byte, binary.BigEndian.Uint16(length[:]))
				if _, err := io.ReadFull(connection, packet); err != nil {
					return
				}
				var query dnsmessage.Message
				if query.Unpack(packet) != nil {
					return
				}
				response := answer(true, query)
				if response == nil {
					return
				}
				out, err := response.Pack()
				if err != nil {
					return
				}
				connection.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(out))), out...))
			}()
		}
	}()
	return tcpListener.Addr().String()
}

// The response to a TXT question: its id and question, these values, then
// edit's changes.
func extenderDnsFakeTxtResponse(query dnsmessage.Message, values []string, edit func(message *dnsmessage.Message)) *dnsmessage.Message {
	message := &dnsmessage.Message{
		Header:    dnsmessage.Header{ID: query.Header.ID, Response: true, RecursionDesired: query.Header.RecursionDesired, RecursionAvailable: true, RCode: dnsmessage.RCodeSuccess},
		Questions: query.Questions,
	}
	for _, value := range values {
		strs := []string{}
		for 255 < len(value) {
			strs = append(strs, value[:255])
			value = value[255:]
		}
		message.Answers = append(message.Answers, dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{Name: query.Questions[0].Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET, TTL: 60},
			Body:   &dnsmessage.TXTResource{TXT: append(strs, value)},
		})
	}
	if edit != nil {
		edit(message)
	}
	return message
}

// A fake DoH server on loopback (https, HTTP/2, the httptest certificate,
// which names 127.0.0.1) that requires the question a client sends: a GET of
// one TXT question with id 0, recursion desired and an OPT record of 1232
// bytes, Accept application/dns-message.
func newExtenderDnsFakeDoh(t *testing.T, answer func(query dnsmessage.Message) (int, []byte)) (string, *x509.Certificate) {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		data, err := base64.RawURLEncoding.DecodeString(request.URL.Query().Get("dns"))
		var query dnsmessage.Message
		if err == nil {
			err = query.Unpack(data)
		}
		if err != nil || request.Method != http.MethodGet || request.Header.Get("Accept") != "application/dns-message" ||
			query.Header.ID != 0 || !query.Header.RecursionDesired || len(query.Questions) != 1 || query.Questions[0].Type != dnsmessage.TypeTXT ||
			len(query.Additionals) != 1 || query.Additionals[0].Header.Type != dnsmessage.TypeOPT || query.Additionals[0].Header.Class != 1232 {
			t.Errorf("a question unlike a client's: %s %+v %v", request.Method, query, err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		status, body := answer(query)
		writer.Header().Set("Content-Type", "application/dns-message")
		writer.WriteHeader(status)
		writer.Write(body)
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	return server.URL + "/dns-query", server.Certificate()
}

func extenderDnsFakeDohBody(t *testing.T, query dnsmessage.Message, values []string, edit func(message *dnsmessage.Message)) []byte {
	t.Helper()
	body, err := extenderDnsFakeTxtResponse(query, values, edit).Pack()
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The monitor reads a DoH answer as a client reads it: against connect's own
// DohQueryTxt on the same server, it keeps the same records of an answer
// under 16 KiB and of one over it, where both keep only the records before
// the cut.
func TestExtenderDnsDohReadsAsTheClientDoes(t *testing.T) {
	small := extenderDnsCountryTestValues(t, 0, 15, extenderDnsTestNow)
	large := extenderDnsCountryTestValues(t, 0, 70, extenderDnsTestNow)
	var valuesLock sync.Mutex
	var values []string
	dohUrl, certificate := newExtenderDnsFakeDoh(t, func(query dnsmessage.Message) (int, []byte) {
		valuesLock.Lock()
		defer valuesLock.Unlock()
		return http.StatusOK, extenderDnsFakeDohBody(t, query, values, nil)
	})
	pool := x509.NewCertPool()
	pool.AddCert(certificate)
	monitorRunner := &runner{cfg: &monitorConfig{env: "main", commandTimeout: 5 * time.Second}, dohTLSConfig: &tls.Config{RootCAs: pool}}

	for _, test := range []struct {
		name   string
		values []string
		cut    bool
	}{{"under 16 KiB", small, false}, {"over 16 KiB", large, true}} {
		valuesLock.Lock()
		values = test.values
		valuesLock.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		status, body, err := monitorRunner.dnsDohTxt(ctx, dohUrl, "extender.bringyour.com", 64*1024, netip.Prefix{})
		if err != nil || status != http.StatusOK {
			cancel()
			t.Fatalf("%s: %d %v", test.name, status, err)
		}
		answer := extenderDnsDohAnswer(body, ExtenderDnsClientDohResponseBytes, 64*1024)

		settings := connect.DefaultDohSettings()
		settings.Log = connect.NewNoopLogger()
		settings.DnsResolverSettings = &connect.DnsResolverSettings{
			EnableRemoteDoh:   true,
			RemoteDohUrlsIpv4: []string{dohUrl},
			TlsConfig:         &tls.Config{RootCAs: pool},
		}
		clientTxts := connect.DohQueryTxt(ctx, settings, "extender.bringyour.com")
		cancel()

		if !slices.Equal(answer.kept, clientTxts) {
			t.Fatalf("%s: the monitor keeps %d records, the client %d", test.name, len(answer.kept), len(clientTxts))
		}
		if answer.served != len(test.values) || (len(answer.kept) < len(test.values)) != test.cut || len(answer.kept) == 0 {
			t.Fatalf("%s: kept %d of %d served, %d bytes", test.name, len(answer.kept), answer.served, len(body))
		}
		if test.cut && len(body) <= ExtenderDnsClientDohResponseBytes {
			t.Fatalf("%s: the body is %d bytes", test.name, len(body))
		}
	}
}

// A fake authoritative nameserver that answers by the client subnet of the
// question, recording each subnet; set for none.
func newExtenderDnsFakeAuthority(t *testing.T, set func(subnet netip.Prefix) []string, asked func(subnet netip.Prefix)) string {
	return newExtenderDnsFakeResolver(t, func(tcp bool, query dnsmessage.Message) *dnsmessage.Message {
		if query.Header.RecursionDesired || !tcp {
			t.Errorf("an authoritative question with recursion desired %t over tcp %t", query.Header.RecursionDesired, tcp)
			return nil
		}
		subnet := netip.Prefix{}
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
					return nil
				}
				var address [4]byte
				copy(address[:], one.Data[4:])
				subnet = netip.PrefixFrom(netip.AddrFrom4(address), int(one.Data[2]))
			}
		}
		asked(subnet)
		return extenderDnsFakeTxtResponse(query, set(subnet), func(message *dnsmessage.Message) { message.Header.Authoritative = true })
	})
}

// The client subnet goes out as RFC 7871 says (family 1, the prefix length,
// scope 0, the prefix's three bytes), and the nameserver's answer for it
// comes back whole over TCP.
func TestDnsTxtAuthoritativeSendsTheClientSubnet(t *testing.T) {
	var stateLock sync.Mutex
	asked := []string{}
	endpoint := newExtenderDnsFakeAuthority(t, func(subnet netip.Prefix) []string {
		if subnet.IsValid() {
			return []string{"location " + subnet.String()}
		}
		return []string{"vantage"}
	}, func(subnet netip.Prefix) {
		stateLock.Lock()
		defer stateLock.Unlock()
		asked = append(asked, subnet.String())
	})
	monitorRunner := &runner{cfg: &monitorConfig{env: "main", commandTimeout: 5 * time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	located, err := monitorRunner.dnsTxtAuthoritative(ctx, "extender.bringyour.com", []string{endpoint}, netip.MustParsePrefix("203.0.113.77/24"))
	if err != nil || strings.Join(located.Texts, ",") != "location 203.0.113.0/24" || !located.Authoritative {
		t.Fatalf("located = %+v %v", located, err)
	}
	vantage, err := monitorRunner.dnsTxtAuthoritative(ctx, "extender.bringyour.com", []string{"127.0.0.1:1", endpoint}, netip.Prefix{})
	if err != nil || strings.Join(vantage.Texts, ",") != "vantage" {
		t.Fatalf("vantage = %+v %v", vantage, err)
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	if strings.Join(asked, ",") != "203.0.113.0/24,invalid Prefix" {
		t.Fatalf("asked %v", asked)
	}
}

// A truncated UDP answer whose TCP retry fails is an answer a client's
// resolver cannot complete either: a truncated answer with none, not an
// unreachable resolver.
func TestDnsWireQueryReportsATruncatedRetry(t *testing.T) {
	values := extenderDnsCountryTestValues(t, 0, 4, extenderDnsTestNow)
	endpoint := newExtenderDnsFakeResolver(t, func(tcp bool, query dnsmessage.Message) *dnsmessage.Message {
		if tcp {
			return nil
		}
		return extenderDnsFakeTxtResponse(query, values, nil)
	})
	monitorRunner := &runner{cfg: &monitorConfig{env: "main", commandTimeout: 5 * time.Second}}
	observation, err := monitorRunner.dnsTxt(context.Background(), endpoint, "extender.bringyour.com", netip.Prefix{})
	var retry *dnsTruncatedRetryError
	if !errors.As(err, &retry) {
		t.Fatalf("observation %+v, err %v, want a truncated retry", observation, err)
	}
	answer := extenderDnsPlainAnswer(observation, err)
	if answer.unreachable != "" || !answer.truncatedFlag || len(answer.kept) != 0 {
		t.Fatalf("answer = %+v", answer)
	}
}

// The whole half over fake servers on loopback, through the production
// transports: DoH with a client's question and read, plain DNS over UDP then
// TCP, and the authoritative set by client subnet.
func TestExtenderDnsCountriesOverFakeServers(t *testing.T) {
	fresh := extenderDnsCountryTestValues(t, 0, 15, extenderDnsTestNow.Add(-4*time.Minute))
	lagging := extenderDnsCountryTestValues(t, 0, 15, extenderDnsTestNow.Add(-3*time.Hour))
	oversize := append(append([]string{}, fresh...), extenderDnsCountryTestValues(t, 100, 75, extenderDnsTestNow.Add(-4*time.Minute))...)

	dohServer := func(values []string, edit func(message *dnsmessage.Message), status int) (string, *x509.Certificate) {
		return newExtenderDnsFakeDoh(t, func(query dnsmessage.Message) (int, []byte) {
			if status != http.StatusOK {
				return status, nil
			}
			return status, extenderDnsFakeDohBody(t, query, values, edit)
		})
	}
	freshDoh, certificate := dohServer(fresh, nil, http.StatusOK)
	cutDoh, _ := dohServer(oversize, nil, http.StatusOK)
	truncatedDoh, _ := dohServer(nil, func(message *dnsmessage.Message) { message.Header.Truncated = true }, http.StatusOK)
	filteredDoh, _ := dohServer(fresh, func(message *dnsmessage.Message) {
		var header dnsmessage.ResourceHeader
		header.SetEDNS0(1232, dnsmessage.RCodeSuccess, false)
		message.Additionals = append(message.Additionals, dnsmessage.Resource{Header: header, Body: &dnsmessage.OPTResource{
			Options: []dnsmessage.Option{{Code: extenderDnsEdeOptionCode, Data: binary.BigEndian.AppendUint16(nil, 15)}},
		}})
	}, http.StatusOK)
	forbiddenDoh, _ := dohServer(nil, nil, http.StatusForbidden)

	plain := func(values []string, edit func(message *dnsmessage.Message)) string {
		return newExtenderDnsFakeResolver(t, func(_ bool, query dnsmessage.Message) *dnsmessage.Message {
			return extenderDnsFakeTxtResponse(query, values, edit)
		})
	}
	freshPlain := plain(fresh, nil)
	laggingPlain := plain(lagging, nil)
	droppedPlain := plain(nil, nil)
	refusedPlain := plain(nil, func(message *dnsmessage.Message) { message.Header.RCode = dnsmessage.RCodeRefused })
	tcpDropPlain := newExtenderDnsFakeResolver(t, func(tcp bool, query dnsmessage.Message) *dnsmessage.Message {
		if tcp {
			return nil
		}
		return extenderDnsFakeTxtResponse(query, fresh, nil)
	})
	// the monitor host's router on 2026-11-11: the A question answered, the
	// TXT question dropped over UDP and TCP
	routerPlain := newExtenderDnsFakeResolver(t, func(_ bool, query dnsmessage.Message) *dnsmessage.Message {
		if query.Questions[0].Type != dnsmessage.TypeA {
			return nil
		}
		return &dnsmessage.Message{
			Header:    dnsmessage.Header{ID: query.Header.ID, Response: true, RecursionDesired: true, RecursionAvailable: true},
			Questions: query.Questions,
			Answers: []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{Name: query.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
				Body:   &dnsmessage.AResource{A: [4]byte{192, 0, 2, 61}},
			}},
		}
	})
	closed, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedPlain := closed.LocalAddr().String()
	closed.Close()

	var stateLock sync.Mutex
	subnets := []string{}
	authority := newExtenderDnsFakeAuthority(t, func(netip.Prefix) []string { return fresh }, func(subnet netip.Prefix) {
		stateLock.Lock()
		defer stateLock.Unlock()
		subnets = append(subnets, subnet.String())
	})

	named := func(name string, transport string, endpoint string) ExtenderDnsResolver {
		return ExtenderDnsResolver{Name: name, Transport: transport, Endpoint: endpoint}
	}
	settings := extenderDnsTestCountrySettings(
		ExtenderDnsCountry{Code: ExtenderDnsDefaultCountry, Doh: []ExtenderDnsResolver{
			named("DoH fresh", ExtenderDnsTransportDoh, freshDoh),
			named("DoH cut", ExtenderDnsTransportDoh, cutDoh),
		}},
		ExtenderDnsCountry{Code: "cn", Doh: []ExtenderDnsResolver{
			named("DoH truncated", ExtenderDnsTransportDoh, truncatedDoh),
			named("DoH filtered", ExtenderDnsTransportDoh, filteredDoh),
			named("DoH forbidden", ExtenderDnsTransportDoh, forbiddenDoh),
		}, Dns: []ExtenderDnsResolver{
			named("Plain fresh", ExtenderDnsTransportDns, freshPlain),
		}},
		ExtenderDnsCountry{Code: "ir", Dns: []ExtenderDnsResolver{
			named("Lagging", ExtenderDnsTransportDns, laggingPlain),
			named("Dropped", ExtenderDnsTransportDns, droppedPlain),
			named("TCP dropped", ExtenderDnsTransportDns, tcpDropPlain),
			named("Router", ExtenderDnsTransportDns, routerPlain),
			named("Refused", ExtenderDnsTransportDns, refusedPlain),
			named("Closed", ExtenderDnsTransportDns, closedPlain),
		}},
	)
	settings.Resolvers = []string{freshPlain}
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
	requireStormStates(t, "fake servers", states, map[string]string{
		"extender-dns-records-stale main/extender-dns-records":                  "healthy",
		"extender-dns-country-unobservable main/extender-dns-records/countries": "healthy",
		stale("default/DoH fresh"):                                              "healthy",
		partial("default/DoH fresh"):                                            "healthy",
		partial("default/DoH cut"):                                              "warn",
		stale("cn/DoH truncated"):                                               "warn",
		stale("cn/DoH filtered"):                                                "warn",
		stale("cn/DoH forbidden"):                                               "hold",
		stale("cn/Plain fresh"):                                                 "healthy",
		partial("cn/Plain fresh"):                                               "healthy",
		stale("ir/Lagging"):                                                     "warn",
		stale("ir/Dropped"):                                                     "warn",
		stale("ir/TCP dropped"):                                                 "warn",
		stale("ir/Router"):                                                      "warn",
		stale("ir/Refused"):                                                     "hold",
		partial("ir/Closed"):                                                    "hold",
	})
	for identity, field := range map[string]string{
		partial("default/DoH cut"): "reasons=client_cap",
		stale("cn/DoH truncated"):  "reasons=truncated_empty",
		stale("cn/DoH filtered"):   "reasons=filtered",
		stale("ir/Lagging"):        "reasons=newest_too_old",
		stale("ir/Dropped"):        "reasons=none_served",
		stale("ir/TCP dropped"):    "reasons=truncated_empty",
		stale("ir/Router"):         "reasons=txt_dropped",
	} {
		if !strings.Contains(observed[identity], field) {
			t.Fatalf("%s lost %q: %s", identity, field, observed[identity])
		}
	}
	if !strings.Contains(observed[stale("ir/Dropped")], "path_unreachable=Refused,Closed") {
		t.Fatalf("Dropped: %s", observed[stale("ir/Dropped")])
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	if !slices.Contains(subnets, "invalid Prefix") || !slices.Contains(subnets, "127.0.0.0/24") {
		t.Fatalf("the authority was asked for %v, want the set from here and the cut answer's location", subnets)
	}
}
