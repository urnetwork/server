package server

import (
	cryptorand "crypto/rand"
	"fmt"
	mathrand "math/rand"
	"net"
	"net/netip"
	"time"

	"testing"

	"github.com/urnetwork/connect/v2026"
)

func TestIpInfo(t *testing.T) {
	ip1 := net.ParseIP("65.19.157.62")
	ipInfo1, err := GetIpInfoFromIp(ip1)
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, ipInfo1, nil)

	connect.AssertEqual(t, ipInfo1.CountryCode, "us")
	connect.AssertEqual(t, ipInfo1.Country, "United States")
	connect.AssertEqual(t, ipInfo1.Region, "California")
	connect.AssertEqual(t, ipInfo1.UserType, UserTypeHosting)
	connect.AssertNotEqual(t, ipInfo1.Longitude, float64(0.0))
	connect.AssertNotEqual(t, ipInfo1.Latitude, float64(0.0))

	ip2 := net.ParseIP("2001:470:173::52")
	ipInfo2, err := GetIpInfoFromIp(ip2)
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, ipInfo2, nil)

	connect.AssertEqual(t, ipInfo2.CountryCode, "us")
	connect.AssertEqual(t, ipInfo2.Country, "United States")
	connect.AssertEqual(t, ipInfo2.Region, "California")
	connect.AssertEqual(t, ipInfo2.UserType, UserTypeHosting)
	connect.AssertNotEqual(t, ipInfo2.Longitude, float64(0.0))
	connect.AssertNotEqual(t, ipInfo2.Latitude, float64(0.0))

	ip3 := net.ParseIP("1.1.1.1")
	ipInfo3, err := GetIpInfoFromIp(ip3)
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, ipInfo3, nil)

	connect.AssertEqual(t, ipInfo3.UserType, UserTypeHosting)
	connect.AssertEqual(t, ipInfo3.Hosting, true)
	connect.AssertNotEqual(t, ipInfo3.Longitude, float64(0.0))
	connect.AssertNotEqual(t, ipInfo3.Latitude, float64(0.0))

}

func TestIpInfoPerf(t *testing.T) {
	Warmup()

	n := 100000
	startTime := time.Now()
	for range n {
		if mathrand.Intn(2) == 0 {
			ipv6Bytes := make([]byte, 16)
			cryptorand.Read(ipv6Bytes)
			ipv6 := net.IP(ipv6Bytes)
			_, err := GetIpInfoFromIp(ipv6)
			connect.AssertEqual(t, err, nil)
		} else {
			ipv4Bytes := make([]byte, 4)
			cryptorand.Read(ipv4Bytes)
			ipv4 := net.IP(ipv4Bytes)
			_, err := GetIpInfoFromIp(ipv4)
			connect.AssertEqual(t, err, nil)
		}
	}
	endTime := time.Now()

	duration := endTime.Sub(startTime)
	fmt.Printf("[ip]%d lookups per second (%s total)\n", int((time.Duration(n)*duration)/time.Second), duration)
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
	addrPort, err := ParseClientAddress("[2001:470:99:57:e643:4bff:fe23:a343]:443")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, addrPort.Addr().String(), "2001:470:99:57:e643:4bff:fe23:a343")
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
	ip1 := net.ParseIP("65.19.157.62")
	arinInfo1, err := GetArinInfoFromIp(ip1)
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, arinInfo1, nil)

	connect.AssertEqual(t, arinInfo1.OrgCountryCodes[0], "us")

	ip2 := net.ParseIP("2001:4200::1")
	arinInfo2, err := GetArinInfoFromIp(ip2)
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, arinInfo2, nil)

	connect.AssertEqual(t, arinInfo2.OrgCountryCodes[0], "mu")

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
			"hosting":      true,
			"latitude":     10,
			"longitude":    20.5,
		},
	}

	overrides := parseIpOverrides(settingsObj)
	connect.AssertEqual(t, len(overrides), 2)

	find := func(addr string) *IpInfo {
		for _, override := range overrides {
			if override.prefix.Contains(netip.MustParseAddr(addr)) {
				ipInfo := override.ipInfo
				return &ipInfo
			}
		}
		return nil
	}

	ipInfo := find("198.18.5.4")
	connect.AssertNotEqual(t, ipInfo, nil)
	connect.AssertEqual(t, ipInfo.CountryCode, "zz")
	connect.AssertEqual(t, ipInfo.Country, "Sim")
	connect.AssertEqual(t, ipInfo.Region, "Sim")
	connect.AssertEqual(t, ipInfo.City, "Sim")
	connect.AssertEqual(t, ipInfo.UserType, UserTypeConsumer)
	connect.AssertEqual(t, ipInfo.Hosting, false)

	ipInfo = find("198.19.0.100")
	connect.AssertNotEqual(t, ipInfo, nil)
	connect.AssertEqual(t, ipInfo.UserType, UserTypeHosting)
	connect.AssertEqual(t, ipInfo.Hosting, true)
	connect.AssertEqual(t, ipInfo.Latitude, float64(10))
	connect.AssertEqual(t, ipInfo.Longitude, 20.5)

	// outside every override subnet
	connect.AssertEqual(t, find("198.20.0.1") == nil, true)
}
