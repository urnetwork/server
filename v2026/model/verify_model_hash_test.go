package model

import (
	"net/netip"
	"testing"
)

func TestVerifyEgressExactIndexAndPrefixScoreAreIndependent(t *testing.T) {
	settings := &VerifySettings{EgressHashKey: []byte("unit-test-pepper"), EgressHashV4Prefix: 29, EgressHashV6Prefix: 48}
	v4a := netip.MustParseAddr("198.51.100.88")
	v4b := netip.MustParseAddr("198.51.100.89")
	if VerifyEgressIndexHashWithSettings(v4a, settings) == VerifyEgressIndexHashWithSettings(v4b, settings) {
		t.Fatal("two exact IPv4 addresses in one /29 collided in the eligibility index")
	}
	if VerifyEgressIpHashWithSettings(v4a, settings) != VerifyEgressIpHashWithSettings(v4b, settings) {
		t.Fatal("two IPv4 addresses in one /29 did not share the public scoring hash")
	}
	v6a := netip.MustParseAddr("2001:db8:1234::1")
	v6b := netip.MustParseAddr("2001:db8:1234::2")
	if VerifyEgressIndexHashWithSettings(v6a, settings) == VerifyEgressIndexHashWithSettings(v6b, settings) {
		t.Fatal("two exact IPv6 addresses in one /48 collided in the eligibility index")
	}
	if VerifyEgressIpHashWithSettings(v6a, settings) != VerifyEgressIpHashWithSettings(v6b, settings) {
		t.Fatal("two IPv6 addresses in one /48 did not share the public scoring hash")
	}
	if VerifyEgressIndexHashWithSettings(v4a, settings) == VerifyEgressIpHashWithSettings(v4a, settings) {
		t.Fatal("exact-index and public-score hash domains were not separated")
	}
}
