package server

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

const portableIpInfoIpv4 = "192.0.2.1"
const portableIpInfoIpv6 = "2001:db8::1"

// The portable suite intentionally replaces the external location databases
// with synthetic documentation-subnet overrides. A developer using external
// resources keeps that separate integration boundary instead of silently
// testing different lookup data here.
func requirePortableIpInfoFixture(t *testing.T) {
	t.Helper()
	if os.Getenv("WARP_TEST_ENV_USE_PORTABLE_RESOURCES") != "1" {
		t.Skip("requires the portable synthetic ip override fixture")
	}
	for _, rawIp := range []string{portableIpInfoIpv4, portableIpInfoIpv6} {
		if ipOverrideFor(netip.MustParseAddr(rawIp)) == nil {
			t.Fatalf("portable fixture does not override documentation address %s", rawIp)
		}
	}
}

func TestIpInfo(t *testing.T) {
	requirePortableIpInfoFixture(t)
	// the fixture sets only the names, so everything GeoLite2 would add
	// (radius, coordinates, geoname ids) stays at its zero, unknown value
	expected := IpInfo{
		CountryCode: "zz",
		Country:     "Fixture Country",
		Region:      "Fixture Region",
		Regions:     []string{"Fixture Region"},
		City:        "Fixture City",
	}
	for _, rawIp := range []string{portableIpInfoIpv4, portableIpInfoIpv6} {
		ipInfo, err := GetIpInfoFromIp(net.ParseIP(rawIp))
		if err != nil {
			t.Fatal(err)
		}
		if ipInfo == nil || !reflect.DeepEqual(*ipInfo, expected) {
			t.Fatalf("documentation address %s resolved to unexpected synthetic info: %+v", rawIp, ipInfo)
		}

		// every lookup is a copy; a caller editing its regions cannot
		// rewrite the fixture for the next lookup
		ipInfo.Regions[0] = "Edited"
		again, err := GetIpInfoFromIp(net.ParseIP(rawIp))
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, again.Regions, []string{"Fixture Region"})
	}
}

func TestIpInfoPerf(t *testing.T) {
	requirePortableIpInfoFixture(t)
	ips := []net.IP{net.ParseIP(portableIpInfoIpv4), net.ParseIP(portableIpInfoIpv6)}
	for _, ip := range ips {
		if _, err := GetIpInfoFromIp(ip); err != nil {
			t.Fatal(err)
		}
	}

	n := 100000
	startTime := time.Now()
	for index := range n {
		ipInfo, err := GetIpInfoFromIp(ips[index%len(ips)])
		if err != nil {
			t.Fatal(err)
		}
		if ipInfo.CountryCode != "zz" {
			t.Fatalf("lookup %d left the fixture: %+v", index, ipInfo)
		}
	}
	endTime := time.Now()

	duration := endTime.Sub(startTime)
	fmt.Printf("[ip]%d lookups per second (%s total)\n", int(float64(n)/duration.Seconds()), duration)
	connect.AssertEqual(t, duration <= 20*time.Second, true)
}

func TestDistance(t *testing.T) {
	var tests = []struct {
		lat1   float64
		lon1   float64
		lat2   float64
		lon2   float64
		km     float64
		millis float64
	}{
		{
			22.55, 43.12, // Rio de Janeiro, Brazil
			13.45, 100.28, // Bangkok, Thailand
			6094.544,
			20.329,
		},
		{
			20.10, 57.30, // Port Louis, Mauritius
			0.57, 100.21, // Padang, Indonesia
			5145.526,
			17.164,
		},
		{
			51.45, 1.15, // Oxford, United Kingdom
			41.54, 12.27, // Vatican, City Vatican City
			1389.179,
			4.634,
		},
		{
			22.34, 17.05, // Windhoek, Namibia
			51.56, 4.29, // Rotterdam, Netherlands
			3429.893,
			11.441,
		},
		{
			63.24, 56.59, // Esperanza, Argentina
			8.50, 13.14, // Luanda, Angola
			6996.186,
			23.337,
		},
		{
			90.00, 0.00, // North/South Poles
			48.51, 2.21, // Paris,  France
			4613.478,
			15.389,
		},
		{
			45.04, 7.42, // Turin, Italy
			3.09, 101.42, // Kuala Lumpur, Malaysia
			10078.112,
			33.617,
		},
	}

	for _, test := range tests {
		km := DistanceKm(test.lat1, test.lon1, test.lat2, test.lon2)
		millis := DistanceMillis(test.lat1, test.lon1, test.lat2, test.lon2)

		eps := 0.1
		if d := km - test.km; d < -eps || eps < d {
			connect.AssertEqual(t, test.km, km)
		}
		if d := millis - test.millis; d < -eps || eps < d {
			connect.AssertEqual(t, test.millis, millis)
		}
	}
}

func TestParseClientAddress(t *testing.T) {
	addrPort, err := ParseClientAddress("[2001:db8:99:57:e643:4bff:fe23:a343]:443")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, addrPort.Addr().String(), "2001:db8:99:57:e643:4bff:fe23:a343")
	connect.AssertEqual(t, int(addrPort.Port()), 443)

	addrPort, err = ParseClientAddress("127.0.0.1:443")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, addrPort.Addr().String(), "127.0.0.1")
	connect.AssertEqual(t, int(addrPort.Port()), 443)

	addrPort, err = ParseClientAddress("fd00:6a4f:a007:15da::1:40704")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, addrPort.Addr().String(), "fd00:6a4f:a007:15da::1")
	connect.AssertEqual(t, int(addrPort.Port()), 40704)

	addrPort, err = ParseClientAddress(":443")
	connect.AssertNotEqual(t, err, nil)
}

func TestArinInfo(t *testing.T) {
	requirePortableIpInfoFixture(t)
	for _, rawIp := range []string{portableIpInfoIpv4, portableIpInfoIpv6} {
		arinInfo, err := GetArinInfoFromIp(net.ParseIP(rawIp))
		if err != nil {
			t.Fatal(err)
		}
		if arinInfo == nil || len(arinInfo.OrgCountryCodes) != 1 || arinInfo.OrgCountryCodes[0] != "zz" {
			t.Fatalf("documentation address %s resolved to unexpected synthetic registration: %+v", rawIp, arinInfo)
		}
	}
}

func TestParseIpOverrides(t *testing.T) {
	// mirrors the yaml parse shape: []any of map[string]any
	settingsObj := []any{
		map[string]any{
			"subnet":       "198.18.0.0/16",
			"country_code": "ZZ",
			"country":      "Sim",
			"region":       "Sim",
			"city":         "Sim",
		},
		map[string]any{
			"subnet":       "198.19.0.0/20",
			"country_code": "zz",
			"country":      "Sim",
			// the old verdict keys still load, whatever their value, and
			// have no effect
			"hosting":   true,
			"privacy":   true,
			"virtual":   "yes",
			"latitude":  10,
			"longitude": 20.5,
		},
		map[string]any{
			"subnet":             "2001:db8:1::/48",
			"continent_code":     "EU",
			"continent":          "Europe",
			"country_code":       "GB",
			"country":            "United Kingdom",
			"region":             "England",
			"city":               "East Finchley",
			"latitude":           51.5967,
			"longitude":          -0.1593,
			"timezone":           "Europe/London",
			"accuracy_radius_km": 200,
			"city_geoname_id":    2650444,
			"region_geoname_id":  6269131,
			"country_geoname_id": 2635167,
		},
	}

	overrides := parseIpOverrides(settingsObj)
	connect.AssertEqual(t, len(overrides), 3)

	find := func(addr string) *IpInfo {
		for _, override := range overrides {
			if override.prefix.Contains(netip.MustParseAddr(addr)) {
				ipInfo := override.ipInfo
				return &ipInfo
			}
		}
		return nil
	}

	// a missing optional key is its zero value, never a panic
	ipInfo := find("198.18.5.4")
	connect.AssertNotEqual(t, ipInfo, nil)
	connect.AssertEqual(t, *ipInfo, IpInfo{
		CountryCode: "zz",
		Country:     "Sim",
		Region:      "Sim",
		Regions:     []string{"Sim"},
		City:        "Sim",
	})

	ipInfo = find("198.19.0.100")
	connect.AssertNotEqual(t, ipInfo, nil)
	connect.AssertEqual(t, *ipInfo, IpInfo{
		CountryCode: "zz",
		Country:     "Sim",
		Regions:     []string{},
		Latitude:    10,
		Longitude:   20.5,
	})

	ipInfo = find("2001:db8:1:2::3")
	connect.AssertNotEqual(t, ipInfo, nil)
	connect.AssertEqual(t, *ipInfo, IpInfo{
		ContinentCode:    "eu",
		Continent:        "Europe",
		CountryCode:      "gb",
		Country:          "United Kingdom",
		Region:           "England",
		Regions:          []string{"England"},
		City:             "East Finchley",
		Latitude:         51.5967,
		Longitude:        -0.1593,
		AccuracyRadiusKm: 200,
		Timezone:         "Europe/London",
		CityGeonameId:    2650444,
		RegionGeonameId:  6269131,
		CountryGeonameId: 2635167,
	})

	// outside every override subnet
	connect.AssertEqual(t, find("203.0.113.1") == nil, true)
}

// A radius or geoname id of the wrong type or range, or a latitude that is
// not a number, panics at parse; the widest geoname id still fits.
func TestParseIpOverridesRejectsMalformedValues(t *testing.T) {
	requirePanic := func(name string, entry map[string]any) {
		t.Helper()
		entry["subnet"] = "198.18.0.0/16"
		defer func() {
			if recover() == nil {
				t.Fatalf("%s: expected a panic", name)
			}
		}()
		parseIpOverrides([]any{entry})
	}

	requirePanic("radius as a string", map[string]any{"accuracy_radius_km": "200"})
	requirePanic("negative radius", map[string]any{"accuracy_radius_km": -1})
	requirePanic("fractional radius", map[string]any{"accuracy_radius_km": 2.5})
	requirePanic("geoname id as a string", map[string]any{"city_geoname_id": "2650444"})
	requirePanic("negative geoname id", map[string]any{"region_geoname_id": -1})
	requirePanic("geoname id over 32 bits", map[string]any{"country_geoname_id": 4294967296})
	requirePanic("latitude as a string", map[string]any{"latitude": "51.5"})

	// the widest geoname id still fits
	overrides := parseIpOverrides([]any{map[string]any{
		"subnet":             "198.18.0.0/16",
		"country_geoname_id": 4294967295,
	}})
	connect.AssertEqual(t, overrides[0].ipInfo.CountryGeonameId, uint32(4294967295))
}
