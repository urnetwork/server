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
	assert.Equal(t, ipInfo1.UserType, UserTypeHosting)
	assert.NotEqual(t, ipInfo1.Longitude, float64(0.0))
	assert.NotEqual(t, ipInfo1.Latitude, float64(0.0))

	ip2 := net.ParseIP("2001:470:173::52")
	ipInfo2, err := GetIpInfoFromIp(ip2)
	assert.Equal(t, err, nil)
	assert.NotEqual(t, ipInfo2, nil)

	assert.Equal(t, ipInfo2.CountryCode, "us")
	assert.Equal(t, ipInfo2.Country, "United States")
	assert.Equal(t, ipInfo2.Region, "California")
	assert.Equal(t, ipInfo2.UserType, UserTypeHosting)
	assert.NotEqual(t, ipInfo2.Longitude, float64(0.0))
	assert.NotEqual(t, ipInfo2.Latitude, float64(0.0))

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
