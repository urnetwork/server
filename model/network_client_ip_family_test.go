package model

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

// The proof rule is the one place a connection row is judged; every case the
// schema can hold is pinned here (connect/IPV6.md A3, A8).
func TestConnectionProvenIpFamily(t *testing.T) {
	cases := []struct {
		ipVersion int
		intent    int
		proven    int
	}{
		// legacy proves v4 whatever it arrived on
		{4, 0, 4},
		{6, 0, 4},
		{0, 0, 4},
		// a declared intent proves itself only when observed
		{4, 4, 4},
		{6, 6, 6},
		// a mismatch proves nothing
		{4, 6, 0},
		{6, 4, 0},
		{0, 4, 0},
		{0, 6, 0},
		// an unknown intent normalizes to legacy
		{4, 5, 4},
		{6, -1, 4},
	}
	for _, c := range cases {
		if proven := ConnectionProvenIpFamily(c.ipVersion, c.intent); proven != c.proven {
			t.Errorf("observed %d intent %d: proven = %d, want %d", c.ipVersion, c.intent, proven, c.proven)
		}
	}
}

func TestClientScoreIpFamily(t *testing.T) {
	cases := []struct {
		ipv4Proven bool
		ipv6Proven bool
		family     connect.IpFamily
		facet      ipFamilyFacet
	}{
		{false, false, connect.IpFamilyV4Only, ipFamilyFacetV4Only},
		{true, false, connect.IpFamilyV4Only, ipFamilyFacetV4Only},
		{false, true, connect.IpFamilyV6Only, ipFamilyFacetV6Only},
		{true, true, connect.IpFamilyDualstack, ipFamilyFacetDualstack},
	}
	for _, c := range cases {
		clientScore := &ClientScore{
			IpFamilies: clientScoreIpFamilies(c.ipv4Proven, c.ipv6Proven),
		}
		if family := clientScore.IpFamily(); family != c.family {
			t.Errorf("v4=%t v6=%t: family = %q, want %q", c.ipv4Proven, c.ipv6Proven, family, c.family)
		}
		if facet := clientScore.ipFamilyFacet(); facet != c.facet {
			t.Errorf("v4=%t v6=%t: facet = %q, want %q", c.ipv4Proven, c.ipv6Proven, facet, c.facet)
		}
	}
	// the gob zero value of an entry written before the field existed is
	// legacy and reads as v4-only
	legacy := &ClientScore{}
	if legacy.IpFamily() != connect.IpFamilyV4Only {
		t.Errorf("zero IpFamilies = %q, want v4-only", legacy.IpFamily())
	}
}

func TestIpFamilyFacetsForFilter(t *testing.T) {
	cases := []struct {
		filter string
		facets []ipFamilyFacet
	}{
		{"", []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}},
		{"v4-capable", []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}},
		{"v6-capable", []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV6Only}},
		{"dualstack", []ipFamilyFacet{ipFamilyFacetDualstack}},
		{"v4-only", []ipFamilyFacet{ipFamilyFacetV4Only}},
		{"v6-only", []ipFamilyFacet{ipFamilyFacetV6Only}},
	}
	for _, c := range cases {
		facets, err := ipFamilyFacetsForFilter(c.filter)
		if err != nil {
			t.Fatalf("%q: %v", c.filter, err)
		}
		if len(facets) != len(c.facets) {
			t.Fatalf("%q: facets = %v, want %v", c.filter, facets, c.facets)
		}
		for i := range facets {
			if facets[i] != c.facets[i] {
				t.Fatalf("%q: facets = %v, want %v", c.filter, facets, c.facets)
			}
		}
	}
	// an unknown filter is a 400, never a wider match
	for _, filter := range []string{"v4", "v6", "both", "V4-CAPABLE", "dual"} {
		_, err := ipFamilyFacetsForFilter(filter)
		if err == nil || !strings.HasPrefix(err.Error(), "400 ") {
			t.Errorf("%q: err = %v, want a 400", filter, err)
		}
	}
}
