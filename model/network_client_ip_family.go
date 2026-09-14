package model

// network_client_ip_family.go — the address-family rules shared by the
// connection record, the reliability aggregation, the score cache and
// find-providers2 (connect/IPV6.md A3, A8).
//
// A connection row carries the family it was observed on (ip_version) and the
// family the transport declared it intends to prove (ip_family_intent). The
// judgement lives in ConnectionProvenIpFamily and nowhere else, so the
// aggregation, the tests and any future reader agree on what a row proves.
//
// The per-client result is a bitmask on the cached score. The mask's zero
// value is LEGACY, not unknown: a score written before the field existed, or a
// client whose rows prove nothing, reads as v4-only, which is what every
// provider was assumed to carry before the field existed. That keeps an old
// cache entry, an old client and an old server all behaving as they do today.

import (
	"fmt"

	"github.com/urnetwork/connect"
)

// normalizeIpFamilyIntent clamps a declared intent to the three values the
// schema means: 0 (legacy), 4 or 6.
func normalizeIpFamilyIntent(ipFamilyIntent int) int {
	switch ipFamilyIntent {
	case 4, 6:
		return ipFamilyIntent
	default:
		return 0
	}
}

// ConnectionProvenIpFamily is the family one connection row proves: 4 or 6,
// or 0 when it proves nothing. A legacy row (intent 0) proves v4. A declared
// intent proves itself only when the observed family agrees; a disagreement
// means something between the client and the edge (an extender, a proxy, a
// misconfigured record) carried the connection on another family, and the row
// then says nothing about the family the client can egress.
func ConnectionProvenIpFamily(ipVersion int, ipFamilyIntent int) int {
	switch normalizeIpFamilyIntent(ipFamilyIntent) {
	case 0:
		return 4
	case 4, 6:
		if ipVersion == ipFamilyIntent {
			return ipFamilyIntent
		}
	}
	return 0
}

// The ClientScore.IpFamilies bits.
const (
	ClientScoreIpFamilyV4 uint8 = 1
	ClientScoreIpFamilyV6 uint8 = 2
)

// clientScoreIpFamilies packs the reliability row's proven flags.
func clientScoreIpFamilies(ipv4Proven bool, ipv6Proven bool) uint8 {
	var families uint8
	if ipv4Proven {
		families |= ClientScoreIpFamilyV4
	}
	if ipv6Proven {
		families |= ClientScoreIpFamilyV6
	}
	return families
}

// IpFamily is the provider's category as the client vocabulary names it.
// Zero (legacy, or nothing proven) is v4-only; see the file comment.
func (self *ClientScore) IpFamily() connect.IpFamily {
	switch self.IpFamilies & (ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6) {
	case ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6:
		return connect.IpFamilyDualstack
	case ClientScoreIpFamilyV6:
		return connect.IpFamilyV6Only
	default:
		return connect.IpFamilyV4Only
	}
}

// ipFamilyFacet names one category's slice of the score cache. The facet is
// part of the redis key, so it is one character.
type ipFamilyFacet string

const (
	ipFamilyFacetDualstack ipFamilyFacet = "d"
	ipFamilyFacetV4Only    ipFamilyFacet = "4"
	ipFamilyFacetV6Only    ipFamilyFacet = "6"
)

// ipFamilyFacets is every facet the export writes, in a fixed order.
var ipFamilyFacets = []ipFamilyFacet{
	ipFamilyFacetDualstack,
	ipFamilyFacetV4Only,
	ipFamilyFacetV6Only,
}

// ipFamilyFacet is the facet this score is exported under.
func (self *ClientScore) ipFamilyFacet() ipFamilyFacet {
	switch self.IpFamily() {
	case connect.IpFamilyDualstack:
		return ipFamilyFacetDualstack
	case connect.IpFamilyV6Only:
		return ipFamilyFacetV6Only
	default:
		return ipFamilyFacetV4Only
	}
}

// ipFamilyFacetsForFilter maps the find-providers2 `ip_family` filter to the
// facets to draw, in preference order: a capable filter draws dualstack first
// and tops up from the single family, an exact filter draws one facet. The
// empty filter is v4-capable, which is every provider an older server knows,
// so an older client keeps today's behavior. An unknown value is a 400: a
// filter this server cannot interpret must not silently widen to "anything".
func ipFamilyFacetsForFilter(ipFamily string) ([]ipFamilyFacet, error) {
	switch connect.IpFamilyFilter(ipFamily) {
	case connect.IpFamilyFilterDefault, connect.IpFamilyFilterV4Capable:
		return []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}, nil
	case connect.IpFamilyFilterV6Capable:
		return []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV6Only}, nil
	case connect.IpFamilyFilterDualstack:
		return []ipFamilyFacet{ipFamilyFacetDualstack}, nil
	case connect.IpFamilyFilterV4Only:
		return []ipFamilyFacet{ipFamilyFacetV4Only}, nil
	case connect.IpFamilyFilterV6Only:
		return []ipFamilyFacet{ipFamilyFacetV6Only}, nil
	default:
		return nil, fmt.Errorf("400 unknown ip_family %q", ipFamily)
	}
}
