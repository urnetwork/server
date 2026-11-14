// A URL request forced to one IP family resolves only that family's record
// and dials only that family's addresses, with no fallback across families,
// and a provider that advertises IPv6 gets a tunnel that carries it.
package providertunnel

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// A resolver whose answers are per record type, recording every record type
// it was asked for and every socket the request dialed.
type familyTestResolver struct {
	answers map[string][]netip.Addr

	stateLock sync.Mutex
	queries   []string
	dials     []string
	dialed    [][]netip.Addr
}

func (self *familyTestResolver) resolver() *providerUrlResolver {
	return &providerUrlResolver{
		query: func(_ context.Context, recordType string, host string) ([]netip.Addr, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.queries = append(self.queries, recordType)
			return self.answers[recordType], true
		},
		dial: func(_ context.Context, network string, address string, addrs []netip.Addr) (net.Conn, error) {
			self.stateLock.Lock()
			self.dials = append(self.dials, network+" "+address)
			self.dialed = append(self.dialed, slices.Clone(addrs))
			self.stateLock.Unlock()
			left, right := net.Pipe()
			right.Close()
			return left, nil
		},
	}
}

func (self *familyTestResolver) snapshot() ([]string, []string, [][]netip.Addr) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.queries), slices.Clone(self.dials), slices.Clone(self.dialed)
}

// A scoped URL request's context, granted for one family.
func familyTestGrant(t *testing.T, target string, ipVersion int) context.Context {
	t.Helper()
	transport := &providerHttpTransport{}
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := transport.ProviderUrlProbeFamilyContext(context.Background(), parsed, false, ipVersion)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// Each family asks only its own record and dials only its own network and
// addresses: IPv4 the A record over tcp4, IPv6 the AAAA record over tcp6. A
// resolver that answers both records, and even mixes families into one
// answer, cannot move a request to the other family.
func TestUrlRequestResolvesAndDialsOnlyItsFamily(t *testing.T) {
	v4 := netip.MustParseAddr("8.8.4.4")
	v6 := netip.MustParseAddr("2606:4700:4700::1111")
	mapped := netip.MustParseAddr("::ffff:8.8.8.8")
	for _, c := range []struct {
		ipVersion int
		query     string
		network   string
		addrs     []netip.Addr
	}{
		{4, "A", "tcp4", []netip.Addr{v4}},
		{6, "AAAA", "tcp6", []netip.Addr{v6}},
	} {
		fake := &familyTestResolver{answers: map[string][]netip.Addr{
			"A":    {v4, v6},
			"AAAA": {v6, mapped, v4},
		}}
		resolver := fake.resolver()
		ctx := familyTestGrant(t, "https://dual.example/", c.ipVersion)
		ctx, observation := traceProviderHttpDial(ctx, "tcp", "dual.example:443")
		conn, err := resolver.dialContext(ctx, "tcp", "dual.example:443", observation)
		observation.finish(err)
		if err != nil {
			t.Fatalf("IPv%d: %v", c.ipVersion, err)
		}
		conn.Close()
		queries, dials, dialed := fake.snapshot()
		if !slices.Equal(queries, []string{c.query}) || !slices.Equal(dials, []string{c.network + " dual.example:443"}) || !reflect.DeepEqual(dialed, [][]netip.Addr{c.addrs}) {
			t.Fatalf("IPv%d: queries=%v dials=%v addrs=%v; want %s alone, %s, %v", c.ipVersion, queries, dials, dialed, c.query, c.network, c.addrs)
		}
	}
}

// No fallback across families: a URL with no AAAA record fails its IPv6
// request at DNS (the leg is not measured), although its A record answers,
// and nothing is dialed; and an IPv4 request of a URL with no A record never
// reaches its AAAA address.
func TestUrlRequestNeverFallsBackToTheOtherFamily(t *testing.T) {
	for _, c := range []struct {
		ipVersion int
		answers   map[string][]netip.Addr
		query     string
	}{
		{6, map[string][]netip.Addr{"A": {netip.MustParseAddr("8.8.4.4")}}, "AAAA"},
		{4, map[string][]netip.Addr{"AAAA": {netip.MustParseAddr("2606:4700:4700::1111")}}, "A"},
	} {
		fake := &familyTestResolver{answers: c.answers}
		resolver := fake.resolver()
		ctx := familyTestGrant(t, "https://one-family.example/", c.ipVersion)
		ctx, observation := traceProviderHttpDial(ctx, "tcp", "one-family.example:443")
		_, err := resolver.dialContext(ctx, "tcp", "one-family.example:443", observation)
		stage := observation.finish(err)
		var dnsErr *net.DNSError
		queries, dials, _ := fake.snapshot()
		if err == nil || !errors.As(err, &dnsErr) || !dnsErr.IsNotFound || stage != "dial_dns" || !slices.Equal(queries, []string{c.query}) || len(dials) != 0 {
			t.Fatalf("IPv%d: err=%v stage=%s queries=%v dials=%v; want an authoritative miss at DNS and no socket", c.ipVersion, err, stage, queries, dials)
		}
	}
}

// Only public addresses of the request's family are URL targets: an IPv6
// request refuses an answer in a special-purpose IPv6 block before any
// socket, as an IPv4 request refuses a private one.
func TestUrlRequestRefusesNonPublicAnswersOfItsFamily(t *testing.T) {
	for _, answer := range []string{"fd00::1", "fe80::1", "::1", "2001:db8::1", "64:ff9b::808:808", "2002:808:808::1", "2001::1", "ff02::1", "3fff::1", "100::1"} {
		fake := &familyTestResolver{answers: map[string][]netip.Addr{"AAAA": {netip.MustParseAddr(answer)}}}
		ctx := familyTestGrant(t, "https://special.example/", 6)
		ctx, observation := traceProviderHttpDial(ctx, "tcp", "special.example:443")
		_, err := fake.resolver().dialContext(ctx, "tcp", "special.example:443", observation)
		observation.finish(err)
		var staged *providerHttpStageError
		_, dials, _ := fake.snapshot()
		if !errors.As(err, &staged) || staged.ProviderHttpStage() != "policy" || len(dials) != 0 {
			t.Fatalf("%s: err=%v dials=%v; a special-purpose IPv6 answer reached a socket", answer, err, dials)
		}
	}
	for answer, public := range map[string]bool{
		"2606:4700:4700::1111": true, "2a00:1450:4001:80b::200e": true, "2620:fe::10": true,
		// outside 2001::/23, which ends at 2001:1ff:ffff::
		"2001:4860:4860::8888": true,
		// teredo and benchmarking, inside 2001::/23, refused whole for URL targets
		"2001:0:4136:e378::1": false, "2001:2::1": false,
		"::ffff:8.8.8.8": false, "8.8.8.8": false,
	} {
		if got := publicUrlProbeAddressFor(netip.MustParseAddr(answer), 6); got != public {
			t.Errorf("%s: public IPv6 URL target %t, want %t", answer, got, public)
		}
	}
	if !publicUrlProbeAddressFor(netip.MustParseAddr("8.8.8.8"), 4) || publicUrlProbeAddressFor(netip.MustParseAddr("2606:4700:4700::1111"), 4) {
		t.Error("the IPv4 rule changed")
	}
}

// A literal host is a family of its own: an IPv6 request of an IPv4 literal
// is refused when it resolves, at the policy stage that measures nothing, and
// an IPv4 request of an IPv6 literal is refused at its grant, as before.
func TestUrlRequestLiteralHostKeepsItsFamily(t *testing.T) {
	transport := &providerHttpTransport{}
	v4Literal, _ := url.Parse("https://8.8.8.8/")
	v6Literal, _ := url.Parse("https://[2606:4700:4700::1111]/")
	if _, err := transport.ProviderUrlProbeContext(context.Background(), v6Literal, false); err == nil {
		t.Fatal("an IPv4 request granted an IPv6 literal")
	}
	if _, err := transport.ProviderUrlProbeFamilyContext(context.Background(), v4Literal, false, 5); err == nil {
		t.Fatal("a grant accepted an IP version that is neither 4 nor 6")
	}
	ctx, err := transport.ProviderUrlProbeFamilyContext(context.Background(), v4Literal, false, 6)
	if err != nil {
		t.Fatal(err)
	}
	fake := &familyTestResolver{}
	ctx, observation := traceProviderHttpDial(ctx, "tcp", "8.8.8.8:443")
	_, err = fake.resolver().dialContext(ctx, "tcp", "8.8.8.8:443", observation)
	observation.finish(err)
	var staged *providerHttpStageError
	queries, dials, _ := fake.snapshot()
	if !errors.As(err, &staged) || staged.ProviderHttpStage() != "policy" || len(queries) != 0 || len(dials) != 0 {
		t.Fatalf("err=%v queries=%v dials=%v; an IPv4 literal reached an IPv6 socket", err, queries, dials)
	}
	ctx, err = transport.ProviderUrlProbeFamilyContext(context.Background(), v6Literal, false, 6)
	if err != nil {
		t.Fatal(err)
	}
	ctx, observation = traceProviderHttpDial(ctx, "tcp", "[2606:4700:4700::1111]:443")
	conn, err := fake.resolver().dialContext(ctx, "tcp", "[2606:4700:4700::1111]:443", observation)
	observation.finish(err)
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, dials, _ := fake.snapshot(); !slices.Equal(dials, []string{"tcp6 [2606:4700:4700::1111]:443"}) {
		t.Fatalf("dials=%v", dials)
	}
}

// A URL's HTTP/3 leg forced to IPv6 resolves the AAAA record alone, opens a
// UDP socket of the IPv6 family alone, and sizes its QUIC datagrams to the
// IPv6 link: the tunnel's 1280-byte MTU less the 40-byte IPv6 header and UDP.
func TestH3LegOverIpv6DialsUdp6AndFitsTheIpv6Link(t *testing.T) {
	saved := urlProbeQuicHandshakeIdleTimeout
	urlProbeQuicHandshakeIdleTimeout = 300 * time.Millisecond
	t.Cleanup(func() { urlProbeQuicHandshakeIdleTimeout = saved })

	blackhole := startH3TestBlackhole(t)
	path := &h3TestPacketPath{target: blackhole.conn.LocalAddr().(*net.UDPAddr)}
	var asked sync.Map
	resolver := &providerUrlResolver{query: func(_ context.Context, recordType string, _ string) ([]netip.Addr, bool) {
		asked.Store(recordType, true)
		if recordType == "AAAA" {
			return []netip.Addr{netip.MustParseAddr("2606:4700:4700::1111")}, true
		}
		return []netip.Addr{netip.MustParseAddr("8.8.4.4")}, true
	}}
	_, client := h3TestClient(t, path, resolver, nil, []string{"dual.example"})
	target, _ := url.Parse("https://dual.example/")
	ctx, err := client.Transport.(*providerH3RoundTripper).ProviderUrlProbeFamilyContext(context.Background(), target, false, 6)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if response, err := client.Do(request); err == nil {
		response.Body.Close()
		t.Fatal("a blackholed path answered HTTP/3")
	}
	dials, _ := path.snapshot()
	if !slices.Equal(dials, []string{"udp6 [2606:4700:4700::1111]:443", "udp6 [2606:4700:4700::1111]:443"}) {
		t.Fatalf("dials=%v; want IPv6 UDP sockets to the AAAA answer alone: the handshake and its one retry", dials)
	}
	if _, a := asked.Load("A"); a {
		t.Fatal("an IPv6 HTTP/3 leg asked for the A record")
	}
	sizes := blackhole.datagramSizes()
	if len(sizes) == 0 {
		t.Fatal("no QUIC datagram reached the provider path")
	}
	for _, size := range sizes {
		if size+40+8 > connect.DefaultMtu || size < 1200 {
			t.Fatalf("datagram of %d bytes does not fit the tunnel's 1280-byte IPv6 link or QUIC's 1200-byte floor", size)
		}
	}
	if config := providerUrlProbeQuicConfigFor(6); config.InitialPacketSize != 1232 {
		t.Fatalf("IPv6 initial packet %d", config.InitialPacketSize)
	}
}

// A provider that advertises IPv6 gets a dual-stack inner tun, so an IPv6 leg
// can reach its site; every other provider keeps the IPv4-only tun.
func TestOpenGivesAProviderThatAdvertisesIpv6ADualStackTun(t *testing.T) {
	for _, c := range []struct {
		family connect.IpFamily
		ipv6   bool
	}{
		{connect.IpFamilyLegacy, false},
		{connect.IpFamilyV4Only, false},
		{connect.IpFamilyDualstack, true},
		{connect.IpFamilyV6Only, true},
	} {
		cfg := dummyOpenConfig()
		cfg.ProviderIpFamily = c.family
		tunnel, err := Open(context.Background(), cfg, connect.NewId())
		if err != nil {
			t.Fatal(err)
		}
		hasIpv6Address := slices.ContainsFunc(tunnel.tun.LocalAddresses(), func(address netip.Addr) bool { return address.Is6() })
		enabled := tunnel.tun.Ipv6Enabled()
		tunnel.Close()
		if enabled != c.ipv6 || hasIpv6Address != c.ipv6 {
			t.Fatalf("%q: ipv6 enabled=%t address=%t, want %t", c.family, enabled, hasIpv6Address, c.ipv6)
		}
	}
}

// The resolver of a dual-stack tunnel keeps to IPv4's servers when the
// provider advertises IPv4, so its DNS rides the IPv4 path it always did and
// only the URL's own request is IPv6; an IPv6-only provider's keeps to IPv6's.
// A tunnel whose provider advertises no IPv6 is handed its settings as they
// were.
func TestDualStackTunnelResolverKeepsToOneFamilysServers(t *testing.T) {
	var captured *connect.DnsResolverSettings
	var dualStack atomic.Bool
	stop := errors.New("synthetic stop at private TUN boundary")
	originalTun, originalDualStackTun := createTun, createDualStackTun
	t.Cleanup(func() { createTun, createDualStackTun = originalTun, originalDualStackTun })
	createTun = func(_ context.Context, resolver *connect.DnsResolverSettings) (*connect.Tun, error) {
		captured = resolver
		dualStack.Store(false)
		return nil, stop
	}
	createDualStackTun = func(_ context.Context, resolver *connect.DnsResolverSettings) (*connect.Tun, error) {
		captured = resolver
		dualStack.Store(true)
		return nil, stop
	}
	base := inTunnelOnlyDnsResolverSettings()
	for _, c := range []struct {
		family   connect.IpFamily
		country  string
		dual     bool
		ipv4     bool
		ipv6     bool
		original bool
	}{
		{connect.IpFamilyV4Only, "", false, true, true, true},
		{connect.IpFamilyLegacy, "", false, true, true, true},
		{connect.IpFamilyDualstack, "", true, true, false, false},
		{connect.IpFamilyDualstack, "cn", true, true, false, false},
		{connect.IpFamilyV6Only, "", true, false, true, false},
	} {
		cfg := probeTransportBudgetTestConfig()
		cfg.ProviderIpFamily, cfg.ProviderCountry = c.family, c.country
		if _, err := Open(t.Context(), cfg, connect.NewId()); !errors.Is(err, stop) {
			t.Fatalf("%q: %v", c.family, err)
		}
		if dualStack.Load() != c.dual || captured == nil {
			t.Fatalf("%q: dual-stack tun %t, want %t", c.family, dualStack.Load(), c.dual)
		}
		ipv4 := 0 < len(captured.RemoteDohUrlsIpv4) || 0 < len(captured.RemoteDnsIpv4)
		ipv6 := 0 < len(captured.RemoteDohUrlsIpv6) || 0 < len(captured.RemoteDnsIpv6)
		if ipv4 != c.ipv4 || ipv6 != c.ipv6 {
			t.Fatalf("%q %q: resolver servers ipv4=%t ipv6=%t, want %t %t", c.family, c.country, ipv4, ipv6, c.ipv4, c.ipv6)
		}
		if c.original && c.country == "" && !reflect.DeepEqual(captured, base) {
			t.Fatalf("%q: a tunnel without IPv6 had its resolver changed: %+v", c.family, captured)
		}
		assertInTunnelOnlyResolverFamily(t, captured)
	}
}

// A family-kept resolver still resolves inside the tunnel alone: no host-side
// toggle and no host-side server.
func assertInTunnelOnlyResolverFamily(t *testing.T, s *connect.DnsResolverSettings) {
	t.Helper()
	if s.EnableLocalDns || s.EnableLocalDoh ||
		0 < len(s.LocalDohUrlsIpv4)+len(s.LocalDohUrlsIpv6)+len(s.LocalDnsIpv4)+len(s.LocalDnsIpv6) {
		t.Fatalf("a family-kept resolver gained a host-side path: %+v", s)
	}
}

// A regional recommendation's resolver, which races the tunnel's own in the
// countries connect recommends one for (cn, ir, kz and ru at connect
// cdbcbf5f; providerDnsResolvers), keeps to the provider's
// family's servers as the tunnel's own does: a dual-stack provider in cn
// races cn's IPv4 servers alone, though cn's recommendation has IPv6 servers
// too, and an IPv4 provider's racing resolver is as it was.
func TestDualStackTunnelRaceResolverKeepsToOneFamilysServers(t *testing.T) {
	if len(connect.RegionalDnsServerIpv6s("cn")) == 0 || len(connect.RegionalDnsServerIps("cn")) == 0 {
		t.Fatal("the fixture needs cn's recommendation to name servers of both families")
	}
	original := newRaceDohCache
	t.Cleanup(func() { newRaceDohCache = original })
	for _, c := range []struct {
		family connect.IpFamily
		ipv6   bool
	}{
		{connect.IpFamilyDualstack, false},
		{connect.IpFamilyV4Only, true},
	} {
		var raced *connect.DnsResolverSettings
		newRaceDohCache = func(tun *connect.Tun, resolver *connect.DnsResolverSettings) *connect.DohCache {
			raced = resolver
			return original(tun, resolver)
		}
		cfg := dummyOpenConfig()
		cfg.ProviderIpFamily, cfg.ProviderCountry = c.family, "cn"
		tunnel, err := Open(context.Background(), cfg, connect.NewId())
		if err != nil {
			t.Fatal(err)
		}
		tunnel.Close()
		if raced == nil || len(raced.RemoteDnsIpv4) == 0 || (0 < len(raced.RemoteDnsIpv6)) != c.ipv6 {
			t.Fatalf("%q: racing resolver %+v; IPv6 servers kept %t, want %t", c.family, raced, raced != nil && 0 < len(raced.RemoteDnsIpv6), c.ipv6)
		}
	}
}
