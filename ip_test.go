package server

import (
	cryptorand "crypto/rand"
	"fmt"
	mathrand "math/rand"
	"net"
	"time"

	"testing"

	"github.com/go-playground/assert/v2"
)

func TestIpInfo(t *testing.T) {
	ip1 := net.ParseIP("65.19.157.62")
	ipInfo1, err := GetIpInfoFromIp(ip1)
	assert.Equal(t, err, nil)
	assert.NotEqual(t, ipInfo1, nil)

	assert.Equal(t, ipInfo1.CountryCode, "us")
	assert.Equal(t, ipInfo1.Country, "United States")
	assert.Equal(t, ipInfo1.Region, "California")
	assert.Equal(t, ipInfo1.UserType, UserTypeConsumer)
	assert.NotEqual(t, ipInfo1.Longitude, float64(0.0))
	assert.NotEqual(t, ipInfo1.Latitude, float64(0.0))

	ip2 := net.ParseIP("2001:470:173::52")
	ipInfo2, err := GetIpInfoFromIp(ip2)
	assert.Equal(t, err, nil)
	assert.NotEqual(t, ipInfo2, nil)

	assert.Equal(t, ipInfo2.CountryCode, "us")
	assert.Equal(t, ipInfo2.Country, "United States")
	assert.Equal(t, ipInfo2.Region, "California")
	assert.Equal(t, ipInfo2.UserType, UserTypeConsumer)
	assert.NotEqual(t, ipInfo2.Longitude, float64(0.0))
	assert.NotEqual(t, ipInfo2.Latitude, float64(0.0))

	ip3 := net.ParseIP("1.1.1.1")
	ipInfo3, err := GetIpInfoFromIp(ip3)
	assert.Equal(t, err, nil)
	assert.NotEqual(t, ipInfo3, nil)

	assert.Equal(t, ipInfo3.UserType, UserTypeHosting)
	assert.Equal(t, ipInfo3.Hosting, true)
	assert.NotEqual(t, ipInfo3.Longitude, float64(0.0))
	assert.NotEqual(t, ipInfo3.Latitude, float64(0.0))

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
			assert.Equal(t, err, nil)
		} else {
			ipv4Bytes := make([]byte, 4)
			cryptorand.Read(ipv4Bytes)
			ipv4 := net.IP(ipv4Bytes)
			_, err := GetIpInfoFromIp(ipv4)
			assert.Equal(t, err, nil)
		}
	}
	endTime := time.Now()

	duration := endTime.Sub(startTime)
	fmt.Printf("[ip]%d lookups per second (%s total)\n", int((time.Duration(n)*duration)/time.Second), duration)
	assert.Equal(t, duration <= 20*time.Second, true)
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
			assert.Equal(t, test.km, km)
		}
		if d := millis - test.millis; d < -eps || eps < d {
			assert.Equal(t, test.millis, millis)
		}
	}
}
