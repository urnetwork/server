package model

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Tests of the lookup behind the lag.dev extender places (lag.dev/PLAN.md A4)
// as production runs it: lagAddressPlaces against hand-built GeoLite2 City
// and GeoLite2 ASN databases in their own record shapes, opened the way the
// runtime opens the deployed ones. The other place tests replace the lookup.

// One network of a hand-built GeoLite2 City database.
type testLagCityBlock struct {
	countryCode string
	city        string
	// 0 writes no accuracy_radius, as GeoLite2 leaves out of some records
	accuracyRadiusKm uint16
	latitude         float64
	longitude        float64
}

// Writes a MaxMind DB of databaseType holding what insert puts in it.
func writeTestLagMmdb(t testing.TB, databaseType string, name string, insert func(writer *mmdbwriter.Tree)) string {
	t.Helper()
	writer, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            databaseType,
		IncludeReservedNetworks: true,
		RecordSize:              28,
	})
	if err != nil {
		t.Fatal(err)
	}
	insert(writer)
	path := filepath.Join(t.TempDir(), name)
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteTo(file); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// A GeoLite2 City database with one record per network.
func writeTestLagCityDb(t testing.TB, blocks map[string]testLagCityBlock) string {
	t.Helper()
	return writeTestLagMmdb(t, "GeoLite2-City", "geolite2.mmdb", func(writer *mmdbwriter.Tree) {
		names := func(name string) mmdbtype.Map {
			return mmdbtype.Map{"en": mmdbtype.String(name)}
		}
		for cidr, block := range blocks {
			_, network, err := net.ParseCIDR(cidr)
			if err != nil {
				t.Fatal(err)
			}
			location := mmdbtype.Map{
				"latitude":  mmdbtype.Float64(block.latitude),
				"longitude": mmdbtype.Float64(block.longitude),
			}
			if block.accuracyRadiusKm != 0 {
				location["accuracy_radius"] = mmdbtype.Uint16(block.accuracyRadiusKm)
			}
			record := mmdbtype.Map{
				"continent": mmdbtype.Map{"code": mmdbtype.String("AS"), "names": names("Asia")},
				"country": mmdbtype.Map{
					"geoname_id": mmdbtype.Uint32(1),
					"iso_code":   mmdbtype.String(strings.ToUpper(block.countryCode)),
					"names":      names(strings.ToUpper(block.countryCode) + " name"),
				},
				"location": location,
			}
			if block.city != "" {
				record["city"] = mmdbtype.Map{"geoname_id": mmdbtype.Uint32(2), "names": names(block.city)}
			}
			if err := writer.Insert(network, record); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// A GeoLite2 ASN database with one autonomous system per network.
func writeTestLagAsnDb(t testing.TB, numbers map[string]uint32) string {
	t.Helper()
	return writeTestLagMmdb(t, server.GeoLite2AsnDatabaseType, "geolite2-asn.mmdb", func(writer *mmdbwriter.Tree) {
		for cidr, number := range numbers {
			_, network, err := net.ParseCIDR(cidr)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.Insert(network, mmdbtype.Map{
				"autonomous_system_number":       mmdbtype.Uint32(number),
				"autonomous_system_organization": mmdbtype.String(fmt.Sprintf("AS%d", number)),
			}); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// Installs the hand-built databases for the rest of a serial test: at
// documentation addresses, what GeoLite2 said on Main at 2026-11-01 of the
// extenders the place rule turns on. 203.0.113.0/24 has a City record
// without an accuracy radius and no ASN record, and 198.18.0.0/15 has
// neither. No ip override answers.
func useTestLagGeoLite2(t testing.TB) {
	t.Helper()
	server.UseNoIpOverridesForTest(t)
	server.UseIpDbFileForTest(t, writeTestLagCityDb(t, map[string]testLagCityBlock{
		"192.0.2.0/24":    {countryCode: "lv", city: "Riga", accuracyRadiusKm: 5, latitude: 56.9473, longitude: 24.0979},
		"2001:db8:1::/48": {countryCode: "hk", accuracyRadiusKm: 1000, latitude: 22.2578, longitude: 114.1657},
		"198.51.100.0/24": {countryCode: "hk", city: "Tseung Kwan O", accuracyRadiusKm: 20, latitude: 22.3346, longitude: 114.2536},
		"2001:db8:2::/48": {countryCode: "my", accuracyRadiusKm: 1000, latitude: 2.5, longitude: 112.5},
		"203.0.113.0/24":  {countryCode: "us", latitude: 37.751, longitude: -97.822},
		"2001:db8:3::/48": {countryCode: "us", city: "Detroit", accuracyRadiusKm: 20, latitude: 42.3329, longitude: -83.0495},
	}))
	restore, err := server.Testing_UseIpAsnDatabase(writeTestLagAsnDb(t, map[string]uint32{
		"192.0.2.0/24":    402279,
		"2001:db8:1::/48": 150452,
		"198.51.100.0/24": 154321,
		"2001:db8:2::/48": 154321,
		"2001:db8:3::/48": 399804,
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restore)
}

// What lagAddressPlaces reads of each address: the City record's place and
// accuracy radius, and the ASN record's number. A record without an accuracy
// radius reads as 0, an address the ASN database does not know as network 0,
// and an address the City database does not know is absent. Pure.
func TestLagAddressPlacesReadsGeoLite2(t *testing.T) {
	useTestLagGeoLite2(t)

	riga := netip.MustParseAddr("192.0.2.1")
	hongKong := netip.MustParseAddr("2001:db8:1::1")
	noRadius := netip.MustParseAddr("203.0.113.1")
	unknown := netip.MustParseAddr("198.18.0.1")
	addressPlaces := lagAddressPlaces([]netip.Addr{riga, hongKong, noRadius, unknown})

	connect.AssertEqual(t, len(addressPlaces), 3)
	connect.AssertEqual(t, addressPlaces[riga], &LagAddressPlace{
		Place:            &LagPlace{City: "Riga", CountryCode: "lv", CountryName: "LV name", Latitude: 56.9473, Longitude: 24.0979},
		AccuracyRadiusKm: 5,
		Asn:              402279,
	})
	connect.AssertEqual(t, addressPlaces[hongKong], &LagAddressPlace{
		Place:            &LagPlace{CountryCode: "hk", CountryName: "HK name", Latitude: 22.2578, Longitude: 114.1657},
		AccuracyRadiusKm: 1000,
		Asn:              150452,
	})
	connect.AssertEqual(t, addressPlaces[noRadius], &LagAddressPlace{
		Place:            &LagPlace{CountryCode: "us", CountryName: "US name", Latitude: 37.751, Longitude: -97.822},
		AccuracyRadiusKm: 0,
		Asn:              0,
	})
	connect.AssertEqual(t, addressPlaces[unknown], (*LagAddressPlace)(nil))

	// without the ASN database every network is unknown
	restore, err := server.Testing_UseIpAsnDatabase("")
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	connect.AssertEqual(t, lagAddressPlaces([]netip.Addr{riga})[riga].Asn, uint32(0))
}

// Without a GeoLite2 City database nothing is looked up and nothing is
// placed, and that is logged once per process, not once per pass. Pure.
func TestLagAddressPlacesWithoutGeoLite2City(t *testing.T) {
	reads := server.UseNoIpDbForTest(t)
	connect.AssertEqual(t, server.IpInfoDatabaseDeployed(), false)
	logged := []string{}
	previousLogf := lagCityDatabaseMissingLogf
	previousLogged := lagCityDatabaseMissingLogged.Load()
	lagCityDatabaseMissingLogf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	lagCityDatabaseMissingLogged.Store(false)
	t.Cleanup(func() {
		lagCityDatabaseMissingLogf = previousLogf
		lagCityDatabaseMissingLogged.Store(previousLogged)
	})

	ips := []netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8:1::1")}
	connect.AssertEqual(t, len(lagAddressPlaces(ips)), 0)
	connect.AssertEqual(t, len(lagAddressPlaces(ips)), 0)
	connect.AssertEqual(t, reads(), 0)
	connect.AssertEqual(t, len(logged), 1)
	connect.AssertEqual(t, strings.Contains(logged[0], "no GeoLite2 City database"), true)
	connect.AssertEqual(t, strings.Contains(logged[0], "extenders are not placed; the order counts stored countries"), true)
}

// Extenders placed through the databases, as production places them: two
// families in two networks by the IPv6 address, in one network by the surer
// address, in one country by the surer record, an away extender by the family
// it last worked at rather than one that stopped weeks before, and an IPv4
// address stored in its IPv4-mapped form as the IPv4 address it is.
func TestGetLagExtenderPlacesReadsGeoLite2(t *testing.T) {
	useTestLagGeoLite2(t)

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		twoNetworks := testLagPlaceExtender(ctx, true, "lv",
			testLagPlaceAddress("192.0.2.1", true),
			testLagPlaceAddress("2001:db8:1::1", true),
		)
		oneNetwork := testLagPlaceExtender(ctx, true, "my",
			testLagPlaceAddress("198.51.100.1", true),
			testLagPlaceAddress("2001:db8:2::1", true),
		)
		// its IPv4 record has no accuracy radius, so its IPv6 record is surer
		oneCountry := testLagPlaceExtender(ctx, true, "us",
			testLagPlaceAddress("203.0.113.1", true),
			testLagPlaceAddress("2001:db8:3::1", true),
		)
		// away: its IPv4 address in the US worked ten minutes ago, its IPv6
		// address in Hong Kong three weeks ago
		awayStale := testLagPlaceExtender(ctx, false, "us",
			testLagPlaceAddressWorked("203.0.113.9", false, now.Add(-10*time.Minute)),
			testLagPlaceAddressWorked("2001:db8:1::9", false, now.Add(-21*24*time.Hour)),
		)
		mapped := testLagPlaceExtender(ctx, true, "lv",
			testLagPlaceAddress("::ffff:192.0.2.7", true),
		)

		places := GetLagExtenderPlaces(ctx, []server.Id{twoNetworks, oneNetwork, oneCountry, awayStale, mapped})
		connect.AssertEqual(t, places[twoNetworks], &LagPlace{CountryCode: "hk", CountryName: "HK name", Latitude: 22.2578, Longitude: 114.1657})
		connect.AssertEqual(t, places[oneNetwork], &LagPlace{City: "Tseung Kwan O", CountryCode: "hk", CountryName: "HK name", Latitude: 22.3346, Longitude: 114.2536})
		connect.AssertEqual(t, places[oneCountry], &LagPlace{City: "Detroit", CountryCode: "us", CountryName: "US name", Latitude: 42.3329, Longitude: -83.0495})
		connect.AssertEqual(t, places[awayStale], &LagPlace{CountryCode: "us", CountryName: "US name", Latitude: 37.751, Longitude: -97.822})
		connect.AssertEqual(t, places[mapped], &LagPlace{City: "Riga", CountryCode: "lv", CountryName: "LV name", Latitude: 56.9473, Longitude: 24.0979})
	})
}
