package server

import (
	"net/netip"
	"os"
	"slices"

	"testing"

	"github.com/go-playground/assert/v2"
)

func TestLimitExcludePrefixes(t *testing.T) {

	v := os.Getenv("WARP_LIMIT_EXCLUDE_SUBNETS")
	defer os.Setenv("WARP_LIMIT_EXCLUDE_SUBNETS", v)
	os.Setenv("WARP_LIMIT_EXCLUDE_SUBNETS", "10.0.0.0/8;172.16.0.0/12;192.168.0.0/16")

	prefixes := limitExcludePrefixes()
	assert.Equal(t, len(prefixes), 3)
	assert.Equal(t, slices.Contains(prefixes, netip.MustParsePrefix("10.0.0.0/8")), true)
	assert.Equal(t, slices.Contains(prefixes, netip.MustParsePrefix("172.16.0.0/12")), true)
	assert.Equal(t, slices.Contains(prefixes, netip.MustParsePrefix("192.168.0.0/16")), true)

	assert.Equal(t, IsLimitExcludeAddr(netip.MustParseAddr("1.1.1.1")), false)
	assert.Equal(t, IsLimitExcludeAddr(netip.MustParseAddr("192.168.1.1")), true)
	assert.Equal(t, IsLimitExcludeAddr(netip.MustParseAddr("10.1.1.1")), true)
	assert.Equal(t, IsLimitExcludeAddr(netip.MustParseAddr("172.16.1.1")), true)

	os.Setenv("WARP_LIMIT_EXCLUDE_SUBNETS", "")
	prefixes = limitExcludePrefixes()
}
