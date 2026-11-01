package model

import (
	"context"
	"math"
	"net/netip"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Tests of where lag.dev places an extender (lag.dev/PLAN.md A4), which is
// also the country the issuance order counts it in (C7).

// What GeoLite2 says of an address, for these tests.
func testLagAddressPlace(countryCode string, city string, accuracyRadiusKm int, asn uint32) *LagAddressPlace {
	return &LagAddressPlace{
		Place: &LagPlace{
			City:        city,
			CountryCode: countryCode,
			CountryName: strings.ToUpper(countryCode),
		},
		AccuracyRadiusKm: accuracyRadiusKm,
		Asn:              asn,
	}
}

// Between an extender's two families, the place is the surer one's, the IPv4
// address's on a tie, when they agree on the country; the IPv6 address's when
// they disagree and are announced by two networks; and the surer one's, the
// IPv6 address's on a tie, when they disagree within one network. The
// fixtures are what GeoLite2 said of extenders on Main at 2026-11-01, whose
// places lag.dev's own round trips settled. Pure.
func TestLagExtenderAddressPlaceFamilies(t *testing.T) {
	// 213.110.70.211, a block leased to an operator in Hong Kong that GeoLite2
	// still places in Riga, and the operator's own IPv6 address
	riga := testLagAddressPlace("lv", "Riga", 5, 402279)
	landups := testLagAddressPlace("hk", "", 1000, 150452)
	// 23.141.12.139, leased from a US network, placed only in the US
	liberally := testLagAddressPlace("us", "", 1000, 152918)
	// 156.239.11.203 and 2001:df6:40c0::72c7, one Malaysian network's
	// addresses in Hong Kong: the IPv4 one placed in a city, the IPv6 one
	// only in the network's country
	tseungKwanO := testLagAddressPlace("hk", "Tseung Kwan O", 20, 154321)
	corenet := testLagAddressPlace("my", "", 1000, 154321)
	// one network's two families, placed as surely in two countries
	houston := testLagAddressPlace("us", "Houston", 20, 393448)
	ayerRaja := testLagAddressPlace("sg", "Ayer Raja New Town", 20, 393448)
	singapore := testLagAddressPlace("sg", "", 1000, 393448)
	// an IPv4 address GeoLite2 ASN has no record for
	unannounced := testLagAddressPlace("us", "", 1000, 0)
	ciallo := testLagAddressPlace("hk", "", 20, 202355)
	// both families in one country: 38.43.93.158 placed in a city and its
	// IPv6 address only in the country; 15.235.108.83 placed only in the
	// country and its IPv6 address in Montreal; and one network's two
	// families placed as surely, both only in the country
	detroit := testLagAddressPlace("us", "Detroit", 20, 399804)
	hostodo := testLagAddressPlace("us", "", 1000, 399804)
	canada := testLagAddressPlace("ca", "", 1000, 16276)
	montreal := testLagAddressPlace("ca", "Montreal", 20, 16276)
	franceV4 := testLagAddressPlace("fr", "", 500, 16276)
	franceV6 := testLagAddressPlace("fr", "", 500, 16276)
	unknownRadiusUs := testLagAddressPlace("us", "Detroit", 0, 399804)
	// an unknown radius is the least sure
	unknownRadius := testLagAddressPlace("hk", "", 0, 154321)

	for _, c := range []struct {
		name string
		v4   *LagAddressPlace
		v6   *LagAddressPlace
		want *LagAddressPlace
	}{
		{name: "neither family placed", v4: nil, v6: nil, want: nil},
		{name: "only IPv4 placed", v4: detroit, v6: nil, want: detroit},
		{name: "only IPv6 placed", v4: nil, v6: landups, want: landups},
		{name: "one country: IPv4 when it is surer", v4: detroit, v6: hostodo, want: detroit},
		{name: "one country: IPv6 when it is surer", v4: canada, v6: montreal, want: montreal},
		{name: "one country, as sure: IPv4", v4: franceV4, v6: franceV6, want: franceV4},
		{name: "one country: an unknown radius the least sure", v4: unknownRadiusUs, v6: hostodo, want: hostodo},
		{name: "two networks: IPv6, however sure IPv4 is", v4: riga, v6: landups, want: landups},
		{name: "two networks, both country only: IPv6", v4: liberally, v6: landups, want: landups},
		{name: "a network GeoLite2 ASN does not know: IPv6", v4: unannounced, v6: ciallo, want: ciallo},
		{name: "one network: IPv4 when it is surer", v4: tseungKwanO, v6: corenet, want: tseungKwanO},
		{name: "one network: IPv6 when it is surer", v4: unknownRadius, v6: corenet, want: corenet},
		{name: "one network: IPv4 when it is surer of a city", v4: houston, v6: singapore, want: houston},
		{name: "one network, as sure: IPv6", v4: houston, v6: ayerRaja, want: ayerRaja},
	} {
		var want *LagPlace
		if c.want != nil {
			want = c.want.Place
		}
		if got := lagExtenderAddressPlace(c.v4, c.v6); got != want {
			t.Fatalf("%s: placed %v, want %v", c.name, got, want)
		}
	}

	// without a GeoLite2 ASN database no network is known, so two families
	// in two countries are placed by their IPv6 address
	connect.AssertEqual(
		t,
		lagExtenderAddressPlace(
			testLagAddressPlace("hk", "Tseung Kwan O", 20, 0),
			testLagAddressPlace("my", "", 1000, 0),
		).CountryCode,
		"my",
	)
}

// The derived location moves the dot only within the country GeoLite2 places
// the extender in, and only while its residual is at most
// LagExtenderDerivedPlaceMaxResidualKm; the country and its name are always
// GeoLite2's. Pure.
func TestRefineLagExtenderPlace(t *testing.T) {
	hamburg := &LagPlace{City: "Hamburg", Region: "Hamburg", CountryCode: "de", CountryName: "Germany", Latitude: 53.55, Longitude: 9.99}
	derived := func(countryCode string, residualKm float32) *lagDerivedPlace {
		return &lagDerivedPlace{
			place:      &LagPlace{City: "Frankfurt am Main", Region: "Hesse", CountryCode: countryCode, CountryName: "Deutschland", Latitude: 50.11, Longitude: 8.68},
			residualKm: residualKm,
		}
	}
	frankfurt := &LagPlace{City: "Frankfurt am Main", Region: "Hesse", CountryCode: "de", CountryName: "Germany", Latitude: 50.11, Longitude: 8.68}

	connect.AssertEqual(t, refineLagExtenderPlace(hamburg, nil), hamburg)
	connect.AssertEqual(t, refineLagExtenderPlace(nil, derived("de", 1)), (*LagPlace)(nil))
	connect.AssertEqual(t, refineLagExtenderPlace(hamburg, derived("de", 4.5)), frankfurt)
	connect.AssertEqual(t, refineLagExtenderPlace(hamburg, derived("de", LagExtenderDerivedPlaceMaxResidualKm)), frankfurt)
	// too far off
	connect.AssertEqual(t, refineLagExtenderPlace(hamburg, derived("de", LagExtenderDerivedPlaceMaxResidualKm+0.5)), hamburg)
	connect.AssertEqual(t, refineLagExtenderPlace(hamburg, derived("de", 45076)), hamburg)
	connect.AssertEqual(t, refineLagExtenderPlace(hamburg, derived("de", float32(math.NaN()))), hamburg)
	// across a border, however close
	connect.AssertEqual(t, refineLagExtenderPlace(hamburg, derived("nl", 1)), hamburg)
	// a row whose country a merge removed
	connect.AssertEqual(t, refineLagExtenderPlace(hamburg, derived("", 1)), hamburg)
}

// One extender with the given addresses, all carrying tcp, for these tests.
func testLagPlaceExtender(ctx context.Context, active bool, countryCode string, addresses ...*NetworkExtenderAddress) server.Id {
	extenderId := server.NewId()
	for _, address := range addresses {
		address.ExtenderId = extenderId
		address.IpVersion = server.IpVersionForAddr(address.Ip)
		if address.Carriers == nil {
			address.Carriers = []string{connect.ExtenderCarrierTcp}
		}
		address.ActivateTime = server.NowUtc()
	}
	Testing_CreateNetworkExtender(ctx, &NetworkExtender{
		ExtenderId:  extenderId,
		NetworkId:   server.NewId(),
		ClientId:    server.NewId(),
		PublicKey:   server.NewId().Bytes(),
		CreateTime:  server.NowUtc(),
		TcpPort:     443,
		CountryCode: countryCode,
		Active:      active,
	}, addresses)
	return extenderId
}

func testLagPlaceAddress(ip string, active bool) *NetworkExtenderAddress {
	return &NetworkExtenderAddress{Ip: netip.MustParseAddr(ip), Active: active}
}

// Replaces GeoLite2 for the rest of the test with `records`, by address.
func useTestLagAddressPlaces(t testing.TB, records map[string]*LagAddressPlace) {
	t.Helper()
	t.Cleanup(Testing_ReplaceLagAddressPlaces(func(ip netip.Addr) *LagAddressPlace {
		return records[ip.String()]
	}))
}

// An extender is placed by the addresses it is measured at: its active
// addresses that carry tcp, or, when none is active, the last address of each
// family it had, so a holder that is away is counted where it is to serve
// again. An extender GeoLite2 places nowhere, and one that is gone, have no
// place.
func TestGetLagExtenderPlaces(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useTestLagAddressPlaces(t, map[string]*LagAddressPlace{
			"192.0.2.1":      testLagAddressPlace("lv", "Riga", 5, 402279),
			"2001:db8:1::1":  testLagAddressPlace("hk", "", 1000, 150452),
			"192.0.2.2":      testLagAddressPlace("hk", "Tseung Kwan O", 20, 154321),
			"2001:db8:2::1":  testLagAddressPlace("my", "", 1000, 154321),
			"192.0.2.3":      testLagAddressPlace("us", "Detroit", 20, 399804),
			"2001:db8:3::1":  testLagAddressPlace("ca", "Windsor", 20, 399804),
			"192.0.2.4":      testLagAddressPlace("jp", "Osaka", 20, 64500),
			"2001:db8:4::1":  testLagAddressPlace("kr", "Seoul", 20, 64501),
			"2001:db8:5::1":  testLagAddressPlace("de", "Hamburg", 20, 64502),
			"203.0.113.6":    testLagAddressPlace("fr", "Lyon", 20, 64503),
			"2001:db8:6::1":  testLagAddressPlace("nl", "Amsterdam", 20, 64504),
			"192.0.2.99":     testLagAddressPlace("za", "", 1000, 0),
			"2001:db8:99::1": testLagAddressPlace("za", "", 1000, 0),
		})

		// two families in two networks: its IPv6 address's place
		dualStack := testLagPlaceExtender(ctx, true, "lv",
			testLagPlaceAddress("192.0.2.1", true),
			testLagPlaceAddress("2001:db8:1::1", true),
		)
		// away: none active, placed by its last addresses, here two families
		// in one network, the IPv4 one placed more surely
		away := testLagPlaceExtender(ctx, false, "my",
			testLagPlaceAddress("192.0.2.2", false),
			testLagPlaceAddress("2001:db8:2::1", false),
		)
		// its IPv6 address is not active: placed by its IPv4 address alone
		oneActive := testLagPlaceExtender(ctx, true, "ca",
			testLagPlaceAddress("192.0.2.3", true),
			testLagPlaceAddress("2001:db8:3::1", false),
		)
		// its IPv6 address does not carry tcp, so it is not measured there
		noTcp := testLagPlaceExtender(ctx, true, "kr",
			testLagPlaceAddress("192.0.2.4", true),
			&NetworkExtenderAddress{Ip: netip.MustParseAddr("2001:db8:4::1"), Active: true, Carriers: []string{connect.ExtenderCarrierQuic}},
		)
		// GeoLite2 places its IPv4 address nowhere
		v6Placed := testLagPlaceExtender(ctx, true, "de",
			testLagPlaceAddress("198.51.100.5", true),
			testLagPlaceAddress("2001:db8:5::1", true),
		)
		// GeoLite2 places it nowhere
		nowhere := testLagPlaceExtender(ctx, true, "us", testLagPlaceAddress("198.51.100.7", true))
		// gone, and one with no address
		gone := server.NewId()
		noAddress := testLagPlaceExtender(ctx, true, "us")

		places := GetLagExtenderPlaces(ctx, []server.Id{dualStack, away, oneActive, noTcp, v6Placed, nowhere, gone, noAddress})
		countries := map[server.Id]string{}
		for extenderId, place := range places {
			countries[extenderId] = place.CountryCode
		}
		connect.AssertEqual(t, countries, map[server.Id]string{
			dualStack: "hk",
			away:      "hk",
			oneActive: "us",
			noTcp:     "jp",
			v6Placed:  "de",
		})
		connect.AssertEqual(t, places[away], &LagPlace{City: "Tseung Kwan O", CountryCode: "hk", CountryName: "HK"})

		// an extender that is away while one address is still marked active
		// is placed by that one
		partly := testLagPlaceExtender(ctx, false, "nl",
			testLagPlaceAddress("203.0.113.6", true),
			testLagPlaceAddress("2001:db8:6::1", false),
		)
		connect.AssertEqual(t, GetLagExtenderPlaces(ctx, []server.Id{partly})[partly].CountryCode, "fr")

		connect.AssertEqual(t, len(GetLagExtenderPlaces(ctx, nil)), 0)
	})
}

// A derived location moves the dot within the country GeoLite2 places the
// extender in, while its residual is small; a derived point across a border,
// however close, or one too far off, never changes the place, and a derived
// location never places an extender GeoLite2 places nowhere. A provider's row
// is never an extender's, whatever its id.
func TestGetLagExtenderPlacesDerivedNeverChangesCountry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		useTestLagAddressPlaces(t, map[string]*LagAddressPlace{
			"192.0.2.11": {Place: &LagPlace{City: "Hamburg", Region: "Hamburg", CountryCode: "de", CountryName: "Germany", Latitude: 53.55, Longitude: 9.99}, AccuracyRadiusKm: 20},
			"192.0.2.12": {Place: &LagPlace{CountryCode: "gb", CountryName: "United Kingdom", Latitude: 51.4964, Longitude: -0.1224}, AccuracyRadiusKm: 1000},
			"192.0.2.13": {Place: &LagPlace{CountryCode: "hk", CountryName: "Hong Kong", Latitude: 22.2578, Longitude: 114.1657}, AccuracyRadiusKm: 10},
			"192.0.2.14": {Place: &LagPlace{City: "Detroit", Region: "Michigan", CountryCode: "us", CountryName: "United States", Latitude: 42.3329, Longitude: -83.0495}, AccuracyRadiusKm: 20},
			"192.0.2.15": {Place: &LagPlace{City: "Montréal", Region: "Quebec", CountryCode: "ca", CountryName: "Canada", Latitude: 45.5, Longitude: -73.6}, AccuracyRadiusKm: 20},
		})
		frankfurt := testStatsCityLocation(ctx, "Frankfurt am Main", "Hesse", "Germany", "de")
		cahermore := testStatsCityLocation(ctx, "Cahermore", "Munster", "Ireland", "ie")
		longgang := testStatsCityLocation(ctx, "Longgang District", "Guangdong", "China", "cn")
		saginaw := testStatsCityLocation(ctx, "Saginaw", "Missouri", "United States", "us")
		longueuil := testStatsCityLocation(ctx, "Longueuil", "Quebec", "Canada", "ca")
		now := server.NowUtc()

		within := testLagPlaceExtender(ctx, true, "de", testLagPlaceAddress("192.0.2.11", true))
		crossed := testLagPlaceExtender(ctx, true, "gb", testLagPlaceAddress("192.0.2.12", true))
		crossedFarOff := testLagPlaceExtender(ctx, true, "hk", testLagPlaceAddress("192.0.2.13", true))
		farOff := testLagPlaceExtender(ctx, true, "us", testLagPlaceAddress("192.0.2.14", true))
		nowhere := testLagPlaceExtender(ctx, true, "ca", testLagPlaceAddress("198.51.100.16", true))
		provider := testLagPlaceExtender(ctx, true, "ca", testLagPlaceAddress("192.0.2.15", true))

		row := func(nodeKind int, nodeId server.Id, city *Location, latitude float64, longitude float64, residualKm float32) *DerivedLocation {
			derived := testDerivedLocation(nodeKind, nodeId, city, now)
			derived.Latitude = latitude
			derived.Longitude = longitude
			derived.ResidualKm = residualKm
			return derived
		}
		ReplaceDerivedLocations(ctx, []*DerivedLocation{
			row(DerivedLocationNodeKindExtender, within, frankfurt, 50.11, 8.68, 4.5),
			// Main's: a derived point in the Atlantic mapped to Ireland, and
			// one mapped into Shenzhen, far off both
			row(DerivedLocationNodeKindExtender, crossed, cahermore, 47.29, -11.73, 4.5),
			row(DerivedLocationNodeKindExtender, crossedFarOff, longgang, 22.757, 114.809, 28192),
			row(DerivedLocationNodeKindExtender, farOff, saginaw, 37.03, -94.48, 45076),
			row(DerivedLocationNodeKindExtender, nowhere, longueuil, 45.54, -73.45, 4.5),
			row(DerivedLocationNodeKindProvider, provider, longueuil, 45.54, -73.45, 4.5),
		})

		places := GetLagExtenderPlaces(ctx, []server.Id{within, crossed, crossedFarOff, farOff, nowhere, provider})
		connect.AssertEqual(t, places[within], &LagPlace{
			City:        "Frankfurt am Main",
			Region:      "Hesse",
			CountryCode: "de",
			CountryName: "Germany",
			Latitude:    50.11,
			Longitude:   8.68,
		})
		connect.AssertEqual(t, places[crossed], &LagPlace{CountryCode: "gb", CountryName: "United Kingdom", Latitude: 51.4964, Longitude: -0.1224})
		connect.AssertEqual(t, places[crossedFarOff], &LagPlace{CountryCode: "hk", CountryName: "Hong Kong", Latitude: 22.2578, Longitude: 114.1657})
		connect.AssertEqual(t, places[farOff], &LagPlace{City: "Detroit", Region: "Michigan", CountryCode: "us", CountryName: "United States", Latitude: 42.3329, Longitude: -83.0495})
		connect.AssertEqual(t, places[nowhere], (*LagPlace)(nil))
		connect.AssertEqual(t, places[provider], &LagPlace{City: "Montréal", Region: "Quebec", CountryCode: "ca", CountryName: "Canada", Latitude: 45.5, Longitude: -73.6})
		connect.AssertEqual(t, len(places), 5)
	})
}
