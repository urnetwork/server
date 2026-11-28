package nginx

// Tests of the concentration gauges (LBALTOPT2 R16): the bucketing of each
// address's connections by bound, the closed label set however many
// addresses connect, and the two ways a connect connection is counted, an h1
// upgrade served in-process until its tunnel closes and a QUIC connection
// until its release.

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The gauges of one family, by bound, as {addresses, connections}.
type testConcentrationValues map[int][2]float64

// metrics' concentration gauges of family, at every bound.
func testConcentrationGauges(t *testing.T, metrics *frontMetrics, family string) testConcentrationValues {
	t.Helper()
	values := testConcentrationValues{}
	for _, bound := range concentrationBounds {
		over := strconv.Itoa(bound)
		values[bound] = [2]float64{
			testutil.ToFloat64(metrics.connectAddresses.WithLabelValues(family, over)),
			testutil.ToFloat64(metrics.connectAddressConnections.WithLabelValues(family, over)),
		}
	}
	return values
}

// Checks one family's gauges against want, by bound.
func checkTestConcentration(t *testing.T, metrics *frontMetrics, family string, want testConcentrationValues) {
	t.Helper()
	got := testConcentrationGauges(t, metrics, family)
	for _, bound := range concentrationBounds {
		if got[bound] != want[bound] {
			t.Errorf("%s over %d: %v addresses and %v connections, want %v and %v", family, bound, got[bound][0], got[bound][1], want[bound][0], want[bound][1])
		}
	}
}

// One address's change moves each bound by what enters and leaves it: an
// address crossing a bound brings or takes all of its connections, and one
// that stays over it moves its connections by the change.
func TestConcentrationBoundDelta(t *testing.T) {
	for _, c := range []struct {
		bound           int
		before          int
		after           int
		wantAddresses   int
		wantConnections int
	}{
		{bound: 0, before: 0, after: 1, wantAddresses: 1, wantConnections: 1},
		{bound: 0, before: 1, after: 0, wantAddresses: -1, wantConnections: -1},
		{bound: 0, before: 5, after: 6, wantAddresses: 0, wantConnections: 1},
		{bound: 10, before: 9, after: 10, wantAddresses: 0, wantConnections: 0},
		// more than the bound, not as many
		{bound: 10, before: 10, after: 11, wantAddresses: 1, wantConnections: 11},
		{bound: 10, before: 11, after: 10, wantAddresses: -1, wantConnections: -11},
		{bound: 10, before: 11, after: 12, wantAddresses: 0, wantConnections: 1},
		{bound: 100, before: 50, after: 49, wantAddresses: 0, wantConnections: 0},
		{bound: 1000, before: 1001, after: 1000, wantAddresses: -1, wantConnections: -1001},
	} {
		addresses, connections := concentrationBoundDelta(c.bound, c.before, c.after)
		if addresses != c.wantAddresses || connections != c.wantConnections {
			t.Errorf("bound %d from %d to %d: %d addresses and %d connections, want %d and %d", c.bound, c.before, c.after, addresses, connections, c.wantAddresses, c.wantConnections)
		}
	}
}

// Addresses count at each bound they hold more connections than, by family,
// with all of their connections; an IPv4-mapped address is its IPv4 address;
// a release is counted once however often it is called; and once every
// connection is released, nothing is left, kept or counted.
func TestFrontConcentrationCountsAddressesOverEachBound(t *testing.T) {
	metrics := newFrontMetrics(prometheus.NewRegistry())
	concentration := newFrontConcentration(metrics)

	releases := []func(){}
	acquire := func(addr string, count int) {
		for range count {
			releases = append(releases, concentration.acquire(netip.MustParseAddr(addr)))
		}
	}
	// a provider farm's address with 1,500 connections, one with 150 and one
	// with 11, on IPv4; the 150 arrive half as IPv4-mapped IPv6
	acquire("198.51.100.1", 1500)
	acquire("198.51.100.2", 75)
	acquire("::ffff:198.51.100.2", 75)
	acquire("198.51.100.3", 11)
	// two with one each, and an address with exactly 10, under every bound
	acquire("198.51.100.4", 1)
	acquire("198.51.100.5", 1)
	acquire("198.51.100.6", 10)
	// on IPv6, one address with 101
	acquire("2001:db8::1", 101)
	acquire("2001:db8::2", 3)

	// six IPv4 addresses: the mapped ones are 198.51.100.2's
	checkTestConcentration(t, metrics, "ipv4", testConcentrationValues{
		0:    {6, 1500 + 150 + 11 + 1 + 1 + 10},
		10:   {3, 1500 + 150 + 11},
		100:  {2, 1500 + 150},
		1000: {1, 1500},
	})
	checkTestConcentration(t, metrics, "ipv6", testConcentrationValues{
		0:    {2, 104},
		10:   {1, 101},
		100:  {1, 101},
		1000: {0, 0},
	})
	if count := concentration.addrCount(); count != 8 {
		t.Errorf("%d addresses kept, want 8", count)
	}

	// releasing the farm's address down to 1,000 takes it out of the top bound
	for _, release := range releases[:500] {
		release()
		release()
	}
	checkTestConcentration(t, metrics, "ipv4", testConcentrationValues{
		0:    {6, 1000 + 150 + 11 + 1 + 1 + 10},
		10:   {3, 1000 + 150 + 11},
		100:  {2, 1000 + 150},
		1000: {0, 0},
	})

	for _, release := range releases {
		release()
	}
	for _, family := range concentrationFamilies {
		checkTestConcentration(t, metrics, family, testConcentrationValues{})
	}
	if count := concentration.addrCount(); count != 0 {
		t.Errorf("%d addresses kept after every release, want 0", count)
	}
}

// However many addresses connect, each gauge has the 8 series of its closed
// labels, the family and the bound, and no label value is an address.
func TestFrontConcentrationLabelsAreBounded(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := newFrontMetrics(registry)
	concentration := newFrontConcentration(metrics)
	for i := range 5000 {
		concentration.acquire(netip.AddrFrom4([4]byte{10, 1, byte(i >> 8), byte(i)}))
		concentration.acquire(netip.MustParseAddr(fmt.Sprintf("2001:db8::%x", i)))
	}
	for range 1500 {
		concentration.acquire(netip.MustParseAddr("203.0.113.9"))
	}

	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, family := range families {
		switch family.GetName() {
		case "urnetwork_front_connect_addresses", "urnetwork_front_connect_address_connections":
		default:
			continue
		}
		found += 1
		if len(family.GetMetric()) != len(concentrationFamilies)*len(concentrationBounds) {
			t.Errorf("%s has %d series, want %d", family.GetName(), len(family.GetMetric()), len(concentrationFamilies)*len(concentrationBounds))
		}
		for _, metric := range family.GetMetric() {
			labelNames := []string{}
			for _, label := range metric.GetLabel() {
				labelNames = append(labelNames, label.GetName())
				if _, err := netip.ParseAddr(label.GetValue()); err == nil {
					t.Errorf("%s has an address as a label value: %s", family.GetName(), label.GetValue())
				}
				switch label.GetName() {
				case "family":
					if !slices.Contains(concentrationFamilies[:], label.GetValue()) {
						t.Errorf("%s family %q", family.GetName(), label.GetValue())
					}
				case "over":
					if !slices.Contains([]string{"0", "10", "100", "1000"}, label.GetValue()) {
						t.Errorf("%s over %q", family.GetName(), label.GetValue())
					}
				}
			}
			slices.Sort(labelNames)
			if !slices.Equal(labelNames, []string{"family", "over"}) {
				t.Errorf("%s labels %v, want family and over", family.GetName(), labelNames)
			}
		}
	}
	if found != 2 {
		t.Fatalf("gathered %d of the 2 concentration gauges", found)
	}
	checkTestConcentration(t, metrics, "ipv4", testConcentrationValues{
		0:    {5001, 6500},
		10:   {1, 1500},
		100:  {1, 1500},
		1000: {1, 1500},
	})
}

// An h1 upgrade to a connect name counts against its address from its
// hand-off to connect until its tunnel closes, after the handler returned;
// one connect refuses without a hijack counts only while its handler runs.
// A connect request that asks for no upgrade, and a forwarded websocket, are
// no connect connections.
func TestFrontConcentrationCountsConnectUpgradesUntilTheTunnelCloses(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	mcp := testRouteService(t, routeTable, "mcp")
	mcp.Websocket = true
	mcp.Streamable = false
	testFront := newTestFront(t, routeTable, testFrontSettings())
	ipv4 := func() [2]float64 {
		return testConcentrationGauges(t, testFront.metrics, "ipv4")[0]
	}

	var tunnel net.Conn
	refuse := false
	inHandler := [2]float64{}
	hijack := func(w http.ResponseWriter) {
		inHandler = ipv4()
		if refuse {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, readWriter, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Fatal(err)
		}
		readWriter.WriteString("HTTP/1.1 101 Switching Protocols\r\n\r\n")
		tunnel = conn
	}
	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		hijack(w)
	}
	testFront.forwarders["mcp"].respond = func(w http.ResponseWriter, r *http.Request, options forwardOptions) {
		hijack(w)
	}
	handler := testFront.front.HttpsHandler("eth1")
	upgrade := func(host string) {
		serverConn, clientConn := net.Pipe()
		t.Cleanup(func() {
			clientConn.Close()
		})
		r := newTestRequestFrom("198.51.100.52:1000", "GET", host, "/", true)
		r.Header.Set("Connection", "Upgrade")
		r.Header.Set("Upgrade", "websocket")
		handler.ServeHTTP(&testHijackResponseWriter{
			ResponseRecorder: httptest.NewRecorder(),
			conn:             serverConn,
		}, r)
	}

	upgrade("connect.service.example")
	if tunnel == nil {
		t.Fatal("no tunnel")
	}
	if inHandler != [2]float64{1, 1} {
		t.Errorf("in connect's handler: %v, want one address with one connection", inHandler)
	}
	// the handler has returned; the tunnel still counts
	if got := ipv4(); got != [2]float64{1, 1} {
		t.Errorf("while the tunnel is open: %v, want one address with one connection", got)
	}
	tunnel.Close()
	tunnel = nil
	if got := ipv4(); got != [2]float64{0, 0} {
		t.Errorf("once the tunnel closed: %v, want none", got)
	}

	refuse = true
	upgrade("connect.service.example")
	if inHandler != [2]float64{1, 1} {
		t.Errorf("in connect's handler that refuses: %v, want one address with one connection", inHandler)
	}
	if got := ipv4(); got != [2]float64{0, 0} {
		t.Errorf("after a refused upgrade: %v, want none", got)
	}
	refuse = false

	testFront.connectHandler.serve = func(w http.ResponseWriter, r *http.Request) {
		inHandler = ipv4()
	}
	serveTestRequest(handler, newTestRequestFrom("198.51.100.52:1000", "GET", "connect.service.example", "/", true))
	if inHandler != [2]float64{0, 0} {
		t.Errorf("a connect request without an upgrade counted: %v", inHandler)
	}

	upgrade("mcp.service.example")
	if tunnel == nil {
		t.Fatal("no forwarded tunnel")
	}
	if got := ipv4(); got != [2]float64{0, 0} {
		t.Errorf("a forwarded websocket counted: %v", got)
	}
	tunnel.Close()
}

// A QUIC connection admitted as connect's counts against its address until
// its release, an excluded address's too; an api connection never counts.
func TestFrontConcentrationCountsConnectQuicConnections(t *testing.T) {
	routeTable := loadTestRouteTable(t)
	setTestRateLimit(t, routeTable, "eth1", 1, 0, 4, "203.0.113.0/24")
	testFront := newTestFront(t, routeTable, testFrontSettings())
	front := testFront.front

	releases := []func(){}
	for _, addr := range []string{"2001:db8::81", "2001:db8::81", "203.0.113.8"} {
		release, ok := front.AcquireQuicConnection("eth1", netip.MustParseAddr(addr), NameClassConnect)
		if !ok {
			t.Fatalf("%s refused", addr)
		}
		releases = append(releases, release)
	}
	apiRelease, ok := front.AcquireQuicConnection("eth1", netip.MustParseAddr("2001:db8::81"), NameClassApi)
	if !ok {
		t.Fatal("the api connection was refused")
	}
	checkTestConcentration(t, testFront.metrics, "ipv6", testConcentrationValues{0: {1, 2}})
	checkTestConcentration(t, testFront.metrics, "ipv4", testConcentrationValues{0: {1, 1}})

	apiRelease()
	for _, release := range releases {
		release()
		release()
	}
	for _, family := range concentrationFamilies {
		checkTestConcentration(t, testFront.metrics, family, testConcentrationValues{})
	}
	// the limit's count was released once, so the cap admits again
	for i := range 4 {
		if _, ok := front.AcquireQuicConnection("eth1", netip.MustParseAddr("2001:db8::81"), NameClassConnect); !ok {
			t.Fatalf("connection %d refused after the releases", i)
		}
	}
}
