// Regional DNS belongs to one provider tunnel and never uses the host resolver.
package providertunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"golang.org/x/net/dns/dnsmessage"
)

// The actual Open boundary must receive the tunnel's own resolver: explicit
// remote settings take precedence, and every other provider, a regional
// recommendation's included, keeps the in-tunnel DoH default.
func TestProbeOpenSelectsPrivateRegionalDns(t *testing.T) {
	original := createTun
	t.Cleanup(func() { createTun = original })
	stop := errors.New("synthetic stop at private TUN boundary")
	var captured *connect.DnsResolverSettings
	createTun = func(_ context.Context, resolver *connect.DnsResolverSettings) (*connect.Tun, error) {
		captured = resolver
		return nil, stop
	}
	custom := &connect.DnsResolverSettings{EnableRemoteDoh: true, RemoteDohUrlsIpv4: []string{"https://custom.example/dns-query"}}
	for _, test := range []struct {
		country string
		custom  *connect.DnsResolverSettings
		want    *connect.DnsResolverSettings
	}{
		{country: " CN ", want: inTunnelOnlyDnsResolverSettings()},
		{country: "zz", want: inTunnelOnlyDnsResolverSettings()},
		{country: "cn", custom: custom, want: custom},
	} {
		cfg := probeTransportBudgetTestConfig()
		cfg.ProviderCountry, cfg.DnsResolverSettings = test.country, test.custom
		_, err := Open(t.Context(), cfg, connect.NewId())
		if !errors.Is(err, stop) || !reflect.DeepEqual(captured, test.want) {
			t.Fatalf("provider resolver precedence lost for %q: got=%+v want=%+v err=%v", test.country, captured, test.want, err)
		}
		if test.custom != nil && captured == test.custom {
			t.Fatal("private tunnel borrowed mutable custom resolver settings")
		}
	}
}

// The countries with a regional recommendation, lower case and sorted.
func regionalRecommendationCountryCodes(t *testing.T) []string {
	countryCodes := []string{}
	for _, server := range connect.RegionalDnsServers() {
		if !slices.Contains(countryCodes, server.CountryCode) {
			countryCodes = append(countryCodes, server.CountryCode)
		}
	}
	slices.Sort(countryCodes)
	if !slices.Contains(countryCodes, "ru") || !slices.Contains(countryCodes, "kz") {
		t.Fatalf("connect's regional recommendations changed shape: %v", countryCodes)
	}
	return countryCodes
}

// The root cause of the dark ru and kz URL cohorts (2026-11-11): a provider in
// a country with a regional recommendation was resolved with the
// recommendation alone, plaintext DNS, which the provider carries over UDP
// only. An exit that carries TCP but not UDP, such as a provider identity
// behind a proxy that only relays TCP, then failed every URL at DNS there,
// while the same network's exits passed elsewhere. On 2026-11-10 one network's
// exits passed 74% of their URL runs in de, fr, br and gb and 0.1% in ru, and
// 93.8% of all ru URL runs ended in a 5 s DNS timeout. The tunnel's own
// resolver must be the in-tunnel DoH, which travels over TCP, in every such
// country, as in every other.
func TestProbeTunnelResolvesOverTcpWhereARegionalRecommendationExists(t *testing.T) {
	original := createTun
	t.Cleanup(func() { createTun = original })
	stop := errors.New("synthetic stop at private TUN boundary")
	for _, countryCode := range regionalRecommendationCountryCodes(t) {
		var captured *connect.DnsResolverSettings
		createTun = func(_ context.Context, resolver *connect.DnsResolverSettings) (*connect.Tun, error) {
			captured = resolver
			return nil, stop
		}
		cfg := probeTransportBudgetTestConfig()
		cfg.ProviderCountry = countryCode
		if _, err := Open(t.Context(), cfg, connect.NewId()); !errors.Is(err, stop) {
			t.Fatalf("%s: Open did not reach its tun: %v", countryCode, err)
		}
		if captured == nil || !captured.EnableRemoteDoh || captured.EnableRemoteDns {
			t.Fatalf("%s: the tunnel resolves without DoH, over UDP alone: %+v", countryCode, captured)
		}
		assertInTunnelOnlyResolver(t, captured)
	}
}

// A regional recommendation races the tunnel's DoH as the private copy the
// tunnel used before; an explicit override, an unknown country and a refused
// host-side override race nothing.
func TestProbeRegionalRecommendationRacesTheTunnelDoh(t *testing.T) {
	for _, countryCode := range regionalRecommendationCountryCodes(t) {
		tunResolver, raceResolver, err := providerDnsResolvers(Config{ProviderCountry: " " + strings.ToUpper(countryCode) + " "})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(tunResolver, inTunnelOnlyDnsResolverSettings()) {
			t.Fatalf("%s: the tunnel left the in-tunnel DoH: %+v", countryCode, tunResolver)
		}
		recommendation := connect.RegionalDnsResolverSettings(countryCode)
		if !reflect.DeepEqual(raceResolver, recommendation) {
			t.Fatalf("%s: the race lost the regional recommendation: got=%+v want=%+v", countryCode, raceResolver, recommendation)
		}
		if raceResolver.EnableLocalDns || raceResolver.EnableLocalDoh || 0 < len(raceResolver.LocalDnsIpv4) || 0 < len(raceResolver.LocalDohUrlsIpv4) {
			t.Fatalf("%s: the race reaches a host-side resolver: %+v", countryCode, raceResolver)
		}
		raceResolver.RemoteDnsIpv4[0] = "192.0.2.53"
		if connect.RegionalDnsResolverSettings(countryCode).RemoteDnsIpv4[0] == "192.0.2.53" {
			t.Fatalf("%s: the race borrowed connect's recommendation", countryCode)
		}
	}

	custom := &connect.DnsResolverSettings{EnableRemoteDns: true, RemoteDnsIpv4: []string{"192.0.2.53"}}
	for _, cfg := range []Config{{ProviderCountry: "zz"}, {}, {ProviderCountry: "ru", DnsResolverSettings: custom}} {
		tunResolver, raceResolver, err := providerDnsResolvers(cfg)
		if err != nil || tunResolver == nil || raceResolver != nil {
			t.Fatalf("%q custom=%t: unexpected race: tun=%+v race=%+v err=%v", cfg.ProviderCountry, cfg.DnsResolverSettings != nil, tunResolver, raceResolver, err)
		}
	}
	if _, _, err := providerDnsResolvers(Config{ProviderCountry: "ru", DnsResolverSettings: &connect.DnsResolverSettings{EnableLocalDns: true}}); !errors.Is(err, ErrOffTunnelDns) {
		t.Fatalf("off-tunnel custom resolver was accepted: %v", err)
	}
}

// Open builds the racing cache for its own tun alone, from the regional
// recommendation, dialing the recommendation's IPv4 servers on port 53 through
// the supplied dialer, and Close retires it: a lookup after Close dials
// nothing. Other providers get no racing cache.
func TestProbeOpenOwnsAndClosesTheRaceCache(t *testing.T) {
	original := newRaceDohCache
	t.Cleanup(func() { newRaceDohCache = original })
	recommendation := connect.RegionalDnsResolverSettings("ru")
	var dialLock sync.Mutex
	dialAddresses := []string{}
	var built *connect.DohCache
	buildCount := 0
	newRaceDohCache = func(tun *connect.Tun, resolver *connect.DnsResolverSettings) *connect.DohCache {
		buildCount++
		if !reflect.DeepEqual(resolver, recommendation) || tun.Ipv6Enabled() {
			t.Errorf("racing cache built from %+v over ipv6=%t", resolver, tun.Ipv6Enabled())
		}
		dial := func(ctx context.Context, network string, address string) (net.Conn, error) {
			dialLock.Lock()
			defer dialLock.Unlock()
			dialAddresses = append(dialAddresses, network+" "+address)
			return nil, errors.New("synthetic provider path unavailable")
		}
		built = newTunnelDohCache(dial, tun.Ipv6Enabled(), resolver)
		return built
	}
	dials := func() []string {
		dialLock.Lock()
		defer dialLock.Unlock()
		return slices.Clone(dialAddresses)
	}

	cfg := probeTransportBudgetTestConfig()
	cfg.ProviderCountry = "ru"
	tunnel, err := Open(t.Context(), cfg, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	if buildCount != 1 || tunnel.raceDohCache == nil || tunnel.raceDohCache != built || tunnel.raceDohCache == tunnel.tun.DohCache() {
		t.Fatalf("Open does not own its racing cache: builds=%d", buildCount)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if addrs, _ := tunnel.raceDohCache.QueryResult(ctx, "A", "race-owner.example"); len(addrs) != 0 || len(dials()) == 0 {
		t.Fatalf("the racing cache did not dial through its tunnel: addrs=%v dials=%d", addrs, len(dials()))
	}
	for _, dialAddress := range dials() {
		network, address, _ := strings.Cut(dialAddress, " ")
		host, port, err := net.SplitHostPort(address)
		if err != nil || network != "udp" || port != "53" || !slices.Contains(recommendation.RemoteDnsIpv4, host) {
			t.Fatalf("the racing cache dialed outside the recommendation's IPv4 servers: %s", dialAddress)
		}
	}
	if err := tunnel.Close(); err != nil {
		t.Fatal(err)
	}
	dialsBeforeLookup := len(dials())
	if addrs, authoritative := built.QueryResult(ctx, "A", "after-close.example"); len(addrs) != 0 || authoritative || len(dials()) != dialsBeforeLookup {
		t.Fatalf("Close left the racing cache live: dials=%d before=%d", len(dials()), dialsBeforeLookup)
	}

	cfg = probeTransportBudgetTestConfig()
	cfg.ProviderCountry = "zz"
	other, err := Open(t.Context(), cfg, connect.NewId())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if buildCount != 1 || other.raceDohCache != nil {
		t.Fatalf("a provider without a recommendation got a racing cache: builds=%d", buildCount)
	}
}

// Even an explicit override cannot resolve provider targets from the operator.
func TestProbeRejectsOffTunnelCustomDns(t *testing.T) {
	for _, custom := range []*connect.DnsResolverSettings{{EnableLocalDns: true}, {EnableLocalDoh: true}} {
		cfg := probeTransportBudgetTestConfig()
		cfg.DnsResolverSettings = custom
		tunnel, err := Open(t.Context(), cfg, connect.NewId())
		if tunnel != nil {
			_ = tunnel.Close()
		}
		if !errors.Is(err, ErrOffTunnelDns) {
			t.Fatalf("off-tunnel custom resolver was accepted: %v", err)
		}
	}
}

// The existing SDK resolver rotates its configured regional servers after a
// failed dial. Its successful response traverses the supplied tunnel dialer;
// the test never opens a real DNS socket or invents a regional DoH URL.
func TestProbeRegionalDnsUsesConfiguredServerFallback(t *testing.T) {
	resolver, err := providerDnsResolverSettings(Config{ProviderCountry: "cn"})
	if err != nil || len(resolver.RemoteDnsIpv4) < 2 {
		t.Fatalf("missing SDK regional recommendation: %v", err)
	}
	settings := connect.DefaultDohSettings()
	settings.IpVersion = 4
	settings.DnsResolverSettings = resolver
	var calls atomic.Int32
	finished := make(chan struct{})
	settings.DialContextSettings = &connect.DialContextSettings{DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "53" || !slices.Contains(resolver.RemoteDnsIpv4, host) {
			return nil, errors.New("escaped configured regional resolvers")
		}
		call := calls.Add(1)
		if call == 1 {
			return nil, errors.New("synthetic first regional server unavailable")
		}
		if call != 2 {
			return nil, errors.New("unexpected extra regional lookup")
		}
		client, server := net.Pipe()
		go func() {
			defer close(finished)
			defer server.Close()
			var size [2]byte
			if _, err := io.ReadFull(server, size[:]); err != nil {
				t.Error(err)
				return
			}
			payload := make([]byte, int(binary.BigEndian.Uint16(size[:])))
			if _, err := io.ReadFull(server, payload); err != nil {
				t.Error(err)
				return
			}
			var query dnsmessage.Message
			if err := query.Unpack(payload); err != nil || len(query.Questions) != 1 {
				t.Error("invalid synthetic query")
				return
			}
			question := query.Questions[0]
			response := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true}, Questions: query.Questions,
				Answers: []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 77}}}}}
			encoded, err := response.Pack()
			if err != nil {
				t.Error(err)
				return
			}
			binary.BigEndian.PutUint16(size[:], uint16(len(encoded)))
			_, err = server.Write(append(size[:], encoded...))
			if err != nil {
				t.Error(err)
			}
		}()
		return client, nil
	}}
	cache := connect.NewDohCache(settings)
	defer cache.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	addrs, authoritative := cache.QueryResult(ctx, "A", "regional-probe.example")
	if !authoritative || len(addrs) != 1 || addrs[0].String() != "192.0.2.77" || calls.Load() != 2 {
		t.Fatalf("configured regional fallback failed: authoritative=%t calls=%d", authoritative, calls.Load())
	}
	<-finished
}

// An unavailable regional configuration remains bounded and nonauthoritative;
// it cannot fall back to the host's resolver or certify a synthetic answer.
func TestProbeRegionalDnsFailureKeepsHardDeadline(t *testing.T) {
	// Even an injected net.Resolver dial initializes process-global DNS state.
	// Keep this actual plaintext lookup outside any synctest bubble.
	resolver, err := providerDnsResolverSettings(Config{ProviderCountry: "cn"})
	if err != nil {
		t.Fatal(err)
	}
	if !resolver.EnableRemoteDns || resolver.EnableRemoteDoh || resolver.EnableLocalDns || resolver.EnableLocalDoh {
		t.Fatal("regional deadline fixture must use only private plaintext DNS")
	}
	settings := connect.DefaultDohSettings()
	settings.IpVersion, settings.DnsResolverSettings = 4, resolver
	var calls atomic.Int32
	settings.DialContextSettings = &connect.DialContextSettings{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || network != "udp" || port != "53" || !slices.Contains(resolver.RemoteDnsIpv4, host) {
			t.Error("regional failure escaped the private remote UDP path")
			return nil, errors.New("unexpected regional DNS dial")
		}
		calls.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	cache := connect.NewDohCache(settings)
	defer cache.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	addrs, authoritative := cache.QueryResult(ctx, "A", "regional-probe.example")
	if authoritative || len(addrs) != 0 || ctx.Err() != context.DeadlineExceeded || calls.Load() == 0 {
		t.Fatalf("regional failure escaped deadline: authoritative=%t calls=%d err=%v", authoritative, calls.Load(), ctx.Err())
	}
}
